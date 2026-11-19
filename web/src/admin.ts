/* gliph admin console (spec F14).
 *
 * A separate page from the client on purpose: it shares nothing but the token
 * and the theme, so an operator's tab cannot accidentally act as their chat
 * session and none of the E2EE machinery is loaded here. Every list it renders
 * is metadata — no message ever reaches this page.
 *
 * It lives at /admin, and each section is a real URL (/admin/users) an operator
 * can bookmark, reload and send to the person on the other end of the incident.
 * The Go server maps /admin* onto this page; vite.config.ts does the same in
 * dev. No router library: history.pushState plus one popstate listener is the
 * whole mechanism, and this page has exactly seven destinations. */

import "./theme.css";
import "./admin.css";
import { esc } from "./escape.ts";
import { icons } from "./icons.ts";
import { clearSession, session } from "./session.ts";

const LS = { theme: "gliph.theme" };

/** A path segment, encoded. Every id here is server-generated hex today, so
 *  nothing breaks either way — but this switch had two spellings sitting next
 *  to each other (encodeURIComponent on two branches, raw on seven), and that
 *  is the kind of thing where the wrong one gets copied. */
const enc = (s: string) => encodeURIComponent(s);

const server = () =>
  session().server || (location.protocol.startsWith("http") ? location.origin : "");

type User = {
  id: string; nick: string; email: string; role: string;
  suspended: boolean; created: string; devices: number; channels: number; online: boolean;
};
type Channel = { id: string; name: string; modes: string; is_dm: boolean; created: string; members: number };
type Upload = {
  id: string; owner: string; name: string; size: number; received: number;
  expires: string; complete: boolean; channel: string;
};
type Invite = { token: string; role: string; created: string; used_by: string };
type Plugin = { id: string; display: string; kind: string; created: string; channels: number; online: boolean; commands: number };
type Stats = Record<string, number | string>;
type Audit = { actor: string; action: string; target: string; detail?: string; ts: string };
type Series = { days: string[]; signups: number[]; messages: number[]; files: number[]; actions: number[] };

const TABS = ["dashboard", "users", "invites", "channels", "plugins", "uploads", "audit", "settings"] as const;
type Tab = (typeof TABS)[number];

/** The one thing each tab lets you create, if it lets you create anything. It
 *  sits on the title line rather than in a strip above the table: the heading
 *  row is where a page says what it is and what you can do to it, and four
 *  tabs each carrying their own row of it was four rows of chrome for one
 *  button. Uploads and audit are records of things that happened — there is
 *  nothing to create there, and no button. */
const CREATE: Partial<Record<Tab, [act: string, label: string]>> = {
  users: ["newuser", "create account"],
  invites: ["newinvite", "create invite"],
  channels: ["newchannel", "create channel"],
  plugins: ["newplugin", "register plugin"],
};

const app = document.getElementById("app")!;
let token = session().token;
let me: { nick: string; role: string } | null = null;
let tab: Tab = "dashboard";
let q = "";        // the filter box, shared by every list
let range = 30;    // chart window, in days
let users: User[] = [];
let invites: Invite[] = [];
let plugins: Plugin[] = [];
let channels: Channel[] = [];
let uploads: Upload[] = [];
let stats: Stats = {};
let series: Series | null = null;
let auditLog: Audit[] = [];
let settings = { motd: "", icon: "" };
let err = "";
let ok = "";

document.documentElement.dataset.theme = localStorage.getItem(LS.theme) ?? "creme";

// ---- routing ----

/** /admin/users → "users". Anything unrecognised (including a bare /admin, or
 *  the legacy /admin.html bookmark) lands on the dashboard. */
function tabFromPath(): Tab {
  const seg = location.pathname.replace(/^\/admin\/?/, "").replace(/\/.*$/, "");
  return (TABS as readonly string[]).includes(seg) ? (seg as Tab) : "dashboard";
}

const pathFor = (t: Tab) => "/admin" + (t === "dashboard" ? "" : "/" + t);

async function go(t: Tab, push = true): Promise<void> {
  tab = t;
  err = ""; ok = "";
  q = "";
  if (push && location.pathname !== pathFor(t)) history.pushState(null, "", pathFor(t));
  await load();
}

addEventListener("popstate", () => void go(tabFromPath(), false));

// ---- api ----

/** Every failure surfaces the server's own message. A 500 with a SQL error in
 *  it is more use to an operator than "something went wrong". */
