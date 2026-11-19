# Feature ledger

Every user-facing feature, and where it exists. **A feature is not done
until every shipped client has it** (web/desktop share one bundle; iOS is
native; Android is native, in `android/`). When a feature lands on one side, add or
update its row in the same commit. `n/a` means the platform has no sensible
equivalent (a phone keyboard has no ↑ key), `—` means still to do.

| feature | web + desktop | iOS | Android |
|---|---|---|---|
| Many servers in one client: `you @ server [n]` in the top bar, avatars to switch | ✓ | ✓ | ✓ |
| Tablet layout: the channel rail docks beside the conversation, `#` collapses it | ✓ (desktop window) | ✓ iPad | — |
| Unread on the servers you are not looking at, polled | ✓ | ✓ | ✓ |
| Add a server without disturbing the session on screen | ✓ | ✓ | ✓ |
| Sign in with a gliph account and get your servers from the directory | — | — | — |
| Channels, DMs (`/msg`), members rail, presence (online/offline) | ✓ | ✓ | ✓ |
| Slash commands from the server manifest, palette | ✓ | ✓ | ✓ |
| Palette: Enter takes the highlighted command | ✓ | ✓ | ✓ |
| Composer ↑/↓ send history | ✓ | n/a | n/a |
| Reply to a message (right-click / long-press); the quote rides in the encrypted body | ✓ | ✓ | ✓ |
| Click the quote to jump to the message it answers, and flash it | ✓ | ✓ | ✓ |
| `@nick` mention: azure, dashed, tap opens the DM | ✓ | ✓ | ✓ |
| Inline code with single backticks | ✓ | ✓ | ✓ |
| Fenced code: block, language label, fold past 7 lines | ✓ | ✓ | ✓ |
| Fenced code: syntax colouring (Monaco) | ✓ | — | — |
| Prose fences (markdown, text) wrap | ✓ | ✓ | ✓ |
| ``` opens the Monaco code editor popup | ✓ | — | — |
| IRC formatting codes, links, link cards | ✓ | partial | partial |
| E2E encrypted channels (MLS) | ✓ | ✓ | ✓ |
| New device approval: `/approve <code>` from a device you use, approval bar, "unapproved device" in the member list, `/devices` | ✓ | ✓ | ✓ |
| Approve by QR: the new device shows its code as a QR code, a phone's camera opens `gliph://approve/<code>` and Gliph asks to confirm | shows the QR | ✓ | ✓ |
| Channel history policy (`/create #name sealed`), shown in the title strip | ✓ | ✓ | ✓ |
| File transfer, nearby mesh | ✓ | ✓ | ✓ (NsdManager + TCP, the `Mesh.swift` wire) |
| Paste an image from the clipboard as a file | ✓ | ✓ (`[paste]` key when the clipboard holds one) | ✓ (`[paste]` key when the clipboard holds one) |
| Drop a file on the window to send it | ✓ | n/a (no window to drop on) | n/a (no window to drop on) |
| Image attachments preview in the stream, cut at half the screen, `[ - ]`/`[ + ]` to resize, tap for the lightbox | ✓ | ✓ | ✓ |
| A file moved peer to peer (DM over WebRTC, or nearby) previews the same way, off the copy in hand | ✓ | ✓ (DM over WebRTC, `mobile/Sources/P2P.swift`; nearby over the mesh) | ✓ (DM over WebRTC, `android/.../P2P.kt`; nearby over the mesh) |
| Blob (todo/pins/devlog/check-in) input keeps focus and caret while typing | ✓ | n/a (native field) | n/a |
| Notifications (desktop panel / OS) | ✓ | ✓ | ✓ |
| Mobile push: content-free wake, notification rendered on the phone | n/a | ✓ direct APNs (`server/apns.go`) | ✓ direct FCM (`server/fcm.go`); off until the Firebase project exists |
| System messages as cards in a proportional face, three lines with expand/collapse: the MOTD in #general, dismissed per device, and this build's release notes in a client-made #system | ✓ | ✓ | ✓ |
| First-run tour: a new account's first hello puts a MOTD-styled card in #general that teaches `/`; dismissed, it never returns | ✓ | ✓ (the same system card) | ✓ (the same system card) |
| Accessibility floor: AA contrast in creme and midnight (`web/scripts/contrast.mjs`, run by `npm run check`), focus rings, reduced motion, labelled controls | ✓ | ✓ VoiceOver labels, Dynamic Type, Reduce Motion | ✓ TalkBack labels, system font scale, system animation scale |
| MOTD and team icon, set in the admin console (Settings) | web only | n/a | n/a |
| Webhooks: `/hook in <name>` posts as a labeled agent line, `/hook out <url>` delivers signed events (no body in +e) | ✓ (server side) | ✓ (server side) | ✓ (agent lines render) |
| Substrate agent channel (substrate.gliph.sh, a plugin): `/substrate link <project>` shows a project's agent conversation, `@<project> text` messages its agent; Sign in with Gliph through `/link.html` | ✓ | ✓ (agent lines and commands render) | ✓ (agent lines render) |
| `/export [txt|jsonl]`: the channel or DM as a text file and a JSONL file, from this device's decrypted history (core `client/export.rs`) | ✓ (downloads) | ✓ (saved to Files) | ✓ (saved to Downloads) |
| Custom themes: `/theme add` (paste, or drop a `.theme` file on the window), `/theme <name>`, `/theme remove`; the core checks the file and names the bad line (`client/theme.rs`) | ✓ (the desktop notification window keeps the base theme) | ✓ (paste; the `r0-*` and `code-*` tokens are web only) | ✓ (paste; the `r0-*` and `code-*` tokens are web only) |
| User scripts: `/script add <name>` (editor, paste, or drop a `.rhai` file), on_message with a wildcard `pattern`, on_highlight, on_join; `say`/`notice` only, 50k-step budget, a runaway is stopped and reported; `/script export` shares it (Rhai in the core, `client/script.rs`) | ✓ | ✓ | ✓ |
| `/status` (server + client build) | ✓ | ✓ | ✓ |
| `/update`, ★ update button, self-update | ✓ desktop | n/a (TestFlight) | n/a (store) |
| Admin console | web only | n/a | n/a |
| Downloads page | web | n/a | n/a |
| gliph.sh front page for signed-out visitors, and the `install.sh` one-liner (`server/landing.go`) | web | n/a | n/a |
| room0: the channel's 3D office — `/room0` (alias `/3d`), presence, voice, whiteboards, voice-note wall, procedural worlds (sea/dunes/campfire/space/terminal) ([room0.md](room0.md)) | ✓ | — | — |
| room0 in the stream: room chat is channel chat with an `[office]` mark, the live strip above the composer, `/room0 invite`, `/room0 quiet` ([room0-chat.md](plans/room0-chat.md)) | ✓ | ✓ mark, strip and invite read-only — the room itself is web/desktop | ✓ mark, strip and invite read-only |

