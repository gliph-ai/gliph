# Gliph desktop (Tauri v2)

Wraps the web client in `../web`. Two build identities, so a dev app and a
production app install and run side by side without touching each other's data.

| | production | dev |
|---|---|---|
| name | Gliph | Gliph Dev |
| identifier | `chat.gliph.desktop` | `chat.gliph.desktop.dev` |
| icon | navy plate | amber plate + corner wedge |
| config | `src-tauri/tauri.conf.json` | + `src-tauri/tauri.dev.conf.json` |

A different identifier means a different app-data container, a different
Keychain entry and a different single-instance lock, so the two never collide.

## Day to day

```bash
npm run dev          # Gliph Dev against the Vite dev server on :5184
npm run dev:prod     # same, but with the production identity
```

`tauri dev` runs the binary straight out of `target/` — it never bundles and
never code-signs, so **the everyday loop does not touch the Keychain at all**.

## Installing a build locally

```bash
npm run install:app:dev   # builds and copies "Gliph Dev.app" to /Applications
npm run install:app       # same for the production identity
```

These sign with a **stable** certificate from the Keychain (preferring
Developer ID, then Apple Development). That matters: `tauri build` on its own
ad-hoc signs, and an ad-hoc signature's cdhash changes on every rebuild. macOS
ties data-protection and Keychain grants to that hash, so every rebuild looks
like a brand new app and you get re-prompted. A real certificate keeps the hash
stable and you are asked once.

Override with `APPLE_SIGNING_IDENTITY="<exact identity>"`, or `"-"` to force
ad-hoc and accept the prompting. The identity is resolved *before* the Rust
build, because a missing certificate otherwise shows up as an opaque
"failed to bundle project: failed codesign application" after a full compile.

## Unsigned bundles

```bash
npm run build        # production, ad-hoc signed
npm run build:dev    # dev identity, ad-hoc signed
```

Fine for a quick local check; use `install:app*` for anything you will launch
repeatedly.

## Notes

- Never run `npx tauri` here. `npx` resolves outside this project and has
  launched a different app's Tauri project by mistake. Use
  `./node_modules/.bin/tauri` or the npm scripts.
- The window is an Overlay-titlebar transparent window with the native traffic
  lights inset into the app's own menubar; the web client stamps
  `data-tauri` / `data-os` on `<html>` and sizes the chrome from that.
- Rust-side logs go to `~/Library/Logs/chat.gliph.desktop{,.dev}/`.

## Releasing

Every push to `main` is a release. `.github/workflows/release.yml` builds the
Mac app (Developer ID, notarized), the Windows installer and the Linux `.deb`
on GitHub-hosted runners, signs all of them for the updater, writes
`latest.json`, and uploads the lot to the `files` bucket under
`downloads/desktop/<version>/`. `bin/release-desktop` does the same from a Mac
by hand (Windows cross-compiled with cargo-xwin, Linux in Docker, see
`linux-builder.Dockerfile`). Running
copies poll `https://gliph.sh/downloads/desktop/latest.json` every 15 minutes
and grow a ★ update button in the menubar.

The version is `bin/version` — `MAJOR.MINOR` from `tauri.conf.json`, patch =
commit count — so nothing is bumped by hand; `/status` in the client prints
what everyone is running. First installs: **https://gliph.sh/downloads** lists the newest release's
installers (Mac, Windows, Linux `.deb`).

By hand, from a terminal on the Mac: `bin/release-desktop` (add `--no-publish`
to only build into `dist/desktop/<version>/`).

### The updater signing key

Updates are trusted because they are signed, not because of where they were
downloaded from. A compromised download host can serve any bytes it likes and
the app will refuse them, because it can only install what this key signed.

**The keypair already exists** (generated 2026-08-26). It lives outside the
repo, in `~/Desktop/gliph-updater-key/` (the same place inbox2 keeps its) with
a copy in `~/.gliph-updater-key/`, which is what `bin/release-desktop` reads —
macOS's Desktop privacy protection hides the original from the CI runner:

| file | what it is |
|---|---|
| `gliph.key` | the private half — signs releases, never leaves that folder |
| `gliph.key.pub` | the public half, already pasted into `src-tauri/tauri.release.conf.json` |
| `password.txt` | the passphrase protecting the private key |

Export the private half when building a release:

```bash
export TAURI_SIGNING_PRIVATE_KEY="$(cat ~/Desktop/gliph-updater-key/gliph.key)"
export TAURI_SIGNING_PRIVATE_KEY_PASSWORD="$(cat ~/Desktop/gliph-updater-key/password.txt)"
```

> **Back that folder up somewhere that is not this Mac.** Lose the private key
> or its password and no future update can ever be signed: every installed copy
> is stranded on the version it has and has to be reinstalled by hand. Losing it
> is unrecoverable — a new key does not open the doors the old one locked.

To start over with a new keypair (only ever after losing the old one — every
already-installed copy stops accepting updates):

```bash
npm run tauri signer generate -- -w ~/Desktop/gliph-updater-key/gliph.key
```

then paste the new public half into `plugins.updater.pubkey`.

The updater config lives in `tauri.release.conf.json` rather than the main
config on purpose — an unset public key then cannot break the everyday
`npm run dev` / `npm run build` loop, and only a real release has to have it
right.

### One build per OS

Tauri cannot cross-compile installers. macOS builds on a Mac, Windows on
Windows, Linux on Linux (or in a container). Each run drops its own OS's
bundles into the same `<version>/` folder and regenerates the manifest over
whatever is present, so gather the folders from each machine and re-run:

```bash
node scripts/gen-manifest.mjs --artifacts dist/desktop/<v> --version <v> \
  --base-url https://gliph.sh/downloads/desktop/<v> --mac-arch aarch64
```

A platform with no `.sig` in the folder is simply absent from the manifest,
rather than offered as a broken download.

### Three things that look like details and are not

- **`strip = false` stays in `Cargo.toml`.** The Tauri CLI embeds a
  `__TAURI_BUNDLE_TYPE` symbol the updater reads to know which installer it is
  running as. Strip it and multi-installer updates silently pick the wrong
  artifact.
- **`latest.json` is served by a handler with a 60-second TTL**, never as a
  static file. Everything else under `/downloads/` is immutable and cached for
  a year; a static manifest inherits that and every release after the first
  becomes invisible. See `downloadRoutes` in `server/http.go`.
- **A prerelease is never served on the stable channel.** `0.10.1-beta.2` is
  genuinely newer than `0.10.0` by SemVer, so ranking it would push every beta
  to everyone. `newestRelease` ignores prereleases outright.

### In the app

The app checks quietly about eight seconds after launch and emits
`update://available` if there is one. **Gliph → Check for Updates…** asks
explicitly and also reports "up to date". The menu item is disabled in the dev
identity, which builds no updater artifacts.
