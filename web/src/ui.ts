import { hasDefaultServer, type Client, type EventRow, type SysMsg, type Transfer } from "./client";
import { renderServerBar } from "./servers";
import { icons } from "./icons";
import type { ChannelView, Command, Member } from "./protocol";
import { isImageName, renderCards, unfurlMode } from "./cards";
import { hasCodeBlock, renderInline, renderWithCodeBlocks, urlsIn } from "./format";
import { renderDoc, renderSwitcher } from "./gum";
import type { Entry as UndoEntry } from "./undo";
import { isNearby, meshSupported } from "./mesh";
import { downloadsURL, getOS, isTauri } from "./desktop";
import { esc } from "./escape.ts";


const hhmm = (iso: string) => {
  const d = new Date(iso);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
};

const dayKey = (iso: string) => new Date(iso).toDateString();

import { nickColor } from "./alertcard";

/** Render a message body: prose, fenced code, and — beneath it — cards for any
 *  media it linked to.
 *
 *  The cards start EMPTY. Nothing remote is fetched until the reader asks, which
 *  is the whole point; see cards.ts for why a chat client that auto-loads a
 *  pasted image is a broadcast tracking pixel. */
function renderBody(text: string): string {
  const body = hasCodeBlock(text) ? renderWithCodeBlocks(text) : renderInline(text);
  return body + renderCards(urlsIn(text));
}

/** The text to show for an event. The core has already resolved this: `text` is
 *  the plaintext, the decrypted copy, or null when this device has no key —
 *  which is the one case the UI has to word for itself. */
function bodyOf(e: EventRow): { text: string; locked: boolean } {
  if (e.text !== null) return { text: e.text, locked: false };
  return { text: "cannot decrypt — this device has no key for this message", locked: true };
}

/** Kinds you can act on with the right-click menu. A join line is not
 *  something anyone replies to, so it carries no handle. */
const ACTIONABLE = ["message", "action", "agent", "file"];

/** Said in the 3D room: the line is marked with the room it was said in, and
 *  the gradient lives inside the glyphs so the mono column stays a column. */
function roomMark(e: EventRow): string {
  const room = String(e.meta?.["room"] ?? "");
  return room ? `<span class="rmark">[${esc(room)}]</span> ` : "";
}

/** An image attachment under its line: capped at half the viewport, cut off
 *  there, [ - ] / [ + ] to resize, a click for the lightbox. Empty for anything
 *  that is not a picture. */
function attachment(url: string, name: string): string {
  if (!isImageName(name)) return "";
  return `<div class="att" data-kind="image" data-url="${esc(url)}"` +
    ` data-name="${esc(name)}" data-auto>` +
    `<div class="attsize"><button data-att="-" title="show less">[ - ]</button>` +
    `<button data-att="+" title="show more">[ + ]</button></div>` +
    `<div class="slot"></div></div>`;
}

