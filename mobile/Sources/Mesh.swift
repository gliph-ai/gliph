//  Nearby mesh (spec F9), iOS transport.
//
//  The wire is the one desktop/src-tauri/src/mesh.rs speaks, deliberately, so a
//  MacBook and an iPhone on the same Wi-Fi are peers in ONE mesh:
//
//    discovery  Bonjour `_gliph._tcp`, TXT keys id / nick / chans
//    links      TCP, newline-delimited JSON frames {"t":..,"d":{..}}
//    dialling   every peer dials every peer, and retries
//
//  Bonjour and not raw multicast because multicast on iOS needs Apple's
//  com.apple.developer.networking.multicast entitlement, which is an
//  application with a review, not a checkbox. Bonjour needs only the
//  local-network prompt — see NSLocalNetworkUsageDescription in Info.plist.
//
//  MeshNode knows nothing about the UI or the core. It has now survived being
//  moved from Capacitor to Flutter to this without its logic changing, which is
//  the whole argument for keeping a transport free of its host.
//
//  Wi-Fi Aware (iOS 26+) runs beside Bonjour, not instead of it. Two paired
//  phones with no shared Wi-Fi find each other over it; a desktop is never a
//  Wi-Fi Aware peer, so Bonjour stays on for it and for any phone that has no
//  Wi-Fi Aware radio. Both paths open the same TCP link and speak the same
//  frames, so the rest of this file cannot tell which one a peer came in on.
//  Pairing is a one-time OS sheet (SettingsView); after it the two phones
//  connect to each other whenever both are in nearby mode.
//
//  ponytail: no encryption on this hop and no store-and-forward, matching the
//  Rust side. Both are said to the user in the channel's topic (core/src/mesh.rs,
//  NEARBY_TOPIC); do not fix one end without the other.

import Foundation
import Network
#if canImport(WiFiAware)
import WiFiAware
#endif

private let SERVICE_TYPE = "_gliph._tcp"
/// Matches REBROWSE_TICKS in mesh.rs — both sides retry on the same cadence.
private let REDIAL_EVERY: DispatchTimeInterval = .seconds(8)

private struct MeshPeer {
    var id: String
    var nick: String
    var chans: [String]
    var link: NWConnection?
    /// Kept so a dial that failed can be tried again. Browse results only
    /// arrive when the result set CHANGES, so without this a single failed
    /// connect means that peer stays unreachable for the whole flight.
    var endpoint: NWEndpoint?
}

/// Everything here runs on `queue`. Network.framework hands us callbacks on it,
/// and the peer table is touched from every one of them.
final class Mesh {
    private let queue = DispatchQueue(label: "chat.gliph.mesh")
    private let id = Mesh.randomID()
    private var nick = ""
    private var chans: [String] = []

    private var listener: NWListener?
    private var browser: NWBrowser?
    private var redialTimer: DispatchSourceTimer?
    private var peers: [String: MeshPeer] = [:]
    /// Half-open links: adopted, but no `hi` yet, so we do not know who they are.
    private var pending: [ObjectIdentifier: NWConnection] = [:]
    private var buffers: [ObjectIdentifier: Data] = [:]

    // Wi-Fi Aware. Its browse results carry no TXT record, so we cannot know
    // who an endpoint is until its `hi`: links are tracked by endpoint, and
    // the `hi` files them under a peer like any other adopted connection.
    private var awareListener: NWListener?
    private var awareBrowser: NWBrowser?
    private var awareParams: NWParameters?
    private var awareSeen: Set<NWEndpoint> = []
    private var awareLinks: [NWEndpoint: NWConnection] = [:]
    /// The OS pairing sheet publishes the same service; ours steps aside
    /// while it is open, or one of the two fails as already publishing.
    private var awarePaused = false

    /// Everything leaves as JSON, because the core takes JSON. No Swift model
    /// of a mesh frame exists, and none should: the shapes belong to
    /// docs/protocol.md, and one of them (`frame`) is deliberately opaque.
    private let onPeers: (String) -> Void
    private let onMsg: (String) -> Void
    private let onFrame: (String) -> Void

    init(onPeers: @escaping (String) -> Void,
         onMsg: @escaping (String) -> Void,
         onFrame: @escaping (String) -> Void) {
        self.onPeers = onPeers
        self.onMsg = onMsg
        self.onFrame = onFrame
    }

