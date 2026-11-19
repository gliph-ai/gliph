// Gliph plugin SDK. Zero dependencies: Node 22+ has a global WebSocket.
//
//   import { plugin } from "./sdk.mjs";
//
//   const p = plugin({ url: "ws://localhost:8787", token: process.env.TOKEN });
//   p.command({ name: "deploy", usage: "/deploy <env>", desc: "ship it",
//               args: [{ name: "env", type: "text", required: true }] },
//             async ({ env, actor }) => `shipping ${env} for ${actor}`);
//   p.on("message", (e) => { ... });
//   await p.connect();

import { mcp } from "./mcp.mjs";

// `agent`: the product behind this plugin's lines (claude, openai, gemini,
// cursor, copilot, github, gitlab). Clients draw its real logo beside them.
export function plugin({ url, token, kind = "bot", display, subscribe = [], agent = "" }) {
  const commands = new Map();
  const handlers = new Map();
  const blobs = new Map();   // id -> spec
  const blobImpl = new Map(); // id -> { open, event }
  let ws = null;
  let retry = 0;

  const send = (t, d) => ws?.readyState === 1 && ws.send(JSON.stringify({ t, d }));

  const api = {
    /** Register a command. The server merges it into the palette and pushes
     *  the new manifest to every connected client immediately. */
    command(spec, handler) {
      if (!spec?.name) throw new Error("command needs a name");
      commands.set(spec.name, {
        name: spec.name,
        usage: spec.usage ?? `/${spec.name}`,
        desc: spec.desc ?? "",
        args: spec.args ?? [],
        scope: spec.scope ?? "channel",
        min_role: spec.min_role ?? "member",
      });
      handlers.set(spec.name, handler);
      if (ws?.readyState === 1) register();
      return api;
    },

    /** Register a blob: an interactive surface attached to a channel, rendered
     *  from the GUM document your handlers return.
     *
     *    p.blob({ id: "servers", title: "servers", mode: "private",
     *             channels: ["ops"] },
     *           { open:  async (ctx)     => doc,
     *             event: async (ctx, ev) => doc | { menu: [...] } | undefined });
     *
     *  ctx = { inst, channel, actor, role }. Returning a doc re-renders,
     *  returning { menu } pops a submenu, returning nothing leaves the screen
     *  alone (useful when you posted to chat instead). */
    blob(spec, impl) {
      if (!spec?.id) throw new Error("a blob needs an id");
      blobs.set(spec.id, {
        id: spec.id,
        title: spec.title ?? spec.id,
        mode: spec.mode ?? "private",
        channels: spec.channels ?? [],
      });
      blobImpl.set(spec.id, impl);
      if (ws?.readyState === 1) register();
      return api;
    },

    /** Replace a shared blob's document for everyone in a channel. */
    publish(channel, blobId, doc) {
      send("publish", { channel, blob: blobId, doc });
    },

    /** Listen to channel events. Agents only (kind: "agent"). */
    on(kindOrFn, maybeFn) {
      const k = typeof kindOrFn === "string" ? kindOrFn : "*";
      const fn = typeof kindOrFn === "string" ? maybeFn : kindOrFn;
      const list = api._listeners.get(k) ?? [];
      list.push(fn);
      api._listeners.set(k, list);
      return api;
    },
    _listeners: new Map(),

    /** Post into a channel this plugin is a member of. */
    say(channel, body) { send("say", { channel, body, agent }); },

    /** Connect to an MCP tool server (spec F11).
     *
     *    const fs = await p.mcp({ name: "fs", tier: "read",
     *      command: "npx", args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv"] });
     *    await fs.call("read_file", { path: "/srv/notes.md" });
     *
     *  `announceIn` names the channel where tool calls that can MUTATE get a
     *  line. gliph's position is that agents are visible members rather than
     *  ambient magic, and an agent quietly changing something in the world
     *  would be the one part of that nobody can see. Reads stay quiet.
     *
     *  Default tier is `read`: an agent gets the least it can be given until
     *  someone decides otherwise, in writing, at the call site. */
    async mcp(spec) {
      const channel = spec.announceIn;
      return mcp({
        ...spec,
        announce: channel ? (line) => api.say(channel, line) : spec.announce ?? null,
      });
    },

    connect() {
      return new Promise((resolve, reject) => {
        ws = new WebSocket(`${url.replace(/^http/, "ws")}/plugin?token=${encodeURIComponent(token)}`);

        ws.addEventListener("open", () => { retry = 0; register(); });

        ws.addEventListener("message", async (m) => {
          const { t, d } = JSON.parse(m.data);

          if (t === "ready") {
            console.log(`[gliph] registered as ${d.id} with ${d.commands} commands`);
            resolve(api);
            return;
          }

          if (t === "invoke") {
            const fn = handlers.get(d.name);
            if (!fn) return send("fail", { call: d.call, message: `no handler for /${d.name}` });
            try {
              const say = await fn({ ...d.arg_map, actor: d.actor, channel: d.channel, raw: d.args });
              send("result", { call: d.call, say: say ?? "", agent });
            } catch (err) {
              send("fail", { call: d.call, message: err?.message ?? String(err) });
            }
            return;
          }

          if (t === "blob_open" || t === "blob_event") {
            const impl = blobImpl.get(d.blob);
            const ctx = { inst: d.inst, channel: d.channel, actor: d.actor, role: d.role };
            if (!impl) return send("blob_fail", { call: d.call, message: `no handler for blob ${d.blob}` });
            try {
              const out = t === "blob_open"
                ? await impl.open?.(ctx)
                : await impl.event?.(ctx, { action: d.action, item: d.item, value: d.value });
              if (out?.menu) send("menu", { call: d.call, item: d.item ?? "", actions: out.menu });
              else if (out) send("render", { call: d.call, doc: out });
              // returning nothing means "nothing changed on screen"
              else send("render", { call: d.call, doc: null });
            } catch (err) {
              send("blob_fail", { call: d.call, message: err?.message ?? String(err) });
            }
            return;
          }

          if (t === "event") {
            for (const fn of api._listeners.get(d.kind) ?? []) await fn(d, api);
            for (const fn of api._listeners.get("*") ?? []) await fn(d, api);
            return;
          }

          if (t === "error") console.error("[gliph] server:", d.message);
        });

        ws.addEventListener("close", () => {
          retry = Math.min(retry + 1, 6);
          const wait = 500 * 2 ** retry;
          console.warn(`[gliph] disconnected, retrying in ${wait}ms`);
          setTimeout(() => api.connect().catch(() => {}), wait);
        });

        ws.addEventListener("error", (e) => {
          if (retry === 0) reject(new Error(`cannot reach ${url}: ${e.message ?? "connection failed"}`));
        });
      });
    },
  };

  function register() {
    send("register", {
      display, kind, subscribe,
      commands: [...commands.values()],
      blobs: [...blobs.values()],
    });
  }

  return api;
}
