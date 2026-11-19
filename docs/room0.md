# room0 — the 3D multiplayer office

A full-3D virtual office per channel: walk around, chat with game-style
bubbles, emote, push and carry objects, work at desks, lie on beach loungers.
Type **`/room0`** (alias **`/3d`**) in a channel to open that channel's world;
the participants are the channel's members. Ported whole from inbox2 — the
Three.js game engine moved untouched; the shell, transport and persistence
were rebuilt on gliph's own seams.

Beyond the office and its four side-rooms, doors lead onward to five
**procedural outdoor worlds** (see §Worlds): the open sea off the beach, dunes
behind the pantry, a campfire clearing past the garden, a space deck under the
lounge, and the terminal below the office.

## Source layout

```
web/src/room/
  room.ts            Vanilla-TS shell: the overlay (outside #app, codepad-
                     style), HUD (roster, chat, emote bar, prompts, touch
                     buttons), whiteboards, voice-note wall, presence wiring
                     over room.* frames. The game itself is imperative — one
                     instance per open.
  room.css           All HUD + CSS2D styling, every rule scoped under .r0.
                     room0 keeps its own dark palette regardless of the gliph
                     theme; the token values it needs are defined on .r0.
  engine.js          Pure room data: tile grids (FLOOR/WALL), props, doors,
                     spawns, walkability. 1 tile = 1 world unit.
  voiceNotes.js      MediaRecorder wrapper (webm/opus, mp4/AAC on Safari).
  three/game3d.js    The game: renderer/camera/input, player physics, dynamic
                     objects, seats, doors, daylight, presence sync, chat
                     bubbles, emotes, touch controls.
  three/world3d.js   Room builder: canvas-texture floors, instanced walls +
                     glowing trim, door portals, wall decorations, seats/
                     workstations, painted sky, daylight curve, tickWorld.
  three/worlds3d.js  The procedural outdoor worlds (kind:'world' rooms):
                     seeded terrain/water/sky generators, obstacle stamping,
                     return portals, per-frame update hooks.
  three/props3d.js   Prop meshes: procedural low-poly builders + the
                     generated-GLB pipeline (manifest, loading, fit/yaw
                     normalisation), dynamic-object physics metadata.
  three/avatar3d.js  Procedurally-animated minifig with real joints; look
                     strings, deterministic per-user palette.
  three/ambient3d.js WebAudio soundscapes, zero asset files.
  three/voice3d.js   Per-room voice: full-mesh WebRTC + spatial (PannerNode)
                     playback. No game knowledge.
web/public/room0/models/   Generated GLB props + manifest.json (~7.7MB).
web/scripts/shrink-glb.mjs GLB compressor (see Assets).
server/room.go             Commands, the relay, boards + voice-note storage.
server/room_test.go        Relay validation, bookkeeping, boards, notes.
```

Everything under `three/` plus `engine.js`/`voiceNotes.js` is plain JS
(`allowJs` in tsconfig) — deliberately untouched from the inbox2 version so
upstream fixes can be diffed across. The whole module (three.js included) is
one lazy chunk: client.ts dynamic-imports `room.ts` the first time a
`room.open` frame arrives, the Monaco deal.

## How /room0 opens it

`/room0` and `/3d` are server builtins (`server/room.go`, scope `channel`).
The handler sends `{"t":"room.open","d":{"channel":<id>}}` to the calling
connection only — the `Conn.Send` UI escape hatch, same shape as `/todos`'s
blob.focus. The web client's `peek()` catches it, lazy-imports the module and
mounts the overlay. iOS receives the frame and ignores it harmlessly (the
core drops unknown frame types); the feature is web/desktop-only, tracked as
`—` in [features.md](features.md).

## Worlds — the procedural outdoor rooms

Five `kind:'world'` rooms hang off the tile rooms by ordinary doors:

| world | door in | voice |
|---|---|---|
| The Open Sea — golden-hour water (Gerstner waves), jetty, rowable boat, lighthouse | Beachfront | live |
| The Dunes — ridged-noise dune field, Journey-style glitter sand, ambient buggy | The Pantry | live |
| The Campfire — midnight pines, lantern trail, fire with sparks, five seat logs | The Garden | live |
| The Space Deck — trim-ringed platform over a banded planet, nebulae, asteroids | Lounge | live |
| The Terminal — phosphor grid, wireframe hills, blinking cursor monolith, chat-log panels | The Office | quiet (vnote wall) |

How they differ from tile rooms, and what stays the same:

- **No tile grid.** Walkability is a radius around the room centre
  (`isWalkable`'s world branch in `engine.js`) minus `room._blocked`, a set of
  integer obstacle cells (tree trunks, fire pit, consoles, monolith) stamped
  by `worlds3d.js` when it builds the scene. Coordinates stay in the same
  tile-unit space, so presence frames, door checks and `visitPeer` all work
  unchanged.
- **Seeded per channel.** `room.ts` passes `worldSeed` (FNV-1a of the channel
  id) into the game; every generator draws from `mulberry32(seed ^ roomId)`,
  so all members deterministically see the same dunes, pines and panels. That
  determinism is the multiplayer contract — never use `Math.random()` for
  anything positional in a world.
- **Terrain height.** A world may expose `heightAt(x,z)` — the same function
  that displaced its ground mesh. game3d places the local avatar, peers, the
  tap marker and the camera on it (`terrainY` in the frame loop), so nothing
  about height rides the wire.
- **Own sky, fog and lights.** Worlds bring a sky dome, fog and their own
  light rig; game3d zeroes the tile rig, skips the daylight pipeline
  (`world.custom`), and raises `camera.far` to 900 while inside. The variant
  palettes are fixed for now (sea is always golden hour, camp always
  midnight); wiring them to `daylightAt` is the obvious upgrade.
- **The rowboat is a vehicle.** The sea world returns a `vehicle` (`worlds3d.js`):
  sit on a bench and the movement keys row the boat; its benches and blocked
  cells track the hull (`vehicle.sync()`). Position and heading sync over the
  ordinary `object` relay (`h` field, `docs/protocol.md`), last writer wins,
  stashed per game instance so the boat stays put across sub-room visits and
  is re-announced to joiners by whoever rowed it last.
- **The rest is inherited:** chat bubbles, emotes, spatial voice (loud
  worlds), the roster's click-to-walk-over teleport, heartbeat/eviction,
  ambient soundscapes (`ROOM_SOUND` entries in ambient3d.js).

The concept art these were built from lives in the "Room0 Atmosphere Lab"
artifact (session-published, six worlds × three palettes each).

## Multiplayer protocol (room.* frames)

Relay-only, nothing persisted — `handleRoomFrame` in `server/room.go`, wire
shapes in [protocol.md](protocol.md) §Room. The server keeps one piece of
state: which connections joined which channel's room (so it can reject
non-joined senders, and broadcast `leave` on disconnect). Membership is
re-read from the store per frame (the p2p.go precedent); payloads are rebuilt
field-by-field, never passed through; `name` is stamped server-side from the
user's nick; rate-limited via the `room` limiter (40/s, burst 80).

- `room.join {channel, room, x, y, h, anim, look}` → broadcast `join` to all
  members (sender included — clients drop their own echo by `user_id`).
- `room.send {channel, type, ...}` with type `move|announce|chat|object|emote|rtc`
  → relayed as `room.event`. `announce` relays as `move`. `chat` text capped
  at 200 and rendered as text, never HTML. `object` carries stable ids
  (`room:type:index`), last-writer-wins ownership, `holder:"me"` rewritten to
  the sender's id. `rtc {to, kind, payload}` is the voice-mesh signaling blob,
  broadcast-then-filtered client-side, payload never parsed.
- `room.leave {channel}` → broadcast `leave`; also fired server-side when the
  socket drops.

Liveness (client-side, `game3d.js`): moves throttled to 100ms and sent only
on change; 20s heartbeat announce on a real `setInterval` (hidden tabs keep
beating and report `anim:"away"`); 60s stale sweep evicts silent peers and
frees the objects they held. A `join` makes everyone re-announce themselves
and their owned objects — that's the only replay mechanism; there is no
join-state snapshot. On socket redial the shell sees the `hello` frame and
re-joins.

## Whiteboards + voice notes (persistence)

Per-channel, in the server's SQLite + data dir (`server/room.go`, tables in
`schema.sql`), bearer-authed and membership-checked like `/api/file`:

- `GET/PUT /api/room/board?channel=<id>&kind=tasks|drawing` — one row per
  (channel, kind), JSON `data`, 512KB cap, last writer wins. The office
  whiteboards open these on F: **This week** (Trello-style sticky lanes,
  `{lanes:[...]}`, 0.8s debounced saves) and **Sketch board** (embedded
  draw.io via its JSON postMessage protocol, `{xml}`, 1.2s debounce —
  `embed.diagrams.net` is whitelisted in both CSPs' frame-src).
- `GET/POST /api/room/notes?channel&room`, `DELETE /api/room/notes/{id}`,
  `GET /api/room/notes/{id}/audio` — the async voice-note wall for quiet
  rooms (office, garden). ≤120s, ≤5MB, 50 per room with overflow reaped
  (rows and files; audio bytes live at `<data>/roomnotes/<id>`). Creates and
  deletes broadcast `{type:"vnote", op:"new"|"del"}` as room.event so open
  clients update live; the author applies their own note from the POST
  response (own echoes are dropped). Delete: author, or channel op/admin.
  Audio streams through the server (bearer auth; the progress bar uses the
  stored `duration_ms` — Chromium webm blobs report `Infinity`). Played ids
  persist in localStorage (`gliph.room0`), capped at 300.

## Voice channels

Loud rooms (kitchen, lounge, beach) get live per-room voice; quiet rooms
(office, garden) get the wall instead — entering one while joined hangs up.
Full-mesh WebRTC audio (`voice3d.js`), right up to ~10 people: signaling
rides the `rtc` relay; who-is-in-voice rides the `voice` flag on
move/announce so the heartbeat doubles as membership discovery. Pairing rule:
the lexicographically **lower** user id offers, the higher answers — glare is
structurally impossible; an incoming offer for a live pair means the
initiator rebuilt after a failure (drop, answer fresh). ICE candidates
arriving early are queued per-pair and flushed after `setRemoteDescription`;
`failed` drops the pair and the 5s `voiceSync()` re-initiates. Spatial audio:
each remote stream feeds a PannerNode (HRTF, inverse distance) pinned to the
speaker's avatar; the listener tracks the player every frame.

Gotchas ported verbatim — do not simplify away:

- Chromium mutes a remote WebRTC stream inside WebAudio unless it is also
  attached to a (muted) `<audio>` element. Each pair keeps one.
- Safari lacks the AudioParam form of listener/panner setters — fall back to
  `setPosition()/setOrientation()`.
- Avatar looks: `setLook()` re-dresses with per-avatar materials, never the
  shared `mat()` cache (that repaints everyone). Hash math uses `>>>`, not
  `>>` (signed shifts produced undefined palette indices).

ICE comes from `GET /api/ice` — the same coturn-shaped endpoint the p2p
transfers use (`server/ice.go`: `use-auth-secret` ephemeral credentials, so
a self-hosted coturn works with just the `-stun`/`-turn`/`-turn-secret`
server flags; `deploy/prod` already has the tfvars). room.ts fetches the
list on open and hands it to `voice3d.setIceServers()`; if the server
configures nothing (or the fetch fails) voice falls back to a hardcoded
public-STUN pair, which covers every non-symmetric-NAT pairing. Symmetric-NAT
pairs need a real coturn behind those flags — TURN needs raw public UDP, so
the relay runs beside the cluster, not behind Cloudflare's proxy.
Desktop (WKWebView) mic capture needs the macOS
`NSMicrophoneUsageDescription` plist entry before `getUserMedia` works there;
browser-first for now.

## Assets — the Higgsfield → GLB pipeline

Hero props are generated, not modelled: `generate_image` (one isolated prop,
"stylized low-poly 3D game prop render", navy/cyan palette) →
`generate_3d` (Meshy image-to-3d, textured, ~12k polys) →
`node web/scripts/shrink-glb.mjs raw.glb out.glb` (strips PBR maps,
re-encodes base color to ≤1024px JPEG via macOS `sips`; ~9MB → ~0.5MB) →
drop into `web/public/room0/models/<type>.glb` and add the type to
`manifest.json`. Types not in the manifest render their procedural fallback
from `props3d.js`. On load each mesh is uniform-scaled into a per-type
`PROP_FIT` box, yawed to the +Z-front convention (`MODEL_YAW`), re-centred
and dropped to y=0.

## Debug & testing

- `window.__room0()` → `{room, x, z, keys, anim, fade, hour}`;
  `window.__room0.setHour(h)`; `?hour=22` URL param for the daylight curve;
  `__room0.go('sea')` jumps to a room's spawn; `__room0.tp(x, z)` teleports.
- Headless smoke pattern: `vite preview` → headless Chrome with
  `--enable-unsafe-swiftshader` (software WebGL) → drive via CDP and poll
  `__room0()`. WebGL-unavailable fails soft with a message.
- `web/room0-smoke.html` (dev server only, not a build input) mounts the game
  with no transport — combined with `__room0.go`/`tp` and CDP key events it
  smoke-tests every room, world walkability and visitPeer without a server.
- Server side: `server/room_test.go` covers relay field rebuild/caps, the
  holder rewrite, join/leave/disconnect bookkeeping, throttling, board
  upsert + size cap, note create/reap/delete permissions.
