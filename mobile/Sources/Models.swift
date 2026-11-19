//  What the core hands back.
//
//  Every one of these mirrors a `#[derive(Serialize)]` in core/src — the wire
//  between the app and the core is JSON, so that the same shape serves the web
//  client without being modelled twice in two binding languages. Adding a field
//  is one edit in Rust and one here, and the compiler finds the second.

import Foundation
import SwiftUI

// MARK: - tolerant decoding

/// Swift's synthesised `Codable` demands every non-optional key, and serde
/// omits fields it is told to skip (`enc`, `epoch`, `meta` on a plain event).
/// The two met, `[EventRow]` failed to decode as a whole, and the stream
/// rendered empty with nothing in the log.
///
/// So every model below decodes field by field with a default. That also makes
/// the app forward-compatible on purpose: a server or a core that grows a field
/// this build has never heard of is not a reason to show the user nothing.
private extension KeyedDecodingContainer {
    func or<T: Decodable>(_ key: Key, _ fallback: T) -> T {
        ((try? decodeIfPresent(T.self, forKey: key)) ?? nil) ?? fallback
    }
    func opt<T: Decodable>(_ key: Key) -> T? {
        (try? decodeIfPresent(T.self, forKey: key)) ?? nil
    }
}

// MARK: - the read models

struct User: Decodable, Hashable {
    var id = ""
    var nick = ""
    var role = "member"

    enum K: String, CodingKey { case id, nick, role }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        id = c.or(.id, ""); nick = c.or(.nick, ""); role = c.or(.role, "member")
    }
}

struct Member: Decodable, Hashable, Identifiable {
    var user_id = ""
    var nick = ""
    var role = "member"
    var away: String?
    var kind = "user"
    var online = false
    /// Has a device no other device of the account approved yet.
    var unverified = false

    enum K: String, CodingKey { case user_id, nick, role, away, kind, online, unverified }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        user_id = c.or(.user_id, ""); nick = c.or(.nick, "")
        role = c.or(.role, "member"); away = c.opt(.away); kind = c.or(.kind, "user")
        online = c.or(.online, false); unverified = c.or(.unverified, false)
    }

    var id: String { user_id.isEmpty ? nick : user_id }

    /// mIRC's sigils: @ is an op, + has voice, ◆ is not a person at all.
    var sigil: String {
        if kind != "user" { return "◆" }
        return role == "op" ? "@" : (role == "voice" ? "+" : " ")
    }
}

struct Channel: Decodable, Hashable, Identifiable {
    var id = ""
    var name = ""
    var topic = ""
    var modes = ""
    /// "open": joiners read what came before. "sealed": they start at their join.
    var history = "open"
    var is_dm = false
    var role = "member"
    var seq: Int64 = 0
    var members: [Member] = []
    var unread: Int64 = 0

    enum K: String, CodingKey { case id, name, topic, modes, history, is_dm, role, seq, members, unread }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        id = c.or(.id, ""); name = c.or(.name, ""); topic = c.or(.topic, "")
        modes = c.or(.modes, ""); history = c.or(.history, "open")
        is_dm = c.or(.is_dm, false); role = c.or(.role, "member")
        seq = c.or(.seq, 0); members = c.or(.members, []); unread = c.or(.unread, 0)
    }

    var encrypted: Bool { modes.contains("e") }
    var nearby: Bool { id.hasPrefix("n_") }
    var label: String { is_dm ? name : "#\(name)" }
}

/// A system message: the operator's MOTD, or the notes for this release. Drawn
/// as a card in a proportional face at the live end of the stream, which is the
/// one place the app leaves its monospace column on purpose.
struct SysMsg: Decodable, Hashable, Identifiable {
    var id = ""
    var kind = "news"
    /// A data URL (the operator's own icon), or "" for ours.
    var icon = ""
    var title = ""
    var body = ""
    var date = ""

    enum K: String, CodingKey { case id, kind, icon, title, body, date }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        id = c.or(.id, ""); kind = c.or(.kind, "news"); icon = c.or(.icon, "")
        title = c.or(.title, ""); body = c.or(.body, ""); date = c.or(.date, "")
    }
}

struct Notice: Decodable, Hashable {
    var channel = ""
    var level = "info"
    var text = ""

    enum K: String, CodingKey { case channel, level, text }
    init(text: String) { self.text = text }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        channel = c.or(.channel, ""); level = c.or(.level, "info"); text = c.or(.text, "")
    }
}

