# Gliph for iOS

A **native SwiftUI app** over the shared Rust core. There is no webview and no
second implementation of anything: the protocol, the crypto, the sync, the
offline cache, the key epochs, the nearby state and the file-transfer engine all
live in `core/`, the same crate the web client runs as wasm.

What is in this target is only the parts of a client that are genuinely
per-platform:

| file | what it owns |
|---|---|
| `Sources/Store.swift` | the socket, the Keychain, UserDefaults, notifications — the host side of the sans-io contract |
| `Sources/Mesh.swift` | the nearby transport: Bonjour, plus Wi-Fi Aware on iOS 26+, and TCP over Network.framework |
| `Sources/P2P.swift` | DM file transfers over WebRTC (the `WebRTC` Swift package), the twin of `web/src/p2p.ts` |
| `Sources/Push.swift` | APNs registration; the notification itself is written locally after decrypting |
| `Sources/Models.swift` | Codable mirrors of the core's JSON |
| `Sources/Views/` | the screens, including `BlobView` — the SwiftUI painter for the core's GUM tree |

The loop is always three lines: call into the core, run the `Effect`s it returns,
publish a fresh snapshot. Nothing else changes state.

## Build

```sh
brew install xcodegen                # the .xcodeproj is generated, not committed
rustup target add aarch64-apple-ios aarch64-apple-ios-sim
cd mobile && xcodegen generate && open Gliph.xcodeproj
```

The Rust core and its Swift bindings are built by a pre-build phase
(`tools/build-core.sh`), so a change to `core/` is picked up without a separate
step. One caveat: Xcode plans a build before running that phase, so a core
change that **adds a method** needs Cmd-R twice — the first build compiles the
new Swift against the old library. The deploy scripts run the script explicitly
first, so a release build never hits it. Nothing generated is committed: a
checked-in binding that has to be regenerated whenever `core/` changes is one
that goes stale unnoticed, and this one holds the encryption.

A dev build has no server baked in; the sign-in screen asks. From the simulator
the Mac's own server is `http://localhost:8787`.

## What is not in it

Deliberate omissions, not oversights:

- **The admin console.** A separate page of the web bundle, and it opens in
  Safari against the server, which is where an admin already signs in.
- **Android.** There was never an Android project. The core already builds for
  it; what is missing is this directory's worth of Kotlin, plus a `NsdManager`
  twin of `Mesh.swift`.

## Nearby mesh

`Sources/Mesh.swift` registers and browses `_gliph._tcp` and holds one TCP link
per peer — the same wire `desktop/src-tauri/src/mesh.rs` speaks, so a Mac and an
iPhone land in one mesh. `docs/protocol.md` ("Nearby mesh") is the contract.

`Info.plist` carries `NSLocalNetworkUsageDescription` and `NSBonjourServices`;
without them iOS returns an empty browse and says nothing about why.

On iOS 26+ the same service also runs over Wi-Fi Aware (`WiFiAwareServices` in
`Info.plist`, the `com.apple.developer.wifi-aware` entitlement), so two paired
phones meet with no shared network. Pair them once in Settings > Phones nearby:
"find a phone" on one, "be found" on the other. The simulator has no Wi-Fi
Aware radio; test it on two phones in nearby mode with Wi-Fi off the network
(Wi-Fi itself stays on).

To check it against a Mac: put the phone into nearby mode (sign-in screen, "chat
with people nearby"), then `cd desktop/src-tauri && cargo test --lib --
--ignored --nocapture join_the_real_mesh`. mDNS is a cache — an empty first
browse is not necessarily a broken one.

## Shipping

Both scripts pin the build to prod (it lands in `Info.plist` as `GliphServer`)
because the app has no page origin to fall back on. **Triple-tap the logo** on
the sign-in screen to reveal the server field in a pinned build.

```sh
mobile/tools/deploy-device.sh     # development build, over Wi-Fi, no review queue
mobile/tools/testflight.sh        # signed App Store build, uploaded
mobile/tools/testflight.sh export # stop after the .ipa
```

Signing is automatic against team `KF53B4S5HD` using the App Store Connect API
key in `~/.appstoreconnect/private_keys/`. The one thing the API refuses is
creating the **app record** — a one-time form at appstoreconnect.apple.com (iOS,
bundle id `chat.gliph.mobile`; the bundle id itself is registered, `64DDQ3Q3Z8`).

`tools/icons.py` regenerates the icon and launch image from
`assets/icon-source.png`.

## Verified

Built and driven end to end on an iPhone 17 Pro simulator against a real server:
sign in, connect, the rail, `/join`, and — the one that matters — a message the
**web client** encrypted in a `+e` channel, decrypted and rendered here. Two
different hosts of one core, in one encrypted channel.

Note that an unsigned simulator build cannot use the Keychain
(`-34018: no application-identifier entitlement`), so the device key is minted
fresh on each launch there. Signed builds are unaffected.
