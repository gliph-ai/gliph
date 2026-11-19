# Gliph wire protocol v0

One TLS WebSocket per client. Every frame is JSON: `{"t": "<type>", "d": {...}}`.
Server assigns a monotonic `seq` per channel. Clients keep two per-channel
numbers and they are not the same: a **cursor**, how far this device has been
sent, and a **read mark**, how far the person has actually read. The cursor is
per device and drives catch-up; the read mark belongs to the account and is
synced across every device on it.

## Connect

    GET /ws?token=<session token>&client=<platform>/<version>

`client` is optional and informational: `/status` echoes it back so a tester
can say which build they are on. Platforms: `web`, `desktop`, `ios`.

### server -> client `hello`

```json
{"t":"hello","d":{
  "user":{"id":"u_1","nick":"waseem","role":"admin"},
  "channels":[{"id":"c_1","name":"general","topic":"...","modes":"nt","history":"open","role":"op","seq":412,
     "read":400,
     "members":[{"user_id":"u_1","nick":"waseem","role":"op","kind":"user","online":true,"unverified":false}],
     "room":{"since":"2026-09-06T14:02:11Z",
             "users":[{"user_id":"u_2","name":"robin","room":"office","voice":true}]}}],
  "commands":[ ...command manifest... ],
  "server":{"name":"acme.gliph.chat","version":"0.1.54+300aae9","upload_max":1073741824}
}}
```

`first` is true on the hello of an account's first connection ever, and only
there. The client shows the first-run tour on it: a MOTD-styled card in
`#general` that teaches `/`, kept until it is dismissed and never shown again.

`history` is the channel's history policy, fixed at creation
(`/create #name [open|sealed]`): `open` lets a newcomer read what was said
before they joined, `sealed` starts them at their join. Missing means `open`. A
member's `unverified` is true while the account has a device no other device of
it approved (see Device trust); clients flag it in the member list.

`seq` is the newest event in the channel and `read` is how far this account has
read it, on any device. Unread is the difference, so a client that has been away
adopts `read` rather than trusting its own cache, and one that has read further
sends its own mark up (see the `read` frame). Older servers omit `read`; treat a
missing value as zero and leave the local mark alone.

The `commands` array IS the client's entire command palette. Clients MUST NOT
hardcode command names. When the registry changes (a plugin connects, an agent
joins a channel), the server pushes `commands` again to every affected client.

## Command manifest entry

```json
{"name":"invite","usage":"/invite <nick>","desc":"invite a user to this channel",
 "args":[{"name":"nick","type":"nick","required":true}],
 "scope":"channel","min_role":"op","source":"builtin"}
```

- `scope`: `channel` | `dm` | `any` | `global`
- `min_role`: `member` | `op` | `admin` (server enforces; clients only grey out)
- `source`: `builtin` or the plugin id that owns it. Plugin-owned commands are
  routed to that plugin; builtins execute in-process.
- `args[].type`: `text` | `nick` | `channel` | `int` | `file` | `rest` —
  clients use this for autocomplete only. The server re-validates everything.

## Client -> server

| type | payload | notes |
|---|---|---|
| `sync` | `{"cursors":{"c_1":400}}` | catch-up; server replies with `events` batches then streams live |
| `read` | `{"marks":{"c_1":412}}` | how far the user has read. Sent on connect for anything read offline, and whenever a channel is read. Forward only: the server keeps the highest mark it has seen and ignores anything lower, so devices reconcile without ordering. A mark past the channel's newest `seq` is clamped to it, and marks for channels you are not in are dropped |
| `message.send` | `{"channel":"c_1","body":"text","nonce":"...","reply_to":"e_...","room":"office","quiet":false}` | `nonce` dedupes offline queue replays; `reply_to` is optional, see Replies; `room` and `quiet` are set when the line was said inside room0, see Room |
| `command.run` | `{"channel":"c_1","name":"invite","args":"robin"}` | raw arg string; server parses per manifest |
| `ping` | `{}` | |

## Server -> client

| type | payload |
|---|---|
| `hello` | see above |
| `commands` | `{"commands":[...]}` — full replacement of the palette |
| `events` | `{"channel":"c_1","events":[{...}],"more":false}` |
| `event` | a single live event |
| `notice` | `{"channel":"c_1","level":"info\|error","text":"..."}` — ephemeral, not in the log |
| `error` | `{"code":"...","message":"..."}` |
| `client.update` | `{}` — `/update` ran on this connection: check for a newer build, install it, restart. Only the connection that asked. Clients without an updater (browser, phone) say so and do nothing. |
| `motd` | `{"motd":{...}\|null}` — the operator saved a new message of the day; same shape as `hello.server.motd`. Pushed to everyone, so an announcement does not wait for each client's next reconnect. |
| `read` | `{"marks":{"c_1":412}}` — another device on this account read that far. Relayed to every device except the one that reported it, so reading on the phone clears the badge on the desktop without waiting for a reconnect. Apply it only if it is higher than the local mark. |