struct Command: Decodable, Hashable, Identifiable {
    var name = ""
    var usage = ""
    var desc = ""
    var scope = "any"
    var min_role = "member"
    var source = "builtin"

    enum K: String, CodingKey { case name, usage, desc, scope, min_role, source }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        name = c.or(.name, ""); usage = c.or(.usage, ""); desc = c.or(.desc, "")
        scope = c.or(.scope, "any"); min_role = c.or(.min_role, "member")
        source = c.or(.source, "builtin")
    }

    var id: String { name }
}

struct Transfer: Decodable, Hashable, Identifiable {
    var id = ""
    var dir = "recv"
    var peer = ""
    var peer_nick = ""
    var chan = ""
    var name = ""
    var mime = ""
    var size = 0
    var bytes = 0
    var acked = 0
    var state = "offered"
    var error: String?

    enum K: String, CodingKey {
        case id, dir, peer, peer_nick, chan, name, mime, size, bytes, acked, state, error
    }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        id = c.or(.id, ""); dir = c.or(.dir, "recv"); peer = c.or(.peer, "")
        peer_nick = c.or(.peer_nick, ""); chan = c.or(.chan, ""); name = c.or(.name, "")
        mime = c.or(.mime, ""); size = c.or(.size, 0); bytes = c.or(.bytes, 0)
        acked = c.or(.acked, 0); state = c.or(.state, "offered"); error = c.opt(.error)
    }

    /// On a send this is what the RECEIVER confirmed, not what went out — a bar
    /// that races ahead and then sits at 100% while nothing arrives is a lie.
    var delivered: Int { dir == "send" ? acked : bytes }
    var progress: Double { size == 0 ? 0 : min(1, Double(delivered) / Double(size)) }
    var active: Bool { state == "offered" || state == "running" }
}

/// One event, with the decryption already resolved by the core. `text` is nil
/// only when this device has no key for the epoch — which is what the honest
/// "cannot decrypt" line is rendered from.
struct EventRow: Decodable, Hashable, Identifiable {
    var seq: Double = 0
    var id = ""
    var channel = ""
    var kind = "message"
    var actor = ""
    var ts = ""
    var body = ""
    var enc = false
    var epoch: Int64 = 0
    var text: String?
    /// `<alice> what she said` when this is a reply, "" otherwise. The core
    /// lifts it off the body so no frontend parses one — see core/src/wire.rs.
    var quote = ""
    var highlight = false
    var meta: [String: AnyCodable]?

    enum K: String, CodingKey {
        case seq, id, channel, kind, actor, ts, body, enc, epoch, text, quote, highlight, meta
    }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        seq = c.or(.seq, 0); id = c.or(.id, ""); channel = c.or(.channel, "")
        kind = c.or(.kind, "message"); actor = c.or(.actor, ""); ts = c.or(.ts, "")
        body = c.or(.body, ""); enc = c.or(.enc, false); epoch = c.or(.epoch, 0)
        text = c.opt(.text); quote = c.or(.quote, "")
        highlight = c.or(.highlight, false); meta = c.opt(.meta)
    }

    var pending: Bool { meta?["pending"]?.bool ?? false }
    /// Which sub-room of room0 this was said in, "" for a line typed in the
    /// chat window. The stream draws it as a mark: [office], [dunes].
    var room: String { meta?["room"]?.string ?? "" }
    /// The event this answers, when it is one — the id the server recorded, so
    /// a quote can be followed back to what it quotes.
    var replyTo: String { meta?["reply_to"]?.string ?? "" }
    var locked: Bool { text == nil }
    var readable: String { text ?? "cannot decrypt — this device has no key for this message" }

    var at: Date {
        ISO8601DateFormatter().date(from: ts) ?? Date()
    }
}

struct Snapshot: Decodable {
    var server = ""
    var server_name = ""
    var signed_in = false
    var online = false
    var solo = false
    var active = ""
    var user: User?
    var channels: [Channel] = []
    var unread_total: Int64 = 0
    var notices: [Notice] = []
    var system: [SysMsg] = []
    var transfers: [Transfer] = []
    var nearby_on = false
    var nearby_peers = 0
    var upload_max: Int64 = 1 << 30
    var command_count = 0
    var plugin_command_count = 0
    var active_blob = ""
    /// `<alice> what she said` while a reply is pending, "" otherwise.
    var reply = ""
    /// One per channel with anybody in its 3D room. The room itself is web and
    /// desktop; the phone still says who is in there and what they said.
    var rooms: [RoomStrip] = []
    /// This device's standing with its account (docs/protocol.md #Device trust).
    var device = DeviceTrust()

