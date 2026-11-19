//  What the core hands back.
//
//  Every one of these mirrors a `#[derive(Serialize)]` in core/src, as
//  mobile/Sources/Models.swift does. The wire between the app and the core is
//  JSON, decoded here with the org.json that ships in Android: every field has
//  a default, so a core that grows a field this build has never heard of is not
//  a reason to show the user nothing.

package chat.gliph.mobile

import android.util.Base64
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant

private fun JSONObject.s(k: String, d: String = "") = if (isNull(k)) d else optString(k, d)
private fun JSONArray?.objs(): List<JSONObject> =
    if (this == null) emptyList() else (0 until length()).mapNotNull { optJSONObject(it) }
private fun JSONArray?.strs(): List<String> =
    if (this == null) emptyList() else (0 until length()).map { optString(it) }

/** Never silently: a decode that fails renders an empty screen. */
inline fun <T> decode(what: String, json: String, fallback: T, f: (String) -> T): T =
    try { f(json) } catch (e: Exception) {
        Log.w("gliph", "could not decode $what: $e"); fallback
    }

data class User(val id: String = "", val nick: String = "", val role: String = "member") {
    companion object {
        fun of(o: JSONObject?) = o?.let { User(it.s("id"), it.s("nick"), it.s("role", "member")) }
    }
}

data class Member(
    val userId: String, val nick: String, val role: String, val away: String?,
    val kind: String, val online: Boolean,
    /** Has a device no other device of the account approved yet. */
    val unverified: Boolean = false,
) {
    val id get() = userId.ifEmpty { nick }
    /** mIRC's sigils: @ is an op, + has voice, ◆ is not a person at all. */
    val sigil get() = if (kind != "user") "◆" else when (role) { "op" -> "@"; "voice" -> "+"; else -> " " }

    companion object {
        fun of(o: JSONObject) = Member(
            o.s("user_id"), o.s("nick"), o.s("role", "member"),
            if (o.isNull("away")) null else o.optString("away"),
            o.s("kind", "user"), o.optBoolean("online"), o.optBoolean("unverified"),
        )
    }
}

data class Channel(
    val id: String, val name: String, val topic: String, val modes: String, val isDm: Boolean,
    val role: String, val members: List<Member>, val unread: Long,
    /** open: newcomers read what came before; sealed: they do not. */
    val history: String = "open",
) {
    val encrypted get() = modes.contains('e')
    val nearby get() = id.startsWith("n_")
    val label get() = if (isDm) name else "#$name"

    companion object {
        fun of(o: JSONObject) = Channel(
            o.s("id"), o.s("name"), o.s("topic"), o.s("modes"), o.optBoolean("is_dm"),
            o.s("role", "member"), o.optJSONArray("members").objs().map(Member::of),
            o.optLong("unread"), o.s("history", "open"),
        )
    }
}

/** The operator's MOTD, or the notes for a release: a card at the live end. */
data class SysMsg(
    val id: String, val kind: String, val icon: String, val title: String, val body: String,
    val date: String,
) {
    companion object {
        fun of(o: JSONObject) = SysMsg(
            o.s("id"), o.s("kind", "news"), o.s("icon"), o.s("title"), o.s("body"), o.s("date"),
        )
    }
}

data class Notice(val channel: String = "", val level: String = "info", val text: String = "") {
    companion object {
        fun of(o: JSONObject) = Notice(o.s("channel"), o.s("level", "info"), o.s("text"))
    }
}

data class Command(
    val name: String, val usage: String, val desc: String, val source: String,
) {
    companion object {
        fun of(o: JSONObject) = Command(o.s("name"), o.s("usage"), o.s("desc"), o.s("source", "builtin"))
        fun list(json: String) = JSONArray(json).objs().map(::of)
    }
}

