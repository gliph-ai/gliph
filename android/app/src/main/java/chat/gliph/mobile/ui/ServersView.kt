//  The server switcher (mobile/Sources/Views/ServersView.swift). A Gliph server
//  IS a team, so switching teams is switching servers, the way mIRC switched
//  networks. The rules live in core/src/servers.rs; this file only draws them.

package chat.gliph.mobile.ui

import android.graphics.BitmapFactory
import android.util.Base64
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import chat.gliph.mobile.ServerEntry
import chat.gliph.mobile.Store
import kotlinx.coroutines.launch

/** The theme's accent tokens in the order a server picks one: deterministic
 *  from the address, the same order and hash as web/src/servers.ts. */
@Composable
fun serverInk(host: String): Color {
    val ink = listOf(t.link, t.agent, t.join, t.action, t.gold, t.accent, t.teal)
    var h = 0u
    for (b in host.toByteArray()) h = h * 31u + b.toUByte().toUInt()
    return ink[(h % ink.size.toUInt()).toInt()]
}

/** The avatar: the server's own icon when it has one, its monogram when not.
 *  Square, never round. Inline bytes only, never a fetch. */
@Composable
fun ServerAvatar(e: ServerEntry, size: Int = 22) {
    val img = remember(e.icon) {
        e.icon.substringAfter("base64,", "").takeIf { it.isNotEmpty() }
            ?.let { runCatching { Base64.decode(it, Base64.DEFAULT) }.getOrNull() }
            ?.let { BitmapFactory.decodeByteArray(it, 0, it.size)?.asImageBitmap() }
    }
    Box(Modifier.size(size.dp).background(serverInk(e.host)), contentAlignment = Alignment.Center) {
        if (img != null) Image(img, null, contentScale = ContentScale.Crop, modifier = Modifier.size(size.dp))
        else Text(e.monogram, style = Face.serif(size * 0.43f), color = Color.White)
    }
}

/** An unread count in mIRC brackets. Unread, never mentions. */
@Composable
fun UnreadBracket(n: Int) {
    if (n > 0) Text("[${if (n > 99) "99+" else "$n"}]", style = Face.mono(10.5f, FontWeight.ExtraBold), color = t.accent)
}

/** The strip in the menubar: where you are, then one avatar per other server. */
@Composable
fun ServerStrip(onSheet: () -> Unit) {
    val others = Store.servers.list.filter { it.host != Store.servers.active }
    val here = Store.activeServer?.name?.ifEmpty { null } ?: Store.snap.serverName
    // Nearby only, with no server at all: nothing to name, nowhere to switch.
    if (here.isEmpty() && others.isEmpty()) return
    Row(horizontalArrangement = Arrangement.spacedBy(7.dp), verticalAlignment = Alignment.CenterVertically) {
        Row(Modifier.background(t.link).clickable(onClick = onSheet).padding(horizontal = 8.dp, vertical = 3.dp),
            verticalAlignment = Alignment.CenterVertically) {
            Text(here, style = Face.mono(12f, FontWeight.Bold),
                color = t.chromeText, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.widthIn(max = 130.dp))
            if (Store.snap.unreadTotal > 0) {
                Text(" [${Store.snap.unreadTotal}]", style = Face.mono(11f, FontWeight.ExtraBold), color = t.chromeText)
            }
            Text(" ▾", style = Face.mono(10f, FontWeight.Bold), color = t.chromeText)
        }
        // The avatar row appears at the SECOND server; two at most on a phone.
        others.take(2).forEach { e ->
            Row(Modifier.clickable { Store.switchTo(e.host) }, verticalAlignment = Alignment.CenterVertically) {
                ServerAvatar(e, 20); Spacer(Modifier.width(3.dp)); UnreadBracket(e.unread)
            }
        }
        if (others.size > 2) {
            Box(Modifier.size(20.dp).border(1.dp, t.chromeDim).clickable(onClick = onSheet), contentAlignment = Alignment.Center) {
                Text("+${others.size - 2}", style = Face.mono(9.5f, FontWeight.ExtraBold), color = t.chromeDim)
            }
        }
    }
}