function renderEvent(c: Client, e: EventRow, onScreen: Set<string>): string {
  const time = `<span class="t">[${hhmm(e.ts)}]</span>`;
  const pending = e.meta?.["pending"] ? " pending" : "";
  const hl = e.highlight ? " hl" : "";
  const handle = ACTIONABLE.includes(e.kind) ? ` data-ev="${esc(e.id)}"` : "";
  const row = (inner: string, kind = e.kind) =>
    `<div class="msg kind-${esc(kind)}${pending}${hl}"${handle}>${time}<span class="b">${inner}</span></div>`;

  switch (e.kind) {
    case "message": {
      const { text, locked } = bodyOf(e);
      const nick = `<span class="nick" style="color:${nickColor(e.actor)}">&lt;${esc(e.actor)}&gt;</span> `;
      if (locked) return `<div class="msg locked">${time}<span class="b">${nick}${esc(text)}</span></div>`;
      // The core has already lifted the quote off a reply. Shown above, clipped
      // to one line; the reply itself gets the gutter bar.
      //
      // The quote is a jump when the thing it answers is actually on screen —
      // `meta.reply_to` is the id the server recorded. A quote whose original
      // has scrolled out of the cache stays readable and simply is not a link,
      // which is more honest than a button that does nothing.
      const target = String(e.meta?.["reply_to"] ?? "");
      const jump = target && onScreen.has(target) ? ` data-jump="${esc(target)}"` : "";
      const mark = roomMark(e);
      return e.quote
        ? row(`<span class="quoted"${jump}>${renderInline(e.quote)}</span>` +
              `<span class="replied">${mark}${nick}${renderBody(text)}</span>`)
        : row(mark + nick + renderBody(text));
    }

    case "action":
      return row(`${roomMark(e)}* ${esc(e.actor)} ${esc(bodyOf(e).text)}`);

    // Someone opened the room and asked you in. The line is written once and
    // never updated: whether join still works is read off who is in there
    // now, which is the strip's business, not this row's.
    case "room_invite": {
      const target = String(e.meta?.["target"] ?? "");
      const mine = c.user?.id === target;
      // A personal invite is posted in the DM but names the channel whose room
      // was opened: that is the room the join button walks into, and the one
      // whose strip says whether anybody is still in there.
      const chan = String(e.meta?.["channel"] ?? e.channel);
      const strip = c.rooms.find((r) => r.channel === chan);
      const inside = (strip?.people.length ?? 0) > 0;
      const what = target
        ? `${esc(e.actor)} ${mine ? "asked you into" : "asked " + esc(String(e.meta?.["target_nick"] ?? "someone")) + " into"} room0 · ${esc(e.body)}`
        : `${esc(e.actor)} opened room0 · ${esc(e.body)} · spawn beside them`;
      const acts = inside
        ? `<button class="rjoin" data-act="room-join" data-chan="${esc(chan)}">` +
          `${strip?.you_inside ? "back to the room" : "join"}</button>` +
          `<a class="rcancel" data-act="room-dismiss">cancel</a>`
        : `<button class="rjoin off" disabled>join</button>` +
          `<span class="rcancel">nobody inside</span>`;
      return `<div class="msg kind-room_invite">${time}<span class="b">` +
        `<span class="rtext">*** ${what}</span><span class="racts">${acts}</span>` +
        `</span></div>`;
    }

    case "agent":
      return row(`<span class="nick agent">&lt;${icons.brand(String(e.meta?.agent ?? "")) || icons.diamond()}${esc(e.actor)}&gt;</span> ` +
        `<span class="badge">AGENT</span> ${renderBody(e.body)}`, "message");

    case "join":
      return row(`*** ${esc(e.actor)} has joined${e.body ? " — " + esc(e.body) : ""}`);

    case "part":
      return row(`*** ${esc(e.actor)} has left`);

    case "mode":
      return row(`*** ${esc(e.actor)} ${esc(e.body)}`);

    case "topic":
      return row(`*** ${esc(e.actor)} set the topic: ${esc(e.body)}`);

    case "kick":
      return row(`*** ${esc(String(e.meta?.["target"] ?? ""))} was kicked by ${esc(e.actor)} (${esc(e.body)})`);

    case "notice":
      return row(`*** ${esc(e.actor)} ${esc(e.body)}`);

    case "file": {
      const id = String(e.meta?.["file_id"] ?? "");
      const size = Number(e.meta?.["size"] ?? 0);
      const mb = size > 1 << 20 ? `${(size / (1 << 20)).toFixed(1)} MB` : `${Math.ceil(size / 1024)} KB`;
      const exp = e.meta?.["expires_at"] ? ` · expires ${new Date(String(e.meta["expires_at"])).toLocaleDateString()}` : "";
      const at = `<span class="fmeta">@</span> `;
      const who = `<span class="nick" style="color:${nickColor(e.actor)}">&lt;${esc(e.actor)}&gt;</span>`;
      // A transfer that went peer to peer: only the two ends have this line at
      // all, and there is no server holding the bytes to link to. The preview
      // comes off the copy in hand — the receiver's, saved when it landed, or
      // the sender's original — and is gone once the tab is.
      if (e.meta?.["p2p"]) {
        const peer = esc(String(e.meta["peer"] ?? ""));
        const what = e.meta["dir"] === "send"
          ? `you sent ${peer} a file: <span class="fname">${esc(e.body)}</span>`
          : `${who} sent you a file: <span class="fname">${esc(e.body)}</span>`;
        const held = c.p2pURL(e.body, size);
        return row(
          `${at}${what} <span class="fmeta">(${mb} · direct)</span>` +
          (held ? attachment(held, e.body) : ""),
          "message",
        );
      }
      // An image that came through this server is not somebody else's URL: the
      // reader is already authenticated to it and the bytes are already ours.
      // So it previews without asking — capped at half the viewport, cut off
      // there, and the lightbox shows the rest.
      const preview = attachment(c.fileURL(id), e.body);
      return row(
        `${at}${who} sent a file: ` +
        `<a class="fname" href="${esc(c.fileURL(id))}" download>${esc(e.body)}</a> ` +
        `<span class="fmeta">(${mb}${esc(exp)})</span>` + preview,
        "message",
      );
    }
    default:
      return row(esc(e.body));
  }
}

/** How long the room has been running, in the roughest unit that is true.
 *  The core keeps no clock, so the arithmetic is here. */
