//  Peer-to-peer file transfer in DMs (spec F7), Android side: the Kotlin twin
//  of mobile/Sources/P2P.swift and web/src/p2p.ts, and it has to stay one: a
//  phone and a desktop are the two ends of the same data channel. The
//  handshake rides the chat socket as `p2p.*` frames (server/p2p.go relays them
//  inside a DM and nowhere else); the bytes go over a WebRTC data channel,
//  direct or through the server's TURN relay (`GET /api/ice`), never through
//  the server.
//
//  The wire, as p2p.ts speaks it:
//    sdp        JSON.stringify(RTCSessionDescription): {"type","sdp"}
//    ice        {candidate, mid}, trickled both ways
//    channel    one ordered data channel "gliph-file", 64 KB binary chunks;
//               the size in p2p.begin is the only end-of-file marker
//    verdict    the receiver sends the string "ok" or "bad" after hashing
//
//  ponytail: no resume and the whole file in memory at both ends, as on the
//  web and iOS. A dropped transfer fails loudly and is started again.
//  Everything here runs on the main thread; WebRTC's callbacks hop to it.

package chat.gliph.mobile

import android.content.Context
import android.os.Handler
import android.os.Looper
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import org.json.JSONObject
import org.webrtc.DataChannel
import org.webrtc.IceCandidate
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RtpReceiver
import org.webrtc.SdpObserver
import org.webrtc.SessionDescription
import java.nio.ByteBuffer
import java.security.MessageDigest
import java.util.UUID
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

private const val CHUNK = 64 * 1024
private const val HIGH_WATER = 1L shl 20
private const val STALL_MS = 30_000L
private const val ACCEPT_MS = 5 * 60_000L
/** What p2p.ts falls back on when the server configures no ICE servers. */
private val DEFAULT_ICE = listOf(PeerConnection.IceServer.builder("stun:stun.l.google.com:19302").createIceServer())

data class P2PInfo(
    val id: String, val channel: String,
    /** nick of the other side */
    val peer: String, val name: String, val size: Int, val send: Boolean,
)

/** Everything the host provides. P2P knows frames and files, not the UI. */
class P2PHost(
    val send: (JSONObject) -> Unit,
    /** Ask the human; nothing is received until this answers true. */
    val confirm: (P2PInfo, (Boolean) -> Unit) -> Unit,
    val ice: suspend () -> List<PeerConnection.IceServer>,
    val notice: (String, String) -> Unit,
    /** Checksum verified at the receiver. `bytes` is null on the sending side. */
    val done: (P2PInfo, ByteArray?) -> Unit,
)

private fun sha256(b: ByteArray) = MessageDigest.getInstance("SHA-256").digest(b).joinToString("") { "%02x".format(it) }

private class DMTransfer(val info: P2PInfo) {
    var pc: PeerConnection? = null
    var dc: DataChannel? = null
    var sha256 = ""
    /** The whole file when sending; what has arrived so far when receiving. */
    var bytes = java.io.ByteArrayOutputStream()
    var out = ByteArray(0)
    var offset = 0
    /** ICE that arrived before the remote description; adding it early fails. */
    val pendingIce = mutableListOf<IceCandidate>()
    var haveRemote = false
    var settled = false
    var stall: Runnable? = null
}

class P2P(ctx: Context, private val host: P2PHost) {
    private val main = Handler(Looper.getMainLooper())
    private val transfers = HashMap<String, DMTransfer>()
    private val factory: PeerConnectionFactory by lazy {
        PeerConnectionFactory.initialize(PeerConnectionFactory.InitializationOptions.builder(ctx).createInitializationOptions())
        PeerConnectionFactory.builder().createPeerConnectionFactory()
    }

    // ---- sending ----

    fun offer(channel: String, peer: String, name: String, bytes: ByteArray) {
        val info = P2PInfo(UUID.randomUUID().toString(), channel, peer, name, bytes.size, true)
        val t = DMTransfer(info)
        t.out = bytes
        t.sha256 = sha256(bytes)
        transfers[info.id] = t
        host.send(frame("p2p.begin", JSONObject().put("channel", channel).put("id", info.id).put("name", name)
            .put("size", bytes.size).put("sha256", t.sha256)))
        host.notice("offering $name directly to $peer…", "info")
        arm(t, ACCEPT_MS, "the other side never answered")
    }