async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(server() + path, {
    ...init,
    headers: { ...(init?.body ? { "Content-Type": "application/json" } : {}), Authorization: "Bearer " + token },
  });
  const text = await res.text();
  let body: unknown = null;
  try { body = text ? JSON.parse(text) : null; } catch { /* not JSON: use the raw text below */ }
  if (!res.ok) {
    const msg = (body as { error?: string } | null)?.error ?? text ?? "";
    throw new Error(msg || `${res.status} ${res.statusText}`);
  }
  return body as T;
}


/** Clipboard, with the http:// fallback — a self-host on a bare LAN address has
 *  no secure context, so navigator.clipboard is simply absent there. */
async function copy(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
}

// ---- readable passwords ----
//
// An operator-set password exists to be read down a phone line once and then
// changed, so it has to survive being spoken. 128 short, unambiguous words —
// no homophones, no letter that could be a digit. Four of them plus two digits
// is ~34 bits, which is plenty for a credential with that lifespan, and the
// list length being a power of two keeps the modulo below unbiased.

const WORDS = (
  "amber anchor apple arrow aspen atlas autumn bamboo banjo basil beacon bison blossom bramble breeze bridge " +
  "bronze butter cactus canyon cedar cello cherry cinder cobalt comet copper coral cotton crimson crystal daisy " +
  "dapple delta denim domino dragon driftwood dusk ember fable falcon fennel fern fiddle flint forest fox " +
  "garnet ginger glacier granite harbor harvest hazel heron hickory hollow indigo ivory jasmine juniper kettle lantern " +
  "lark lemon lichen lilac linen lotus lumber magnet maple marble meadow mesa mint mosaic moss nectar " +
  "nickel nutmeg oak ocean olive onyx orchard otter oxide paprika pebble pepper pewter pine plaza plum " +
  "pollen poplar prairie quartz quill radish raven ribbon river rosemary rustic saffron sage salmon sandal sapphire " +
  "satin sequoia shadow silver slate spruce sugar summit sundial thicket thistle timber topaz tulip velvet walnut"
).split(" ");

const rand = (n: number) => crypto.getRandomValues(new Uint32Array(1))[0]! % n;

const passphrase = () =>
  Array.from({ length: 4 }, () => WORDS[rand(WORDS.length)]).join("-") +
  "-" + String(rand(100)).padStart(2, "0");

// ---- create dialogs ----

type Field<K extends string = string> = {
  name: K; label: string;
  type?: "text" | "password" | "select";
  placeholder?: string; options?: string[]; required?: boolean; hint?: string;
};

function field(f: Field): string {
  const id = "f-" + f.name;
  const attrs = `id="${id}" spellcheck="false" autocapitalize="off" ${f.required ? "required" : ""}`;
  const body =
    f.type === "select"
      ? `<select ${attrs}>${f.options!.map((o) => `<option>${esc(o)}</option>`).join("")}</select>`
      // Shown in the clear on purpose: the operator has to read it out, and
      // hiding a password they are about to dictate helps nobody.
      : f.type === "password"
        ? `<div class="gen"><input ${attrs} value="${esc(passphrase())}">
           <button type="button" class="btn" data-gen="${id}">new</button></div>`
        : `<input ${attrs} placeholder="${esc(f.placeholder ?? "")}">`;
  return `<label for="${id}">${esc(f.label)}</label>${body}` +
    (f.hint ? `<div class="hint">${esc(f.hint)}</div>` : "");
}

/** A create form as a real <dialog>: modal, Esc-closable, focus-trapped and
 *  validated by the platform, so none of that is written here. It hangs off
 *  <body> rather than #app, because a paint() mid-flight would otherwise pull
 *  it out from under the operator. Resolves to the trimmed field values, or
 *  null if they backed out. */
function modal<K extends string>(
  o: { title: string; submit: string; note?: string; fields: Field<K>[]; danger?: boolean },
): Promise<Record<K, string> | null> {
  const d = document.createElement("dialog");
  d.className = o.fields.length ? "modal" : "modal ask";
  // The primary button comes first so Enter in any input activates it —
  // .row reverses them visually, which is where cancel belongs.
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- dialog template, every caller value esc()'d in field() and here
  d.innerHTML = `<form method="dialog">
    <h3>${esc(o.title)}</h3>
    ${o.fields.map(field).join("")}
    ${o.note ? `<p class="note">${esc(o.note)}</p>` : ""}
    <div class="row">
      <button class="pri${o.danger ? " danger" : ""}" value="ok">${esc(o.submit)}</button>
      <button class="btn" value="" formnovalidate>cancel</button>
    </div>
  </form>`;
  d.addEventListener("click", (e) => {
    const g = (e.target as HTMLElement).closest<HTMLElement>("[data-gen]");
    if (!g) return;
    const input = d.querySelector<HTMLInputElement>("#" + g.dataset.gen)!;
    input.value = passphrase();
    input.select();
  });
  document.body.append(d);
  d.showModal();
  return new Promise((resolve) => {
    d.addEventListener("close", () => {
      const vals = Object.fromEntries(
        o.fields.map((f) => [f.name, d.querySelector<HTMLInputElement>("#f-" + f.name)!.value.trim()]),
      ) as Record<K, string>;
      d.remove();
      resolve(d.returnValue === "ok" ? vals : null);
    });
  });
}

