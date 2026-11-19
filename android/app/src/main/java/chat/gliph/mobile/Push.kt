//  Mobile push (spec F5), Android half: mobile/Sources/Push.swift over FCM.
//
//  The server's wake carries nothing: a data message whose only field is
//  wake=1 (server/fcm.go). So this file does not display anything the push
//  said. It hands the FCM token to the server, and when FCM wakes us the core
//  re-syncs, decrypts with this device's own key, and writes the notification
//  locally. Everything the user reads was written on this phone.
//
//  There is no google-services.json: FirebaseApp is built from the four
//  BuildConfig values, and when they are empty push is simply off.

package chat.gliph.mobile

import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.util.Log
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.google.firebase.messaging.FirebaseMessaging
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject

object Push {
    const val CHANNEL_DIRECT = "direct"
    const val CHANNEL_MESSAGES = "messages"

    private val http = OkHttpClient()
    private var registered = ""
    private var on = false

    fun init(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL_DIRECT, "Direct messages and tags",
            NotificationManager.IMPORTANCE_HIGH))
        nm.createNotificationChannel(NotificationChannel(CHANNEL_MESSAGES, "Highlights and channel activity",
            NotificationManager.IMPORTANCE_DEFAULT))

        if (BuildConfig.FCM_APP_ID.isEmpty() || BuildConfig.FCM_PROJECT_ID.isEmpty()) {
            Log.i("gliph", "push: no FCM config in this build, phone wakes are off")
            return
        }
        val opts = FirebaseOptions.Builder()
            .setApplicationId(BuildConfig.FCM_APP_ID).setApiKey(BuildConfig.FCM_API_KEY)
            .setProjectId(BuildConfig.FCM_PROJECT_ID).setGcmSenderId(BuildConfig.FCM_SENDER_ID)
            .build()
        if (FirebaseApp.getApps(ctx).isEmpty()) FirebaseApp.initializeApp(ctx, opts)
        on = true
    }

    /** Ask FCM for the token and hand it to the server. Safe on every launch:
     *  the server upserts by token, so a rotation replaces the row. */
    fun register() {
        if (!on || Store.token.isEmpty()) return
        FirebaseMessaging.getInstance().token.addOnSuccessListener { send(it) }
            .addOnFailureListener { Log.w("gliph", "push: no FCM token: $it") }
    }

    fun send(token: String) {
        if (token.isEmpty() || token == registered || Store.token.isEmpty()) return
        val url = Store.api("/api/push/devices") ?: return
        val req = Request.Builder().url(url).header("Authorization", "Bearer ${Store.token}")
            .post(JSONObject().put("platform", "android").put("token", token).toString()
                .toRequestBody("application/json".toMediaType())).build()
        Store.scope.launch {
            val code = withContext(Dispatchers.IO) {
                runCatching { http.newCall(req).execute().use { it.code } }.getOrDefault(0)
            }
            Log.i("gliph", "push: token handed to the server: HTTP $code")
            if (code == 200) registered = token
        }
    }

    /** Hand the token back on sign-out, or the account keeps waking a phone
     *  that is no longer signed in to it. */
    fun unregister() {
        val tok = registered.ifEmpty { return }
        val url = Store.api("/api/push/devices") ?: return
        val req = Request.Builder().url(url).header("Authorization", "Bearer ${Store.token}")
            .delete(JSONObject().put("token", tok).toString().toRequestBody("application/json".toMediaType()))
            .build()
        registered = ""
        Store.scope.launch(Dispatchers.IO) { runCatching { http.newCall(req).execute().close() } }
    }
}

/** The wake. It carries nothing, so the only correct reaction is to go and look. */
class WakeService : FirebaseMessagingService() {
    override fun onMessageReceived(message: RemoteMessage) {
        Store.scope.launch { Store.init(applicationContext); Store.wake() }
    }

    override fun onNewToken(token: String) {
        Store.scope.launch { Store.init(applicationContext); Push.send(token) }
    }
}