    // ---- frames ----

    /** Every p2p.* frame from the server. A frame for a transfer this device
     *  is not driving is dropped: the server relays to all of a user's devices. */
    fun handle(type: String, d: JSONObject) {
        val id = d.optString("id").ifEmpty { return }
        val x = transfers[id]
        when (type) {
            "p2p.begin" -> if (x == null) receive(d)
            "p2p.accept" -> if (x != null && x.info.send && x.pc == null) Store.scope.launch { connect(x) }
            "p2p.reject" -> x?.let { fail(it, "the other side declined the transfer", notify = false) }
            "p2p.cancel" -> x?.let {
                val r = d.optString("reason")
                fail(it, if (r.isEmpty()) "the other side cancelled" else "the other side cancelled: $r", notify = false)
            }
            "p2p.offer" -> if (x != null && !x.info.send && !x.haveRemote) {
                val sdp = d.optString("sdp"); Store.scope.launch { answer(x, sdp) }
            }
            "p2p.answer" -> if (x != null && x.info.send) {
                val desc = decode(d.optString("sdp")) ?: return
                Store.scope.launch {
                    runCatching { setRemote(x.pc ?: return@launch, desc) }
                        .onSuccess { x.haveRemote = true; flushIce(x) }
                        .onFailure { fail(x, "bad answer from the other client: ${it.message}") }
                }
            }
            "p2p.ice" -> if (x != null) {
                val c = d.optString("candidate").ifEmpty { return }
                val cand = IceCandidate(d.optString("mid"), 0, c)
                val pc = x.pc
                if (x.haveRemote && pc != null) pc.addIceCandidate(cand) else x.pendingIce.add(cand)
            }
        }
    }

    // ---- the two halves of the handshake ----

    private fun receive(d: JSONObject) {
        val info = P2PInfo(d.optString("id"), d.optString("channel"), d.optString("from"),
            d.optString("name", "file"), d.optInt("size"), false)
        if (info.size <= 0) return
        val t = DMTransfer(info)
        t.sha256 = d.optString("sha256").lowercase()
        transfers[info.id] = t
        host.confirm(info) { ok ->
            if (transfers[info.id] !== t) return@confirm
            val ids = JSONObject().put("channel", info.channel).put("id", info.id)
            if (!ok) {
                host.send(frame("p2p.reject", ids.put("reason", "declined")))
                transfers.remove(info.id)
                return@confirm
            }
            host.send(frame("p2p.accept", ids))
            arm(t, STALL_MS, "the sender never connected")
        }
    }

    /** Sender, after the accept: open the channel and make the offer. */
    private suspend fun connect(t: DMTransfer) {
        clear(t)
        val pc = peer(t) ?: return fail(t, "could not start WebRTC")
        val dc = pc.createDataChannel("gliph-file", DataChannel.Init().apply { ordered = true })
            ?: return fail(t, "could not open a data channel")
        watch(t, dc)
        runCatching {
            val offer = sdp { pc.createOffer(it, MediaConstraints()) }
            setLocal(pc, offer)
            send(t, "p2p.offer", JSONObject().put("sdp", encode(offer)))
            arm(t, STALL_MS, "could not reach the other client")
        }.onFailure { fail(t, "could not make a transfer offer: ${it.message}") }
    }

    /** Receiver: answer the sender's offer; the channel arrives through onDataChannel. */
    private suspend fun answer(t: DMTransfer, sdp: String) {
        val desc = decode(sdp) ?: return fail(t, "bad offer from the other client")
        val pc = peer(t) ?: return fail(t, "could not start WebRTC")
        runCatching {
            setRemote(pc, desc)
            t.haveRemote = true
            flushIce(t)
            val ans = sdp { pc.createAnswer(it, MediaConstraints()) }
            setLocal(pc, ans)
            send(t, "p2p.answer", JSONObject().put("sdp", encode(ans)))
            arm(t, STALL_MS, "could not reach the sender")
        }.onFailure { fail(t, "could not answer the transfer offer: ${it.message}") }
    }