data class Transfer(
    val id: String, val dir: String, val peerNick: String, val name: String, val size: Long,
    val bytes: Long, val acked: Long, val state: String, val error: String?,
) {
    /** On a send this is what the RECEIVER confirmed, not what went out. */
    val delivered get() = if (dir == "send") acked else bytes
    val progress get() = if (size == 0L) 0f else minOf(1f, delivered.toFloat() / size)

    companion object {
        fun of(o: JSONObject) = Transfer(
            o.s("id"), o.s("dir", "recv"), o.s("peer_nick"), o.s("name"), o.optLong("size"),
            o.optLong("bytes"), o.optLong("acked"), o.s("state", "offered"),
            if (o.isNull("error")) null else o.optString("error"),
        )
    }
}

/** One event, with the decryption already resolved by the core. `text` is null
 *  only when this device has no key for the epoch. */
data class EventRow(
    val id: String, val kind: String, val actor: String, val ts: String, val body: String,
    val text: String?, val quote: String, val highlight: Boolean, val meta: JSONObject?,
) {
    val pending get() = meta?.optBoolean("pending") ?: false
    /** Which sub-room of room0 this was said in, "" for the chat window. */
    val room get() = meta?.s("room") ?: ""
    val replyTo get() = meta?.s("reply_to") ?: ""
    val locked get() = text == null
    val readable get() = text ?: "cannot decrypt — this device has no key for this message"
    fun metaS(k: String) = meta?.s(k) ?: ""
    fun metaL(k: String) = meta?.optLong(k) ?: 0L
    fun metaB(k: String) = meta?.optBoolean(k) ?: false
    val at: Instant get() = runCatching { Instant.parse(ts) }.getOrElse { Instant.now() }

    companion object {
        fun of(o: JSONObject) = EventRow(
            o.s("id"), o.s("kind", "message"), o.s("actor"), o.s("ts"), o.s("body"),
            if (o.isNull("text") || !o.has("text")) null else o.getString("text"),
            o.s("quote"), o.optBoolean("highlight"), o.optJSONObject("meta"),
        )
        fun list(json: String) = JSONArray(json).objs().map(::of)
    }
}

data class RoomStrip(
    val channel: String, val room: String, val since: String, val people: List<String>,
    val voiceCount: Int,
) {
    /** How long the room has been running, in the roughest unit that is true. */
    val elapsed: String get() {
        val at = runCatching { Instant.parse(since) }.getOrNull() ?: return ""
        val m = (Instant.now().epochSecond - at.epochSecond) / 60
        return when { m < 1 -> "just now"; m < 60 -> "${m}m"; else -> "${m / 60}h" }
    }

    companion object {
        fun of(o: JSONObject) = RoomStrip(
            o.s("channel"), o.s("room"), o.s("since"),
            o.optJSONArray("people").objs().map { it.s("name") }, o.optInt("voice_count"),
        )
    }
}

/** This device's standing with its account (docs/protocol.md #Device trust). */
data class DeviceTrust(
    val known: Boolean = false, val signed: Boolean = false,
    /** This device's approval code, to read off it. */
    val code: String = "",
    /** The code as QR rows ("1" dark) of gliph://approve/<code>, while unsigned. */
    val qr: List<String> = emptyList(),
    val pending: List<Pair<String, String>> = emptyList(), // name, code
) {
    companion object {
        fun of(o: JSONObject?) = if (o == null) DeviceTrust() else DeviceTrust(
            o.optBoolean("known"), o.optBoolean("signed"), o.s("code"),
            o.optJSONArray("qr").let { a -> if (a == null) emptyList() else List(a.length()) { a.optString(it) } },
            o.optJSONArray("pending").objs().map { it.s("name") to it.s("code") },
        )
    }
}

