//  Nearby mesh (spec F9), Android transport: mobile/Sources/Mesh.swift on
//  NsdManager and plain sockets.
//
//  The wire is the one desktop/src-tauri/src/mesh.rs speaks, so a Mac, an
//  iPhone and an Android phone on the same Wi-Fi are peers in ONE mesh:
//
//    discovery  DNS-SD `_gliph._tcp`, TXT keys id / nick / chans
//    links      TCP, newline-delimited JSON frames {"t":..,"d":{..}}
//    dialling   every peer dials every peer, and retries
//
//  Mesh knows nothing about the UI or the core. Everything that touches the
//  peer table runs on one thread, `exec`, as the Swift twin runs on its queue.
//
//  ponytail: no encryption on this hop and no store-and-forward, matching the
//  Rust side. Both are said to the user in the channel's topic.

package chat.gliph.mobile

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.net.wifi.WifiManager
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.io.BufferedReader
import java.io.InputStreamReader
import java.io.OutputStream
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.time.Instant
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread
import kotlin.random.Random

private const val SERVICE_TYPE = "_gliph._tcp"
/** Matches REBROWSE_TICKS in mesh.rs: both sides retry on the same cadence. */
private const val REDIAL_SECONDS = 8L

private class Link(val sock: Socket) {
    private val out: OutputStream = sock.getOutputStream()
    fun write(frame: JSONObject) {
        val bytes = (frame.toString() + "\n").toByteArray()
        runCatching { synchronized(this) { out.write(bytes); out.flush() } }
    }
    fun close() = runCatching { sock.close() }
}

private data class Peer(
    val id: String, var nick: String, var chans: List<String>, var link: Link? = null,
    /** Kept so a dial that failed can be tried again. */
    var host: InetAddress? = null, var port: Int = 0,
)

