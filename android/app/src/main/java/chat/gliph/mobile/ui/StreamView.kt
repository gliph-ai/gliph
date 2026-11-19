//  The scrollback (mobile/Sources/Views/StreamView.swift). One line per event,
//  IRC's shape: a timestamp, who, what. Everything is one monospace face on
//  purpose (spec F10/F12), so ASCII art lines up column for column.
//
//  The list is laid out bottom-up (reverseLayout), which is what keeps the
//  reader on the newest line as lines and pictures arrive without any of the
//  re-pinning the iOS view has to do.

package chat.gliph.mobile.ui

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.graphics.BitmapFactory
import android.util.Base64
import android.util.LruCache
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.tween
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.gestures.detectTransformGestures
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.res.imageResource
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.LinkAnnotation
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextLinkStyles
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withLink
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import chat.gliph.mobile.EventRow
import chat.gliph.mobile.R
import chat.gliph.mobile.Store
import chat.gliph.mobile.SysMsg
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale

private sealed interface Item {
    val key: String
    data class Day(val label: String) : Item { override val key get() = "day:$label" }
    data class Event(val e: EventRow) : Item { override val key get() = e.id }
    data class System(val m: SysMsg) : Item { override val key get() = "sys:${m.id}" }
    data class Line(val text: String, val error: Boolean, val n: Int) : Item { override val key get() = "line:$n" }
    data object Top : Item { override val key get() = "top" }
}

private val dayFmt = DateTimeFormatter.ofPattern("EEE MMM d yyyy", Locale.ENGLISH)
private val hhmmFmt = DateTimeFormatter.ofPattern("HH:mm")
private fun dayLabel(i: Instant) = dayFmt.format(i.atZone(ZoneId.systemDefault())).lowercase()
private fun hhmm(i: Instant) = hhmmFmt.format(i.atZone(ZoneId.systemDefault()))

/** Bytes as the stream says them, matching the web client exactly. */
fun sizeLabel(n: Long) = if (n > 1 shl 20) "%.1f MB".format(n / 1048576.0) else "${(n + 1023) / 1024} KB"

/** The web client's list, minus SVG: it is a document that can carry script. */
private fun isImageName(name: String) =
    name.substringAfterLast('.', "").lowercase() in setOf("png", "jpg", "jpeg", "gif", "webp", "avif", "bmp", "ico")

/** Kinds worth a long-press. Nobody replies to a join line. */
private val actionable = setOf("message", "action", "agent", "file")