function elapsed(since: string): string {
  const ms = Date.now() - new Date(since).getTime();
  if (!since || !Number.isFinite(ms) || ms < 0) return "";
  const m = Math.floor(ms / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h${m % 60 ? `${m % 60}m` : ""}`;
}

/** The live room line, pinned above the composer while anybody is inside.
 *  One per channel and only for the one being read: who is in there, where,
 *  and the way in. It updates in place, which is why nobody's coming and
 *  going is written to the stream at all. */
function renderRoomStrip(c: Client, hidden: string): string {
  const r = c.rooms.find((x) => x.channel === c.active);
  if (!r || c.activeBlob || hidden === c.active) return "";
  const dots = r.people
    .map((p) => `<span class="rdot" style="background:${nickColor(p.name)}" title="${esc(p.name)} · ${esc(p.room)}"></span>`)
    .join("");
  const names = r.people.map((p) => esc(p.name)).join(", ");
  const when = elapsed(r.since);
  // "you_inside" here means another device of yours is standing in there —
  // the room is a full-screen surface, so you cannot be reading this and be
  // in it on the same screen. Either way the button is the way in.
  const acts =
    `<button class="rjoin" data-act="room-join" data-chan="${esc(r.channel)}">` +
    `${r.you_inside ? "back to the room" : "join"}</button>` +
    `<a class="rcancel" data-act="room-hide">hide</a>`;
  return `<div class="rstrip">
    <span class="rdots">${dots}</span>
    <span class="rmark">room0</span>
    <span class="rwho">${esc(r.room)} · ${names}${when ? " · " + when : ""}</span>
    ${r.voice_count ? `<span class="rvoice">voice ${r.voice_count}</span>` : ""}
    ${r.you_quiet ? `<span class="rquiet">you · quiet</span>` : ""}
    <span class="racts">${acts}</span>
  </div>`;
}

/** The app-icon unicorn, downsampled to a character grid. The letters are
 *  palette slots, not text: k outline, w coat, p/y/g/b the mane. */
const UNICORN = [
  "..........................k.",
  ".........................kyk",
  "........................kwyk",
  ".......................kwyk.",
  "......................kyyk..",
  "..........kkkkk.k.kk.kywyk..",
  "........kkpppppkwkppkywyk...",
  ".......kppppppkwwkpkywyk....",
  "......kpppyyykwwwkkywyyk....",
  ".....kpppyyggkwwwkkyyyk.k...",
  "....kpppyyggbkwwwwwkykpkpk..",
  "....kppyyggbkwwwwwwwkkpppk..",
  "...kpppyggbbkwwwwwwwwwkpk...",
  "...kppyygbbkwwwwwwewwwwk....",
  "..kppgyygbbkwwwwwwkkwwwk....",
  "..kppgyygbbkwwwwwwwwwwwwk...",
  ".kpppgyyggbkwkwwwwwwwwwwwk..",
  ".kpppgyyggbkwkwwwwwwwwwwwwk.",
  ".kpppggyyggbkwkwwwwwwwwwwwwk",
  ".kpppggyyyggkwwkwwwwwwwwwkwk",
  "..kpppggyyygbkwwkkkkkwwwwwwk",
  "..kpppyggyygg.kwwk...kkwwwk.",
  "...kpppyggyggbkwwk.....kkk..",
  "....kppygggygb.kwwk.........",
  ".k..kppyyggggb.kwwk.........",
  "kpkkpppyyggbgb.kwwwk........",
  "kppppppyygbbgbkwwwwk........",
  "kpppppyyggbgb.kwwwwwk.......",
  ".kpppyyggbbb.kwwwwwwk.......",
  "..kkygggbbb.kwwwwwwwk.......",
  "....k.bbbb.kkkkkkkkkk.......",
  ".....k....k.................",
  "......kkkk..................",
];

const COAT: Record<string, string> = {
  k: "var(--border)", w: "var(--border-2)", p: "var(--accent)",
  y: "var(--gold)", g: "var(--join)", b: "var(--link)", e: "var(--text)",
};

/** Top-of-history watermark: the icon, drawn in block characters. */
const splash = () =>
  `<pre class="splash" aria-hidden="true">` +
  UNICORN.map((r) =>
    r.replace(/(.)\1*/g, (run, c) =>
      c === "." ? " ".repeat(run.length)
      : `<span style="color:${COAT[c]}">${"\u2588".repeat(run.length)}</span>`),
  ).join("\n") + `</pre>`;

/** The browser's pitch, right under the watermark where every signed-in
 *  session starts. The desktop app is what it is pitching, so it never shows
 *  there. */
const PLATFORMS = [["mac", "macOS", icons.apple], ["windows", "Windows", icons.windows], ["linux", "Linux", icons.linux]] as const;
const getApp = (server: string) => {
  if (isTauri()) return "";
  const os = ({ macos: "mac", windows: "windows", linux: "linux" } as const)[getOS()];
  const label = PLATFORMS.find(([k]) => k === os)![1];
  return `<div class="getapp splitwrap">
    <div class="split">
      <a class="go" href="${esc(downloadsURL(server, os))}">${icons.download()} get Gliph for ${label}</a>
      <button class="gocaret" data-act="appmenu" aria-haspopup="true" aria-expanded="false"
        aria-label="other platforms" title="other platforms">&#9660;</button>
    </div>
    <div class="appmenu" hidden>
      ${PLATFORMS.map(([k, l, i]) => `<a href="${esc(downloadsURL(server, k))}">${i()}${l}</a>`).join("")}
      <span class="soon" aria-disabled="true">${icons.phone()}iOS <small>soon</small></span>
      <span class="soon" aria-disabled="true">${icons.android()}Android <small>soon</small></span>
    </div>
  </div>`;
};

// ---- system messages: the MOTD, and the notes for a release ----
//
// These deliberately break the typeface. Everything else in the stream is one
// monospace column because it is chat; a system message is the product
// speaking, so it is drawn as a card in a normal font with an icon on it, and
// nobody mistakes it for someone in the channel.
//
// Which cards are open. Module state rather than a class on the node: the
// stream is thrown away and rebuilt on every event, so a reader who expanded
// a note would watch it fold itself up the moment anybody spoke.
const openSys = new Set<string>();
export const toggleSystem = (id: string) => {
  if (!openSys.delete(id)) openSys.add(id);
};

const sysDate = (iso: string) => {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleDateString(undefined, { day: "numeric", month: "long" });
};

function renderSystem(c: Client, m: SysMsg): string {
  // The icon is inline bytes or ours. Never a URL from anywhere else — same
  // rule as cards.ts, for the same reason.
  const icon = m.kind === "motd" && m.icon.startsWith("data:image/") ? m.icon : "/unicorn.png";
  const kicker = m.kind === "motd" ? c.serverName || "message of the day" : "what's new";
  return `<div class="sysmsg${openSys.has(m.id) ? " open" : ""}" data-sys="${esc(m.id)}">
    <img class="sysicon" src="${esc(icon)}" alt="">
    <div class="syscol">
      <div class="syshead">
        <span class="syskick">${esc(kicker)}</span>
        ${m.date ? `<span class="sysdate">${esc(sysDate(m.date))}</span>` : ""}
      </div>
      ${m.title ? `<div class="systitle">${esc(m.title)}</div>` : ""}
      <div class="systext">${renderInline(m.body)}</div>
      <button class="sysmore" data-sysmore="${esc(m.id)}" hidden>read more</button>
    </div>
    ${m.kind === "motd" ? `<button class="sysx" data-sysx="${esc(m.id)}" title="dismiss" aria-label="dismiss">×</button>` : ""}
  </div>`;
}

// Only the newest window of a long channel is in the DOM — a year of chat as
// one innerHTML per keystroke is where the memory goes. Scrolling to the top
// widens the window (main.ts calls growStream and repaints). Module state for
// the same reason as openSys above.
const STREAM_WINDOW = 200;
let streamCap = STREAM_WINDOW;
let streamAt = "";
export const growStream = () => { streamCap += STREAM_WINDOW; };

function renderStream(c: Client): string {
  if (c.active !== streamAt) { streamAt = c.active; streamCap = STREAM_WINDOW; }
  const all = c.visibleEvents();
  const evs = all.length > streamCap ? all.slice(-streamCap) : all;
  const onScreen = new Set(evs.map((e) => e.id));
  // The splash belongs above the first line ever said, not above line 1800 of
  // a cut window; while cut, an anchor for the scroll loader stands in.
  const parts: string[] = evs.length < all.length
    ? [`<div class="divider" id="older">-- earlier messages --</div>`]
    : [splash() + getApp(c.server)];
  let day = "";
  for (const e of evs) {
    const k = dayKey(e.ts);
    if (k !== day) {
      day = k;
      parts.push(`<div class="divider">-- ${esc(new Date(e.ts).toDateString().toLowerCase())} --</div>`);
    }
    parts.push(renderEvent(c, e, onScreen));
  }
  // At the live end of the stream, under the newest line: a system message is
  // news, and news belongs where the reader already is.
  for (const m of c.systemMessages()) parts.push(renderSystem(c, m));
  for (const n of c.channelNotices().slice(-8)) {
    parts.push(`<div class="sysnotice ${n.level === "error" ? "error" : ""}">${renderInline(n.text)}</div>`);
  }
  if (!parts.length) {
    parts.push(`<div class="sysnotice">no one has spoken here yet. say something, or /topic to set the scene.</div>`);
  }
  for (const [, tr] of c.transfers) {
    const pct = tr.total ? Math.round((tr.bytes / tr.total) * 100) : 0;
    const via = tr.transport === "unknown" ? "connecting" : tr.transport;
    parts.push(
      `<div class="sysnotice">${esc(tr.dir === "send" ? "sending" : "receiving")} ` +
      `${esc(tr.name)} — ${pct}% · ${esc(via)}` +
      (tr.rate ? ` · ${Math.round(tr.rate / 1024)} KB/s` : "") + `</div>`,
    );
  }
  // A nearby channel does not care that the server is down — saying messages
  // "will queue" there would be a lie about the one thing still working.
  if (!c.online && !isNearby(c.active)) {
    parts.push(`<div class="sysnotice error">*** disconnected — messages you send will queue until the server is back</div>`);
  }
  return parts.join("");
}

/** The core's QR rows as one SVG path: a 1x1 square per dark module, with a
 *  4-module quiet zone. Only '1'/'0' reach the markup. */
function qrSvg(rows: string[]): string {
  if (!rows?.length) return "";
  const n = rows.length + 8;
  let d = "";
  rows.forEach((r, y) => { for (let x = 0; x < r.length; x++) if (r[x] === "1") d += `M${x + 4} ${y + 4}h1v1h-1z`; });
  return `<svg class="qr" viewBox="0 0 ${n} ${n}" role="img" aria-label="approval QR code" shape-rendering="crispEdges">` +
    `<rect width="${n}" height="${n}" fill="#fff"/><path d="${d}" fill="#000"/></svg>`;
}

/** The device-approval bar: this device waiting, or another one waiting on it. */
function renderDeviceBar(c: Client): string {
  const d = c.device;
  if (!d.known || c.solo) return "";
  if (!d.signed) {
    return `<div class="devicebar">${qrSvg(d.qr)}<span>this device is not approved yet, so it cannot read encrypted channels. ` +
      `scan this with your phone's camera, or on a device you already use, type <b>/approve ${esc(d.code)}</b></span></div>`;
  }
  return d.pending.map((p) =>
    `<div class="devicebar">${esc(p.name || "a new device")} wants to read your encrypted channels. ` +
    `if it shows ${esc(p.code)}, type <b>/approve ${esc(p.code)}</b></div>`).join("");
}

