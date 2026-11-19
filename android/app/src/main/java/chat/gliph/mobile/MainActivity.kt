//  Gliph for Android. A native Compose client over the shared Rust core.
//
//  There is no webview here and no second implementation of anything: the
//  protocol, the crypto, the sync and the offline cache are core/, the same
//  crate the web client runs as wasm and the iOS app links. What is in this
//  module is the parts of a client that are genuinely per-platform.

package chat.gliph.mobile

import android.Manifest
import android.app.Application
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.OpenableColumns
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import chat.gliph.mobile.ui.Root

class GliphApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Push.init(this)
        Store.init(this)
    }
}

class MainActivity : ComponentActivity() {
    private val pick = registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        uri?.let(::attach)
    }
    private val askNotify = registerForActivityResult(ActivityResultContracts.RequestPermission()) {}

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        Store.onPickFile = { pick.launch(arrayOf("*/*")) }
        setContent { Root() }
        intent?.let(::fromNotification)
        intent?.let(::fromLink)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        fromNotification(intent)
        fromLink(intent)
    }

    /** gliph://approve/<code>: a new device's QR code, scanned with the camera.
     *  It only opens the confirmation; nothing is approved without a tap. */
    private fun fromLink(i: Intent) {
        val u = i.data ?: return
        if (u.scheme == "gliph" && u.host == "approve") u.lastPathSegment?.let { Store.approveCode = it }
        i.data = null
    }

    override fun onStart() {
        super.onStart()
        // Coming back after the OS suspended the socket: the only honest
        // assumption is that we missed something.
        Store.foreground = true
        if (Store.signedIn) {
            Store.wake()
            // Cheap when nothing changed (send dedupes by token), and the only
            // way a token minted after launch reaches the server.
            Push.register()
            if (Build.VERSION.SDK_INT >= 33) askNotify.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    override fun onStop() {
        Store.foreground = false
        super.onStop()
    }

    /** Tapping a notification opens the conversation it came from. */
    private fun fromNotification(i: Intent) {
        i.getStringExtra("channel")?.let { Store.openFromNotification(it); i.removeExtra("channel") }
    }

    /** A file from the picker, or an image from the clipboard. */
    fun attach(uri: Uri) {
        val cr = contentResolver
        val bytes = runCatching { cr.openInputStream(uri)?.use { it.readBytes() } }.getOrNull() ?: return
        val name = cr.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use {
            if (it.moveToFirst()) it.getString(0) else null
        } ?: uri.lastPathSegment ?: "file"
        val mime = cr.getType(uri) ?: "application/octet-stream"
        Store.offerFile(name, mime, bytes)
    }
}