@Composable
fun StreamView() {
    val rows = Store.rows
    val snap = Store.snap
    val nearby = Store.channel()?.nearby ?: false
    val list = rememberLazyListState()
    val scope = rememberCoroutineScope()
    var flashed by remember { mutableStateOf("") }
    val flash = remember { Animatable(0f) }

    // Oldest first, then reversed for the bottom-up list.
    val items = buildList {
        add(Item.Top)
        var day = ""
        for (e in rows) {
            val d = dayLabel(e.at)
            if (d != day) { day = d; add(Item.Day(d)) }
            add(Item.Event(e))
        }
        // At the live end, under the newest line: the MOTD and any release
        // notes this phone has not read.
        snap.system.forEach { add(Item.System(it)) }
        val lines = snap.notices.takeLast(8).map { it.text to (it.level == "error") } +
            Store.saving.entries.sortedBy { it.key }.map { "receiving ${it.key}: ${it.value}%" to false }
        lines.forEachIndexed { i, (s, err) -> add(Item.Line(s, err, i)) }
        // A nearby channel does not care that the server is down.
        if (!snap.online && !snap.solo && !nearby) {
            add(Item.Line("*** disconnected: messages you send will queue until the server is back", true, 99))
        }
    }.asReversed()
    val onScreen = remember(rows) { rows.mapTo(HashSet()) { it.id } }

    // A new stream starts at its newest line.
    LaunchedEffect(snap.active) { list.scrollToItem(0) }
    // A lazy list keeps the line on screen anchored by key, so lines arriving
    // at the live end would land below the fold. Whether the reader is at the
    // newest line is decided only when THEY scroll; while they are, every new
    // line keeps them there, and a reader up in history is left alone.
    var pinned by remember { mutableStateOf(true) }
    LaunchedEffect(list) {
        snapshotFlow { list.isScrollInProgress }.collect { moving ->
            if (!moving) pinned = list.firstVisibleItemIndex == 0
        }
    }
    LaunchedEffect(items.firstOrNull()?.key, items.size) { if (pinned) list.scrollToItem(0) }

    fun jump(id: String) {
        val i = items.indexOfFirst { it.key == id }
        if (i < 0) return
        scope.launch { list.animateScrollToItem(i) }
        // Lit at once, faded out afterwards: a flash that eases IN is a reveal.
        flashed = id
        scope.launch {
            flash.snapTo(1f); delay(200); flash.animateTo(0f, tween(1200)); flashed = ""
        }
    }

    LazyColumn(Modifier.fillMaxSize(), state = list, reverseLayout = true,
        verticalArrangement = Arrangement.spacedBy(3.dp)) {
        items(items.size, key = { items[it].key }) { i ->
            when (val it = items[i]) {
                Item.Top -> Splash()
                is Item.Day -> Text("-- ${it.label} --", style = Face.mono(11f), color = t.muted,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp))
                is Item.Event -> EventLine(it.e, onScreen, if (it.e.id == flashed) flash.value else 0f, ::jump)
                is Item.System -> SystemCard(it.m)
                is Item.Line -> Text(it.text, style = Face.mono(12f), color = if (it.error) t.accent else t.muted,
                    modifier = Modifier.padding(horizontal = 12.dp))
            }
        }
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun EventLine(e: EventRow, onScreen: Set<String>, lit: Float, jump: (String) -> Unit) {
    var menu by remember { mutableStateOf(false) }
    val ctx = LocalContext.current
    val canAct = e.kind in actionable && !e.locked
    val bg = when {
        lit > 0f -> t.gold.copy(alpha = 0.45f * lit)
        e.highlight -> t.gold.copy(alpha = 0.14f)
        else -> Color.Transparent
    }
    Box {
        Column(Modifier.fillMaxWidth().background(bg)
            .combinedClickable(onClick = {}, onLongClick = { if (canAct) menu = true }, indication = null,
                interactionSource = null)
            .padding(horizontal = 12.dp).alpha(if (e.pending) 0.55f else 1f)) {
            // A reply puts the clock on the quote line, so the pair reads as one
            // block with one timestamp.
            if (e.quote.isNotEmpty()) {
                val target = e.replyTo.takeIf { it in onScreen } ?: ""
                Text(buildAnnotatedString {
                    append("[${hhmm(e.at)}] ")
                    if (target.isNotEmpty()) withStyle(SpanStyle(textDecoration = TextDecoration.Underline)) { append(e.quote) }
                    else append(e.quote)
                }, style = Face.mono(11.5f), color = t.muted, maxLines = 1, overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.fillMaxWidth().clickable(enabled = target.isNotEmpty(), indication = null,
                        interactionSource = null) { jump(target) })
            }
            Row {
                if (e.quote.isNotEmpty()) {
                    Text(">", style = Face.mono(12.5f), color = t.border, modifier = Modifier.padding(end = 8.dp))
                }
                Inner(e)
            }
        }
        DropdownMenu(menu, { menu = false }, containerColor = t.surface2) {
            DropdownMenuItem({ Text("reply", style = Face.mono(13f), color = t.text) },
                { menu = false; Store.setReply(e.id) })
            DropdownMenuItem({ Text("copy text", style = Face.mono(13f), color = t.text) }, {
                menu = false
                (ctx.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager)
                    .setPrimaryClip(ClipData.newPlainText("gliph", e.readable))
            })
        }
    }
}

/** Where an image line's picture comes from: a file id on the server, or the
 *  copy this device holds for a file that went peer to peer. */
private fun attachmentID(e: EventRow): String? {
    if (!e.metaB("p2p")) return e.metaS("file_id").ifEmpty { null }
    val key = "${e.body}|${e.metaL("size")}"
    return if (Store.p2pBytes(key) == null) null else "p2p:$key"
}

@Composable
private fun Inner(e: EventRow) {
    val time = if (e.quote.isEmpty()) "[${hhmm(e.at)}] " else ""
    val style = Face.mono(13f)
    val th = t
    when {
        e.kind == "message" && !e.locked && e.readable.contains("```") -> {
            // A fenced block cannot live inside one paragraph: it is its own
            // view, folded past seven lines like the web client.
            Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                fences(e.readable).forEachIndexed { i, part ->
                    when (part) {
                        is Part.Prose -> if (i == 0) Text(line(e, time) { inline(part.text, th) }, style = style)
                            else if (part.text.isNotEmpty()) Text(buildAnnotatedString { inline(part.text, th) }, style = style)
                        is Part.Code -> {
                            if (i == 0) Text(line(e, time) {}, style = style)
                            CodeBlock(part.lang, part.code)
                        }
                    }
                }
            }
        }
        e.kind == "file" && isImageName(e.body) && attachmentID(e) != null -> Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(content(e, time), style = style)
            Attachment(attachmentID(e)!!, e.body)
        }
        else -> Text(content(e, time), style = style)
    }
}

