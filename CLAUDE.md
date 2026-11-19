# Gliph

Text-first, E2E-encrypted, self-hostable team chat with mIRC's soul. Go server, custom protocol, MLS group encryption, web/desktop/mobile clients.

## Plans

- `docs/plans/gliph.md` — the product plan (market research, assumption verdicts, build scope).
- `docs/plans/gliph-spec.md` — the build spec. **Builds follow this spec.** When a feature lands and its acceptance criteria verify, flip its `[ ]` to `[x]` and commit the spec change with the feature. Run `/product-planner check` to audit coverage.
- `docs/plans/gliph-assumptions.md` — original framing notes.
- `design/` — mockup artboards (`.dc.html`) behind the published design canvas. Edit these files and re-seed to update the canvas; never hand-edit `gliph-mockups.html`.

## Build

- `server/` Go (`go test ./...`), `core/` Rust client core (`cargo test`), `web/` Vite+TS (`npm run build`), `desktop/` Tauri, `mobile/` native SwiftUI over the core, `plugins/` plugin SDK + examples.
- **`core/` is the client.** Protocol, crypto, sync, offline cache, key epochs, nearby state, file transfer and the blob (GUM) surface live there once — the inbox2 architecture (`docs/plans/gliph.md` #15). `web/` and `mobile/` are frontends: they own the socket, storage and pixels, and nothing else. Put new client logic in `core/`, not in a frontend.
- The core is **sans-io**: no network, storage or clock access. Entry points return `Vec<Effect>` and the host performs them. That is what makes `core/tests` possible without a server or a simulator — add a test there for any behaviour rule.
- `core/tests/webcrypto_compat.rs` opens a blob sealed by the browser's own WebCrypto. If it fails, existing accounts can no longer read their history. Never "fix" it by regenerating the fixture.
- `docs/protocol.md` is the wire contract. Change it and the clients in the same commit.
- **Every user-facing feature adds a release note** to `server/releases.json` in the same commit: a stable `id`, a `date`, a plain title and two or three sentences. Run the copy through `/voice-dna` before committing it — these notes are the product talking to everyone who uses it, and they land in the stream on the next upgrade (`docs/protocol.md` #System messages; the whole list is served at `/api/releases`). No note for a fix nobody would notice.
- Slash commands: builtins live in `server/commands.go`; plugins register theirs at runtime and the server pushes a new manifest to every client. **Clients must never hardcode a command list.**
- `/room0` (alias `/3d`) opens the channel's 3D multiplayer office (ported from inbox2) — `docs/room0.md`. Web/desktop only; `web/src/room/` + `server/room.go`.
- Dev ports: server 8787, web 5184. Workspace: `herdr-gliph`.
- `bin/dev` opens that workspace, `bin/tauri <args>` is the project-local Tauri CLI from the repo root (`bin/tauri dev`), `bin/install-app` builds and installs Gliph.app into /Applications. Never `npx tauri` — it resolves outside the project.
- **"ota install"** (or "ota deploy the app") means `bin/ota`: reinstall and relaunch the desktop app, then build and install onto the paired iPhone over Wi-Fi. `bin/ota desktop` / `bin/ota ios` for one of them.
- `plugins/e2e.mjs` drives a real server over real sockets; run it after protocol changes.
- Security scanning is Pushleft's daily source scan (Trivy, gitleaks, OpenGrep). Accepted findings live in `.security-scan-ignore` (path, reason) and `// nosemgrep: <rule>` comments with a reason. The web client renders by assigning templates to `innerHTML`: every wire-sourced value goes through `esc()` (`web/src/escape.ts`), message bodies through `renderInline()`, and `npm run check` (`web/scripts/check-render.mjs`) holds that line.

## Prod

- Live at **https://gliph.sh** on Pushleft (`.pl.yaml`, org `empire`). `docs/deploy.md` has the release loop, the DNS records and what is deliberately not deployed.
- State is Pushleft PostgreSQL plus the `files` bucket (uploads, voice notes, desktop releases); no volume. A self-host still runs on SQLite in `-data`. Keep one web instance: Append's seq and the live fan-out assume a single writer process.
- A prod build pins its server (`GLIPH_SERVER=https://gliph.sh`) and hides the login screen's server field; **triple-tap the logo to reveal it**.
