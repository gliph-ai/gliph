//  The sign-in screen (mobile/Sources/Views/AuthView.swift). Two doors: an
//  account on a server, or nobody at all: nearby mode (F9) needs no account.

package chat.gliph.mobile.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.unit.dp
import androidx.compose.ui.zIndex
import chat.gliph.mobile.Store
import kotlinx.coroutines.launch

@Composable
fun AuthScreen() {
    var registering by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var err by remember { mutableStateOf("") }
    /** A pinned build hides the server field; triple-tapping the logo shows it. */
    var serverShown by remember { mutableStateOf(false) }
    var menuOpen by remember { mutableStateOf(false) }
    var logoTaps by remember { mutableIntStateOf(0) }
    var lastTap by remember { mutableLongStateOf(0L) }
    var server by remember { mutableStateOf(Store.server) }
    var nick by remember { mutableStateOf("") }
    var email by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var invite by remember { mutableStateOf("") }
    val scope = rememberCoroutineScope()
    val showServer = serverShown || !Store.hasPinnedServer

    fun go() {
        menuOpen = false; busy = true; err = ""
        scope.launch {
            try {
                val s = server.trim()
                if (s.isEmpty()) throw chat.gliph.mobile.Fail("no server address")
                if (registering) Store.register(s, nick.trim(), email.trim(), password, invite.trim())
                else Store.signIn(s, nick.trim(), password)
            } catch (e: Exception) {
                err = e.message ?: "could not sign in"
            }
            busy = false
        }
    }

    Box(Modifier.fillMaxSize().background(t.bg).clickable(indication = null, interactionSource = null) { menuOpen = false }) {
        Column(
            Modifier.fillMaxSize().safeDrawingPadding().imePadding().verticalScroll(rememberScrollState())
                .padding(horizontal = 22.dp, vertical = 40.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Column(Modifier.widthIn(max = 420.dp).fillMaxWidth()) {
                Text("gliph", style = Face.serif(46f), color = t.accent, textAlign = TextAlign.Center,
                    modifier = Modifier.fillMaxWidth().clickable(indication = null, interactionSource = null) {
                        val n = System.currentTimeMillis()
                        logoTaps = if (n - lastTap > 600) 1 else logoTaps + 1
                        lastTap = n
                        if (logoTaps >= 3) serverShown = true
                    })
                Text(if (registering) "redeem your invite" else "sign in to your server",
                    style = Face.mono(13f), color = t.muted, textAlign = TextAlign.Center,
                    modifier = Modifier.fillMaxWidth().padding(top = 6.dp, bottom = 26.dp))

                if (showServer) {
                    Label("server")
                    Field(server, { server = it; menuOpen = false }, hint = "https://chat.example.com", kind = KeyboardType.Uri)
                }
                Label("nick")
                Field(nick, { nick = it; menuOpen = false })
                if (registering) {
                    Label("email")
                    Field(email, { email = it; menuOpen = false }, kind = KeyboardType.Email)
                }
                Label("password")
                Field(password, { password = it; menuOpen = false }, secure = true, onDone = if (registering) null else ::go)
                if (registering) {
                    Label("invite code")
                    Text("from your server's admin, blank only if you are the first account",
                        style = Face.mono(11f), color = t.muted, modifier = Modifier.padding(bottom = 4.dp))
                    Field(invite, { invite = it; menuOpen = false }, onDone = ::go)
                }

                // A split button: the thing you almost always want, and a caret
                // holding the ways in that are not it.
                Box(Modifier.padding(top = 18.dp).zIndex(1f)) {
                    Row(Modifier.height(44.dp)) {
                        Box(Modifier.weight(1f).height(44.dp).background(if (busy) t.muted else t.chrome)
                            .clickable(enabled = !busy) { go() }, contentAlignment = Alignment.Center) {
                            Text(if (busy) "…" else if (registering) "join" else "connect",
                                style = Face.mono(13f, FontWeight.Bold), color = Color.White)
                        }
                        Box(Modifier.width(1.dp).height(44.dp).background(t.bg))
                        Box(Modifier.width(46.dp).height(44.dp)
                            .background(if (menuOpen) t.accent else if (busy) t.muted else t.chrome)
                            .clickable { menuOpen = !menuOpen }, contentAlignment = Alignment.Center) {
                            Text(if (menuOpen) "▲" else "▼", style = Face.mono(11f, FontWeight.Bold), color = Color.White)
                        }
                    }
                    if (menuOpen) {
                        Column(Modifier.align(Alignment.TopEnd).offset(y = 50.dp).width(236.dp).background(t.accent)) {
                            MenuRow(if (registering) "sign in with an account" else "redeem an invite") {
                                menuOpen = false; registering = !registering; err = ""
                            }
                            if (Store.hasPinnedServer) {
                                Box(Modifier.fillMaxWidth().height(1.dp).background(t.onAccentLine))
                                MenuRow(if (showServer) "use the standard server" else "use a custom server") {
                                    menuOpen = false
                                    if (showServer) {
                                        Store.useDefaultServer(); server = Store.server; serverShown = false
                                    } else serverShown = true
                                }
                            }
                        }
                    }
                }

                if (err.isNotEmpty()) {
                    Text(err, style = Face.mono(12f), color = t.accent, modifier = Modifier.padding(top = 14.dp))
                }

                Text(buildAnnotatedString {
                    pushStyle(SpanStyle(color = t.muted)); append("or "); pop()
                    pushStyle(SpanStyle(color = t.link, textDecoration = TextDecoration.Underline))
                    append("chat with people nearby"); pop()
                }, style = Face.mono(13f), textAlign = TextAlign.Center,
                    modifier = Modifier.fillMaxWidth().padding(top = 22.dp).clickable {
                        if (nick.isBlank()) err = "pick a nick first: it is what people nearby will see"
                        else Store.startSolo(nick.trim())
                    }.padding(vertical = 4.dp))
                Text("no account, no server, just this network", style = Face.mono(11f), color = t.muted,
                    textAlign = TextAlign.Center, modifier = Modifier.fillMaxWidth())
            }
        }
    }
}

@Composable
private fun MenuRow(label: String, act: () -> Unit) {
    Text(label, style = Face.mono(12f, FontWeight.Bold), color = Color.White, textAlign = TextAlign.End,
        modifier = Modifier.fillMaxWidth().clickable(onClick = act).padding(horizontal = 13.dp, vertical = 11.dp))
}

@Composable
private fun Label(s: String) {
    Text(s, style = Face.mono(11f, FontWeight.Bold), color = t.muted, modifier = Modifier.padding(top = 14.dp, bottom = 5.dp))
}

/** A squared-off field, shared with the servers sheet. */
@Composable
fun Field(
    value: String, onChange: (String) -> Unit, hint: String = "", secure: Boolean = false,
    kind: KeyboardType = KeyboardType.Text, bg: Color = t.surface, pad: Int = 12,
    /** The keyboard's done key; the last field of a form submits it. */
    onDone: (() -> Unit)? = null,
) {
    Box(Modifier.fillMaxWidth().background(bg).border(1.dp, t.border).padding(horizontal = 11.dp, vertical = pad.dp)) {
        if (value.isEmpty() && hint.isNotEmpty()) Text(hint, style = Face.mono(13f), color = t.muted)
        BasicTextField(value, onChange, singleLine = true,
            textStyle = Face.mono(13f).copy(color = t.text), cursorBrush = SolidColor(t.accent),
            visualTransformation = if (secure) PasswordVisualTransformation() else VisualTransformation.None,
            keyboardOptions = KeyboardOptions(autoCorrectEnabled = false,
                keyboardType = if (secure) KeyboardType.Password else kind,
                imeAction = if (onDone != null) ImeAction.Done else ImeAction.Next),
            keyboardActions = KeyboardActions(onDone = { onDone?.invoke() }),
            modifier = Modifier.fillMaxWidth())
    }
}