    private suspend fun peer(t: DMTransfer): PeerConnection? {
        val servers = host.ice().ifEmpty { DEFAULT_ICE }
        val cfg = PeerConnection.RTCConfiguration(servers).apply { sdpSemantics = PeerConnection.SdpSemantics.UNIFIED_PLAN }
        val pc = factory.createPeerConnection(cfg, object : PeerConnection.Observer {
            override fun onIceCandidate(c: IceCandidate) = on(t) {
                host.send(frame("p2p.ice", JSONObject().put("channel", t.info.channel).put("id", t.info.id)
                    .put("candidate", c.sdp).put("mid", c.sdpMid ?: "")))
            }
            override fun onDataChannel(dc: DataChannel) = on(t) { watch(t, dc); if (dc.state() == DataChannel.State.OPEN) opened(t) }
            override fun onConnectionChange(s: PeerConnection.PeerConnectionState) {
                // Usually both ends behind symmetric NAT and no TURN relay configured.
                if (s == PeerConnection.PeerConnectionState.FAILED) on(t) {
                    fail(t, "could not establish a peer-to-peer connection (no route, and no TURN relay configured)")
                }
            }
            override fun onSignalingChange(s: PeerConnection.SignalingState) {}
            override fun onIceConnectionChange(s: PeerConnection.IceConnectionState) {}
            override fun onIceConnectionReceivingChange(b: Boolean) {}
            override fun onIceGatheringChange(s: PeerConnection.IceGatheringState) {}
            override fun onIceCandidatesRemoved(c: Array<out IceCandidate>) {}
            override fun onAddStream(s: MediaStream) {}
            override fun onRemoveStream(s: MediaStream) {}
            override fun onRenegotiationNeeded() {}
            override fun onAddTrack(r: RtpReceiver, s: Array<out MediaStream>) {}
        })
        t.pc = pc
        return pc
    }

    private fun watch(t: DMTransfer, dc: DataChannel) {
        t.dc = dc
        dc.registerObserver(object : DataChannel.Observer {
            override fun onBufferedAmountChange(previous: Long) = on(t) { pump(t) }
            override fun onStateChange() {
                val s = dc.state()
                on(t) {
                    if (s == DataChannel.State.OPEN) opened(t)
                    if (s == DataChannel.State.CLOSED) fail(t, "the connection dropped before the file finished")
                }
            }
            override fun onMessage(buf: DataChannel.Buffer) {
                // The buffer is only valid during this call: copy it out first.
                val data = ByteArray(buf.data.remaining()).also { buf.data.get(it) }
                val binary = buf.binary
                on(t) { got(t, data, binary) }
            }
        })
    }

    private fun flushIce(t: DMTransfer) {
        val queued = t.pendingIce.toList()
        t.pendingIce.clear()
        queued.forEach { t.pc?.addIceCandidate(it) }
    }

    // ---- bytes ----

    private fun opened(t: DMTransfer) {
        clear(t)
        if (t.info.send) pump(t)
    }

    /** Fill the SCTP buffer to HIGH_WATER and stop; the buffered-amount
     *  callback calls this again as it drains. */
    private fun pump(t: DMTransfer) {
        val dc = t.dc ?: return
        if (!t.info.send || t.settled || dc.state() != DataChannel.State.OPEN) return
        while (t.offset < t.out.size && dc.bufferedAmount() < HIGH_WATER) {
            val end = minOf(t.offset + CHUNK, t.out.size)
            dc.send(DataChannel.Buffer(ByteBuffer.wrap(t.out, t.offset, end - t.offset), true))
            t.offset = end
        }
        // Not done until the receiver says the checksum matched.
        arm(t, STALL_MS, if (t.offset < t.out.size) "the transfer stalled" else "the receiver never confirmed the file")
    }

    private fun got(t: DMTransfer, data: ByteArray, binary: Boolean) {
        if (t.info.send) {
            if (binary) return
            if (String(data) == "ok") {
                t.settled = true
                host.done(t.info, null)
                close(t)
            } else fail(t, "the receiver's checksum did not match")
            return
        }
        if (!binary) return
        t.bytes.write(data)
        arm(t, STALL_MS, "the transfer stalled")
        if (t.bytes.size() < t.info.size) return
        clear(t)
        val all = t.bytes.toByteArray()
        val good = sha256(all) == t.sha256
        t.dc?.send(DataChannel.Buffer(ByteBuffer.wrap((if (good) "ok" else "bad").toByteArray()), false))
        if (good) {
            t.settled = true
            host.done(t.info, all)
            close(t)
        } else fail(t, "the file arrived corrupted (checksum mismatch), nothing was saved")
    }