@Composable
private fun line(e: EventRow, time: String, body: AnnotatedString.Builder.() -> Unit): AnnotatedString {
    val th = t
    return buildAnnotatedString {
        withStyle(SpanStyle(color = th.muted, fontSize = 11.5.sp)) { append(time) }
        withStyle(SpanStyle(color = th.nick(e.actor), fontWeight = FontWeight.Bold)) { append("<${e.actor}> ") }
        body()
    }
}

/** mIRC's control codes (bold, colour, reset, reverse, italic, underline) are
 *  dropped rather than drawn. */
private val ircCodes = Regex("\u0003\\d{0,2}(,\\d{1,2})?|[\u0002\u000f\u0016\u001d\u001f]")
private val urlRe = Regex("https?://[^\\s<>\"]+[^\\s<>\".,;:!?)\\]]")

/** `like this` reads as code: a chip in the other theme's surface, as on web. */
private fun AnnotatedString.Builder.inline(raw: String, theme: Theme) {
    val s = raw.replace(ircCodes, "")
    val other = if (theme.isDark) Theme.creme else Theme.midnight
    var rest = s
    while (true) {
        val open = rest.indexOf('`')
        if (open < 0) break
        val close = rest.indexOf('`', open + 1)
        if (close <= open + 1 || rest.substring(open + 1, close).contains('\n')) {
            prose(rest.substring(0, open + 1), theme); rest = rest.substring(open + 1); continue
        }
        prose(rest.substring(0, open), theme)
        withStyle(SpanStyle(color = other.text, background = other.surface2, fontWeight = FontWeight.Bold, fontSize = 12.5.sp)) {
            append(rest.substring(open + 1, close))
        }
        rest = rest.substring(close + 1)
    }
    prose(rest, theme)
}

/** Plain text with @nick runs (tap opens the DM) and links made tappable. */
private fun AnnotatedString.Builder.prose(s: String, theme: Theme) {
    var i = 0
    val mention = Regex("(?<=^|[\\s(])@([\\p{L}\\p{N}_-]+)")
    val hits = (mention.findAll(s).map { it to true } + urlRe.findAll(s).map { it to false })
        .sortedBy { it.first.range.first }
    for ((m, isNick) in hits) {
        if (m.range.first < i) continue
        withStyle(SpanStyle(color = theme.text)) { append(s.substring(i, m.range.first)) }
        val link = if (isNick) {
            val nick = m.groupValues[1]
            LinkAnnotation.Clickable("dm:$nick", TextLinkStyles(SpanStyle(color = theme.link,
                textDecoration = TextDecoration.Underline))) { Store.openDm(nick) }
        } else LinkAnnotation.Url(m.value, TextLinkStyles(SpanStyle(color = theme.link, textDecoration = TextDecoration.Underline)))
        withLink(link) { append(m.value) }
        i = m.range.last + 1
    }
    withStyle(SpanStyle(color = theme.text)) { append(s.substring(i)) }
}

/** One paragraph per event: the timestamp, the nick and the body wrap as one,
 *  which is the difference between a chat log and a table. */