## System messages

Two things in the stream are not chat: the operator's message of the day, and
the release notes for the build the reader is running. Both ride in `hello`:

```json
"server": {
  "name": "acme.gliph.chat", "version": "0.1.54+300aae9", "upload_max": 1073741824,
  "motd": {"id":"motd-9f3c1a2b4d5e","text":"standup moved to 09:30","icon":"data:image/png;base64,..."},
  "news": [{"id":"r-room0","date":"2026-09-03","title":"Your channel has a 3D room","body":"..."}]
}
```

- `motd` is `null` when the operator has not set one. Its `id` is a digest of
  the text, so editing the MOTD shows it to everyone again and re-saving the
  same words does not.
- `icon` is a **data URL**, never a link. A client draws it directly, and a
  chat client that fetched a remote image to render a line would be telling
  that image's owner who read the channel and when.
- `news` is the newest five release notes, oldest first, compiled into the
  server binary from `server/releases.json`. They describe the build, so they
  ship with it. The whole list is public at `GET /api/releases` (same shape,
  no auth) for a website or a changelog page.

Where they are drawn is the client's business (`core/src/client.rs`), and the
two do not share a place. The MOTD is the operator talking to the team, so it
shows in `#general` until it is dismissed — a per-device fact the client
caches, since the server keeps no read state for it. The notes are the product
talking about the build, so they get `#system`: a channel the client makes
up out of `news`, all of them, oldest first, with no × on them.

## Event shape

```json
{"seq":413,"id":"e_...","channel":"c_1","kind":"message","actor":"waseem",
 "ts":"2026-08-25T14:02:11Z","body":"...","meta":{}}
```

`kind`: `message` | `action` (/me) | `join` | `part` | `mode` | `topic` |
`kick` | `notice` | `agent` | `file` | `room_invite`.

Events are append-only. Edits, deletes and reactions are new events carrying
`meta.target` = the edited event id. Clients fold them at render time.

### Replies

A reply is an ordinary `message` whose body opens with a quote line:

```text
> <alice> the line being answered
my reply
```

The quote is always one line (the newline is the delimiter), clipped to 140
characters with `…`, and quoting a reply quotes what it said rather than what
it was quoting.

`message.send` may also carry `reply_to`, the id of the event being answered.
The server records it as `meta.reply_to` on the posted event, after checking
that the id names an event **in that same channel** — a `reply_to` that does
not is dropped and the message still posts, because the quote in the body is
what carries the meaning either way.

The two halves are not redundant, and the split is deliberate:

- the **quote** is what you READ. It is part of the body, so in a `+e` channel
  it is inside the ciphertext, and it survives the original being deleted,
  scrolling out of the client's cache, or arriving on a device with no key.
- `meta.reply_to` is what you FOLLOW. It is a bare id in the clear, and it
  costs the server knowing the reply graph — that is the price of a jump
  target, and the reason the quote is not simply replaced by it.

Clients make a quote clickable only when its target is in the scrollback they
hold; one whose original is not stays readable and is not a link.

The format lives in `core/src/wire.rs`, and every client gets the pieces from
`EventRow.quote`, `EventRow.meta.reply_to` and `Snapshot.reply` rather than
parsing a body itself.

A `join` posted when an account is created (self-registration or admin
provisioning) carries `meta.new: true`. Clients announce that one join to every
member — someone new is on the server; a `/join` by an existing member is a
stream line only.

## Plugins and agents

A plugin is a WebSocket client at `/plugin?token=<plugin token>`, issued by an
admin. It is a first-class member: it appears in member lists and its messages
are labelled.

### plugin -> server `register`

```json
{"t":"register","d":{
  "id":"deploybot","display":"deploybot","kind":"agent",
  "commands":[{"name":"deploy","usage":"/deploy <env>","desc":"ship it",
               "args":[{"name":"env","type":"text","required":true}],
               "scope":"channel","min_role":"op"}],
  "subscribe":["c_1"]
}}
```

`display` is the name every message the plugin posts is attributed to, and it is
declared here rather than fixed at mint time, so a plugin can be renamed without
reissuing its token. It is refused if it collides with a member's nick — agent
lines are badged in the client, but a bot calling itself `waseem` is still a lie.
Omit it to keep the name the token was minted with.