    static func randomID() -> String {
        (0..<6).map { _ in String(format: "%02x", Int.random(in: 0...255)) }.joined()
    }

    func start(nick: String) throws {
        try queue.sync {
            guard listener == nil else { return }
            self.nick = nick
            let params = NWParameters.tcp
            params.includePeerToPeer = true   // AWDL: works without an access point
            let l = try NWListener(using: params)
            l.service = NWListener.Service(name: id, type: SERVICE_TYPE, domain: nil,
                                           txtRecord: txtRecord())
            l.newConnectionHandler = { [weak self] conn in self?.adopt(conn, expect: nil) }
            l.stateUpdateHandler = { state in
                if case .failed(let e) = state { NSLog("mesh listener failed: \(e)") }
            }
            l.start(queue: queue)
            listener = l
            startBrowsing(params: params)
            startAware()

            let t = DispatchSource.makeTimerSource(queue: queue)
            t.schedule(deadline: .now() + REDIAL_EVERY, repeating: REDIAL_EVERY)
            t.setEventHandler { [weak self] in self?.redial() }
            t.resume()
            redialTimer = t
        }
    }

    func stop() {
        queue.sync {
            redialTimer?.cancel(); redialTimer = nil
            browser?.cancel(); browser = nil
            listener?.cancel(); listener = nil
            stopAware()
            for (_, p) in peers { p.link?.cancel() }
            for (_, c) in pending { c.cancel() }
            peers = [:]; pending = [:]; buffers = [:]
        }
        onPeers("[]")
    }

    func join(chans: [String]) {
        queue.sync {
            self.chans = chans
            // Two ways a peer learns our channel list, and both matter: the TXT
            // record for anyone who has not linked yet, the frame for anyone who
            // has.
            listener?.service = NWListener.Service(name: id, type: SERVICE_TYPE, domain: nil,
                                                   txtRecord: txtRecord())
            broadcast(["t": "chans", "d": ["id": id, "chans": chans]])
        }
    }

    /// While the pairing sheet is up, the sheet owns the Wi-Fi Aware service.
    func pauseAware(_ paused: Bool) {
        queue.sync {
            awarePaused = paused
            if paused { stopAware() } else if listener != nil { startAware() }
        }
    }

    /// Send an arbitrary frame to ONE peer. A transfer concerns two people;
    /// broadcasting its chunks would multiply the bytes by the room size.
    func sendTo(peer peerID: String, t: String, json: String) {
        let d = (json.data(using: .utf8)
            .flatMap { try? JSONSerialization.jsonObject(with: $0) }) as? [String: Any] ?? [:]
        queue.sync {
            guard let link = peers[peerID]?.link else { return }
            write(link, ["t": t, "d": d])
        }
    }

    /// Returns the message as JSON so the core can ingest our own line the same
    /// way it ingests a peer's.
    func say(chan: String, body: String, kind: String, replyTo: String) -> String? {
        let msg: [String: Any] = [
            "id": "n_" + Mesh.randomID(),
            "chan": chan,
            "nick": nick,
            "body": body,
            "ts": ISO8601DateFormatter().string(from: Date()),
            // "message" or "action" (/me), and the id this answers. A peer on
            // an older build sends neither and reads neither; both sides
            // default, so a mixed mesh is still one mesh.
            "kind": kind,
            "reply_to": replyTo,
        ]
        queue.sync { broadcast(["t": "msg", "d": msg]) }
        return encode(msg)
    }

    // MARK: - discovery

    private func txtRecord() -> NWTXTRecord {
        NWTXTRecord(["id": id, "nick": nick, "chans": chans.joined(separator: ",")])
    }