@Composable
private fun content(e: EventRow, time: String): AnnotatedString {
    val th = t
    return buildAnnotatedString {
        withStyle(SpanStyle(color = th.muted, fontSize = 11.5.sp)) { append(time) }
        fun nick(n: String, c: Color = th.nick(n)) = withStyle(SpanStyle(color = c, fontWeight = FontWeight.Bold)) { append("<$n> ") }
        fun sys(s: String, c: Color) = withStyle(SpanStyle(color = c, fontSize = 12.sp)) { append(s) }
        // Said in the 3D room: the line carries the room it was said in.
        fun mark() { if (e.room.isNotEmpty()) withStyle(SpanStyle(color = th.r0From, fontWeight = FontWeight.Bold, fontSize = 12.sp)) { append("[${e.room}] ") } }
        when (e.kind) {
            "message" -> {
                mark(); nick(e.actor)
                if (e.locked) withStyle(SpanStyle(color = th.muted, fontStyle = FontStyle.Italic)) { append(e.readable) }
                else inline(e.readable, th)
            }
            "action" -> { mark(); sys("* ${e.actor} ${e.readable}", th.action) }
            "room_invite" -> {
                // room0 runs on web and desktop, so the phone reads the ask and
                // says where to answer it.
                val who = e.metaS("target")
                val target = e.metaS("target_nick").ifEmpty { "someone" }
                sys(when {
                    who.isEmpty() -> "*** ${e.actor} opened room0 · ${e.body}"
                    who == Store.snap.user?.id -> "*** ${e.actor} asked you into room0 · ${e.body}"
                    else -> "*** ${e.actor} asked $target into room0 · ${e.body}"
                }, th.r0From)
                sys("  open on desktop", th.muted)
            }
            "agent" -> {
                nick("◆${e.actor}", th.agent)
                withStyle(SpanStyle(color = th.agent, fontWeight = FontWeight.Bold, fontSize = 11.sp)) { append("[AGENT] ") }
                withStyle(SpanStyle(color = th.text)) { append(e.body) }
            }
            "join" -> sys("*** ${e.actor} has joined" + (if (e.body.isEmpty()) "" else ": ${e.body}"), th.join)
            "part" -> sys("*** ${e.actor} has left", th.join)
            "mode" -> sys("*** ${e.actor} ${e.body}", th.mode)
            "topic" -> sys("*** ${e.actor} set the topic: ${e.body}", th.mode)
            "kick" -> sys("*** ${e.metaS("target")} was kicked by ${e.actor} (${e.body})", th.accent)
            "notice" -> sys(if (e.actor == "*") e.body else "*** ${e.actor} ${e.body}", th.muted)
            "file" -> {
                withStyle(SpanStyle(color = th.muted)) { append("@ ") }
                val mb = sizeLabel(e.metaL("size"))
                if (e.metaB("p2p")) {
                    // A nearby transfer went peer to peer: only the two ends
                    // have this line, and no server holds the bytes.
                    val peer = e.metaS("peer")
                    if (e.metaS("dir") == "send") {
                        withStyle(SpanStyle(color = th.text)) { append("you sent $peer a file: ") }; nick(e.body, th.text)
                    } else {
                        nick(e.actor); withStyle(SpanStyle(color = th.text)) { append("sent you a file: ") }; nick(e.body, th.text)
                    }
                    withStyle(SpanStyle(color = th.muted)) { append("($mb · direct)") }
                } else {
                    nick(e.actor)
                    withStyle(SpanStyle(color = th.text)) { append("sent a file: ") }
                    val id = e.metaS("file_id")
                    withLink(LinkAnnotation.Clickable("file:$id", TextLinkStyles(SpanStyle(color = th.link,
                        textDecoration = TextDecoration.Underline))) { Store.saveFile(id, e.body) }) { append(e.body) }
                }
            }
            else -> withStyle(SpanStyle(color = if (e.locked) th.muted else th.text)) { append(e.readable) }
        }
    }
}

private sealed interface Part {
    data class Prose(val text: String) : Part
    data class Code(val lang: String, val code: String) : Part
}

/** Split a body on ``` fences. Even pieces are prose, odd ones code whose
 *  first line names the language. An unclosed fence is just text. */
private fun fences(body: String): List<Part> {
    val pieces = body.split("```")
    if (pieces.size < 3) return listOf(Part.Prose(body))
    return pieces.mapIndexedNotNull { i, piece ->
        when {
            i % 2 == 0 -> piece.trim('\n').let { if (it.isNotEmpty() || i == 0) Part.Prose(it) else null }
            i < pieces.size - 1 || pieces.size % 2 == 1 -> {
                val nl = piece.indexOf('\n')
                Part.Code(if (nl < 0) "" else piece.substring(0, nl), if (nl < 0) "" else piece.substring(nl + 1).trim('\n'))
            }
            else -> Part.Prose("```$piece")
        }
    }
}

/** A fenced block: language label, the code in mono, seven lines then a fold.
 *  Always dark, both themes, like the web client. */
