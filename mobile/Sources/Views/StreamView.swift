//  The scrollback. One line per event, IRC's shape: a timestamp, who, what.
//
//  Everything is text in one monospace face on purpose (spec F10/F12): the day
//  rule is `-- mon aug 25 2026 --`, a system line starts with `***`, and ASCII
//  art pasted into a channel lines up column for column because nothing
//  reflows it.

import SwiftUI

struct StreamView: View {
    @Environment(\.theme) private var t
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @EnvironmentObject private var store: Store
    let rows: [EventRow]
    let notices: [Notice]
    let system: [SysMsg]
    let online: Bool
    let solo: Bool
    let nearby: Bool
    /// Whose stream this is, as a user id. Only the room invite needs it: an
    /// ask addressed to you reads differently from one you sent.
    var me: String = ""
    /// A tap on @nick. The link rides a gliph://dm/<nick> URL through
    /// SwiftUI's openURL, which is the only way a run inside one Text can act.
    var onMention: (String) -> Void = { _ in }
    /// A tap on a file name, same trick: gliph://file/<id>?n=<name>.
    var onFile: (String, String) -> Void = { _, _ in }
    /// Long-press → reply. The web client's right-click menu, on the gesture a
    /// phone has for the same thing.
    var onReply: (String) -> Void = { _ in }
    /// The × on a system card. Closing one is remembered by the core, so it
    /// stays closed on this phone.
    var onDismissSystem: (String) -> Void = { _ in }

    /// The line most recently jumped to, lit until the eye finds it.
    @State private var flashed = ""