    init() {}

    enum K: String, CodingKey {
        case server, server_name, signed_in, online, solo, active, user, channels
        case unread_total, notices, system, transfers, nearby_on, nearby_peers, upload_max
        case command_count, plugin_command_count, active_blob, reply, rooms, device
    }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        server = c.or(.server, ""); server_name = c.or(.server_name, "")
        signed_in = c.or(.signed_in, false); online = c.or(.online, false)
        solo = c.or(.solo, false); active = c.or(.active, ""); user = c.opt(.user)
        channels = c.or(.channels, []); unread_total = c.or(.unread_total, 0)
        notices = c.or(.notices, []); system = c.or(.system, [])
        transfers = c.or(.transfers, [])
        nearby_on = c.or(.nearby_on, false); nearby_peers = c.or(.nearby_peers, 0)
        upload_max = c.or(.upload_max, 1 << 30)
        command_count = c.or(.command_count, 0)
        plugin_command_count = c.or(.plugin_command_count, 0)
        active_blob = c.or(.active_blob, ""); reply = c.or(.reply, "")
        rooms = c.or(.rooms, []); device = c.or(.device, DeviceTrust())
    }
}

struct DeviceTrust: Decodable {
    var known = false
    var signed = false
    /// This device's approval code, to read off it.
    var code = ""
    /// The code as QR rows ("1" dark) of gliph://approve/<code>, while unsigned.
    var qr: [String] = []
    var pending: [PendingDevice] = []

    init() {}
    enum K: String, CodingKey { case known, signed, code, qr, pending }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        known = c.or(.known, false); signed = c.or(.signed, false)
        code = c.or(.code, ""); qr = c.or(.qr, []); pending = c.or(.pending, [])
    }
}

struct PendingDevice: Decodable, Hashable {
    var name = ""
    var code = ""
}

/// A channel's 3D room as the strip draws it. `since` is RFC3339 from the
/// server: the core keeps no clock, so the elapsed time is formatted here.
struct RoomStrip: Decodable, Hashable, Identifiable {
    var channel = ""
    var room = ""
    var since = ""
    var people: [RoomPerson] = []
    var voice_count = 0
    var you_inside = false
    var you_quiet = false

    var id: String { channel }

    enum K: String, CodingKey {
        case channel, room, since, people, voice_count, you_inside, you_quiet
    }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        channel = c.or(.channel, ""); room = c.or(.room, ""); since = c.or(.since, "")
        people = c.or(.people, []); voice_count = c.or(.voice_count, 0)
        you_inside = c.or(.you_inside, false); you_quiet = c.or(.you_quiet, false)
    }

    /// How long the room has been running, in the roughest unit that is true.
    var elapsed: String {
        guard let at = ISO8601DateFormatter().date(from: since) else { return "" }
        let m = Int(Date().timeIntervalSince(at) / 60)
        if m < 1 { return "just now" }
        if m < 60 { return "\(m)m" }
        return "\(m / 60)h"
    }
}

extension RoomStrip {
    /// The lit edge over the strip: the same gradient the web draws, one pixel
    /// tall, which is what makes it read as furniture rather than history.
    func gradientLine(_ t: Theme) -> some View {
        Rectangle().fill(t.r0Gradient).frame(height: 1)
    }
}

struct RoomPerson: Decodable, Hashable {
    var user_id = ""
    var name = ""
    var room = ""
    var voice = false

    enum K: String, CodingKey { case user_id, name, room, voice }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        user_id = c.or(.user_id, ""); name = c.or(.name, "")
        room = c.or(.room, ""); voice = c.or(.voice, false)
    }
}

// MARK: - the blob surface (GUM)

/// One app attached to a channel, as the switcher lists it.
struct BlobSpec: Decodable, Hashable, Identifiable {
    var id = ""
    var title = ""
    var mode = ""
    var source = "builtin"

    enum K: String, CodingKey { case id, title, mode, source }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        id = c.or(.id, ""); title = c.or(.title, "")
        mode = c.or(.mode, ""); source = c.or(.source, "builtin")
    }
}

struct MenuEntry: Decodable, Hashable, Identifiable {
    var id = ""
    var label = ""
    var selected = false