@Composable
private fun CodeBlock(lang: String, code: String) {
    var open by remember { mutableStateOf(false) }
    val lines = code.split('\n').size
    val wraps = lang.lowercase() in setOf("", "md", "markdown", "txt", "text", "plaintext", "plain")
    val m = Theme.midnight
    Column(Modifier.fillMaxWidth().background(m.surface).padding(horizontal = 10.dp, vertical = 6.dp)) {
        Row {
            Text(lang.ifEmpty { "code" }.uppercase(), style = Face.mono(10.5f, FontWeight.Bold), color = m.agent)
            Spacer(Modifier.weight(1f))
            if (lines > 7) Text(if (open) "collapse" else "expand · $lines lines", style = Face.mono(10.5f, FontWeight.Bold),
                color = m.join, modifier = Modifier.clickable { open = !open })
        }
        val max = if (open || lines <= 7) Int.MAX_VALUE else 7
        // Prose fences wrap like text; code scrolls sideways.
        if (wraps) Text(code, style = Face.mono(12f), color = m.text, maxLines = max)
        else Text(code, style = Face.mono(12f), color = m.text, maxLines = max, softWrap = false,
            modifier = Modifier.horizontalScroll(rememberScrollState()))
    }
}

/** Pictures already decoded this session, the room the reader gave them, and
 *  the ones that could not be had. A lazy list throws a row away the moment it
 *  scrolls off, so without these scrolling back re-downloads every picture. */
private object Pictures {
    val cache = LruCache<String, ImageBitmap>(48)
    val heights = HashMap<String, Float>()
    val dead = HashSet<String>()
}

/** An inline image preview, fetched with the token the server wants. */
@Composable
private fun Attachment(id: String, name: String) {
    val cfg = LocalConfiguration.current
    val base = cfg.screenHeightDp / 2f
    val step = cfg.screenHeightDp / 4f
    var image by remember(id) { mutableStateOf(Pictures.cache.get(id)) }
    var failed by remember(id) { mutableStateOf(id in Pictures.dead) }
    var shown by remember(id) { mutableStateOf(Pictures.heights[id]) }
    var open by remember { mutableStateOf(false) }

    LaunchedEffect(id) {
        if (image != null || failed) return@LaunchedEffect
        val bytes = if (id.startsWith("p2p:")) Store.p2pBytes(id.removePrefix("p2p:")) else Store.fileBytes(id)
        val bmp = bytes?.let { BitmapFactory.decodeByteArray(it, 0, it.size) }?.asImageBitmap()
        if (bmp == null) {
            // Only a failure with the server on the line says the file is gone.
            if (Store.snap.online) Pictures.dead.add(id)
            failed = true
        } else {
            Pictures.cache.put(id, bmp); image = bmp
        }
    }

    val img = image
    when {
        img != null -> {
            val full = (cfg.screenWidthDp - 24f) * img.height / maxOf(img.width, 1)
            val cap = minOf(shown ?: base, full)
            fun size(h: Float) { shown = h; Pictures.heights[id] = h }
            Box(Modifier.fillMaxWidth().height(cap.dp).clipToBounds().border(1.dp, t.border).clickable { open = true }) {
                Image(img, name, contentScale = ContentScale.FillWidth, alignment = Alignment.TopCenter,
                    modifier = Modifier.fillMaxWidth())
                if (full > cap + 1 || shown != null) {
                    Row(Modifier.align(Alignment.TopEnd).background(Color.Black.copy(alpha = 0.55f))
                        .padding(horizontal = 5.dp, vertical = 3.dp)) {
                        Text("[ - ]", style = Face.mono(11f, FontWeight.Bold), color = Color.White,
                            modifier = Modifier.clickable { size(maxOf(120f, cap - step)) })
                        Spacer(Modifier.width(2.dp))
                        Text("[ + ]", style = Face.mono(11f, FontWeight.Bold), color = Color.White,
                            modifier = Modifier.clickable { size(minOf(full, cap + step)) })
                    }
                }
            }
            if (open) Lightbox(img, name, id) { open = false }
        }
        // The bytes are gone: one line saying so, not half a screen of nothing.
        failed -> Text("[ preview unavailable ]", style = Face.mono(12f), color = t.muted)
        // Hold half a screen while the bytes are on their way, so the stream
        // does not lurch when the picture lands.
        else -> Box(Modifier.fillMaxWidth().height(base.dp).background(t.surface2).border(1.dp, t.border))
    }
}

