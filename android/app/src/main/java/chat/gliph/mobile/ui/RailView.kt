//  The rail (mobile/Sources/Views/RailView.swift): channels, nearby channels,
//  DMs, then who is in the room. A drawer over the conversation.

package chat.gliph.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import chat.gliph.mobile.Channel
import chat.gliph.mobile.Member
import chat.gliph.mobile.Store

@Composable
fun RailView(onPick: () -> Unit) {
    val snap = Store.snap
    val channels = snap.channels.filter { !it.isDm && !it.nearby }
    val nearby = snap.channels.filter { it.nearby }
    val dms = snap.channels.filter { it.isDm }
    Column(Modifier.fillMaxSize().background(t.rail).safeDrawingPadding().verticalScroll(rememberScrollState())
        .padding(vertical = 20.dp)) {
        Title("Channels")
        channels.forEach { ChannelRow(it, onPick) }
        if (channels.isEmpty()) Hint("none yet")
        if (snap.nearbyOn) {
            Title("Nearby", "${snap.nearbyPeers} peers")
            nearby.forEach { ChannelRow(it, onPick) }
            if (nearby.isEmpty()) Hint("/nearby #name to open one")
        }
        if (dms.isNotEmpty()) {
            Title("Direct")
            dms.forEach { ChannelRow(it, onPick) }
        }
        Store.channel()?.let { ch ->
            Title("Members", "${ch.members.size}")
            ch.members.forEach { m ->
                Row(Modifier.padding(horizontal = 14.dp, vertical = 3.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text(m.sigil, style = Face.mono(13f, FontWeight.Bold), color = sigilColor(m))
                    Spacer(Modifier.width(7.dp))
                    // Offline regulars stay listed, greyed, IRC style.
                    Text(m.nick, style = Face.mono(13f), color = if (m.online || m.kind != "user") t.text2 else t.muted,
                        modifier = Modifier.weight(1f))
                    if (m.unverified) Text("unapproved device", style = Face.mono(10.5f, FontWeight.Bold), color = t.accent,
                        modifier = Modifier.padding(end = 6.dp))
                    if (m.away != null) Text("away", style = Face.mono(10.5f), color = t.muted)
                }
            }
        }
        Text("${snap.serverName.ifEmpty { "gliph" }}\n${snap.commandCount} commands · ${snap.pluginCommandCount} from plugins",
            style = Face.mono(11f), color = t.muted, modifier = Modifier.padding(start = 14.dp, end = 14.dp, top = 16.dp))
    }
}

@Composable
private fun sigilColor(m: Member): Color =
    if (m.kind != "user") t.agent else when (m.role) { "op" -> t.accent; "voice" -> t.join; else -> t.border }

@Composable
private fun Title(s: String, badge: String? = null) {
    Row(Modifier.padding(start = 14.dp, end = 14.dp, top = 14.dp, bottom = 4.dp)) {
        Text(s, style = Face.mono(11f, FontWeight.ExtraBold), color = t.muted)
        if (badge != null) { Spacer(Modifier.width(8.dp)); Text(badge, style = Face.mono(10.5f), color = t.muted) }
    }
}

@Composable
private fun Hint(s: String) = Text(s, style = Face.mono(11f), color = t.muted, modifier = Modifier.padding(horizontal = 14.dp))

@Composable
private fun ChannelRow(ch: Channel, onPick: () -> Unit) {
    val selected = ch.id == Store.snap.active
    Row(Modifier.fillMaxWidth().heightIn(min = 48.dp).background(if (selected) t.surface2 else Color.Transparent)
        .clickable { Store.setActive(ch.id); onPick() }.padding(horizontal = 14.dp, vertical = 11.dp),
        verticalAlignment = Alignment.CenterVertically) {
        // A DM's dot is the other person's presence.
        val dot = when {
            ch.isDm -> if (ch.members.firstOrNull { it.nick == ch.name }?.online == true) t.join else t.muted
            ch.nearby -> t.agent
            else -> t.muted
        }
        Text(if (ch.isDm) "•" else "#", style = Face.mono(13f), color = dot)
        Spacer(Modifier.width(7.dp))
        Text(ch.name, style = Face.mono(13f, if (selected) FontWeight.Bold else FontWeight.Medium),
            color = if (selected) t.text else t.text2, maxLines = 1, overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f))
        if (ch.encrypted) Text("e2e", style = Face.mono(10f), color = t.muted, modifier = Modifier.padding(start = 6.dp))
        if (ch.unread > 0) Text("${ch.unread}", style = Face.mono(10f, FontWeight.Bold), color = Color.White,
            modifier = Modifier.padding(start = 6.dp).background(t.accent).padding(horizontal = 5.dp, vertical = 1.dp))
    }
}