    enum K: String, CodingKey { case id, label, selected }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        id = c.or(.id, ""); label = c.or(.label, ""); selected = c.or(.selected, false)
    }
}

struct GumButton: Decodable, Hashable, Identifiable {
    var index = 0
    var label = ""
    var selected = false

    var id: Int { index }
    enum K: String, CodingKey { case index, label, selected }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        index = c.or(.index, 0); label = c.or(.label, ""); selected = c.or(.selected, false)
    }
}

/// A drawable node. The core resolved selection, numbering and submenus before
/// this got here — a frontend paints it and reports taps, and decides nothing.
enum GumNode: Decodable, Hashable, Identifiable {
    case text(body: String, style: String)
    case divider(label: String)
    case row(index: Int, number: Int, label: String, hint: String,
             state: String, hasActions: Bool, selected: Bool)
    case table(cols: [String], rows: [[String]])
    case input(index: Int, label: String, placeholder: String, value: String, selected: Bool)
    case buttons([GumButton])
    case bar(body: String)
    /// A node kind this build has never heard of. Skipped rather than fatal:
    /// the core may be newer than the app after an over-the-air core update.
    case unknown

    var id: String {
        switch self {
        case .row(let i, _, _, _, _, _, _): return "row\(i)"
        case .input(let i, _, _, _, _): return "input\(i)"
        case .buttons(let b): return "btns\(b.first?.index ?? 0)"
        case .text(let body, _): return "text:\(body.hashValue)"
        case .divider(let l): return "div:\(l.hashValue)"
        case .table(let c, _): return "table:\(c.hashValue)"
        case .bar(let b): return "bar:\(b.hashValue)"
        case .unknown: return "unknown"
        }
    }

    enum K: String, CodingKey {
        case kind, body, style, label, index, number, hint, state, has_actions, selected
        case cols, rows, placeholder, value, buttons
    }

    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        switch c.or(.kind, "") {
        case "Text": self = .text(body: c.or(.body, ""), style: c.or(.style, "plain"))
        case "Divider": self = .divider(label: c.or(.label, ""))
        case "Row":
            self = .row(index: c.or(.index, 0), number: c.or(.number, 0),
                        label: c.or(.label, ""), hint: c.or(.hint, ""),
                        state: c.or(.state, ""), hasActions: c.or(.has_actions, false),
                        selected: c.or(.selected, false))
        case "Table": self = .table(cols: c.or(.cols, []), rows: c.or(.rows, []))
        case "Input":
            self = .input(index: c.or(.index, 0), label: c.or(.label, ""),
                          placeholder: c.or(.placeholder, ""), value: c.or(.value, ""),
                          selected: c.or(.selected, false))
        case "Buttons": self = .buttons(c.or(.buttons, []))
        case "Bar": self = .bar(body: c.or(.body, ""))
        default: self = .unknown
        }
    }
}

struct Surface: Decodable {
    var blob = ""
    var title = ""
    var loaded = false
    var nodes: [GumNode] = []
    var menu: [MenuEntry]?
    var blobs: [BlobSpec] = []

    init() {}

    enum K: String, CodingKey { case blob, title, loaded, nodes, menu, blobs }
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: K.self)
        blob = c.or(.blob, ""); title = c.or(.title, ""); loaded = c.or(.loaded, false)
        nodes = c.or(.nodes, []); menu = c.opt(.menu); blobs = c.or(.blobs, [])
    }
}

// MARK: - effects

/// What the core asks this app to do. An enum rather than a dictionary so a new
/// effect in Rust is a compile error here rather than a silently ignored case.
enum Effect {
    case send(String)
    case persist(key: String, value: String, secret: Bool)
    case forget(key: String, secret: Bool)
    case alert(id: String, channel: String, nick: String, text: String, isDM: Bool, event: String)
    case changed
    case meshStart(nick: String)
    case meshStop
    case meshJoin(chans: [String])
    case meshSay(chan: String, body: String, kind: String, replyTo: String)
    case meshSendTo(peer: String, t: String, json: String)
    case saveFile(name: String, mime: String, bytes: Data)
    case pickFile
    case askConfirm(message: String)
    case askPrompt(message: String)
    case compose(title: String, text: String, submit: String)
    case theme(op: String, name: String, theme: String)

    static func list(_ json: String) -> [Effect] {
        guard let data = json.data(using: .utf8),
              let raw = try? JSONSerialization.jsonObject(with: data) as? [[String: Any]]
        else { return [] }
        return raw.compactMap(Effect.init(raw:))
    }

