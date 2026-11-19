// End-to-end smoke test against a FRESH server (the first account registered
// becomes admin; against a server that already has accounts, registration
// correctly demands an invite and this script will fail on step 1).
//
// End-to-end smoke test against a running server: register a user, connect the
// client socket, connect a plugin, verify the palette is pushed, run the
// plugin command, verify it lands in the channel — and the same for BLOBS,
// which are the other half of what a plugin can extend. Exits non-zero on
// failure.
//
//   node e2e.mjs http://localhost:8787 <plugin-token>
//
// The token has to be minted for an AGENT, not a bot:
//
//   gliph -data ./data -plugin "e2ebot:E2E Bot:agent"
//
// A plugin's kind is fixed when its token is minted and cannot be claimed in
// the register frame (server/plugin.go) — being streamed every subscribed
// channel's events is the admin's call, not the plugin's. So a bot token gets
// through registration and the command lane, then fails on "agent received the
// channel event" and everything after it, which reads like a broken server.

const base = process.argv[2] ?? "http://localhost:8787";
const pluginToken = process.argv[3];
const fails = [];
const ok = (name, cond, extra = "") =>
  cond ? console.log(`  ok   ${name}`) : (fails.push(name), console.log(`  FAIL ${name} ${extra}`));

const post = async (path, body, token) => {
  const res = await fetch(base + path, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...(token ? { Authorization: `Bearer ${token}` } : {}) },
    body: JSON.stringify(body),
  });
  return { status: res.status, body: await res.json().catch(() => ({})) };
};

/// Like waitFor, but a timeout is a `null` rather than an exception. One check
/// that fails should cost one line of output, not every check after it.
const tryFor = (frames, pred, ms = 4000) =>
  waitFor(frames, pred, ms).then((f) => f, () => null);

/// The notices this client has been sent, newest last — printed when something
/// that expected one did not get it, so a failure names what DID arrive.
const notices = (frames) => frames.filter((f) => f.t === "notice").map((f) => f.d.text);

const waitFor = (frames, pred, ms = 4000) =>
  new Promise((resolve, reject) => {
    const t = setInterval(() => {
      const hit = frames.find(pred);
      if (hit) { clearInterval(t); clearTimeout(k); resolve(hit); }
    }, 25);
    const k = setTimeout(() => { clearInterval(t); reject(new Error("timeout waiting for frame")); }, ms);
  });

const openSocket = (url) => {
  const ws = new WebSocket(url);
  const frames = [];
  ws.addEventListener("message", (m) => frames.push(JSON.parse(m.data)));
  return new Promise((resolve, reject) => {
    ws.addEventListener("open", () => resolve({ ws, frames, send: (t, d) => ws.send(JSON.stringify({ t, d })) }));
    ws.addEventListener("error", () => reject(new Error(`cannot connect: ${url}`)));
  });
};

const nick = "e2e" + Math.floor(Math.random() * 1e6);

console.log("\ngliph end-to-end\n");

// 1. health + register
const health = await fetch(base + "/healthz").then((r) => r.json());
ok("server healthy", health.ok === true);

const reg = await post("/api/register", { nick, email: `${nick}@test.local`, password: "hunter2hunter2" });
ok("register returns a token", !!reg.body.token, JSON.stringify(reg.body));
if (!reg.body.token) {
  console.log("\n  cannot continue without a session — point this at a fresh server\n");
  process.exit(1);
}
const token = reg.body.token;

const bad = await post("/api/login", { nick, password: "wrong-password" });
ok("wrong password is refused", bad.status === 401);

// 2. client socket: hello carries the palette
const client = await openSocket(`${base.replace(/^http/, "ws")}/ws?token=${token}`);
const hello = await waitFor(client.frames, (f) => f.t === "hello");
ok("hello carries a command manifest", Array.isArray(hello.d.commands) && hello.d.commands.length > 5);
ok("manifest entries are fully specified",
  hello.d.commands.every((c) => c.name && c.usage && c.scope && c.min_role && c.source));
const builtinCount = hello.d.commands.length;

const state = await waitFor(client.frames, (f) => f.t === "state");
const general = state.d.channels.find((c) => c.name === "general");
ok("new user landed in #general", !!general);