/** The full list: hosts, your nick on each, and the way to add another. */
@Composable
fun ServersView(close: () -> Unit) {
    var adding by remember { mutableStateOf(false) }
    var host by remember { mutableStateOf("") }
    var nick by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var err by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    Column(Modifier.fillMaxWidth().imePadding()) {
        SheetBar("Servers", close)
        Column(Modifier.verticalScroll(rememberScrollState())) {
            Store.servers.list.forEach { e ->
                val here = e.host == Store.servers.active
                Row(Modifier.fillMaxWidth().background(if (here) t.surface2 else t.surface)
                    .clickable { Store.switchTo(e.host); close() }.padding(horizontal = 16.dp, vertical = 11.dp),
                    verticalAlignment = Alignment.CenterVertically) {
                    ServerAvatar(e, 36)
                    Spacer(Modifier.width(12.dp))
                    Column(Modifier.weight(1f)) {
                        Text(e.name.ifEmpty { e.shortHost }, style = Face.mono(13.5f, FontWeight.Bold), color = t.text)
                        Text(if (e.nick.isEmpty()) e.shortHost else "${e.shortHost} · <${e.nick}>", style = Face.mono(10.5f),
                            color = t.muted, maxLines = 1, overflow = TextOverflow.MiddleEllipsis)
                    }
                    if (here) Text("✓", style = Face.sans(14f, FontWeight.Bold), color = t.join) else UnreadBracket(e.unread)
                }
                Box(Modifier.fillMaxWidth().height(1.dp).background(t.border))
            }
            if (!adding) {
                Row(Modifier.fillMaxWidth().clickable { adding = true; host = ""; nick = ""; password = ""; err = "" }
                    .padding(horizontal = 16.dp, vertical = 13.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text("+", style = Face.mono(17f), color = t.link, modifier = Modifier.width(36.dp))
                    Spacer(Modifier.width(11.dp))
                    Text("add a server", style = Face.mono(12.5f, FontWeight.ExtraBold), color = t.link)
                }
            } else {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(9.dp)) {
                    Text("sign in to another server", style = Face.mono(11f, FontWeight.ExtraBold), color = t.muted)
                    Small("address"); Field(host, { host = it }, kind = KeyboardType.Uri, bg = t.bg, pad = 9)
                    Small("nick"); Field(nick, { nick = it }, bg = t.bg, pad = 9)
                    Small("password"); Field(password, { password = it }, secure = true, bg = t.bg, pad = 9)
                    if (err.isNotEmpty()) Text(err, style = Face.mono(11f), color = t.accent)
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(if (busy) "signing in…" else "sign in", style = Face.mono(12.5f, FontWeight.ExtraBold),
                            color = t.chromeText, modifier = Modifier.background(t.chrome).clickable(enabled = !busy) {
                                busy = true; err = ""
                                scope.launch {
                                    try { Store.addServer(host, nick, password); adding = false; close() }
                                    catch (e: Exception) { err = e.message ?: "could not sign in" }
                                    busy = false
                                }
                            }.padding(horizontal = 14.dp, vertical = 9.dp))
                        Spacer(Modifier.width(10.dp))
                        Text("cancel", style = Face.mono(12f), color = t.muted, modifier = Modifier.clickable { adding = false })
                    }
                }
            }
        }
    }
}

@Composable
private fun Small(s: String) = Text(s, style = Face.mono(10f, FontWeight.ExtraBold), color = t.muted)

/** The navy bar at the top of a sheet: a title and the way out. */
@Composable
fun SheetBar(title: String, close: () -> Unit) {
    Row(Modifier.fillMaxWidth().background(t.chrome).padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically) {
        Text(title, style = Face.serif(15f), color = t.chromeText, modifier = Modifier.weight(1f))
        Text("done", style = Face.mono(12f, FontWeight.Bold).copy(fontSize = 12.sp), color = t.chromeDim,
            modifier = Modifier.clickable(onClick = close))
    }
}