`kind`: `bot` (commands only) or `agent` (commands + streamed events from
`subscribe` channels, so it can watch and act). **Set when the token is minted;
the server ignores it in this frame.** It decides whether the plugin is streamed
the events of every channel it subscribes to, which is the admin's call, not the
plugin's — a `bot` cannot promote itself to `agent` by asking.

On register the server merges these commands into the registry and pushes a new
`commands` manifest to every connected client. On disconnect they are removed
and the manifest is pushed again. A plugin claiming a builtin's name is rejected.

### server -> plugin

| type | payload |
|---|---|
| `ready` | `{"id":"deploybot"}` |
| `invoke` | `{"call":"k_9","channel":"c_1","actor":"waseem","name":"deploy","args":"staging","arg_map":{"env":"staging"}}` |
| `event` | streamed channel events (agents only) |

### plugin -> server

| type | payload |
|---|---|
| `register` | see above |
| `result` | `{"call":"k_9","say":"shipping staging…"}` — posts as the plugin into the call's channel |
| `say` | `{"channel":"c_1","body":"build #412 green","agent":"github"}` — unsolicited, needs membership. `agent` is optional, also on `result`: one of `claude`, `openai`, `gemini`, `cursor`, `copilot`, `github`, `gitlab`; clients draw that product's logo in place of the agent diamond. Anything else is dropped |
| `fail` | `{"call":"k_9","message":"unknown env"}` — becomes an ephemeral notice to the caller only |

Plugins may only post to channels they are a member of. The server rejects
anything else, so a compromised plugin token cannot reach other channels.

## Webhooks

Three kinds of hook live on one channel each, minted by an op with `/hook` and
revoked with `/hook revoke <id>`. The token is shown once and is the whole
credential. A token works only at its own endpoint. Webhooks are refused in DMs.
Minting one posts a `notice` in the channel, so every member knows it exists.

| kind | minted with | endpoint |
|---|---|---|
| `git` | `/hook new` | `POST /api/hooks/git/<token>`, GitHub or GitLab push (see `server/devlog.go`) |
| `in` | `/hook in <name>` | `POST /api/hooks/in/<token>` |
| `out` | `/hook out <url>` | the server POSTs to `<url>` |

**Incoming.** The body is `{"text":"…"}` with `Content-Type: application/json`,
or plain text. The line posts as an `agent` event with `actor` = the hook's
name and `meta.webhook` = the hook id, so clients label it like any agent line.
The name cannot be a member's nick. The rate is 1 line/s after a burst of 20
(429 past it). Up to 4000 characters. In a `+e` channel the line is plaintext
on the server: the sender is outside the channel and holds no key.

**Outgoing.** Every event posted in the channel is delivered, one POST each:

```json
{"hook":"h_1","channel":{"id":"c_1","name":"dev","encrypted":false},
 "event":{"seq":42,"id":"e_9","kind":"message","actor":"dana",
          "ts":"2026-11-18T12:00:00Z","enc":false,"body":"shipped"}}
```

Headers: `X-Gliph-Hook` (id), `X-Gliph-Event` (kind) and
`X-Gliph-Signature: sha256=<hex HMAC-SHA256 of the raw body, keyed by the
token>`. Compare it in constant time. **In a `+e` channel `body` is left out**
and `enc` is `true`: the server has only ciphertext and no key, so the delivery
carries the kind, actor, time and seq. Lines that came in through an incoming
hook are not delivered out again. One attempt, 5 s timeout, no redirects. The
target must resolve to a public address (no loopback, private, link-local or
CGNAT ranges).

## Blob surface (GUM)

A channel can carry mini-apps ("blobs") that a provider renders and the client
drives. The client hardcodes no app UI, exactly as it hardcodes no commands:
`state` carries `blobs: [{id, title, mode, source}]` per channel and the
switcher is built from that.

`mode` is `shared` (one document per channel, everyone sees the same one) or
`private` (rendered per viewer). `source` is `builtin` or the plugin id.

| dir | type | payload |
|---|---|---|
| `→` | `blob.open` | `{channel, blob}` — subscribe (shared) or render for me (private) |
| `→` | `blob.event` | `{channel, blob, action, item?, value?}` |
| `←` | `blob.render` | `{channel, blob, doc}` — a GUM document |
| `←` | `blob.menu` | `{channel, blob, item, actions}` — a transient submenu over the current document |
| `←` | `blob.focus` | `{channel, blob}` — "open this app" |

`blob.focus` is the only frame the server sends to move the UI. It is addressed
at the ONE connection that asked (`/todos`, `/pins`), not at the user's devices:
you typed it here, so it opens here.

