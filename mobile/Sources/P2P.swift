//  Peer-to-peer file transfer in DMs (spec F7), iOS side.
//
//  The Swift twin of web/src/p2p.ts, and it has to stay one: an iPhone and a
//  desktop are the two ends of the same data channel. The handshake rides the
//  chat socket as `p2p.*` frames (server/p2p.go relays them inside a DM and
//  nowhere else); the bytes go over a WebRTC data channel, direct or through
//  the server's TURN relay (`GET /api/ice`), and never through the server.
//
//  The wire, as p2p.ts speaks it:
//    sdp        JSON.stringify(RTCSessionDescription) — {"type","sdp"}
//    ice        {candidate, mid}, trickled both ways
//    channel    one ordered data channel "gliph-file", 64 KB binary chunks;
//               the size in p2p.begin is the only end-of-file marker
//    verdict    the receiver sends the string "ok" or "bad" after hashing
//
//  ponytail: no resume and the whole file in memory at both ends, as on the
//  web. A dropped transfer fails loudly and is started again.

import CryptoKit
import Foundation
import WebRTC

private let CHUNK = 64 * 1024
private let HIGH_WATER: UInt64 = 1 << 20
private let STALL: TimeInterval = 30
private let ACCEPT: TimeInterval = 5 * 60
/// What p2p.ts falls back on when the server configures no ICE servers.
private let DEFAULT_ICE = [RTCIceServer(urlStrings: ["stun:stun.l.google.com:19302"])]

struct P2PInfo {
    let id: String
    let channel: String
    /// nick of the other side
    var peer: String
    let name: String
    let size: Int
    let send: Bool
}

/// Everything the host provides. P2P knows frames and files, not the UI.
struct P2PHost {
    var send: ([String: Any]) -> Void
    /// Ask the human; nothing is received until this answers true.
    var confirm: (P2PInfo, @escaping (Bool) -> Void) -> Void
    var ice: () async -> [RTCIceServer]
    var notice: (String, String) -> Void
    /// Checksum verified at the receiver. `bytes` is nil on the sending side.
    var done: (P2PInfo, Data?) -> Void
}

@MainActor
final class P2P {
    private static let factory: RTCPeerConnectionFactory = {
        RTCInitializeSSL()
        return RTCPeerConnectionFactory()
    }()

    private let host: P2PHost
    private var transfers: [String: DMTransfer] = [:]

    init(host: P2PHost) { self.host = host }

    // MARK: - sending

    func offer(channel: String, peer: String, name: String, bytes: Data) {
        let info = P2PInfo(id: UUID().uuidString.lowercased(), channel: channel, peer: peer,
                           name: name, size: bytes.count, send: true)
        let t = DMTransfer(info: info, owner: self)
        t.bytes = bytes
        t.sha256 = SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined()
        transfers[info.id] = t
        host.send(["t": "p2p.begin", "d": ["channel": channel, "id": info.id, "name": name,
                                           "size": bytes.count, "sha256": t.sha256]])
        host.notice("offering \(name) directly to \(peer)…", "info")
        arm(t, ACCEPT, "the other side never answered")
    }

    // MARK: - frames

    /// Every p2p.* frame from the server. A frame for a transfer this device is
    /// not driving is dropped: the server relays to all of a user's devices.
    func handle(_ t: String, _ d: [String: Any]) {
        guard let id = d["id"] as? String, !id.isEmpty else { return }
        let x = transfers[id]
        switch t {
        case "p2p.begin":
            if x == nil { receive(d) }
        case "p2p.accept":
            guard let x, x.info.send, x.pc == nil else { return }
            Task { await self.connect(x) }
        case "p2p.reject":
            if let x { fail(x, "the other side declined the transfer", notify: false) }
        case "p2p.cancel":
            let why = (d["reason"] as? String).map { "the other side cancelled: \($0)" }
                ?? "the other side cancelled"
            if let x { fail(x, why, notify: false) }
        case "p2p.offer":
            if let x, !x.info.send, !x.haveRemote, let sdp = d["sdp"] as? String {
                Task { await self.answer(x, sdp) }
            }
        case "p2p.answer":
            if let x, x.info.send, let sdp = d["sdp"] as? String, let desc = Self.decode(sdp) {
                x.pc?.setRemoteDescription(desc) { err in
                    Task { @MainActor in
                        if let err { self.fail(x, "bad answer from the other client: \(err.localizedDescription)") }
                        else { x.haveRemote = true; self.flushIce(x) }
                    }
                }
            }
        case "p2p.ice":
            guard let x, let c = d["candidate"] as? String else { return }
            let mid = d["mid"] as? String ?? ""
            let cand = RTCIceCandidate(sdp: c, sdpMLineIndex: 0, sdpMid: mid.isEmpty ? nil : mid)
            if x.haveRemote, let pc = x.pc { pc.add(cand) { _ in } } else { x.pendingIce.append(cand) }
        default:
            break
        }
    }