    var body: some View {
        // A quote is a way back only when the thing it answers is still in the
        // scrollback. One that has scrolled out stays readable and simply is
        // not tappable, which beats a tap that does nothing.
        let onScreen = Set(rows.map(\.id))
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 3) {
                    Splash()
                    ForEach(Array(dated.enumerated()), id: \.offset) { _, item in
                        switch item {
                        case .day(let label):
                            Text("-- \(label) --")
                                .font(Face.mono(11)).foregroundStyle(t.muted)
                                .padding(.horizontal, 12).padding(.vertical, 6)
                        case .event(let e):
                            Row(e, proxy, onScreen).id(e.id)
                        // At the live end, under the newest line: the MOTD and
                        // any release notes this phone has not read. They ride
                        // in the same list as the events rather than in a
                        // ForEach of their own — a second ForEach after this
                        // one is built by SwiftUI and then never laid out.
                        case .system(let m):
                            SystemCard(msg: m, onDismiss: { onDismissSystem(m.id) })
                        }
                    }
                    ForEach(Array(notices.suffix(8).enumerated()), id: \.offset) { _, n in
                        Text(n.text)
                            .font(Face.mono(12))
                            .foregroundStyle(n.level == "error" ? t.accent : t.muted)
                            .padding(.horizontal, 12)
                    }
                    // A nearby channel does not care that the server is down —
                    // saying messages "will queue" there would be a lie about
                    // the one thing still working.
                    if !online && !solo && !nearby {
                        Text("*** disconnected — messages you send will queue until the server is back")
                            .font(Face.mono(12)).foregroundStyle(t.accent)
                            .padding(.horizontal, 12)
                    }
                    Color.clear.frame(height: 1).id("bottom")
                }
                .padding(.vertical, 8)
            }
            // The newest line, not the oldest. `count` alone missed the two
            // cases that matter: coming back from a blob tab rebuilds this
            // view, which starts at the top, and switching channels can land
            // on a room with exactly as many lines as the last one.
            .onChange(of: rows.last?.id) { _ in pin(proxy) }
            // Switching channel keeps this view and swaps its rows, and the two
            // rooms can end on the same line (a `state` frame re-sending it, or
            // simply the same last message id after a rebuild). The active
            // channel changing is the honest signal for "this is a new stream".
            .onChange(of: store.snap.active) { _ in
                pin(proxy)
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { pin(proxy) }
            }
            .onAppear {
                // Twice, and neither is superstition: the first hop is because
                // nothing is laid out yet on the first pass, the second is
                // because a LazyVStack only measures the rows it has built, so
                // the first landing is short by whatever it built on the way.
                pin(proxy)
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { pin(proxy) }
            }
            .environment(\.openURL, OpenURLAction { url in
                if url.scheme == "gliph", url.host == "dm" {
                    onMention(url.lastPathComponent)
                    return .handled
                }
                if url.scheme == "gliph", url.host == "file" {
                    let name = URLComponents(url: url, resolvingAgainstBaseURL: false)?
                        .queryItems?.first { $0.name == "n" }?.value ?? "file"
                    onFile(url.lastPathComponent, name)
                    return .handled
                }
                return .systemAction
            })
        }
    }

    private func pin(_ proxy: ScrollViewProxy) {
        DispatchQueue.main.async {
            withAnimation(.none) { proxy.scrollTo("bottom", anchor: .bottom) }
        }
    }

    private enum Item {
        case day(String)
        case event(EventRow)
        case system(SysMsg)
    }

    private var dated: [Item] {
        var out: [Item] = []
        var day = ""
        for e in rows {
            let key = Self.dayLabel(e.at)
            if key != day {
                day = key
                out.append(.day(key))
            }
            out.append(.event(e))
        }
        out.append(contentsOf: system.map(Item.system))
        return out
    }

    static func dayLabel(_ d: Date) -> String {
        let f = DateFormatter()
        f.dateFormat = "EEE MMM d yyyy"
        return f.string(from: d).lowercased()
    }

    /// Bytes as the stream says them, matching the web client exactly.
    /// Agent brands with a mark in Assets.xcassets (brand-<name>), the same
    /// allowlist the server keeps (server/plugin.go agentBrands).
    static let brands: Set<String> = ["claude", "openai", "gemini", "cursor", "copilot", "github", "gitlab"]

    static func size(_ n: Int) -> String {
        n > 1 << 20 ? String(format: "%.1f MB", Double(n) / 1048576) : "\((n + 1023) / 1024) KB"
    }

    static func hhmm(_ d: Date) -> String {
        let f = DateFormatter()
        f.dateFormat = "HH:mm"
        return f.string(from: d)
    }

    /// Kinds worth a long-press. Nobody replies to a join line.
    private static let actionable = ["message", "action", "agent", "file"]

    @ViewBuilder
    private func Row(_ e: EventRow, _ proxy: ScrollViewProxy, _ onScreen: Set<String>) -> some View {
        // A reply puts the clock on the quote line instead, the way the web
        // client does, so the pair reads as one block with one timestamp.
        let time = e.quote.isEmpty
            ? Text("[\(Self.hhmm(e.at))] ").font(Face.mono(11.5)).foregroundColor(t.muted)
            : Text("")

        VStack(alignment: .leading, spacing: 1) {
            if !e.quote.isEmpty {
                let jump = onScreen.contains(e.replyTo) ? e.replyTo : ""
                (Text("[\(Self.hhmm(e.at))] ") + Text(e.quote).underline(!jump.isEmpty))
                    .font(Face.mono(11.5)).foregroundColor(t.muted)
                    .lineLimit(1).truncationMode(.tail)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .contentShape(Rectangle())
                    .onTapGesture {
                        guard !jump.isEmpty else { return }
                        withAnimation(reduceMotion ? nil : .default) { proxy.scrollTo(jump, anchor: .center) }
                        // Lit at once, faded out afterwards: a flash that
                        // eases IN is not a flash, it is a slow reveal.
                        flashed = jump
                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) {
                            withAnimation(reduceMotion ? nil : .easeOut(duration: 1.2)) { flashed = "" }
                        }
                    }
            }
            HStack(alignment: .top, spacing: 8) {
                // the `>` marker under a quote, matching the web client
                if !e.quote.isEmpty {
                    Text(">").font(Face.mono(12.5)).foregroundColor(t.border)
                }
                Inner(e, time, proxy)
            }
            .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.horizontal, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(e.id == flashed ? t.gold.opacity(0.45)
                    : e.highlight ? t.gold.opacity(0.14) : .clear)
        .contextMenu {
            if Self.actionable.contains(e.kind), !e.locked {
                Button("reply") { onReply(e.id) }
                Button("copy text") { UIPasteboard.general.string = e.readable }
            }
        }
    }

    /// Where an image line's picture comes from: a file id on the server, or —
    /// for a transfer that went peer to peer, which the server never saw — the
    /// copy this device is holding, keyed by name and size the way the web
    /// client keys it. Nothing to show if neither is there.
    private func attachmentID(_ e: EventRow) -> String? {
        guard e.meta?["p2p"]?.bool == true else { return e.meta?["file_id"]?.string }
        let key = "\(e.body)|\(e.meta?["size"]?.int ?? 0)"
        return store.p2pBytes(key) == nil ? nil : "p2p:" + key
    }

    @ViewBuilder
    private func Inner(_ e: EventRow, _ time: Text, _ proxy: ScrollViewProxy) -> some View {
        if e.kind == "message", !e.locked, e.readable.contains("```") {
            // A fenced block cannot live inside one concatenated Text: it is
            // its own view, folded past seven lines like the web client.
            VStack(alignment: .leading, spacing: 4) {
                ForEach(Array(Self.fences(e.readable).enumerated()), id: \.offset) { i, part in
                    switch part {
                    case .prose(let p):
                        if i == 0 {
                            (time + nickText(e.actor) + inline(p)).font(Face.mono(13))
                        } else if !p.isEmpty {
                            inline(p).font(Face.mono(13))
                        }
                    case .code(let lang, let code):
                        if i == 0 { (time + nickText(e.actor)).font(Face.mono(13)) }
                        CodeBlock(lang: lang, code: code)
                    }
                }
            }
            .textSelection(.enabled)
            .opacity(e.pending ? 0.55 : 1)
            .frame(maxWidth: .infinity, alignment: .leading)
        } else if e.kind == "file", Self.isImageName(e.body), let id = attachmentID(e) {
            // An image previews under its line, as on web: half the screen at
            // most, cut off there, a tap for the rest of it.
            VStack(alignment: .leading, spacing: 4) {
                (time + content(e))
                    .font(Face.mono(13))
                    .textSelection(.enabled)
                Attachment(id: id, name: e.body, onReady: {
                    // The row just changed height, after the layout that put
                    // the reader on the newest line. Only for one near the end:
                    // a picture settling ten screens up belongs to somebody
                    // reading history, and yanking them to the bottom for it
                    // would be the same bug pointed the other way.
                    guard rows.suffix(3).contains(where: { $0.id == e.id }) else { return }
                    pin(proxy)
                })
            }
            .opacity(e.pending ? 0.55 : 1)
            .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            (time + content(e))
            .font(Face.mono(13))
            .textSelection(.enabled)
            .opacity(e.pending ? 0.55 : 1)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// The web client's list, minus SVG for the same reason: it is a document
    /// that can carry script and remote references, not a picture.
    static func isImageName(_ name: String) -> Bool {
        ["png", "jpg", "jpeg", "gif", "webp", "avif", "bmp", "ico"]
            .contains((name as NSString).pathExtension.lowercased())
    }

    private func nickText(_ n: String) -> Text {
        Text("<\(n)> ").font(Face.mono(13, .bold)).foregroundColor(t.nick(n))
    }

    /// `like this` on one line reads as code: a chip in the other theme's
    /// surface, as on web (theme.css code.inl), so it stands off the line in both.
    private func inline(_ s: String) -> Text {
        let other = t.isDark ? Theme.creme : Theme.midnight
        func chip(_ w: Substring) -> Text {
            var a = AttributedString(String(w))
            a.font = Face.mono(12.5, .bold)
            a.foregroundColor = other.text
            a.backgroundColor = other.surface2
            return Text(a)
        }
        var out = Text("")
        var rest = Substring(s)
        while let open = rest.firstIndex(of: "`") {
            let after = rest.index(after: open)
            if let close = rest[after...].firstIndex(of: "`"), !rest[after..<close].contains("\n"), after < close {
                out = out + prose(rest[..<open])
                out = out + chip(rest[after..<close])
                rest = rest[rest.index(after: close)...]
            } else {
                out = out + prose(rest[...open])
                rest = rest[after...]
            }
        }
        return out + prose(rest)
    }

    /// Plain text with @nick runs turned into tappable, azure, dashed links.
    private func prose(_ s: Substring) -> Text {
        var out = Text("")
        var rest = s
        while let at = rest.firstIndex(of: "@") {
            let before = at == rest.startIndex ? " " : rest[rest.index(before: at)]
            let nameStart = rest.index(after: at)
            let nameEnd = rest[nameStart...].firstIndex { !($0.isLetter || $0.isNumber || $0 == "_" || $0 == "-") } ?? rest.endIndex
            if (before == " " || before == "(" || before == "\n"), nameStart < nameEnd {
                out = out + Text(rest[..<at]).foregroundColor(t.text)
                let nick = String(rest[nameStart..<nameEnd])
                var link = AttributedString("@" + nick)
                link.link = URL(string: "gliph://dm/\(nick)")
                link.foregroundColor = t.link
                link.underlineStyle = .patternDash
                out = out + Text(link)
                rest = rest[nameEnd...]
            } else {
                out = out + Text(rest[...at]).foregroundColor(t.text)
                rest = rest[nameStart...]
            }
        }
        return out + Text(rest).foregroundColor(t.text)
    }

    enum Part { case prose(String), code(lang: String, code: String) }

    /// Split a body on ``` fences. Even pieces are prose, odd ones code whose
    /// first line names the language. An unclosed fence is just text.
    static func fences(_ body: String) -> [Part] {
        let pieces = body.components(separatedBy: "```")
        guard pieces.count >= 3 else { return [.prose(body)] }
        var out: [Part] = []
        for (i, piece) in pieces.enumerated() {
            if i % 2 == 0 {
                let p = piece.trimmingCharacters(in: .newlines)
                if !p.isEmpty || i == 0 { out.append(.prose(p)) }
            } else if i < pieces.count - 1 || pieces.count % 2 == 1 {
                let nl = piece.firstIndex(of: "\n")
                let lang = nl.map { String(piece[..<$0]) } ?? ""
                let code = nl.map { String(piece[piece.index(after: $0)...]) } ?? ""
                out.append(.code(lang: lang, code: code.trimmingCharacters(in: .newlines)))
            } else {
                out.append(.prose("```" + piece))
            }
        }
        return out
    }

    /// One Text per event rather than a stack: concatenated `Text` is what lets
    /// the timestamp, the nick and the body wrap as one paragraph, which is the
    /// difference between a chat log and a table.
    private func content(_ e: EventRow) -> Text {
        func nick(_ n: String, _ c: Color? = nil) -> Text {
            Text("<\(n)> ").font(Face.mono(13, .bold)).foregroundColor(c ?? t.nick(n))
        }
        func sys(_ s: String, _ c: Color) -> Text {
            Text(s).font(Face.mono(12)).foregroundColor(c)
        }
        // Said in the 3D room: the line carries the room it was said in.
        // SwiftUI cannot pour a gradient into concatenated Text, so the mark
        // takes the near end of the same pair the web fades between.
        func mark(_ e: EventRow) -> Text {
            e.room.isEmpty
                ? Text("")
                : Text("[\(e.room)] ").font(Face.mono(12, .bold)).foregroundColor(t.r0From)
        }
        let body = Text(e.readable).foregroundColor(e.locked ? t.muted : t.text)

        switch e.kind {
        case "message":
            return e.locked
                ? mark(e) + nick(e.actor) + Text(e.readable).italic().foregroundColor(t.muted)
                : mark(e) + nick(e.actor) + inline(e.readable)
        case "action":
            return mark(e) + sys("* \(e.actor) \(e.readable)", t.action)
        case "room_invite":
            // room0 runs on web and desktop, so the phone reads the ask and
            // says where to answer it.
            let who = e.meta?["target"]?.string ?? ""
            let mine = who == me
            let nick = e.meta?["target_nick"]?.string ?? "someone"
            let what = who.isEmpty
                ? "*** \(e.actor) opened room0 · \(e.body)"
                : mine
                ? "*** \(e.actor) asked you into room0 · \(e.body)"
                : "*** \(e.actor) asked \(nick) into room0 · \(e.body)"
            return sys(what, t.r0From) + sys("  open on desktop", t.muted)
        case "agent":
            // A plugin may name the agent behind a line (server allowlist):
            // its own mark replaces the diamond, as on web (icons.brand).
            if let b = e.meta?["agent"]?.string, Self.brands.contains(b) {
                return Text("<").font(Face.mono(13, .bold)).foregroundColor(t.agent)
                    + Text(Image("brand-\(b)")).foregroundColor(t.agent)
                    + Text("\(e.actor)> ").font(Face.mono(13, .bold)).foregroundColor(t.agent)
                    + Text("[AGENT] ").font(Face.mono(11, .bold)).foregroundColor(t.agent)
                    + Text(e.body).foregroundColor(t.text)
            }
            return nick("◆\(e.actor)", t.agent)
                + Text("[AGENT] ").font(Face.mono(11, .bold)).foregroundColor(t.agent)
                + Text(e.body).foregroundColor(t.text)
        case "join":
            return sys("*** \(e.actor) has joined\(e.body.isEmpty ? "" : " — \(e.body)")", t.join)
        case "part":
            return sys("*** \(e.actor) has left", t.join)
        case "mode":
            return sys("*** \(e.actor) \(e.body)", t.mode)
        case "topic":
            return sys("*** \(e.actor) set the topic: \(e.body)", t.mode)
        case "kick":
            let who = e.meta?["target"]?.string ?? ""
            return sys("*** \(who) was kicked by \(e.actor) (\(e.body))", t.accent)
        case "notice":
            return sys(e.actor == "*" ? e.body : "*** \(e.actor) \(e.body)", t.muted)
        case "file":
            let at = Text("@ ").foregroundColor(t.muted)
            let mb = Self.size(e.meta?["size"]?.int ?? 0)
            // A nearby transfer went peer to peer: only the two ends have this
            // line, and there is no server holding the bytes to link to. What
            // arrived was saved as it landed; the sender has the original.
            if e.meta?["p2p"]?.bool == true {
                let peer = e.meta?["peer"]?.string ?? ""
                let what = e.meta?["dir"]?.string == "send"
                    ? Text("you sent \(peer) a file: ").foregroundColor(t.text) + nick(e.body, t.text)
                    : nick(e.actor) + Text("sent you a file: ").foregroundColor(t.text) + nick(e.body, t.text)
                return at + what + Text("(\(mb) · direct)").foregroundColor(t.muted)
            }
            var name = AttributedString(e.body)
            name.foregroundColor = t.link; name.underlineStyle = .patternDash
            var u = URLComponents(); u.scheme = "gliph"; u.host = "file"
            u.path = "/" + (e.meta?["file_id"]?.string ?? ""); u.queryItems = [.init(name: "n", value: e.body)]
            name.link = u.url
            return at + nick(e.actor) + Text("sent a file: ").foregroundColor(t.text) + Text(name)
        default:
            return body
        }
    }
}


/// A fenced block: language label, the code in mono, seven lines then a fold.
/// No colouring — the phone has no Monaco, and a highlighter is a dependency
/// for the least-used screen a snippet lands on.
struct CodeBlock: View {
    let lang: String
    let code: String
    @State private var open = false

    private var lines: Int { code.split(separator: "\n", omittingEmptySubsequences: false).count }
    private var wraps: Bool { ["", "md", "markdown", "txt", "text", "plaintext", "plain"].contains(lang.lowercased()) }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                Text(lang.isEmpty ? "CODE" : lang.uppercased())
                    .font(Face.mono(10.5, .bold)).foregroundStyle(Theme.midnight.agent)
                Spacer()
                if lines > 7 {
                    Button(open ? "collapse" : "expand · \(lines) lines") { open.toggle() }
                        .font(Face.mono(10.5, .bold)).foregroundStyle(Theme.midnight.join)
                }
            }
            // Prose fences (markdown, text) wrap like text; code scrolls sideways.
            if wraps {
                Text(code)
                    .font(Face.mono(12)).foregroundStyle(Theme.midnight.text)
                    .lineLimit(open || lines <= 7 ? nil : 7)
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    Text(code)
                        .font(Face.mono(12)).foregroundStyle(Theme.midnight.text)
                        .lineLimit(open || lines <= 7 ? nil : 7)
                        .fixedSize(horizontal: true, vertical: false)
                }
            }
        }
        .padding(.horizontal, 10).padding(.vertical, 6)
        // Code is always dark, both themes, like the web client.
        .background(Theme.midnight.surface)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}


