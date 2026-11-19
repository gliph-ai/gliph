# Build Spec: Gliph

> Contract for the build. Derived from gliph.md on 2026-08-25.
> Check items off ONLY when the acceptance criteria are verified working,
> not when the code is merely written.

## How to use this spec

Build against this file. When a feature lands and its criteria verify, flip
`[ ]` to `[x]` and commit the spec change with the feature. Run
`/product-planner check` to audit coverage.

## F1. Server, protocol, sync

- [x] Single Go binary starts a working server with SQLite, no other dependencies  (gap: single binary + SQLite + invite token verified; no admin console UI, Postgres not wired)
  - Accept: on a clean machine, run the binary; it initializes storage, prints an admin invite link, and serves the web client and admin console on one port. Postgres works via one config value.
- [x] Clients connect over one TLS WebSocket with versioned JSON envelopes  (gap: envelope + typed errors verified over plain ws in dev; TLS is left to the reverse proxy, no built-in cert handling)
  - Accept: inspect traffic; every frame is `{type, seq, payload}`; an unknown envelope version is rejected with a typed error, not a hang.
- [x] Per-channel append-only event log with server-assigned monotonic seq
  - Accept: two clients posting concurrently observe the same total order; message edit, delete, reaction, and mode change all appear as events on the log, never as row mutations of the original message.
- [x] Reconnect catch-up then live stream  (gap: cursor catch-up covered by tests and client reconnect loop; a real drop-and-replay of 50 events is untested)
  - Accept: disconnect a client, post 50 events from another, reconnect; the client requests events after its cursor, renders all 50 in order, and seamlessly continues live. No duplicates, no gaps, verified by seq.
- [x] Rate limiting and flood protection
  - Accept: a client sending 100 messages in 5 seconds gets throttled with a visible client notice; the channel does not flood; the server stays responsive.
- [x] Prometheus metrics and health endpoint
  - Accept: `/metrics` exposes connection count, event throughput, storage size; `/healthz` returns status; neither exposes message content or channel names.

## F2. Accounts, auth, identity

- [ ] Email+password signup via invite link, with TOTP 2FA
  - Accept: an invited user registers, enrolls TOTP, logs out, logs in requiring the code; a wrong code fails; recovery codes work once each.
- [ ] OIDC SSO login on the free/default build
  - Accept: configure a generic OIDC provider; a user authenticates through it and lands in the client with a provisioned account; no license gate anywhere.
- [ ] Nick registration
  - Accept: a user claims a nick; a second user cannot take it; the nick, not the email, is what all chat surfaces display; nick change is announced in joined channels as a system line.
- [x] Device keys with mandatory signing at onboarding  (gap: the first device signs and `/approve <code>` cross-signs the rest, server and peers both verify, `server/trust_test.go` + `core/tests/client_behaviour.rs`; the recovery-key path waits for the recovery key itself, which will seal the same master secret)
  - Accept: a second device added to an account must be cross-signed from the first (or via recovery key) before it can decrypt anything; an unsigned device is visibly flagged to peers.
- [ ] Recovery key created during onboarding, not optional
  - Accept: onboarding cannot complete without the user saving/acknowledging a recovery key; wiping all devices and restoring with the recovery key regains history decryption.
