//  The blob surface, drawn (mobile/Sources/Views/BlobView.swift). That is all
//  this file does: the document, focus order, selection, menus and drafts live
//  in core/src/gum.rs, the same code the web client runs as wasm.

package chat.gliph.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
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
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp
import chat.gliph.mobile.GumNode
import chat.gliph.mobile.Store

@Composable
fun BlobView() {
    val s = Store.surface
    Box(Modifier.fillMaxSize().background(t.surface)) {
        LazyColumn(Modifier.fillMaxSize().padding(vertical = 8.dp)) {
            if (!s.loaded) item { Text("loading…", style = Face.mono(13f), color = t.muted, modifier = Modifier.padding(12.dp)) }
            items(s.nodes.size) { Node(s.nodes[it]) }
        }
        // The scrim is the only way off this menu on a touch device: esc closes
        // it, and a phone has no esc.
        s.menu?.let { menu ->
            Box(Modifier.fillMaxSize().background(t.scrim).clickable(indication = null, interactionSource = null) { Store.gumEscape() })
            Column(Modifier.align(Alignment.Center).padding(horizontal = 40.dp).fillMaxWidth()
                .background(t.surface2).border(1.dp, t.border)) {
                menu.forEach { e ->
                    Text(e.label, style = Face.mono(13f, if (e.selected) FontWeight.Bold else FontWeight.Medium), color = t.text,
                        modifier = Modifier.fillMaxWidth().background(if (e.selected) t.surface else Color.Transparent)
                            .clickable { Store.gumTapMenu(e.id) }.padding(horizontal = 14.dp, vertical = 11.dp))
                }
            }
        }
    }
}

@Composable
private fun Node(n: GumNode) {
    when (n) {
        is GumNode.Text -> Text(n.body, style = Face.mono(13f), color = when (n.style) {
            "info" -> t.link; "warn" -> t.gold; "error" -> t.accent; else -> t.text
        }, modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp))
        is GumNode.Divider -> Text(if (n.label.isEmpty()) "--" else "-- ${n.label} --", style = Face.mono(11f), color = t.muted,
            modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp))
        is GumNode.Row -> Row(Modifier.fillMaxWidth().background(if (n.selected) t.surface2 else Color.Transparent)
            .clickable { Store.gumTap(n.index) }.padding(horizontal = 12.dp, vertical = 7.dp),
            verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(if (n.number == 0) " " else "${n.number}", style = Face.mono(11f), color = t.muted, modifier = Modifier.width(12.dp))
            val (glyph, colour) = when (n.state) {
                "ok", "done" -> "✓" to t.join; "warn" -> "!" to t.gold; "error" -> "×" to t.accent
                "open" -> "○" to t.muted; else -> " " to t.muted
            }
            Text(glyph, style = Face.mono(11f), color = colour)
            Text(n.label, style = Face.mono(13f), color = t.text)
            if (n.hint.isNotEmpty()) Text(n.hint, style = Face.mono(11.5f), color = t.muted, maxLines = 1, modifier = Modifier.weight(1f, false))
            Spacer(Modifier.weight(1f))
            if (n.hasActions) Text("›", style = Face.mono(13f), color = t.muted)
        }
        // Sideways rather than squeezed: a table with wrapped cells is not one.
        is GumNode.Table -> Row(Modifier.horizontalScroll(rememberScrollState()).padding(horizontal = 12.dp, vertical = 6.dp),
            horizontalArrangement = Arrangement.spacedBy(14.dp)) {
            n.cols.forEachIndexed { c, name ->
                Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text(name, style = Face.mono(11f, FontWeight.Bold), color = t.muted)
                    n.rows.forEach { r -> Text(r.getOrElse(c) { "" }, style = Face.mono(12f), color = t.text2) }
                }
            }
        }
        is GumNode.Input -> BlobInput(n)
        is GumNode.Buttons -> Row(Modifier.padding(horizontal = 12.dp, vertical = 6.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            n.buttons.forEach { b ->
                Text(b.label, style = Face.mono(12f, FontWeight.Bold), color = if (b.selected) Color.White else t.text,
                    modifier = Modifier.background(if (b.selected) t.accent else t.surface2).border(1.dp, t.border)
                        .clickable { Store.gumTap(b.index) }.padding(horizontal = 12.dp, vertical = 8.dp))
            }
        }
        is GumNode.Bar -> Text(n.body, style = Face.mono(11f), color = t.muted,
            modifier = Modifier.fillMaxWidth().background(t.surface2).padding(horizontal = 12.dp, vertical = 8.dp))
    }
}

/** The input keeps its own text while the user types and hands it to the core
 *  on every change; the core is the truth for what is in the box (it clears it
 *  on submit). */
@Composable
private fun BlobInput(n: GumNode.Input) {
    var text by remember { mutableStateOf(n.value) }
    LaunchedEffect(n.value) { if (n.value != text) text = n.value }
    Column(Modifier.padding(horizontal = 12.dp, vertical = 6.dp)) {
        if (n.label.isNotEmpty()) Text(n.label, style = Face.mono(11f, FontWeight.Bold), color = t.muted,
            modifier = Modifier.padding(bottom = 4.dp))
        Row(Modifier.fillMaxWidth().background(t.surface2).border(1.dp, if (n.selected) t.accent else t.border)
            .padding(horizontal = 10.dp, vertical = 9.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.weight(1f)) {
                if (text.isEmpty()) Text(n.placeholder, style = Face.mono(13f), color = t.muted)
                BasicTextField(text, { text = it; Store.gumSetDraft(it) }, singleLine = true,
                    textStyle = Face.mono(13f).copy(color = t.text), cursorBrush = SolidColor(t.accent),
                    keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, imeAction = ImeAction.Send),
                    keyboardActions = KeyboardActions(onSend = { Store.gumActivate() }),
                    // Typing into a field means selecting it too.
                    modifier = Modifier.fillMaxWidth().onFocusChanged { if (it.isFocused) Store.gumFocusInput(n.index) })
            }
            Spacer(Modifier.width(6.dp))
            Text("[go]", style = Face.mono(12f, FontWeight.Bold), color = t.accent, modifier = Modifier.clickable { Store.gumActivate() })
        }
    }
}

/** The switcher: chat first, then this channel's apps, with a 2px accent rule
 *  under the one you are in. */
@Composable
fun BlobSwitcher() {
    val blobs = Store.surface.blobs
    if (blobs.isEmpty()) return
    Row(Modifier.fillMaxWidth().background(t.chrome).horizontalScroll(rememberScrollState()).padding(horizontal = 12.dp),
        horizontalArrangement = Arrangement.spacedBy(14.dp)) {
        Pick("", "chat", "builtin")
        blobs.forEach { Pick(it.id, it.title, it.source) }
    }
}

@Composable
private fun Pick(id: String, label: String, source: String) {
    val on = Store.snap.activeBlob == id
    Column(Modifier.clickable { Store.openBlob(id) }) {
        Text(label, style = Face.mono(12f, if (on) FontWeight.ExtraBold else FontWeight.SemiBold),
            color = if (on) t.chromeText else if (source == "builtin") t.chromeDim else t.agent,
            modifier = Modifier.padding(top = 5.dp, bottom = 6.dp))
        Box(Modifier.height(2.dp).fillMaxWidth().background(if (on) t.accent else Color.Transparent))
    }
}
