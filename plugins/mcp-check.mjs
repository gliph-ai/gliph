// Assertions over the MCP client's permission model — the part of it that is
// not JSON-RPC plumbing and would be dangerous to get wrong.
//
//   node plugins/mcp-check.mjs
//
// Spins up a REAL MCP server over stdio (a few lines below, in its own process)
// so the transport is exercised rather than mocked.

import assert from "node:assert/strict";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { mcp } from "./mcp.mjs";
import * as prompts from "./prompts.mjs";

// A minimal MCP server: one read-only tool, one write tool, one destructive
// tool, and one that forgets to say which it is.
const SERVER = `
let buf = "";
const send = (m) => process.stdout.write(JSON.stringify(m) + "\\n");
const TOOLS = [
  { name: "peek",   description: "read a thing",   annotations: { readOnlyHint: true } },
  { name: "poke",   description: "change a thing", annotations: { readOnlyHint: false } },
  { name: "nuke",   description: "end a thing",    annotations: { destructiveHint: true } },
  { name: "vague",  description: "who knows" },
];
process.stdin.on("data", (c) => {
  buf += c;
  let nl;
  while ((nl = buf.indexOf("\\n")) !== -1) {
    const line = buf.slice(0, nl); buf = buf.slice(nl + 1);
    if (!line.trim()) continue;
    const msg = JSON.parse(line);
    if (msg.method === "initialize") send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-06-18", capabilities: {}, serverInfo: { name: "t", version: "0" } } });
    else if (msg.method === "tools/list") send({ jsonrpc: "2.0", id: msg.id, result: { tools: TOOLS } });
    else if (msg.method === "tools/call") send({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: "did " + msg.params.name }] } });
    else if (msg.id) send({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: "no" } });
  }
});
`;

const dir = mkdtempSync(path.join(tmpdir(), "gliph-mcp-"));
const server = path.join(dir, "server.mjs");
writeFileSync(server, SERVER);

const spawnArgs = { command: process.execPath, args: [server], timeoutMs: 5000 };
let checks = 0;
const check = (name, fn) => fn().then(() => { checks += 1; console.log(`  ok  ${name}`); });

await check("read tier exposes only what the server SAID is read-only", async () => {
  const c = await mcp({ ...spawnArgs, name: "t", tier: "read" });
  const names = c.tools.map((t) => t.name).sort();
  assert.deepEqual(names, ["t__peek"],
    `an unannotated tool must not pass as read-only: ${names}`);
  c.close();
});

await check("a withheld tool is still refused if called directly", async () => {
  const c = await mcp({ ...spawnArgs, name: "t", tier: "read" });
  await assert.rejects(() => c.call("poke", {}), /not permitted|no tool/);
  await assert.rejects(() => c.call("vague", {}), /not permitted|no tool/);
  c.close();
});

await check("read_write allows writes but still refuses destructive", async () => {
  const c = await mcp({ ...spawnArgs, name: "t", tier: "read_write" });
  const names = c.tools.map((t) => t.name).sort();
  assert.ok(names.includes("t__poke") && names.includes("t__vague"), names.join());
  assert.ok(!names.includes("t__nuke"), "destructiveHint must survive read_write");
  await assert.rejects(() => c.call("nuke", {}), /not permitted|no tool/);
  c.close();
});

await check("full allows everything", async () => {
  const c = await mcp({ ...spawnArgs, name: "t", tier: "full" });
  assert.equal(c.tools.length, 4);
  assert.equal(await c.call("nuke", {}), "did nuke");
  c.close();
});

await check("a write is announced, a read is not", async () => {
  const said = [];
  const c = await mcp({ ...spawnArgs, name: "t", tier: "full", announce: (l) => said.push(l) });
  await c.call("peek", { path: "/x" });
  assert.deepEqual(said, [], "reads must stay quiet or nobody reads the ones that matter");
  await c.call("poke", { path: "/x" });
  assert.equal(said.length, 1, "a mutating call must be visible in the channel");
  assert.match(said[0], /t\.poke/);
  c.close();
});

await check("the prefixed name the model was given also works", async () => {
  const c = await mcp({ ...spawnArgs, name: "t", tier: "full" });
  assert.equal(await c.call("t__peek", {}), "did peek");
  c.close();
});

await check("an unknown tier is refused rather than defaulted", async () => {
  await assert.rejects(() => mcp({ ...spawnArgs, tier: "admin" }), /unknown tier/);
});

// ---- prompt overrides: a bad one must change NOTHING ----

const sync = (name, fn) => { fn(); checks += 1; console.log(`  ok  ${name}`); };

sync("a good override applies", () => {
  prompts.reset();
  assert.equal(prompts.setOverrides({ summarise: "in one line." }), null);
  assert.equal(prompts.current().summarise, "in one line.");
  // Untouched sections keep their defaults rather than vanishing.
  assert.equal(prompts.current().ask, prompts.DEFAULTS.ask);
});

sync("a typo'd section name changes nothing at all", () => {
  prompts.reset();
  prompts.setOverrides({ summarise: "custom" });
  const before = prompts.current();
  const err = prompts.setOverrides({ sumarise: "typo", system: "You are {name}." });
  assert.match(err, /unknown section/);
  assert.deepEqual(prompts.current(), before,
    "a partial apply leaves an agent half-configured, which debugs as the model behaving oddly");
});

sync("a template that loses a required placeholder is refused", () => {
  prompts.reset();
  const err = prompts.setOverrides({ ask: "just answer: {question}" });
  assert.match(err, /\{context\}/);
  assert.equal(prompts.current().ask, prompts.DEFAULTS.ask);
});

sync("substitution leaves an unknown placeholder visible", () => {
  prompts.reset();
  const out = prompts.prompt("system", { name: "scribe" });
  assert.ok(out.includes("scribe"));
  prompts.setOverrides({ summarise: "sum up {nothing}" });
  assert.equal(prompts.prompt("summarise"), "sum up {nothing}",
    "blanking it is how a typo goes unnoticed");
  prompts.reset();
});

console.log(`\n${checks} checks passed`);
