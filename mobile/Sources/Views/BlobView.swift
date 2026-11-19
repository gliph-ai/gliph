//  The blob surface, drawn. That is all this file does.
//
//  The document model, the focus order, the selection, the menus and the input
//  drafts live in core/src/gum.rs — the same code the web client runs as wasm.
//  What arrives here is a flat list of nodes with "this row is selected", "this
//  one is number 3", "this one has a submenu" already decided, so this app and
//  the web app cannot disagree about what a tap does.
//
//  Design: docs/plans/gliph-apps.md.

import SwiftUI

struct BlobView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t

    private var surface: Surface { store.surface }

    var body: some View {
        ZStack {
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0) {
                    if !surface.loaded {
                        Text("loading…")
                            .font(Face.mono(13)).foregroundStyle(t.muted)
                            .padding(12)
                    }
                    ForEach(surface.nodes) { node(   $0) }
                }
                .padding(.vertical, 8)
            }

            // The scrim is the only way off this menu on a touch device: esc
            // closes it, and a phone has no esc. Without it the only exits were
            // the actions themselves — every one of which changes something.
            if let menu = surface.menu {
                t.scrim
                    .ignoresSafeArea()
                    .onTapGesture { store.gumEscape() }
                VStack(spacing: 0) {
                    ForEach(menu) { entry in
                        Button { store.gumTapMenu(entry.id) } label: {
                            Text(entry.label)
                                .font(Face.mono(13, entry.selected ? .bold : .medium))
                                .foregroundStyle(t.text)
                                .padding(.horizontal, 14).padding(.vertical, 11)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .background(entry.selected ? t.surface : .clear)
                                .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                    }
                }
                .background(t.surface2)
                .overlay(Rectangle().stroke(t.border, lineWidth: 1))
                .padding(.horizontal, 40)
            }
        }
        .background(t.surface)
    }

    @ViewBuilder
    private func node(_ n: GumNode) -> some View {
        switch n {
        case .text(let body, let style):
            Text(body)
                .font(Face.mono(13))
                .foregroundStyle(colour(style))
                .padding(.horizontal, 12).padding(.vertical, 4)
                .frame(maxWidth: .infinity, alignment: .leading)

        case .divider(let label):
            Text(label.isEmpty ? "--" : "-- \(label) --")
                .font(Face.mono(11)).foregroundStyle(t.muted)
                .padding(.horizontal, 12).padding(.vertical, 8)

        case .row(let index, let number, let label, let hint, let state, let hasActions, let selected):
            Button { store.gumTap(index) } label: {
                HStack(spacing: 8) {
                    Text(number == 0 ? " " : "\(number)")
                        .font(Face.mono(11)).foregroundStyle(t.muted).frame(width: 12)
                    Text(glyph(state)).font(Face.mono(11)).foregroundStyle(stateColour(state))
                    Text(label).font(Face.mono(13)).foregroundStyle(t.text)
                    if !hint.isEmpty {
                        Text(hint).font(Face.mono(11.5)).foregroundStyle(t.muted).lineLimit(1)
                    }
                    Spacer(minLength: 0)
                    if hasActions { Text("›").font(Face.mono(13)).foregroundStyle(t.muted) }
                }
                .padding(.horizontal, 12).padding(.vertical, 7)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(selected ? t.surface2 : .clear)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)

        case .table(let cols, let rows):
            // Sideways rather than squeezed: a table with wrapped cells is not
            // a table any more.
            ScrollView(.horizontal, showsIndicators: false) {
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 14) {
                        ForEach(Array(cols.enumerated()), id: \.offset) { _, c in
                            Text(c).font(Face.mono(11, .bold)).foregroundStyle(t.muted)
                        }
                    }
                    ForEach(Array(rows.enumerated()), id: \.offset) { _, r in
                        HStack(spacing: 14) {
                            ForEach(Array(r.enumerated()), id: \.offset) { _, cell in
                                Text(cell).font(Face.mono(12)).foregroundStyle(t.text2)
                            }
                        }
                    }
                }
                .padding(.horizontal, 12).padding(.vertical, 6)
            }

        case .input(let index, let label, let placeholder, let value, let selected):
            BlobInput(index: index, label: label, placeholder: placeholder,
                      value: value, selected: selected)

        case .buttons(let buttons):
            HStack(spacing: 8) {
                ForEach(buttons) { b in
                    Button { store.gumTap(b.index) } label: {
                        Text(b.label)
                            .font(Face.mono(12, .bold))
                            .foregroundStyle(b.selected ? .white : t.text)
                            .padding(.horizontal, 12).padding(.vertical, 8)
                            .background(b.selected ? t.accent : t.surface2)
                            .overlay(Rectangle().stroke(t.border, lineWidth: 1))
                    }
                    .buttonStyle(.plain)
                }
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 12).padding(.vertical, 6)

        case .bar(let body):
            Text(body)
                .font(Face.mono(11)).foregroundStyle(t.muted)
                .padding(.horizontal, 12).padding(.vertical, 8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(t.surface2)

        case .unknown:
            // A node kind from a newer core. Drawing nothing is better than
            // drawing a guess.
            EmptyView()
        }
    }

    private func colour(_ style: String) -> Color {
        switch style {
        case "info": return t.link
        case "warn": return t.gold
        case "error": return t.accent
        default: return t.text
        }
    }

    private func glyph(_ state: String) -> String {
        switch state {
        case "ok", "done": return "✓"
        case "warn": return "!"
        case "error": return "×"
        case "open": return "○"
        default: return " "
        }
    }

    private func stateColour(_ state: String) -> Color {
        switch state {
        case "ok", "done": return t.join
        case "warn": return t.gold
        case "error": return t.accent
        default: return t.muted
        }
    }
}