How to read `partial`: IRC colour codes are stripped on both phones; iOS draws
links as plain text, Android makes them tappable; neither draws link cards. Track new gaps here rather than in chat.

## Nearby parity

Nearby (F9) is the second lane: no server, no account, one Wi-Fi hop, and it
must stay usable as *chat* rather than as a demo. It exists on desktop, iOS and
Android only — a browser tab has no Bonjour and no listening socket, so
`meshSupported()` is `isTauri()`.

Everything a person does to a line works there:

| | nearby | note |
|---|---|---|
| messages | ✓ | |
| `/me` | ✓ | a `kind` on the mesh, because there is no server to make an event of it |
| reply, with the quote | ✓ | the quote is in the body, so it is the same body everywhere |
| jump to what a reply answers | ✓ | `reply_to` on the mesh msg frame |
| right-click / long-press menu | ✓ | |
| file transfer | ✓ | `/send <nick>`, DCC-style to ONE peer, 10 MB. Completing leaves a `file` line at both ends |
| code blocks, cards, mentions, IRC colours, ASCII | ✓ | all render-side, nothing to route |
| every arrival announces | ✓ | deliberately: a room of five people you are standing next to is a DM, not a channel |

Deliberately **not** in nearby, each for a reason rather than an oversight:

| | why |
|---|---|
| E2E encryption | one Wi-Fi hop, and the channel topic says so. The fix is a per-link ECDH handshake with fingerprints in the member list — `core/src/mesh.rs` |
| history | "gone when the last peer leaves" is the feature. Nearby scrollback is never persisted, and there is a test that says so |
| blobs (todos, pins, devlog, check-ins) | a blob is a document the server or a plugin renders; there is neither here |
| every other slash command | they are the server's. Nearby adds its own four, and `/me`, `/clear` and `/ignore` are local so they work |
| push notifications | nothing is awake to send one |

When a chat feature lands, ask which of those two lists it belongs in and add
the row in the same commit.
