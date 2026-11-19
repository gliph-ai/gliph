//  The chat screen (mobile/Sources/Views/ChatView.swift): chrome at the top,
//  scrollback in the middle, composer at the bottom, status line under that.
//  The rail is a drawer that slides in from the left, over the conversation
//  rather than instead of it.

package chat.gliph.mobile.ui

import android.content.ClipboardManager
import android.content.Context
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBars
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.ModalDrawerSheet
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberDrawerState
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.ui.platform.LocalView
import androidx.core.view.WindowCompat
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.isShiftPressed
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import chat.gliph.mobile.R
import chat.gliph.mobile.Store
import kotlinx.coroutines.launch

@Composable
fun Root() {
    val theme = Store.theme
    // Light icons on the navy chrome; dark ones on the creme sign-in screen.
    val view = LocalView.current
    val lightBars = !Store.signedIn && !theme.isDark
    SideEffect {
        (view.context as? android.app.Activity)?.window?.let {
            WindowCompat.getInsetsController(it, view).isAppearanceLightStatusBars = lightBars
            WindowCompat.getInsetsController(it, view).isAppearanceLightNavigationBars = lightBars
        }
    }
    CompositionLocalProvider(LocalTheme provides theme) {
        Box(Modifier.fillMaxSize().background(theme.surface)) {
            if (Store.signedIn) ChatScreen() else AuthScreen()
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ChatScreen() {
    val drawer = rememberDrawerState(DrawerValue.Closed)
    val scope = rememberCoroutineScope()
    var sheet by remember { mutableStateOf("") } // "", settings, transfers, servers
    val snap = Store.snap

    ModalNavigationDrawer(
        drawerState = drawer,
        scrimColor = t.scrim,
        drawerContent = {
            ModalDrawerSheet(Modifier.width(300.dp), drawerContainerColor = t.rail,
                drawerShape = androidx.compose.ui.graphics.RectangleShape) {
                RailView(onPick = { scope.launch { drawer.close() } })
            }
        },
    ) {
        Column(Modifier.fillMaxSize().background(t.surface)) {
            Menubar(
                onRail = { scope.launch { if (drawer.isOpen) drawer.close() else drawer.open() } },
                onSheet = { sheet = it },
            )
            TitleStrip()
            Column(Modifier.weight(1f).imePadding()) {
                BlobSwitcher()
                DeviceBar()
                if (snap.activeBlob.isEmpty()) {
                    Box(Modifier.weight(1f)) { StreamView() }
                    Conversation()
                } else {
                    // A blob replaces the stream and the composer: it has its
                    // own focus model.
                    Box(Modifier.weight(1f)) { BlobView() }
                }
                StatusBar()
            }
        }
    }

    if (sheet.isNotEmpty()) {
        ModalBottomSheet(
            onDismissRequest = { sheet = "" },
            sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true),
            containerColor = t.surface,
            shape = androidx.compose.ui.graphics.RectangleShape,
            dragHandle = null,
        ) {
            when (sheet) {
                "settings" -> SettingsView(close = { sheet = "" })
                "transfers" -> TransfersView(close = { sheet = "" })
                "servers" -> ServersView(close = { sheet = "" })
            }
        }
    }

    AskDialog()
    ApproveDialog()
    ComposeEditor()
}

/** Device approval: this device waiting, or another one waiting on it. At
 *  most a few lines, scrolled past that, so the composer stays reachable. */
@Composable
private fun DeviceBar() {
    val d = Store.snap.device
    if (!d.known || Store.snap.solo) return
    val lines = if (!d.signed) {
        listOf("this device is not approved yet, so it cannot read encrypted channels. scan this with your phone's camera, or on a device you already use, type /approve ${d.code}")
    } else d.pending.map { (name, code) ->
        "${name.ifEmpty { "a new device" }} wants to read your encrypted channels. if it shows $code, type /approve $code"
    }
    if (lines.isEmpty()) return
    Column(Modifier.fillMaxWidth().heightIn(max = 260.dp).background(t.surface).verticalScroll(rememberScrollState())) {
        if (!d.signed && d.qr.isNotEmpty()) QRCode(d.qr, Modifier.padding(start = 12.dp, top = 8.dp).size(132.dp))
        lines.forEach {
            Text(it, style = Face.mono(11.5f), color = t.text2, modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp))
        }
        Box(Modifier.fillMaxWidth().height(1.dp).background(t.border))
    }
}

/** The multi-line editor the core asks for: a theme file, a script. Save
 *  submits the core's command with the text under it. */
@Composable
private fun ComposeEditor() {
    val c = Store.compose ?: return
    var text by remember(c) { mutableStateOf(c.text) }
    androidx.compose.ui.window.Dialog({ Store.compose = null },
        androidx.compose.ui.window.DialogProperties(usePlatformDefaultWidth = false)) {
        Column(Modifier.fillMaxSize().background(t.surface).windowInsetsPadding(WindowInsets.statusBars).imePadding()) {
            Row(Modifier.fillMaxWidth().background(t.chrome).padding(horizontal = 16.dp, vertical = 12.dp),
                verticalAlignment = Alignment.CenterVertically) {
                Text("cancel", style = Face.mono(12f), color = t.chromeDim, modifier = Modifier.clickable { Store.compose = null })
                Text(c.title, style = Face.mono(13f, FontWeight.Bold), color = t.chromeText, maxLines = 1,
                    modifier = Modifier.weight(1f).padding(horizontal = 12.dp))
                Text("save", style = Face.mono(12f, FontWeight.Bold), color = t.accent, modifier = Modifier.clickable {
                    Store.compose = null
                    Store.submit(c.submit + "\n" + text)
                })
            }
            BasicTextField(text, { text = it }, textStyle = Face.mono(12f).copy(color = t.text),
                cursorBrush = SolidColor(t.accent),
                keyboardOptions = KeyboardOptions(autoCorrectEnabled = false),
                modifier = Modifier.fillMaxSize().padding(12.dp))
        }
    }
}

/** The core's QR rows, dark on white with a 4-module quiet zone. */
@Composable
private fun QRCode(rows: List<String>, modifier: Modifier) {
    androidx.compose.foundation.Canvas(modifier.semantics { contentDescription = "approval QR code" }) {
        val m = size.width / (rows.size + 8)
        drawRect(androidx.compose.ui.graphics.Color.White)
        rows.forEachIndexed { y, row ->
            row.forEachIndexed { x, c ->
                if (c == '1') drawRect(androidx.compose.ui.graphics.Color.Black,
                    androidx.compose.ui.geometry.Offset((x + 4) * m, (y + 4) * m),
                    androidx.compose.ui.geometry.Size(m + 0.5f, m + 0.5f))
            }
        }
    }
}

/** A gliph://approve/<code> link, scanned off a new device: confirm which
 *  device before running /approve. The core checks the code itself. */
@Composable
private fun ApproveDialog() {
    val code = Store.approveCode ?: return
    val d = Store.snap.device
    if (!d.known) return // still connecting: ask once the devices are in
    // Approve only a device of this account waiting on the server we are on now.
    val waiting = d.signed && d.pending.any { it.second == code }
    val name = d.pending.firstOrNull { it.second == code }?.first.orEmpty().ifEmpty { "the new device" }
    AlertDialog(
        onDismissRequest = { Store.approveCode = null },
        containerColor = t.surface,
        shape = androidx.compose.ui.graphics.RectangleShape,
        text = {
            Text(when {
                !d.signed -> "this device is not approved itself, so it cannot approve another."
                !waiting -> "no device of yours on ${Store.snap.serverName} is waiting with $code. switch to the server the new device signed in to, then scan again."
                else -> "approve $name? it can then read your encrypted channels. approve it only if it is yours and shows $code."
            }, style = Face.mono(13f), color = t.text)
        },
        confirmButton = {
            if (waiting) TextButton({ Store.approveCode = null; Store.submit("/approve $code") }) {
                Text("approve", style = Face.mono(13f, FontWeight.Bold), color = t.accent)
            }
        },
        dismissButton = {
            TextButton({ Store.approveCode = null }) {
                Text(if (waiting) "cancel" else "ok", style = Face.mono(13f), color = t.muted)
            }
        },
    )
}

/** The core asks; the platform does the asking. A cancel is an answer too: it
 *  is what stops a confirm turning into a delete. */
@Composable
private fun AskDialog() {
    val ask = Store.ask ?: return
    var text by remember(ask) { mutableStateOf("") }
    AlertDialog(
        onDismissRequest = { Store.answer(false) },
        containerColor = t.surface,
        shape = androidx.compose.ui.graphics.RectangleShape,
        text = {
            Column {
                Text(ask.message, style = Face.mono(13f), color = t.text)
                if (ask.wantsText) {
                    Spacer(Modifier.height(10.dp))
                    Field(text, { text = it }, bg = t.surface2, pad = 9)
                }
            }
        },
        confirmButton = {
            TextButton({ Store.answer(true, text) }) {
                Text(ask.accept ?: if (ask.wantsText) "ok" else "yes", style = Face.mono(13f, FontWeight.Bold), color = t.accent)
            }
        },
        dismissButton = {
            TextButton({ Store.answer(false) }) {
                Text(if (ask.accept != null) "decline" else if (ask.wantsText) "cancel" else "no", style = Face.mono(13f), color = t.muted)
            }
        },
    )
}

@Composable
private fun Menubar(onRail: () -> Unit, onSheet: (String) -> Unit) {
    Row(
        Modifier.fillMaxWidth().background(t.chrome).windowInsetsPadding(WindowInsets.statusBars)
            .padding(start = 12.dp, end = 12.dp, bottom = 4.dp, top = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text("gliph", style = Face.serif(19f), color = t.accentChrome, modifier = Modifier.padding(end = 6.dp))
        Glyph(R.drawable.ic_hash, "channels", size = 16, chrome = true, action = onRail)
        Glyph(if (t.isDark) R.drawable.ic_moon else R.drawable.ic_sun,
            if (t.isDark) "switch to the light theme" else "switch to the dark theme",
            size = 16, chrome = true) { Store.useTheme(if (Store.theme.isDark) Theme.creme else Theme.midnight) }
        if (Store.snap.transfers.isNotEmpty()) {
            Glyph(R.drawable.ic_transfers, "file transfers", size = 16, chrome = true) { onSheet("transfers") }
        }
        Spacer(Modifier.weight(1f).width(6.dp))
        ServerStrip(onSheet = { onSheet("servers") })
        Glyph(R.drawable.ic_gear, "settings", size = 16, chrome = true) { onSheet("settings") }
    }
}

@Composable
private fun TitleStrip() {
    val ch = Store.channel()
    Row(Modifier.fillMaxWidth().heightIn(min = 34.dp).background(t.chrome).padding(horizontal = 12.dp),
        verticalAlignment = Alignment.CenterVertically) {
        Text(ch?.label ?: "gliph", style = Face.mono(13f, FontWeight.ExtraBold), color = t.chromeText, maxLines = 1)
        if (ch != null) {
            Spacer(Modifier.width(8.dp))
            Text("[${ch.members.size}] [+${ch.modes}]", style = Face.mono(11.5f), color = t.chromeDim, maxLines = 1)
            if (ch.encrypted) {
                Spacer(Modifier.width(8.dp))
                Text("e2e", style = Face.mono(11f, FontWeight.Bold), color = t.join)
            }
            if (!ch.isDm) {
                Spacer(Modifier.width(8.dp))
                Text("history: ${ch.history}", style = Face.mono(11f), color = t.chromeDim, maxLines = 1)
            }
        }
        Spacer(Modifier.width(8.dp))
        Text(ch?.topic ?: "", style = Face.mono(11.5f), color = t.chromeDim, maxLines = 1, overflow = TextOverflow.Ellipsis)
    }
}

/** Palette, room strip, reply strip and composer: everything under the stream. */
@Composable
private fun Conversation() {
    var draft by remember { mutableStateOf("") }
    val ctx = LocalContext.current
    val clip = remember { ctx.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager }
    fun clipImage() = clip.primaryClipDescription?.hasMimeType("image/*") == true
    var clipHasImage by remember { mutableStateOf(clipImage()) }
    DisposableEffect(clip) {
        val l = ClipboardManager.OnPrimaryClipChangedListener { clipHasImage = clipImage() }
        clip.addPrimaryClipChangedListener(l)
        onDispose { clip.removePrimaryClipChangedListener(l) }
    }
    LaunchedEffect(draft) { Store.updatePalette(draft) }
    val ch = Store.channel()

    fun send() {
        val text = draft
        if (text.isBlank()) return
        // Return with the palette up takes its first entry, like the web
        // client: "/upd⏎" is /update, not a message saying "/upd".
        val cmd = Store.palette.firstOrNull()
        if (cmd != null && text.startsWith("/") && !text.contains(' ')) {
            if (cmd.usage.contains('<') || cmd.usage.contains('[')) { draft = "/${cmd.name} "; return }
            draft = ""
            Store.submit("/${cmd.name}")
            return
        }
        draft = ""
        Store.submit(text)
    }

    fun pasteImage() {
        val item = clip.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.uri ?: return
        val mime = ctx.contentResolver.getType(item) ?: "image/png"
        val bytes = runCatching { ctx.contentResolver.openInputStream(item)?.use { it.readBytes() } }.getOrNull() ?: return
        Store.offerFile(if (mime == "image/jpeg") "image.jpg" else "image.png", mime, bytes)
    }

    if (Store.palette.isNotEmpty()) {
        Column(Modifier.fillMaxWidth().heightIn(max = 190.dp).background(t.surface2).verticalScroll(rememberScrollState())) {
            Box(Modifier.fillMaxWidth().height(1.dp).background(t.border))
            for (cmd in Store.palette) {
                Row(Modifier.fillMaxWidth().clickable { draft = "/${cmd.name} " }.padding(horizontal = 12.dp, vertical = 7.dp),
                    verticalAlignment = Alignment.CenterVertically) {
                    Text(cmd.usage, style = Face.mono(13f, FontWeight.Bold), color = t.text, maxLines = 1)
                    Spacer(Modifier.width(10.dp))
                    Text(cmd.desc, style = Face.mono(11.5f), color = t.muted, maxLines = 1,
                        overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
                    // A plugin's commands appear and vanish with it.
                    if (cmd.source != "builtin") Text(cmd.source, style = Face.mono(10.5f), color = t.agent)
                }
            }
        }
    }

    // The live room line. room0 itself is web and desktop, so the phone gets
    // the half it can honour: who is in there, where, and for how long.
    Store.snap.rooms.firstOrNull { it.channel == Store.snap.active }?.let { r ->
        Column(Modifier.fillMaxWidth().background(t.surface)) {
            Box(Modifier.fillMaxWidth().height(1.dp).background(t.r0Gradient))
            Row(Modifier.padding(horizontal = 12.dp, vertical = 7.dp), verticalAlignment = Alignment.CenterVertically) {
                Text("room0", style = Face.mono(12f, FontWeight.Bold), color = t.r0From)
                Spacer(Modifier.width(8.dp))
                Text("${r.room} · ${r.people.joinToString(", ")}" + (if (r.elapsed.isEmpty()) "" else " · ${r.elapsed}"),
                    style = Face.mono(12f), color = t.text2, maxLines = 1, overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f))
                if (r.voiceCount > 0) {
                    Text("voice ${r.voiceCount}", style = Face.mono(11f, FontWeight.Bold), color = t.teal,
                        modifier = Modifier.padding(horizontal = 8.dp))
                }
                Text("on desktop", style = Face.mono(11f), color = t.muted)
            }
        }
    }

    // "you are answering this", above the input. The line is the core's.
    if (Store.snap.reply.isNotEmpty()) {
        Row(Modifier.fillMaxWidth().background(t.surface).padding(start = 10.dp, end = 10.dp, top = 8.dp),
            verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.width(2.dp).height(18.dp).background(t.border))
            Spacer(Modifier.width(10.dp))
            Text("replying to ${Store.snap.reply}", style = Face.mono(11.5f), color = t.muted, maxLines = 1,
                overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
            Glyph(R.drawable.ic_close, "cancel the reply", color = t.muted, size = 14, chrome = true) { Store.setReply("") }
        }
    }

    Column(Modifier.fillMaxWidth().background(t.surface)) {
        Box(Modifier.fillMaxWidth().height(1.dp).background(t.border))
        Row(Modifier.padding(horizontal = 10.dp, vertical = 8.dp), verticalAlignment = Alignment.Bottom) {
            Box(Modifier.weight(1f).background(t.surface2).border(1.dp, t.border).padding(horizontal = 10.dp, vertical = 11.dp)) {
                if (draft.isEmpty()) {
                    Text((ch?.let { if (it.isDm) "message ${it.name}" else "message #${it.name}" } ?: "…") + "  (/ for commands)",
                        style = Face.mono(13f), color = t.muted, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
                BasicTextField(draft, { draft = it }, maxLines = 5,
                    textStyle = Face.mono(13f).copy(color = t.text), cursorBrush = SolidColor(t.accent),
                    keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, imeAction = ImeAction.Send),
                    keyboardActions = KeyboardActions(onSend = { send() }),
                    // A hardware Enter sends too, as return does on iOS;
                    // shift+Enter is the newline.
                    modifier = Modifier.fillMaxWidth().onPreviewKeyEvent {
                        if (it.key == Key.Enter && !it.isShiftPressed) {
                            if (it.type == KeyEventType.KeyDown) send()
                            true
                        } else false
                    })
            }
            // An image on the clipboard: the field's own paste only knows
            // text, so the bar grows a key while there is an image to take.
            if (clipHasImage) Glyph(R.drawable.ic_paste, "paste image", color = t.muted) { pasteImage() }
            Glyph(R.drawable.ic_clip, "attach a file", color = t.muted) { Store.onPickFile?.invoke() }
            Glyph(R.drawable.ic_send, "send", color = t.accent) { send() }
        }
    }
}

@Composable
private fun StatusBar() {
    val s = Store.snap
    Row(Modifier.fillMaxWidth().background(t.chrome).windowInsetsPadding(WindowInsets.navigationBars)
        .padding(horizontal = 12.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(if (s.solo) "nearby only" else if (s.online) "connected" else "reconnecting…",
            style = Face.mono(11f), color = if (s.online || s.solo) t.join else t.accent)
        if (s.nearbyOn) {
            Spacer(Modifier.width(12.dp))
            Text("nearby: ${s.nearbyPeers}", style = Face.mono(11f), color = t.chromeDim)
        }
        Spacer(Modifier.weight(1f))
        Store.channel()?.let { ch ->
            Text("${ch.role.ifEmpty { "member" }} · ${s.user?.nick ?: ""}", style = Face.mono(11f), color = t.chromeDim)
        }
    }
}

/** The keys are drawn, not spelled: the same pixelarticons the web client
 *  draws in web/src/icons.ts, carried here as vector drawables. */
@Composable
fun Glyph(icon: Int, label: String, color: Color? = null, size: Int = 20, chrome: Boolean = false, action: () -> Unit) {
    Box(Modifier.size(if (chrome) 28.dp else 34.dp, if (chrome) 30.dp else 38.dp)
        .clickable(onClick = action).semantics { contentDescription = label },
        contentAlignment = Alignment.Center) {
        Icon(painterResource(icon), null, tint = color ?: t.chromeDim, modifier = Modifier.size(size.dp))
    }
}
