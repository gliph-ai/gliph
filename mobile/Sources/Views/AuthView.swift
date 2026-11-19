//  The sign-in screen. Two doors: an account on a server, or nobody at all —
//  nearby mode (F9) needs no account, because on a plane there is nothing to
//  authenticate against.

import SwiftUI

struct AuthView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t

    @State private var registering = false
    @State private var busy = false
    @State private var err = ""

    /// A pinned build hides the server field. Triple-tapping the logo shows it,
    /// so a tester can point the shipped app at another server without a
    /// rebuild — the same escape hatch the web and desktop clients have.
    @State private var serverShown = false
    /// The split button's menu. Everything in it used to be a line of its own
    /// under the form, which made the screen read as four choices when it is
    /// really one.
    @State private var menuOpen = false
    @State private var logoTaps = 0
    @State private var lastTap = Date.distantPast

    @State private var server = ""
    @State private var nick = ""
    @State private var email = ""
    @State private var password = ""
    @State private var invite = ""

    private var showServer: Bool { serverShown || !store.hasPinnedServer }
    /// Height of the split button. The floating menu drops by exactly this, so
    /// the two stay joined without a hardcoded guess in a second place.
    private let ctrlH: CGFloat = 44

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                Text("gliph")
                    .font(Face.serif(46))
                    .foregroundStyle(t.accent)
                    .frame(maxWidth: .infinity)
                    .contentShape(Rectangle())
                    .onTapGesture(perform: tapLogo)

                Text(registering ? "redeem your invite" : "sign in to your server")
                    .font(Face.mono(13))
                    .foregroundStyle(t.muted)
                    .frame(maxWidth: .infinity)
                    .padding(.top, 6)
                    .padding(.bottom, 26)

                if showServer {
                    Label("server")
                    Field(text: $server, hint: "https://chat.example.com", kind: .URL)
                }
                Label("nick")
                Field(text: $nick, content: .username)
                if registering {
                    Label("email")
                    Field(text: $email, kind: .emailAddress)
                }
                Label("password")
                Field(text: $password, secure: true)
                if registering {
                    Label("invite code")
                    Text("from your server's admin — blank only if you are the first account")
                        .font(Face.mono(11))
                        .foregroundStyle(t.muted)
                        .padding(.bottom, 4)
                    Field(text: $invite)
                }

                // A split button: the thing you almost always want, and a
                // caret holding the ways in that are not it.
                HStack(spacing: 0) {
                    // Navy with the accent as the active state, the same
                    // way round as every other button in the product —
                    // this screen had them inverted against the web.
                    Button(action: go) {
                        Text(busy ? "…" : (registering ? "join" : "connect"))
                            .font(Face.mono(13, .bold))
                            .foregroundStyle(.white)
                            .frame(maxWidth: .infinity, minHeight: ctrlH)
                            .background(busy ? t.muted : t.chrome)
                    }
                    .disabled(busy)

                    // One hairline of the panel's own background, so the
                    // two halves read as one control that has been divided
                    // rather than two buttons pushed together.
                    Rectangle().fill(t.bg).frame(width: 1, height: ctrlH)

                    Button { withAnimation(.none) { menuOpen.toggle() } } label: {
                        Text(menuOpen ? "▲" : "▼")
                            .font(Face.mono(11, .bold))
                            .foregroundStyle(.white)
                            .frame(width: 46, height: ctrlH)
                            .background(menuOpen ? t.accent : (busy ? t.muted : t.chrome))
                    }
                    .accessibilityLabel("other ways to sign in")
                }
                // Floating, over the form rather than in it: opening a menu
                // should not move the button it hangs off. An overlay has no
                // say in layout, which is the point; the offset drops it clear
                // of the button it belongs to.
                .overlay(alignment: .topTrailing) {
                    if menuOpen { authMenu.offset(y: ctrlH) }
                }
                .padding(.top, 18)
                // Above what comes after it in the stack, or the "chat nearby"
                // line draws straight through the menu that is covering it.
                .zIndex(1)

                if !err.isEmpty {
                    Text(err)
                        .font(Face.mono(12))
                        .foregroundStyle(t.accent)
                        .padding(.top, 14)
                }

                Alt(lead: "or ", action: "chat with people nearby", act: nearby)
                    .padding(.top, 22)
                Text("no account, no server — just this network")
                    .font(Face.mono(11))
                    .foregroundStyle(t.muted)
                    .frame(maxWidth: .infinity)

            }
            .frame(maxWidth: 420)
            .padding(.horizontal, 22)
            .padding(.vertical, 40)
            .frame(maxWidth: .infinity)
        }
        .background(t.bg)
        // Tapping anywhere off the menu closes it. The buttons and fields
        // swallow their own taps, so this only fires on the screen itself.
        .onTapGesture { menuOpen = false }
        .onAppear { if server.isEmpty { server = store.server } }
        // Typing closes the menu: start filling in the form and it should get
        // out of the way rather than sit over the thing you are doing.
        .onChange(of: server + nick + email + password + invite) { _ in menuOpen = false }
    }

    /// The ways in that are not "sign in with what I just typed". Nearby stays
    /// outside it: it is a front door (spec F9), not another spelling of this
    /// one.
    @ViewBuilder private var authMenu: some View {
        VStack(spacing: 0) {
            MenuRow(registering ? "sign in with an account" : "redeem an invite",
                    registering ? "arrow.right.square" : "person.badge.plus") {
                registering.toggle()
                err = ""
            }
            if store.hasPinnedServer {
                t.onAccentLine.frame(height: 1)
                MenuRow(showServer ? "use the standard server" : "use a custom server",
                        "server.rack") {
                    if showServer {
                        store.useDefaultServer()
                        server = store.server
                        serverShown = false
                    } else {
                        serverShown = true
                    }
                }
            }
        }
        // It wears the accent the caret turns when it is open, and hangs from
        // that caret rather than spanning the button — the handle and the thing
        // it pulled out, not a second control.
        .background(t.accent)
        // The same hard offset the blob menu uses on the web (.gmenu). A
        // floating surface needs to say it is floating.
        .shadow(color: .black.opacity(0.14), radius: 0, x: 3, y: 3)
        .frame(width: 236, alignment: .trailing)
        .padding(.top, 6)
    }

    /// The mark sits on the aligned edge, past the label, so the icons line up
    /// under the caret rather than against a ragged left. SF Symbols rather than
    /// the web's drawn set: two of them, and the system already has them.
    @ViewBuilder private func MenuRow(_ label: String, _ symbol: String,
                                      _ act: @escaping () -> Void) -> some View {
        Button {
            menuOpen = false
            act()
        } label: {
            HStack(spacing: 9) {
                Text(label).font(Face.mono(12, .bold))
                Image(systemName: symbol).font(.system(size: 13)).opacity(0.85)
            }
            .foregroundStyle(.white)
            .padding(.horizontal, 13).padding(.vertical, 11)
            .frame(maxWidth: .infinity, alignment: .trailing)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private func tapLogo() {
        let now = Date()
        logoTaps = now.timeIntervalSince(lastTap) > 0.6 ? 1 : logoTaps + 1
        lastTap = now
        if logoTaps >= 3 { serverShown = true }
    }

    private func go() {
        menuOpen = false
        busy = true
        err = ""
        Task {
            do {
                let s = server.trimmingCharacters(in: .whitespaces)
                guard !s.isEmpty else { throw Fail("no server address") }
                if registering {
                    try await store.register(server: s, nick: nick.trimmed, email: email.trimmed,
                                             password: password, invite: invite.trimmed)
                } else {
                    try await store.signIn(server: s, nick: nick.trimmed, password: password)
                }
            } catch {
                err = error.localizedDescription
            }
            busy = false
        }
    }

    private func nearby() {
        guard !nick.trimmed.isEmpty else {
            err = "pick a nick first — it is what people nearby will see"
            return
        }
        store.startSolo(nick: nick.trimmed)
    }

    // MARK: - the squared-off bits

    @ViewBuilder private func Label(_ s: String) -> some View {
        Text(s)
            .font(Face.mono(11, .bold))
            .foregroundStyle(t.muted)
            .padding(.top, 14)
            .padding(.bottom, 5)
    }

    @ViewBuilder private func Alt(lead: String, action: String, dim: Bool = false,
                                  act: @escaping () -> Void) -> some View {
        Button(action: act) {
            (Text(lead).foregroundColor(t.muted)
                + Text(action).foregroundColor(dim ? t.muted : t.link).underline())
                .font(Face.mono(13))
                .frame(maxWidth: .infinity)
        }
        .padding(.vertical, 4)
    }

    private struct Field: View {
        @Environment(\.theme) private var t
        @Binding var text: String
        var hint: String = ""
        var secure = false
        var kind: UIKeyboardType = .default
        var content: UITextContentType?

        var body: some View {
            Group {
                if secure {
                    SecureField(hint, text: $text)
                } else {
                    TextField(hint, text: $text)
                        .keyboardType(kind)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                }
            }
            .textContentType(content)
            .font(Face.mono(13))
            .foregroundStyle(t.text)
            .padding(.horizontal, 11)
            .padding(.vertical, 12)
            .background(t.surface)
            .overlay(Rectangle().stroke(t.border, lineWidth: 1))
        }
    }
}

extension String {
    var trimmed: String { trimmingCharacters(in: .whitespacesAndNewlines) }
}
