# Gliph Blobs & GUM — design and implementation plan

> Status: DESIGN FOR REVIEW — nothing here is built yet.
> Covers: daily check-ins, channel todos, pins, devlog with git watcher,
> devops/server status — and the generic framework they all sit on.

## 0. The one idea

All five processes are the same thing wearing different hats: **a channel gets
attached mini-apps ("blobs") that a provider renders and users drive from the
keyboard**. So we do not build five features. We build one framework and then
five small apps on it:

- **Blob** — a named interactive surface attached to a channel. Users flip into
  it from chat (like switching windows in mIRC) and back with esc.
- **Provider** — whoever renders a blob and handles its events. Two kinds,
  one contract: *builtin* providers compiled into the server (check-ins, todos,
  pins, devlog) and *plugin* providers over the existing plugin socket (devops,
  and anything third parties build).
- **GUM (Gliph UI Markup)** — the declarative JSON a provider pushes and every
  client renders. The client hardcodes no app UI, exactly as it hardcodes no
  commands. mSL had `/dialog` custom dialogs; GUM is its spiritual successor.

The trust model is copied from the command registry, because it already works:
providers declare, the server enforces (roles, membership), clients only render.
A provider can never reach a channel it is not a member of, and an action marked
`min_role: "op"` is refused server-side no matter what the client sends.

Everything below is additive. No existing frame, table, or client behaviour
changes.

## 1. Where blobs live in the UI

- The channel title strip grows a **blob switcher**: `chat · todos · pins ·
  servers`. Chat is always first. Keyboard: `ctrl+1..9` jumps (chat is 1 —
  `Client::blob_switch` decides which app is nth, so every client agrees),
  `esc` always returns to chat, `/blobs` lists.
