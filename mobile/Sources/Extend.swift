//  Custom themes and the multi-line editor behind /theme add and /script add.
//
//  The core checks a theme file (core/src/client/theme.rs) and hands over its
//  name, base and tokens; this keeps them per device beside the theme choice
//  and builds a `Theme` from them. A token the file leaves out is its base's.

import SwiftUI

struct CustomTheme: Codable {
    let name: String
    let base: String
    let tokens: [String: String]
}

enum Themes {
    private static let key = "gliph.themes"

    static var installed: [String: CustomTheme] {
        get {
            guard let d = UserDefaults.standard.data(forKey: key),
                  let all = try? JSONDecoder().decode([String: CustomTheme].self, from: d) else { return [:] }
            return all
        }
        set { UserDefaults.standard.set(try? JSONEncoder().encode(newValue), forKey: key) }
    }

    /// The `Theme` effect. Returns what to tell the user, and the theme to put
    /// on screen when there is one.
    static func effect(op: String, name: String, json: String) -> (say: String, level: String, show: Theme?) {
        var all = installed
        switch op {
        case "add":
            guard let d = json.data(using: .utf8), let t = try? JSONDecoder().decode(CustomTheme.self, from: d)
            else { return ("theme not installed", "error", nil) }
            all[t.name] = t
            installed = all
            return ("installed theme \(t.name). /theme creme or /theme midnight goes back", "info", Theme.custom(t))
        case "use":
            if name == "creme" || name == "midnight" { return ("theme: \(name)", "info", Theme.named(name)) }
            guard let t = all[name] else { return ("no theme called \(name). /theme add installs one", "error", nil) }
            return ("theme: \(name)", "info", Theme.custom(t))
        case "remove":
            guard all.removeValue(forKey: name) != nil else { return ("no theme called \(name)", "error", nil) }
            installed = all
            return ("removed theme \(name)", "info", nil)
        default:
            let names = ["creme", "midnight"] + all.keys.sorted()
            return ("themes: \(names.joined(separator: ", ")). /theme <name> switches, /theme add installs", "info", nil)
        }
    }
}

extension Theme {
    static func custom(_ c: CustomTheme) -> Theme {
        let b = c.base == "midnight" ? Theme.midnight : Theme.creme
        func k(_ token: String, _ fallback: Color) -> Color {
            guard let v = c.tokens[token], v.count == 7, let n = UInt32(v.dropFirst(), radix: 16) else { return fallback }
            return .hex(n)
        }
        return Theme(
            name: c.name,
            chrome: k("chrome", b.chrome), chromeText: k("chrome-text", b.chromeText), chromeDim: k("chrome-dim", b.chromeDim),
            bg: k("bg", b.bg), surface: k("surface", b.surface), surface2: k("surface-2", b.surface2), rail: k("rail", b.rail),
            text: k("text", b.text), text2: k("text-2", b.text2), muted: k("muted", b.muted),
            border: k("border", b.border), border2: k("border-2", b.border2),
            accent: k("accent", b.accent), link: k("link", b.link), join: k("join", b.join),
            action: k("action", b.action), mode: k("mode", b.mode), agent: k("agent", b.agent), gold: k("gold", b.gold),
            azure: k("azure", b.azure), teal: k("teal", b.teal), olive: k("olive", b.olive), magenta: k("magenta", b.magenta),
            base: c.base
        )
    }
}

/// The core wants a multi-line editor: a theme file or a script.
struct Compose: Identifiable {
    let id = UUID()
    let title: String
    var text: String
    let submit: String
}

struct ComposeSheet: View {
    @EnvironmentObject var store: Store
    @Environment(\.theme) var t
    @State var compose: Compose

    var body: some View {
        NavigationStack {
            TextEditor(text: $compose.text)
                .font(Face.mono(12))
                .foregroundStyle(t.text)
                .scrollContentBackground(.hidden)
                .background(t.surface)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .navigationTitle(compose.title)
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("cancel") { store.compose = nil } }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("save") {
                            let c = compose
                            store.compose = nil
                            store.submit(c.submit + "\n" + c.text)
                        }
                    }
                }
        }
    }
}