    // ---- plumbing ----

    private fun send(t: DMTransfer, type: String, extra: JSONObject) =
        host.send(frame(type, extra.put("channel", t.info.channel).put("id", t.info.id)))

    private fun frame(type: String, d: JSONObject) = JSONObject().put("t", type).put("d", d)

    /** On the main thread, and only while the transfer is still alive. */
    private fun on(t: DMTransfer, f: () -> Unit) { main.post { if (!t.settled) f() } }

    /** The one way a transfer dies: always reported, never left hanging. */
    private fun fail(t: DMTransfer, why: String, notify: Boolean = true) {
        if (t.settled) return
        t.settled = true
        if (notify) send(t, "p2p.cancel", JSONObject().put("reason", why))
        host.notice("${t.info.name}: $why", "error")
        close(t)
    }

    private fun close(t: DMTransfer) {
        clear(t)
        t.dc?.unregisterObserver(); t.dc?.close(); t.dc?.dispose()
        t.pc?.dispose()
        t.dc = null; t.pc = null
        t.out = ByteArray(0); t.bytes = java.io.ByteArrayOutputStream()
        transfers.remove(t.info.id)
    }

    private fun arm(t: DMTransfer, ms: Long, why: String) {
        clear(t)
        val r = Runnable { fail(t, why) }
        t.stall = r
        main.postDelayed(r, ms)
    }

    private fun clear(t: DMTransfer) { t.stall?.let(main::removeCallbacks); t.stall = null }

    // ---- SDP, as the browser writes it ----

    private suspend fun sdp(make: (SdpObserver) -> Unit): SessionDescription = suspendCancellableCoroutine { k ->
        make(object : SdpObserver {
            override fun onCreateSuccess(d: SessionDescription) = k.resume(d)
            override fun onCreateFailure(e: String) = k.resumeWithException(Exception(e))
            override fun onSetSuccess() {}
            override fun onSetFailure(e: String) {}
        })
    }

    private suspend fun set(f: (SdpObserver) -> Unit): Unit = suspendCancellableCoroutine { k ->
        f(object : SdpObserver {
            override fun onCreateSuccess(d: SessionDescription) {}
            override fun onCreateFailure(e: String) {}
            override fun onSetSuccess() = k.resume(Unit)
            override fun onSetFailure(e: String) = k.resumeWithException(Exception(e))
        })
    }
    private suspend fun setLocal(pc: PeerConnection, d: SessionDescription) = set { pc.setLocalDescription(it, d) }
    private suspend fun setRemote(pc: PeerConnection, d: SessionDescription) = set { pc.setRemoteDescription(it, d) }

    /** JSON.stringify of the description, so p2p.ts can JSON.parse it straight
     *  into setRemoteDescription. */
    private fun encode(d: SessionDescription) =
        JSONObject().put("type", d.type.canonicalForm()).put("sdp", d.description).toString()

    private fun decode(s: String): SessionDescription? = runCatching {
        val o = JSONObject(s)
        SessionDescription(SessionDescription.Type.fromCanonicalForm(o.getString("type")), o.getString("sdp"))
    }.getOrNull()

    companion object {
        /** `GET /api/ice` → IceServer. `urls` is a list on the wire, but a
         *  browser accepts a bare string too, so this does. */
        fun iceServers(json: String): List<PeerConnection.IceServer> = runCatching {
            val list = JSONObject(json).optJSONArray("ice_servers") ?: return emptyList()
            (0 until list.length()).mapNotNull { i ->
                val s = list.getJSONObject(i)
                val urls = s.optJSONArray("urls")?.let { a -> (0 until a.length()).map { a.getString(it) } }
                    ?: s.optString("urls").takeIf { it.isNotEmpty() }?.let { listOf(it) } ?: emptyList()
                if (urls.isEmpty()) null else PeerConnection.IceServer.builder(urls)
                    .setUsername(s.optString("username")).setPassword(s.optString("credential")).createIceServer()
            }
        }.getOrDefault(emptyList())
    }
}