class Mesh(
    ctx: Context,
    private val onPeers: (String) -> Unit,
    private val onMsg: (String) -> Unit,
    private val onFrame: (String) -> Unit,
) {
    private val exec = Executors.newSingleThreadScheduledExecutor { Thread(it, "gliph-mesh") }
    private val nsd = ctx.getSystemService(NsdManager::class.java)
    // Some phones drop multicast, and with it every DNS-SD answer, unless an
    // app holds this lock.
    private val lock = ctx.applicationContext.getSystemService(WifiManager::class.java)
        .createMulticastLock("gliph-mesh").apply { setReferenceCounted(false) }

    private val id = randomID()
    private var nick = ""
    private var chans = emptyList<String>()
    private var server: ServerSocket? = null
    private var registration: NsdManager.RegistrationListener? = null
    private var discovery: NsdManager.DiscoveryListener? = null
    private var redial: ScheduledFuture<*>? = null
    private val peers = HashMap<String, Peer>()
    /** NsdManager resolves one service at a time; the rest wait here. */
    private val toResolve = ArrayDeque<NsdServiceInfo>()
    private var resolving = false

    fun start(nick: String) {
        if (server != null) return
        this.nick = nick
        val ss = ServerSocket(0)
        server = ss
        lock.acquire()
        thread(name = "gliph-mesh-accept", isDaemon = true) {
            while (!ss.isClosed) {
                val s = runCatching { ss.accept() }.getOrNull() ?: break
                exec.execute { adopt(s, null) }
            }
        }
        exec.execute {
            advertise()
            browse()
            redial = exec.scheduleWithFixedDelay({ redialAll() }, REDIAL_SECONDS, REDIAL_SECONDS, TimeUnit.SECONDS)
        }
    }

    fun stop() {
        exec.execute {
            redial?.cancel(false); redial = null
            registration?.let { runCatching { nsd.unregisterService(it) } }; registration = null
            discovery?.let { runCatching { nsd.stopServiceDiscovery(it) } }; discovery = null
            runCatching { server?.close() }; server = null
            peers.values.forEach { it.link?.close() }
            peers.clear(); toResolve.clear(); resolving = false
            if (lock.isHeld) lock.release()
            onPeers("[]")
        }
    }

    fun join(chans: List<String>) {
        exec.execute {
            this.chans = chans
            // Two ways a peer learns our channels: the TXT record for anyone not
            // linked yet (re-registered, DNS-SD has no update), the frame for
            // anyone who is.
            registration?.let { runCatching { nsd.unregisterService(it) } }
            advertise()
            broadcast(frame("chans", JSONObject().put("id", id).put("chans", JSONArray(chans))))
        }
    }

    /** An arbitrary frame for ONE peer: a transfer concerns two people. */
    fun sendTo(peer: String, t: String, json: String) {
        val d = runCatching { JSONObject(json) }.getOrDefault(JSONObject())
        exec.execute { peers[peer]?.link?.write(frame(t, d)) }
    }

    /** Returns the message as JSON so the core ingests our own line the same
     *  way it ingests a peer's. */
    fun say(chan: String, body: String, kind: String, replyTo: String): String {
        val msg = JSONObject()
            .put("id", "n_" + randomID()).put("chan", chan).put("nick", nick).put("body", body)
            .put("ts", Instant.now().toString()).put("kind", kind).put("reply_to", replyTo)
        exec.execute { broadcast(frame("msg", msg)) }
        return msg.toString()
    }

    // ---- discovery ----

    private fun advertise() {
        val ss = server ?: return
        val info = NsdServiceInfo().apply {
            serviceName = id
            serviceType = SERVICE_TYPE
            port = ss.localPort
            setAttribute("id", id)
            setAttribute("nick", nick)
            setAttribute("chans", chans.joinToString(","))
        }
        val l = object : NsdManager.RegistrationListener {
            override fun onServiceRegistered(i: NsdServiceInfo) {}
            override fun onRegistrationFailed(i: NsdServiceInfo, code: Int) { Log.w("gliph", "mesh register failed $code") }
            override fun onServiceUnregistered(i: NsdServiceInfo) {}
            override fun onUnregistrationFailed(i: NsdServiceInfo, code: Int) {}
        }
        registration = l
        nsd.registerService(info, NsdManager.PROTOCOL_DNS_SD, l)
    }

    private fun browse() {
        val l = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(type: String) {}
            override fun onDiscoveryStopped(type: String) {}
            override fun onStartDiscoveryFailed(type: String, code: Int) { Log.w("gliph", "mesh browse failed $code") }
            override fun onStopDiscoveryFailed(type: String, code: Int) {}
            override fun onServiceFound(info: NsdServiceInfo) {
                exec.execute {
                    if (info.serviceName == id) return@execute
                    toResolve.addLast(info); resolveNext()
                }
            }
            // A withdrawal is the "they left" signal only for a peer we are not
            // already talking to: the link closing is what proves they left.
            override fun onServiceLost(info: NsdServiceInfo) {
                exec.execute {
                    val p = peers[info.serviceName] ?: return@execute
                    if (p.link == null) { peers.remove(p.id); emitPeers() }
                }
            }
        }
        discovery = l
        nsd.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, l)
    }

    @Suppress("DEPRECATION") // resolveService still works; the callback API is 34+
    private fun resolveNext() {
        if (resolving) return
        val next = toResolve.removeFirstOrNull() ?: return
        resolving = true
        nsd.resolveService(next, object : NsdManager.ResolveListener {
            override fun onResolveFailed(i: NsdServiceInfo, code: Int) {
                exec.execute { resolving = false; resolveNext() }
            }
            override fun onServiceResolved(i: NsdServiceInfo) {
                exec.execute {
                    resolving = false
                    found(i)
                    resolveNext()
                }
            }
        })
    }

    private fun found(i: NsdServiceInfo) {
        val txt = i.attributes.mapValues { (_, v) -> v?.let { String(it) } ?: "" }
        val pid = txt["id"]?.takeIf { it.isNotEmpty() } ?: i.serviceName
        if (pid == id) return
        val p = peers.getOrPut(pid) { Peer(pid, pid, emptyList()) }
        p.nick = txt["nick"]?.ifEmpty { null } ?: pid
        p.chans = txt["chans"].orEmpty().split(',').filter { it.isNotEmpty() }
        @Suppress("DEPRECATION")
        p.host = i.host; p.port = i.port
        if (p.link == null) dial(p)
        emitPeers()
    }

    private fun redialAll() {
        for (p in peers.values) if (p.link == null && p.host != null) dial(p)
    }

    private fun dial(p: Peer) {
        val host = p.host ?: return
        val port = p.port
        thread(name = "gliph-mesh-dial", isDaemon = true) {
            val s = Socket()
            if (runCatching { s.connect(InetSocketAddress(host, port), 5000) }.isSuccess) {
                exec.execute { adopt(s, p.id) }
            } else runCatching { s.close() }
        }
    }

    // ---- links ----

    private fun adopt(s: Socket, expect: String?) {
        val link = Link(s)
        link.write(frame("hi", JSONObject().put("id", id).put("nick", nick).put("chans", JSONArray(chans))))
        if (expect != null) peers[expect]?.let { if (it.link == null) it.link = link else { link.close(); return } }
        thread(name = "gliph-mesh-read", isDaemon = true) {
            runCatching {
                BufferedReader(InputStreamReader(s.getInputStream())).use { r ->
                    while (true) {
                        val line = r.readLine() ?: break
                        exec.execute { handle(line, link) }
                    }
                }
            }
            exec.execute { drop(link) }
        }
    }

    private fun drop(link: Link) {
        link.close()
        for (p in peers.values) if (p.link === link) p.link = null
        emitPeers()
    }

    private fun handle(line: String, link: Link) {
        val obj = runCatching { JSONObject(line) }.getOrNull() ?: return
        val t = obj.optString("t").ifEmpty { return }
        val d = obj.optJSONObject("d") ?: JSONObject()
        when (t) {
            "hi" -> {
                val pid = d.optString("id").ifEmpty { return }
                if (pid == id) return
                val p = peers.getOrPut(pid) { Peer(pid, pid, emptyList()) }
                p.nick = d.optString("nick").ifEmpty { pid }
                p.chans = d.optJSONArray("chans").let { a -> (0 until (a?.length() ?: 0)).map { a!!.optString(it) } }
                if (p.link == null) p.link = link
                emitPeers()
            }
            "chans" -> {
                val p = peers[d.optString("id")] ?: return
                p.chans = d.optJSONArray("chans").let { a -> (0 until (a?.length() ?: 0)).map { a!!.optString(it) } }
                emitPeers()
            }
            "msg" -> onMsg(d.toString())
            else -> {
                // Not ours to understand. The core needs to know who sent it.
                val from = peers.values.firstOrNull { it.link === link }
                onFrame(JSONObject().put("from", from?.id ?: "").put("nick", from?.nick ?: "")
                    .put("t", t).put("d", d).toString())
            }
        }
    }

    private fun broadcast(f: JSONObject) { peers.values.forEach { it.link?.write(f) } }

    /** Only peers we hold a live connection to: a DNS-SD record outlives the
     *  device that published it; a TCP link does not. */
    private fun emitPeers() {
        val list = JSONArray()
        peers.values.filter { it.link != null }.forEach {
            list.put(JSONObject().put("id", it.id).put("nick", it.nick).put("chans", JSONArray(it.chans)))
        }
        onPeers(list.toString())
    }

    private fun frame(t: String, d: JSONObject) = JSONObject().put("t", t).put("d", d)

    companion object {
        fun randomID() = Random.nextBytes(6).joinToString("") { "%02x".format(it) }
    }
}
