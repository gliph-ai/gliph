//  The iOS host for the Rust core.
//
//  Everything the client DECIDES lives in core/ — the protocol, the crypto, the
//  sync, the offline cache, the key epochs, the nearby state, the transfer
//  engine. This file is what the core cannot be: the socket, the Keychain,
//  UserDefaults, the mesh, the notification centre, the files.
//
//  The loop is always the same three lines. Call into the core, run the effects
//  it returns, publish a fresh snapshot. There is no other path by which this
//  app changes.

import Foundation
import SwiftUI
import UserNotifications
import WebRTC

@MainActor
final class Store: ObservableObject {
    private let core = Core()

    @Published private(set) var snap = Snapshot()
    @Published private(set) var rows: [EventRow] = []
    @Published private(set) var palette: [Command] = []
    @Published private(set) var surface = Surface()
    /// Every server this account is signed in to, and which one is on screen.
    @Published private(set) var servers = ServerList()
    /// A confirm or prompt the core is waiting on. Only one exists at a time,
    /// because a modal is a modal.
    @Published var ask: Ask?
    /// The multi-line editor the core asked for (/theme add, /script add).
    @Published var compose: Compose?
    /// The chat is the root screen (the rail is a drawer over it), so while the
    /// app is in front the active channel is being looked at.
    var foreground = true
    @Published var theme: Theme = .creme {
        didSet { UserDefaults.standard.set(theme.name, forKey: "gliph.theme") }
    }

    /// Set by ChatView: opening the file picker is the UI layer's business.
    var onPickFile: (() -> Void)?

    private var socket: URLSessionWebSocketTask?
    private var session: URLSession!
    private var retry = 0
    /// Last frame or pong from the server. A pong that never comes never fires
    /// sendPing's callback either, so silence is measured here.
    private var lastSeen = Date()
    private var closed = false
    private var mesh: Mesh?
    /// DM transfers over WebRTC (P2P.swift). The core does not model p2p.*
    /// frames; this host does, as the web host does.
    private lazy var p2p = P2P(host: P2PHost(
        send: { [weak self] f in
            guard let data = try? JSONSerialization.data(withJSONObject: f),
                  let s = String(data: data, encoding: .utf8) else { return }
            self?.socket?.send(.string(s)) { _ in }
        },
        confirm: { [weak self] x, answer in
            self?.prompt(Ask(message: "\(x.peer) wants to send you \(x.name) (\(x.size) bytes) directly",
                             wantsText: false, accept: "accept", onAnswer: answer))
        },
        ice: { [weak self] in await self?.iceServers() ?? [] },
        notice: { [weak self] text, level in
            guard let self else { return }
            self.run(self.core.notice(text: text, level: level))
        },
        done: { [weak self] x, bytes in
            guard let self else { return }
            if let bytes { self.save(name: x.name, bytes: bytes) }
            // The line in the channel is the record, the same shape at both ends.
            self.run(self.core.p2pFileLine(cid: x.channel, id: x.id, name: x.name,
                                           size: Double(x.size), sent: x.send, peer: x.peer))
        }))
    private var ice: [RTCIceServer]?
    private var ticker: Timer?
    private var serverTicker: Timer?

    private static let keySession = "gliph.session"
    private static let keyCache = "gliph.cache"
    private static let keyDevice = "gliph.device"
    private static let keyServers = "gliph.servers"

