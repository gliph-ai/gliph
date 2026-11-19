// An MCP client for gliph agents (spec F11: "agent runtime with MCP client").
//
// Zero dependencies. MCP is JSON-RPC 2.0 with about four methods that matter,
// which is less protocol than a client library would be.
//
// TWO RULES THAT ARE NOT ABOUT THE PROTOCOL, AND ARE THE POINT OF THIS FILE:
//
//  1. PERMISSION IS DECIDED HERE, NOT BY THE TOOL SERVER. An MCP server
//     describes its own tools, including whether they are read-only. That
//     description is a claim by the very thing being restricted, so a tier of
//     `read` refuses anything not explicitly annotated read-only — an
//     unannotated tool is treated as a write. Failing closed on a missing
//     annotation is the only safe reading of "I forgot to say".
//
//  2. WRITES ARE ANNOUNCED IN THE CHANNEL. gliph's whole position on agents
//     (F11) is that they are visible members, not ambient magic: they carry a
//     sigil, they hold their own keys, removing them cuts their access. An
//     agent that quietly reaches out and changes something in the world would
//     be the one part of that picture nobody can see. So a tool call that can
//     mutate posts a line saying so. Reads stay quiet — they are frequent,
//     harmless, and announcing them would train everyone to ignore the ones
//     that matter.

import { spawn } from "node:child_process";

const PROTOCOL_VERSION = "2025-06-18";

export const TIERS = ["read", "read_write", "full"];

/** Is this tool safe under a read-only tier?
 *
 *  `readOnlyHint` is the server's own claim about its own tool. We believe a
 *  "yes"; we do not accept silence as one. */
function isReadOnly(tool) {
  return tool?.annotations?.readOnlyHint === true;
}

/** Is this tool destructive enough that a `read_write` tier should still refuse
 *  it? MCP's own annotation for irreversible operations. */
function isDestructive(tool) {
  return tool?.annotations?.destructiveHint === true;
}

function allowed(tool, tier) {
  if (tier === "full") return true;
  if (tier === "read") return isReadOnly(tool);
  // read_write: anything except what the server itself flagged as destructive.
  return !isDestructive(tool);
}

// ---- transports -----------------------------------------------------------

/** stdio: the ordinary shape for a tool server running on the same box. */
function stdioTransport({ command, args = [], env = {}, cwd }) {
  const child = spawn(command, args, {
    stdio: ["pipe", "pipe", "pipe"],
    env: { ...process.env, ...env },
    cwd,
  });
  const pending = new Map();
  let buffer = "";

  child.stdout.on("data", (chunk) => {
    buffer += chunk.toString();
    // Line-delimited JSON. A partial line at the end stays in the buffer.
    let nl;
    while ((nl = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, nl).trim();
      buffer = buffer.slice(nl + 1);
      if (!line) continue;
      let msg;
      try { msg = JSON.parse(line); } catch { continue; }
      const waiter = pending.get(msg.id);
      if (waiter) { pending.delete(msg.id); waiter(msg); }
    }
  });
  // The server's stderr is its log, not our protocol. Keep it visible but out
  // of the way — a tool server that cannot start says so there.
  child.stderr.on("data", (d) => process.stderr.write(`[mcp:${command}] ${d}`));

  let nextID = 1;
  return {
    async call(method, params, timeoutMs) {
      const id = nextID++;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          pending.delete(id);
          reject(new Error(`${method} timed out after ${timeoutMs}ms`));
        }, timeoutMs);
        pending.set(id, (msg) => {
          clearTimeout(timer);
          if (msg.error) reject(new Error(msg.error.message ?? "mcp error"));
          else resolve(msg.result);
        });
        child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
      });
    },
    notify(method, params) {
      child.stdin.write(JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n");
    },
    close() { child.kill(); },
  };
}

/** Streamable HTTP: a tool server somewhere else. */
function httpTransport({ url, headers = {} }) {
  let sessionID = null;
  let nextID = 1;
  return {
    async call(method, params, timeoutMs) {
      const res = await fetch(url, {
        method: "POST",
        signal: AbortSignal.timeout(timeoutMs),
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json, text/event-stream",
          ...(sessionID ? { "Mcp-Session-Id": sessionID } : {}),
          ...headers,
        },
        body: JSON.stringify({ jsonrpc: "2.0", id: nextID++, method, params }),
      });
      const sid = res.headers.get("Mcp-Session-Id");
      if (sid) sessionID = sid;
      if (!res.ok) throw new Error(`mcp http ${res.status}`);
      const msg = await res.json();
      if (msg.error) throw new Error(msg.error.message ?? "mcp error");
      return msg.result;
    },
    notify() { /* notifications carry no reply; nothing here needs one */ },
    close() { /* stateless */ },
  };
}

