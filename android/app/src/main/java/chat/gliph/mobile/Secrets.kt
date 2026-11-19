//  The device key store: the Android counterpart of mobile/Sources/Keychain.swift.
//
//  One value goes in here: the device's private scalar (core/src/crypto.rs). It
//  is encrypted under a Keystore key that never leaves this phone, and the
//  manifest turns backup off, so a phone restored onto another device does not
//  carry the ability to read a channel with it.

package chat.gliph.mobile

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

@Suppress("DEPRECATION") // ponytail: security-crypto is deprecated but works; a hand-rolled Keystore AES wrapper if it breaks
object Secrets {
    private var prefs: SharedPreferences? = null

    fun init(ctx: Context) {
        val key = MasterKey.Builder(ctx).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build()
        prefs = EncryptedSharedPreferences.create(
            ctx, "gliph.secret", key,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    fun read(key: String): String? = prefs?.getString(key, null)
    fun write(key: String, value: String) { prefs?.edit()?.putString(key, value)?.apply() }
    fun delete(key: String) { prefs?.edit()?.remove(key)?.apply() }
}