const sigil = (m: Member) =>
  m.role === "op" ? `<span class="sigil op">@</span>`
  : m.role === "voice" ? `<span class="sigil voice">+</span>`
  : `<span class="sigil plain">${icons.member()}</span>`;

function renderRail(c: Client): string {
  const ch = c.channel();

  const chans = c.channels.filter((x) => !x.is_dm && !isNearby(x.id));
  const near = c.channels.filter((x) => isNearby(x.id));
  const dms = c.channels.filter((x) => x.is_dm);

  const row = (x: ChannelView) => {
    const unread = c.unread(x.id);
    // A DM is named after the other person; their presence is the dot.
    const peerOn = x.is_dm && (x.members.find((m) => m.nick === x.name)?.online ?? false);
    return `<button class="rl ${x.id === c.active ? "sel" : ""}" data-chan="${esc(x.id)}">` +
      (x.is_dm
        ? `<span class="rmark dm ${peerOn ? "on" : "off"}">${icons.dot(peerOn)}</span>`
        : `<span class="rmark ${isNearby(x.id) ? "near" : ""}">${icons.hash()}</span>`) +
      `<span>${esc(x.name)}</span>` +
      (x.modes.includes("i") ? `<span class="rmark">${icons.lock(10)}</span>` : "") +
      (unread ? `<span class="count">${unread}</span>` : "") +
      `</button>`;
  };

  // Presence the IRC way: who is here is the list; who is not is greyed, not
  // hidden, because a channel is also its regulars.
  const members = (ch?.members ?? []).map((m) =>
    `<div class="rl ${m.online || m.kind !== "user" ? "on" : "off"}${m.away ? " away" : ""}">${m.kind === "user" ? sigil(m) : `<span class="sigil bot">${icons.diamond()}</span>`}` +
    `<span>${esc(m.nick)}</span>` +
    (m.unverified ? `<span class="unverified" title="has a device that is not approved yet. it reads nothing encrypted">unapproved device</span>` : "") +
    (m.away ? `<span class="away" title="away: ${esc(m.away)}">${esc(m.away)}</span>` : "") +
    `</div>`).join("");

  return `
    <div class="boxtitle">Channels</div>
    <div class="railgroup">${chans.map(row).join("")}</div>
    ${c.nearby.on ? `<div class="boxtitle">Nearby <span class="boxcount">${c.nearby.peers.length} peer${c.nearby.peers.length === 1 ? "" : "s"}</span></div>
    <div class="railgroup">${near.length ? near.map(row).join("")
      : `<div class="railfoot tight">/nearby #name to open one</div>`}</div>` : ""}
    ${dms.length ? `<div class="boxtitle">Direct</div><div class="railgroup">${dms.map(row).join("")}</div>` : ""}
    ${ch ? `<div class="boxtitle">Members <span class="boxcount">${ch.members.length}</span></div>
    <div class="railgroup">${members}</div>` : ""}
    <div class="railfoot">
      ${esc(c.serverName || "gliph")}<br>
      ${c.commandCount} commands · ${c.pluginCommandCount} from plugins
    </div>`;
}