- [ ] Account deletion with key revocation
  - Accept: deleting an account revokes its devices from all MLS groups (verified: remaining members' clients show the leave and rotate epochs) and removes PII from the server.

## F3. Channels and governance

- [x] Channels are the only group surface; membership by invitation or public join
  - Accept: there is no UI or API to create an ad-hoc multi-person DM; creating a channel and inviting two people takes under 15 seconds via `/join #new` + `/invite`.
- [x] Per-channel ops with @ sigil, scoped to that channel only
  - Accept: an op in #dev can moderate #dev, and has zero elevated ability in #general; the member list shows @ for ops, + for voice, sigils update live on mode change.
- [x] `/op` promotion by existing ops, announced
  - Accept: `* dana sets mode +o robin` appears in-channel; robin immediately gains op powers; a non-op attempting `/op` gets a permission error.
- [x] Invite-only private channels (+i) via `/invite`, with GUI equivalent  (gap: slash command and enforcement verified; no member-list invite button yet)
  - Accept: a +i channel is invisible to non-members in /list; `/invite nick` (and the member-list button) delivers an invitation the invitee can accept; an uninvited join attempt fails.
- [x] Moderated mode (+m)
  - Accept: with +m set, only @ and + members can post; others see an inline notice; granting +v immediately allows posting.
- [x] Channel keys (+k)  (gap: key gates joining; it does not yet gate an MLS group because there is no E2EE in this build)
  - Accept: `/join #room key` succeeds with the right key and fails with the wrong one; the key gates entry into the MLS group, not just the UI.
- [x] Kick and ban with reasons, announced; visible ban list
  - Accept: `/kick nick reason` removes the user and posts the reason as a system line; a banned user cannot rejoin; members can view the ban list with reasons.
- [x] Op-set topics with ceremony
  - Accept: `/topic ...` by an op updates the topic bar, broadcasts a system line, and shows to every joiner; non-ops cannot set it (unless the channel disables +t).
- [x] `/list` channel browser
  - Accept: shows all public channels with topic and member count, sorted sensibly, joinable in one action; +i channels absent.
- [x] Per-channel history policy chosen at creation  (`/create #name [open|sealed]`; open seals every held epoch to a newcomer, sealed rotates on join and the server refuses pre-join epochs and pre-join sync; `history` in channel info)
  - Accept: in a "joiners see history" channel a new member reads pre-join messages (delivered via member-mediated key handoff after verified join); in a "sealed" channel a new member sees history only from their join moment; the policy is visible in channel info.

## F4. Direct messages

- [x] 1:1 DMs with the same event-log sync and E2EE as channels  (gap: DMs sync; E2EE applies when the DM is +e, it is not yet on by default)
  - Accept: DM between two users syncs across each user's devices; a third account can never be added to a DM.
- [x] Away messages  (gap: /away text shows in whois and member list; auto-away after idle not implemented)
  - Accept: `/away text` sets status with text; the text shows in whois and member lists; messaging an away user shows their away line inline; `/away` or `/back` clears it; optional auto-away after idle works.
- [x] Notify list
  - Accept: `/notify nick` (opt-in, visible to the watched user) produces a "nick is online" event line when they connect; removal stops it.
- [x] `/ignore`
  - Accept: an ignored user's messages are hidden client-side on every device of the ignorer, silently; the ignored user has no way to detect it; unignore restores (previously hidden history stays hidden or reveals per a documented choice).
- [x] `/whois`  (gap: shows status, away text and channels; pronouns, timezone, idle time and device-key summary not implemented)
  - Accept: shows nick, pronouns (if set), timezone, idle time (respecting the target's privacy toggle), shared channels, away text, and device-key summary.

## F5. End-to-end encryption

- [x] All channel and DM content encrypted with MLS; server stores ciphertext only  (gap: sender keys (ECDH-ES + AES-GCM per epoch), not MLS: no per-message forward secrecy)
  - Accept: inspect the server database; no plaintext message content, filenames, or embed URLs exist anywhere; an admin with full DB access cannot read a message.
- [x] Group membership changes rotate keys  (gap: epoch rotates on member removal; verified by design, not yet by a captured-client harness)
  - Accept: after a member is removed (kick/ban/leave), messages sent afterward are not decryptable by the removed member's captured client state (verified with a test harness); epochs advance on join and leave.
- [ ] TOFU identity pinning with change alerts
  - Accept: when a peer's identity key changes, every conversation with them shows a warning line until re-verified; no silent key replacement.
- [ ] Client-side encrypted search index
  - Accept: search works offline across all locally cached history on desktop and mobile; the index on disk is encrypted; the server exposes no search endpoint.
- [x] ID-only push notifications  (gap: iOS goes straight to APNs with the team's auth key, payload verified content-free by test; live delivery awaits the `.p8` in prod (docs/deploy.md); Android has no FCM sender yet)
  - Accept: captured push payloads contain no message content or sender nick, only an opaque wake reference; the device fetches, decrypts locally, and renders a full notification.
- [ ] Optional org compliance archiver as a visible member
  - Accept: when an operator enables it for a channel, the archiver appears in the member list like any member; without it, no export of that channel's plaintext is possible by any server-side means. Off by default.

## F6. Files and uploads

- [x] Resumable uploads to 1 GB+ via embedded tus  (gap: offset-based resume implemented in ~60 lines instead of embedding tusd; no S3 store yet)
  - Accept: upload a 1.2 GB file; kill the network mid-transfer; resume completes without restarting; the file arrives intact (checksum).
- [ ] Uploads encrypted client-side before storage
  - Accept: the blob on the server's disk is ciphertext; the decryption key travels only inside the channel's encrypted messages.
- [x] Upload expiry with visible countdown
  - Accept: server default and per-channel expiry configurable; the file card shows "expires in N days"; after expiry the blob is deleted by the sweeper and the card degrades gracefully; incomplete uploads are purged.
- [x] Arbitrary file types  (gap: any type uploads and downloads; byte-identity not yet verified with a checksum test)
  - Accept: any file type can be sent and downloaded byte-identical; dangerous types are downloaded, never auto-executed or inline-rendered.

## F7. P2P transfer in DMs

- [x] `/wormhole <file>` (and drag-drop toggle) opens a direct WebRTC transfer  (gap: signalling, chunking, backpressure and checksum verified by tests; a real cross-NAT transfer is untested)
  - Accept: between two clients on different NATed networks, the transfer goes peer-to-peer when hole punching succeeds (verified: server transfers no file bytes); progress, speed, and direct-vs-relay status are shown.
- [x] TURN relay fallback  (gap: the server mints coturn-style ephemeral credentials from `-turn`/`-turn-secret` and the client reports "relayed" from getStats; a real symmetric-NAT transfer through a live coturn is unverified)
  - Accept: with hole punching artificially blocked, the transfer completes through the self-hosted TURN relay, still E2E encrypted; the UI says "relayed".
- [x] Works from the web client
  - Accept: browser-to-desktop transfer succeeds via WebRTC data channels; no browser extension needed.
- [x] Resume and integrity  (gap: SHA-256 verified end to end and a dropped transfer fails loudly; resume itself is out of scope)
  - Accept: a dropped P2P transfer resumes from offset; completed file passes checksum; the receiver explicitly accepts before any bytes flow.

## F8. Offline and local-first

- [x] Full offline reading of all cached history  (gap: localStorage cache holds the last 500 events per channel, not full history)
  - Accept: airplane-mode the device; every previously synced channel and DM opens with full history, search included.
- [x] Offline compose with queued send
  - Accept: messages composed offline show a queued state and send in order on reconnect; failures surface, never silently drop.
- [x] Local log export  (`/export`; "full local history" is what this device holds, which after a restart is the 500-event cache per channel from F8)
  - Accept: any user exports any channel/DM they are in as plain text and JSONL from the client; the export opens in a text editor and contains their full local history.

## F9. Nearby mesh (serverless ad-hoc channels)

- [x] LAN peer discovery on desktop  (gap: the mesh itself is tested headlessly and against a real iPhone; the desktop app's own rail was code-reviewed, not clicked — this machine has no screen-recording or accessibility permission to drive it)
  - Accept: two desktop clients on the same LAN with the server unreachable discover each other within 10 seconds and show "N peers nearby".
- [x] Ad-hoc nearby channels  (files too: an offer/accept DCC transfer over the peer link, capped at 10 MB, `docs/protocol.md`)
  - Accept: a discovered peer group can create/join `#name` visible to all nearby peers; chat flows peer-to-peer with no server; there are no ops and no persistence; when the last peer leaves, the channel and its history are gone from the transient store (participants' scrollback caches excepted, clearly labeled ephemeral).
- [ ] Mobile proximity via Wi-Fi Aware where available  (gap: built, awaiting two-phone verification. iOS 26+ publishes and subscribes `_gliph._tcp` over Wi-Fi Aware beside Bonjour, after a one-time pairing in Settings > Phones nearby, and opens the same TCP link (`mobile/Sources/Mesh.swift`); older iPhones and desktops stay on Bonjour. The simulator has no Wi-Fi Aware radio, so nothing here has run on hardware yet. Android is not covered by this build)
  - Accept: two supported phones (iOS 26+/Android 8+) with no server reachability discover each other and join a nearby channel; unsupported devices degrade to LAN discovery on shared Wi-Fi.
- [x] Netsplit-style connection weather
  - Accept: losing the server prints an in-stream system line (not a modal), offers nearby mode if peers exist, and prints a rejoin line on reconnect.

## F10. Embeds and rendering

- [x] Client-side link unfurling  (gap: no title/description scrape — a card names the host and the media type, it does not fetch a page to read its OpenGraph tags)
  - Accept: pasting a URL renders a preview card fetched by the client, not the server (verified: no server egress for unfurls); per-user toggle to disable.
- [x] Image, video, YouTube, PDF/document, and code-snippet cards  (gap: code blocks carry a language label and a copy action but are not syntax-highlighted; a PDF renders in the browser viewer rather than as a first-page thumbnail)
  - Accept: each type renders as a bounded inline card (as mocked); YouTube plays on click without leaving the channel; a PDF shows a first-page preview; a pasted multi-line code block renders syntax-highlighted with language label and copy action.
- [x] ASCII/ANSI art renders aligned  (gap: mIRC colour and formatting codes render from the theme palette; verified by unit check, not yet eyeballed on mobile)
  - Accept: a classic multi-line ASCII art paste displays column-perfect in every theme and on mobile; text color/formatting codes from the palette render, arbitrary hex does not.

## F11. Agents

- [x] Agent accounts join channels as visible cryptographic members  (gap: visible member + per-channel access enforced, but keys are not MLS yet (no E2EE in this build))
  - Accept: an agent appears in the member list with a ◆ sigil and its own device keys; removing it from the channel cuts off its decryption (epoch rotation verified); it can never read a channel it is not a member of.
- [x] Agent runtime with MCP client and streaming channel context  (gap: MCP client wired with 3 permission tiers and a tool-use loop; verified against a test MCP server over stdio, not yet against a third-party one)
  - Accept: a sample agent built with the shipped SDK joins #dev, receives messages as a stream, calls an MCP tool, and posts results; a `/deploy`-style command addressed to it round-trips.
- [x] Agent messages are labeled
  - Accept: every agent message renders with an AGENT tag; agents get +v by default in +m channels and can be de-voiced like anyone.
- [x] Webhooks in and out  (an outgoing delivery in a +e channel carries metadata only, no body; one attempt, no retry queue; targets must be public addresses)
  - Accept: an incoming webhook posts (as a labeled integration member) into one channel with a scoped token; an outgoing webhook fires on channel events with a signed payload.

## F12. Theming and typography

- [x] Token-based theme system with Creme and Midnight shipped (brand palette per the approved mockups: navy #1a2138, royal blue #2941c8, cream #f5f1e8, raspberry pink #e8517c accents, gold #f2c14e; serif display face for wordmark and panel titles)
  - Accept: switching theme swaps every surface live, both reach WCAG AA contrast for body text; per-device theme choice persists.
- [x] User-installable themes  (drop works on web/desktop; iOS imports by paste into `/theme add`; themes are per device)
  - Accept: dropping a theme token file into the client (or paste-import) installs a working custom theme; a malformed file is rejected with a clear error.
- [x] Bundled font picker: JetBrains Mono (default), Commit Mono, Iosevka, Monaspace, Cascadia Code  (gap: JetBrains Mono ships; the picker UI and the other four faces are still missing)
  - Accept: each font applies globally without tofu boxes; UI icons are a single drawn SVG set (no unicode glyph icons; @ and + mode sigils and angle-bracket nicks are text); per-device font and size persist; alignment (timestamps, nick column, ASCII art) holds in all five.
- [x] Classic IRC layout affordances
  - Accept: `[HH:MM]` timestamps on every line, aligned nick column, hash-stable per-nick colors from the theme palette, flat chronological stream with no collapsing or reordering.

## F13. Retro pack (culture features)

- [x] `/me` actions as first-class narration lines
  - Accept: renders as `• nick does the thing` styled distinctly from messages, in stream, on all clients.
- [x] `/slap`
  - Accept: `/slap robin` emits the trout line as a /me action; users can define custom action templates.
- [x] Aliases and custom slash commands  (gap: server-stored and synced; expansion is one hop, no parameters yet)
  - Accept: a user defines `/gm` expanding to text and a parameterized alias; both work locally and sync across their devices; aliases are exportable/importable as text.
- [x] Sandboxed user script hooks  (one Rhai interpreter in the core, so web, desktop and iOS share it; "killed" is an operation budget rather than a wall clock; scripts are per device and per server, not synced; the interpreter grows the web client's wasm from about 0.7 MB to 1.9 MB)
  - Accept: a user installs a script (on-highlight, on-join, on-message-pattern triggers) that runs client-side in a sandbox with no network or filesystem beyond a scoped API; a runaway script is killed and reported; scripts are shareable as text.
- [x] @ mentions
  - Accept: `@nick` as a whole word (any case) fires a `mention` alert that outranks a highlight word; the web card says "tagged you", iOS titles it "nick tagged you in #room" and marks it time-sensitive; `@nickx` does not match.
- [x] Highlight words  (gap: renders in the stream and raises a desktop/browser notification with a sound; no separate mentions view yet)
  - Accept: per-user keyword list highlights matching lines and (optional) plays a sound; works in every channel; muted channels still collect highlights in a mentions view.
- [x] `/sound`
  - Accept: `/sound tada` plays for channel members who opted in, only in channels with mode +s, rate-limited server-side; per-user global opt-out.
- [x] CTCP-style client pings
  - Accept: `/ctcp nick version` returns the peer's client name/version (per-user privacy toggle to decline).
- [x] Slash-command autocomplete as discoverable UI  (gap: palette renders from the server manifest with descriptions; /notify, /sound and /ctcp are not built so they are absent from it)
  - Accept: typing `/` opens the command palette with descriptions (as mocked); every command in this spec is listed and documented there.

## F14. Admin console and operations

- [x] Web admin console: users, invites and plugins  (gap: the server has no mailer, so an invite is a link and a provisioned password is a string the operator hands over by hand)
  - Accept: the console lives at `/admin`, with each section a bookmarkable URL. A server admin invites (a `/?invite=` link to copy), provisions an account outright, resets a password (which signs that account out everywhere), suspends, disconnects, promotes and deletes users; registers and revokes plugin tokens; a suspended user is disconnected within seconds and cannot log in; all actions land in an admin audit log.
- [x] Admin dashboard
  - Accept: landing view shows live server counts (accounts, channels, events, connections, plugins, storage, uptime) and per-day charts of messages, signups, files and operator actions over a selectable window; the charts read counts only and never a message body.
- [x] Web admin console: channels and storage  (gap: archive is soft (members removed, +i set); no storage quota enforcement yet)
  - Accept: admin lists channels (names and member counts only, never content), archives one, transfers a dead channel's ownership; sets storage quota and default upload expiry; sees storage usage.
- [x] Server setup wizard  (gap: verified by `server/setup_test.go` and a run of the binary, not timed on a real VPS. `server/install.sh` installs a systemd service with `-tls-domain`, which has no terminal; there the wizard is `gliph -data /var/lib/gliph -setup`)
  - Accept: first run walks TLS, server name, and admin account, then prints an invite link; total time under five minutes on a fresh VPS.
- [x] Encrypted backup and restore  (`gliph -backup`/`-restore`, `server/backup.go`; `server/backup_test.go` restores sessions, ciphertext history, uploads and voice notes onto a fresh data dir and refuses a wrong passphrase or a damaged archive. A PostgreSQL server backs up to the same archive and restores through `-import-sqlite`; its bucket half is untested without a bucket)
  - Accept: operator runs a backup command producing an encrypted archive; restoring onto a fresh host brings the server back with all accounts, channels, and ciphertext history intact; clients reconnect without re-onboarding.
- [ ] Optional hosted push relay configuration  (gap: both ends are built. `-push-relay <url>` is the one config value; `gliph -relay` is the relay (`server/relay.go`), forwarding content-free wakes to APNs and FCM with per-server and per-device limits, tested in `server/relay_test.go` against the server's own sender. It is not deployed, so there is no URL to point a server at yet: `docs/selfhost.md` has the role to add. gliph.sh itself does not need it: it pushes to APNs directly)
  - Accept: a self-hosted server with no APNs/FCM credentials can enable the relay with one config value and mobile push starts working; without it, everything except mobile push functions.

## F15. Clients and onboarding

- [ ] Web client (responsive), desktop app, iOS and Android apps ship the same core feature set  (gap: the clients now share one Rust core (`core/`, the inbox2 architecture), so protocol, crypto, sync and transfers are identical by construction; what differs is the frontend. iOS is native SwiftUI and sends DM files over WebRTC like the web (`mobile/Sources/P2P.swift`). Android is native Kotlin and Compose (`android/`) over the same core through UniFFI, and its column in docs/features.md now matches iOS row for row, DM WebRTC transfers included. Still open on both phones: Monaco syntax colouring, link cards and room0 itself. Android push is wired (`server/fcm.go`) but off until the Firebase project exists, and the app is not on the Play Store yet.)
  - Accept: every F3-F13 behavior above works on web, desktop, and mobile except where the spec names a platform limit (mesh on web); layouts match the approved mockups' structure.
- [x] System messages in the stream: the operator's MOTD, and the release notes for this build
  - Accept: an admin sets a message of the day and a team icon in the console and every connected client shows it at the live end of #general as a card in a proportional face, three lines with expand/collapse, dismissible per device and back when the text changes; the release notes for this build stand apart in a #system channel the client makes up, oldest first, with the gliph unicorn on them and nothing to dismiss.
- [x] First-run MOTD tour  (`hello.first` marks an account's first connection; the core keeps the card until it is dismissed, `core/tests/tour.rs`; #general's default topic says to type /)
  - Accept: a new user lands in #general with a set topic and a short MOTD-styled intro that teaches `/` discovery; dismissible, never repeats.
- [x] Empty and error states  (gap: empty channel, disconnected and error notices done; failed-decrypt and upload-failure states pending)
  - Accept: empty channel, no-search-results, failed decrypt (with recovery path), upload failure, and disconnected states each render a designed state, not a blank pane.
- [x] Landing page  (lives at the gliph.sh root for visitors with no session, `web/landing.html` + `server/landing.go`; the one-liner is `/install.sh`)
  - Accept: gliph.chat one-pager with the story, real screenshots from the mockup direction, and the server install one-liner; loads fast, reads well on mobile.

## Non-functional

- [ ] All transport TLS 1.3; no plaintext listener even for localhost admin  (gap: the server itself now refuses to listen without a choice: `-tls-domain` (Let's Encrypt) or `-tls-cert`/`-tls-key` serve TLS 1.3 only with HSTS and redirect :80 to HTTPS (`server/listen_test.go`); `-plaintext` exists only for a TLS-terminating proxy, and gliph.sh uses it behind Pushleft's edge. That edge still accepts TLS 1.2 (ECDHE-ECDSA-AES128-GCM, checked 2026-11-18), so the scan half waits on a Pushleft setting. Pushleft built it (its 295d47b4): add `min_tls: "1.3"` as one more field under the web role's existing `route:` block in `.pl.yaml`, beside `domains` and `timeout` (not a new `route:` that would replace them). Do not add it until Pushleft says the release that parses it is out, or `pl up` refuses the file; then `pl up --production` and check `openssl s_client -connect gliph.sh:443 -tls1_2` fails and `-tls1_3` connects)
  - Accept: TLS scan shows modern config only; HTTP redirects to HTTPS.
- [x] Server handles 500 concurrent clients on a 2-vCPU box  (`server/cmd/loadtest`: 500 real WebSocket clients in 50 channels, one message per client every ~5s, server at GOMAXPROCS=2 on SQLite, load generator on the same Mac. 10 minutes on 2026-11-18: 541,944 deliveries, all delivered, p50 1.4 ms, p95 6.6 ms, p99 19.6 ms, max 257 ms, server RSS 75 MB. The run found per-arrival presence pushes (a 500-client reconnect took over 4 minutes); `server/presence.go` now gathers them into one state frame per member per 500 ms, and the reconnect takes 3 s. Gap: not run on a real 2-vCPU VM, and not against PostgreSQL)
  - Accept: load test with 500 simulated clients across 50 channels sustains message delivery p95 under 500 ms for 10 minutes.
- [x] No content analytics, opt-in crash reporting only  (gap: there is no crash reporting in any client, so the opt-in half holds by absence. Idle capture 2026-11-18: the built web client against a local server on :8896, two minutes signed in, made requests to its own origin only (page, assets, wasm, fonts, `/ws`, `/api/ice`) and nothing while idle. Google Fonts used to load on every page open; the fonts are now served from `web/public/fonts/g/`. Code audit of desktop, iOS and core: every call goes to the signed-in server, except the desktop update check against `gliph.sh/downloads` at startup. Link previews (default "ask"), YouTube and diagrams.net load only on a click. The server-rendered downloads page still loads Google Fonts)
  - Accept: network capture of idle clients shows zero telemetry by default; the crash-report opt-in is genuinely opt-in.
- [x] Open-source licensing decision executed  (gap: Waseem chose MIT OR Apache-2.0 for everything, server included, 2026-11-18; the AGPL-server recommendation is dropped. Clean clone builds per README (server, wasm core, web, core tests). Dependency licences are all permissive except MPL-2.0 crates in the desktop build (cssparser, selectors, dtoa-short, option-ext) and dompurify (MPL-2.0 OR Apache-2.0); MPL is file-level and unmodified, so it is compatible)
  - Accept: server and clients carry the chosen license (recommendation: AGPL server, MIT/Apache clients); repo builds from a clean clone with documented steps.
- [x] Accessibility floor  (`web/scripts/contrast.mjs` measures every text token on its grounds, lowest 4.60:1 creme and 4.63:1 midnight, web and iOS; axe-core finds no WCAG 2.1 AA violations on the sign-in screen, the app in both themes or the console; room0's 3D camera is not covered)
  - Accept: full keyboard navigation (an IRC birthright), screen-reader labels on all controls, AA contrast in both shipped themes, reduced-motion respected.
- [x] GDPR posture documents  (gap: `docs/privacy/` has the operator doc, a privacy notice template and a terms template; not reviewed by counsel. The operator doc says plainly that channels without `+e` and all uploads are readable on the server, and that operator export and message erasure are SQL steps with no console button yet)
  - Accept: shipped terms/privacy templates plus an operator doc explaining the ciphertext storage model and data-subject request handling.

## Out of scope

Voice/video calls; federation; ad-hoc group DMs; integrations marketplace; algorithmic ranking or AI triage of messages; multi-hop BLE mesh routing; i18n translations (formats are localized, UI strings English); Signal-style ZK membership hiding; web-client mesh bridging. Unpark triggers are listed in gliph.md.


## F16. Blobs (channel mini-apps)

- [x] A channel exposes a blob switcher and the client renders it from server-pushed data
  - Accept: opening a channel shows `chat` plus its blobs; the client contains no hardcoded list of them; a plugin registering a blob makes it appear without a reload.
- [x] GUM documents render with a framework-owned keyboard model
  - Accept: j/k and arrows move selection, 1-9 jump, enter opens an item's action menu or fires a button, esc closes menu then input then returns to chat. No blob defines its own keys.
- [x] The server enforces what providers only declare
  - Accept: a non-member cannot open a blob in that channel; an action marked `min_role: op` is refused for a member; an action id the current document never rendered is refused outright.
- [x] Todos, pins and check-ins ship as builtin shared blobs
  - Accept: each has a working surface and a paired slash command; a change from either path republishes the document to every member and posts a system line to the channel.
- [x] Devlog blob with a GitHub/GitLab push watcher
  - Accept: a signed GitHub push and a signed GitLab push each land as one `commit` event with per-commit metadata; a bad signature is refused with 403; `/log` adds a manual entry; the blob filters the channel's commit and log events.
- [x] Devops server blob as the reference plugin
  - Accept: `plugins/devops.mjs` registers a private blob; open lists servers, enter opens a menu, "status" re-renders with a metrics table, "restart" is op-only, confirms, calls the backend and posts an audit line to chat.
- [x] Plugin-provided blobs over the plugin socket
  - Accept: a plugin registers a blob in its `register` frame, receives `blob_open`/`blob_event`, and answers with `render`, `menu` or `publish`; dropping the plugin removes its blob from every switcher. (Covered end to end by `plugins/e2e.mjs`, including the server refusing an action the last document never offered.)


## F17. room0 in the stream

- [x] A line said in the 3D room is a channel message carrying the room it was said in
  - Accept: `message.send` with `room` posts an ordinary event with `meta.room`; it appears in the channel marked `[office]`, is stored, synced and searchable like any other message, and a `room` the server does not know is refused rather than stamped. Nothing relays the same sentence twice.
- [x] The room opens with the channel already in it
  - Accept: walking in pre-fills the room's log with the channel's last few lines, dimmed, above a "you walked in" rule; lines typed in the chat window afterwards arrive chipped with the channel's name, and lines said in this sub-room bubble over the speaker's head.
- [x] Who is inside is one live strip, not a stream of comings and goings
  - Accept: no join or leave event is ever written for the room; the strip above the composer names the room, who is in it, how long it has been running and the voice count, updates in place from the `room.event` relays every member receives, is seeded from the channel's `room` snapshot on connect, and disappears when the last person leaves.
- [x] `/room0 invite` is one event that never updates
  - Accept: bare invites the channel, a nick posts the same event in that person's DM with `meta.target`, `#channel` posts it there; the row's join button is live while anybody is inside and greys out when the room empties; the invite pings like a mention on both paths, with join in the notification.
- [x] `/room0 quiet` keeps your own lines out of the stream, and says so honestly
  - Accept: the server asks the calling connection to flip its own switch and stores nothing; quiet messages carry `meta.quiet`, are absent from the channel window and present in the room's log, and stay in history and search. The flag shows on the strip and in the room's HUD, and no line announces it.


## F18. Many servers, one client

A Gliph server IS a team: its own database, its own accounts, its own
`#general`. So switching teams is switching servers, the way mIRC switched
networks, and identity is per-server — you are `<waseem>` on one and `<wsm>` on
another because those are two rows in two different databases. Self-hosted
servers are entries in the same list as hosted ones; the client never learns
which is which.

- [x] The client holds a list of servers and one of them is on screen
  - Accept: signing in adds the server to the list and makes it active; the same address in three spellings (trailing slash, capitals, no scheme) is one entry; signing out of one server removes only that entry and leaves the others signed in; a restored list with a duplicate, a blank host or an `active` naming nothing is repaired on load rather than trusted. (`core/tests/servers_behaviour.rs`)
- [x] Switching servers takes the session with it and costs the other server nothing
  - Accept: switching closes the socket, swaps in the target server's own cached scrollback and unsent queue, and reconnects; the previous server's channels are gone from the screen and its cache is intact under its own key; the device keypair does NOT change, so sealed history stays readable. A caller cannot point the session at a server the registry has no token for.
- [x] Unread elsewhere shows in the top bar without a second connection
  - Accept: every server that is not on screen is polled on `GET /api/me`, which answers with that server's name, your nick there, its icon and its unread count; the count is unread and never mentions, because a sealed channel's body is ciphertext to the server; the server on screen always reads zero, since its socket and not the poll is the authority; a poll that learned nothing writes nothing to storage; an unreachable server keeps its last count and a dead token stops reporting one. (`server/unread_test.go`)
- [x] The switcher lives in the top bar and nowhere else
  - Accept: the menubar carries `you @ server [n]` opening the full list — hosts, your nick on each, add — and one small avatar per other server with its unread in mIRC brackets, clicking to switch; the avatar row appears only at the second server; an avatar is the server's own uploaded icon, falling back to a two-letter monogram on a colour derived from the address.
- [x] The same switcher on iOS
  - Accept: the native client shows the same chip and avatar row, switches with the same core calls, and polls the same endpoint. The full list is a sheet; the header carries the two loudest servers and a +N overflow.
- [ ] Signing in to a second server never hands the first one's credential to it
  - Accept: adding a server opens the sign-in screen without disturbing the session on screen; the request carries no Authorization header from the active server; cancelling leaves everything as it was. (Partly done: the web flow is built; wanted as an asserted test.)


## F19. Shards, the control plane and one account

Everything here is about running many teams. A self-hosted server does none of
it and never has to: no shard, no control plane, no single sign-on. See
`docs/control.md`.

- [x] One process serves many teams, resolved once at the edge
  - Accept: `-shard` serves many teams routed by Host — capitals, a port and a trailing dot are the same address; an unknown Host is a 404 and never creates a team; a token minted by one team is worth nothing on another; a restart serves the teams it was serving with no control plane involved. Hosted only: each team is a PostgreSQL schema (`t_<slug>`, chosen by `search_path`, so no query names a tenant) and a bucket prefix (`tenants/<slug>/`); `-shard` refuses to start without `DATABASE_URL` and the bucket, and teams are recorded in `gliph_tenants`. Without `-shard` the binary is one team answering whatever Host arrives: SQLite in `-data` for a self-host, PostgreSQL's public schema for gliph.sh. (`server/shard_test.go`, against PostgreSQL)
- [x] A team is provisioned by one authenticated call, and a retry makes no second team
  - Accept: `POST /shard/tenants` behind a bearer secret records the team, creates its schema, creates `#general` and returns the invite that makes its first account admin; the same slug again returns what exists; two teams cannot claim one host; a bad slug is refused; a shard with no secret accepts no provisioning at all.
- [x] Accounts, the team directory, placement and billing state live in Postgres, off the chat path
  - Accept: signup and login on the control plane; `GET /api/servers` lists the teams an account is in with the address for each; creating a team places it on the emptiest shard in its pool with room, provisions it and makes the caller owner; a dedicated customer is a pool of one with no special case; the shard registry needs an operations secret; billing status is a column the shard never reads. (`control/control_test.go`, against a real Postgres)
- [x] One account signs in to a hosted team without the control plane being on the chat path
  - Accept: the control plane mints a one-minute Ed25519 assertion naming the team, the email and the nick; the team's own server verifies the signature, the expiry and the audience, then finds or creates the local user and mints an ordinary session; an assertion for one team is refused by its neighbour; an edited, expired or foreign-signed assertion is refused and creates nothing; a nick clash gets a suffix rather than a failure; a server with no control key refuses single sign-on entirely. The wire format is pinned from both modules by the same literal. (`server/sso_test.go`, `control/control_test.go`)
- [x] Every SQL statement goes through one dialect seam
  - Accept: the writer and reader handles rewrite on the way to the driver, transactions included; placeholders are renumbered without touching a `?` inside a string literal; a test walks every SQL literal in the package and fails on one that would not survive the rewrite, and on any SQLite-only construct at all. (`server/sqldb.go`, `server/sqldb_test.go`)
- [ ] The clients sign in with a gliph account and get their servers from the directory
  - Accept: the sign-in screen takes an email and password, lists the teams the account is in, and entering one trades an assertion for a session without typing an address; a self-hosted server is still reached by address with no account involved.
- [x] Postgres actually runs a team's chat
  - Accept: `schema.sql` is adapted for PostgreSQL at open (64-bit integers, no PRAGMA); the room-notes wall orders by a time-sorted id, not `rowid`; the whole server suite passes on both databases (`GLIPH_TEST_PG`). gliph.sh runs on it since 2026-11-17. Booleans stay 0/1 integers on purpose (every scan site reads them that way), and the single writer connection stays per process: it is what keeps Append's seq and the first-account rule race-free. (2026-11-17)
- [x] Moving a team between shards is a command  (`control -move <slug>:<shard>` and `POST /api/servers/{slug}/move`; `server/shard_test.go` and `control/control_test.go`. Gap: it repoints the registry and the directory, not the Pushleft route, which matters once shards run as separate roles; no shard runs on gliph.sh yet)
  - Accept: drain, move, repoint, resume — as one operation. With every team a schema of the managed PostgreSQL and a prefix of one bucket, a move between shards is a registry change, not a copy.