/// An inline image preview. The bytes come from the server behind the same
/// token as everything else, so this cannot be an AsyncImage — it fetches with
/// the auth header and holds the decoded picture for as long as the row lives.
struct Attachment: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    let id: String
    let name: String
    /// Called once, when the bytes have just arrived and the row grew.
    var onReady: () -> Void = {}

    /// Pictures already decoded this session, and the room the reader gave
    /// them. A LazyVStack throws a row away the moment it scrolls off screen,
    /// so without these, scrolling back re-downloads every picture and the
    /// stream lurches as each one lands.
    private static let cache = NSCache<NSString, UIImage>()
    private static var heights: [String: CGFloat] = [:]
    /// Files whose bytes could not be had this session. A dead one fails once:
    /// without this it is re-asked for on every scroll back, and holds half a
    /// screen of nothing open each time while it waits to fail again.
    private static var dead: Set<String> = []

    @State private var image: UIImage?
    @State private var failed: Bool
    @State private var open = false
    /// How much room the reader gave this one. nil until they touch [ - ]/[ + ].
    @State private var shown: CGFloat?

    // What is already known is filled in here rather than in `task`, which runs
    // after the first layout: seeded there, a picture this phone already holds
    // still flashes half a screen of placeholder and then jumps.
    init(id: String, name: String, onReady: @escaping () -> Void = {}) {
        self.id = id
        self.name = name
        self.onReady = onReady
        _image = State(initialValue: Self.cache.object(forKey: id as NSString))
        _failed = State(initialValue: Self.dead.contains(id))
        _shown = State(initialValue: Self.heights[id])
    }

    private var base: CGFloat { UIScreen.main.bounds.height / 2 }
    private var step: CGFloat { UIScreen.main.bounds.height / 4 }

    /// The picture at its full height in the stream's width — the ceiling for
    /// [ + ]. 100% is the whole thing; past that is the lightbox's job.
    private func full(_ img: UIImage) -> CGFloat {
        (UIScreen.main.bounds.width - 24) * img.size.height / max(img.size.width, 1)
    }

    /// A size the reader chose outlives the row it was chosen on.
    private func size(_ h: CGFloat) {
        shown = h
        Self.heights[id] = h
    }

    var body: some View {
        Group {
            if let image {
                let cap = min(shown ?? base, full(image))
                Image(uiImage: image)
                    .resizable().aspectRatio(contentMode: .fill)
                    .frame(maxWidth: .infinity, maxHeight: cap, alignment: .top)
                    .clipped()
                    .overlay(alignment: .topTrailing) {
                        if full(image) > cap + 1 || shown != nil {
                            HStack(spacing: 2) {
                                Button("[ - ]") { size(max(120, cap - step)) }
                                    .accessibilityLabel("smaller")
                                Button("[ + ]") { size(min(full(image), cap + step)) }
                                    .accessibilityLabel("larger")
                            }
                            .font(Face.mono(11, .bold))
                            .foregroundColor(.white)
                            .padding(.horizontal, 5).padding(.vertical, 3)
                            .background(.black.opacity(0.55))
                        }
                    }
                    .border(t.border)
                    .contentShape(Rectangle())
                    .onTapGesture { open = true }
            } else if failed {
                // The bytes are gone (an expired upload, a file that went peer
                // to peer to somebody else's phone). One line saying so, not
                // half a screen of nothing: the name is already on the line
                // above this one.
                Text("[ preview unavailable ]")
                    .font(Face.mono(12)).foregroundStyle(t.muted)
            } else {
                // Hold half a screen while the bytes are on their way. The
                // picture almost always fills it, so the stream does not lurch
                // when it lands — and the reader keeps the line they were on.
                Rectangle().fill(t.surface2)
                    .frame(maxWidth: .infinity)
                    .frame(height: base)
                    .border(t.border)
            }
        }
        .task {
            guard image == nil, !failed else { return }
            // "p2p:<name>|<size>" is a file this device already holds: it went
            // peer to peer and there is no server copy to fetch.
            let bytes = id.hasPrefix("p2p:")
                ? store.p2pBytes(String(id.dropFirst(4)))
                : await store.fileBytes(id: id)
            guard let data = bytes, let got = UIImage(data: data) else {
                // Only a failure with the server on the line says the file is
                // gone. Offline, this row simply has not had its turn yet, so
                // the next time it is built it asks again.
                if store.snap.online { Self.dead.insert(id) }
                failed = true
                // The row just lost half a screen; a reader parked at the
                // bottom has to be put back on it.
                onReady()
                return
            }
            Self.cache.setObject(got, forKey: id as NSString)
            image = got
            onReady()
        }
        .fullScreenCover(isPresented: $open) {
            if let image { Lightbox(image: image, name: name, id: id) }
        }
    }
}