    // MARK: - the two halves of the handshake

    private func receive(_ d: [String: Any]) {
        let info = P2PInfo(id: d["id"] as? String ?? "", channel: d["channel"] as? String ?? "",
                           peer: d["from"] as? String ?? "", name: d["name"] as? String ?? "file",
                           size: (d["size"] as? NSNumber)?.intValue ?? 0, send: false)
        guard info.size > 0 else { return }
        let t = DMTransfer(info: info, owner: self)
        t.sha256 = (d["sha256"] as? String ?? "").lowercased()
        transfers[info.id] = t
        host.confirm(info) { [weak self] ok in
            guard let self, self.transfers[info.id] === t else { return }
            let ids: [String: Any] = ["channel": info.channel, "id": info.id]
            if !ok {
                self.host.send(["t": "p2p.reject", "d": ids.merging(["reason": "declined"]) { a, _ in a }])
                self.transfers[info.id] = nil
                return
            }
            self.host.send(["t": "p2p.accept", "d": ids])
            self.arm(t, STALL, "the sender never connected")
        }
    }

    /// Sender, after the accept: open the channel and make the offer.
    private func connect(_ t: DMTransfer) async {
        clear(t)
        let pc = await peer(t)
        let cfg = RTCDataChannelConfiguration()
        cfg.isOrdered = true
        guard let dc = pc.dataChannel(forLabel: "gliph-file", configuration: cfg) else {
            return fail(t, "could not open a data channel")
        }
        dc.delegate = t
        t.dc = dc
        do {
            let offer = try await pc.offer(for: Self.noConstraints)
            try await pc.setLocalDescription(offer)
            send(t, "p2p.offer", ["sdp": Self.encode(offer)])
            arm(t, STALL, "could not reach the other client")
        } catch {
            fail(t, "could not make a transfer offer: \(error.localizedDescription)")
        }
    }

    /// Receiver: answer the sender's offer; the channel arrives through the delegate.
    private func answer(_ t: DMTransfer, _ sdp: String) async {
        guard let desc = Self.decode(sdp) else { return fail(t, "bad offer from the other client") }
        let pc = await peer(t)
        do {
            try await pc.setRemoteDescription(desc)
            t.haveRemote = true
            flushIce(t)
            let ans = try await pc.answer(for: Self.noConstraints)
            try await pc.setLocalDescription(ans)
            send(t, "p2p.answer", ["sdp": Self.encode(ans)])
            arm(t, STALL, "could not reach the sender")
        } catch {
            fail(t, "could not answer the transfer offer: \(error.localizedDescription)")
        }
    }

    private func peer(_ t: DMTransfer) async -> RTCPeerConnection {
        let servers = await host.ice()
        let cfg = RTCConfiguration()
        cfg.iceServers = servers.isEmpty ? DEFAULT_ICE : servers
        cfg.sdpSemantics = .unifiedPlan
        let pc = Self.factory.peerConnection(with: cfg, constraints: Self.noConstraints, delegate: t)!
        t.pc = pc
        return pc
    }

    private func flushIce(_ t: DMTransfer) {
        let queued = t.pendingIce
        t.pendingIce = []
        for c in queued { t.pc?.add(c) { _ in } }
    }

    // MARK: - bytes (called by the delegates, on the main actor)