// 3. a message round-trips through the event log
client.send("message.send", { channel: general.id, body: "hello from e2e", nonce: crypto.randomUUID() });
const msg = await waitFor(client.frames, (f) => f.t === "event" && f.d.body === "hello from e2e");
ok("message appears as an event with a seq", msg.d.seq > 0 && msg.d.kind === "message");

// 4. builtin command works, and unknown ones are refused
client.send("command.run", { channel: general.id, name: "me", args: "waves at the test suite" });
await waitFor(client.frames, (f) => f.t === "event" && f.d.kind === "action");
ok("/me posts an action event", true);

client.send("command.run", { channel: general.id, name: "nope", args: "" });
const unknown = await waitFor(client.frames, (f) => f.t === "notice" && /unknown command/.test(f.d.text));
ok("unknown command is refused", !!unknown);

// 5. the headline: a plugin extends the palette at runtime
if (!pluginToken) {
  console.log("\n  (skipping plugin checks — no plugin token given)\n");
} else {
  const plug = await openSocket(`${base.replace(/^http/, "ws")}/plugin?token=${pluginToken}`);
  plug.send("register", {
    display: "e2ebot", kind: "agent", subscribe: ["general"],
    commands: [{
      name: "e2eping", usage: "/e2eping <word>", desc: "echo a word back",
      scope: "channel", min_role: "member",
      args: [{ name: "word", type: "text", required: true }],
    }],
    blobs: [{ id: "e2eblob", title: "e2e app", mode: "private" }],
  });
  await waitFor(plug.frames, (f) => f.t === "ready");
  ok("plugin registered", true);

  // every connected client is pushed the new palette, unprompted
  const pushed = await waitFor(client.frames, (f) => f.t === "commands");
  const names = pushed.d.commands.map((c) => c.name);
  ok("client was pushed a new palette", names.includes("e2eping"));
  ok("palette grew by exactly the plugin's commands", pushed.d.commands.length === builtinCount + 1);
  ok("plugin command is attributed to its plugin",
    pushed.d.commands.find((c) => c.name === "e2eping")?.source !== "builtin");

  // invoking routes to the plugin
  client.send("command.run", { channel: general.id, name: "e2eping", args: "pong" });
  const invoke = await waitFor(plug.frames, (f) => f.t === "invoke");
  ok("plugin received the invoke", invoke.d.name === "e2eping");
  ok("named args were parsed from the manifest", invoke.d.arg_map?.word === "pong");

  plug.send("result", { call: invoke.d.call, say: `pong: ${invoke.d.arg_map.word}` });
  const agentEv = await waitFor(client.frames, (f) => f.t === "event" && f.d.kind === "agent");
  ok("plugin result posted into the channel", agentEv.d.body === "pong: pong");
  ok("agent event is attributed", agentEv.d.actor === "e2ebot");

  // agents see the stream
  client.send("message.send", { channel: general.id, body: "does the agent see this", nonce: crypto.randomUUID() });
  const seen = await waitFor(plug.frames, (f) => f.t === "event" && f.d.body === "does the agent see this");
  ok("agent received the channel event", !!seen);

  // ---- blobs: the other half of what a plugin extends ----
  //
  // The whole lane, end to end: a blob declared in `register` reaches the
  // switcher, `blob.open` becomes `blob_open` on the plugin socket, a `render`
  // comes back as `blob.render`, an event can be answered with a `menu`, and
  // the server refuses an action the document never offered.

  const withBlob = await waitFor(client.frames, (f) => f.t === "state" &&
    f.d.channels.some((c) => c.blobs?.some((b) => b.id === "e2eblob")), 5000);
  ok("plugin blob appears in the channel's switcher", !!withBlob);
  const spec = withBlob.d.channels
    .find((c) => c.id === general.id).blobs.find((b) => b.id === "e2eblob");
  ok("blob is attributed to its plugin", spec.source !== "builtin" && spec.mode === "private");

  const doc = (body) => ({
    title: "e2e app",
    blocks: [
      { t: "text", body, style: "info" },
      { t: "list", id: "rows", items: [
          { id: "r1", label: "row one", state: "ok",
            actions: [{ id: "poke", label: "poke" }, { id: "more", label: "more…" }] }] },
      { t: "buttons", actions: [{ id: "refresh", label: "refresh" }] },
    ],
  });

  client.send("blob.open", { channel: general.id, blob: "e2eblob" });
  const bOpen = await waitFor(plug.frames, (f) => f.t === "blob_open");
  ok("blob.open reached the plugin as blob_open", bOpen.d.blob === "e2eblob");
  ok("blob_open names the caller and their role", bOpen.d.actor === nick && !!bOpen.d.role);
  ok("a private blob is addressed by session", typeof bOpen.d.inst === "string" && bOpen.d.inst.length > 0);

  plug.send("render", { call: bOpen.d.call, doc: doc("opened") });
  const rendered = await waitFor(client.frames, (f) => f.t === "blob.render");
  ok("the plugin's document reached the client", rendered.d.doc?.title === "e2e app");
  ok("the document is the one the plugin sent", rendered.d.doc.blocks[0].body === "opened");

  // An event answered with a full document re-renders.
  client.send("blob.event", { channel: general.id, blob: "e2eblob", action: "refresh" });
  const bEv = await waitFor(plug.frames, (f) => f.t === "blob_event" && f.d.action === "refresh");
  ok("blob.event reached the plugin", !!bEv);
  plug.send("render", { call: bEv.d.call, doc: doc("refreshed") });
  const rerendered = await waitFor(client.frames,
    (f) => f.t === "blob.render" && f.d.doc?.blocks[0].body === "refreshed");
  ok("a render answer re-renders the surface", !!rerendered);

  // ...and an event answered with a menu becomes a submenu, not a document.
  client.send("blob.event", { channel: general.id, blob: "e2eblob", action: "more", item: "r1" });
  const bMenu = await waitFor(plug.frames, (f) => f.t === "blob_event" && f.d.action === "more");
  ok("blob_event carries the item it was fired on", bMenu.d.item === "r1");
  plug.send("menu", { call: bMenu.d.call, item: "r1",
    actions: [{ id: "one", label: "one" }, { id: "two", label: "two" }] });
  const menuFrame = await waitFor(client.frames, (f) => f.t === "blob.menu");
  ok("a menu answer reaches the client as blob.menu", menuFrame.d.actions?.length === 2);
  ok("the menu names the item it belongs to", menuFrame.d.item === "r1");

  // The server, not the client, is the gate: an action the last document never
  // offered is refused even though the plugin would happily have handled it.
  client.send("blob.event", { channel: general.id, blob: "e2eblob", action: "rm-rf" });
  const refused = await waitFor(client.frames,
    (f) => f.t === "notice" && /not available/.test(f.d.text ?? ""), 4000);
  ok("an action the document never offered is refused", !!refused);

  // dropping the plugin retracts its commands AND its blobs
  plug.ws.close();
  const retracted = await waitFor(client.frames, (f) => f.t === "commands" &&
    !f.d.commands.some((c) => c.name === "e2eping"), 5000);
  ok("palette shrank when the plugin disconnected", !!retracted);
  const noBlob = await waitFor(client.frames, (f) => f.t === "state" &&
    !f.d.channels.some((c) => c.blobs?.some((b) => b.id === "e2eblob")), 5000);
  ok("switcher shrank when the plugin disconnected", !!noBlob);
}

