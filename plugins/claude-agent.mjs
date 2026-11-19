// An AI agent in a channel. It registers /ask and /summarise, and answers when
// someone says its name. It reads the channel stream, so it has context, and it
// can be given MCP tool servers to call (spec F11).
//
//   node claude-agent.mjs
//
// Env: GLIPH_URL, GLIPH_TOKEN (required), GLIPH_CHANNELS, ANTHROPIC_API_KEY,
//      GLIPH_PROMPTS (a JSON file overriding any prompt section),
//      GLIPH_MCP (a JSON file of MCP servers to connect — see below).

import { readFileSync } from "node:fs";
import Anthropic from "@anthropic-ai/sdk";
import { plugin } from "./sdk.mjs";
import { loadFromEnv, prompt } from "./prompts.mjs";

const url = process.env.GLIPH_URL ?? "http://localhost:8787";
const token = process.env.GLIPH_TOKEN;
const channels = (process.env.GLIPH_CHANNELS ?? "general").split(",");
const NAME = process.env.GLIPH_AGENT_NAME ?? "scribe";

if (!token) {
  console.error("set GLIPH_TOKEN — create one with: gliph -plugin scribe:scribe:agent");
  process.exit(1);
}

// Resolves ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or an `ant auth login` profile.
const claude = new Anthropic();

loadFromEnv();
const SYSTEM = prompt("system", { name: NAME });

/** Rolling per-channel context. The agent only ever sees channels it is a
 *  member of, and the member list shows it, so nobody is being read silently. */
const recent = new Map();
const remember = (cid, line) => {
  const list = recent.get(cid) ?? [];
  list.push(line);
  recent.set(cid, list.slice(-40));
};
const contextFor = (cid) => (recent.get(cid) ?? []).join("\n") || "(no messages yet)";

/** MCP servers, from a JSON file:
 *
 *    [{ "name": "fs", "tier": "read",
 *       "command": "npx",
 *       "args": ["-y", "@modelcontextprotocol/server-filesystem", "/srv"] }]
 *
 *  Tier defaults to `read` in the SDK, so an agent gets the least it can be
 *  given until somebody writes down otherwise. Tool calls that can mutate post
 *  a line in the channel — see plugins/mcp.mjs for why that is not optional. */
async function connectTools(p, announceIn) {
  const file = process.env.GLIPH_MCP;
  if (!file) return [];
  let specs;
  try {
    specs = JSON.parse(readFileSync(file, "utf8"));
  } catch (err) {
    console.error(`[mcp] ignoring ${file}: ${err.message}`);
    return [];
  }
  const out = [];
  for (const spec of specs) {
    try {
      out.push(await p.mcp({ ...spec, announceIn }));
    } catch (err) {
      // One unreachable tool server must not stop the agent joining the
      // channel. It says so and carries on with the tools it does have.
      console.error(`[mcp] ${spec.name ?? "?"} unavailable: ${err.message}`);
    }
  }
  return out;
}

let servers = [];
const toolIndex = () => new Map(servers.flatMap((s) => s.tools.map((t) => [t.name, s])));

async function ask(question, cid) {
  const tools = servers.flatMap((s) => s.tools);
  const index = toolIndex();
  const messages = [{
    role: "user",
    content: prompt("ask", { context: contextFor(cid), question }),
  }];

  // Up to a few tool rounds. A cap rather than a loop-until-done: an agent that
  // can call tools forever is an agent that can bill forever.
  for (let round = 0; round < 5; round += 1) {
    const res = await claude.messages.create({
      model: "claude-opus-5",
      max_tokens: 2000, // chat replies are deliberately short
      system: SYSTEM,
      thinking: { type: "adaptive" },
      ...(tools.length ? { tools } : {}),
      messages,
    });
    if (res.stop_reason === "refusal") return "I can't help with that one.";

    const calls = res.content.filter((b) => b.type === "tool_use");
    if (!calls.length) {
      return res.content
        .filter((b) => b.type === "text")
        .map((b) => b.text)
        .join("\n")
        .trim() || "(no answer)";
    }

    messages.push({ role: "assistant", content: res.content });
    const results = [];
    for (const call of calls) {
      const server = index.get(call.name);
      try {
        if (!server) throw new Error(`no such tool ${call.name}`);
        results.push({
          type: "tool_result", tool_use_id: call.id,
          content: await server.call(call.name, call.input ?? {}),
        });
      } catch (err) {
        // Hand the failure back to the model rather than aborting: a refused
        // tool is information it can act on, and "not permitted at tier read"
        // is exactly the kind of thing it should say out loud.
        results.push({
          type: "tool_result", tool_use_id: call.id,
          content: `error: ${err.message}`, is_error: true,
        });
      }
    }
    messages.push({ role: "user", content: results });
  }
  return "I got stuck going back and forth with my tools on that one.";
}

const p = plugin({ url, token, kind: "agent", display: NAME, subscribe: channels, agent: "claude" });

p.command(
  {
    name: "ask",
    usage: "/ask <question>",
    desc: `ask ${NAME}, with channel context`,
    args: [{ name: "question", type: "rest", required: true }],
    scope: "any",
  },
  async ({ question, channel, actor }) => {
    const answer = await ask(question, channel);
    return `${actor}: ${answer}`;
  },
);

p.command(
  {
    name: "summarise",
    usage: "/summarise",
    desc: "catch up on what this channel has been discussing",
    scope: "channel",
  },
  async ({ channel }) => ask(prompt("summarise"), channel),
);

// Watch the stream: remember everything, answer when addressed by name.
p.on("message", async (e, api) => {
  if (e.actor === NAME) return;
  remember(e.channel, `<${e.actor}> ${e.body}`);

  const addressed = new RegExp(`(^|\\W)${NAME}\\b`, "i").test(e.body);
  if (!addressed) return;
  try {
    api.say(e.channel, await ask(e.body, e.channel));
  } catch (err) {
    console.error("[claude]", err.message);
    api.say(e.channel, `sorry, I could not reach the model just now (${err.message})`);
  }
});

await p.connect();
servers = await connectTools(p, channels[0]);
const toolCount = servers.reduce((n, s) => n + s.tools.length, 0);
console.log(`${NAME} watching: ${channels.join(", ")}${toolCount ? ` · ${toolCount} MCP tools` : ""}`);
