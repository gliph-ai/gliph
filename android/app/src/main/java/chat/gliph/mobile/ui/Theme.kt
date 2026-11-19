//  Gliph theme tokens, the same hex the web, desktop and iOS clients use
//  (web/src/theme.css, mobile/Sources/Theme.swift). A phone next to a laptop
//  should be the same product, not a tribute to it. The palette meets AA
//  contrast in both themes (web/scripts/contrast.mjs checks the web's copy).

package chat.gliph.mobile.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.sp

private fun hex(v: Long) = Color(0xFF000000 or v)

data class Theme(
    val name: String,
    val chrome: Color, val chromeText: Color, val chromeDim: Color,
    val bg: Color, val surface: Color, val surface2: Color, val rail: Color,
    val text: Color, val text2: Color, val muted: Color, val border: Color, val border2: Color,
    val accent: Color, val link: Color, val join: Color, val action: Color, val mode: Color,
    val agent: Color, val gold: Color,
    val azure: Color, val teal: Color, val olive: Color, val magenta: Color,
    /** A custom theme's base (Themes.kt); "" for the two built-ins. */
    val base: String = "",
) {
    val isDark get() = name == "midnight" || base == "midnight"
    /** room0's pair: the mark and the strip run a gradient between them. */
    val r0From get() = if (isDark) hex(0x39C6FF) else hex(0x0B8FD0)
    val r0To get() = if (isDark) hex(0x9D8EF5) else hex(0x2941C8)
    val r0Gradient get() = Brush.horizontalGradient(listOf(r0From, r0To))
    /** The accent where it sits on the chrome bar (the wordmark): the creme
     *  accent is darkened for AA on the page, so the bar gets its own. */
    val accentChrome get() = if (isDark) hex(0xEF6A90) else hex(0xEC6189)
    val scrim get() = Color.Black.copy(alpha = 0.35f)
    val onAccentLine get() = Color.White.copy(alpha = 0.22f)

    /** Stable per-nick colour, mIRC style: same ten, same order, same hash as
     *  web/src/alertcard.ts nickColor. */
    fun nick(n: String): Color {
        var h = 0
        for (c in n.codePoints()) h = (h * 31 + c) and 0x7fffffff
        return listOf(accent, link, join, action, agent, gold, azure, teal, olive, magenta)[h % 10]
    }

    companion object {
        val creme = Theme(
            "creme",
            hex(0x1A2138), hex(0xF5F1E8), hex(0x9AA3C4),
            hex(0xE9E3D2), hex(0xF5F1E8), hex(0xECE7D8), hex(0xECE7D8),
            hex(0x23262E), hex(0x383C4A), hex(0x6A6453), hex(0xC2BBA6), hex(0xCBC4AE),
            hex(0xBB2851), hex(0x2941C8), hex(0x32712E), hex(0x8A35C8), hex(0x2941C8),
            hex(0x994F2C), hex(0x7A5F15),
            hex(0x2F60BB), hex(0x196F6F), hex(0x5B690F), hex(0xA73491),
        )
        val midnight = Theme(
            "midnight",
            hex(0x232B4A), hex(0xF0ECE0), hex(0x99A1C2),
            hex(0x12172A), hex(0x171D33), hex(0x212843), hex(0x101528),
            hex(0xE2DDCD), hex(0xC9C4B2), hex(0x8991AC), hex(0x2E3654), hex(0x364060),
            hex(0xEB658B), hex(0x7D92F0), hex(0x93C99A), hex(0xC084F0), hex(0x7D92F0),
            hex(0xEBA98A), hex(0xF2C14E),
            hex(0x9FC0FF), hex(0x5FC9C9), hex(0xB9C85A), hex(0xE08AD6),
        )
    }
}

val LocalTheme = staticCompositionLocalOf { Theme.creme }
val t: Theme @Composable get() = LocalTheme.current

/** The chat is a monospace grid: ASCII art (spec F10) only lines up if every
 *  glyph is the same width, so this is load-bearing rather than decorative. */
object Face {
    fun mono(size: Float, weight: FontWeight = FontWeight.Medium) =
        TextStyle(fontFamily = FontFamily.Monospace, fontSize = size.sp, fontWeight = weight)
    fun serif(size: Float) =
        TextStyle(fontFamily = FontFamily.Serif, fontSize = size.sp, fontWeight = FontWeight.Black)
    fun sans(size: Float, weight: FontWeight = FontWeight.Normal) =
        TextStyle(fontSize = size.sp, fontWeight = weight)
}