**The server is the gate, not the client.** `blob.event` is refused unless the
action id appears in the document that user was last sent, and unless they clear
its `min_role` in that channel. A `confirm` on an action is a courtesy the
client renders; it is not what makes the action safe.

The GUM document schema (block types, actions, `confirm`/`prompt`) is in
`docs/plans/gliph-apps.md` §3. The client half — the focus order, the selection,
the keyboard model — is `core/src/gum.rs`, shared by every frontend.

## Room

Each channel carries a 3D multiplayer room. The server's part splits the same
way p2p does: presence, movement, objects and voice signalling
are **relayed and never stored** — when the last socket closes, the room is
empty and nothing remembers it — while the shared boards and the voice-note
wall **persist**, because they are what should still be there tomorrow.

`/room0` (alias `/3d`) sends `room.open` `{"channel":"c_1"}` to the ONE
connection that typed it, the `blob.focus` rule: you asked here, so it opens
here. `/room0 invite [nick|#channel]` posts the invite event below, and
`/room0 quiet` sends `room.quiet` `{"channel"}` to that same one connection.

### Chat in the room is chat in the channel

There is no room-only chat. A line said in the room is an ordinary
`message.send` carrying `room` (which sub-room, one of `office`, `garden`,
`kitchen`, `lounge`, `beach`, `sea`, `dunes`, `camp`, `space`, `terminal`) and
`quiet` (the sender is keeping their lines out of the stream). The server
records them as `meta.room` and `meta.quiet`, refusing a `room` it does not
know rather than stamping it, because clients render the mark as-is. Both sit
in the clear beside the ciphertext: the relay below already carries room names,
so this leaks nothing new, and it is what lets a client mark a line `[office]`
without a key.

`quiet` is a rendering flag and nothing more. The event is stored, synced and
searched like any other; clients keep it out of the stream and show it in the
room's own log. The server never decides anything from it.

The bubble over an avatar is drawn from the `event` frame every member already
receives. Nothing relays the same sentence twice.

### The invite

```json
{"kind":"room_invite","actor":"robin","body":"office",
 "meta":{"target":"u_7","target_nick":"waseem","channel":"c_1"}}
```

`body` is the room to spawn in. `meta.target` present means a personal invite,
posted in the DM; absent means the whole channel. A personal invite also
carries `meta.channel`, the channel whose room was opened, because the DM it
arrives in has no room of its own: that is the channel the join button walks
into, and the one whose occupancy decides whether it is still live. The event is written once and
never updated: whether its join button still works is read off who is in the
room right now, which is the channel view's `room` snapshot plus the relays
below. That is why an invite has no expiry and no second event.

### Client -> server

| type | payload | notes |
|---|---|---|
| `room.join` | `{"channel","room","x","y","h","anim","look"}` | membership re-checked from the store per frame |
| `room.send` | `{"channel","type",...}` | refused until this connection has sent `room.join` for that channel |
| `room.leave` | `{"channel"}` | |

`room.send` types and the fields each carries: `move` / `announce` (`room, x,
y, h, anim, carry, moving, voice, look` — both relay as `move`; `announce`
asks the room to answer with their own state), `object`
(`id, room, x, y, h, vx, vy, holder` — `h` is a heading, used by rideable
vehicles like the sea rowboat), `rtc` (`to, kind, payload`), `emote`
(`kind`).

### Server -> client `room.event`

Every relay lands on **every member of the channel, sender included** —
clients filter their own echo by `user_id`:

```json
{"t":"room.event","d":{"channel":"c_1","type":"move","user_id":"u_1",
 "name":"waseem","room":"office","x":3.2,"y":-1.4,"h":1.57,
 "anim":"walk","moving":true,"voice":false,"look":"suit"}}
```

`type` is `join`, `move`, `object`, `rtc`, `emote`, `leave` or `vnote`
(the voice-note wall, below). Chat is not among them: it is a channel message,
above. A closed socket broadcasts the same `leave` as
the frame would have.