// ---- the client -----------------------------------------------------------

/** Connect to one MCP server.
 *
 *    const tools = await mcp({
 *      name: "fs",
 *      command: "npx", args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv"],
 *      tier: "read",
 *      announce: (line) => p.say(channel, line),
 *    });
 *    await tools.call("read_file", { path: "/srv/notes.md" });
 */
export async function mcp(opts) {
  const {
    name = "mcp",
    tier = "read",
    timeoutMs = 30_000,
    announce = null,
  } = opts;

  if (!TIERS.includes(tier)) {
    throw new Error(`unknown tier "${tier}" — one of ${TIERS.join(", ")}`);
  }

  const transport = opts.url ? httpTransport(opts) : stdioTransport(opts);

  await transport.call("initialize", {
    protocolVersion: PROTOCOL_VERSION,
    capabilities: {},
    clientInfo: { name: "gliph-agent", version: "0.1.0" },
  }, timeoutMs);
  transport.notify("notifications/initialized", {});

  const listed = (await transport.call("tools/list", {}, timeoutMs))?.tools ?? [];
  // Filter at connect time so the agent's prompt only ever sees tools it is
  // actually permitted to call. Offering a tool and then refusing it wastes a
  // model turn and reads, to the agent, like a broken server.
  const tools = listed.filter((t) => allowed(t, tier));
  const byName = new Map(tools.map((t) => [t.name, t]));

  const skipped = listed.length - tools.length;
  if (skipped > 0) {
    console.log(`[mcp:${name}] ${tools.length} tools available, ${skipped} withheld by tier "${tier}"`);
  }

  return {
    name,
    tier,
    /** The tools this tier permits, in the shape an LLM tool-use API wants. */
    tools: tools.map((t) => ({
      name: `${name}__${t.name}`,
      description: t.description ?? "",
      input_schema: t.inputSchema ?? { type: "object", properties: {} },
    })),

    /** Call a tool. Accepts either the bare name or the `<server>__<tool>`
     *  form the model was given. */
    async call(toolName, args = {}) {
      const bare = toolName.startsWith(`${name}__`) ? toolName.slice(name.length + 2) : toolName;
      const tool = byName.get(bare);
      // Re-check rather than trusting that the filtered list was the only way
      // in: `call` is public, and a permission enforced in one code path is a
      // permission enforced nowhere.
      if (!tool) throw new Error(`no tool "${bare}" on ${name} (or not permitted at tier "${tier}")`);
      if (!allowed(tool, tier)) throw new Error(`"${bare}" is not permitted at tier "${tier}"`);

      // Announce BEFORE the call, not after: if the tool hangs or the process
      // dies mid-write, the channel should still show that something was
      // attempted. An audit line that only appears on success is not an audit.
      if (announce && !isReadOnly(tool)) {
        announce(`⚙ calling \`${name}.${bare}\`${summarize(args)}`);
      }
      const out = await transport.call("tools/call", { name: bare, arguments: args }, timeoutMs);
      return flatten(out);
    },

    close() { transport.close(); },
  };
}

/** A one-line hint at what the call was for. Deliberately short and truncated:
 *  the arguments can be large or sensitive, and this is a notice in a chat
 *  channel, not a log. */
function summarize(args) {
  const keys = Object.keys(args ?? {});
  if (!keys.length) return "";
  const first = keys[0];
  const v = String(args[first] ?? "");
  return ` (${first}: ${v.length > 40 ? v.slice(0, 40) + "…" : v}${keys.length > 1 ? ", …" : ""})`;
}

/** MCP returns content blocks. Agents want text. */
function flatten(result) {
  if (result?.isError) {
    const msg = (result.content ?? []).map((c) => c.text ?? "").join("\n");
    throw new Error(msg || "tool call failed");
  }
  return (result?.content ?? [])
    .map((c) => (c.type === "text" ? c.text : `[${c.type}]`))
    .join("\n");
}

export { allowed as _allowed, isReadOnly as _isReadOnly };