/** The whole picture: pinch to zoom, save hands it to the share sheet. Black
 *  either theme: a photo viewer is not a chat surface. */
@Composable
private fun Lightbox(img: ImageBitmap, name: String, id: String, close: () -> Unit) {
    var scale by remember { mutableStateOf(1f) }
    Dialog(close, DialogProperties(usePlatformDefaultWidth = false)) {
        Box(Modifier.fillMaxSize().background(Color.Black)) {
            Image(img, name, contentScale = ContentScale.Fit, modifier = Modifier.fillMaxSize()
                .pointerInput(Unit) { detectTransformGestures { _, _, zoom, _ -> scale = (scale * zoom).coerceIn(1f, 5f) } }
                .graphicsLayer { scaleX = scale; scaleY = scale })
            Row(Modifier.fillMaxWidth().safeDrawingPadding().background(Color.Black.copy(alpha = 0.6f))
                .padding(horizontal = 14.dp, vertical = 10.dp)) {
                Text(name, style = Face.mono(12f, FontWeight.Bold), color = Color.White, maxLines = 1,
                    overflow = TextOverflow.MiddleEllipsis, modifier = Modifier.weight(1f))
                Text("save", style = Face.mono(12f, FontWeight.Bold), color = Color.White,
                    modifier = Modifier.clickable { Store.saveFile(id, name) }.padding(horizontal = 8.dp))
                Text("done", style = Face.mono(12f, FontWeight.Bold), color = Color.White,
                    modifier = Modifier.clickable(onClick = close))
            }
        }
    }
}

/** A system message: the MOTD, or the notes for a release. The one card in the
 *  stream that is not chat, and it says so by leaving the monospace column.
 *  Three lines until the reader asks for the rest. */
@Composable
private fun SystemCard(msg: SysMsg) {
    var open by remember { mutableStateOf(false) }
    var clipped by remember { mutableStateOf(false) }
    // Inline bytes or ours, never a URL: fetching a remote image to draw a
    // line of chat tells its owner who read the channel and when.
    val icon = remember(msg.icon) {
        if (msg.kind != "motd") null else msg.icon.substringAfter("base64,", "").takeIf { it.isNotEmpty() }
            ?.let { runCatching { Base64.decode(it, Base64.DEFAULT) }.getOrNull() }
            ?.let { BitmapFactory.decodeByteArray(it, 0, it.size)?.asImageBitmap() }
    }
    val unicorn = ImageBitmap.imageResource(R.drawable.unicorn)
    val date = runCatching {
        LocalDate.parse(msg.date.take(10)).format(DateTimeFormatter.ofPattern("d MMMM", Locale.ENGLISH))
    }.getOrDefault("")
    Row(Modifier.padding(horizontal = 10.dp, vertical = 4.dp).fillMaxWidth().background(t.surface).padding(12.dp)) {
        Image(icon ?: unicorn, null, Modifier.size(30.dp).border(1.dp, t.border2))
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(if (msg.kind == "motd") "MESSAGE OF THE DAY" else "WHAT'S NEW",
                    style = Face.sans(10.5f, FontWeight.Bold).copy(letterSpacing = 0.8.sp), color = t.accent)
                if (date.isNotEmpty()) {
                    Spacer(Modifier.width(8.dp)); Text(date, style = Face.sans(11f), color = t.muted)
                }
            }
            if (msg.title.isNotEmpty()) Text(msg.title, style = Face.sans(15f, FontWeight.SemiBold), color = t.text)
            Text(msg.body, style = Face.sans(13.5f), color = t.text2, maxLines = if (open) Int.MAX_VALUE else 3,
                overflow = TextOverflow.Ellipsis, onTextLayout = { if (!open) clipped = it.hasVisualOverflow })
            // Measured, which Compose makes free, rather than guessed from length.
            if (clipped || open) Text(if (open) "show less" else "read more", style = Face.sans(12.5f, FontWeight.SemiBold),
                color = t.link, modifier = Modifier.padding(top = 2.dp).clickable { open = !open })
        }
        // Only the MOTD closes. A release note lives in #system, and a log you
        // can empty is not a log.
        if (msg.kind == "motd") Text("×", style = Face.sans(17f), color = t.muted,
            modifier = Modifier.clickable { Store.dismissSystem(msg.id) }.padding(start = 6.dp))
    }
}
