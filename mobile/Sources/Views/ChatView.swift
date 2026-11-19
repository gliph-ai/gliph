//  The chat screen: chrome at the top, scrollback in the middle, composer at
//  the bottom, status line under that. The rail slides in from the left under
//  the channel name, over the conversation rather than instead of it, because
//  a phone has one column and switching channel is a glance at the list, not a
//  screen of its own.

import SwiftUI
import UniformTypeIdentifiers

struct ChatView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    @State private var draft = ""
    @State private var railOpen = false
    /// On an iPad (regular width) the rail is a docked sidebar instead of a
    /// drawer; the # key collapses and restores it, and the choice sticks.
    @AppStorage("railDocked") private var railDocked = true
    @Environment(\.horizontalSizeClass) private var sizeClass
    private var wide: Bool { sizeClass == .regular }
    @State private var transfersOpen = false
    @State private var settingsOpen = false
    @State private var serversOpen = false
    @State private var picking = false
    @State private var clipHasImage = UIPasteboard.general.hasImages
    /// What a prompt's field holds. SwiftUI's `Alert` has no text field, so a
    /// prompt asks for confirmation of what the composer already has — the one
    /// place a phone and a browser genuinely differ here.
    @State private var answerText = ""
    @FocusState private var composerFocused: Bool

    private var channel: Channel? { store.channel() }

    var body: some View {
        VStack(spacing: 0) {
            menubar
            titlestrip
            HStack(spacing: 0) {
            if wide && railDocked {
                // A constant binding: picking a channel must not collapse a sidebar.
                RailView(open: .constant(true))
                    .frame(width: 280)
                    .overlay(Rectangle().frame(width: 1).foregroundStyle(t.border), alignment: .trailing)
                    .transition(.move(edge: .leading))
            }
            ZStack(alignment: .topLeading) {
                conversation
                    // Swipe in from the left edge to open it. Simultaneous, so
                    // the stream keeps its own scrolling: a drag that started
                    // anywhere but the edge, or went any way but right, simply
                    // does not match.
                    .simultaneousGesture(DragGesture(minimumDistance: 20).onEnded { g in
                        if !wide, !railOpen, g.startLocation.x < 30, g.translation.width > 60 {
                            railOpen = true
                        }
                    })
                if railOpen && !wide { rail }
            }
            .animation(reduceMotion ? nil : .easeOut(duration: 0.18), value: railOpen)
            }
            .animation(reduceMotion ? nil : .easeOut(duration: 0.18), value: railDocked)
            statusbar
        }
        .background(t.surface)
        .ignoresSafeArea(.container, edges: .bottom)
        // A sheet is presented from a new root, so the store has to be handed
        // in explicitly — inheriting it looked like it worked and then served a
        // snapshot frozen at the moment the sheet was built.
        .sheet(isPresented: $settingsOpen) {
            SettingsView(open: $settingsOpen).environmentObject(store).environment(\.theme, t)
        }
        .sheet(isPresented: $transfersOpen) {
            TransfersView().environmentObject(store).environment(\.theme, t)
        }
        .sheet(item: $store.compose) { c in
            ComposeSheet(compose: c).environmentObject(store).environment(\.theme, t)
        }
        .sheet(isPresented: $serversOpen) {
            ServersView(open: $serversOpen).environmentObject(store).environment(\.theme, t)
        }
        .fileImporter(isPresented: $picking, allowedContentTypes: [.item]) { result in
            guard case .success(let url) = result else { return }
            attach(url)
        }
        // The core asks; SwiftUI does the asking. A cancel is an answer too —
        // it is what stops a confirm turning into a delete.
        .alert(item: $store.ask) { ask in
            if ask.wantsText {
                return Alert(
                    title: Text(ask.message),
                    primaryButton: .default(Text("ok")) { store.answer(true, answerText) },
                    secondaryButton: .cancel(Text("cancel")) { store.answer(false) })
            }
            if let yes = ask.accept {
                return Alert(
                    title: Text(ask.message),
                    primaryButton: .default(Text(yes)) { store.answer(true) },
                    secondaryButton: .cancel(Text("decline")) { store.answer(false) })
            }
            return Alert(
                title: Text(ask.message),
                primaryButton: .destructive(Text("yes")) { store.answer(true) },
                secondaryButton: .cancel(Text("no")) { store.answer(false) })
        }
        .onAppear { store.onPickFile = { picking = true } }
        .onChange(of: draft) { new in store.updatePalette(new) }
    }

    /// Everything between the channel name and the status line. Named so the
    /// drawer can sit over it without the layout below reading as a nest.
    private var conversation: some View {
        VStack(spacing: 0) {
            BlobSwitcher()
            if store.snap.active_blob.isEmpty {
                StreamView(rows: store.rows,
                           notices: store.snap.notices + store.saving.sorted { $0.key < $1.key }.map { Notice(text: "receiving \($0.key) — \($0.value)%") },
                           system: store.snap.system, online: store.snap.online,
                           solo: store.snap.solo, nearby: channel?.nearby ?? false,
                           me: store.snap.user?.id ?? "",
                           onMention: { store.openDm($0) },
                           onFile: { store.saveFile(id: $0, name: $1) },
                           onReply: { store.setReply($0) },
                           onDismissSystem: { store.dismissSystem($0) })
                if !store.palette.isEmpty { paletteView }
                if let r = store.snap.rooms.first(where: { $0.channel == store.snap.active }) {
                    roomStrip(r)
                }
                if !store.snap.reply.isEmpty { replybar }
                deviceBar
                composer
            } else {
                // A blob replaces the stream and the composer: it has its own
                // focus model, and two things competing for the keyboard is
                // what the framework's key rules exist to avoid.
                BlobView()
            }
        }
    }

    /// The rail, as a drawer. The scrim is half the gesture: a tap back on the
    /// conversation is how a drawer closes on a phone, and without it the only
    /// way out is the key you opened it with.
    @ViewBuilder private var rail: some View {
        Color.black.opacity(0.35)
            .onTapGesture { railOpen = false }
            .transition(.opacity)
        RailView(open: $railOpen)
            .frame(width: railWidth)
            .overlay(Rectangle().frame(width: 1).foregroundStyle(t.border), alignment: .trailing)
            .transition(.move(edge: .leading))
            .gesture(DragGesture(minimumDistance: 20).onEnded { g in
                if g.translation.width < -40 { railOpen = false }
            })
    }

    /// Wide enough to read a channel name and its badge, narrow enough that the
    /// conversation behind it is still visibly there.
    private var railWidth: CGFloat { min(UIScreen.main.bounds.width * 0.82, 300) }

    // MARK: - chrome

    private var menubar: some View {
        HStack(spacing: 2) {
            Text("gliph").font(Face.serif(19)).foregroundStyle(t.accentChrome)
                .padding(.trailing, 6)
            Glyph("hash", "channels", size: 16, box: chromeKey) {
                if wide { railDocked.toggle() } else { railOpen.toggle() }
            }
            Glyph(store.theme.isDark ? "moon" : "sun",
                  store.theme.isDark ? "switch to the light theme" : "switch to the dark theme",
                  size: 16, box: chromeKey) {
                store.theme = store.theme.isDark ? .creme : .midnight
            }
            if !store.snap.transfers.isEmpty {
                Glyph("transfers", "file transfers", size: 16, box: chromeKey) { transfersOpen = true }
            }
            Spacer(minLength: 6)
            ServerStrip(sheetOpen: $serversOpen)
            Glyph("gear", "settings", size: 16, box: chromeKey) { settingsOpen = true }
        }
        .padding(.horizontal, 12)
        .padding(.bottom, 4)
        .background(t.chrome.ignoresSafeArea(edges: .top))
    }

    private var titlestrip: some View {
        HStack(spacing: 8) {
            Text(channel?.label ?? "gliph")
                .font(Face.mono(13, .heavy)).foregroundStyle(t.chromeText)
            if let ch = channel {
                Text("[\(ch.members.count)] [+\(ch.modes)]")
                    .font(Face.mono(11.5)).foregroundStyle(t.chromeDim)
                if ch.encrypted {
                    Text("e2e").font(Face.mono(11, .bold)).foregroundStyle(t.join)
                }
                if !ch.is_dm {
                    Text("history: \(ch.history)").font(Face.mono(11)).foregroundStyle(t.chromeDim)
                }
            }
            Text(channel?.topic ?? "")
                .font(Face.mono(11.5)).foregroundStyle(t.chromeDim)
                .lineLimit(1).truncationMode(.tail)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 12)
        .frame(height: 34)
        .background(t.chrome)
    }

    private var paletteView: some View {
        ScrollView {
            VStack(spacing: 0) {
                ForEach(store.palette) { cmd in
                    Button {
                        draft = "/\(cmd.name) "
                    } label: {
                        HStack(spacing: 10) {
                            Text(cmd.usage).font(Face.mono(13, .bold)).foregroundStyle(t.text)
                            Text(cmd.desc).font(Face.mono(11.5)).foregroundStyle(t.muted)
                                .lineLimit(1)
                            Spacer(minLength: 0)
                            // Where a command came from is worth showing: a
                            // plugin's commands appear and vanish with it.
                            if cmd.source != "builtin" {
                                Text(cmd.source).font(Face.mono(10.5)).foregroundStyle(t.agent)
                            }
                        }
                        .padding(.horizontal, 12).padding(.vertical, 7)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                }
            }
        }
        .frame(maxHeight: 190)
        .background(t.surface2)
        .overlay(Divider().background(t.border), alignment: .top)
    }

    /// Device approval: this device waiting, or another one waiting on it.
    @ViewBuilder private var deviceBar: some View {
        let d = store.snap.device
        if d.known && !store.snap.solo {
            if !d.signed {
                // Side by side on an iPad; the QR above the text on a phone.
                let layout = wide ? AnyLayout(HStackLayout(spacing: 12)) : AnyLayout(VStackLayout(alignment: .leading, spacing: 0))
                layout {
                    if !d.qr.isEmpty { QRCode(rows: d.qr).frame(width: 112, height: 112) }
                    deviceLine("this device is not approved yet, so it cannot read encrypted channels. scan this with your phone's camera, or on a device you already use, type /approve \(d.code)")
                }
                .background(t.surface)
            } else {
                ForEach(d.pending, id: \.code) { p in
                    deviceLine("\(p.name.isEmpty ? "a new device" : p.name) wants to read your encrypted channels. if it shows \(p.code), type /approve \(p.code)")
                }
            }
        }
    }

    /// The core's QR rows, dark on white with a 4-module quiet zone.
    private struct QRCode: View {
        let rows: [String]
        var body: some View {
            Canvas { ctx, size in
                let n = CGFloat(rows.count + 8), m = size.width / n
                ctx.fill(Path(CGRect(origin: .zero, size: size)), with: .color(.white))
                for (y, row) in rows.enumerated() {
                    for (x, c) in row.enumerated() where c == "1" {
                        ctx.fill(Path(CGRect(x: (CGFloat(x) + 4) * m, y: (CGFloat(y) + 4) * m, width: m, height: m)),
                                 with: .color(.black))
                    }
                }
            }
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .padding(.leading, 12).padding(.vertical, 6)
            .accessibilityLabel("approval QR code")
        }
    }

    private func deviceLine(_ text: String) -> some View {
        Text(text).font(Face.mono(11.5)).foregroundStyle(t.text2)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 12).padding(.vertical, 6)
            .background(t.surface)
    }

    /// "you are answering this", above the input. The line is the core's — it
    /// is exactly what will go on the wire, clipped the same way.
    private var replybar: some View {
        HStack(spacing: 10) {
            Rectangle().fill(t.border).frame(width: 2)
            Text("replying to \(store.snap.reply)")
                .font(Face.mono(11.5)).foregroundStyle(t.muted)
                .lineLimit(1).truncationMode(.tail)
            Spacer(minLength: 0)
            Glyph("close", "cancel the reply", color: t.muted,
                  size: 14, box: chromeKey) { store.setReply("") }
        }
        .fixedSize(horizontal: false, vertical: true)
        .padding(.horizontal, 10).padding(.top, 8)
        .background(t.surface)
    }

    /// The live room line. room0 itself is web and desktop, so the phone gets
    /// the half it can honour: who is in there, where, and for how long.
    private func roomStrip(_ r: RoomStrip) -> some View {
        HStack(spacing: 8) {
            Text("room0").font(Face.mono(12, .bold)).foregroundStyle(t.r0From)
            Text("\(r.room) · \(r.people.map(\.name).joined(separator: ", "))"
                 + (r.elapsed.isEmpty ? "" : " · \(r.elapsed)"))
                .font(Face.mono(12)).foregroundStyle(t.text2)
                .lineLimit(1).truncationMode(.tail)
            if r.voice_count > 0 {
                Text("voice \(r.voice_count)").font(Face.mono(11, .bold)).foregroundStyle(t.teal)
            }
            Spacer(minLength: 0)
            Text("on desktop").font(Face.mono(11)).foregroundStyle(t.muted)
        }
        .padding(.horizontal, 12).padding(.vertical, 7)
        .background(t.surface)
        .overlay(r.gradientLine(t), alignment: .top)
    }

    private var composer: some View {
        HStack(alignment: .bottom, spacing: 2) {
            TextField(
                channel.map { $0.is_dm ? "message \($0.name)" : "message #\($0.name)" }
                    .map { "\($0)  (/ for commands)" } ?? "…",
                text: $draft, axis: .vertical)
                .lineLimit(1...5)
                .font(Face.mono(13))
                .foregroundStyle(t.text)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .focused($composerFocused)
                .onSubmit(send)
                .padding(.horizontal, 10).padding(.vertical, 11)
                .background(t.surface2)
                .overlay(Rectangle().stroke(t.border, lineWidth: 1))
            // A screenshot on the clipboard: the text field's own paste only
            // knows strings, so the bar grows a key while there is an image
            // to take. Asking whether there is one does not trip the paste
            // prompt; reading it does, once.
            if clipHasImage { Glyph("paste", "paste image", color: t.muted, action: pasteImage) }
            Glyph("clip", "attach a file", color: t.muted) { picking = true }
            Glyph("send", "send", color: t.accent, action: send)
        }
        .padding(.horizontal, 10).padding(.vertical, 8)
        .background(t.surface)
        .overlay(Divider().background(t.border), alignment: .top)
        .onReceive(NotificationCenter.default.publisher(for: UIPasteboard.changedNotification)) { _ in
            clipHasImage = UIPasteboard.general.hasImages
        }
        .onReceive(NotificationCenter.default.publisher(for: UIApplication.didBecomeActiveNotification)) { _ in
            clipHasImage = UIPasteboard.general.hasImages
        }
    }

    private var statusbar: some View {
        HStack(spacing: 12) {
            Text(store.snap.solo ? "nearby only"
                 : (store.snap.online ? "connected" : "reconnecting…"))
                .font(Face.mono(11))
                .foregroundStyle(store.snap.online || store.snap.solo ? t.join : t.accent)
            if store.snap.nearby_on {
                Text("nearby: \(store.snap.nearby_peers)")
                    .font(Face.mono(11)).foregroundStyle(t.chromeDim)
            }
            Spacer()
            if let ch = channel {
                Text("\(ch.role.isEmpty ? "member" : ch.role) · \(store.snap.user?.nick ?? "")")
                    .font(Face.mono(11)).foregroundStyle(t.chromeDim)
            }
        }
        .padding(.horizontal, 12).padding(.top, 4).padding(.bottom, 22)
        .background(t.chrome)
    }

    // MARK: - actions

    private func send() {
        let text = draft
        guard !text.trimmed.isEmpty else { return }
        // Return with the palette up takes its first entry, like the web
        // client: "/upd⏎" is /update, not a message saying "/upd".
        if let cmd = store.palette.first, text.hasPrefix("/"), !text.contains(" ") {
            if cmd.usage.contains("<") || cmd.usage.contains("[") {
                draft = "/\(cmd.name) "
                return
            }
            draft = ""
            store.submit("/\(cmd.name)")
            return
        }
        draft = ""
        store.submit(text)
    }

    private func pasteImage() {
        let pb = UIPasteboard.general
        // The bytes as copied where they are png or jpeg already; anything
        // else (a copied photo region) is rendered to png.
        if let d = pb.data(forPasteboardType: UTType.png.identifier) {
            store.offerFile(name: "image.png", mime: "image/png", bytes: d)
        } else if let d = pb.data(forPasteboardType: UTType.jpeg.identifier) {
            store.offerFile(name: "image.jpg", mime: "image/jpeg", bytes: d)
        } else if let d = pb.image?.pngData() {
            store.offerFile(name: "image.png", mime: "image/png", bytes: d)
        }
    }

    private func attach(_ url: URL) {
        // A security-scoped URL from the picker: the read has to be bracketed,
        // or it comes back empty on a file outside the sandbox.
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        guard let data = try? Data(contentsOf: url) else { return }
        let mime = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType
            ?? "application/octet-stream"
        store.offerFile(name: url.lastPathComponent, mime: mime, bytes: data)
    }

    /// The keys are drawn, not spelled. A desktop menubar has room to say
    /// [#] [o] [^] [x] and a phone does not: brackets crowded against a title
    /// or an input field stop reading as a menu and start eating the line. The
    /// glyphs are the same pixelarticons the web client draws in
    /// `web/src/icons.ts`, carried here as vector imagesets so one set answers
    /// for both clients.

    /// A chrome key is smaller than a composer key and sits on a shorter bar.
    private let chromeKey = CGSize(width: 28, height: 30)

    @ViewBuilder
    private func Glyph(_ asset: String, _ label: String, color: Color? = nil,
                       size: CGFloat = 20, box: CGSize = CGSize(width: 34, height: 38),
                       action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(asset)
                .renderingMode(.template)
                .resizable()
                .frame(width: size, height: size)
                .foregroundStyle(color ?? t.chromeDim)
                .frame(width: box.width, height: box.height)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
    }
}
