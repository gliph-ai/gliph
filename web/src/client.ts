// The web host for the Rust core.
//
// Everything the client DECIDES now lives in core/ (Rust, compiled to wasm and
// shared with the iOS app). This file is what the core cannot be: the socket,
// localStorage, the mesh transport, the HTTP calls, and the browser APIs around
// them. It performs effects and hands frames back.
//
// The class keeps the shape the UI already used — `channels`, `unread()`,
// `submit()` — so ui.ts and main.ts did not have to be rewritten alongside it.
// Underneath, every one of those is the core answering.
//
// What is still TypeScript, deliberately, because the core has no equivalent:
//   - blobs / GUM (gum.ts), a DOM renderer with no counterpart on the phone
//   - peer-to-peer DM transfers (p2p.ts), which are WebRTC and browser-only
// Both peek at frames here before the core sees them, and the core ignores the
// frame types it does not know.

import init, { GliphCore } from "./core/gliph_core.js";
import type { ChannelView, Command, Ev, Frame, RoomPerson, User } from "./protocol";
import type { Surface } from "./gum";
import { activeTransfers, handleP2PFrame, offerFile, type Progress, type TransferInfo } from "./p2p";
import { inverseOf, record as recordUndo, reset as resetUndo } from "./undo";
import { isNearby, meshSupported, meshTransport, type MeshTransport } from "./mesh";
import type { Event as NotifyEvent } from "./notify";
import { isTauri } from "./desktop";
import { ask } from "./ask";
import { applyCustom, themeEffect } from "./themes";

/** Baked in at build time by vite.config.ts (GLIPH_SERVER). "" in dev and in a
 *  self-hoster's own build. */
declare const __GLIPH_SERVER__: string;
declare const __GLIPH_VERSION__: string;
export const pinnedServer: string = __GLIPH_SERVER__;
/** What this build is, as `/status` reports it: `desktop/0.1.54+300aae9`. */
export const clientVersion: string = __GLIPH_VERSION__;
export const clientId = () => `${isTauri() ? "desktop" : "web"}/${clientVersion}`;

/** In the desktop shell the page origin is tauri://, which is not a server.
 *  Only an http(s) origin is a sensible default. In dev the origin is the Vite
 *  server, which proxies /api and /ws through to the Go server. */
const defaultServer = () =>
  pinnedServer ||
  (location.protocol === "http:" || location.protocol === "https:" ? location.origin : "");

/** Whether there is anywhere to go back TO — false only in a packaged build
 *  with no pinned server, where the address has to be typed. */
export const hasDefaultServer = () => defaultServer() !== "";

const LS_THEME = "gliph.theme";

// ---- the shapes the core hands back ----

type Effect =
  | { kind: "Send"; json: string }
  | { kind: "Persist"; key: string; value: string; store: "Plain" | "Secret" }
  | { kind: "Update" }
  | { kind: "Forget"; key: string; store: "Plain" | "Secret" }
  | { kind: "Alert"; id: string; channel: string; nick: string; text: string; is_dm: boolean; event: NotifyEvent }
  | { kind: "Changed" }
  | { kind: "MeshStart"; nick: string }
  | { kind: "MeshStop" }
  | { kind: "MeshJoin"; chans: string[] }
  | { kind: "MeshSay"; chan: string; body: string; msg_kind: string; reply_to: string }
  | { kind: "MeshSendTo"; peer: string; t: string; json: string }
  | { kind: "SaveFile"; name: string; mime: string; bytes: string }
  | { kind: "PickFile" }
  | { kind: "AskConfirm"; message: string }
  | { kind: "AskPrompt"; message: string }
  | { kind: "Compose"; title: string; lang: string; text: string; submit: string }
  | { kind: "Theme"; op: string; name: string; theme: string };

export type EventRow = Ev & { text: string | null; quote: string; highlight: boolean };

/** One signed-in server, as core/src/servers.rs persists it. */
export type ServerEntry = {
  host: string;
  token: string;
  name: string;
  /** Your nick THERE — different servers, different rows, different nicks. */
  nick: string;
  icon: string;
  /** Unread, never mentions: a sealed channel's body is ciphertext to the
   *  server, so a count is the most it can honestly report. */
  unread: number;
  checked_ms: number;
};

export type ServerList = { list: ServerEntry[]; active: string };

type Snapshot = {
  server: string;
  server_name: string;
  signed_in: boolean;
  online: boolean;
  solo: boolean;
  active: string;
  user: User | null;
  channels: (ChannelView & { unread: number })[];
  unread_total: number;
  notices: { channel: string; level: "info" | "error"; text: string }[];
  system: SysMsg[];
  transfers: Transfer[];
  nearby_on: boolean;
  nearby_peers: number;
  upload_max: number;
  command_count: number;
  plugin_command_count: number;
  active_blob: string;
  reply: string;
  /** One per channel with anybody in its 3D room: the live strip above the
   *  composer. Empty almost always. */
  rooms: RoomStrip[];
  /** This device's standing with its account (docs/protocol.md #Device trust). */
  device: DeviceTrust;
};