- A blob fills the same area the message stream occupies (the composer is
  replaced by the blob's own focus model — see §5). The right rail stays.
- Mobile: same switcher as a horizontal scroll row under the title strip. Same
  bundle, same renderer, all three platforms — nothing is injected per-app;
  the renderer is part of the shared web client, so Tauri gets it for free.
  (Now: `core/src/gum.rs` flattens a Doc into a platform-neutral tree with the
  selection, numbering and menus resolved, and each frontend paints it — DOM in
  `web/src/gum.ts`, SwiftUI in `mobile/Sources/Views/BlobView.swift`.)
- Slash commands stay first-class: every app exposes both. `/todo add buy milk`
  never requires opening the blob; the blob is the browsing/bulk surface.

## 2. Instances: shared documents vs private sessions

Two render targets, because the five apps genuinely split this way:

| mode | what it means | used by |
|---|---|---|
| `shared` | One document per (channel, blob). Provider publishes; every member sees the same thing, live. Cached offline like chat. | todos, pins, check-ins, devlog |
| `private` | One session per (user, channel, blob), opened on demand. Browsing state (your filters, your submenu) is yours alone. Requires connectivity. | devops server browser |

A shared blob can still have per-user interactions (ticking a todo) — the
interaction goes to the provider, the provider updates state and publishes a
new document to everyone.

## 3. GUM — the markup

A GUM document is JSON: `{"title": "...", "blocks": [...]}`. Seven block
types. Deliberately no layout primitives (no rows/columns/grids): blocks stack
vertically in a monospace column, which is the aesthetic anyway. If a provider
needs columns it uses a `table`.

```jsonc
{
  "title": "servers",
  "blocks": [
    { "t": "text", "body": "3 servers · 1 alert", "style": "info" },       // plain | info | warn | error
    { "t": "divider", "label": "production" },
    { "t": "list", "id": "srv", "items": [
        { "id": "web-1", "label": "web-1", "hint": "ok · 12ms · 41d up",
          "state": "ok",                       // ok | warn | error | done | open — renders a status dot/checkbox
          "actions": [
            { "id": "status",  "label": "show status" },
            { "id": "logs",    "label": "tail logs" },
            { "id": "restart", "label": "restart", "min_role": "op",
              "confirm": "restart web-1? active connections will drop." }
          ] },
        { "id": "web-2", "label": "web-2", "hint": "DEGRADED · 480ms", "state": "warn", "actions": [ ... ] }
      ] },
    { "t": "table", "id": "alerts", "cols": ["time", "server", "alert"],
      "rows": [["14:02", "web-2", "p95 latency over budget"]] },
    { "t": "input", "id": "note", "label": "annotate", "placeholder": "what did you do?",
      "action": "annotate" },                  // submitting fires this action with the value
    { "t": "buttons", "actions": [
        { "id": "refresh", "label": "refresh" },
        { "id": "add",     "label": "add server", "min_role": "op", "prompt": "hostname" } ] },
    { "t": "bar", "body": "enter: menu · r: refresh · esc: back" }
  ]
}
```

Rules that keep it sane:

- **Bodies are plain text.** No HTML, no markdown. The client escapes
  everything. Links: `text` and `hint` auto-link bare URLs exactly as chat does
  (client-side, server never fetches).
- **Actions are the only way anything happens.** An action has `id`, `label`,
  and optionally `min_role` (member|op|admin, enforced server-side against the
  caller's channel role), `confirm` (client shows it, server does not care —
  role enforcement is the real gate), and `prompt` (client collects a one-line
  value and sends it as the action's `value`).
- **A provider answers an event with one of three things**: a full document
  (re-render), a `menu` (transient list of actions the client shows as a popup
  over the current document — this is how submenus like "assign to whom?"
  work), or nothing (state unchanged; maybe it posted to chat instead).
- **Documents are small and rendered whole.** No partial-update protocol, no
  diffing on the wire. A server list with 200 rows is a few KB of JSON. The
  client is responsible for not losing focus position across re-renders (§5).
  `// ponytail: full-document render; add row-level patching only if a real app
  // exceeds ~100KB documents.`

## 4. Wire protocol additions

### Client ↔ server

| dir | type | payload |
|---|---|---|
| → | `blob.open` | `{channel, blob}` — opens (private: creates a session; shared: subscribes) |
| → | `blob.event` | `{channel, blob, action, item?, value?}` |
| ← | `blob.render` | `{channel, blob, doc}` — a GUM document (session render or shared publish) |
| ← | `blob.menu` | `{channel, blob, item, actions: [...]}` — transient submenu |
| ← | `blob.focus` | `{channel, blob}` — "open this app", sent to the ONE connection that ran a command like `/todos`. The only server-to-client frame that moves the UI. |

There is deliberately no `blob.close`. An earlier draft of this table had one;
nothing ever needed it. A private blob's session id is derived
(`channel:blob:user`), not stored, so closing frees nothing, and no provider
polls. Add it the day one does.

The channel state (`state` frame) gains `blobs: [{id, title, mode, source}]`
per channel, so the switcher renders from server-pushed data exactly like the
command palette. When a plugin registers or drops blobs, the affected channels
get a fresh `state` — same broadcast mechanism as `commands` today.

### Plugin ↔ server (additions to the existing `/plugin` socket)

`register` gains a `blobs` array:

```json
{ "t": "register", "d": {
    "commands": [...],
    "blobs": [
      { "id": "servers", "title": "servers", "mode": "private", "channels": ["ops"] }
    ] } }
```

| dir | type | payload |
|---|---|---|
| ← | `blob_open` | `{inst, channel, blob, actor, role}` — a user opened a private blob (or first subscriber of a shared one) |
| ← | `blob_event` | `{inst, channel, blob, actor, role, action, item?, value?}` |
| → | `render` | `{inst, doc}` — answer to open/event for a private session |
| → | `menu` | `{inst, item, actions}` — transient submenu |
| → | `publish` | `{channel, blob, doc}` — replace the shared document for everyone |

`inst` is a server-generated session id for private blobs; for shared blobs it
identifies the subscription round-trip. The server checks membership and
`min_role` **before** forwarding `blob_event`, and stamps `actor`/`role`
itself — a provider never trusts the client's word for who clicked.

### Builtin providers — the same contract, in-process

```go
// server/blobs.go
type Provider interface {
    // Open returns the initial document for a session (private) or the
    // current document (shared).
    Open(ctx *BlobCtx) (*Doc, error)
    // Event handles an action. Return values: doc (re-render), menu, or
    // neither (no visual change).
    Event(ctx *BlobCtx, ev BlobEvent) (*Doc, *Menu, error)
}
```

The router keeps `map[viewKey]Provider`. A plugin-backed blob is a thin
`Provider` implementation that forwards over the socket and awaits the reply
(30s timeout, same as command invokes). **This interface is the whole
abstraction**: check-ins, todos, pins and devlog are Go structs implementing
it; devops is a socket forwarding it. Nothing above the router knows which is
which.

### Shared-document persistence

`blob_docs(channel_id, view_id, doc JSON, updated_at)` — one row per shared
blob, replaced on publish. Clients cache the last doc in localStorage next to
the event cache, so shared blobs render offline.

## 5. Client renderer and keyboard model

One TS module (`web/src/gum.ts`) renders a Doc to DOM with the existing theme
tokens — the same monospace, navy/creme, sharp-corner language. Written once,
paints the tree `core/src/gum.rs` emits. The keyboard model below is the core's
too, so a blob's keys mean the same thing on every client by construction —
`core/tests/gum_behaviour.rs` is what holds that promise.

Keyboard (the framework owns this; apps never define keys):

- `↑/↓` or `j/k` — move selection through focusable blocks (list items,
  buttons, inputs). `1..9` quick-jump list items.
- `enter` — on a list item: open its action menu (client-side popup built from
  the item's `actions`, same look as the command palette). On a button: fire.
  On an input: submit.
- `esc` — close menu → clear input → leave blob (in that order).
- `tab` — next input.
- Touch: tap = enter, long-press = action menu; the same popup component.

Re-render focus rule: the renderer keys selection by `(block id, item id)` and
restores it after swapping the DOM; if the item vanished, nearest neighbour.
Confirm dialogs and prompts are framework popups, not per-app markup.

Failure states the framework renders (apps get them free): provider
disconnected ("*** servers is unavailable: deploybot is not connected"),
event timeout, and role-refused.

## 6. The five apps

### 6.1 Check-ins (builtin, shared)

- Commands: `/checkin <what I'm doing today>` (also aliased `/standup`),
  `/checkin` alone shows today's board as a notice.
- Blob `checkins`: today's board — one row per member: nick, their line, time;
  a "not yet" section listing members who haven't checked in; yesterday
  collapsible below (divider + rows).
- Actions: on your own row — edit (prompt), clear. Op action on the blob:
  `remind` (posts "*** 3 people have not checked in yet: …" to chat).
- Data: `checkins(channel_id, user_id, day, body, ts)`, upsert per day.
- Chat visibility: a check-in posts a system-style event to the channel
  (`*** waseem checked in: MLS key rotation`) so the channel log IS the
  history; the blob is just today, organised.
- Not building: scheduled reminders/cron. Unpark when asked; `remind` covers it
  manually.

### 6.2 Todos (builtin, shared)

- Commands: `/todo <text>` add · `/todo done <n>` · `/todos` opens the blob.
- Blob `todos`: open items as a checkbox list (`state: open|done`), done items
  under a divider (last 20). Item actions: toggle done, assign (provider
  returns a `menu` of channel members), delete (op or author),
  edit (prompt).
- Item label renders assignee as a `@nick` suffix in the hint.
- Data: `todos(id, channel_id, body, author, assignee, done, created_at,
  done_at)`.
- Chat visibility: add/done post system events (`*** waseem completed: ship the
  freeze build`).

### 6.3 Pins (builtin, shared)

- Commands: `/pin <text or url>` · `/pin <seq>` pins an existing message by its
  visible timestamp-order number (client sends the event id) · `/pins` opens
  the blob.
- Blob `pins`: list, newest first; URLs auto-linked; pinned-message pins show
  `<nick> original text…` as the label. Actions: open link, unpin (op or
  pinner), copy.
- Data: `pins(id, channel_id, body, event_id?, author, ts)`.
- Chat: pin/unpin post system events, mIRC-style ceremony.

### 6.4 Devlog (builtin, private + webhook ingestion)

> Registered `private`, not `shared` as §2's table says: the blob is a
> per-viewer filtered lens over the channel's own events, so the document
> differs by who is looking. See `server/devlog.go`.

The devlog is deliberately mostly *the channel itself* — commits and log
entries are events in the normal stream, so offline cache, search-by-scroll
and the flat log all just work. The blob is a filtered lens.

- **Git watcher**: `POST /api/hooks/git/{hook_token}` accepts GitHub and
  GitLab push webhooks (detected by header: `X-GitHub-Event` /
  `X-Gitlab-Event`; secret verified — GitHub HMAC signature, GitLab token
  header). Each push becomes one `commit` event per push (not per commit)
  in the linked channel:
  `*** push to gliph/main by waseem — 3 commits: fix cursor replay; …`
  with `meta: {repo, branch, pusher, commits: [{id, msg, url}]}` so the
  client renders a card with per-commit lines linking out.
- `/hook new` (op) mints a hook token for the channel and prints the URL to
  paste into GitHub/GitLab. `/hook list`, `/hook revoke <id>`.
- **Manual entries**: `/log ran ANALYZE on prod db — 40s, lock-free` posts a
  `log` event (distinct colour in chat, mode-line family).
- Blob `devlog`: the channel's `commit` + `log` events, newest first,
  filterable by an input (matches repo, branch, author, text). Actions on a
  commit row: open in browser, copy sha.
- Data: `hooks(id, channel_id, token, kind, created_by)`. No devlog table —
  the event log is the store; the blob queries `events WHERE kind IN
  ('commit','log')`.

### 6.5 Devops / server status (plugin, private) — the reference app

Ships in `plugins/devops.mjs` as the worked example of the whole framework,
with a stub backend (a JSON file of servers + fake latencies) and a clearly
marked seam where real APIs (SSH, Prometheus, your PaaS) plug in.

- Registers blob `servers` (`mode: private`) on `#ops` plus commands
  `/servers` (opens the blob), `/status <name>` (chat-only quick path — prints
  a status block as a notice, no blob needed).
- Flow, exactly as you described: open blob → list of servers with state dots →
  enter on one → menu: show status / tail logs / restart → "show status"
  re-renders with a `table` of metrics for that server and a back button;
  "restart" is `min_role: op` + confirm, invokes the backend API, re-renders
  the list, and posts `*** deploybot restarted web-1 (asked by waseem)` to
  chat so there is an audit trail where everyone lives.
- Alerts: the plugin watches its backend and `publish`es nothing — alerts go
  to chat via the existing `say` as warn-styled lines, because alerts are
  events, and the blob shows current state when you open it. (A shared
  "alerts" blob can be added later with zero framework change.)

### SDK additions (`plugins/sdk.mjs`)

```js
p.blob(
  { id: "servers", title: "servers", mode: "private", channels: ["ops"] },
  {
    open:  async (ctx)      => gumDoc(await backend.list()),
    event: async (ctx, ev)  => {
      if (ev.action === "restart") { await backend.restart(ev.item); p.say(ctx.channel, `restarted ${ev.item} (asked by ${ev.actor})`); }
      if (ev.action === "status")  return statusDoc(await backend.status(ev.item));
      return gumDoc(await backend.list());
    },
  });
```

`ctx` = `{inst, channel, actor, role}`. Returning a doc renders; returning
`{menu: [...]}` pops a submenu; returning nothing leaves the screen alone.

## 7. Security

- Membership: a provider's blobs only attach to channels it is a member of;
  `blob.open`/`blob.event` are refused for non-members of the channel. Same
  rule and code path as `say` today.
- Roles: `min_role` on actions enforced by the server before forwarding, using
  the caller's channel role. Clients grey out; the server is the gate.
- Webhooks: per-channel random tokens in the URL plus provider-side signature
  verification (GitHub HMAC-SHA256, GitLab secret token). A bad signature is
  a 403 and never touches the channel. Hook tokens are revocable.
- GUM is data: bodies rendered as text, URLs linkified with the existing
  escaper, action ids validated against the last-rendered document (an event
  naming an action the current doc never offered is refused — prevents a
  malicious client replaying privileged action ids the provider stopped
  rendering). `prompt`/`input` values reach providers as untrusted strings.
- Private blob sessions die with the socket; the server caps sessions per user
  per blob (2) and total live private sessions per provider (64).

## 8. Implementation plan (ordered, one scope)

Build order is dependency order; each step lands with its check.

1. **Framework — server** (`server/blobs.go`, ~400 lines): Provider interface,
   registry keyed off channel+blob, session table (in-memory), routing of
   `blob.open/event/close`, role/membership enforcement, `blob_docs`
   persistence + publish fan-out, plugin `blobs` registration + the socket
   forwarding Provider, state-frame `blobs` field.
   *Check: a Go test registers a fake provider, opens a blob, fires an event
   with too-low a role (refused) and enough role (routed), publishes, and sees
   the doc fan out to two fake members.*
2. **Framework — client** (`web/src/gum.ts` + wiring, ~450 lines): renderer for
   the seven blocks, keyboard model, focus preservation, menus/confirm/prompt
   popups, blob switcher in the title strip, offline cache of shared docs,
   provider-gone states.
   *Check: driven in Chrome against a live server: open, navigate by keys,
   fire a confirmed op action, watch a publish re-render without losing
   selection.*
3. **SDK** (`plugins/sdk.mjs` +60 lines): `p.blob()`, docs in README.
   *Check: e2e.mjs grows a section — register a blob from a fake plugin, open
   it from a fake client socket, click, assert the render round-trip.*
4. **Todos** then **Pins** (builtin, each ~150 lines server): the two simplest
   shared apps; they prove shared publishing and menus (assign).
   *Check: Go tests over the data ops; browser pass for the blobs.*
5. **Check-ins** (~120 lines): daily upsert semantics + not-yet section.
6. **Devlog** (~250 lines): hook tokens + `/hook`, webhook endpoint with both
   providers' signature checks, commit event rendering in chat (card), `/log`,
   the filtered blob.
   *Check: replay a captured real GitHub and GitLab push payload against the
   endpoint (fixtures in repo), assert the event lands with correct meta and a
   bad signature 403s.*
7. **Devops reference plugin** (`plugins/devops.mjs`, ~200 lines with the stub
   backend): the full worked example, including the audit-to-chat pattern.
   *Check: extend e2e to drive it: list → status → restart (as op) → refused
   (as member).*
8. **Docs**: README section, `docs/protocol.md` §blobs + §GUM, spec checkboxes
   added to `gliph-spec.md` under a new F16 with the acceptance criteria above.

Rough size: ~1.6k lines across server/client/SDK/apps. No new dependencies.

## 9. Decisions I want your eyes on

1. **Names**: the surfaces are **blobs** (your call, applied throughout);
   the markup stays "GUM" unless you want that renamed too.
2. **Alerts as chat lines, not a badge system.** I kept devops alerts as
   warn-styled channel events (mIRC soul: everything is the log). If you want
   unread-style badges on the blob tab, that is a small framework addition —
   say so and I'll spec it.
3. **Check-in reminders are manual** (`remind` op action), no scheduler in v1.
4. **Per-push, not per-commit devlog events** — a 30-commit push is one line
   with an expandable card, not 30 lines. Flip if you want per-commit.
5. **No free-form layout in GUM** — vertical blocks + tables only, on purpose.
   The moment we add rows/columns we are reimplementing HTML badly.
6. **Server-scoped blobs**: v1 attaches blobs to channels only. A "server
   status" that should exist org-wide just lives in #ops. If you want blobs
   outside any channel (a home-screen dashboard), that is v2.