    init() {
        session = URLSession(configuration: .default)
        theme = Theme.named(UserDefaults.standard.string(forKey: "gliph.theme"))
        core.restoreServers(json: UserDefaults.standard.string(forKey: Self.keyServers) ?? "")

        // An install from before the switcher is signed in to one server,
        // recorded the old way. Adopt it rather than signing everybody out.
        let legacy = UserDefaults.standard.string(forKey: Self.keySession) ?? ""
        if core.activeServer().isEmpty, !legacy.isEmpty,
           let d = legacy.data(using: .utf8),
           let old = try? JSONSerialization.jsonObject(with: d) as? [String: Any],
           let token = old["token"] as? String, let server = old["server"] as? String,
           !token.isEmpty, !server.isEmpty {
            run(core.addServer(host: server, token: token, nick: ""))
        }

        // The registry is the source of truth for where we are and what token
        // gets us in; gliph.session is downstream of it.
        let active = core.activeServer()
        let here = decode(ServerList.self, from: core.serversJson(), or: ServerList())
            .list.first { $0.host == active }
        let sessionJSON = here.flatMap { e -> String? in
            let blob = ["token": e.token, "server": e.host]
            return (try? JSONSerialization.data(withJSONObject: blob))
                .flatMap { String(data: $0, encoding: .utf8) }
        } ?? legacy
        // Same one-time adoption for the cache: before the switcher there was
        // one unsuffixed blob, holding the unsent queue too. It belongs to the
        // server gliph.session named and to no other, so it moves under that
        // server's key once and is gone.
        let defaults = UserDefaults.standard
        let key = core.cacheKeyFor(server: active)
        if let oldCache = defaults.string(forKey: Self.keyCache), defaults.string(forKey: key) == nil,
           let d = legacy.data(using: .utf8),
           let old = try? JSONSerialization.jsonObject(with: d) as? [String: Any],
           let from = old["server"] as? String, core.cacheKeyFor(server: from) == key {
            defaults.set(oldCache, forKey: key)
        }
        defaults.removeObject(forKey: Self.keyCache)
        core.restore(
            session: sessionJSON,
            cache: defaults.string(forKey: key) ?? "",
            device: Keychain.read(Self.keyDevice) ?? ""
        )
        // A pinned build knows where home is; a dev build asks. Either way a
        // server the user typed wins, which is what the stored value is.
        if core.server().isEmpty { core.setServer(server: Build.pinnedServer) }
        refresh()

        // The only thing the core needs a clock for: failing a transfer whose
        // other end has gone quiet.
        // The bracket on every server we are NOT looking at. A poll, not a
        // socket per server: a count a minute stale is the right accuracy for
        // "something is waiting elsewhere".
        serverTicker = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.pollServers() }
        }
        Task { @MainActor in await self.pollServers() }

        ticker = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self else { return }
                self.run(self.core.tick(nowMs: now()))
                // A socket iOS quietly killed still looks open; the ping says otherwise.
                let task = self.socket
                // Fifteen seconds of nothing (three pings, or a dial that never
                // opened) is a dead socket: drop it, which redials.
                if let task, Date().timeIntervalSince(self.lastSeen) > 15 {
                    task.cancel()
                    self.dropped()
                    return
                }
                task?.sendPing { err in
                    Task { @MainActor in
                        guard self.socket === task else { return }
                        if err == nil { self.lastSeen = Date() } else { self.dropped() }
                    }
                }
            }
        }
    }

    // MARK: - effects

    private func run(_ json: String) {
        var changed = false
        for effect in Effect.list(json) {
            switch effect {
            case .send(let frame):
                socket?.send(.string(frame)) { _ in }

            case .persist(let key, let value, let secret):
                if secret { Keychain.write(key, value) }
                else { UserDefaults.standard.set(value, forKey: key) }

            case .forget(let key, let secret):
                if secret { Keychain.delete(key) }
                else { UserDefaults.standard.removeObject(forKey: key) }

            case .alert(let id, let channel, let nick, let text, let isDM, let event):
                notify(id: id, channel: channel, nick: nick, text: text, isDM: isDM, event: event)

            case .changed:
                changed = true

            case .meshStart(let nick):
                startMesh(nick: nick)

            case .meshStop:
                mesh?.stop()
                mesh = nil

            case .meshJoin(let chans):
                mesh?.join(chans: chans)

            case .meshSay(let chan, let body, let kind, let replyTo):
                if let msg = mesh?.say(chan: chan, body: body, kind: kind, replyTo: replyTo) {
                    run(core.meshMsg(json: msg, nowMs: now()))
                }

            case .meshSendTo(let peer, let t, let json):
                mesh?.sendTo(peer: peer, t: t, json: json)

            case .saveFile(let name, _, let bytes):
                save(name: name, bytes: bytes)

            case .pickFile:
                onPickFile?()

            // The core cannot draw a dialog, so it asks. Answering is what
            // fires the action — a cancelled confirm must never delete.
            case .askConfirm(let message):
                ask = Ask(message: message, wantsText: false)
            case .askPrompt(let message):
                ask = Ask(message: message, wantsText: true)

            case .compose(let title, let text, let submit):
                compose = Compose(title: title, text: text, submit: submit)
            case .theme(let op, let name, let json):
                let r = Themes.effect(op: op, name: name, json: json)
                if let show = r.show { theme = show }
                run(core.notice(text: r.say, level: r.level))
            }
        }
        if changed { refresh() }
    }

    private func refresh() {
        // Mark before decoding, so the snapshot already shows the badge gone.
        // markActiveRead never says Changed, so this does not re-enter refresh.
        if foreground { run(core.markActiveRead()) }
        snap = decode(Snapshot.self, from: core.snapshot(), or: Snapshot())
        if !pendingDm.isEmpty, let dm = snap.channels.first(where: { $0.is_dm && $0.name == pendingDm }) {
            pendingDm = ""
            setActive(dm.id)
        }
        rows = decode([EventRow].self, from: core.events(cid: snap.active), or: [])
        surface = decode(Surface.self, from: core.surface(), or: Surface())
        servers = decode(ServerList.self, from: core.serversJson(), or: ServerList())
    }

    // MARK: - what the views call

    func submit(_ text: String) { run(core.submit(text: text)) }

    /// A tap on @nick: switch to that DM, opening one first if there is none.
    /// The server answers `/msg nick` with a fresh channel list; the switch
    /// happens in refresh() when the conversation shows up in it.
    private var pendingDm = ""
    func openDm(_ nick: String) {
        guard !nick.isEmpty, nick != snap.user?.nick else { return }
        if let dm = snap.channels.first(where: { $0.is_dm && $0.name == nick }) {
            return setActive(dm.id)
        }
        pendingDm = nick
        submit("/msg \(nick)")
    }
    func setActive(_ cid: String) { run(core.setActive(cid: cid)) }

    /// Long-press → reply. The id is all the core needs: it builds the quote,
    /// puts it on the wire and clears itself when the message goes.
    func setReply(_ eventId: String) { run(core.setReply(eventId: eventId)) }
    /// The reader closed a system message (the MOTD, or a release note).
    func dismissSystem(_ id: String) { run(core.dismissSystem(id: id)) }
    func accept(_ id: String) { run(core.acceptTransfer(id: id)) }
    func decline(_ id: String) { run(core.declineTransfer(id: id)) }
    /// Nearby goes over the mesh link (the core drives it); a DM goes over
    /// WebRTC, straight to the other person's device.
    func offerFile(name: String, mime: String, bytes: Data) {
        keepP2P(name: name, bytes: bytes)
        if let ch = channel(), ch.is_dm {
            p2p.offer(channel: ch.id, peer: ch.name, name: name, bytes: bytes)
            return
        }
        run(core.offerFile(name: name, mime: mime, bytes: bytes))
    }

    /// `GET /api/ice`, once per session. An older server has none; that is
    /// not an error, the default STUN server takes over.
    private func iceServers() async -> [RTCIceServer] {
        if let ice { return ice }
        var got: [RTCIceServer] = []
        if let url = api("/api/ice") {
            var req = URLRequest(url: url)
            req.setValue("Bearer \(core.token())", forHTTPHeaderField: "Authorization")
            if let (data, _) = try? await session.data(for: req) {
                got = P2P.iceServers(try? JSONSerialization.jsonObject(with: data))
            }
        }
        ice = got
        return got
    }

    /// Files that moved peer to peer this session, by `name|size`. The bytes
    /// never touched the server, so the preview under the line in the stream
    /// has nowhere else to come from — the copy in hand is the only copy there
    /// is. ponytail: no eviction. It is the handful of files you moved by hand,
    /// and they go when the app does.
    private var p2pFiles: [String: Data] = [:]
    private func keepP2P(name: String, bytes: Data) { p2pFiles["\(name)|\(bytes.count)"] = bytes }
    func p2pBytes(_ key: String) -> Data? { p2pFiles[key] }

    // MARK: - the blob surface (GUM)
    //
    // Every one of these is a forward. The document, the focus order, the
    // selection, the menus and the drafts are core/src/gum.rs — the same rules
    // the web client obeys, tested there once.

    func openBlob(_ id: String) { run(core.openBlob(blob: id)) }
    func gumTap(_ index: Int) { run(core.gumTap(index: UInt32(index))) }
    func gumTapMenu(_ id: String) { run(core.gumTapMenu(actionId: id)) }
    func gumEscape() { run(core.gumEscape()) }
    func gumActivate() { run(core.gumActivate()) }
    func gumSetDraft(_ text: String) {
        run(core.gumSetDraft(block: core.gumFocusedInput(), text: text))
    }
    /// Typing into an input on a phone means selecting it too: there is no
    /// separate "move the cursor there" gesture.
    func gumFocusInput(_ index: Int) { run(core.gumJump(n: UInt32(index + 1))) }

    func answer(_ ok: Bool, _ value: String = "") {
        let was = ask
        ask = queuedAsks.isEmpty ? nil : queuedAsks.removeFirst()
        if let reply = was?.onAnswer { return reply(ok) }
        run(core.gumAnswer(ok: ok, value: value))
    }

    /// A host-side question (an incoming DM transfer) waits its turn behind
    /// whatever is on screen rather than replacing it.
    private var queuedAsks: [Ask] = []
    private func prompt(_ a: Ask) {
        if ask == nil { ask = a } else { queuedAsks.append(a) }
    }

    func updatePalette(_ text: String) {
        palette = decode([Command].self, from: core.palette(text: text), or: [])
    }

    func channel(_ cid: String? = nil) -> Channel? {
        snap.channels.first { $0.id == (cid ?? snap.active) }
    }

    var server: String { core.server() }
    var signedIn: Bool { !core.token().isEmpty || snap.solo }
    var hasPinnedServer: Bool { !Build.pinnedServer.isEmpty }

    func useDefaultServer() {
        core.setServer(server: Build.pinnedServer)
        refresh()
    }

    // MARK: - auth

    /// The core owns the session; this owns the request. Login is a stateless
    /// POST, so putting it behind the FFI would buy nothing and cost a hop.
    func signIn(server: String, nick: String, password: String) async throws {
        let body = ["nick": nick, "password": password]
        let r = try await post(server: server, path: "/api/login", body: body)
        adopt(server: server, response: r)
    }

    func register(server: String, nick: String, email: String, password: String,
                  invite: String) async throws {
        let body = ["nick": nick, "email": email, "password": password, "invite": invite]
        let r = try await post(server: server, path: "/api/register", body: body)
        adopt(server: server, response: r)
    }

    private func adopt(server: String, response: [String: Any]) {
        let token = response["token"] as? String ?? ""
        let user = response["user"] as? [String: Any] ?? [:]
        let userJSON = (try? JSONSerialization.data(withJSONObject: user))
            .flatMap { String(data: $0, encoding: .utf8) } ?? "{}"
        run(core.setSession(server: server, token: token, userJson: userJSON))
        run(core.addServer(host: server, token: token, nick: user["nick"] as? String ?? ""))
        connect()
        Task { await Push.register(self) }
    }

    func startSolo(nick: String) {
        run(core.startSolo(nick: nick, chan: "nearby"))
    }

    func signOut() {
        closed = true
        Task { await Push.unregister(self) }
        if let url = api("/api/logout") {
            var req = URLRequest(url: url)
            req.httpMethod = "POST"
            req.setValue("Bearer \(core.token())", forHTTPHeaderField: "Authorization")
            session.dataTask(with: req).resume()
        }
        socket?.cancel()
        socket = nil
        mesh?.stop()
        mesh = nil
        let leaving = core.activeServer()
        run(core.logout())
        // Signing out of one server is not signing out of the product: the
        // entry goes, the rest of the list stays signed in.
        run(core.forgetServer(host: leaving))
        closed = false
        if let next = servers.list.first(where: { $0.host == servers.active }) {
            switchTo(next.host, force: true)
        }
    }

    // MARK: - servers (the switcher)

    /// Unread waiting on every server except this one.
    var unreadElsewhere: Int { Int(core.unreadElsewhere()) }

    /// The server on screen, as the registry has it.
    var activeServer: ServerEntry? {
        servers.list.first { $0.host == servers.active }
    }

    /// Put another server on screen. The socket goes with it — one server, one
    /// session, one connection.
    func switchTo(_ host: String, force: Bool = false) {
        guard force || host != core.activeServer(), !host.isEmpty else { return }
        socket?.cancel()
        socket = nil
        // The core cannot read storage, so the cache for where we are going has
        // to be handed in. The token is NOT: it comes from the registry, so
        // nothing can point the session at a server we are not signed in to.
        let cache = UserDefaults.standard.string(forKey: core.cacheKeyFor(server: host)) ?? ""
        run(core.switchServer(host: host, cacheJson: cache, userJson: "{}"))
        connect()
        Task { await Push.register(self) }
    }

    /// Sign in on another server without disturbing the one on screen.
    ///
    /// `post` deliberately carries no Authorization header, which is what
    /// makes it safe here: handing this server's token to another one would be
    /// giving one server a credential for another. If it ever grows one, this
    /// call needs its own request.
    func addServer(server: String, nick: String, password: String) async throws {
        let host = core.normaliseServer(host: server)
        let r = try await post(server: host, path: "/api/login", body: ["nick": nick, "password": password])
        let token = r["token"] as? String ?? ""
        let user = r["user"] as? [String: Any] ?? [:]
        run(core.addServer(host: host, token: token, nick: user["nick"] as? String ?? ""))
        switchTo(host, force: true)
    }

    /// Refresh the bracket on every server we are NOT looking at.
    ///
    /// ponytail: a poll, not a socket per server. A count a minute stale is the
    /// right accuracy for "something is waiting elsewhere", and it costs one
    /// small request per server instead of N live connections.
    func pollServers() async {
        let here = core.activeServer()
        for e in servers.list where e.host != here && !e.token.isEmpty {
            guard let url = URL(string: e.host + "/api/me") else { continue }
            var req = URLRequest(url: url)
            req.setValue("Bearer \(e.token)", forHTTPHeaderField: "Authorization")
            guard let (data, resp) = try? await session.data(for: req),
                  let http = resp as? HTTPURLResponse else { continue }
            if http.statusCode == 401 {
                // The token died. Keep the entry so it stays switchable, but
                // stop it reporting a count that is no longer true.
                run(core.observeServer(host: e.host, name: "", nick: "", icon: "", unread: 0, nowMs: now()))
                continue
            }
            guard http.statusCode == 200,
                  let m = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { continue }
            run(core.observeServer(
                host: e.host,
                name: m["server"] as? String ?? "",
                nick: m["nick"] as? String ?? "",
                icon: m["icon"] as? String ?? "",
                unread: Int64(m["unread"] as? Int ?? 0),
                nowMs: now()
            ))
        }
    }

    func api(_ path: String) -> URL? {
        URL(string: core.server().trimmingCharacters(in: CharacterSet(charactersIn: "/")) + path)
    }

    var token: String { core.token() }

    /// Fetch a shared file and hand it to the share sheet: Files, AirDrop,
    /// Photos, whatever the person picks. Nothing is kept past the sheet.
    /// `saving` is name → percent while it runs; the stream shows it as a line.
    @Published var saving: [String: Int] = [:]
    func saveFile(id: String, name: String) {
        // A file that went peer to peer is already in hand: there is no server
        // copy to fetch, so it goes straight to the sheet.
        if id.hasPrefix("p2p:"), let held = p2pBytes(String(id.dropFirst(4))) {
            let dst = FileManager.default.temporaryDirectory.appendingPathComponent(name)
            try? held.write(to: dst)
            share(dst)
            return
        }
        guard let url = api("/api/file/\(id)"), saving[name] == nil else { return }
        var req = URLRequest(url: url)
        req.setValue("Bearer \(core.token())", forHTTPHeaderField: "Authorization")
        saving[name] = 0
        let dl = Download(
            progress: { [weak self] pct in self?.saving[name] = pct },
            done: { [weak self] tmp in
                self?.saving[name] = nil
                guard let tmp else { return }
                let dst = FileManager.default.temporaryDirectory.appendingPathComponent(name)
                try? FileManager.default.removeItem(at: dst)
                guard (try? FileManager.default.moveItem(at: tmp, to: dst)) != nil else { return }
                self?.share(dst)
            })
        URLSession(configuration: .default, delegate: dl, delegateQueue: .main)
            .downloadTask(with: req).resume()
    }

    private func share(_ file: URL) {
        let sheet = UIActivityViewController(activityItems: [file], applicationActivities: nil)
        UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
            .flatMap(\.windows).first { $0.isKeyWindow }?.rootViewController?
            .present(sheet, animated: true)
    }

    /// The bytes of a shared file, with the token the server wants. Used by the
    /// inline image preview; saving to Files is `saveFile`.
    func fileBytes(id: String) async -> Data? {
        guard let url = api("/api/file/\(id)") else { return nil }
        var req = URLRequest(url: url)
        req.setValue("Bearer \(core.token())", forHTTPHeaderField: "Authorization")
        return try? await URLSession.shared.data(for: req).0
    }

    private func post(server: String, path: String, body: [String: String]) async throws
        -> [String: Any]
    {
        let base = server.hasSuffix("/") ? String(server.dropLast()) : server
        guard let url = URL(string: base + path) else { throw Fail("that is not an address") }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, response) = try await session.data(for: req)
        let json = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
        let code = (response as? HTTPURLResponse)?.statusCode ?? 0
        if code >= 400 || json == nil {
            // A 404 or a non-JSON reply almost always means the address is not
            // a Gliph server rather than a bad password.
            if let err = json?["error"] as? String { throw Fail(err) }
            throw Fail("no gliph server at \(base) (got \(code))")
        }
        return json!
    }

    // MARK: - socket

    func connect() {
        guard !core.token().isEmpty, !closed else { return }
        guard var comps = URLComponents(string: core.server() + "/ws") else { return }
        comps.scheme = comps.scheme == "https" ? "wss" : "ws"
        comps.queryItems = [URLQueryItem(name: "token", value: core.token())]
        guard let url = comps.url else { return }

        socket?.cancel()
        let task = session.webSocketTask(with: url)
        socket = task
        lastSeen = Date()
        task.resume()
        run(core.connected(platform: "iPhone", nowMs: now()))
        // Catch-up arrives in the next few frames; announcements arm once it
        // has settled, or forty banners land at once after a reconnect.
        Task { [weak self] in
            try? await Task.sleep(for: .seconds(2.5))
            self?.core.goLive()
        }
        receive(on: task)
    }

    private func receive(on task: URLSessionWebSocketTask) {
        task.receive { [weak self] result in
            Task { @MainActor in
                guard let self, self.socket === task else { return }
                switch result {
                case .success(let message):
                    self.lastSeen = Date()
                    if case .string(let json) = message {
                        if !self.peekP2P(json) { self.run(self.core.handleFrame(json: json, nowMs: now())) }
                    }
                    self.retry = 0
                    self.receive(on: task)
                case .failure:
                    self.dropped()
                }
            }
        }
    }

    /// p2p.* frames go to P2P.swift; the core has no model of them.
    private func peekP2P(_ json: String) -> Bool {
        guard json.contains("\"p2p."), let data = json.data(using: .utf8),
              let f = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let t = f["t"] as? String, t.hasPrefix("p2p.") else { return false }
        p2p.handle(t, f["d"] as? [String: Any] ?? [:])
        return true
    }

    private func dropped() {
        guard !closed else { return }
        socket = nil
        run(core.disconnected(nowMs: now()))
        retry = min(retry + 1, 6)
        let delay = Double(500 * (1 << retry)) / 1000
        Task { [weak self] in
            try? await Task.sleep(for: .seconds(delay))
            self?.connect()
        }
    }

    /// Woken by a push, or returning to the foreground. The push carried
    /// nothing — that is the design — so the only correct reaction is to go and
    /// find out what happened.
    func wake() {
        if socket == nil { connect() } else { run(core.wake()) }
        refresh()
    }

    // MARK: - nearby mesh

    private func startMesh(nick: String) {
        if mesh == nil {
            mesh = Mesh(
                onPeers: { [weak self] json in
                    Task { @MainActor in self.map { $0.run($0.core.meshPeers(json: json)) } }
                },
                onMsg: { [weak self] json in
                    Task { @MainActor in self.map { $0.run($0.core.meshMsg(json: json, nowMs: now())) } }
                },
                onFrame: { [weak self] json in
                    Task { @MainActor in self.map { $0.run($0.core.meshFrame(json: json, nowMs: now())) } }
                }
            )
        }
        do { try mesh?.start(nick: nick) } catch {
            run(core.notice(text: "could not start nearby mode: \(error.localizedDescription)",
                            level: "error"))
        }
    }

    func pauseAware(_ paused: Bool) { mesh?.pauseAware(paused) }

    // MARK: - files and notifications

    /// A completed nearby transfer. Documents is exposed to the Files app in
    /// Info.plist, so a received file is one the user can actually reach.
    private func save(name: String, bytes: Data) {
        let dir = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
        let url = dir.appendingPathComponent(name)
        keepP2P(name: name, bytes: bytes)
        do {
            try bytes.write(to: url)
            run(core.notice(text: "saved \(name) to Files", level: "info"))
        } catch {
            run(core.notice(text: "could not save \(name)", level: "error"))
        }
    }

    /// The core decided this is worth announcing. Here we decide whether the
    /// user can already see it — an alert for the channel on screen is noise.
    private func notify(id: String, channel: String, nick: String, text: String, isDM: Bool, event: String) {
        let looking = channel == snap.active && UIApplication.shared.applicationState == .active
        guard !looking else { return }
        let content = UNMutableNotificationContent()
        let room = "#\(self.channel(channel)?.name ?? "")"
        content.title = isDM ? nick : event == "mention" ? "\(nick) tagged you in \(room)" : "\(nick) in \(room)"
        // A tag is addressed to you by name: it breaks through Focus modes the
        // way a highlight word, which you might have set loosely, should not.
        if event == "mention" { content.interruptionLevel = .timeSensitive }
        content.body = text
        content.userInfo = ["channel": channel]
        content.sound = .default
        UNUserNotificationCenter.current().add(
            UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }

    func openFromNotification(_ channel: String) {
        setActive(channel)
    }
}

