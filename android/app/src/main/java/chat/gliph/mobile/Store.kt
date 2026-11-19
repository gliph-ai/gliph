//  The Android host for the Rust core: mobile/Sources/Store.swift, in Kotlin.
//
//  Everything the client DECIDES lives in core/: the protocol, the crypto, the
//  sync, the offline cache, the key epochs, the nearby state, the transfer
//  engine. This file is what the core cannot be: the socket, the key store,
//  SharedPreferences, the mesh, the notifications, the files.
//
//  The loop is always the same three lines. Call into the core, run the effects
//  it returns, publish a fresh snapshot. There is no other path by which this
//  app changes. Everything here runs on the main thread, as on iOS.

package chat.gliph.mobile

import android.annotation.SuppressLint
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.os.Handler
import android.os.Looper
import android.provider.MediaStore
import android.util.Log
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.core.app.NotificationCompat
import androidx.core.content.FileProvider
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject
import uniffi.gliph_core.Core
import java.io.File
import java.util.concurrent.TimeUnit

/** A failure with something to say to the user. */
class Fail(message: String) : Exception(message)

/** A question the core (or this host) needs a human to answer before an
 *  action can fire. `accept` and `onAnswer` are set for a host question, an
 *  incoming DM transfer: the yes label, and who hears the answer. */
data class Ask(
    val message: String, val wantsText: Boolean,
    val accept: String? = null, val onAnswer: ((Boolean) -> Unit)? = null,
)

fun now() = System.currentTimeMillis()