/** A yes/no as the same <dialog>: the question is the title, the consequence
 *  the note, the verb the button. Resolves true only on the verb — Esc, the
 *  backdrop and cancel are all "no". Replaces window.confirm, which the
 *  desktop app renders as a bare OS alert. */
const ask = (title: string, note: string, verb: string, danger = true) =>
  modal({ title, submit: verb, note, fields: [], danger }).then((r) => r !== null);

// ---- formatting ----

function bytes(n: number): string {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${u[i]}`;
}

const num = (n: number) => n.toLocaleString("en-US");
const date = (s: string) => (s ? s.slice(0, 10) : "—");

function duration(sec: number): string {
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

/** The filter box. Every list tab runs its rows through this against whatever
 *  text identifies a row — no per-tab search syntax, no server round trip. */
const hits = (...parts: unknown[]) =>
  !q || parts.join(" ").toLowerCase().includes(q.toLowerCase());

// ---- loading ----

/** Fetches the current tab. It deliberately does NOT clear `err` first: act()
 *  sets an error and then reloads, and a load that clears it swallows every
 *  failed mutation — a rejected create looked to the operator like nothing had
 *  happened at all. Whoever opens a fresh view clears the notice; see go(). */
async function load(): Promise<void> {
  try {
    if (tab === "users") users = await api<User[]>("/api/admin/users");
    if (tab === "invites") invites = await api<Invite[]>("/api/admin/invites");
    if (tab === "plugins") plugins = await api<Plugin[]>("/api/admin/plugins");
    if (tab === "channels") channels = await api<Channel[]>("/api/admin/channels");
    if (tab === "uploads") uploads = await api<Upload[]>("/api/admin/uploads");
    if (tab === "audit") auditLog = await api<Audit[]>("/api/admin/audit");
    if (tab === "settings") settings = await api<typeof settings>("/api/admin/settings");
    if (tab === "dashboard") {
      [stats, series] = await Promise.all([
        api<Stats>("/api/admin/stats"),
        api<Series>(`/api/admin/series?days=${range}`),
      ]);
    }
  } catch (e) {
    err = String((e as Error).message);
  }
  paint();
}

/** Run a mutation, then reload the tab so the table reflects what the server
 *  actually did rather than what the console assumed. */
async function act(label: string, path: string, init: RequestInit): Promise<void> {
  try {
    err = ""; ok = "";
    await api(path, init);
    ok = label;
  } catch (e) {
    err = String((e as Error).message);
  }
  await load();
}

// ---- render ----

/** Signing in is the client's job, not this page's.
 *
 *  This console used to carry its own login form: a nick, a password and a
 *  submit. It was a worse copy of the real one — no server field, no invite,
 *  no nearby, no way in for anyone whose route to an account is not "type the
 *  two things I already know". Two forms against one /api/login, and only one
 *  of them got the new door each time a door was added.
 *
 *  So there is one form now, on the client, and this page hands it a return
 *  address. Both pages read the one session the core persists (see session.ts),
 *  which is what makes coming back free. */
function toLogin(): void {
  location.replace("/?next=" + encodeURIComponent(location.pathname));
}

// ---- charts ----
//
// A chart here is one polyline and three dotted rules. No fill, no gradient, no
// second series, no colour per line: the whole dashboard draws in one ink, the
// way a status page on a VT100 would have. What a reader needs off one of these
// is the shape and the peak, and both survive being this plain.
//
// preserveAspectRatio="none" lets the box stretch to whatever column width it
// gets; vector-effect keeps the stroke 1px while it does, which is the entire
// reason this is not a canvas.

function chart(title: string, days: string[], vals: number[]): string {
  const W = 300, H = 84;
  const peak = Math.max(1, ...vals);
  const total = vals.reduce((a, b) => a + b, 0);
  const x = (i: number) => (i / Math.max(1, vals.length - 1)) * W;
  const y = (v: number) => H - (v / peak) * (H - 4) - 2;
  const pts = vals.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
  const last = vals.at(-1) ?? 0;
  return `<figure class="chart">
    <figcaption><span class="k">${esc(title)}</span><span class="v">${num(total)}</span></figcaption>
    <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img"
         aria-label="${esc(title)}: ${num(total)} over ${vals.length} days, peak ${num(peak)} in a day">
      ${[0.25, 0.5, 0.75].map((f) => `<line class="grid" x1="0" y1="${(H * f).toFixed(1)}" x2="${W}" y2="${(H * f).toFixed(1)}"/>`).join("")}
      <polyline class="line" points="${pts}"/>
    </svg>
    <div class="axis">
      <span>${esc(days[0]?.slice(5) ?? "")}</span>
      <span class="mid">peak ${num(peak)}/day · today ${num(last)}</span>
      <span>${esc(days.at(-1)?.slice(5) ?? "")}</span>
    </div>
  </figure>`;
}

function renderDashboard(): string {
  const tile = (k: string, v: string) => `<div class="tile"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`;
  const n = (k: string) => Number(stats[k] ?? 0);
  const tiles = `<div class="tiles">
    ${tile("users", num(n("users")))}
    ${tile("channels", num(n("channels")))}
    ${tile("events", num(n("events")))}
    ${tile("connected", num(n("connections")))}
    ${tile("plugins", num(n("plugins")))}
    ${tile("storage", bytes(n("storage_bytes")))}
    ${tile("database", bytes(n("db_bytes")))}
    ${tile("uploads", bytes(n("upload_bytes")))}
    ${tile("uptime", duration(n("uptime_seconds")))}
    ${tile("server", String(stats["server"] ?? "—"))}
  </div>`;
  const picker = `<div class="tools">
    <label>window</label>
    ${[7, 30, 90].map((d) => `<button class="btn${d === range ? " sel" : ""}" data-act="range" data-days="${d}">${d}d</button>`).join("")}
    <span class="dim">counts per UTC day. events are counted, never read — see the privacy note in server/admin.go.</span>
  </div>`;
  if (!series) return tiles + picker + `<div class="empty">no series yet.</div>`;
  return tiles + picker + `<div class="charts">
    ${chart("messages", series.days, series.messages)}
    ${chart("signups", series.days, series.signups)}
    ${chart("files shared", series.days, series.files)}
    ${chart("operator actions", series.days, series.actions)}
  </div>`;
}

function renderUsers(): string {
  // Provisioning by hand, next to the invite flow rather than instead of it: a
  // team that already knows who is joining should not have to send links.
  return listTab({
    all: users,
    match: (u) => hits(u.nick, u.email, u.role),
    empty: "no accounts yet.",
    cols: ["nick", "email", "role", "state", "devices", "channels", "joined", ""],
    row: (u) => `<tr>
      <td class="nick">${esc(u.nick)}</td>
      <td class="dim">${esc(u.email)}</td>
      <td><span class="tag ${u.role === "admin" ? "admin" : ""}">${esc(u.role)}</span></td>
      <td>${u.suspended ? `<span class="tag susp">suspended</span>`
        : u.online ? `<span class="tag on">online</span>` : `<span class="dim">offline</span>`}</td>
      <td class="num">${u.devices}</td>
      <td class="num">${u.channels}</td>
      <td class="dim">${esc(date(u.created))}</td>
      <td class="acts">
        ${u.online ? `<button class="btn" data-act="drop" data-id="${esc(u.id)}" data-nick="${esc(u.nick)}">disconnect</button>` : ""}
        <button class="btn" data-act="passwd" data-id="${esc(u.id)}" data-nick="${esc(u.nick)}">reset password</button>
        <button class="btn" data-act="role" data-id="${esc(u.id)}" data-nick="${esc(u.nick)}" data-role="${esc(u.role)}">
          ${u.role === "admin" ? "demote" : "promote"}</button>
        <button class="btn" data-act="susp" data-id="${esc(u.id)}" data-nick="${esc(u.nick)}" data-on="${u.suspended}">
          ${u.suspended ? "unsuspend" : "suspend"}</button>
        <button class="btn danger" data-act="deluser" data-id="${esc(u.id)}" data-nick="${esc(u.nick)}">delete</button>
      </td></tr>`,
  });
}

/** The redeem link, not the bare token: the operator pastes this into whatever
 *  they already use to reach the person. The client reads ?invite= on boot. */
const inviteLink = (t: string) => `${server()}/?invite=${t}`;

function renderInvites(): string {
  return listTab({
    all: invites,
    match: (i) => hits(i.token, i.role, i.used_by),
    empty: "no invites yet.",
    cols: ["link", "role", "created", "state", ""],
    row: (i) => `<tr>
      <td><code class="link">${esc(inviteLink(i.token))}</code></td>
      <td><span class="tag ${i.role === "admin" ? "admin" : ""}">${esc(i.role)}</span></td>
      <td class="dim">${esc(date(i.created))}</td>
      <td>${i.used_by ? `<span class="dim">redeemed by ${esc(i.used_by)}</span>` : `<span class="tag on">open</span>`}</td>
      <td class="acts">
        <button class="btn" data-act="copyinvite" data-id="${esc(i.token)}">copy</button>
        ${i.used_by ? "" : `<button class="btn danger" data-act="revinvite" data-id="${esc(i.token)}">revoke</button>`}
      </td></tr>`,
  });
}

function renderPlugins(): string {
  return listTab({
    all: plugins,
    match: (p) => hits(p.id, p.display, p.kind),
    empty: "no plugins registered.",
    cols: ["plugin", "id", "kind", "state", "commands", "channels", "added", ""],
    row: (p) => `<tr>
      <td class="nick">${esc(p.display)}</td>
      <td class="dim mono">${esc(p.id)}</td>
      <td class="dim">${esc(p.kind)}</td>
      <td>${p.online ? `<span class="tag on">connected</span>` : `<span class="dim">offline</span>`}</td>
      <td class="num">${p.online ? p.commands : "—"}</td>
      <td class="num">${p.channels}</td>
      <td class="dim">${esc(date(p.created))}</td>
      <td class="acts">
        <button class="btn danger" data-act="delplugin" data-id="${esc(p.id)}" data-name="${esc(p.display)}">revoke</button>
      </td></tr>`,
  });
}

function renderChannels(): string {
  // Names and counts only — this console has no view of message bodies and
  // never will; in a +e channel the server holds nothing but ciphertext.
  return listTab({
    all: channels,
    match: (c) => hits(c.name, c.modes, c.is_dm ? "dm" : "channel"),
    empty: "no channels yet.",
    cols: ["channel", "kind", "members", "modes", "created", ""],
    row: (c) => `<tr>
      <td class="nick">${c.is_dm ? "" : "#"}${esc(c.name)}</td>
      <td class="dim">${c.is_dm ? "dm" : "channel"}</td>
      <td class="num">${c.members}</td>
      <td class="dim">+${esc(c.modes)}</td>
      <td class="dim">${esc(date(c.created))}</td>
      <td class="acts">
        <button class="btn" data-act="transfer" data-id="${esc(c.id)}" data-name="${esc(c.name)}">transfer</button>
        <button class="btn danger" data-act="archive" data-id="${esc(c.id)}" data-name="${esc(c.name)}">archive</button>
      </td></tr>`,
  });
}

function renderUploads(): string {
  return listTab({
    all: uploads,
    match: (f) => hits(f.name, f.owner, f.channel),
    empty: "no stored uploads.",
    cols: ["file", "owner", "channel", "size", "state", "expires", ""],
    row: (f) => `<tr>
      <td>${esc(f.name)}</td>
      <td class="nick">${esc(f.owner)}</td>
      <td class="dim">${esc(f.channel)}</td>
      <td class="num">${esc(bytes(f.size))}</td>
      <td class="dim">${f.complete ? "complete" : `${Math.round((f.received / f.size) * 100)}% uploaded`}</td>
      <td class="dim">${esc(date(f.expires))}</td>
      <td class="acts">
        <button class="btn danger" data-act="delfile" data-id="${esc(f.id)}" data-name="${esc(f.name)}">delete</button>
      </td></tr>`,
  });
}

/** The message of the day, and the icon it carries.
 *
 *  Both land in the chat stream as a card in a proportional font (web/src/ui.ts)
 *  rather than as a line of chat, so what is written here is read as the
 *  server talking. Saving reaches everyone who is connected right now. */
function renderSettings(): string {
  const icon = settings.icon
    ? `<img class="iconprev" src="${esc(settings.icon)}" alt="">`
    : `<div class="iconprev empty">none</div>`;
  return `<div class="wrap pad">
    <label for="motd">message of the day</label>
    <p class="hint">Shown at the live end of every channel until a reader closes it.
      Editing the text shows it again to everybody.</p>
    <textarea id="motd" rows="4" placeholder="welcome to the server">${esc(settings.motd)}</textarea>
    <label>team icon</label>
    <p class="hint">Drawn beside the message. Any picture, cropped square and
      shrunk to 128 pixels here in the browser.</p>
    <div class="iconrow">
      ${icon}
      <input type="file" id="iconfile" accept="image/*">
      ${settings.icon ? `<button class="btn danger" data-act="iconclear">remove</button>` : ""}
    </div>
    <div class="savebar"><button class="pri" data-act="savemotd">save</button></div>
  </div>`;
}

/** Any picture, cropped square and shrunk, as a data URL. The icon rides inside
 *  every hello frame, so the megabyte the operator picked never leaves this
 *  page. */
async function iconData(file: File): Promise<string> {
  const bmp = await createImageBitmap(file);
  const n = 128;
  const c = document.createElement("canvas");
  c.width = c.height = n;
  const side = Math.min(bmp.width, bmp.height);
  c.getContext("2d")!.drawImage(
    bmp, (bmp.width - side) / 2, (bmp.height - side) / 2, side, side, 0, 0, n, n);
  return c.toDataURL("image/png");
}

/** The operator's own history. Rows name their target as text, so an entry for
 *  a deleted user still reads as that user. */
function renderAudit(): string {
  return listTab({
    all: auditLog,
    match: (a) => hits(a.actor, a.action, a.target, a.detail),
    empty: "no operator actions recorded yet",
    cols: ["when", "operator", "action", "target", "detail"],
    row: (a) => `<tr>
      <td class="dim mono">${esc(a.ts.replace("T", " ").replace("Z", ""))}</td>
      <td>${esc(a.actor)}</td><td><span class="tag">${esc(a.action)}</span></td>
      <td>${esc(a.target)}</td><td class="dim">${esc(a.detail ?? "")}</td>
    </tr>`,
  });
}

/** Every list tab is the same three lines around a different table: filter the
 *  rows, print "N of M", then either an empty state or the table. That shape was
 *  written out five times — five places to fix the day the empty state or the
 *  count line changes. The parts that genuinely differ per tab (the filter
 *  predicate, the columns, the row) stay arguments. */
function listTab<T>(o: {
  all: T[];
  match: (x: T) => boolean;
  empty: string;
  cols: string[];
  row: (x: T) => string;
}): string {
  const rows = o.all.filter(o.match);
  if (!rows.length) {
    return search(o.all.length) + `<div class="empty">${q ? "nothing matches." : o.empty}</div>`;
  }
  return search(o.all.length, rows.length) + table(o.cols, rows.map(o.row));
}

const search = (total: number, shown = total) =>
  `<div class="search">
    <div class="qbox">${icons.search(14)}
      <input id="q" placeholder="filter…" autocapitalize="off" spellcheck="false" value="${esc(q)}">
    </div>
    <span class="dim">${q ? `${num(shown)} of ${num(total)}` : `${num(total)} row${total === 1 ? "" : "s"}`}</span>
  </div>`;

const table = (head: string[], rows: string[]) =>
  `<div class="wrap"><table><thead><tr>${head.map((h) => `<th>${h}</th>`).join("")}</tr></thead>
   <tbody>${rows.join("")}</tbody></table></div>`;

function paint(): void {
  if (!token || !me) return toLogin();
  if (me.role !== "admin") {
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- static template, err is esc()'d
    app.innerHTML = `<div class="auth"><div class="panel">
      <h1 class="serif">gliph</h1>
      <div class="sub">admin console</div>
      <!-- One line in the source: .err is pre-wrap, so a break here becomes a
           break and an indent on screen. -->
      <div class="err">admin only — the account <b>${esc(me.nick)}</b> is not a server admin. Ask an admin to promote it, then sign in again.</div>
      <button class="go" data-act="logout">sign out</button>
    </div></div>`;
    wire();
    return;
  }
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- tab templates esc() every server value; tab names and labels are constants
  app.innerHTML = `<div class="adm">
    <nav>
      <a class="wordmark serif" href="/admin">gliph</a>
      <div class="sub">admin console</div>
      ${TABS.map((t) => `<a href="${pathFor(t)}" data-tab="${t}" class="${t === tab ? "sel" : ""}">${t}</a>`).join("")}
      <div class="foot">
        <div class="who">${esc(me.nick)}</div>
        <button data-act="reload">reload</button>
        <button data-act="logout">sign out</button>
      </div>
    </nav>
    <main>
      <div class="head">
        <h2>${tab}</h2>
        ${CREATE[tab] ? `<button class="pri" data-act="${CREATE[tab]![0]}">${CREATE[tab]![1]}</button>` : ""}
      </div>
      ${err ? `<div class="err">${esc(err)}</div>` : ""}
      ${ok ? `<div class="ok">${esc(ok)}</div>` : ""}
      ${tab === "dashboard" ? renderDashboard() : tab === "users" ? renderUsers()
        : tab === "invites" ? renderInvites() : tab === "channels" ? renderChannels()
        : tab === "plugins" ? renderPlugins() : tab === "uploads" ? renderUploads()
        : tab === "settings" ? renderSettings() : renderAudit()}
    </main>
  </div>`;
  wire();
}

// ---- wiring ----

function wire(): void {
  // The MOTD survives a repaint the same way the filter box does: the textarea
  // is the draft until save, so a picked icon must not throw the typing away.
  const motd = app.querySelector<HTMLTextAreaElement>("#motd");
  if (motd) motd.addEventListener("input", () => { settings.motd = motd.value; });
  app.querySelector<HTMLInputElement>("#iconfile")?.addEventListener("change", async (e) => {
    const f = (e.target as HTMLInputElement).files?.[0];
    if (!f) return;
    try {
      settings.icon = await iconData(f);
      err = "";
    } catch {
      err = "that file is not a picture this browser can read";
    }
    paint();
  });
  // Filtering repaints the whole page, so the box has to take its focus and
  // caret back afterwards or typing a second character is impossible.
  const box = app.querySelector<HTMLInputElement>("#q");
  if (box) {
    box.addEventListener("input", () => { q = box.value; paint(); });
    if (q) { box.focus(); box.setSelectionRange(q.length, q.length); }
  }
}

// One delegated listener for the whole console: paint() replaces the markup on
// every action, so per-button listeners would pile up on the surviving #app.
app.addEventListener("click", onClick);

async function onClick(e: MouseEvent): Promise<void> {
  const nav = (e.target as HTMLElement).closest<HTMLElement>("a[data-tab]");
  if (nav) {
    // A real <a href> so it opens in a new tab, copies as a link and works with
    // a middle click. Only the plain left click is taken over for pushState.
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
    e.preventDefault();
    return void go(nav.dataset.tab as Tab);
  }
  const b = (e.target as HTMLElement).closest("button");
  if (!b) return;

  const id = b.dataset.id ?? "";
  const name = b.dataset.nick ?? b.dataset.name ?? "";
  // Every destructive action confirms first. window.confirm is modal, keyboard
  // accessible and impossible to mis-wire — a hand-rolled dialog would be all
  // three worse.
  switch (b.dataset.act) {
    case "reload": err = ""; ok = ""; return void load();
    case "iconclear":
      settings.icon = "";
      return paint();
    case "savemotd":
      return void act("saved — everyone connected has it now", "/api/admin/settings", {
        method: "POST",
        body: JSON.stringify({ motd: settings.motd, icon: settings.icon }),
      });
    case "range":
      range = Number(b.dataset.days);
      return void load();
    case "logout":
      clearSession();
      token = ""; me = null; err = ""; ok = "";
      return paint();
    case "newuser": {
      const v = await modal({
        title: "create account",
        submit: "create account",
        note: "there is no mailer. Read the password out yourself, and have them change it.",
        fields: [
          { name: "nick", label: "nick", placeholder: "robin", required: true },
          { name: "email", label: "email", placeholder: "robin@example.com", required: true },
          { name: "password", label: "password", type: "password", hint: "shown in the clear so you can dictate it — regenerate, or type your own." },
          { name: "role", label: "role", type: "select", options: ["member", "admin"] },
        ],
      });
      if (!v) return;
      await act(`${v.nick} created as ${v.role} — password (copied): ${v.password}`, "/api/admin/users",
        { method: "POST", body: JSON.stringify(v) });
      if (!err) await copy(v.password);
      return;
    }
    case "passwd": {
      // The operator hands the new password over out of band; the account's
      // live sessions are dropped server-side, which is the point.
      const v = await modal({
        title: `reset ${name}'s password`,
        submit: "reset password",
        note: `${name} is signed out everywhere and has to use this to get back in.`,
        fields: [{ name: "password", label: "new password", type: "password" }],
      });
      if (!v) return;
      await act(`${name}'s password reset (copied): ${v.password} — they are signed out everywhere`,
        `/api/admin/users/${enc(id)}/password`, { method: "POST", body: JSON.stringify(v) });
      if (!err) await copy(v.password);
      return;
    }
    case "role": {
      const to = b.dataset.role === "admin" ? "member" : "admin";
      if (!await ask(`make ${name} a server ${to}?`,
        to === "admin" ? `${name} gets the console and every channel.` : `${name} loses the console.`,
        `make ${to}`, false)) return;
      return void act(`${name} is now ${to}`, `/api/admin/users/${enc(id)}/role`,
        { method: "POST", body: JSON.stringify({ role: to }) });
    }
    case "susp": {
      const on = b.dataset.on === "true";
      if (!await ask(on ? `restore ${name}?` : `suspend ${name}?`,
        on ? `${name} can sign in again.` : `${name} is disconnected immediately and cannot log back in.`,
        on ? "restore" : "suspend", !on)) return;
      return void act(on ? `${name} restored` : `${name} suspended`,
        `/api/admin/users/${enc(id)}/${on ? "unsuspend" : "suspend"}`, { method: "POST" });
    }
    case "deluser":
      if (!await ask(`delete ${name}?`,
        "Their memberships and devices go with them. This cannot be undone.", "delete")) return;
      return void act(`${name} deleted`, `/api/admin/users/${enc(id)}`, { method: "DELETE" });
    case "newchannel": {
      const v = await modal({
        title: "create channel",
        submit: "create channel",
        note: "opens public and empty — anyone can /join it, and whoever runs it takes ops there.",
        fields: [
          { name: "name", label: "name", placeholder: "support", required: true },
          { name: "topic", label: "topic", placeholder: "who to ask when it breaks" },
        ],
      });
      if (!v) return;
      return void act(`#${v.name.replace(/^#/, "")} created`, "/api/admin/channels",
        { method: "POST", body: JSON.stringify(v) });
    }
    case "archive":
      if (!await ask(`archive #${name}?`,
        "Every member is removed and the channel is set invite-only. History is kept.", "archive")) return;
      return void act(`#${name} archived`, `/api/admin/channels/${enc(id)}/archive`, { method: "POST" });
    case "transfer": {
      const v = await modal({
        title: `transfer #${name}`,
        submit: "transfer",
        note: "They are joined as op.",
        fields: [{ name: "nick", label: "to nick", required: true }],
      });
      if (!v) return;
      const nick = v.nick;
      return void act(`#${name} transferred to ${nick}`, `/api/admin/channels/${enc(id)}/transfer`,
        { method: "POST", body: JSON.stringify({ nick }) });
    }
    case "drop":
      if (!await ask(`disconnect ${name}?`,
        "Their clients are dropped and have to sign in again. The account is untouched.", "disconnect")) return;
      return void act(`${name} disconnected`, `/api/admin/users/${enc(id)}/disconnect`, { method: "POST" });
    case "newinvite": {
      const v = await modal({
        title: "create invite",
        submit: "create invite",
        note: "the link is copied to your clipboard. Unused invites can be revoked.",
        fields: [{ name: "role", label: "role", type: "select", options: ["member", "admin"] }],
      });
      if (!v) return;
      const role = v.role;
      try {
        err = ""; ok = "";
        const d = await api<{ invite: string }>("/api/admin/invites",
          { method: "POST", body: JSON.stringify({ role }) });
        await copy(inviteLink(d.invite));
        ok = `invite created and copied: ${inviteLink(d.invite)}`;
      } catch (e2) {
        err = String((e2 as Error).message);
      }
      invites = await api<Invite[]>("/api/admin/invites").catch(() => invites);
      return paint();
    }
    case "copyinvite":
      await copy(inviteLink(id));
      ok = "invite link copied";
      return paint();
    case "revinvite":
      if (!await ask("revoke this invite?", "The link stops working immediately.", "revoke")) return;
      return void act("invite revoked", `/api/admin/invites/${enc(id)}`, { method: "DELETE" });
    case "newplugin": {
      const v = await modal({
        title: "register plugin",
        submit: "register",
        note: "the token is shown once, here, and copied. There is no route that reads it back.",
        fields: [
          { name: "id", label: "id", placeholder: "standup", required: true },
          { name: "display", label: "name", placeholder: "Standup Bot" },
          { name: "kind", label: "kind", type: "select", options: ["bot", "agent"] },
        ],
      });
      if (!v) return;
      const pid = v.id;
      try {
        err = ""; ok = "";
        const p = await api<{ id: string; token: string }>("/api/admin/plugins",
          { method: "POST", body: JSON.stringify(v) });
        await copy(p.token);
        // Shown once. There is no route that reads a plugin token back.
        ok = `${pid} registered. Token (copied, shown only now): ${p.token}`;
      } catch (e2) {
        err = String((e2 as Error).message);
      }
      plugins = await api<Plugin[]>("/api/admin/plugins").catch(() => plugins);
      return paint();
    }
    case "delplugin":
      if (!await ask(`revoke ${name}?`,
        "Its token stops working, it is disconnected, and it leaves every channel.", "revoke")) return;
      return void act(`${name} revoked`, `/api/admin/plugins/${enc(id)}`, { method: "DELETE" });
    case "delfile":
      if (!await ask(`delete ${name}?`,
        "The file is removed from disk and its link stops working.", "delete")) return;
      return void act(`${name} deleted`, `/api/admin/uploads/${enc(id)}`, { method: "DELETE" });
  }
}

// ---- boot ----

(async () => {
  tab = tabFromPath();
  if (token) {
    try {
      me = await api<{ nick: string; role: string }>("/api/me");
    } catch {
      // A stale or suspended token. Clearing it is what stops the bounce to the
      // login page from bouncing straight back here on the next load.
      token = "";
      clearSession();
    }
  }
  if (me?.role === "admin") await load(); else paint();
})();