function renderPalette(cmds: Command[], sel: number): string {
  if (!cmds.length) return "";
  return `<div class="palette" id="palette" role="listbox" aria-label="commands">${cmds.map((c, i) =>
    `<div class="row ${i === sel ? "sel" : ""}" id="cmd-${i}" role="option" aria-selected="${i === sel}" data-cmd="${esc(c.name)}">` +
    `<span class="usage">${esc(c.usage)}</span>` +
    `<span class="desc">${esc(c.desc)}</span>` +
    (c.source !== "builtin" ? `<span class="src">${esc(c.source)}</span>` : "") +
    `</div>`).join("")}</div>`;
}

/** The "you just did a thing" toast. Shows for every op action so you can see
 *  what landed, with an undo button only when something honestly reverses it —
 *  a kick appears here and offers nothing, because nothing un-kicks. */
export function renderUndoToast(entry: UndoEntry | null): string {
  if (!entry) return "";
  return `<div class="undotoast">
    <span class="what">${esc(entry.label)}</span>
    ${entry.inverse ? `<button data-act="undo">undo</button>` : ""}
    <button class="x" data-act="dismisstoast" title="dismiss">×</button>
  </div>`;
}

/** The desktop updater's one button: a gold star and the version waiting, in
 *  the same azure as send so it reads as "press this". Once pressed it counts
 *  the download down and then the app relaunches itself. */