// ---- builtin blobs and the commands that pair with them ----
//
// These run one command after another, and the server rate-limits a socket on
// purpose (server/ratelimit.go). Pacing them is not a workaround: a test that
// trips the limiter is testing the limiter, and the dropped frame shows up as
// whichever unrelated check came next.

const breathe = (ms = 350) => new Promise((r) => setTimeout(r, ms));

const noticed = (re, ms = 4000) =>
  tryFor(client.frames, (f) => f.t === "notice" && re.test(f.d.text ?? ""), ms);

await breathe();
client.send("command.run", { channel: general.id, name: "blobs", args: "" });
const blobList = await noticed(/apps here:/);
ok("/blobs lists this channel's apps",
  /todos/.test(blobList?.d.text ?? "") && /pins/.test(blobList?.d.text ?? ""),
  JSON.stringify(notices(client.frames)));

// /todos asks THIS client to switch surface — the one frame that moves the UI.
await breathe();
client.send("command.run", { channel: general.id, name: "todos", args: "" });
const focus = await tryFor(client.frames, (f) => f.t === "blob.focus");
ok("/todos focuses the blob it names",
  focus?.d.blob === "todos" && focus?.d.channel === general.id);

// `/todo done <n>` used to add a todo called "done 1".
await breathe();
client.send("command.run", { channel: general.id, name: "todo", args: "write the e2e check" });
await tryFor(client.frames, (f) => f.t === "event" && /added a todo: write the e2e check/.test(f.d.body ?? ""));
await breathe();
client.send("command.run", { channel: general.id, name: "todo", args: "done 1" });
const completed = await tryFor(client.frames,
  (f) => f.t === "event" && /completed: write the e2e check/.test(f.d.body ?? ""));