The payload is **rebuilt field by field, never forwarded** — the p2p rule: a
pass-through relay would carry any extra keys a client invented, which is a
covert channel between clients the server claims to be mediating. Strings are
capped (`room` 24, `anim` 16, `look` 48, `carry`/`holder`/`id`/`to` 64,
`kind` 24), coordinates and velocities are clamped to ±1000, and `name` always
comes from the server's user record, never from the client. Two rewrites
matter: an `object` whose `holder` is `"me"` gets the sender's real user id
(nobody puts an object in someone else's hands), and `rtc.payload` is the one
field carried opaque — it is SDP/ICE between two peers, uninspected for
exactly the reasons p2p signalling is.

`room.join` and `room.send` are metered as their own flood kind: 40 events/s
with a burst of 80, sized for 10 Hz movement plus 10 Hz object streams plus
ICE bursts.

### Boards and the voice-note wall

Plain HTTP, all bearer-authed and membership-checked against the channel.
Boards are one JSON document per (channel, kind), replaced on save; kinds are
`tasks` and `drawing`. Voice notes are left per sub-room (`office`, `garden`,
`kitchen`, `lounge`, `beach`); each wall keeps the newest 50 and reaps the
rest, rows and audio files both.

| method | path | notes |
|---|---|---|
| `GET` | `/api/room/board?channel&kind` | `{"board":{"kind","data","updated_at"}}` — never saved is `data: {}`, not a 404 |
| `PUT` | `/api/room/board?channel&kind` | body `{"data":{...}}`; a JSON object, max 512 KB serialized (else 400/413); upserts and returns the board |
| `GET` | `/api/room/notes?channel&room` | `{"voice_notes":[...]}` newest-first, max 50 |
| `POST` | `/api/room/notes` | multipart: `channel`, `room`, `duration_ms` (1..120000), `file` (≤5 MB; `audio/webm`, `audio/mp4`, `audio/mpeg`, `audio/ogg` or `audio/wav`) → 201 `{"voice_note":{...}}` |
| `DELETE` | `/api/room/notes/{id}` | the author, a channel op or a server admin; 204 |
| `GET` | `/api/room/notes/{id}/audio` | the bytes, stored content type, inline; accepts `?token=` the way `/api/file` does, for the `<audio>` tag that cannot set a header |

A note serializes as:

```json
{"id":"vn_...","room":"lounge","duration_ms":4200,
 "created_at":"2026-09-03T11:02:44Z","author_id":"u_2","author_name":"robin",
 "audio_url":"/api/room/notes/vn_.../audio"}
```

Creating one broadcasts `room.event` `{"type":"vnote","op":"new","note":{...}}`
to the channel, deleting one broadcasts `{"type":"vnote","op":"del","id","room"}`
— so a wall someone is looking at updates without polling.

## Mobile push

Two HTTP endpoints, both bearer-authenticated as the user.

| method | path | body |
|---|---|---|
| `POST` | `/api/push/devices` | `{"platform":"ios"\|"android","token":"<os push token>"}` |
| `DELETE` | `/api/push/devices` | `{"token":"<os push token>"}` |

Registration upserts on the token, so a client may register on every launch and
an OS token rotation replaces the row rather than adding one. The token is
write-only: no endpoint reads one back out.

**The wake payload is empty, and that is the contract.** gliph is end-to-end
encrypted, so the server cannot write a useful notification and must not try.
What leaves the server is exactly this, and nothing more:

- to Apple directly (`-apns-key`): a background push, `apns-push-type:
  background`, whose payload is `{"aps":{"content-available":1}}`;
- to Google directly (`-fcm-credentials`): an FCM v1 data-only message,
  `{"message":{"token":"…","data":{"wake":"1"},"android":{"priority":"high"}}}`,
  with no `notification` block, so Android draws nothing on its own;
- to the optional relay (`-push-relay`): `{"wake":[{"platform":"ios","token":"…"}]}`.

No body, no sender nick, no channel name, no channel id, no unread count. The
device wakes, re-syncs from its own cursor, decrypts with its own key, and
renders the notification locally. Every word the user reads is produced on their
own phone — see `server/push.go` and `web/src/notify.ts`.

Wakes are coalesced to at most one per user per 30 seconds, and are sent only to
members who hold no socket at that moment. Housekeeping events (join, part,
mode, topic) never wake anyone.

iOS goes to Apple when `-apns-key` is set, Android to Google when
`-fcm-credentials` is set; either falls back to the relay otherwise. A server
with none of them sends no wakes at all; everything else works unchanged.

## Device link (PKCE loopback)

Lets a desktop or mobile client sign in without ever seeing a password. The app
opens the browser at `/link.html`, the already-signed-in user approves, and the
app redeems a one-time code for a session.

| method | path | auth | body |
|---|---|---|---|
| `POST` | `/api/auth/device-link/authorize` | bearer (the approving user) | `{"device_name","platform","redirect_uri","code_challenge","code_challenge_method":"S256"}` |
| `POST` | `/api/auth/device-link/token` | **none** | `{"code","code_verifier","redirect_uri"}` → `{"token","user"}`, plus `"email"` for `platform=substrate` only |
| `GET` | `/api/auth/devices` | bearer | linked devices (opaque handles, never tokens) |
| `DELETE` | `/api/auth/devices/{handle}` | bearer | sign out one device |

For any new client:

1. `code_verifier` = 64 random bytes, base64url, unpadded.
   `code_challenge` = base64url(SHA256(verifier)), unpadded. Method `S256` only.
   Plus a random `state`.
2. Bind `127.0.0.1:<port>`; `redirect_uri = http://127.0.0.1:<port>/cb`.
3. Open `{web}/link.html?device_name&platform&redirect_uri&state&code_challenge&code_challenge_method=S256`.
4. Catch `GET /cb?code&state`, check `state` matches, acknowledge in the tab.
5. `POST /api/auth/device-link/token` with the code and the verifier. Store the
   returned token as the bearer.

`platform` is `desktop`, `ios` or `android`, or `substrate`: Substrate's "Sign in
with Gliph" (substrate.gliph.sh). That one is accepted only when `redirect_uri`
equals the server's `-substrate-callback`, and only its redemption carries the
account's `email`, which Substrate signs people in by. Codes live 5 minutes and are
single-use — including when redemption *fails*, so a wrong verifier gets no
second attempt.

**The redirect is validated loopback-only, server side.** A code can only ever
be delivered to something on the same machine; without that check a crafted
`/link.html` URL would be a way to have a signed-in user hand their fresh code
to any host on the internet.

**PKCE is what makes this safe on a shared machine.** Another local process can
race for the port and catch the code, but cannot redeem it: the verifier never
left the app that generated it.

## End-to-end encryption

A `+e` channel's messages are AES-256-GCM under a channel key per epoch. The
key is sealed to each member device with ECDH-ES (P-256, HKDF-SHA256, info
`gliph/e2ee/v1/wrap/{channel}/{epoch}`); the server stores the sealed blobs and
hands each device only its own. All of it lives in `core/src/crypto.rs`.

| direction | type | payload |
|---|---|---|
| c→s | `device.hello` | `{"device","name","pubkey"}`: first frame on every socket. The server answers `device.trust` |
| s→c | `device.trust` | `{"master","signed","sealed_master","can_establish","devices":[{"id","user_id","name","pubkey","sig","master","legacy"}]}`: this account's devices. Pushed again to every device of the account whenever one is added, approved, forgotten or reset |
| c→s | `device.sign` | `{"master"?,"sigs":{dev:sig},"sealed"?:{dev:blob},"reset"?:true}` |
| c→s | `keys.peers` | `{"channel","epoch"}`. Reply `keys.peers` `{"channel","epoch","max_epoch","devices":[...]}`: approved member devices missing that epoch (all of them when `epoch` is 0) |
| c→s | `keys.publish` | `{"channel","epoch","entries":{dev:blob}}`. Everyone in the channel gets `keys.epoch` `{"channel","epoch"}` |
| c→s | `keys.fetch` | `{"channel"}`. Reply `keys` `{"channel","keys":[{"epoch","wrapped","by_device"}]}` |
| s→c | `keys.rekey` | `{"channel"}`: a device needs keys; whoever holds them answers `keys.peers` |

### Device trust

Each account has one ECDSA P-256 **master key**. It signs every device:
`sig = ECDSA-SHA256(master, "gliph/device/v1\n{user_id}\n{device_id}\n{pubkey}")`,
DER, base64. `master` is base64 SPKI.

- **First device.** `device.trust` has `master: ""` and `can_establish: true`.
  The client mints the master key and sends `device.sign` with `master` and its
  own signature. On an account whose devices predate signing (`legacy`), the
  first one to connect signs the other legacy devices in the same frame; it
  may not sign a device registered after signing shipped.
- **Every later device** arrives unsigned. It shows its **approval code**,
  `abcd-ef01-2345`: the first 48 bits of SHA-256 over its SPKI. On an approved
  device the person types `/approve <code>`; that client signs the matching
  device and seals the master secret to it with the same ECDH-ES construction
  (info `gliph/e2ee/v1/master`), sent as `sealed`. The new device finds it in
  `sealed_master`, and can approve the next one. The code is the check: a
  device a server slipped in has a different key, so a different code.
  The unapproved device also shows the code as a QR code of
  `gliph://approve/<code>` (the core's `device.qr`). A phone's camera opens
  it in Gliph, which asks the person to confirm and then runs the same
  `/approve <code>`. The code still travels by the person, never the server.
- **The gate.** The server verifies every signature against `master`, lists
  only signed devices in `keys.peers`, drops `keys.publish` entries for any
  other, refuses `keys.publish` from an unsigned device, and answers its
  `keys.fetch` with no keys. Clients check each signature again before sealing,
  so a server cannot add a device of its own.
- **Reset.** `/devices reset` mints a new master key that signs only this
  device (`reset: true`). Every other device loses its signature and the keys
  sealed to it. Refused while another approved device of the account is online.
  `/devices` lists the account's devices and codes; `/devices forget <code>`
  removes one.
- A changed device key (a reinstall) clears that device's signature.

The master secret is the thing a recovery key will protect (spec F2): sealed
under the recovery key and parked on the server, so a recovery key can approve
a device the way another device does.

### History policy

- `open`: an existing member answering `keys.peers` for a newcomer seals every
  epoch it holds, not only the current one, so the newcomer reads history.
- `sealed`: clients rotate the epoch when anyone joins (as well as when anyone
  leaves) and seal only the current epoch to a newcomer. The server refuses any
  epoch that existed when the member joined (`members.joined_epoch`), and
  `sync` starts a member at their join `seq`, for plaintext channels too.

## ICE configuration

`GET /api/ice` (bearer auth) returns what a client should pass to
`RTCPeerConnection` for a peer-to-peer transfer:

```json
{"ice_servers":[
  {"urls":["stun:stun.example.com:3478"]},
  {"urls":["turn:turn.example.com:3478?transport=udp"],
   "username":"1756200000:3f2a1c…","credential":"base64(HMAC-SHA1)"}
]}
```

Configured with `-stun`, `-turn` and `-turn-secret` (coturn's
`static-auth-secret`). An unconfigured server returns an empty list; direct
transfers still work, relayed ones do not — which is the ~20-30% of peer pairs
behind symmetric NAT.

TURN credentials are **ephemeral**: `username` is `<unix-expiry>:<random>` and
`credential` is `base64(HMAC-SHA1(secret, username))`, valid 12 hours. The static
secret never leaves the server. The random half is deliberately not the nick —
it lands in the relay's logs, and a timestamped record of which named users were
transferring files is a social graph the rest of gliph goes to some trouble not
to keep.

Responses are `Cache-Control: no-store`, because a cached credential outlives
itself.

## Nearby mesh (F9)

Serverless ad-hoc channels on a local network. Nothing here goes through a
gliph server, and nothing here is persisted: when the last peer leaves, the
channel is gone. Two implementations speak it — `desktop/src-tauri/src/mesh.rs`
(Rust) and `mobile/Sources/Mesh.swift` (Swift) — and they are peers of each
other, so a change to one is a change to both. The client SIDE of it (channels,
peers, ingest) is shared: `core/src/mesh.rs`.

### Discovery — Bonjour

Every peer registers `_gliph._tcp` and browses for it. The TXT record IS the
announcement:

| key | value |
|---|---|
| `id` | 12 hex chars, per session, unique per device |
| `nick` | what the user calls themselves — no account exists here |
| `chans` | comma-separated nearby channels this peer is in |

The service instance name is the `id`, which is what makes a Bonjour withdrawal
enough to know who left. Bonjour and not raw multicast because iOS gates
multicast behind `com.apple.developer.networking.multicast` (an application to
Apple) while Bonjour needs only the local-network prompt.

### Discovery — Wi-Fi Aware (iOS 26+)

An iPhone on iOS 26 or later also publishes and subscribes `_gliph._tcp` over
Wi-Fi Aware, beside Bonjour and not instead of it, so two phones with no shared
network still meet. It is declared in `Info.plist` (`WiFiAwareServices`) and
needs the `com.apple.developer.wifi-aware` entitlement (`Publish`, `Subscribe`).

Wi-Fi Aware only connects paired devices. Pairing is once per pair of phones,
in Settings > Phones nearby: one phone picks "find a phone", the other "be
found", and the OS sheet does the rest. After that each phone accepts and dials
every paired device whenever nearby mode is on.

A Wi-Fi Aware browse result has no TXT record, so the peer is unknown until the
link's `hi` frame. Everything after discovery is the same as for Bonjour: the
same TCP link, the same frames, the same "both ends dial" rule. A desktop is
never a Wi-Fi Aware peer; it reaches a phone over Bonjour on shared Wi-Fi.

### Links — TCP

**Every peer dials every peer it has no link to**, and retries every 8 seconds.
An earlier version nominated the lower id as the dialler so a pair held exactly
one link; an iPhone then showed the cost, stalling every outbound connection in
Bonjour name resolution (`resolver:children_stall`) while the Mac reached it on
the first try. Nominating a dialler nominates a single point of failure, so both
ends try. The first link to land is the one a peer sends on; a second socket is
read from and otherwise ignored, and duplicate delivery is harmless because
every message carries an id the client already de-duplicates.

Discovery is not membership: a peer is only shown as nearby once a link to it is
up. mDNS is a cache and will happily keep describing a laptop that closed ten
minutes ago; an open socket cannot be stale. For the same reason a browse result
that stops listing a peer we are talking to does NOT drop it — only the link
closing does.

Frames are newline-delimited JSON in the same envelope as the server protocol:

```json
{"t":"hi","d":{"id":"9a6d34dc376d","nick":"waseem","chans":["flight"]}}
{"t":"chans","d":{"id":"9a6d34dc376d","chans":["flight","seat-14c"]}}
{"t":"msg","d":{"id":"n_29281cdbf8de","chan":"flight","nick":"waseem",
                "body":"wheels up","ts":"2026-08-26T15:05:43Z",
                "kind":"message","reply_to":""}}
```

A message goes out on every link and is not relayed: on one LAN everyone can
reach everyone, so there is no routing to do. A peer that was not connected when
a line was sent never sees it.

`kind` is `message` or `action` (`/me` — there is no server here to turn a
command into an event), and `reply_to` is the mesh id of the message being
answered, which is what makes a quote a jump. **Both default and both are
ignored when unknown**, in every implementation: a mesh is mixed the moment one
device updates, and neither direction may drop a peer over a field the other
has never heard of. A receiver accepts only those two kinds — a peer does not
get to name a kind the stream reads as the room talking.

**This hop is not encrypted.** The client says so in the channel topic. Adding
it means a per-link ECDH handshake with fingerprints in the member list, at
which point both implementations change together.

### File transfer (DCC, kept deliberately)

mIRC's shape: you OFFER a file to one person, they ACCEPT, and the bytes flow
between the two of you while a transfer window counts them. Nobody receives a
file they did not agree to, and the room is not copied on it.

```json
{"t":"file.offer","d":{"id":"x_3f2a","chan":"flight","name":"photo.jpg",
                       "size":2411008,"mime":"image/jpeg"}}
{"t":"file.accept","d":{"id":"x_3f2a"}}
{"t":"file.chunk","d":{"id":"x_3f2a","seq":0,"data":"<base64, 48 KB raw>"}}
{"t":"file.ack","d":{"id":"x_3f2a","seq":0}}
{"t":"file.done","d":{"id":"x_3f2a"}}
{"t":"file.cancel","d":{"id":"x_3f2a","reason":"declined"}}
```

**The receiver sets the pace.** It acknowledges each chunk once the bytes are in
its buffer, and the sender keeps at most **8 chunks unacknowledged**. Without
that a fast sender pushes the whole file into a socket buffer the receiver has
not drained, the progress bar reaches 100% while nothing has arrived, and a
link that then stalls loses megabytes nobody can account for. `file.done` is
only sent once every chunk has been acknowledged, so "sent" means delivered.

Ordering and integrity come from TCP, so `file.ack` is flow control and not
retransmission. A `seq` that is not the one expected means the link dropped
frames: the receiver fails the transfer and cancels it rather than assembling
something corrupt and calling it done. A receiver silent for 20 seconds ends
the transfer too.

These frames are **not** interpreted by the Rust or Swift mesh transports.
Anything a peer sends that is not `hi`, `chans` or `msg` is forwarded to the
client as-is, and `mesh_send_to` / `sendTo` addresses one peer — so the whole
transfer lives in `core/src/xfer.rs`, written once for every client.

The cap is 10 MB and the whole file is held in memory at both ends. There is no
resume: a broken transfer is one you start again, which is what mIRC did too.
The remaining ceiling is memory rather than pacing, so raising the cap means
streaming chunks to disk on both ends, not more protocol.

A completed transfer leaves a `file` event in the channel's scrollback, the way
a served upload does — but written locally at each end, not sent, because only
the two of you were ever party to it. It carries `meta.p2p`, `meta.dir`
(`send`/`recv`), `meta.peer` and `meta.size`, and deliberately **no**
`meta.file_id`: there is no server holding the bytes to link to. The receiver's
copy was handed to the host by `SaveFile` as it landed.

A DM transfer (WebRTC, `p2p.*` above) leaves the same line, from the host rather
than from `xfer.rs`: the client calls `p2p_file_line` at each end when the
checksum matches. An image line previews under itself the way an upload does,
off the copy the host is already holding — the bytes are in that client and
nowhere else, so the preview lives as long as the session does.

### Client channels

Nearby channels are `n_<name>` client-side and never collide with a server
channel id. `/nearby`, `/nearby-leave` and `/nearby-off` are merged into the
command manifest by the client the way a plugin's commands are — they exist only
while nearby mode is on.
