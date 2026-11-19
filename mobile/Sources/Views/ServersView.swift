//  The server switcher.
//
//  A Gliph server IS a team — its own database, its own accounts, its own
//  #general — so switching teams is switching servers, the way mIRC switched
//  networks. Your identity is per-server too: <waseem> on one, <wsm> on
//  another, because those are two rows in two different databases.
//
//  The rules (one spelling per address, which server is on screen, what the
//  bracket says) all live in core/src/servers.rs. This file only draws them.

import SwiftUI

/// The theme's accent tokens, in the order a server picks one. Deterministic
/// from the address, so a server keeps its colour on every device with nothing
/// to configure, and follows the theme (midnight included) — the same order
/// and hash as web/src/servers.ts.
func serverInk(_ host: String, _ t: Theme) -> Color {
    let ink: [Color] = [t.link, t.agent, t.join, t.action, t.gold, t.accent, t.teal]
    var h: UInt32 = 0
    for b in host.utf8 { h = h &* 31 &+ UInt32(b) }
    return ink[Int(h % UInt32(ink.count))]
}

/// The avatar: the server's own icon when it has uploaded one, its monogram
/// when it has not. Square, never round — a circle here would read as somebody
/// else's product.
struct ServerAvatar: View {
    let entry: ServerEntry
    var size: CGFloat = 22
    @Environment(\.theme) private var t

    var body: some View {
        ZStack {
            serverInk(entry.host, t)
            if let img = inlineImage(entry.icon) {
                Image(uiImage: img).resizable().scaledToFill()
            } else {
                Text(entry.monogram).font(Face.serif(size * 0.43)).foregroundStyle(.white)
            }
        }
        .frame(width: size, height: size)
        .clipped()
    }
}

/// A server's icon is a data: URL (the settings value, as the system card
/// reads it). Inline bytes only, never a fetch: loading a remote image would
/// tell its owner who looked at the switcher and when.
func inlineImage(_ s: String) -> UIImage? {
    guard let comma = s.range(of: "base64,"),
          let data = Data(base64Encoded: String(s[comma.upperBound...])) else { return nil }
    return UIImage(data: data)
}

/// An unread count in mIRC brackets. Unread, never mentions.
struct UnreadBracket: View {
    let n: Int
    @Environment(\.theme) private var t

    var body: some View {
        if n > 0 {
            Text("[\(n > 99 ? "99+" : String(n))]")
                .font(Face.mono(10.5, .heavy)).foregroundStyle(t.accent)
        }
    }
}

/// The strip in the menubar: where you are, then one avatar per other server.
struct ServerStrip: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @Binding var sheetOpen: Bool

    private var others: [ServerEntry] {
        store.servers.list.filter { $0.host != store.servers.active }
    }

    var body: some View {
        HStack(spacing: 7) {
            Button { sheetOpen = true } label: {
                HStack(spacing: 5) {
                    Text(store.activeServer?.name ?? store.snap.server_name)
                        .font(Face.mono(12, .bold)).foregroundStyle(t.chromeText)
                        .lineLimit(1)
                    if store.snap.unread_total > 0 {
                        Text("[\(store.snap.unread_total)]")
                            .font(Face.mono(11, .heavy)).foregroundStyle(t.chromeText)
                    }
                    Image(systemName: "chevron.down")
                        .font(.system(size: 8, weight: .bold)).foregroundStyle(t.chromeText)
                }
                .padding(.horizontal, 8).padding(.vertical, 3)
                .background(t.link)
            }
            .accessibilityLabel("switch server")

            // The avatar row appears at the SECOND server. One server means the
            // chip alone — nothing to explain until there is somewhere to go.
            // Two at most on a phone; the rest are one tap away in the sheet.
            ForEach(others.prefix(2)) { e in
                Button { store.switchTo(e.host) } label: {
                    HStack(spacing: 3) {
                        ServerAvatar(entry: e, size: 20)
                        UnreadBracket(n: e.unread)
                    }
                }
                .accessibilityLabel("switch to \(e.name.isEmpty ? e.shortHost : e.name)")
            }
            if others.count > 2 {
                Button { sheetOpen = true } label: {
                    Text("+\(others.count - 2)")
                        .accessibilityLabel("\(others.count - 2) more servers")
                        .font(Face.mono(9.5, .heavy)).foregroundStyle(t.chromeDim)
                        .frame(width: 20, height: 20)
                        .overlay(Rectangle().stroke(t.chromeDim, lineWidth: 1))
                }
            }
        }
    }
}