function renderUpdate(u: { version: string; progress: number | null; busy: boolean } | null): string {
  if (!u) return "";
  const label = !u.busy ? `update → ${esc(u.version)}`
    : u.progress === null ? "updating…"
    : u.progress >= 1 ? "restarting…"
    : `updating ${Math.round(u.progress * 100)}%`;
  return `<button class="update" data-act="update" ${u.busy ? "disabled" : ""}
    title="a newer gliph is out — this downloads it, installs it and restarts">
    <span class="star">★</span> ${label}</button>`;
}

/** menubar labels: the word on a desktop, an ascii token on a phone. */
const mlabel = (token: string, word: string) =>
  `<span class="only-sm">${token}</span><span class="hide-sm">${esc(word)}</span>`;

export function render(
  c: Client,
  state: {
    palette: Command[]; sel: number; railOpen: boolean; notifyMode: string;
    update: { version: string; progress: number | null; busy: boolean } | null;
    /** The channel whose room strip the reader waved away this session. */
    hiddenStrip: string;
  },
): string {
  const ch = c.channel();
  const theme = c.theme();
  const notifyMode = state.notifyMode;
  const unfurl = unfurlMode();
  return `
  <div class="app">
    <div class="menubar">
      <span class="wordmark serif">gliph</span>
      <button data-act="list">${mlabel("[#]", "channels")}</button>
      <button data-act="theme" title="theme">${mlabel(theme === "creme" ? "[*]" : "[o]", `theme: ${theme === "creme" ? "midnight" : "creme"}`)}</button>
      <button data-act="alerts" title="whether an arrival also raises a desktop notification — the panel always has it either way">${mlabel(`[!${notifyMode === "system" ? "+" : "-"}]`, `alerts: ${notifyMode === "custom" ? "desktop" : notifyMode}`)}</button>
      <button data-act="unfurl" title="whether media in links is loaded (${esc(unfurl)}) — gliph never fetches a link for you">${mlabel(`[^${unfurl === "ask" ? "?" : unfurl === "always" ? "+" : "-"}]`, `media: ${unfurl}`)}</button>
      <span class="spacer"></span>
      ${renderServerBar(c)}
      ${renderUpdate(state.update)}
      <button data-act="help">${mlabel("[?]", "help")}</button>
      ${c.user?.role === "admin"
        ? `<button data-act="manage" title="server admin console: accounts, invites, channels, plugins">${mlabel("[@]", "manage")}</button>`
        : ""}
      <button data-act="logout">${mlabel("[x]", "quit")}</button>
    </div>

    <div class="body">
      <div class="main">
        <div class="titlestrip">
          <button class="mobileback" data-act="rail">${icons.back()}</button>
          <span class="name">${ch ? (ch.is_dm ? esc(ch.name) : "#" + esc(ch.name)) : "gliph"}</span>
          <span class="flags">${ch ? `[${ch.members.length}] [+${esc(ch.modes)}]` : ""}</span>
          ${ch?.modes.includes("e") ? `<span class="e2e" title="end-to-end encrypted">${icons.lock(11)} e2e</span>` : ""}
          ${ch && !ch.is_dm ? (ch.history === "sealed"
            ? `<span class="history" title="people who join read only what is said after they join">history: sealed</span>`
            : `<span class="history" title="people who join read what was said before they joined">history: open</span>`) : ""}
          <span class="topic">${esc(ch?.topic ?? "")}</span>
          ${renderSwitcher(c.blobs(), c.activeBlob)}
          <button class="railtoggle" data-act="rail" title="members">${icons.members()}</button>
        </div>

        ${renderDeviceBar(c)}

        ${c.activeBlob
          ? `<div class="stream" id="stream" role="region" aria-label="${esc(c.activeBlob)}">${renderDoc(c.surface())}</div>`
          : `<div class="stream" id="stream" role="region" aria-label="messages">${renderStream(c)}</div>`}

        ${renderRoomStrip(c, state.hiddenStrip)}

        <div class="composer" ${c.activeBlob ? "hidden" : ""}>
          ${renderPalette(state.palette, state.sel)}
          ${c.replyTo
            ? `<div class="replybar"><span class="q">replying to ${esc(c.replyTo)}</span>` +
              `<button class="x" data-act="unreply" title="cancel the reply (esc)">×</button></div>`
            : ""}
          <div class="box">
            <input id="input" autocomplete="off" autocapitalize="off" spellcheck="false"
              aria-label="message" ${state.palette.length ? `role="combobox" aria-expanded="true" aria-controls="palette" aria-activedescendant="cmd-${state.sel}"` : ""}
              placeholder="${ch ? (ch.is_dm ? "message " + esc(ch.name) : "message #" + esc(ch.name)) : "…"}  (/ for commands)">
            <span class="tools">
              <button class="attach" data-act="attach" title="attach a file">${icons.clip()}</button>
              <button class="send" data-act="send" title="send">${icons.send()}send</button>
            </span>
          </div>
          <input type="file" id="file" hidden>
        </div>
      </div>

      ${state.railOpen ? `<div class="scrim railscrim" data-act="rail"></div>` : ""}
      <div class="rail ${state.railOpen ? "open" : ""}" id="rail">${renderRail(c)}</div>
    </div>

    <div class="statusbar">
      <span class="${c.online ? "" : "off"}">${c.online ? "connected" : "reconnecting…"}</span>
      ${c.nearby.on ? `<span>nearby: ${c.nearby.peers.length}</span>` : ""}
      ${c.xfer.list.length
        ? `<button class="xlink" data-act="transfers">transfers: ${c.xfer.active().length || c.xfer.list.length}</button>`
        : ""}
      <span>${esc(c.user?.role ?? "")}</span>
      ${ch ? `<span>${esc(ch.role || "member")} in ${ch.is_dm ? esc(ch.name) : "#" + esc(ch.name)}</span>` : ""}
      <span class="spacer">theme: ${theme}</span>
    </div>
  </div>`;
}