export type DeviceTrust = {
  known: boolean;
  signed: boolean;
  /** this device's approval code, to read off it */
  code: string;
  /** the code as QR rows ("1" dark) of gliph://approve/<code>, while unsigned */
  qr: string[];
  /** other devices of the account waiting for approval */
  pending: { name: string; code: string }[];
};

/** A channel's room, ready to draw. `since` is RFC3339 from the server: the
 *  core has no clock, so the elapsed time is formatted here. */
export type RoomStrip = {
  channel: string;
  room: string;
  since: string;
  people: RoomPerson[];
  voice_count: number;
  you_inside: boolean;
  you_quiet: boolean;
};

/** A system message: the operator's MOTD, or the notes for this release. The
 *  icon is a data URL (the operator's own) or "" for ours. */
export type SysMsg = {
  id: string;
  kind: "motd" | "news";
  icon: string;
  title: string;
  body: string;
  date: string;
};

export type Transfer = {
  id: string;
  dir: "send" | "recv";
  peer: string;
  peer_nick: string;
  chan: string;
  name: string;
  mime: string;
  size: number;
  bytes: number;
  acked: number;
  state: "offered" | "running" | "done" | "declined" | "failed";
  error?: string | null;
};

const empty: Snapshot = {
  server: "", server_name: "", signed_in: false, online: false, solo: false, active: "",
  user: null, channels: [], unread_total: 0, notices: [], system: [], transfers: [],
  nearby_on: false, nearby_peers: 0, upload_max: 1 << 30,
  command_count: 0, plugin_command_count: 0, active_blob: "", reply: "", rooms: [],
  device: { known: false, signed: false, code: "", qr: [], pending: [] },
};

/** The one secret in the product. It lives in IndexedDB rather than
 *  localStorage because a store that only script on this origin can reach is
 *  still better than one every extension content-script can read casually.
 *
 *  ponytail: this IS weaker than what the TypeScript client did, and the
 *  regression is worth naming. That version kept a NON-EXTRACTABLE CryptoKey,
 *  which script on the origin could use but never read out. The core does ECDH
 *  itself, so it needs the raw scalar, and WebCrypto cannot hand one back. The
 *  fix is a host-supplied ECDH callback so the private key never enters wasm —
 *  which means the key paths go async, and that is a change to make on purpose
 *  rather than in passing. iOS is unaffected: the Keychain stores a scalar
 *  either way. */
const idb = {
  open: (): Promise<IDBDatabase> =>
    new Promise((res, rej) => {
      const r = indexedDB.open("gliph-keys", 1);
      r.onupgradeneeded = () => r.result.createObjectStore("keys");
      r.onsuccess = () => res(r.result);
      r.onerror = () => rej(r.error);
    }),
  get: async (key: string): Promise<string> => {
    const db = await idb.open();
    return new Promise((res) => {
      const r = db.transaction("keys", "readonly").objectStore("keys").get(key);
      r.onsuccess = () => res((r.result as string) ?? "");
      r.onerror = () => res("");
    });
  },
  put: async (key: string, value: string) => {
    const db = await idb.open();
    db.transaction("keys", "readwrite").objectStore("keys").put(value, key);
  },
  del: async (key: string) => {
    const db = await idb.open();
    db.transaction("keys", "readwrite").objectStore("keys").delete(key);
  },
};

/** An Effect::Alert as it reaches the UI. `event` is the core's own reason for
 *  firing — see the effect's doc comment; the frontend never re-derives it. */
export type AlertEv = {
  id: string; channel: string; nick: string; text: string;
  isDM: boolean; event: NotifyEvent;
};

export class Client {
  private core!: GliphCore;
  private snap: Snapshot = empty;
  private rows: EventRow[] = [];
  private ws: WebSocket | null = null;
  private ticker: ReturnType<typeof setInterval> | null = null;
  private retry = 0;
  private redial: ReturnType<typeof setTimeout> | null = null;
  private lastSeen = 0;
  private dialAt = 0;
  private closed = false;
  private listeners: (() => void)[] = [];
  private alertListeners: ((a: AlertEv) => void)[] = [];
  private mesh: MeshTransport | null = null;

  /** The blob surface, as the core resolved it. Selection, menus, drafts and
   *  the keyboard model are all its; this is a picture to draw. */
  private surf: Surface | null = null;

  /** live peer-to-peer transfers (WebRTC, browser-only), for the progress line */
  transfers = new Map<string, Progress & { name: string; dir: string }>();
  /** Set by main.ts: opening the file picker is the DOM's business. */
  pickFile: (() => void) | null = null;

