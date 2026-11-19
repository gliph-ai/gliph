# room0 in the stream

Wiring the 3D room's chat, presence and invites into the channel it belongs to.
The looks are approved on the mockup canvas (`design/room0/`, artifact
`bb76f4db-33cd-4448-a405-12122bf99c30`); this is the build.

The shape in one paragraph: a room0 line is an ordinary channel message with an
origin flag, so it lands in the same history and the same search as everything
else. Nobody's coming and going is stored at all. A live strip above the
composer carries who is inside, and it is computed from presence the client
already receives. The invite is one event that never changes, because whether
its join button works is derived from the strip, not from the event.

## What gets deleted

`room.send {"type":"chat"}` and its relay in `server/room.go`. Room chat stops
being an ephemeral bubble and becomes a message. The bubble over the avatar is
drawn from the `event` frame the client gets anyway, so no second frame carries
the same sentence. No alias, no fallback.

## F1. Protocol

`docs/protocol.md` in the same commit as the code.

**`message.send` grows two optional fields.**

```json
{"t":"message.send","d":{"channel":"c_1","body":"...","nonce":"...",
                         "room":"office","quiet":false}}
```

`room` is the sub-room the sender was standing in, capped at 24 runes and
checked against the known room ids. `quiet` is the sender's own flag. The
server records them as `meta.room` and `meta.quiet` on the posted event. Both
sit in the clear beside the ciphertext, which is no new leak: the server
already relays room names in `room.event`, and it is worth saying so in the
protocol doc rather than leaving a reader to work it out.

**One new event kind: `room_invite`.**

```json
{"kind":"room_invite","actor":"robin","body":"office",
 "meta":{"room":"office","target":"u_7"}}
```

`meta.target` present means a personal invite (posted in the DM channel);
absent means the whole channel. The event is written once and never updated.
Whether its join button is live is read off the strip: someone inside, join
works; empty room, join greys out. That is what buys us an invite with no
expiry, no timeout and no second event.

**One new server frame: `room.state`.**

```json
{"t":"room.state","d":{"channel":"c_1","since":"2026-09-06T14:02:11Z",
  "users":[{"user_id":"u_2","name":"robin","room":"office","voice":true}]}}
```

Sent per occupied channel on `hello`, and on any occupancy change. The gap it
fills: `room.event` join/leave already reaches every member of the channel, so
a client that was connected can follow along, but one that just dialled in
knows nothing. This is the snapshot; the existing relays are the delta.

## F2. Core (`core/src/`)

The bridge is client logic, so it lives here and both frontends read it. The
3D scene stays in `web/src/room/`.

- `client.rs handle_frame`: fold `room.state` and the `join` / `leave` / `move`
  cases of `room.event` into `rooms: HashMap<String, RoomHere>` keyed by
  channel. `move` carries the voice flag, so voice count comes free.
- `api.rs Snapshot`: add `rooms: Vec<RoomStrip>` with `channel`, `room`,
  `since_ms`, `people` (nick, room, voice, is_self), `voice_count`,
  `you_inside`, `you_quiet`. Everything the strip draws, computed once.
- `visible_events`: an event with `meta.quiet` is skipped. It stays in the
  cache and stays searchable, which is the whole distinction.
- `is_highlight`: a `room_invite` highlights. That is what makes the
  notification window and the iOS push fire, both of which already key off the
  highlight flag.
- `EventRow`: nothing new. The mark is `meta.room`, which the frontends
  already receive.

Behaviour tests in `core/tests/client_behaviour.rs`, one per rule: occupancy
folds from a snapshot plus deltas, the strip empties on the last leave, a quiet
event is absent from the stream and present in the cache, an invite highlights,
an invite in an empty room reports itself unjoinable.

## F3. Server (`server/`)

- `room.go registerRoomCommands`: `/room0 invite [nick|#channel]` and
  `/room0 quiet`. Bare `/room0 invite` means this channel. A nick opens the DM
  (`DMChannel`) and posts the invite there. Scope `channel`, min role `member`,
  because anyone in the channel can open one.
- `server.go handleSend`: accept `room` and `quiet`, validate the room id, and
  stamp `meta`. `Post` is untouched.
- `room.go`: track occupancy per channel from the existing `roomJoined` map,
  emit `room.state` on change, and send the snapshot inside `hello`. Delete the
  `chat` case from `handleRoomFrame`.
- Quiet is a client-side flag; the server never sees it as policy, only as the
  `meta.quiet` bit it stamps and stores. Say that in the doc, because it is the
  honest limit of the promise.

Tests in `server/room_test.go`: the two commands, the invite landing in a DM,
`room.state` on connect, and `message.send` with a bogus room id being refused
rather than stamped.

## F4. Web (`web/src/`)

- `ui.ts renderEvent`: the `[office]` mark on any event with `meta.room`, and a
  `room_invite` row with the join button and cancel link.
- `theme.css`: the gradient-text mark (`#39c6ff` to `#9d8ef5`,
  `background-clip: text`), the pinned strip with its gradient hairline, and
  the join button reusing the `.xacts button` geometry with `--accent` behind
  it. Creme gets `#0b8fd0` to `#2941c8`.
- `main.ts`: draw the strip between the stream and the composer from
  `snapshot.rooms`, and nothing when the list is empty.
- `room/room.ts`: send chat through `message.send` with the room name, pre-fill
  the log with the channel's last few events on entry, chip channel-origin
  lines with `[#dev]`, and add the quiet toggle beside the composer.
- The keyboard bug found on the way here rides along: `game3d.js:947` refuses
  every key while `document.activeElement` is editable, and after `/room0` that
  is the composer, so the room opens deaf. Focus the overlay root on open.

## F5. iOS (`mobile/Sources/`)

The parity ledger says every feature change reaches the phone in the same work.
room0 itself stays web and desktop.

- `Views/StreamView.swift`: the mark on `meta.room` lines and a `room_invite`
  row whose action reads "open on desktop".
- The strip renders as a one-line bar with the count and no join.
- `docs/features.md`: room0's row gains the bridge, marked web/desktop for the
  room and all three for the stream side.

## F6. Copy and the note

- `/room0 quiet` answers with what actually happens, not "quiet on".
- A release note in `server/releases.json`, through `/voice-dna` before it
  lands.
- Spec entries: room0 has no section in `docs/plans/gliph-spec.md` at all, so
  this adds F17 with the bridge, the strip, the invite and quiet as separate
  acceptance lines.

## Order

Protocol doc and server first, because the frames are what everything else
reads. Then core with its tests, which can be written before either frontend
exists. Then web, then iOS. `plugins/e2e.mjs` after the protocol change,
`bin/ota` before calling the client work done.

One thing to watch once it is real: the strip holds about 34px above the
composer for as long as anyone is in the room. If that starts to read as a
toolbar, `hide` demotes it to a rail entry for the session.
