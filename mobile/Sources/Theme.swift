//  Gliph theme tokens, the same hex the web and desktop clients use.
//
//  A phone next to a laptop should be the same product, not a tribute to it, so
//  these are copied from web/src/theme.css rather than re-picked. A custom
//  theme is a third `Theme` and nothing else.

import SwiftUI

struct Theme {
    let name: String
    let chrome, chromeText, chromeDim: Color
    let bg, surface, surface2, rail: Color
    let text, text2, muted, border, border2: Color
    let accent, link, join, action, mode, agent, gold: Color
    let azure, teal, olive, magenta: Color
    /// A custom theme's base (Extend.swift); "" for the two built-ins.
    var base = ""
    /// A hairline drawn ON the accent surface, where no border colour in the
    /// palette sits correctly — the accent is the same in both themes, so this
    /// is too. And the backdrop behind anything modal. Both used to be bare
    /// `Color.white`/`Color.black` literals at their call sites, which made
    /// them the only two colours in the app a theme could not reach.
    var onAccentLine: Color { .white.opacity(0.22) }
    /// room0's two: the mark and the strip run a gradient between them. Same
    /// pair as --r0-from / --r0-to in web/src/theme.css.
    var r0From: Color { isDark ? .hex(0x39C6FF) : .hex(0x0B8FD0) }
    var r0To: Color { isDark ? .hex(0x9D8EF5) : .hex(0x2941C8) }
    var r0Gradient: LinearGradient {
        LinearGradient(colors: [r0From, r0To], startPoint: .leading, endPoint: .trailing)
    }
    var scrim: Color { .black.opacity(0.35) }
    /// The accent where it sits on the chrome bar (the wordmark): the creme
    /// accent is darkened for AA on the page, so the bar gets its own. Same
    /// pair as --accent-chrome in web/src/theme.css.
    var accentChrome: Color { isDark ? .hex(0xEF6A90) : .hex(0xEC6189) }

    var isDark: Bool { name == "midnight" || base == "midnight" }

    static func named(_ n: String?) -> Theme {
        if let n, let c = Themes.installed[n] { return .custom(c) }
        return n == "midnight" ? .midnight : .creme
    }

    /// Stable per-nick colour, mIRC style, drawn from the palette.
    func nick(_ n: String) -> Color {
        var h = 0
        for u in n.unicodeScalars { h = (h &* 31 &+ Int(u.value)) & 0x7fffffff }
        // Same ten, same order, as web/src/alertcard.ts nickColor.
        return [accent, link, join, action, agent, gold, azure, teal, olive, magenta][h % 10]
    }

    static let creme = Theme(
        name: "creme",
        chrome: .hex(0x1A2138), chromeText: .hex(0xF5F1E8), chromeDim: .hex(0x9AA3C4),
        bg: .hex(0xE9E3D2), surface: .hex(0xF5F1E8), surface2: .hex(0xECE7D8), rail: .hex(0xECE7D8),
        text: .hex(0x23262E), text2: .hex(0x383C4A), muted: .hex(0x6A6453),
        border: .hex(0xC2BBA6), border2: .hex(0xCBC4AE),
        accent: .hex(0xBB2851), link: .hex(0x2941C8), join: .hex(0x32712E),
        action: .hex(0x8A35C8), mode: .hex(0x2941C8), agent: .hex(0x994F2C), gold: .hex(0x7A5F15),
        azure: .hex(0x2F60BB), teal: .hex(0x196F6F), olive: .hex(0x5B690F), magenta: .hex(0xA73491)
    )

    static let midnight = Theme(
        name: "midnight",
        chrome: .hex(0x232B4A), chromeText: .hex(0xF0ECE0), chromeDim: .hex(0x99A1C2),
        bg: .hex(0x12172A), surface: .hex(0x171D33), surface2: .hex(0x212843), rail: .hex(0x101528),
        text: .hex(0xE2DDCD), text2: .hex(0xC9C4B2), muted: .hex(0x8991AC),
        border: .hex(0x2E3654), border2: .hex(0x364060),
        accent: .hex(0xEB658B), link: .hex(0x7D92F0), join: .hex(0x93C99A),
        action: .hex(0xC084F0), mode: .hex(0x7D92F0), agent: .hex(0xEBA98A), gold: .hex(0xF2C14E),
        azure: .hex(0x9FC0FF), teal: .hex(0x5FC9C9), olive: .hex(0xB9C85A), magenta: .hex(0xE08AD6)
    )
}

extension Color {
    static func hex(_ v: UInt32) -> Color {
        Color(red: Double((v >> 16) & 0xff) / 255,
              green: Double((v >> 8) & 0xff) / 255,
              blue: Double(v & 0xff) / 255)
    }
}

/// The chat is a monospace grid — ASCII art (spec F10) only lines up if every
/// glyph is the same width, so this is load-bearing rather than decorative.
/// SF Mono ships with the system, so nothing is bundled for it.
enum Face {
    static func mono(_ size: CGFloat, _ weight: Font.Weight = .medium) -> Font {
        .system(size: scaled(size), weight: weight, design: .monospaced)
    }
    static func serif(_ size: CGFloat) -> Font {
        .system(size: scaled(size), weight: .heavy, design: .serif)
    }
    /// Dynamic Type: every size follows the reader's text size setting.
    /// ponytail: capped at 1.6x, because the chrome bars are fixed-height rows
    /// of monospace; past that they clip. Lift the cap when the bars wrap.
    private static func scaled(_ size: CGFloat) -> CGFloat {
        min(UIFontMetrics(forTextStyle: .body).scaledValue(for: size), size * 1.6)
    }
}

private struct ThemeKey: EnvironmentKey {
    static let defaultValue = Theme.creme
}

extension EnvironmentValues {
    var theme: Theme {
        get { self[ThemeKey.self] }
        set { self[ThemeKey.self] = newValue }
    }
}