  static async load(): Promise<Client> {
    const c = new Client();
    await init();
    c.core = new GliphCore();
    c.core.restoreServers(localStorage.getItem("gliph.servers") ?? "");

    // An install from before the switcher is signed in to one server, recorded
    // the old way. Adopt it rather than signing everybody out on upgrade.
    const legacy = localStorage.getItem("gliph.session");
    if (!c.core.activeServer() && legacy) {
      try {
        const old = JSON.parse(legacy) as { token?: string; server?: string };
        if (old.token && old.server) c.run(c.core.addServer(old.server, old.token, ""));
      } catch { /* unreadable: treat it as a fresh install */ }
    }

    // The registry is the source of truth for where we are and what token gets
    // us in; gliph.session is downstream of it, written by set_session.
    const active = c.core.activeServer();
    const here = c.servers().list.find((e) => e.host === active);
    // Same one-time adoption for the cache: before the switcher there was one
    // unsuffixed blob, holding the unsent queue as well as scrollback. It
    // belongs to the server gliph.session named and to no other, so it moves
    // under that server's key once and is gone.
    const key = c.core.cacheKeyFor(active);
    const oldCache = localStorage.getItem("gliph.cache");
    if (oldCache !== null && localStorage.getItem(key) === null) {
      try {
        const from = (JSON.parse(legacy ?? "{}") as { server?: string }).server;
        if (from && c.core.cacheKeyFor(from) === key) localStorage.setItem(key, oldCache);
      } catch { /* no recorded server: the blob is nobody's to adopt */ }
    }
    localStorage.removeItem("gliph.cache");
    c.core.restore(
      here ? JSON.stringify({ token: here.token, server: here.host }) : (legacy ?? ""),
      localStorage.getItem(key) ?? "",
      await idb.get("gliph.device"),
    );
    if (!c.core.server()) c.core.setServer(defaultServer());
    c.refresh();
    return c;
  }

  // ---- servers (the switcher) ----

  /** Every server this account is signed in to, and which one is on screen. */
  servers(): ServerList {
    try {
      const v = JSON.parse(this.core.serversJson()) as Partial<ServerList>;
      return { list: v.list ?? [], active: v.active ?? "" };
    } catch {
      return { list: [], active: "" };
    }
  }

  /** Unread waiting on every server except this one. */
  get unreadElsewhere() { return this.core.unreadElsewhere(); }

  /** Unread on the server on screen. This one comes from the live socket, not
   *  the poll — the socket is the authority on the room you are looking at. */
  get unreadHere() { return this.snap.unread_total; }