    fileprivate func opened(_ t: DMTransfer) {
        clear(t)
        if t.info.send { pump(t) }
    }

    /// Fill the SCTP buffer to HIGH_WATER and stop; the buffered-amount
    /// callback calls this again as it drains.
    fileprivate func pump(_ t: DMTransfer) {
        guard t.info.send, !t.settled, let dc = t.dc, dc.readyState == .open else { return }
        while t.offset < t.bytes.count, dc.bufferedAmount < HIGH_WATER {
            let end = min(t.offset + CHUNK, t.bytes.count)
            dc.sendData(RTCDataBuffer(data: t.bytes.subdata(in: t.offset..<end), isBinary: true))
            t.offset = end
        }
        // Not done until the receiver says the checksum matched.
        arm(t, STALL, t.offset < t.bytes.count ? "the transfer stalled" : "the receiver never confirmed the file")
    }

    fileprivate func got(_ t: DMTransfer, _ buf: RTCDataBuffer) {
        if t.info.send {
            guard !buf.isBinary else { return }
            if String(data: buf.data, encoding: .utf8) == "ok" {
                t.settled = true
                host.done(t.info, nil)
                close(t)
            } else {
                fail(t, "the receiver's checksum did not match")
            }
            return
        }
        guard buf.isBinary else { return }
        t.bytes.append(buf.data)
        arm(t, STALL, "the transfer stalled")
        guard t.bytes.count >= t.info.size else { return }
        clear(t)
        let sum = SHA256.hash(data: t.bytes).map { String(format: "%02x", $0) }.joined()
        let good = sum == t.sha256
        t.dc?.sendData(RTCDataBuffer(data: Data((good ? "ok" : "bad").utf8), isBinary: false))
        if good {
            t.settled = true
            host.done(t.info, t.bytes)
            close(t)
        } else {
            fail(t, "the file arrived corrupted (checksum mismatch), nothing was saved")
        }
    }

    // MARK: - plumbing

    private func send(_ t: DMTransfer, _ type: String, _ extra: [String: Any]) {
        host.send(["t": type, "d": extra.merging(["channel": t.info.channel, "id": t.info.id]) { a, _ in a }])
    }

    /// The one way a transfer dies: always reported, never left hanging.
    fileprivate func fail(_ t: DMTransfer, _ why: String, notify: Bool = true) {
        guard !t.settled else { return }
        t.settled = true
        if notify { send(t, "p2p.cancel", ["reason": why]) }
        host.notice("\(t.info.name): \(why)", "error")
        close(t)
    }

    private func close(_ t: DMTransfer) {
        clear(t)
        t.dc?.delegate = nil
        t.dc?.close()
        t.pc?.close()
        t.dc = nil; t.pc = nil
        t.bytes = Data()
        transfers[t.info.id] = nil
    }

    private func arm(_ t: DMTransfer, _ secs: TimeInterval, _ why: String) {
        clear(t)
        let w = DispatchWorkItem { [weak self, weak t] in
            guard let self, let t else { return }
            self.fail(t, why)
        }
        t.stall = w
        DispatchQueue.main.asyncAfter(deadline: .now() + secs, execute: w)
    }

    private func clear(_ t: DMTransfer) {
        t.stall?.cancel()
        t.stall = nil
    }

    private static let noConstraints = RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil)

    /// The SDP travels the way the browser writes it: JSON.stringify of the
    /// description, so p2p.ts can JSON.parse it straight into setRemoteDescription.
    private static func encode(_ d: RTCSessionDescription) -> String {
        let obj = ["type": RTCSessionDescription.string(for: d.type), "sdp": d.sdp]
        return (try? JSONSerialization.data(withJSONObject: obj)).flatMap { String(data: $0, encoding: .utf8) } ?? ""
    }

    private static func decode(_ s: String) -> RTCSessionDescription? {
        guard let data = s.data(using: .utf8),
              let o = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let type = o["type"] as? String, let sdp = o["sdp"] as? String else { return nil }
        return RTCSessionDescription(type: RTCSessionDescription.type(for: type), sdp: sdp)
    }

    /// `GET /api/ice` → RTCIceServer. `urls` is a list on the wire, but a
    /// browser accepts a bare string too, so this does.
    static func iceServers(_ json: Any?) -> [RTCIceServer] {
        guard let list = (json as? [String: Any])?["ice_servers"] as? [[String: Any]] else { return [] }
        return list.compactMap { s in
            let urls = (s["urls"] as? [String]) ?? (s["urls"] as? String).map { [$0] } ?? []
            guard !urls.isEmpty else { return nil }
            return RTCIceServer(urlStrings: urls, username: s["username"] as? String,
                                credential: s["credential"] as? String)
        }
    }
}