/// The full list: hosts, your nick on each, and the way to add another.
struct ServersView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @Binding var open: Bool

    @State private var adding = false
    @State private var host = ""
    @State private var nick = ""
    @State private var password = ""
    @State private var err = ""
    @State private var busy = false

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text("Servers").font(Face.serif(15)).foregroundStyle(t.chromeText)
                Spacer()
                Button("done") { open = false }
                    .font(Face.mono(12, .bold)).foregroundStyle(t.chromeDim)
            }
            .padding(.horizontal, 16).padding(.vertical, 12)
            .background(t.chrome)

            ScrollView {
                VStack(spacing: 0) {
                    ForEach(store.servers.list) { e in
                        row(e)
                        Divider().overlay(t.border)
                    }

                    if adding { addForm } else {
                        Button {
                            adding = true
                            host = ""; nick = ""; password = ""; err = ""
                        } label: {
                            HStack(spacing: 11) {
                                Text("+").font(Face.mono(17)).frame(width: 36)
                                Text("add a server").font(Face.mono(12.5, .heavy))
                                Spacer()
                            }
                            .foregroundStyle(t.link)
                            .padding(.horizontal, 16).padding(.vertical, 13)
                        }
                    }
                }
            }
            .background(t.surface)
        }
        .background(t.surface)
    }

    private func row(_ e: ServerEntry) -> some View {
        let here = e.host == store.servers.active
        return Button {
            store.switchTo(e.host)
            open = false
        } label: {
            HStack(spacing: 12) {
                ServerAvatar(entry: e, size: 36)
                VStack(alignment: .leading, spacing: 3) {
                    Text(e.name.isEmpty ? e.shortHost : e.name)
                        .font(Face.mono(13.5, .bold)).foregroundStyle(t.text)
                    Text(e.nick.isEmpty ? e.shortHost : "\(e.shortHost) · <\(e.nick)>")
                        .font(Face.mono(10.5)).foregroundStyle(t.muted)
                        .lineLimit(1).truncationMode(.middle)
                }
                Spacer(minLength: 6)
                if here {
                    Image(systemName: "checkmark")
                        .font(.system(size: 13, weight: .bold)).foregroundStyle(t.join)
                } else {
                    UnreadBracket(n: e.unread)
                }
            }
            .padding(.horizontal, 16).padding(.vertical, 11)
            .background(here ? t.surface2 : t.surface)
        }
    }

    private var addForm: some View {
        VStack(alignment: .leading, spacing: 9) {
            Text("sign in to another server")
                .font(Face.mono(11, .heavy)).foregroundStyle(t.muted)
            field("address", text: $host, keyboard: .URL)
            field("nick", text: $nick)
            field("password", text: $password, secure: true)
            if !err.isEmpty {
                Text(err).font(Face.mono(11)).foregroundStyle(t.accent)
            }
            HStack(spacing: 10) {
                Button(busy ? "signing in…" : "sign in") { submit() }
                    .font(Face.mono(12.5, .heavy)).foregroundStyle(t.chromeText)
                    .padding(.horizontal, 14).padding(.vertical, 9)
                    .background(t.chrome)
                    .disabled(busy)
                Button("cancel") { adding = false }
                    .font(Face.mono(12)).foregroundStyle(t.muted)
            }
        }
        .padding(16)
    }

    private func field(_ label: String, text: Binding<String>,
                       keyboard: UIKeyboardType = .default, secure: Bool = false) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(label).font(Face.mono(10, .heavy)).foregroundStyle(t.muted)
            Group {
                if secure { SecureField("", text: text) } else { TextField("", text: text) }
            }
            .font(Face.mono(13)).foregroundStyle(t.text)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
            .keyboardType(keyboard)
            .padding(.horizontal, 11).padding(.vertical, 9)
            .background(t.bg)
            .overlay(Rectangle().stroke(t.border, lineWidth: 1))
        }
    }

    private func submit() {
        busy = true
        err = ""
        Task {
            do {
                try await store.addServer(server: host, nick: nick, password: password)
                adding = false
                open = false
            } catch {
                err = (error as? Fail)?.message ?? "could not sign in"
            }
            busy = false
        }
    }
}