/// A failure with something to say to the user. Anything else that escapes is a
/// bug and should look like one rather than being rendered as advice.
struct Fail: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

func now() -> Int64 { Int64(Date().timeIntervalSince1970 * 1000) }

enum Build {
    /// Baked in at build time by tools/deploy-device.sh and testflight.sh. A
    /// shipped app has no page origin to fall back on, so without this it opens
    /// on an empty server field.
    static let pinnedServer: String =
        Bundle.main.object(forInfoDictionaryKey: "GliphServer") as? String ?? ""
}

/// A question the core needs a human to answer before an action can fire.
struct Ask: Identifiable {
    let id = UUID()
    let message: String
    let wantsText: Bool
    /// Set for a question the host asked (not the core): the label on the yes
    /// button, and who hears the answer.
    var accept: String? = nil
    var onAnswer: ((Bool) -> Void)? = nil
}

/// URLSession's delegate for one file download: percent as it goes, the temp
/// file (moved synchronously, the OS deletes it after the callback) at the end.
private final class Download: NSObject, URLSessionDownloadDelegate {
    let progress: (Int) -> Void
    let done: (URL?) -> Void
    init(progress: @escaping (Int) -> Void, done: @escaping (URL?) -> Void) {
        self.progress = progress; self.done = done
    }
    func urlSession(_ s: URLSession, downloadTask: URLSessionDownloadTask, didWriteData _: Int64,
                    totalBytesWritten w: Int64, totalBytesExpectedToWrite total: Int64) {
        if total > 0 { progress(Int(w * 100 / total)) }
    }
    func urlSession(_ s: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo tmp: URL) {
        let ok = (downloadTask.response as? HTTPURLResponse)?.statusCode == 200
        done(ok ? tmp : nil)
        s.finishTasksAndInvalidate()
    }
    func urlSession(_ s: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        if error != nil { done(nil); s.invalidateAndCancel() }
    }
}
