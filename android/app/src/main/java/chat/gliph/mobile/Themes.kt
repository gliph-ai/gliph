//  Custom themes (mobile/Sources/Extend.swift). The core checks a theme file
//  (core/src/client/theme.rs) and hands over its name, base and tokens; this
//  keeps them per device beside the theme choice and builds a Theme from them.
//  A token the file leaves out is its base's.

package chat.gliph.mobile

import android.content.SharedPreferences
import androidx.compose.ui.graphics.Color
import chat.gliph.mobile.ui.Theme
import org.json.JSONObject

object Themes {
    private const val KEY = "gliph.themes"
    lateinit var prefs: SharedPreferences

    /** name → the core's checked theme JSON ({name, base, tokens}). */
    private var installed: JSONObject
        get() = runCatching { JSONObject(prefs.getString(KEY, "{}") ?: "{}") }.getOrDefault(JSONObject())
        set(v) { prefs.edit().putString(KEY, v.toString()).apply() }

    fun named(n: String): Theme = when {
        installed.has(n) -> custom(installed.getJSONObject(n))
        n == "midnight" -> Theme.midnight
        else -> Theme.creme
    }

    data class Result(val say: String, val level: String, val show: Theme?)

    /** The `Theme` effect: what to tell the user, and the theme to show. */
    fun effect(op: String, name: String, json: String): Result {
        val all = installed
        return when (op) {
            "add" -> {
                val t = runCatching { JSONObject(json) }.getOrNull()
                    ?: return Result("theme not installed", "error", null)
                val n = t.optString("name")
                all.put(n, t); installed = all
                Result("installed theme $n. /theme creme or /theme midnight goes back", "info", custom(t))
            }
            "use" -> when {
                name == "creme" || name == "midnight" -> Result("theme: $name", "info", named(name))
                all.has(name) -> Result("theme: $name", "info", custom(all.getJSONObject(name)))
                else -> Result("no theme called $name. /theme add installs one", "error", null)
            }
            "remove" -> if (all.remove(name) == null) Result("no theme called $name", "error", null)
                else { installed = all; Result("removed theme $name", "info", null) }
            else -> {
                val names = listOf("creme", "midnight") + all.keys().asSequence().sorted()
                Result("themes: ${names.joinToString(", ")}. /theme <name> switches, /theme add installs", "info", null)
            }
        }
    }

    private fun custom(c: JSONObject): Theme {
        val base = c.optString("base")
        val b = if (base == "midnight") Theme.midnight else Theme.creme
        val tok = c.optJSONObject("tokens") ?: JSONObject()
        fun k(token: String, fallback: Color): Color {
            val v = tok.optString(token)
            if (v.length != 7 || v[0] != '#') return fallback
            return v.drop(1).toLongOrNull(16)?.let { Color(0xFF000000 or it) } ?: fallback
        }
        return Theme(
            c.optString("name"),
            k("chrome", b.chrome), k("chrome-text", b.chromeText), k("chrome-dim", b.chromeDim),
            k("bg", b.bg), k("surface", b.surface), k("surface-2", b.surface2), k("rail", b.rail),
            k("text", b.text), k("text-2", b.text2), k("muted", b.muted), k("border", b.border), k("border-2", b.border2),
            k("accent", b.accent), k("link", b.link), k("join", b.join), k("action", b.action), k("mode", b.mode),
            k("agent", b.agent), k("gold", b.gold),
            k("azure", b.azure), k("teal", b.teal), k("olive", b.olive), k("magenta", b.magenta),
            base,
        )
    }
}