/// The whole picture: pinch to zoom, save hands it to the share sheet, done
/// closes. Black either theme — a photo viewer is not a chat surface.
private struct Lightbox: View {
    @EnvironmentObject private var store: Store
    @Environment(\.dismiss) private var dismiss
    let image: UIImage
    let name: String
    let id: String

    @State private var scale: CGFloat = 1

    var body: some View {
        // The picture sits in the middle of the screen; the bar rides over it
        // rather than pushing it down, so a wide photo is not pinned to the top
        // with all the black underneath it.
        ZStack {
            Color.black.ignoresSafeArea()
            Image(uiImage: image)
                .resizable().aspectRatio(contentMode: .fit)
                .scaleEffect(scale)
                .gesture(MagnificationGesture()
                    .onChanged { scale = min(max($0, 1), 5) }
                    .onEnded { _ in if scale < 1.05 { withAnimation { scale = 1 } } })
        }
        .overlay(alignment: .top) {
            HStack {
                Text(name).lineLimit(1).truncationMode(.middle)
                Spacer()
                Button("save") { store.saveFile(id: id, name: name) }
                Button("done") { dismiss() }
            }
            .font(Face.mono(12, .bold))
            .foregroundColor(.white)
            .padding(.horizontal, 14).padding(.vertical, 10)
            .background(.black.opacity(0.6))
        }
    }
}