export function renderAuth(
  mode: "login" | "register", server: string, err: string, showServer = true,
  invite = "", menuOpen = false,
  /** Signing in to a SECOND server while already signed in to one. The only
   *  difference is the wording and a way back to where you were. */
  adding = false,
): string {
  // Almost nobody types a server address: the web app is served by the server it
  // talks to, and a packaged build is pinned to one. So the field is hidden and
  // the escape hatch is in the split button's menu — or a triple-tap on the
  // logo, for whoever already knows (see main.ts).
  //
  // Hidden rather than dropped: the form submit and the device-link flow both
  // read [name="server"], so there is one code path either way.
  //
  // The menu holds the ways in that are not "sign in with what I just typed".
  // They were four lines under the form, which made one obvious choice look
  // like five. Nearby stays outside it: it is a front door (spec F9), not an
  // alternative spelling of this one.
  // type="button": these live inside the form, and a bare <button> in a form is
  // a submit button.
  const row = (act: string, icon: string, label: string) =>
    `<button type="button" data-act="${act}">${label}${icon}</button>`;
  const menu: string[] = [
    mode === "login"
      ? row("toregister", icons.invite(13), "redeem an invite")
      : row("tologin", icons.login(13), "sign in with an account"),
  ];
  if (hasDefaultServer()) {
    menu.push(showServer
      ? row("defaultserver", icons.server(13), "use the standard server")
      : row("customserver", icons.server(13), "use a custom server"));
  }
  if (adding) {
    menu.length = 0; // none of the front doors apply: there is a session already
    menu.push(row("canceladd", icons.back(13), "back to where you were"));
  }
  return `
  <div class="auth">
    <div class="panel">
      <h1 class="serif" id="logo">gliph</h1>
      <div class="sub">${adding ? "sign in to another server" : mode === "login" ? "sign in to your server" : "redeem your invite"}</div>
      <form id="authform">
        ${showServer ? `<label for="auth-server">server</label>` : ""}
        <input id="auth-server" name="server" value="${esc(server)}" placeholder="https://chat.example.com"
          ${showServer ? "" : `type="hidden"`}
          inputmode="url" autocapitalize="none" autocorrect="off" spellcheck="false">
        <label for="auth-nick">nick</label>
        <input id="auth-nick" name="nick" autocapitalize="none" autocorrect="off" spellcheck="false" autocomplete="username" required>
        ${mode === "register" ? `<label for="auth-email">email</label><input id="auth-email" name="email" type="email" autocapitalize="none" autocorrect="off" spellcheck="false" required>` : ""}
        <label for="auth-password">password</label>
        <input id="auth-password" name="password" type="password" autocomplete="current-password" required>
        ${mode === "register" ? `<label for="auth-invite">invite code <span class="sub">(from your server's admin — blank only if you are the first account)</span></label><input id="auth-invite" name="invite" value="${esc(invite)}" autocapitalize="none" autocorrect="off" spellcheck="false">` : ""}
        <div class="splitwrap">
          <div class="split">
            <button class="go" type="submit">${mode === "login" ? "connect" : "join"}</button>
            <button class="gocaret ${menuOpen ? "open" : ""}" type="button" data-act="authmenu"
              aria-haspopup="true" aria-expanded="${menuOpen}" aria-label="other ways to sign in"
              title="other ways to sign in">${menuOpen ? "&#9650;" : "&#9660;"}</button>
          </div>
          ${menuOpen ? `<div class="authmenu">${menu.join("")}</div>` : ""}
        </div>
      </form>
      ${err ? `<div class="err">${esc(err)}</div>` : ""}
      ${meshSupported()
        ? `<div class="alt">or <button data-act="nearby">chat with people nearby</button></div>`
        : ""}
      ${isTauri() ? "" : `<div class="alt quiet"><a href="${esc(downloadsURL(server))}" target="_blank" rel="noopener">get the Gliph Desktop App</a></div>`}
    </div>
  </div>`;
}

/** The transfer window, mIRC's DCC panel: one row per transfer, a bar that
 *  fills, and the two buttons that matter on an offer someone made you.
 *
 *  It is a window rather than lines in the stream because that is what a
 *  transfer is — a thing with a duration that you glance at while the
 *  conversation carries on above it. */
export function renderTransfers(list: Transfer[]): string {
  // On a send, `bytes` is what went out and `acked` is what the other side
  // confirmed. The bar shows the confirmed number: a bar that races ahead of
  // the receiver and then sits at 100% while nothing arrives is a lie.
  const done = (t: Transfer) => (t.dir === "send" ? t.acked : t.bytes);
  const pct = (t: Transfer) => t.size ? Math.min(100, Math.round((done(t) / t.size) * 100)) : 0;
  const size = (n: number) =>
    n >= 1048576 ? `${(n / 1048576).toFixed(1)} MB`
    : n >= 1024 ? `${Math.round(n / 1024)} KB` : `${n} B`;
  // No rate: the core does not keep a clock, and a number that would have to be
  // recomputed from a host timestamp on every repaint is not worth the field.
  const rate = (_t: Transfer) => "";
  const status = (t: Transfer) => {
    switch (t.state) {
      case "offered": return t.dir === "recv" ? "offers you" : "waiting for accept";
      case "running": return `${pct(t)}%${rate(t)}`;
      case "done": return t.dir === "recv" ? "received" : "sent";
      case "declined": return t.error ?? "declined";
      case "failed": return t.error ?? "failed";
    }
  };

  const row = (t: Transfer) => `
    <div class="xrow ${t.state}">
      <div class="xhead">
        <span class="xdir">${t.dir === "send" ? "&gt;&gt;" : "&lt;&lt;"}</span>
        <span class="xname">${esc(t.name)}</span>
        <span class="xpeer">${t.dir === "send" ? "to" : "from"} ${esc(t.peer_nick || "?")}</span>
        <span class="xsize">${size(t.size)}</span>
      </div>
      <div class="xbar"><div class="xfill" style="width:${pct(t)}%"></div></div>
      <div class="xfoot">
        <span>${esc(status(t))}</span>
        ${t.dir === "recv" && t.state === "offered"
          ? `<span class="xacts">
               <button data-xaccept="${esc(t.id)}">accept</button>
               <button data-xdecline="${esc(t.id)}" class="ghost">decline</button>
             </span>`
          : ""}
        ${t.state === "done"
          ? `<span class="xacts done">saved</span>`
          : ""}
      </div>
    </div>`;

  return `
  <div class="overlay" data-overlay="transfers">
    <div class="panel">
      <h2>file transfers</h2>
      <div class="xlist">
        ${list.length ? list.slice().reverse().map(row).join("")
          : `<div class="sysnotice">nothing yet. /send &lt;nick&gt; offers a file to someone nearby.</div>`}
      </div>
      <div class="foot">peer to peer over the local network · nothing is uploaded anywhere · esc to close</div>
    </div>
  </div>`;
}