    private func startBrowsing(params: NWParameters) {
        let b = NWBrowser(for: .bonjourWithTXTRecord(type: SERVICE_TYPE, domain: nil), using: params)
        b.browseResultsChangedHandler = { [weak self] results, _ in
            guard let self else { return }
            var live = Set<String>()
            for r in results {
                guard case .bonjour(let txt) = r.metadata else { continue }
                guard let pid = txt["id"], pid != self.id else { continue }
                live.insert(pid)
                let nick = txt["nick"] ?? pid
                let chans = (txt["chans"] ?? "").split(separator: ",").map(String.init)
                let existing = self.peers[pid]
                self.peers[pid] = MeshPeer(id: pid, nick: nick, chans: chans,
                                           link: existing?.link, endpoint: r.endpoint)
                // Everyone dials everyone — see the long note in mesh.rs. This
                // device's outbound connections can stall in Bonjour resolution
                // while the other side reaches it immediately, so nominating one
                // dialler per pair is nominating a single point of failure.
                if existing?.link == nil { self.dial(r.endpoint, expect: pid) }
            }
            // Bonjour withdrawals are the primary "they left" signal — but only
            // for peers we are not already talking to. A browse result set can
            // blink for a peer sitting right there on an open socket, and
            // cancelling that link drops someone mid-conversation. The link
            // closing is what proves they left; see mesh.rs, same lesson.
            for (pid, p) in self.peers where !live.contains(pid) && p.link == nil {
                self.peers.removeValue(forKey: pid)
            }
            self.emitPeers()
        }
        b.start(queue: queue)
        browser = b
    }

    /// Every peer we have no link to, dialled again.
    private func redial() {
        // A Wi-Fi Aware listener fails outright with no paired device; retrying
        // on this tick is what picks up a pairing made after nearby mode began.
        if awareListener == nil || awareBrowser == nil { startAware() }
        for ep in awareSeen where awareLinks[ep] == nil { dialAware(ep) }
        for (pid, p) in peers where p.link == nil {
            guard let endpoint = p.endpoint else { continue }
            NSLog("mesh: redialling \(p.nick) [\(pid)]")
            dial(endpoint, expect: pid)
        }
    }

    private func dial(_ endpoint: NWEndpoint, expect: String) {
        let params = NWParameters.tcp
        params.includePeerToPeer = true
        adopt(NWConnection(to: endpoint, using: params), expect: expect)
    }

    // MARK: - Wi-Fi Aware

    /// Publish and subscribe `_gliph._tcp` to every paired device. The legacy
    /// NWListener/NWBrowser take Wi-Fi Aware through the providers' own
    /// `service`/`makeDescriptor` and `configureParameters`, which keeps one
    /// connection type for every link in this file.
    private func startAware() {
        #if canImport(WiFiAware)
        guard #available(iOS 26, *), !awarePaused else { return }
        guard WACapabilities.supportedFeatures.contains(.wifiAware),
              let pub = WAPublishableService.allServices[SERVICE_TYPE],
              let sub = WASubscribableService.allServices[SERVICE_TYPE] else { return }

        if awareListener == nil {
            let provider = WAPublisherListener.wifiAware(.connecting(to: pub, from: .allPairedDevices))
            let params = NWParameters.tcp
            provider.configureParameters(params)
            if let l = try? NWListener(service: provider.service, using: params) {
                l.newConnectionHandler = { [weak self] conn in self?.adopt(conn, expect: nil) }
                l.stateUpdateHandler = { [weak self, weak l] state in
                    guard case .failed(let e) = state else { return }
                    NSLog("mesh: wifi aware listener failed: \(e.wifiAware.map { "\($0)" } ?? "\(e)")")
                    l?.cancel()
                    self?.awareListener = nil
                }
                l.start(queue: queue)
                awareListener = l
            }
        }
        if awareBrowser == nil {
            let provider = WASubscriberBrowser.wifiAware(.connecting(to: .allPairedDevices, from: sub))
            let params = provider.configureParameters(NWParameters.tcp)
            awareParams = params
            let b = NWBrowser(for: provider.makeDescriptor(), using: params)
            b.browseResultsChangedHandler = { [weak self] results, _ in
                guard let self else { return }
                self.awareSeen = Set(results.map(\.endpoint))
                for ep in self.awareSeen where self.awareLinks[ep] == nil { self.dialAware(ep) }
            }
            b.stateUpdateHandler = { [weak self, weak b] state in
                guard case .failed(let e) = state else { return }
                NSLog("mesh: wifi aware browser failed: \(e.wifiAware.map { "\($0)" } ?? "\(e)")")
                b?.cancel()
                self?.awareBrowser = nil
            }
            b.start(queue: queue)
            awareBrowser = b
        }
        #endif
    }

