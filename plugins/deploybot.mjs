// A plain bot: registers two commands, watches the channel, posts on demand.
//
//   node deploybot.mjs
//
// Env: GLIPH_URL (default http://localhost:8787), GLIPH_TOKEN (required),
//      GLIPH_CHANNELS (comma separated names, default "general")

import { plugin } from "./sdk.mjs";

const url = process.env.GLIPH_URL ?? "http://localhost:8787";
const token = process.env.GLIPH_TOKEN;
const channels = (process.env.GLIPH_CHANNELS ?? "general").split(",");

if (!token) {
  console.error("set GLIPH_TOKEN — create one with: gliph -plugin deploybot:deploybot:agent");
  process.exit(1);
}

const builds = new Map();

const p = plugin({ url, token, kind: "agent", display: "deploybot", subscribe: channels });

p.command(
  {
    name: "deploy",
    usage: "/deploy <env>",
    desc: "ship the current build to an environment",
    args: [{ name: "env", type: "text", required: true }],
    min_role: "op", // the server enforces this, not the client
  },
  async ({ env, actor }) => {
    if (!["staging", "prod"].includes(env)) {
      throw new Error(`unknown environment "${env}" — try staging or prod`);
    }
    const n = (builds.get(env) ?? 411) + 1;
    builds.set(env, n);
    return `build #${n} shipping to ${env}, requested by ${actor}`;
  },
);

p.command(
  {
    name: "builds",
    usage: "/builds",
    desc: "show the last build per environment",
    min_role: "member",
  },
  async () => {
    if (!builds.size) return "no builds yet this session";
    return [...builds].map(([env, n]) => `${env}: #${n}`).join(" · ");
  },
);

// Agents see the stream, so they can react without being addressed.
p.on("message", (e, api) => {
  if (/\bship it\b/i.test(e.body)) {
    api.say(e.channel, `heard "ship it" from ${e.actor} — run /deploy staging when ready`);
  }
});

await p.connect();
console.log(`deploybot watching: ${channels.join(", ")}`);
