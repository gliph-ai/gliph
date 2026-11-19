// The reference blob: a devops server browser.
//
//   node devops.mjs
//
// Env: GLIPH_URL, GLIPH_TOKEN (required), GLIPH_CHANNELS (default "ops")
//
// This is the worked example of the whole framework — open a list, pick a
// server, get a menu, run an action that calls a real API. The backend below is
// a stub with fake metrics; everything marked BACKEND is the seam where a real
// one (SSH, Prometheus, your PaaS) plugs in without touching the blob code.

import { plugin } from "./sdk.mjs";

const url = process.env.GLIPH_URL ?? "http://localhost:8787";
const token = process.env.GLIPH_TOKEN;
const channels = (process.env.GLIPH_CHANNELS ?? "ops").split(",");

if (!token) {
  console.error("set GLIPH_TOKEN — create one with: go run . -plugin devops:devops:agent");
  process.exit(1);
}

// ---- BACKEND (stub) ---------------------------------------------------------
// Replace these four functions with real calls. Nothing above them cares how
// they are implemented, only that they resolve.

const servers = [
  { name: "web-1", env: "production", state: "ok", latency: 12, uptime: "41d" },
  { name: "web-2", env: "production", state: "warn", latency: 480, uptime: "41d" },
  { name: "db-1", env: "production", state: "ok", latency: 3, uptime: "112d" },
  { name: "web-staging", env: "staging", state: "ok", latency: 21, uptime: "6d" },
];

const backend = {
  list: async () => servers,
  status: async (name) => {
    const s = servers.find((x) => x.name === name);
    if (!s) throw new Error(`no server named ${name}`);
    return {
      ...s,
      cpu: `${(Math.random() * 60 + 5).toFixed(0)}%`,
      memory: `${(Math.random() * 40 + 30).toFixed(0)}%`,
      disk: `${(Math.random() * 30 + 40).toFixed(0)}%`,
    };
  },
  logs: async (name) => [
    `[${new Date().toISOString()}] ${name} GET /healthz 200 2ms`,
    `[${new Date().toISOString()}] ${name} GET /api/state 200 14ms`,
  ],
  restart: async (name) => {
    const s = servers.find((x) => x.name === name);
    if (!s) throw new Error(`no server named ${name}`);
    s.state = "ok";
    s.latency = Math.round(Math.random() * 20 + 5);
    s.uptime = "0d";
    return true;
  },
};

// ---- GUM documents ----------------------------------------------------------

const dot = { ok: "ok", warn: "warn", error: "error" };

async function listDoc() {
  const all = await backend.list();
  const byEnv = {};
  for (const s of all) (byEnv[s.env] ??= []).push(s);

  const blocks = [
    {
      t: "text",
      style: all.some((s) => s.state !== "ok") ? "warn" : "info",
      body: `${all.length} servers · ${all.filter((s) => s.state !== "ok").length} needing attention`,
    },
  ];
  for (const [env, list] of Object.entries(byEnv)) {
    blocks.push({ t: "divider", label: env });
    blocks.push({
      t: "list",
      id: env,
      items: list.map((s) => ({
        id: s.name,
        label: s.name,
        hint: `${s.state} · ${s.latency}ms · up ${s.uptime}`,
        state: dot[s.state] ?? "ok",
        actions: [
          { id: "status", label: "show status" },
          { id: "logs", label: "tail logs" },
          {
            id: "restart",
            label: "restart",
            // The server enforces this; the client only greys it out.
            min_role: "op",
            confirm: `restart ${s.name}? active connections will drop.`,
          },
        ],
      })),
    });
  }
  blocks.push(
    { t: "buttons", actions: [{ id: "refresh", label: "refresh" }] },
    { t: "bar", body: "enter: menu · esc: back to chat" },
  );
  return { title: "servers", blocks };
}

async function statusDoc(name) {
  const s = await backend.status(name);
  return {
    title: `${name} status`,
    blocks: [
      { t: "text", style: s.state === "ok" ? "info" : "warn", body: `${name} — ${s.state}` },
      {
        t: "table",
        cols: ["metric", "value"],
        rows: [
          ["environment", s.env],
          ["latency", `${s.latency}ms`],
          ["uptime", s.uptime],
          ["cpu", s.cpu],
          ["memory", s.memory],
          ["disk", s.disk],
        ],
      },
      { t: "buttons", actions: [{ id: "back", label: "back to servers" }] },
      { t: "bar", body: "esc: back to chat" },
    ],
  };
}

async function logsDoc(name) {
  const lines = await backend.logs(name);
  return {
    title: `${name} logs`,
    blocks: [
      { t: "text", style: "info", body: `last ${lines.length} lines from ${name}` },
      { t: "table", cols: ["line"], rows: lines.map((l) => [l]) },
      { t: "buttons", actions: [{ id: "back", label: "back to servers" }] },
    ],
  };
}

// ---- wiring -----------------------------------------------------------------

const p = plugin({ url, token, kind: "agent", display: "devops", subscribe: channels });

p.blob(
  { id: "servers", title: "servers", mode: "private", channels },
  {
    open: () => listDoc(),
    event: async (ctx, ev) => {
      switch (ev.action) {
        case "status":
          return statusDoc(ev.item);
        case "logs":
          return logsDoc(ev.item);
        case "restart":
          await backend.restart(ev.item);
          // Every restart leaves a line in the channel. The blob is private to
          // whoever opened it, so without this nobody else would ever know.
          p.say(ctx.channel, `restarted ${ev.item} (asked by ${ctx.actor})`);
          return listDoc();
        case "back":
        case "refresh":
          return listDoc();
        default:
          throw new Error(`unknown action ${ev.action}`);
      }
    },
  },
);

// The chat-only quick path: no blob needed to answer "is web-1 alright?".
p.command(
  {
    name: "status",
    usage: "/status <server>",
    desc: "status of one server, in chat",
    args: [{ name: "server", type: "text", required: true }],
    scope: "any",
  },
  async ({ server }) => {
    const s = await backend.status(server);
    return `${s.name}: ${s.state} · ${s.latency}ms · cpu ${s.cpu} · mem ${s.memory} · up ${s.uptime}`;
  },
);

await p.connect();
console.log(`devops watching: ${channels.join(", ")}`);
