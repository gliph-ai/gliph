# Gliph

Text-first team chat with mIRC's soul. Go server, custom protocol over one
WebSocket, and **one client core in Rust** that every app is a frontend over:
the web client (wasm), the desktop app (Tauri around that same bundle), and a
native SwiftUI app on iOS (UniFFI).

Slash commands are **extensible at runtime**: plugins and AI agents register
their own commands over the wire, the server pushes a new palette to every
connected client, and clients hardcode nothing.

```
server/    Go server: event log, channels, modes, commands, uploads, plugins
core/      Rust client core: protocol, crypto, sync, offline cache, blob surface
web/       Vite + TypeScript frontend over the core (browser and desktop)
desktop/   Tauri v2 shell around web/dist
mobile/    Native SwiftUI frontend over the same core
plugins/   Plugin SDK + example bot + example Claude agent
docs/      protocol.md, plans/
design/    mockup artboards behind the published design canvas
```

## Run it

You need Go 1.26, Node 22, Rust with the `wasm32-unknown-unknown` target
(`rustup target add wasm32-unknown-unknown`) and `wasm-pack`
(`cargo install wasm-pack`). The web build compiles the Rust core to wasm on
the way.

```bash
# 1. server (first account created becomes admin). -plaintext is for a dev
#    box; a real server takes -tls-domain, or run `go run . -setup`.
cd server && go run . -data ./data -name my.gliph.chat -plaintext
# → listening on :8787

# 2. web client, in another shell
cd web && npm install && npm run dev
# → http://localhost:5184
```

Register at the client, pick any nick, leave the invite blank for the first
account. You land in `#general`. Type `/` to see the palette.

### Dev accounts (local server only)

The checkout's `server/data` already has these; never create them on prod.

Password for all of them: `Password123`.

| nick | role |
|---|---|
| `waseem` | admin |
| `demo` | member |
| `mika` | member |
| `pingbot` | member, the DM test sender |

Reset one with `cd server && go run . -data ./data -passwd <nick>:<new>`.

To serve the built client from the Go binary itself (single-process deploy):

```bash
cd web && npm run build
cd ../server && go run . -web ../web/dist
```

## Adding people

```bash
cd server && go run . -invite          # prints an invite token
```

Reset someone's password (sessions stay valid, so nobody is signed out):

```bash
cd server && go run . -passwd nick:newpassword
```

Give the token to the person; they enter it on the register screen.

## Commands

Every command below is enforced server-side by role, and the palette the client
renders comes from the server, so plugin commands appear the same way builtins do.

| | |
|---|---|
| `/me`, `/slap` | action lines |
| `/join`, `/part`, `/list` | channels; `/join` creates and makes you op |
| `/topic`, `/mode`, `/invite` | `+i` invite-only, `+m` moderated, `+k` key, `+t` op-topic |
| `/op`, `/deop`, `/voice`, `/devoice` | per-channel roles, scoped to that channel only |
| `/kick`, `/ban`, `/unban`, `/bans` | with reasons, announced in channel |
| `/nick`, `/away`, `/whois` | identity and presence |
| `/msg` | opens a 1:1 DM |
| `/ignore`, `/unignore`, `/clear` | client-side only, never sent to the server |
| `/help`, `/plugins` | what exists right now |

## Plugins and AI agents

A plugin is a WebSocket client that registers commands. Give it a token:

```bash
cd server && go run . -plugin deploybot:deploybot:agent
# → token: 9f3c…
```

`kind` is `bot` (commands only) or `agent` (commands **plus** the channel event
stream, so it can watch and act unprompted). Then:

```bash
cd plugins && npm install
GLIPH_TOKEN=9f3c… GLIPH_CHANNELS=general node deploybot.mjs
```

`/deploy` and `/builds` appear in every connected client's palette immediately,
tagged with the plugin that owns them. Close the plugin and they disappear again.

Writing one is about ten lines:

```js
import { plugin } from "./sdk.mjs";

const p = plugin({ url: "http://localhost:8787", token, kind: "agent",
                   display: "deploybot", subscribe: ["general"] });

p.command({ name: "deploy", usage: "/deploy <env>", desc: "ship it",
            args: [{ name: "env", type: "text", required: true }],
            min_role: "op" },
          async ({ env, actor }) => `shipping ${env} for ${actor}`);

p.on("message", (e, api) => { /* the agent sees the stream */ });

await p.connect();
```

`claude-agent.mjs` is the AI version: it registers `/ask` and `/summarise`,
keeps rolling channel context, and answers when addressed by name.

```bash
ANTHROPIC_API_KEY=… GLIPH_TOKEN=… node claude-agent.mjs
```

An agent is a **visible member**: it appears in the member list with its own
marker, it can only read channels it has been joined to, and removing it from a
channel cuts off its access. There is no silent listener.

## Tests

```bash
cd server && go test ./...          # command registry, roles, modes, event log
cd plugins && node e2e.mjs http://localhost:8787 <plugin-token>
```

The end-to-end script drives a real server over real sockets: register, sync,
post, run a builtin, connect a plugin, watch the palette grow, invoke the
plugin command, watch the palette shrink when it disconnects.

## Desktop and mobile

`desktop/` wraps `web/dist`, so `cd web && npm run build` first (which builds
the wasm core on the way). `mobile/` builds the same core for iOS and links it —
see `mobile/README.md`.

From the repo root:

```bash
bin/dev              # tmux workspace: server, web, deploybot, tauri dev, claude
bin/tauri dev        # just the desktop app (expects web on :5184)
bin/install-app      # build + sign + install Gliph.app into /Applications (macOS)
```

## Protocol

`docs/protocol.md` is the contract. Product plan and build spec are in
`docs/plans/`.

## License

Everything in this repository (server, core, web, desktop, mobile and plugins)
is dual-licensed under [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), at
your option. A contribution is offered under the same two terms unless you say
otherwise.