data class Snapshot(
    val server: String = "",
    val serverName: String = "",
    val online: Boolean = false,
    val solo: Boolean = false,
    val active: String = "",
    val user: User? = null,
    val channels: List<Channel> = emptyList(),
    val unreadTotal: Long = 0,
    val notices: List<Notice> = emptyList(),
    val system: List<SysMsg> = emptyList(),
    val transfers: List<Transfer> = emptyList(),
    val nearbyOn: Boolean = false,
    val nearbyPeers: Int = 0,
    val commandCount: Int = 0,
    val pluginCommandCount: Int = 0,
    val activeBlob: String = "",
    /** `<alice> what she said` while a reply is pending, "" otherwise. */
    val reply: String = "",
    val rooms: List<RoomStrip> = emptyList(),
    val device: DeviceTrust = DeviceTrust(),
) {
    companion object {
        fun of(json: String): Snapshot {
            val o = JSONObject(json)
            return Snapshot(
                o.s("server"), o.s("server_name"), o.optBoolean("online"), o.optBoolean("solo"),
                o.s("active"), User.of(o.optJSONObject("user")),
                o.optJSONArray("channels").objs().map(Channel::of), o.optLong("unread_total"),
                o.optJSONArray("notices").objs().map(Notice::of),
                o.optJSONArray("system").objs().map(SysMsg::of),
                o.optJSONArray("transfers").objs().map(Transfer::of),
                o.optBoolean("nearby_on"), o.optInt("nearby_peers"), o.optInt("command_count"),
                o.optInt("plugin_command_count"), o.s("active_blob"), o.s("reply"),
                o.optJSONArray("rooms").objs().map(RoomStrip::of),
                DeviceTrust.of(o.optJSONObject("device")),
            )
        }
    }
}

// ---- the blob surface (GUM) ----

data class BlobSpec(val id: String, val title: String, val source: String)
data class MenuEntry(val id: String, val label: String, val selected: Boolean)
data class GumButton(val index: Int, val label: String, val selected: Boolean)

/** A drawable node. The core resolved selection, numbering and submenus before
 *  this got here: a frontend paints it and reports taps, and decides nothing. */
sealed interface GumNode {
    data class Text(val body: String, val style: String) : GumNode
    data class Divider(val label: String) : GumNode
    data class Row(
        val index: Int, val number: Int, val label: String, val hint: String, val state: String,
        val hasActions: Boolean, val selected: Boolean,
    ) : GumNode
    data class Table(val cols: List<String>, val rows: List<List<String>>) : GumNode
    data class Input(
        val index: Int, val label: String, val placeholder: String, val value: String,
        val selected: Boolean,
    ) : GumNode
    data class Buttons(val buttons: List<GumButton>) : GumNode
    data class Bar(val body: String) : GumNode

    companion object {
        /** A node kind from a newer core is skipped rather than fatal. */
        fun of(o: JSONObject): GumNode? = when (o.s("kind")) {
            "Text" -> Text(o.s("body"), o.s("style", "plain"))
            "Divider" -> Divider(o.s("label"))
            "Row" -> Row(
                o.optInt("index"), o.optInt("number"), o.s("label"), o.s("hint"), o.s("state"),
                o.optBoolean("has_actions"), o.optBoolean("selected"),
            )
            "Table" -> Table(
                o.optJSONArray("cols").strs(),
                o.optJSONArray("rows").let { a ->
                    if (a == null) emptyList() else (0 until a.length()).map { a.optJSONArray(it).strs() }
                },
            )
            "Input" -> Input(
                o.optInt("index"), o.s("label"), o.s("placeholder"), o.s("value"),
                o.optBoolean("selected"),
            )
            "Buttons" -> Buttons(o.optJSONArray("buttons").objs().map {
                GumButton(it.optInt("index"), it.s("label"), it.optBoolean("selected"))
            })
            "Bar" -> Bar(o.s("body"))
            else -> null
        }
    }
}

data class Surface(
    val blob: String = "",
    val loaded: Boolean = false,
    val nodes: List<GumNode> = emptyList(),
    val menu: List<MenuEntry>? = null,
    val blobs: List<BlobSpec> = emptyList(),
) {
    companion object {
        fun of(json: String): Surface {
            val o = JSONObject(json)
            return Surface(
                o.s("blob"), o.optBoolean("loaded"),
                o.optJSONArray("nodes").objs().mapNotNull(GumNode::of),
                o.optJSONArray("menu")?.objs()?.map {
                    MenuEntry(it.s("id"), it.s("label"), it.optBoolean("selected"))
                },
                o.optJSONArray("blobs").objs().map { BlobSpec(it.s("id"), it.s("title"), it.s("source", "builtin")) },
            )
        }
    }
}