/// A system message: the MOTD, or the notes for a release.
///
/// The one card in the stream that is not chat, and it says so by leaving the
/// monospace column — proportional type, an icon, a rule around it. Three
/// lines until the reader asks for the rest, because a release note is allowed
/// to be long and the scrollback is not allowed to be swallowed by one.
struct SystemCard: View {
    @Environment(\.theme) private var t
    let msg: SysMsg
    var onDismiss: () -> Void = {}
    @State private var open = false

    private var icon: Image {
        // Inline bytes or ours. Never a URL from anywhere else: fetching a
        // remote image to draw a line of chat tells its owner who read the
        // channel and when.
        if msg.kind == "motd", let comma = msg.icon.range(of: "base64,"),
           let data = Data(base64Encoded: String(msg.icon[comma.upperBound...])),
           let img = UIImage(data: data) {
            return Image(uiImage: img)
        }
        return Image("unicorn")
    }

    private var when: String {
        let iso = ISO8601DateFormatter()
        iso.formatOptions = [.withFullDate]
        guard let d = iso.date(from: msg.date) else { return "" }
        return d.formatted(.dateTime.day().month(.wide))
    }

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            icon.resizable().frame(width: 30, height: 30)
                .overlay(Rectangle().stroke(t.border2, lineWidth: 1))
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 8) {
                    Text(msg.kind == "motd" ? "message of the day" : "what's new")
                        .font(.system(size: 10.5, weight: .bold))
                        .textCase(.uppercase).kerning(0.8)
                        .foregroundStyle(t.accent)
                    if !when.isEmpty {
                        Text(when).font(.system(size: 11)).foregroundStyle(t.muted)
                    }
                }
                if !msg.title.isEmpty {
                    Text(msg.title).font(.system(size: 15, weight: .semibold)).foregroundStyle(t.text)
                }
                Text(msg.body)
                    .font(.system(size: 13.5)).foregroundStyle(t.text2)
                    .lineLimit(open ? nil : 3)
                    .fixedSize(horizontal: false, vertical: true)
                // ponytail: length, not a measured overflow. SwiftUI cannot say
                // whether a Text clipped without measuring it twice, and three
                // lines on a phone is about this many characters. The web
                // client measures because there it is free.
                if msg.body.count > 110 {
                    Button(open ? "show less" : "read more") { open.toggle() }
                        .font(.system(size: 12.5, weight: .semibold))
                        .foregroundStyle(t.link)
                        .padding(.top, 2)
                }
            }
            Spacer(minLength: 0)
            // Only the MOTD closes. A release note lives in #system, and a
            // log you can empty is not a log.
            if msg.kind == "motd" {
                Button(action: onDismiss) {
                    Text("×").font(.system(size: 17)).foregroundStyle(t.muted)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("dismiss")
            }
        }
        .padding(12)
        .background(t.surface)
        .padding(.horizontal, 10)
        .padding(.vertical, 4)
    }
}
