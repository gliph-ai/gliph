//  Settings (SettingsView.swift) and the transfer sheet (TransfersView.swift).

package chat.gliph.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.RectangleShape
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import chat.gliph.mobile.Store
import chat.gliph.mobile.Transfer

/** Who you are, what you are connected to, and the way out. One tap from
 *  every screen to a destructive action is how a phone loses a session. */
@Composable
fun SettingsView(close: () -> Unit) {
    val s = Store.snap
    var confirm by remember { mutableStateOf(false) }
    val ctx = LocalContext.current
    val version = remember {
        runCatching { ctx.packageManager.getPackageInfo(ctx.packageName, 0).let { "${it.versionName} (${it.longVersionCode})" } }
            .getOrDefault("?")
    }
    Column(Modifier.fillMaxWidth().background(t.rail).verticalScroll(rememberScrollState())) {
        SheetBar("Settings", close)
        Title("Signed in")
        Pair("nick", s.user?.nick ?: if (s.solo) "nearby only" else "—")
        s.user?.role?.takeIf { it.isNotEmpty() }?.let { Pair("role", it) }
        Pair("server", s.serverName.ifEmpty { s.server })
        Pair("connection", if (s.solo) "nearby only" else if (s.online) "connected" else "reconnecting")
        Title("App")
        Pair("version", version)
        Pair("commands", "${s.commandCount} · ${s.pluginCommandCount} from plugins")
        Title(if (s.solo) "Nearby" else "Session")
        Box(Modifier.padding(horizontal = 14.dp).padding(top = 4.dp).fillMaxWidth().heightIn(min = 48.dp)
            .background(t.surface2).border(1.dp, t.border)
            .clickable { if (s.solo) { Store.submit("/nearby-off"); close() } else confirm = true }
            .padding(horizontal = 14.dp), contentAlignment = Alignment.CenterStart) {
            Text(if (s.solo) "leave nearby" else "sign out", style = Face.mono(13f, FontWeight.Bold), color = t.accent)
        }
        Text("signing out makes the encrypted history on this device unreadable: it forgets its key as well as its session.",
            style = Face.mono(11f), color = t.muted, modifier = Modifier.padding(start = 14.dp, end = 14.dp, top = 8.dp, bottom = 28.dp))
    }
    if (confirm) AlertDialog(
        onDismissRequest = { confirm = false }, containerColor = t.surface, shape = RectangleShape,
        title = { Text("sign out?", style = Face.mono(14f, FontWeight.Bold), color = t.text) },
        text = {
            Text("this device forgets its key as well as its session, so the encrypted history on it becomes unreadable. " +
                "signing back in re-collects the keys from the server.", style = Face.mono(12f), color = t.text2)
        },
        confirmButton = { TextButton({ confirm = false; close(); Store.signOut() }) {
            Text("sign out", style = Face.mono(13f, FontWeight.Bold), color = t.accent) } },
        dismissButton = { TextButton({ confirm = false }) { Text("stay", style = Face.mono(13f), color = t.muted) } },
    )
}

@Composable
private fun Title(s: String) = Text(s, style = Face.mono(11f, FontWeight.ExtraBold), color = t.muted,
    modifier = Modifier.padding(start = 14.dp, end = 14.dp, top = 16.dp, bottom = 6.dp))

@Composable
private fun Pair(label: String, value: String) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 14.dp, vertical = 5.dp)) {
        Text(label, style = Face.mono(12f), color = t.muted)
        Spacer(Modifier.weight(1f).width(12.dp))
        Text(value, style = Face.mono(12.5f), color = t.text2, maxLines = 1, overflow = TextOverflow.MiddleEllipsis)
    }
}

/** mIRC's DCC panel: one row per transfer, a bar that fills, and the two
 *  buttons that matter on an offer someone made you. */
@Composable
fun TransfersView(close: () -> Unit) {
    val list = Store.snap.transfers
    Column(Modifier.fillMaxWidth().background(t.surface)) {
        SheetBar("File transfers", close)
        Column(Modifier.padding(14.dp).verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            if (list.isEmpty()) Text("nothing yet. /send <nick> offers a file to someone nearby.",
                style = Face.mono(13f), color = t.muted, modifier = Modifier.padding(vertical = 20.dp))
            list.asReversed().forEach { TransferRow(it) }
            Text("peer to peer over the local network · nothing is uploaded anywhere", style = Face.mono(10.5f),
                color = t.muted, textAlign = TextAlign.Center, modifier = Modifier.fillMaxWidth().padding(top = 12.dp, bottom = 16.dp))
        }
    }
}

private fun size(n: Long) = when {
    n >= 1048576 -> "%.1f MB".format(n / 1048576.0)
    n >= 1024 -> "${n / 1024} KB"
    else -> "$n B"
}

@Composable
private fun TransferRow(x: Transfer) {
    val bad = x.state == "failed" || x.state == "declined"
    val status = when (x.state) {
        "offered" -> if (x.dir == "recv") "offers you" else "waiting for accept"
        "running" -> "${(x.progress * 100).toInt()}%"
        "done" -> if (x.dir == "recv") "received" else "sent"
        else -> x.error ?: x.state
    }
    Column(Modifier.fillMaxWidth().background(t.surface2).border(1.dp, t.border).padding(10.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(if (x.dir == "send") ">>" else "<<", style = Face.mono(13f, FontWeight.ExtraBold), color = t.muted)
            Spacer(Modifier.width(8.dp))
            Text(x.name, style = Face.mono(13f, FontWeight.Bold), color = t.text, maxLines = 1, modifier = Modifier.weight(1f))
            Text(size(x.size), style = Face.mono(11f), color = t.muted)
        }
        Text("${if (x.dir == "send") "to" else "from"} ${x.peerNick}", style = Face.mono(11f), color = t.muted)
        LinearProgressIndicator(progress = { x.progress }, modifier = Modifier.fillMaxWidth(),
            color = if (bad) t.accent else t.join, trackColor = t.border, drawStopIndicator = {})
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(status, style = Face.mono(11f), color = if (bad) t.accent else t.muted, modifier = Modifier.weight(1f))
            if (x.dir == "recv" && x.state == "offered") {
                Text("[accept]", style = Face.mono(11.5f, FontWeight.Bold), color = t.join,
                    modifier = Modifier.clickable { Store.accept(x.id) }.padding(horizontal = 4.dp))
                Text("[decline]", style = Face.mono(11.5f, FontWeight.Bold), color = t.muted,
                    modifier = Modifier.clickable { Store.decline(x.id) }.padding(horizontal = 4.dp))
            }
            if (x.state == "done" && x.dir == "recv") Text("in Downloads", style = Face.mono(11.5f, FontWeight.Bold), color = t.link)
        }
    }
}