// ---- effects ----

/** What the core asks this app to do. A sealed type so a new effect in Rust is
 *  one `when` branch here rather than a silently ignored case. */
sealed interface Effect {
    data class Send(val json: String) : Effect
    data class Persist(val key: String, val value: String, val secret: Boolean) : Effect
    data class Forget(val key: String, val secret: Boolean) : Effect
    data class Alert(
        val id: String, val channel: String, val nick: String, val text: String,
        val isDm: Boolean, val event: String,
    ) : Effect
    data object Changed : Effect
    data class MeshStart(val nick: String) : Effect
    data object MeshStop : Effect
    data class MeshJoin(val chans: List<String>) : Effect
    data class MeshSay(val chan: String, val body: String, val kind: String, val replyTo: String) : Effect
    data class MeshSendTo(val peer: String, val t: String, val json: String) : Effect
    data class SaveFile(val name: String, val mime: String, val bytes: ByteArray) : Effect
    data object PickFile : Effect
    data class AskConfirm(val message: String) : Effect
    data class AskPrompt(val message: String) : Effect
    data object Update : Effect
    /** The core wants a multi-line editor: a theme file or a script. */
    data class Compose(val title: String, val text: String, val submit: String) : Effect
    /** /theme: add, use, remove or list a custom theme. */
    data class Theme(val op: String, val name: String, val theme: String) : Effect

    companion object {
        fun list(json: String): List<Effect> = decode("effects", json, emptyList()) {
            JSONArray(it).objs().mapNotNull { o ->
                when (o.s("kind")) {
                    "Send" -> Send(o.s("json"))
                    "Persist" -> Persist(o.s("key"), o.s("value"), o.s("store") == "Secret")
                    "Forget" -> Forget(o.s("key"), o.s("store") == "Secret")
                    "Alert" -> Alert(o.s("id"), o.s("channel"), o.s("nick"), o.s("text"),
                        o.optBoolean("is_dm"), o.s("event"))
                    "Changed" -> Changed
                    "MeshStart" -> MeshStart(o.s("nick"))
                    "MeshStop" -> MeshStop
                    "MeshJoin" -> MeshJoin(o.optJSONArray("chans").strs())
                    "MeshSay" -> MeshSay(o.s("chan"), o.s("body"), o.s("msg_kind"), o.s("reply_to"))
                    "MeshSendTo" -> MeshSendTo(o.s("peer"), o.s("t"), o.s("json"))
                    "SaveFile" -> SaveFile(o.s("name"), o.s("mime"),
                        Base64.decode(o.s("bytes"), Base64.DEFAULT))
                    "PickFile" -> PickFile
                    "AskConfirm" -> AskConfirm(o.s("message"))
                    "AskPrompt" -> AskPrompt(o.s("message"))
                    "Update" -> Update
                    "Compose" -> Compose(o.s("title"), o.s("text"), o.s("submit"))
                    "Theme" -> Theme(o.s("op"), o.s("name"), o.s("theme"))
                    else -> null
                }
            }
        }
    }
}

// ---- servers (the switcher) ----

/** One signed-in server, as core/src/servers.rs persists it. */
data class ServerEntry(
    val host: String, val token: String, val name: String, val nick: String, val icon: String,
    val unread: Int,
) {
    val shortHost get() = host.removePrefix("https://").removePrefix("http://")
    /** Two letters for a server with no icon of its own. */
    val monogram get() = name.ifEmpty { shortHost }.filter { it.isLetterOrDigit() }.take(2).uppercase()
}

data class ServerList(val list: List<ServerEntry> = emptyList(), val active: String = "") {
    companion object {
        fun of(json: String): ServerList {
            val o = JSONObject(json)
            return ServerList(
                o.optJSONArray("list").objs().map {
                    ServerEntry(it.s("host"), it.s("token"), it.s("name"), it.s("nick"), it.s("icon"), it.optInt("unread"))
                },
                o.s("active"),
            )
        }
    }
}