ok("/todo done <n> ticks the nth open todo off", !!completed, JSON.stringify(notices(client.frames)));

await breathe();
client.send("command.run", { channel: general.id, name: "todo", args: "done 99" });
const outOfRange = await noticed(/only \d+ open todo/);
ok("/todo done <n> past the end says so", !!outOfRange, JSON.stringify(notices(client.frames)));

// bare /checkin reads the board instead of complaining about usage
await breathe();
client.send("command.run", { channel: general.id, name: "checkin", args: "" });
const emptyBoard = await noticed(/nobody has checked in today/);
ok("bare /checkin reads the board", !!emptyBoard, JSON.stringify(notices(client.frames)));
await breathe();
client.send("command.run", { channel: general.id, name: "standup", args: "closing the gaps" });
const stood = await tryFor(client.frames, (f) => f.t === "event" && /checked in: closing the gaps/.test(f.d.body ?? ""));
ok("/standup is /checkin by its other name", !!stood);
await breathe();
client.send("command.run", { channel: general.id, name: "checkin", args: "" });
const board = await noticed(new RegExp(`${nick}: closing the gaps`));
ok("the board shows who has checked in", !!board, JSON.stringify(notices(client.frames)));

// bare /pin pins the last thing said, rather than a note called nothing
await breathe();
client.send("message.send", { channel: general.id, body: "pin this one", nonce: crypto.randomUUID() });
await waitFor(client.frames, (f) => f.t === "event" && f.d.body === "pin this one");
await breathe();
client.send("command.run", { channel: general.id, name: "pin", args: "" });
const pinned = await tryFor(client.frames,
  (f) => f.t === "event" && /pinned: <.*> pin this one/.test(f.d.body ?? ""));
ok("bare /pin pins the last message", !!pinned, JSON.stringify(notices(client.frames)));

// ---- room0 in the stream ----

// a line said in the room is a channel message wearing the room's name
await breathe();
client.send("room.join", { channel: general.id, room: "office" });
await breathe();
client.send("message.send", {
  channel: general.id, body: "east wall now", nonce: crypto.randomUUID(), room: "office",
});
const said = await tryFor(client.frames,
  (f) => f.t === "event" && f.d.body === "east wall now" && f.d.meta?.room === "office");
ok("a room line is a channel message with the room on it", !!said);

// a room nobody can stand in is refused rather than stamped
await breathe();
client.send("message.send", {
  channel: general.id, body: "nowhere", nonce: crypto.randomUUID(), room: "atlantis",
});
ok("an invented room is refused", !!(await noticed(/no such room/)), JSON.stringify(notices(client.frames)));

// the invite is one event, naming the room the inviter is standing in
await breathe();
client.send("command.run", { channel: general.id, name: "room0", args: "invite" });
const invited = await tryFor(client.frames,
  (f) => f.t === "event" && f.d.kind === "room_invite" && f.d.body === "office");
ok("/room0 invite posts one invite naming the room", !!invited);

// quiet is the client's switch: the server asks the asking connection to flip
await breathe();
client.send("command.run", { channel: general.id, name: "room0", args: "quiet" });
ok("/room0 quiet answers the caller", !!(await tryFor(client.frames, (f) => f.t === "room.quiet")));

// who is inside rides along with the channel, for a client that just dialled
await breathe();
client.send("sync", { cursors: {} });
const occupied = await tryFor(client.frames, (f) => f.t === "state" &&
  (f.d.channels ?? []).some((c) => c.id === general.id && (c.room?.users ?? []).length > 0));
ok("the channel state says who is in the room", !!occupied);

client.ws.close();
console.log(fails.length ? `\n${fails.length} FAILED: ${fails.join(", ")}\n` : "\nall checks passed\n");
process.exit(fails.length ? 1 : 0);