/// The input keeps its own text while the user types and hands it to the core
/// on every change. Reading the value straight off the surface each keystroke
/// would fight the keyboard: the core echoes it back and the cursor jumps.
private struct BlobInput: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t

    let index: Int
    let label: String
    let placeholder: String
    let value: String
    let selected: Bool

    @State private var text = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if !label.isEmpty {
                Text(label).font(Face.mono(11, .bold)).foregroundStyle(t.muted)
            }
            HStack(spacing: 6) {
                TextField(placeholder, text: $text)
                    .font(Face.mono(13))
                    .foregroundStyle(t.text)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .submitLabel(.send)
                    .onChange(of: text) { new in store.gumSetDraft(new) }
                    .onSubmit { store.gumActivate() }
                    // Typing into a field means selecting it too: a phone has
                    // no separate "move the cursor there".
                    .onTapGesture { store.gumFocusInput(index) }
                Button { store.gumActivate() } label: {
                    Text("[go]").font(Face.mono(12, .bold)).foregroundStyle(t.accent)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 10).padding(.vertical, 9)
            .background(t.surface2)
            .overlay(Rectangle().stroke(selected ? t.accent : t.border, lineWidth: 1))
        }
        .padding(.horizontal, 12).padding(.vertical, 6)
        // The core is the truth for what is in the box — it clears it on
        // submit, and it is what a second device would see.
        .onChange(of: value) { new in if new != text { text = new } }
        .onAppear { text = value }
    }
}

/// The switcher: chat first, then this channel's apps. A horizontal row under
/// the title strip rather than a tab bar, so it never reads as another list of
/// channels. Styled like the web client's: no box around the selected tab, on
/// the chrome's own background, with a 2px accent rule under the one you are
/// in — a boxed tab reads as a button, and these are places.
struct BlobSwitcher: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t

    var body: some View {
        let blobs = store.surface.blobs
        if !blobs.isEmpty {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 14) {
                    pick(id: "", label: "chat", source: "builtin")
                    ForEach(blobs) { pick(id: $0.id, label: $0.title, source: $0.source) }
                }
                .padding(.horizontal, 12)
            }
            .background(t.chrome)
        }
    }

    @ViewBuilder
    private func pick(id: String, label: String, source: String) -> some View {
        let on = store.snap.active_blob == id
        Button { store.openBlob(id) } label: {
            Text(label)
                .font(Face.mono(12, on ? .heavy : .semibold))
                .foregroundStyle(on ? t.chromeText : (source == "builtin" ? t.chromeDim : t.agent))
                .padding(.top, 5).padding(.bottom, 6)
                .overlay(Rectangle().fill(on ? t.accent : .clear).frame(height: 2),
                         alignment: .bottom)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }
}