/// One transfer, either direction. It is the delegate for its own peer
/// connection and data channel; WebRTC calls those on its signalling thread,
/// so each one hops to the main actor before touching anything.
private final class DMTransfer: NSObject, RTCPeerConnectionDelegate, RTCDataChannelDelegate {
    var info: P2PInfo
    weak var owner: P2P?
    var pc: RTCPeerConnection?
    var dc: RTCDataChannel?
    var sha256 = ""
    /// The whole file when sending; what has arrived so far when receiving.
    var bytes = Data()
    var offset = 0
    /// ICE that arrived before the remote description; adding it early fails.
    var pendingIce: [RTCIceCandidate] = []
    var haveRemote = false
    var settled = false
    var stall: DispatchWorkItem?

    init(info: P2PInfo, owner: P2P) {
        self.info = info
        self.owner = owner
    }

    private func main(_ f: @escaping @MainActor (P2P) -> Void) {
        Task { @MainActor [weak self] in
            guard let self, let owner = self.owner, !self.settled else { return }
            f(owner)
        }
    }

    // RTCPeerConnectionDelegate
    func peerConnection(_ pc: RTCPeerConnection, didGenerate c: RTCIceCandidate) {
        let d: [String: Any] = ["channel": info.channel, "id": info.id,
                                "candidate": c.sdp, "mid": c.sdpMid ?? ""]
        main { $0.sendIce(d) }
    }
    func peerConnection(_ pc: RTCPeerConnection, didOpen dc: RTCDataChannel) {
        main { _ in
            self.dc = dc
            dc.delegate = self
            if dc.readyState == .open { self.owner?.opened(self) }
        }
    }
    func peerConnection(_ pc: RTCPeerConnection, didChange state: RTCPeerConnectionState) {
        guard state == .failed else { return }
        // Usually both ends behind symmetric NAT and no TURN relay configured.
        main { $0.fail(self, "could not establish a peer-to-peer connection (no route, and no TURN relay configured)") }
    }
    func peerConnection(_ pc: RTCPeerConnection, didChange s: RTCSignalingState) {}
    func peerConnection(_ pc: RTCPeerConnection, didAdd s: RTCMediaStream) {}
    func peerConnection(_ pc: RTCPeerConnection, didRemove s: RTCMediaStream) {}
    func peerConnectionShouldNegotiate(_ pc: RTCPeerConnection) {}
    func peerConnection(_ pc: RTCPeerConnection, didChange s: RTCIceConnectionState) {}
    func peerConnection(_ pc: RTCPeerConnection, didChange s: RTCIceGatheringState) {}
    func peerConnection(_ pc: RTCPeerConnection, didRemove c: [RTCIceCandidate]) {}

    // RTCDataChannelDelegate
    func dataChannelDidChangeState(_ dc: RTCDataChannel) {
        let state = dc.readyState
        main { p in
            if state == .open { p.opened(self) }
            if state == .closed { p.fail(self, "the connection dropped before the file finished") }
        }
    }
    func dataChannel(_ dc: RTCDataChannel, didReceiveMessageWith buf: RTCDataBuffer) {
        main { $0.got(self, buf) }
    }
    func dataChannel(_ dc: RTCDataChannel, didChangeBufferedAmount amount: UInt64) {
        main { $0.pump(self) }
    }
}

private extension P2P {
    func sendIce(_ d: [String: Any]) { host.send(["t": "p2p.ice", "d": d]) }
}