  /** Sign in on another server without disturbing the one on screen. The
   *  reload that follows comes up on the new one, because adding it made it
   *  active.
   *
   *  Deliberately not `request()`: that attaches the CURRENT server's token,
   *  and handing acme's token to hooli would be giving one server a credential
   *  for another.
   */
  async addServerLogin(nick: string, password: string, server: string) {
    const host = this.core.normaliseServer(server);
    const res = await fetch(host + "/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ nick, password }),
    });
    const body = await res.json().catch(() => null);
    if (!res.ok || !body?.token) {
      throw new Error(body?.error ?? `could not sign in at ${host}`);
    }
    this.dispatch(this.core.addServer(host, body.token, body.user?.nick ?? ""));
  }

  /** Put another server on screen. The socket goes with it — one server, one
   *  session, one connection. */
  switchTo(host: string) {
    if (!host || host === this.core.activeServer()) return;
    if (this.redial) clearTimeout(this.redial);
    this.redial = null;
    this.ws?.close();
    this.ws = null;
    // The core cannot read storage, so the cache for where we are going has to
    // be handed in. The token is NOT: it comes from the registry, so nothing
    // can point the session at a server we are not signed in to.
    const cache = localStorage.getItem(this.core.cacheKeyFor(host)) ?? "";
    this.dispatch(this.core.switchServer(host, cache, "{}"));
    this.connect();
  }

  /** Refresh the bracket on every server we are NOT looking at.
   *
   *  ponytail: a poll, not a socket per server. A count that is a minute stale
   *  is the right accuracy for "something is waiting elsewhere", and it costs
   *  one small request per server per minute instead of N live connections.
   *  Open a socket per server when a live count — or a cross-server mention
   *  with its text — actually matters. */
  async pollServers(): Promise<void> {
    const { list, active } = this.servers();
    await Promise.all(
      list
        .filter((e) => e.host !== active && e.token)
        .map(async (e) => {
          try {
            const res = await fetch(e.host.replace(/\/$/, "") + "/api/me", {
              headers: { Authorization: `Bearer ${e.token}` },
            });
            if (res.status === 401) {
              // The token died — pruned, or an admin signed this account out.
              // Keep the entry so it stays switchable, but stop it reporting a
              // count that is no longer true.
              this.dispatch(this.core.observeServer(e.host, "", "", "", 0, Date.now()));
              return;
            }
            if (!res.ok) return;
            const m = (await res.json()) as {
              server?: string; nick?: string; icon?: string; unread?: number;
            };
            this.dispatch(this.core.observeServer(
              e.host, m.server ?? "", m.nick ?? "", m.icon ?? "", Number(m.unread ?? 0), Date.now(),
            ));
          } catch {
            // Unreachable right now: keep the last count rather than claiming
            // the server went quiet.
          }
        }),
    );
  }

  // ---- effects ----

  private run(json: string) {
    let fx: Effect[];
    try {
      fx = JSON.parse(json) as Effect[];
    } catch {
      return;
    }
    let changed = false;
    for (const e of fx) {
      switch (e.kind) {
        case "Send":
          if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(e.json);
          break;
        case "Persist":
          if (e.store === "Secret") void idb.put(e.key, e.value);
          else try { localStorage.setItem(e.key, e.value); } catch { /* quota: keep running */ }
          break;
        case "Forget":
          if (e.store === "Secret") void idb.del(e.key);
          else localStorage.removeItem(e.key);
          break;
        case "Alert":
          for (const fn of this.alertListeners) {
            fn({ id: e.id, channel: e.channel, nick: e.nick, text: e.text, isDM: e.is_dm, event: e.event });
          }
          break;
        case "Changed":
          changed = true;
          break;
        case "MeshStart":
          void this.startMesh(e.nick);
          break;
        case "MeshStop":
          void this.mesh?.stop();
          break;
        case "MeshJoin":
          void this.mesh?.join(e.chans);
          break;
        case "MeshSay":
          void this.mesh?.send(e.chan, e.body, e.msg_kind, e.reply_to)
            .then((m) => this.dispatch(this.core.meshMsg(JSON.stringify(m), Date.now())));
          break;
        case "MeshSendTo":
          void this.mesh?.sendTo(e.peer, e.t, JSON.parse(e.json));
          break;
        case "SaveFile":
          this.save(e.name, e.mime, e.bytes);
          break;
        case "PickFile":
          this.pickFile?.();
          break;
        // The core cannot draw a dialog, so it asks. Answering is what fires
        // the action — a cancelled confirm must never become a delete.
        case "AskConfirm":
          void ask(e.message, "").then((ok) => this.dispatch(this.core.gumAnswer(ok, "")));
          break;
        case "AskPrompt": {
          const got = prompt(e.message);
          this.dispatch(this.core.gumAnswer(got !== null, got ?? ""));
          break;
        }
        // /update. The updater lives in the desktop shell, and main.ts owns
        // the button and the progress, so this only forwards.
        case "Update":
          this.onUpdate?.();
          break;
        // A theme file or a script: the composer is one line, so the host
        // opens its editor and submits the result back to the core.
        case "Compose":
          this.onCompose?.(e.title, e.lang, e.text, e.submit);
          break;
        case "Theme": {
          const r = themeEffect(e.op, e.name, e.theme);
          if (r.base) this.setBase(r.base);
          this.core.notice(r.say, r.level);
          changed = true;
          break;
        }
      }
    }
    if (changed) this.refresh();
  }

  /** Effects, then a repaint. Every core call goes through here. */
  private dispatch(json: string) {
    this.run(json);
  }

  private refresh() {
    try {
      this.snap = JSON.parse(this.core.snapshot()) as Snapshot;
      this.rows = JSON.parse(this.core.events(this.snap.active)) as EventRow[];
      this.surf = JSON.parse(this.core.surface()) as Surface;
    } catch {
      /* a snapshot that will not parse is a bug in the core, not a reason to
         blank the screen */
    }
    for (const fn of this.listeners) fn();
  }

  private save(name: string, mime: string, b64: string) {
    const bin = atob(b64);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    const blob = new Blob([bytes], { type: mime });
    this.keepP2P(name, bytes.length, blob);
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = name;
    a.click();
  }

  private async startMesh(nick: string) {
    if (!meshSupported()) return;
    if (!this.mesh) {
      this.mesh = await meshTransport();
      await this.mesh.listen(
        (peers) => this.dispatch(this.core.meshPeers(JSON.stringify(peers))),
        (m) => this.dispatch(this.core.meshMsg(JSON.stringify(m), Date.now())),
        (f) => this.dispatch(this.core.meshFrame(JSON.stringify(f), Date.now())),
      );
    }
    await this.mesh.start(nick);
  }

  onChange(fn: () => void) { this.listeners.push(fn); }
  onAlert(fn: (a: AlertEv) => void) {
    this.alertListeners.push(fn);
  }

  // ---- what the UI reads ----

  get server() { return this.snap.server; }
  get token() { return this.core.token(); }
  get user() { return this.snap.user; }
  get serverName() { return this.snap.server_name; }
  get uploadMax() { return this.snap.upload_max; }
  get commandCount() { return this.snap.command_count; }
  get pluginCommandCount() { return this.snap.plugin_command_count; }
  get channels(): ChannelView[] { return this.snap.channels; }
  /** The live room strips, one per channel with anybody inside. */
  get rooms(): RoomStrip[] { return this.snap.rooms; }
  get device(): DeviceTrust { return this.snap.device ?? empty.device; }
  get active() { return this.snap.active; }
  set active(cid: string) { this.dispatch(this.core.setActive(cid)); }
  get online() { return this.snap.online; }
  get solo() { return this.snap.solo; }
  get nearby() { return { on: this.snap.nearby_on, peers: new Array(this.snap.nearby_peers) }; }
  get xfer() {
    const list = this.snap.transfers;
    return { list, active: () => list.filter((t) => t.state === "offered" || t.state === "running") };
  }

  channel(cid = this.active): ChannelView | undefined {
    return this.snap.channels.find((c) => c.id === cid);
  }
  unread(cid: string): number {
    return this.snap.channels.find((c) => c.id === cid)?.unread ?? 0;
  }
  unreadTotal(): number { return this.snap.unread_total; }
  visibleEvents(cid = this.active): EventRow[] {
    return cid === this.snap.active ? this.rows : (JSON.parse(this.core.events(cid)) as EventRow[]);
  }
  channelNotices() { return this.snap.notices; }
  /** The MOTD and any unread release notes. Empty almost always. */
  systemMessages(): SysMsg[] { return this.snap.system; }
  dismissSystem(id: string) { this.dispatch(this.core.dismissSystem(id)); }
  availableCommands(): Command[] { return JSON.parse(this.core.palette("/")) as Command[]; }
  matchCommands(text: string): Command[] { return JSON.parse(this.core.palette(text)) as Command[]; }
  markRead() { this.dispatch(this.core.markActiveRead()); }

  /** What the next message answers, as an event id; "" cancels. The quote, and
   *  when it clears, are the core's — see core/src/wire.rs. */
  setReply(eventId: string) { this.dispatch(this.core.setReply(eventId)); }
  /** `<alice> what she said` while a reply is pending, "" otherwise. */
  get replyTo() { return this.snap.reply; }

  // ---- actions ----

  submit(text: string) { this.dispatch(this.core.submit(text)); }
  sendMessage(body: string) { this.dispatch(this.core.sendMessage(body)); }

  /** A line said inside the 3D room: the same message on the same wire,
   *  marked with the sub-room it came from. */
  sendRoomMessage(body: string, room: string) { this.dispatch(this.core.sendRoomMessage(body, room)); }
  /** Set by main.ts: `/update` asked this client to update itself. */
  onUpdate?: () => void;
  onCompose?: (title: string, lang: string, text: string, submit: string) => void;
  /** Set by main.ts: the socket (re)connected. A deploy restarts the server,
   *  so this is the moment a new build is most likely to exist. */
  onConnected?: () => void;

  notice(text: string, level: "info" | "error" = "info") { this.dispatch(this.core.notice(text, level)); }

  /** Files that moved peer to peer this session, by `name|size`. The bytes
   *  never touched the server, so a preview in the stream has nowhere else to
   *  come from — the copy in hand is the only copy there is.
   *  ponytail: no eviction. It is the handful of files you moved by hand. */
  private p2pFiles = new Map<string, string>();
  private keepP2P(name: string, size: number, blob: Blob) {
    this.p2pFiles.set(`${name}|${size}`, URL.createObjectURL(blob));
  }
  p2pURL(name: string, size: number) { return this.p2pFiles.get(`${name}|${size}`); }
  acceptTransfer(id: string) { this.dispatch(this.core.acceptTransfer(id)); }
  declineTransfer(id: string) { this.dispatch(this.core.declineTransfer(id)); }

  /** THE dispatcher for op actions. Undo is recorded here and nowhere else —
   *  recording per call site works until one path forgets, and then a
   *  destructive action is quietly not undoable. */
  runCommand(name: string, args: string, opts: { record?: boolean } = {}) {
    if (opts.record !== false) {
      const entry = inverseOf(name, args, { topic: this.channel()?.topic });
      if (entry) recordUndo(entry);
    }
    this.dispatch(this.core.submit(`/${name} ${args}`.trimEnd()));
  }

  async startSolo(nick: string, chan = "nearby") {
    if (!meshSupported()) throw new Error("nearby mode needs the desktop app");
    this.dispatch(this.core.startSolo(nick, chan));
  }

  // ---- auth (HTTP: the core owns the session, not the request) ----

  api(path: string) { return this.server.replace(/\/$/, "") + path; }

  async request(path: string, opts: RequestInit = {}) {
    const res = await fetch(this.api(path), {
      ...opts,
      headers: {
        "Content-Type": "application/json",
        ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
        ...(opts.headers ?? {}),
      },
    });
    const body = await res.json().catch(() => null);
    if (!res.ok || body === null) {
      // A 404 or a non-JSON reply almost always means the address is not a
      // Gliph server (a dev server, a proxy, or a typo) rather than a bad login.
      if (body?.error) throw new Error(body.error);
      if (res.status === 404 || body === null) {
        throw new Error(`no gliph server at ${this.server} (got ${res.status})`);
      }
      throw new Error(`request failed (${res.status})`);
    }
    return body;
  }

  async login(nick: string, password: string, server: string) {
    this.core.setServer(server);
    const r = await this.request("/api/login", {
      method: "POST",
      body: JSON.stringify({ nick, password }),
    });
    this.dispatch(this.core.setSession(server, r.token, JSON.stringify(r.user)));
    this.dispatch(this.core.addServer(server, r.token, r.user?.nick ?? ""));
  }

  async register(nick: string, email: string, password: string, invite: string, server: string) {
    this.core.setServer(server);
    const r = await this.request("/api/register", {
      method: "POST",
      body: JSON.stringify({ nick, email, password, invite }),
    });
    this.dispatch(this.core.setSession(server, r.token, JSON.stringify(r.user)));
    this.dispatch(this.core.addServer(server, r.token, r.user?.nick ?? ""));
  }

  /** Take up a session minted elsewhere — today, by the device-link flow. */
  adopt(token: string, server: string) {
    this.dispatch(this.core.setSession(server, token, "{}"));
    this.dispatch(this.core.addServer(server, token, ""));
  }

  /** Forget a hand-typed server and go back to the pinned one. */
  useDefaultServer() {
    this.core.setServer(defaultServer());
    this.refresh();
  }

  logout() {
    resetUndo(); // one account's history is not the next one's
    const leaving = this.core.activeServer();
    this.closed = true;
    if (this.ticker) clearInterval(this.ticker);
    this.request("/api/logout", { method: "POST" }).catch(() => {});
    this.dispatch(this.core.logout());
    // Signing out of one server is not signing out of the product: the entry
    // goes, the rest of the list stays, and the reload lands on whichever is
    // active afterwards.
    this.dispatch(this.core.forgetServer(leaving));
    this.ws?.close();
    location.reload();
  }

  // ---- socket ----

  connect() {
    if (!this.token || this.closed) return;
    if (this.redial) clearTimeout(this.redial);
    this.redial = null;
    if (this.ws && this.ws.readyState <= WebSocket.OPEN) return; // already dialling
    // The core's only clock. Housekeeping — transfer timeouts, ageing out the
    // netsplit lines — happens here, mirroring the mobile store's ticker.
    this.ticker ??= setInterval(() => {
      this.dispatch(this.core.tick(Date.now()));
      this.heartbeat();
    }, 5_000);
    const url = this.api("/ws").replace(/^http/, "ws") + `?token=${encodeURIComponent(this.token)}&client=${encodeURIComponent(clientId())}`;
    const ws = new WebSocket(url);
    this.ws = ws;
    this.dialAt = Date.now();

    ws.onopen = () => {
      this.retry = 0;
      this.lastSeen = Date.now();
      this.onConnected?.();
      this.dispatch(this.core.connected(navigator.platform || "web", Date.now()));
      // Catch-up arrives in the next few frames; go live once it has landed.
      setTimeout(() => this.core.goLive(), 2500);
      // The ICE list is warmed here: a transfer offer can arrive at any moment
      // and the receiving path builds its peer connection synchronously.
      void this.iceServers();
    };
    ws.onmessage = (m) => {
      this.lastSeen = Date.now();
      const raw = m.data as string;
      // Blobs and WebRTC are still TypeScript, so they read the frame first.
      // The core ignores what it does not recognise, so both see everything.
      this.peek(raw);
      this.dispatch(this.core.handleFrame(raw, Date.now()));
    };
    ws.onclose = () => {
      // A socket we already let go of (a server switch, a sign-out) closes
      // late; it must not mark the new session offline or redial.
      if (this.ws !== ws) return;
      this.ws = null;
      this.dispatch(this.core.disconnected(Date.now()));
      this.retry = Math.min(this.retry + 1, 4); // 1s … 8s; focus/online redial at once
      this.redial = setTimeout(() => this.connect(), 500 * 2 ** this.retry);
    };
    ws.onerror = () => ws.close();
  }

  /** A socket that went quiet is dead — a sleeping laptop or a NAT that
   *  forgot us leaves it OPEN with nobody home. Ping every tick; three
   *  unanswered and it is closed, which is what redials it. */
  private heartbeat() {
    const ws = this.ws;
    if (!ws) return;
    if (ws.readyState === WebSocket.CONNECTING) {
      // A dial that never answers is the other way to be dead. WebKit leaves a
      // socket opened during sleep in CONNECTING for as long as it likes, with
      // no close and no error, and connect() takes "still dialling" at its
      // word — so the desktop app sat on "reconnecting…" with nothing
      // reconnecting. Closing a CONNECTING socket fires onclose, which redials.
      if (Date.now() - this.dialAt > 10_000) ws.close();
      return;
    }
    if (ws.readyState !== WebSocket.OPEN) return;
    if (Date.now() - this.lastSeen > 15_000) ws.close();
    else ws.send('{"t":"ping"}');
  }

  /** Woken by a push, or returning to the foreground. */
  wake() {
    if (this.ws?.readyState === WebSocket.OPEN) this.dispatch(this.core.wake());
    else {
      this.retry = 0;
      // A dial older than a moment is one that started before the network
      // came back; drop it rather than wait out its deadline.
      if (this.ws?.readyState === WebSocket.CONNECTING && Date.now() - this.dialAt > 3_000) this.ws.close();
      this.connect();
    }
  }

  /** Frame types the core does not model, handled here. */
  private peek(raw: string) {
    let f: Frame;
    try {
      f = JSON.parse(raw) as Frame;
    } catch {
      return;
    }
    switch (f.t) {
      case "p2p.begin":
      case "p2p.accept":
      case "p2p.reject":
      case "p2p.offer":
      case "p2p.answer":
      case "p2p.ice":
      case "p2p.cancel":
        handleP2PFrame(f, this.p2pCtx());
        break;
      // /room0 (or /3d) answered: open the 3D room for that channel. The whole
      // module — three.js included — arrives on this first use, like Monaco.
      case "room.open":
        void import("./room/room.ts").then((m) => m.openRoom(this.roomCtx(), String(f.d?.channel ?? "")));
        break;
    }
    // The open room sees every frame (room.event relays, hello for re-join).
    this.roomFrames?.(f);
  }

  private send(f: Frame) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(f));
  }

  // ---- the 3D room (room.ts, lazy; browser and desktop only) ----

  /** Set by room.ts while a room is open; sees every incoming frame. */
  private roomFrames: ((f: Frame) => void) | null = null;

  roomCtx() {
    return {
      send: (f: Frame) => this.send(f),
      api: (p: string) => this.api(p),
      token: () => this.token,
      user: () => this.user,
      members: (cid: string) => this.channel(cid)?.members ?? [],
      ice: () => this.iceServers(),
      setFrames: (fn: ((f: Frame) => void) | null) => { this.roomFrames = fn; },
      // The room says what it has to say through the core, like every other
      // surface, and reads the channel's tail for the log it opens with.
      say: (body: string, room: string) => this.sendRoomMessage(body, room),
      log: (cid: string, limit: number) => JSON.parse(this.core.roomLog(cid, limit)) as EventRow[],
      quiet: () => this.snap.rooms.find((r) => r.channel === this.active)?.you_quiet ?? false,
      run: (name: string, args: string) => this.runCommand(name, args),
      // Anything the core changed: new messages (decrypted, quiet lines and
      // all), a quiet toggle, a fresh occupancy snapshot. The room re-reads
      // rather than parsing frames itself, which is what keeps it out of the
      // decryption business.
      onChange: (fn: () => void) => this.onChange(fn),
      channelName: (cid: string) => this.channels.find((c) => c.id === cid)?.name ?? "",
    };
  }

  // ---- peer-to-peer transfers (WebRTC, browser and desktop only) ----

  private ice: RTCIceServer[] | null = null;

  private async iceServers(): Promise<RTCIceServer[] | undefined> {
    if (this.ice) return this.ice.length ? this.ice : undefined;
    try {
      const r = await this.request("/api/ice");
      this.ice = (r.ice_servers ?? []) as RTCIceServer[];
    } catch {
      this.ice = []; // an older server has no /api/ice; that is not an error
    }
    return this.ice.length ? this.ice : undefined;
  }

  private p2pCtx() {
    return {
      send: (f: Frame) => this.send(f),
      iceServers: this.ice?.length ? this.ice : undefined,
      confirm: (t: TransferInfo) =>
        ask(`${t.peer} wants to send you a file`, `"${t.name}" (${t.size} bytes), directly over the local network.`, "accept"),
      onProgress: (p: Progress) => {
        const t = activeTransfers().find((x) => x.id === p.id);
        this.transfers.set(p.id, { ...p, name: t?.name ?? "file", dir: t?.dir ?? "recv" });
        this.refresh();
      },
      onDone: (t: TransferInfo, blob: Blob) => {
        this.transfers.delete(t.id);
        this.keepP2P(t.name, t.size, blob);
        const a = document.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = t.name;
        a.click();
        // The line in the channel is the record — same shape as a nearby
        // transfer, and it previews an image the way an upload does.
        this.dispatch(this.core.p2pFileLine(t.channel, t.id, t.name, t.size, false, t.peer));
      },
      onError: (t: TransferInfo, message: string) => {
        this.transfers.delete(t.id);
        this.notice(`${t.name}: ${message}`, "error");
      },
    };
  }

  /** Send a file. Nearby goes peer to peer over the mesh link (the core drives
   *  it); a DM goes over WebRTC; anything else goes through the server. */
  async sendFile(file: File) {
    if (isNearby(this.active)) {
      const bytes = new Uint8Array(await file.arrayBuffer());
      this.keepP2P(file.name, file.size, file);
      this.dispatch(this.core.offerFile(file.name, file.type || "application/octet-stream", bytes));
      return;
    }
    const ch = this.channel();
    if (ch?.is_dm) {
      this.notice(`offering ${file.name} directly to ${ch.name}…`);
      try {
        const id = await offerFile((f) => this.send(f), ch.id, file, this.p2pCtx().onProgress, await this.iceServers());
        this.keepP2P(file.name, file.size, file);
        this.dispatch(this.core.p2pFileLine(ch.id, id, file.name, file.size, true, ch.name));
      } catch (err) {
        this.notice(err instanceof Error ? err.message : "transfer failed", "error");
      }
      return;
    }
    await this.upload(file, (pct) => {
      if (pct % 25 === 0) this.notice(`${file.name}: ${pct}%`);
    });
  }

  /** Resumable upload. Chunking is dumb request/response, so it stays here
   *  rather than crossing the FFI boundary twice per chunk. */
  async upload(file: File, onProgress: (pct: number) => void) {
    if (file.size > this.uploadMax) throw new Error("file is larger than this server allows");
    const created = await this.request("/api/upload", {
      method: "POST",
      body: JSON.stringify({ channel: this.active, name: file.name, size: file.size }),
    });
    const id: string = created.id;
    let offset = 0;
    const CHUNK = 4 << 20;
    while (offset < file.size) {
      const slice = file.slice(offset, Math.min(offset + CHUNK, file.size));
      const res = await fetch(this.api(`/api/upload/${id}`), {
        method: "PATCH",
        headers: { Authorization: `Bearer ${this.token}`, "Upload-Offset": String(offset) },
        body: slice,
      });
      if (res.status === 409) { // resume from where the server actually is
        offset = Number(res.headers.get("Upload-Offset") ?? 0);
        continue;
      }
      if (!res.ok) throw new Error("upload failed");
      offset = (await res.json()).offset;
      onProgress(Math.round((offset / file.size) * 100));
    }
    return id;
  }

  fileURL(id: string) { return this.api(`/api/file/${id}?token=${encodeURIComponent(this.token)}`); }

  // ---- blobs (GUM) ----
  //
  // Every one of these is a forward. The document, the focus order, the
  // selection, the menu and the drafts live in core/src/gum.rs — see the tests
  // there for what each key is promised to do.

  get activeBlob() { return this.snap.active_blob; }
  surface(): Surface | null { return this.surf; }
  blobs() { return this.surf?.blobs ?? []; }

  openBlob(id: string) { this.dispatch(this.core.openBlob(id)); }
  /** ctrl+1..9: the nth entry in the switcher, chat being 1. */
  blobSwitch(n: number) { this.dispatch(this.core.blobSwitch(n)); }
  gumMove(delta: number) { this.dispatch(this.core.gumMove(delta)); }
  gumJump(n: number) { this.dispatch(this.core.gumJump(n)); }
  gumActivate() { this.dispatch(this.core.gumActivate()); }
  gumEscape() { this.dispatch(this.core.gumEscape()); }
  gumTap(index: number) { this.dispatch(this.core.gumTap(index)); }
  gumTapMenu(id: string) { this.dispatch(this.core.gumTapMenu(id)); }
  gumSetDraft(text: string) {
    this.dispatch(this.core.gumSetDraft(this.core.gumFocusedInput(), text));
  }

  // ---- theme (purely a browser preference; the core has no opinion) ----

  setTheme(t: "creme" | "midnight") {
    applyCustom(null);
    this.setBase(t);
  }

  /** The base theme under a custom one, or the whole theme without one. */
  private setBase(t: "creme" | "midnight") {
    document.documentElement.dataset.theme = t;
    localStorage.setItem(LS_THEME, t);
    this.refresh();
  }

  theme(): "creme" | "midnight" {
    return (localStorage.getItem(LS_THEME) as "creme" | "midnight") ?? "creme";
  }
}

/** What room.ts gets from the client: a send seam, auth, and the roster. */
export type RoomCtx = ReturnType<Client["roomCtx"]>;