@SuppressLint("StaticFieldLeak") // the application context, which outlives everything
object Store {
    private val core = Core()
    private lateinit var ctx: Context
    private lateinit var prefs: SharedPreferences
    private val main = Handler(Looper.getMainLooper())
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)

    var snap by mutableStateOf(Snapshot()); private set
    var rows by mutableStateOf(emptyList<EventRow>()); private set
    var palette by mutableStateOf(emptyList<Command>()); private set
    var surface by mutableStateOf(Surface()); private set
    /** Every server this account is signed in to, and which one is on screen. */
    var servers by mutableStateOf(ServerList()); private set
    /** A confirm or prompt the core is waiting on. Only one at a time. */
    var ask by mutableStateOf<Ask?>(null)
    var signedIn by mutableStateOf(false); private set
    /** The theme on screen: creme, midnight, or an installed custom theme. */
    var theme by mutableStateOf(chat.gliph.mobile.ui.Theme.creme)
        private set
    /** The multi-line editor the core asked for (/theme add, /script add). */
    var compose by mutableStateOf<Effect.Compose?>(null)
    /** The code from a gliph://approve/<code> link, until the person confirms. */
    var approveCode by mutableStateOf<String?>(null)
    /** name → percent while a file download runs; the stream shows it as a line. */
    val saving = mutableStateMapOf<String, Int>()

    /** True while an activity is on screen: the active channel is being looked at. */
    var foreground = false
    /** Set by the chat screen: opening the file picker is the UI's business. */
    var onPickFile: (() -> Unit)? = null

    private val http = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        // A socket the OS quietly killed still looks open; a ping with no pong
        // inside the interval fails it, which redials.
        .pingInterval(5, TimeUnit.SECONDS)
        .build()
    private var socket: WebSocket? = null
    private var retry = 0
    private var closed = false
    private var mesh: Mesh? = null
    private var started = false

    private const val KEY_DEVICE = "gliph.device"
    private const val KEY_SERVERS = "gliph.servers"

    val pinnedServer: String get() = BuildConfig.GLIPH_SERVER
    val hasPinnedServer get() = pinnedServer.isNotEmpty()

    fun init(context: Context) {
        if (started) return
        started = true
        ctx = context.applicationContext
        prefs = ctx.getSharedPreferences("gliph", Context.MODE_PRIVATE)
        Secrets.init(ctx)
        Themes.prefs = prefs
        theme = Themes.named(prefs.getString("gliph.theme", "") ?: "")
        core.restoreServers(prefs.getString(KEY_SERVERS, "") ?: "")

        // The registry is the source of truth for where we are and what token
        // gets us in; the session is downstream of it.
        val active = core.activeServer()
        val here = ServerList.of(core.serversJson()).list.firstOrNull { it.host == active }
        val session = here?.let { JSONObject().put("token", it.token).put("server", it.host).toString() } ?: ""
        core.restore(session, prefs.getString(core.cacheKeyFor(active), "") ?: "", Secrets.read(KEY_DEVICE) ?: "")
        if (core.server().isEmpty()) core.setServer(pinnedServer)
        refresh()

        // The only thing the core needs a clock for: failing a transfer whose
        // other end has gone quiet.
        tickEvery(5_000) { run(core.tick(now())) }
        // The bracket on every server we are NOT looking at. A poll, not a
        // socket per server: a count a minute stale is the right accuracy.
        tickEvery(60_000) { scope.launch { pollServers() } }
        scope.launch { pollServers() }
        if (signedIn) connect()
    }

    private fun tickEvery(ms: Long, f: () -> Unit) {
        main.postDelayed(object : Runnable {
            override fun run() { f(); main.postDelayed(this, ms) }
        }, ms)
    }

    fun useTheme(t: chat.gliph.mobile.ui.Theme) {
        theme = t
        prefs.edit().putString("gliph.theme", t.name).apply()
    }

    // ---- effects ----

    private fun run(json: String) {
        var changed = false
        for (effect in Effect.list(json)) {
            when (effect) {
                is Effect.Send -> socket?.send(effect.json)
                is Effect.Persist ->
                    if (effect.secret) Secrets.write(effect.key, effect.value)
                    else prefs.edit().putString(effect.key, effect.value).apply()
                is Effect.Forget ->
                    if (effect.secret) Secrets.delete(effect.key)
                    else prefs.edit().remove(effect.key).apply()
                is Effect.Alert -> notify(effect)
                Effect.Changed -> changed = true
                is Effect.MeshStart -> startMesh(effect.nick)
                Effect.MeshStop -> { mesh?.stop(); mesh = null }
                is Effect.MeshJoin -> mesh?.join(effect.chans)
                is Effect.MeshSay ->
                    mesh?.say(effect.chan, effect.body, effect.kind, effect.replyTo)
                        ?.let { run(core.meshMsg(it, now())) }
                is Effect.MeshSendTo -> mesh?.sendTo(effect.peer, effect.t, effect.json)
                is Effect.SaveFile -> save(effect.name, effect.mime, effect.bytes)
                Effect.PickFile -> onPickFile?.invoke()
                // The core cannot draw a dialog, so it asks. Answering is what
                // fires the action: a cancelled confirm must never delete.
                is Effect.AskConfirm -> ask = Ask(effect.message, false)
                is Effect.AskPrompt -> ask = Ask(effect.message, true)
                Effect.Update -> run(core.notice("this app updates through the Play Store", "info"))
                is Effect.Compose -> compose = effect
                is Effect.Theme -> {
                    val r = Themes.effect(effect.op, effect.name, effect.theme)
                    r.show?.let(::useTheme)
                    run(core.notice(r.say, r.level))
                }
            }
        }
        if (changed) refresh()
    }

    private fun refresh() {
        // Mark before decoding, so the snapshot already shows the badge gone.
        if (foreground) run(core.markActiveRead())
        snap = decode("snapshot", core.snapshot(), Snapshot(), Snapshot::of)
        if (pendingDm.isNotEmpty()) {
            snap.channels.firstOrNull { it.isDm && it.name == pendingDm }?.let {
                pendingDm = ""
                setActive(it.id)
            }
        }
        rows = decode("events", core.events(snap.active), emptyList(), EventRow::list)
        surface = decode("surface", core.surface(), Surface(), Surface::of)
        servers = decode("servers", core.serversJson(), ServerList(), ServerList::of)
        signedIn = core.token().isNotEmpty() || snap.solo
    }

    // ---- what the screens call ----

    fun submit(text: String) = run(core.submit(text))

    /** A tap on @nick: switch to that DM, opening one first if there is none.
     *  The switch happens in refresh() when the conversation shows up. */
    private var pendingDm = ""
    fun openDm(nick: String) {
        if (nick.isEmpty() || nick == snap.user?.nick) return
        snap.channels.firstOrNull { it.isDm && it.name == nick }?.let { return setActive(it.id) }
        pendingDm = nick
        submit("/msg $nick")
    }
    fun setActive(cid: String) = run(core.setActive(cid))
    fun setReply(eventId: String) = run(core.setReply(eventId))
    fun dismissSystem(id: String) = run(core.dismissSystem(id))
    fun accept(id: String) = run(core.acceptTransfer(id))
    fun decline(id: String) = run(core.declineTransfer(id))
    fun offerFile(name: String, mime: String, bytes: ByteArray) {
        keepP2P(name, bytes)
        // Nearby goes over the mesh link (the core drives it); a DM goes over
        // WebRTC, straight to the other person's device.
        channel()?.takeIf { it.isDm }?.let { return p2p.offer(it.id, it.name, name, bytes) }
        run(core.offerFile(name, mime, bytes))
    }

    /** Files that moved peer to peer this session, by `name|size`. The copy in
     *  hand is the only copy there is. ponytail: no eviction, as on iOS. */
    private val p2pFiles = HashMap<String, ByteArray>()
    private fun keepP2P(name: String, bytes: ByteArray) { p2pFiles["$name|${bytes.size}"] = bytes }
    fun p2pBytes(key: String) = p2pFiles[key]

    // ---- the blob surface (GUM): every one of these is a forward ----

    fun openBlob(id: String) = run(core.openBlob(id))
    fun gumTap(index: Int) = run(core.gumTap(index.toUInt()))
    fun gumTapMenu(id: String) = run(core.gumTapMenu(id))
    fun gumEscape() = run(core.gumEscape())
    fun gumActivate() = run(core.gumActivate())
    fun gumSetDraft(text: String) = run(core.gumSetDraft(core.gumFocusedInput(), text))
    /** Typing into an input on a phone means selecting it too. */
    fun gumFocusInput(index: Int) = run(core.gumJump((index + 1).toUInt()))

    fun answer(ok: Boolean, value: String = "") {
        val was = ask
        ask = queuedAsks.removeFirstOrNull()
        was?.onAnswer?.let { return it(ok) }
        run(core.gumAnswer(ok, value))
    }

    /** A host-side question waits its turn behind whatever is on screen. */
    private val queuedAsks = ArrayDeque<Ask>()
    private fun prompt(a: Ask) { if (ask == null) ask = a else queuedAsks.addLast(a) }

    /** DM transfers over WebRTC (P2P.kt). The core does not model p2p.*
     *  frames; this host does, as the web and iOS hosts do. */
    private val p2p by lazy {
        P2P(ctx, P2PHost(
            send = { socket?.send(it.toString()) },
            confirm = { x, answer ->
                prompt(Ask("${x.peer} wants to send you ${x.name} (${x.size} bytes) directly", false, "accept", answer))
            },
            ice = { iceServers() },
            notice = { text, level -> run(core.notice(text, level)) },
            done = { x, bytes ->
                if (bytes != null) save(x.name, java.net.URLConnection.guessContentTypeFromName(x.name) ?: "", bytes)
                // The line in the channel is the record, the same shape at both ends.
                run(core.p2pFileLine(x.channel, x.id, x.name, x.size.toDouble(), x.send, x.peer))
            },
        ))
    }

    private var ice: List<org.webrtc.PeerConnection.IceServer>? = null
    /** `GET /api/ice`, once per session. An older server has none; the
     *  default STUN server takes over. */
    private suspend fun iceServers(): List<org.webrtc.PeerConnection.IceServer> {
        ice?.let { return it }
        val got = api("/api/ice")?.let { url ->
            withContext(Dispatchers.IO) {
                runCatching { http.newCall(authed(url)).execute().use { it.body?.string() ?: "" } }.getOrDefault("")
            }
        }?.let(P2P::iceServers) ?: emptyList()
        ice = got
        return got
    }

    /** p2p.* frames go to P2P.kt; the core has no model of them. */
    private fun peekP2P(json: String): Boolean {
        if (!json.contains("\"p2p.")) return false
        val f = runCatching { JSONObject(json) }.getOrNull() ?: return false
        val type = f.optString("t")
        if (!type.startsWith("p2p.")) return false
        p2p.handle(type, f.optJSONObject("d") ?: JSONObject())
        return true
    }

    fun updatePalette(text: String) {
        palette = decode("palette", core.palette(text), emptyList(), Command::list)
    }

    fun channel(cid: String? = null) = snap.channels.firstOrNull { it.id == (cid ?: snap.active) }

    val server: String get() = core.server()
    val token: String get() = core.token()

    fun useDefaultServer() {
        core.setServer(pinnedServer)
        refresh()
    }

    // ---- auth ----

    /** The core owns the session; this owns the request. */
    suspend fun signIn(server: String, nick: String, password: String) {
        adopt(server, post(server, "/api/login", mapOf("nick" to nick, "password" to password)))
    }

    suspend fun register(server: String, nick: String, email: String, password: String, invite: String) {
        val body = mapOf("nick" to nick, "email" to email, "password" to password, "invite" to invite)
        adopt(server, post(server, "/api/register", body))
    }

    private fun adopt(server: String, r: JSONObject) {
        val token = r.optString("token")
        val user = r.optJSONObject("user") ?: JSONObject()
        run(core.setSession(server, token, user.toString()))
        run(core.addServer(server, token, user.optString("nick")))
        connect()
        Push.register()
    }

    fun startSolo(nick: String) = run(core.startSolo(nick, "nearby"))

    fun signOut() {
        closed = true
        Push.unregister()
        api("/api/logout")?.let { url ->
            val req = Request.Builder().url(url).post(ByteArray(0).toRequestBody())
                .header("Authorization", "Bearer ${core.token()}").build()
            scope.launch(Dispatchers.IO) { runCatching { http.newCall(req).execute().close() } }
        }
        socket?.cancel(); socket = null
        mesh?.stop(); mesh = null
        val leaving = core.activeServer()
        run(core.logout())
        // Signing out of one server is not signing out of the product.
        run(core.forgetServer(leaving))
        closed = false
        servers.list.firstOrNull { it.host == servers.active }?.let { switchTo(it.host, force = true) }
    }

    // ---- servers (the switcher) ----

    val unreadElsewhere: Int get() = core.unreadElsewhere().toInt()
    val activeServer: ServerEntry? get() = servers.list.firstOrNull { it.host == servers.active }

    /** Put another server on screen. One server, one session, one connection. */
    fun switchTo(host: String, force: Boolean = false) {
        if ((!force && host == core.activeServer()) || host.isEmpty()) return
        socket?.cancel(); socket = null
        // The cache for where we are going is handed in; the token is NOT: it
        // comes from the registry, so nothing can point the session at a
        // server we are not signed in to.
        val cache = prefs.getString(core.cacheKeyFor(host), "") ?: ""
        run(core.switchServer(host, cache, "{}"))
        connect()
        Push.register()
    }

    /** Sign in on another server without disturbing the one on screen. `post`
     *  carries no Authorization header, which is what makes it safe here. */
    suspend fun addServer(server: String, nick: String, password: String) {
        val host = core.normaliseServer(server)
        val r = post(host, "/api/login", mapOf("nick" to nick, "password" to password))
        run(core.addServer(host, r.optString("token"), r.optJSONObject("user")?.optString("nick") ?: ""))
        switchTo(host, force = true)
    }

    /** Refresh the bracket on every server we are NOT looking at. */
    suspend fun pollServers() {
        val here = core.activeServer()
        for (e in servers.list) {
            if (e.host == here || e.token.isEmpty()) continue
            val req = runCatching {
                Request.Builder().url(e.host + "/api/me").header("Authorization", "Bearer ${e.token}").build()
            }.getOrNull() ?: continue
            val (code, body) = withContext(Dispatchers.IO) {
                runCatching { http.newCall(req).execute().use { it.code to (it.body?.string() ?: "") } }
                    .getOrDefault(0 to "")
            }
            if (code == 401) {
                // The token died. Keep the entry, stop it reporting a count.
                run(core.observeServer(e.host, "", "", "", 0, now()))
                continue
            }
            if (code != 200) continue
            val m = runCatching { JSONObject(body) }.getOrNull() ?: continue
            run(core.observeServer(e.host, m.optString("server"), m.optString("nick"),
                m.optString("icon"), m.optLong("unread"), now()))
        }
    }

    fun api(path: String): String? = core.server().trimEnd('/').takeIf { it.isNotEmpty() }?.plus(path)

    private fun authed(url: String) =
        Request.Builder().url(url).header("Authorization", "Bearer ${core.token()}").build()

    /** Fetch a shared file and hand it to the share sheet. Nothing is kept past it. */
    fun saveFile(id: String, name: String) {
        if (id.startsWith("p2p:")) {
            p2pBytes(id.removePrefix("p2p:"))?.let { share(File(shareDir(), name).apply { writeBytes(it) }) }
            return
        }
        val url = api("/api/file/$id") ?: return
        if (saving.containsKey(name)) return
        saving[name] = 0
        scope.launch {
            val file = withContext(Dispatchers.IO) {
                runCatching {
                    http.newCall(authed(url)).execute().use { r ->
                        if (r.code != 200) return@use null
                        val body = r.body ?: return@use null
                        val total = body.contentLength()
                        val dst = File(shareDir(), name)
                        dst.outputStream().use { out ->
                            val src = body.byteStream()
                            val buf = ByteArray(64 * 1024)
                            var got = 0L
                            while (true) {
                                val n = src.read(buf); if (n < 0) break
                                out.write(buf, 0, n); got += n
                                if (total > 0) main.post { saving[name] = (got * 100 / total).toInt() }
                            }
                        }
                        dst
                    }
                }.getOrNull()
            }
            saving.remove(name)
            file?.let(::share)
        }
    }

    private fun shareDir() = File(ctx.cacheDir, "share").apply { mkdirs() }

    private fun share(file: File) {
        val uri = FileProvider.getUriForFile(ctx, "${ctx.packageName}.files", file)
        val send = Intent(Intent.ACTION_SEND).setType(ctx.contentResolver.getType(uri) ?: "*/*")
            .putExtra(Intent.EXTRA_STREAM, uri).addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        ctx.startActivity(Intent.createChooser(send, file.name).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
    }

    /** The bytes of a shared file, with the token the server wants. */
    suspend fun fileBytes(id: String): ByteArray? {
        val url = api("/api/file/$id") ?: return null
        return withContext(Dispatchers.IO) {
            runCatching { http.newCall(authed(url)).execute().use { if (it.code == 200) it.body?.bytes() else null } }
                .getOrNull()
        }
    }

    private suspend fun post(server: String, path: String, body: Map<String, String>): JSONObject {
        val base = server.trimEnd('/')
        val req = runCatching {
            Request.Builder().url(base + path)
                .post(JSONObject(body).toString().toRequestBody("application/json".toMediaType())).build()
        }.getOrElse { throw Fail("that is not an address") }
        val (code, text) = withContext(Dispatchers.IO) {
            runCatching { http.newCall(req).execute().use { it.code to (it.body?.string() ?: "") } }
                .getOrElse { throw Fail("could not reach $base") }
        }
        val json = runCatching { JSONObject(text) }.getOrNull()
        if (code >= 400 || json == null) {
            // A 404 or a non-JSON reply almost always means the address is not
            // a Gliph server rather than a bad password.
            json?.optString("error")?.takeIf { it.isNotEmpty() }?.let { throw Fail(it) }
            throw Fail("no gliph server at $base (got $code)")
        }
        return json
    }

    // ---- socket ----

    fun connect() {
        if (core.token().isEmpty() || closed) return
        val base = core.server()
        val url = when {
            base.startsWith("https://") -> "wss://" + base.removePrefix("https://")
            base.startsWith("http://") -> "ws://" + base.removePrefix("http://")
            else -> return
        } + "/ws?token=" + java.net.URLEncoder.encode(core.token(), "UTF-8") +
            // What /status names this connection: the web says web/<version>.
            "&client=" + java.net.URLEncoder.encode("android/${BuildConfig.VERSION_NAME}", "UTF-8")
        socket?.cancel()
        val req = runCatching { Request.Builder().url(url).build() }.getOrNull() ?: return
        lateinit var ws: WebSocket
        ws = http.newWebSocket(req, object : WebSocketListener() {
            override fun onMessage(webSocket: WebSocket, text: String) {
                main.post {
                    if (socket !== ws) return@post
                    retry = 0
                    if (!peekP2P(text)) run(core.handleFrame(text, now()))
                }
            }
            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                main.post { if (socket === ws) dropped() }
            }
            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                main.post { if (socket === ws) dropped() }
            }
        })
        socket = ws
        run(core.connected("Android", now()))
        // Catch-up arrives in the next few frames; announcements arm once it
        // has settled, or forty banners land at once after a reconnect.
        main.postDelayed({ core.goLive() }, 2500)
    }

    private fun dropped() {
        if (closed) return
        socket = null
        run(core.disconnected(now()))
        retry = minOf(retry + 1, 6)
        main.postDelayed({ connect() }, 500L * (1 shl retry))
    }

    /** Woken by a push, or returning to the foreground. The push carried
     *  nothing, so the only correct reaction is to go and find out. */
    fun wake() {
        if (socket == null) connect() else run(core.wake())
        refresh()
    }

    // ---- nearby mesh ----

    private fun startMesh(nick: String) {
        if (mesh == null) {
            mesh = Mesh(ctx,
                onPeers = { json -> main.post { run(core.meshPeers(json)) } },
                onMsg = { json -> main.post { run(core.meshMsg(json, now())) } },
                onFrame = { json -> main.post { run(core.meshFrame(json, now())) } })
        }
        try { mesh?.start(nick) } catch (e: Exception) {
            run(core.notice("could not start nearby mode: ${e.message}", "error"))
        }
    }

    // ---- files and notifications ----

    /** A completed nearby transfer, saved to Downloads where the user can reach it. */
    private fun save(name: String, mime: String, bytes: ByteArray) {
        keepP2P(name, bytes)
        val ok = runCatching {
            val values = ContentValues().apply {
                put(MediaStore.Downloads.DISPLAY_NAME, name)
                put(MediaStore.Downloads.MIME_TYPE, mime.ifEmpty { "application/octet-stream" })
            }
            val uri = ctx.contentResolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)!!
            ctx.contentResolver.openOutputStream(uri)!!.use { it.write(bytes) }
        }.isSuccess
        run(core.notice(if (ok) "saved $name to Downloads" else "could not save $name", if (ok) "info" else "error"))
    }

    /** The core decided this is worth announcing. Here we decide whether the
     *  user can already see it: an alert for the channel on screen is noise. */
    private fun notify(a: Effect.Alert) {
        if (a.channel == snap.active && foreground) return
        val room = "#${channel(a.channel)?.name ?: ""}"
        val title = when {
            a.isDm -> a.nick
            a.event == "mention" -> "${a.nick} tagged you in $room"
            else -> "${a.nick} in $room"
        }
        val open = PendingIntent.getActivity(ctx, a.channel.hashCode(),
            Intent(ctx, MainActivity::class.java).putExtra("channel", a.channel)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        // A tag or a DM is addressed to you by name: it gets the channel that
        // breaks through, a loose highlight word does not.
        val urgent = a.isDm || a.event == "mention"
        val n = NotificationCompat.Builder(ctx, if (urgent) Push.CHANNEL_DIRECT else Push.CHANNEL_MESSAGES)
            .setSmallIcon(R.drawable.ic_hash)
            .setContentTitle(title).setContentText(a.text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(a.text))
            .setContentIntent(open).setAutoCancel(true)
            .build()
        runCatching {
            ctx.getSystemService(NotificationManager::class.java).notify(a.id, 0, n)
        }.onFailure { Log.w("gliph", "notify: $it") }
    }

    fun openFromNotification(channel: String) = setActive(channel)
}