    private func dialAware(_ ep: NWEndpoint) {
        guard let params = awareParams else { return }
        let conn = NWConnection(to: ep, using: params)
        awareLinks[ep] = conn
        adopt(conn, expect: nil)
    }

    private func stopAware() {
        awareListener?.cancel(); awareListener = nil
        awareBrowser?.cancel(); awareBrowser = nil
        for (_, c) in awareLinks { c.cancel() }
        awareLinks = [:]; awareSeen = []
    }

    // MARK: - links

    private func adopt(_ conn: NWConnection, expect: String?) {
        pending[ObjectIdentifier(conn)] = conn
        conn.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready:
                self.write(conn, ["t": "hi",
                                  "d": ["id": self.id, "nick": self.nick, "chans": self.chans]])
                if let expect, var p = self.peers[expect] { p.link = conn; self.peers[expect] = p }
                self.receive(conn)
            case .failed, .cancelled:
                self.drop(conn)
            default:
                break
            }
        }
        conn.start(queue: queue)
    }

    private func drop(_ conn: NWConnection) {
        let key = ObjectIdentifier(conn)
        pending.removeValue(forKey: key)
        buffers.removeValue(forKey: key)
        for (ep, c) in awareLinks where c === conn { awareLinks.removeValue(forKey: ep) }
        for (pid, p) in peers where p.link === conn {
            var q = p; q.link = nil; peers[pid] = q
        }
        emitPeers()
    }

    private func receive(_ conn: NWConnection) {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 65536) {
            [weak self] data, _, done, err in
            guard let self else { return }
            if let data, !data.isEmpty {
                let key = ObjectIdentifier(conn)
                var buf = self.buffers[key] ?? Data()
                buf.append(data)
                // Frames are newline-delimited: split, keep the tail for next time.
                while let nl = buf.firstIndex(of: 0x0a) {
                    let line = buf[buf.startIndex..<nl]
                    buf = buf[buf.index(after: nl)...]
                    self.handle(line: Data(line), from: conn)
                }
                self.buffers[key] = buf
            }
            if done || err != nil { self.drop(conn); return }
            self.receive(conn)
        }
    }

    private func handle(line: Data, from conn: NWConnection) {
        guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
              let t = obj["t"] as? String else { return }
        let d = obj["d"] as? [String: Any] ?? [:]
        switch t {
        case "hi":
            guard let pid = d["id"] as? String, pid != id else { return }
            var p = peers[pid] ?? MeshPeer(id: pid, nick: pid, chans: [], link: nil)
            p.nick = d["nick"] as? String ?? pid
            p.chans = d["chans"] as? [String] ?? []
            if p.link == nil { p.link = conn }
            peers[pid] = p
            emitPeers()
        case "chans":
            guard let pid = d["id"] as? String, var p = peers[pid] else { return }
            p.chans = d["chans"] as? [String] ?? []
            peers[pid] = p
            emitPeers()
        case "msg":
            if let json = encode(d) { onMsg(json) }
        default:
            // Not ours to understand. The core needs to know who sent it, so
            // the sender rides along with the frame.
            let from = peers.first(where: { $0.value.link === conn })?.value
            let wrapped: [String: Any] = ["from": from?.id ?? "", "nick": from?.nick ?? "",
                                          "t": t, "d": d]
            if let json = encode(wrapped) { onFrame(json) }
        }
    }

    private func write(_ conn: NWConnection, _ frame: [String: Any]) {
        guard var data = try? JSONSerialization.data(withJSONObject: frame) else { return }
        data.append(0x0a)
        conn.send(content: data, completion: .contentProcessed { _ in })
    }

    private func broadcast(_ frame: [String: Any]) {
        for (_, p) in peers { if let link = p.link { write(link, frame) } }
    }

    /// Only peers we hold a live connection to — see the same note in
    /// mesh.rs::peer_views. A Bonjour record outlives the device that published
    /// it; a TCP link does not.
    private func emitPeers() {
        let list = peers.values.filter { $0.link != nil }
            .map { ["id": $0.id, "nick": $0.nick, "chans": $0.chans] as [String: Any] }
        if let json = encode(list) { onPeers(json) }
    }

    private func encode(_ any: Any) -> String? {
        guard let data = try? JSONSerialization.data(withJSONObject: any) else { return nil }
        return String(data: data, encoding: .utf8)
    }
}
