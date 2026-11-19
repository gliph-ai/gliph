# Gliph for Android

A **native Compose app** over the shared Rust core: the Android twin of
`mobile/` (iOS). The protocol, crypto, sync, offline cache, key epochs, nearby
state and transfer engine are `core/`, reached through UniFFI's Kotlin binding.
This module holds only what is per-platform:

| file | what it owns |
|---|---|
| `Store.kt` | the socket (OkHttp), SharedPreferences, notifications, files: the host side of the sans-io contract |
| `Secrets.kt` | the device key, Keystore-encrypted (the iOS Keychain's counterpart) |
| `Mesh.kt` | the nearby transport: NsdManager (`_gliph._tcp`) + TCP, the wire `Mesh.swift` and `desktop/src-tauri/src/mesh.rs` speak |
| `Push.kt` | FCM registration and the content-free wake |
| `Models.kt` | org.json readers for the core's JSON |
| `ui/` | the screens, one per SwiftUI view |

## Build

Needs the Android SDK + NDK, JDK 17, `cargo install cargo-ndk` and
`rustup target add aarch64-linux-android x86_64-linux-android`.

```sh
cd android && ./gradlew assembleDebug
```

`preBuild` runs `tools/build-core.sh`, which builds `core/` for arm64 and
x86_64 and generates the Kotlin binding into `android/core/` (never committed).
`-Pgliph.skipCore` reuses the last build when someone else has `core/` mid-edit.

A debug build has no server baked in; from the emulator the Mac is
`http://10.0.2.2:<port>`. A release build pins `https://gliph.sh`
(`-Pgliph.server=` to change it). Triple-tap the logo to reveal the field.

## Push

The server wakes the phone with a data-only FCM message (`server/fcm.go`,
`-fcm-credentials`). The app needs the Firebase project's four client values,
as Gradle properties (e.g. in `~/.gradle/gradle.properties`) or env:

```
gliph.fcm.appId=1:…:android:…      GLIPH_FCM_APP_ID
gliph.fcm.apiKey=AIza…              GLIPH_FCM_API_KEY
gliph.fcm.projectId=gliph-…         GLIPH_FCM_PROJECT_ID
gliph.fcm.senderId=…                GLIPH_FCM_SENDER_ID
```

Without them the build still works and push is off; everything else runs.

## Not in it

The same deliberate omissions as iOS: the admin console, DM peer-to-peer
transfers (WebRTC), room0 itself (the strip, marks and invites are here), and
uploads into a server channel. Release signing uses the debug key until there is
a Play upload key.