    private init?(raw: [String: Any]) {
        let s = { (k: String) in raw[k] as? String ?? "" }
        switch raw["kind"] as? String {
        case "Send": self = .send(s("json"))
        case "Persist": self = .persist(key: s("key"), value: s("value"), secret: s("store") == "Secret")
        case "Forget": self = .forget(key: s("key"), secret: s("store") == "Secret")
        case "Alert":
            self = .alert(id: s("id"), channel: s("channel"), nick: s("nick"),
                          text: s("text"), isDM: raw["is_dm"] as? Bool ?? false, event: s("event"))
        case "Changed": self = .changed
        case "MeshStart": self = .meshStart(nick: s("nick"))
        case "MeshStop": self = .meshStop
        case "MeshJoin": self = .meshJoin(chans: raw["chans"] as? [String] ?? [])
        case "MeshSay":
            self = .meshSay(chan: s("chan"), body: s("body"),
                            kind: s("msg_kind"), replyTo: s("reply_to"))
        case "MeshSendTo": self = .meshSendTo(peer: s("peer"), t: s("t"), json: s("json"))
        case "SaveFile":
            self = .saveFile(name: s("name"), mime: s("mime"),
                             bytes: Data(base64Encoded: s("bytes")) ?? Data())
        case "PickFile": self = .pickFile
        case "AskConfirm": self = .askConfirm(message: s("message"))
        case "AskPrompt": self = .askPrompt(message: s("message"))
        case "Compose": self = .compose(title: s("title"), text: s("text"), submit: s("submit"))
        case "Theme": self = .theme(op: s("op"), name: s("name"), theme: s("theme"))
        default: return nil
        }
    }
}

// MARK: - a JSON value that survives a round trip

/// `meta` is whatever the server put there. Decoding it into a concrete type
/// would mean this client deciding what the server is allowed to say, which is
/// exactly the coupling docs/protocol.md avoids.
struct AnyCodable: Decodable, Hashable {
    let value: AnyHashable?

    var bool: Bool? { value as? Bool }
    var string: String? { value as? String }
    var int: Int? { value as? Int }

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { value = nil }
        else if let b = try? c.decode(Bool.self) { value = b }
        else if let i = try? c.decode(Int.self) { value = i }
        else if let d = try? c.decode(Double.self) { value = d }
        else if let s = try? c.decode(String.self) { value = s }
        else if let a = try? c.decode([AnyCodable].self) { value = a }
        else if let o = try? c.decode([String: AnyCodable].self) { value = o }
        else { value = nil }
    }
}

// MARK: - decoding helpers

extension JSONDecoder {
    static let core = JSONDecoder()
}

func decode<T: Decodable>(_ type: T.Type, from json: String, or fallback: T) -> T {
    guard let data = json.data(using: .utf8) else { return fallback }
    do {
        return try JSONDecoder.core.decode(T.self, from: data)
    } catch {
        // Never silently: a decode that fails renders an empty screen, and an
        // empty screen with nothing in the log is what cost an hour once.
        NSLog("gliph: could not decode %@ — %@", String(describing: T.self),
              String(describing: error))
        return fallback
    }
}

// MARK: - servers (the switcher)

/// One signed-in server, as core/src/servers.rs persists it. A Gliph server IS
/// a team, so this list is the team list — and your nick is per-server, because
/// those are rows in two different databases.
struct ServerEntry: Codable, Identifiable, Equatable {
    var host: String
    var token: String = ""
    var name: String = ""
    var nick: String = ""
    var icon: String = ""
    /// Unread, never mentions: a sealed channel's body is ciphertext to the
    /// server, so a count is the most it can honestly report.
    var unread: Int = 0
    var checked_ms: Int64 = 0

    var id: String { host }

    /// The address without its scheme, for the line under the name.
    var shortHost: String {
        host.replacingOccurrences(of: "https://", with: "")
            .replacingOccurrences(of: "http://", with: "")
    }

    /// Two letters for a server with no icon of its own.
    var monogram: String {
        let src = (name.isEmpty ? shortHost : name).filter { $0.isLetter || $0.isNumber }
        return String(src.prefix(2)).uppercased()
    }
}

struct ServerList: Codable {
    var list: [ServerEntry] = []
    /// Host of the server on screen. Always names an entry in `list`.
    var active: String = ""
}
