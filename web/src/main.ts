import "./theme.css";
import { Client, clientVersion } from "./client";
import type { Command } from "./protocol";
import { render, renderAuth, renderTransfers, toggleSystem, growStream } from "./ui";
import { checkForUpdate, downloadsURL, initDesktop, installUpdate, isTauri, log, onUpdateAvailable, openExternal, startDrag } from "./desktop";
import { overlayOpen, renderHelp, resolve, typing } from "./shortcuts";
import {
  alert as showAlert, dismissAll, dismissChannel, mode as notifyMode, onStackChange,
  requestPermission, retheme, setMode, shouldAlert, unlockAudio, visible,
} from "./notify";
import { renderStack } from "./alertcard";
import { loadCard, openLightbox, reapBlobs, setUnfurlMode, unfurlMode } from "./cards";
import { currentToast, dismissToast, onUndoChange, takeRedo, takeUndo } from "./undo";
import { renderUndoToast } from "./ui";
import { nextPath } from "./redirect";
import { markSignedIn } from "./session";
import { openServerMenu } from "./servers";
import { codepadOpen, colorize, openCodepad } from "./codepad";
import { activeCustom, applyCustom } from "./themes";
import { openMenu } from "./menu";

const app = document.getElementById("app")!;
// The client core is wasm, so it has to be fetched and instantiated before
// anything can render. Top-level await: the bundle targets es2022 and this is
// the one thing the whole page depends on.
const client = await Client.load();

/** An invite link is /?invite=<token> — the admin console hands one out and the
 *  form opens on the redeem side with the code already in it. */
const invited = new URLSearchParams(location.search).get("invite") ?? "";
/** /?next=/admin/users — the admin console has no login form of its own and
 *  sends people here to use this one. Sanitised, because the address arrives in
 *  a URL anyone can write; see nextPath. Honoured the moment there is a session,
 *  which covers both "sign in and come back" and "you were already signed in,
 *  the console just could not tell yet". */
const next = nextPath(new URLSearchParams(location.search).get("next"));
const goNext = () => { if (next) location.replace(next); return !!next; };
let authMode: "login" | "register" = invited ? "register" : "login";
let authErr = "";
/** A pinned build hides the server field. Triple-tapping the logo shows it, so
 *  a tester can point the shipped app at a different server without a rebuild. */
let serverShown = false;
/** The split button's menu. Closed on every repaint that is not its own toggle,
 *  because a menu that survives the thing it was opened over is a ghost. */
let authMenu = false;
let logoTaps = 0;
let logoTimer = 0;
let palette: Command[] = [];
let sel = 0;
let railOpen = false;
/** The channel whose room strip was waved away. Live state, so it comes back
 *  the next time the room does — hiding is for this reading, not forever. */
let hiddenStrip = "";
let draft = "";
let helpOpen = false;
/** The transfer window. Opens itself when someone offers you a file — that is
 *  the moment you have a decision to make. */
let xferOpen = false;

/** A newer desktop build is published. Set by the Rust updater's poll; the
 *  menubar grows a ★ update button until it is taken. `progress` is the
 *  download fraction once it is. */
let update: { version: string; progress: number | null; busy: boolean } | null = null;

/** The version the updater last found, remembered across launches so the ★
 *  is there the moment the window opens rather than after the next check. */
const LS_UPDATE = "gliph.update";
const semver = (v: string) => v.split("+")[0]!.split(".").map(Number);
const newer = (a: string, b: string) => {
  const [x, y] = [semver(a), semver(b)];
  for (let i = 0; i < 3; i++) if ((x[i] ?? 0) !== (y[i] ?? 0)) return (x[i] ?? 0) > (y[i] ?? 0);
  return false;
};
function foundUpdate(version: string) {
  if (!newer(version, clientVersion)) { try { localStorage.removeItem(LS_UPDATE); } catch { /* fine */ } return; }
  try { localStorage.setItem(LS_UPDATE, version); } catch { /* fine */ }
  if (update?.busy || update?.version === version) return;
  update = { version, progress: null, busy: false };
  paint();
}

document.documentElement.dataset.theme = client.theme();
applyCustom(activeCustom());
// /theme add and /script add with no body: the core asks for an editor.
client.onCompose = (title, lang, text, submit) => {
  void openCodepad((_, code) => { client.submit(submit + "\n" + code); paint(); }, { value: text, lang, verb: "save" });
  client.notice(title, "info");
};
initDesktop();
if (isTauri()) {
  const remembered = localStorage.getItem(LS_UPDATE);
  if (remembered) foundUpdate(remembered);
}
onUpdateAvailable(foundUpdate);
// A deploy restarts the server, so a reconnect is when a new build most likely
// exists: ask right then rather than waiting out the poll.
client.onConnected = () => {
  if (isTauri()) checkForUpdate().then((v) => { if (v) foundUpdate(v); }).catch(() => {});
};

/** Take the update that is waiting: download with the menubar counting the
 *  bytes, install, relaunch. Shared by the ★ button and `/update`. */
function runUpdate() {
  if (!update || update.busy) return;
  update.busy = true; paint();
  installUpdate((fraction) => { if (update) { update.progress = fraction; paint(); } })
    .catch((err) => {
      client.notice(`update failed: ${err instanceof Error ? err.message : String(err)}`, "error");
      if (update) { update.busy = false; update.progress = null; paint(); }
    });
}

/** A check is in flight until this moment. `/update` twice in a row otherwise
 *  starts a second check and prints the whole conversation twice.
 *  ponytail: a deadline rather than a boolean, so a check that never settles —
 *  an updater with no network to answer it — frees itself instead of wedging
 *  `/update` for the rest of the session. */
let checkingUntil = 0;

/** `/update`: look now rather than waiting for the poll, then take it. A
 *  browser tab or a phone has no updater and hears that instead. */
client.onUpdate = () => {
  if (!isTauri()) return client.notice("updates are a desktop app thing; this client is already the latest");
  if (update) return runUpdate();
  if (Date.now() < checkingUntil) return;
  checkingUntil = Date.now() + 60_000;
  client.notice("checking for a new build…");
  checkForUpdate()
    .then((version) => {
      if (!version) return client.notice(`you are on the latest build (${clientVersion})`);
      foundUpdate(version);
      client.notice(`update → ${version}: downloading`);
      runUpdate();
    })
    .catch((err) => client.notice(`update check failed: ${err instanceof Error ? err.message : String(err)}`, "error"))
    .finally(() => { checkingUntil = 0; });
};

/** Adding a server reuses the sign-in screen. The session on screen is left
 *  alone until the new sign-in succeeds — cancelling puts you back where you
 *  were with nothing changed. */
let addingServer = false;

function startAddServer() {
  addingServer = true;
  authMode = "login";
  authErr = "";
  serverShown = true;
  paint();
}

export function cancelAddServer() {
  addingServer = false;
  paint();
}

/** The last value handed to markSignedIn, so paint writes the cookie only on a change. */
let markedIn: boolean | null = null;

function paint() {
  if (markedIn !== !!client.token) markSignedIn((markedIn = !!client.token));
  if (addingServer || (!client.token && !client.solo)) {
    // Shown when asked for, and whenever there is no address to fall back on —
    // a packaged build with no pin opens on a tauri:// origin, which is not a
    // server, so hiding the field there would leave nowhere to sign in.
    const form = app.querySelector<HTMLFormElement>("#authform");
    const kept = form ? new FormData(form) : null;
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- renderAuth() escapes server, error and invite
    app.innerHTML = renderAuth(
      authMode,
      addingServer ? "" : client.server,
      authErr,
      addingServer || serverShown || !client.server,
      invited,
      authMenu,
      addingServer,
    );
    // Opening the menu or switching mode rebuilds this form under someone's
    // hands. Put back what they had typed — but only into the fields the render
    // left empty, so "use the standard server" still wins over the old address.
    kept?.forEach((v, k) => {
      const el = app.querySelector<HTMLInputElement>(`#authform [name="${k}"]`);
      if (el && !el.value) el.value = String(v);
    });
    wireAuth();
    return;
  }
  // There is a session now, so a pending return address is due. Not on solo:
  // nearby mode has no server account, and /admin needs one.
  if (client.token && goNext()) return;
  // Where the reader was. A repaint builds a fresh DOM, which starts at the
  // top — so anyone scrolled up into history got thrown back to the first
  // line of it by the next keystroke.
  const wasAt = document.getElementById("stream")?.scrollTop ?? 0;

  // Mark the open channel read BEFORE rendering, so its own badge is already
  // gone by the time the rail is drawn. Only when the window actually has
  // focus: a message that arrives while gliph is buried behind something else
  // has not been read, whatever the DOM says is on screen.
  if (document.hasFocus() && !helpOpen) client.markRead();
  const total = client.unreadTotal();
  document.title = total ? `(${total}) gliph` : "gliph";

  // A blob's input repaints on every keystroke and click (the draft lives in
  // the core), and a rebuilt DOM has no focus and no caret. Put both back.
  const gi = document.activeElement instanceof HTMLInputElement && document.activeElement.id === "gum-input"
    ? { s: document.activeElement.selectionStart, e: document.activeElement.selectionEnd } : null;

  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- ui.ts render: every wire value esc()'d, bodies through renderInline() (npm run check)
  app.innerHTML =
    render(client, { palette, sel, railOpen, notifyMode: notifyMode(), update, hiddenStrip }) +
    renderStack(visible()) +
    renderUndoToast(currentToast()) +
    (xferOpen ? renderTransfers(client.xfer.list) : "") +
    (helpOpen ? renderHelp() : "");
  wireApp();
  hydrateCode();
  if (gi) {
    const el = document.getElementById("gum-input") as HTMLInputElement | null;
    if (el) { el.focus(); el.setSelectionRange(gi.s, gi.e); }
  }
  if (pendingDm) {
    const dm = client.channels.find((c) => c.is_dm && c.name === pendingDm);
    if (dm) { pendingDm = ""; return openChannel(dm.id); }
  }

  const input = document.getElementById("input") as HTMLInputElement | null;
  if (input) {
    input.value = draft;
    // keep focus on desktop; on touch, focusing steals the keyboard — and never
    // out from under the code editor, nor the room0 overlay (a full-screen game
    // that owns WASD + its own chat input; stealing focus here every repaint
    // kills its keyboard). Checked via the DOM so room0 stays lazily loaded.
    if (!matchMedia("(max-width: 760px)").matches && !codepadOpen()
      && !document.querySelector("[data-overlay-open]")) input.focus();
  }
  const s = document.getElementById("stream");
  if (s) s.scrollTop = pinBottom ? s.scrollHeight : wasAt;
}

/** True while the reader is parked at the newest line. Set by their own
 *  scrolling and nothing else: deriving it from the DOM each repaint read a
 *  not-yet-laid-out window at startup (scrollTop 0, zero height) as "scrolled
 *  to the top", and the desktop app opened every launch on the first line of
 *  history. It also survives an image that finishes decoding after the
 *  repaint and quietly un-parks the reader. */
let pinBottom = true;

let lastXfers = 0;
client.onChange(() => {
  // A new transfer is a thing to look at: the window comes to you rather than
  // hiding behind a command nobody remembers.
  if (client.xfer.list.length > lastXfers) xferOpen = true;
  lastXfers = client.xfer.list.length;
  paint();
});

// An arrival the client judged worth announcing. It decided WHETHER; here we
// decide whether the user can already see it, and hand the rest to notify.ts.
client.onAlert((a) => {
  const channelActive = a.channel === client.active && !client.activeBlob;
  const focused = document.hasFocus();
  // Every alert leaves a line in the desktop log: "it never notified" is
  // otherwise undebuggable in a shipped app.
  log("info", `alert ${a.event} from ${a.nick} in ${a.channel}: active=${channelActive} focused=${focused}`);
  if (!shouldAlert({ mine: false, channelActive, focused })) return;
  const ch = client.channel(a.channel);
  showAlert({
    id: a.id,
    channel: a.channel,
    event: a.event,
    nick: a.nick,
    // Empty for a DM: "ada in #ada" is nonsense, the nick alone is the title.
    where: a.isDM ? "" : `#${ch?.name ?? ""}`,
    text: a.text,
    at: Date.now(),
  }, openChannel);
});

onStackChange(paint);
onUndoChange(paint);

// A click on a card in the desktop notification window. That window is its own
// process-level surface with no idea what a channel is — it says which one was
// clicked and this window does the rest, including coming to the front, which
// is the one moment a notification IS allowed to steal focus.
if (isTauri()) {
  void (async () => {
    const [{ listen }, { getCurrentWindow }] = await Promise.all([
      import("@tauri-apps/api/event"),
      import("@tauri-apps/api/window"),
    ]);
    await listen<{ channel: string }>("notify-open", async (e) => {
      const w = getCurrentWindow();
      try { await w.unminimize(); } catch { /* not minimised */ }
      await w.show();
      await w.setFocus();
      openChannel(e.payload.channel);
    });
  })();
}
// Asking for OS permission at load gets it denied before the user knows what
// it is for. Ask on the first real click instead.
document.addEventListener("click", () => { unlockAudio(); void requestPermission(); }, { once: true });
// Coming back to the window is a read event for whatever is on screen.
window.addEventListener("focus", () => { if (client.token || client.solo) paint(); });
// Coming back — to the tab, from sleep, onto Wi-Fi — redials at once instead of
// waiting out the backoff.
for (const ev of ["focus", "online", "pageshow"]) window.addEventListener(ev, () => { if (client.token) client.wake(); });
document.addEventListener("visibilitychange", () => { if (!document.hidden && client.token) client.wake(); });

const sendFile = async (file: File) => {
  try {
    // In a DM this goes peer to peer and never touches the server; in a
    // channel it uses the resumable upload.
    await client.sendFile(file);
  } catch (err) {
    client.notice(err instanceof Error ? err.message : "transfer failed", "error");
  }
};
// A file dropped on the window is a file to send. Without these the browser
// takes the drop itself and navigates away — the app is gone and the image is
// the page. Both halves matter: the dragover preventDefault is what makes the
// window a drop target at all.
document.addEventListener("dragover", (e) => e.preventDefault());
document.addEventListener("drop", (e) => {
  e.preventDefault();
  const file = e.dataTransfer?.files[0];
  if (!file || !(client.token || client.solo)) return;
  // A theme or a script file installs instead of sending. The core checks it
  // and says what is wrong with it if anything is.
  const ext = file.name.toLowerCase().match(/\.(gliph-theme|theme|rhai)$/)?.[1];
  if (ext && file.size < 64 * 1024) {
    const stem = file.name.replace(/\.[^.]+$/, "").replace(/[^A-Za-z0-9_-]/g, "-");
    void file.text().then((t) => { client.submit((ext === "rhai" ? `/script add ${stem}` : "/theme add") + "\n" + t); paint(); });
    return;
  }
  void sendFile(file);
});

// ---- auth wiring ----

function wireAuth() {
  app.querySelector<HTMLElement>("#logo")?.addEventListener("click", () => {
    clearTimeout(logoTimer);
    logoTimer = setTimeout(() => { logoTaps = 0; }, 1500);
    if (++logoTaps < 3 || serverShown) return;
    serverShown = true;
    paint();
  });

  app.querySelector<HTMLButtonElement>('[data-act="authmenu"]')
    ?.addEventListener("click", () => { if (authMenu) closeAuthMenu(); else { authMenu = true; paint(); } });

  // Anywhere else closes it. Picking an item closes it too — every handler
  // below repaints, and a repaint is what clears the flag.
  if (authMenu) {
    app.querySelector<HTMLElement>(".auth")?.addEventListener("mousedown", (e) => {
      // The menu itself is not "outside": closing on mousedown would destroy
      // the item before its own click could fire.
      if (!(e.target as HTMLElement).closest(".splitwrap")) closeAuthMenu();
    });
  }

  app.querySelector<HTMLButtonElement>('[data-act="customserver"]')
    ?.addEventListener("click", () => { serverShown = true; authMenu = false; paint(); });

  app.querySelector<HTMLButtonElement>('[data-act="canceladd"]')
    ?.addEventListener("click", cancelAddServer);

  // Back to the pinned address, and forget whatever was typed — otherwise the
  // old one quietly wins over the default on every later visit.
  app.querySelector<HTMLButtonElement>('[data-act="defaultserver"]')
    ?.addEventListener("click", () => {
      client.useDefaultServer();
      serverShown = false;
      authErr = "";
      authMenu = false;
      paint();
    });

  app.querySelector<HTMLButtonElement>('[data-act="toregister"]')
    ?.addEventListener("click", () => { authMode = "register"; authErr = ""; authMenu = false; paint(); });
  app.querySelector<HTMLButtonElement>('[data-act="tologin"]')
    ?.addEventListener("click", () => { authMode = "login"; authErr = ""; authMenu = false; paint(); });

  // The second door: no account, no server, nobody to ask for permission. Only
  // the nick field is read — everything else on this form is about a server
  // that, on a plane, is not there.
  app.querySelector<HTMLButtonElement>('[data-act="nearby"]')?.addEventListener("click", async () => {
    const nick = (app.querySelector('[name="nick"]') as HTMLInputElement | null)?.value.trim();
    if (!nick) {
      authErr = "pick a nick first — it is the only thing people nearby will see";
      return paint();
    }
    try {
      authErr = "";
      await client.startSolo(nick);
    } catch (err) {
      authErr = err instanceof Error ? err.message : "could not start nearby mode";
    }
    paint();
  });

  app.querySelector<HTMLFormElement>("#authform")?.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = new FormData(e.target as HTMLFormElement);
    const server = String(f.get("server") || "").trim();
    if (!server) { authErr = "enter your server address, e.g. http://localhost:8787"; return paint(); }
    try {
      authErr = "";
      if (addingServer) {
        // Signing in somewhere else. The session on screen is untouched until
        // this succeeds; the reload comes up on the new server, because adding
        // it made it the active one.
        await client.addServerLogin(String(f.get("nick")), String(f.get("password")), server);
        location.reload();
        return;
      }
      if (authMode === "login") {
        await client.login(String(f.get("nick")), String(f.get("password")), server);
      } else {
        await client.register(
          String(f.get("nick")), String(f.get("email")),
          String(f.get("password")), String(f.get("invite") ?? ""), server,
        );
        // A fresh account in a browser tab is the moment to point at the real
        // thing; the notice renders inline, so the address is a link.
        if (!isTauri()) client.notice(`welcome. gliph is nicer as a desktop app: ${downloadsURL(server)}`);
      }
      client.connect();
      paint();
    } catch (err) {
      authErr = err instanceof Error ? err.message : "could not connect";
      paint();
    }
  });
}

// ---- app wiring ----

/** Theme changes have to reach the notification window too — it is a separate
 *  document with its own copy of the stylesheet. */
function setTheme(t: "creme" | "midnight") {
  client.setTheme(t);
  void retheme(t);
}

/** Blocks the reader unfolded. Keyed by content, not event id, so the same
 *  paste stays open across repaints and re-syncs. */
const expandedCode = new Set<string>();

/** Second pass over the stream's code blocks: fold state, then colour. Colour
 *  is async and best-effort — a block reads fine in plain mono until Monaco
 *  arrives, and a language Monaco does not know stays plain. */
function hydrateCode() {
  for (const card of app.querySelectorAll<HTMLElement>(".card.code")) {
    const id = card.dataset["code"] ?? "";
    card.classList.toggle("open", expandedCode.has(id));
    card.querySelector<HTMLButtonElement>("[data-expand]")?.addEventListener("click", () => {
      if (expandedCode.has(id)) expandedCode.delete(id); else expandedCode.add(id);
      card.classList.toggle("open", expandedCode.has(id));
    });
    const code = card.querySelector<HTMLElement>("code[data-lang]");
    if (!code) continue;
    const text = code.textContent ?? "";
    colorize(id, text, code.dataset["lang"] ?? "plaintext")
      // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- Monaco colorize() output: it escapes the code it tokenises
      .then((html) => { if (code.isConnected && code.textContent === text) code.innerHTML = html; })
      .catch(() => {});
  }
}

/** The DM we asked the server to open and are waiting to see in the channel
 *  list; paint() switches to it the moment it appears. */
let pendingDm = "";

function openDm(nick: string) {
  if (!nick || nick === client.user?.nick) return;
  const dm = client.channels.find((c) => c.is_dm && c.name === nick);
  if (dm) return openChannel(dm.id);
  pendingDm = nick;
  client.runCommand("msg", nick, { record: false });
}

function openChannel(cid: string) {
  if (!client.channels.some((c) => c.id === cid)) return;
  // The core drops the blob when the channel changes — an app belongs to the
  // room it is attached to.
  client.active = cid;
  railOpen = false;
  // A channel opens on its newest line, whatever the last one was scrolled to.
  pinBottom = true;
  paint();
}

/** What was sent, newest last, for ↑/↓ in an empty palette — the shell's
 *  habit, and the one that makes retyping `/msg robin` unnecessary. Kept in
 *  localStorage so a relaunch does not forget it; fifty is plenty. */
const LS_HISTORY = "gliph.history";
let history: string[] = (() => { try { return JSON.parse(localStorage.getItem(LS_HISTORY) ?? "[]"); } catch { return []; } })();
/** Where ↑ has walked to; -1 is "the line being typed", kept in `stash`. */
let histAt = -1;
let stash = "";

function walkHistory(dir: -1 | 1) {
  if (!history.length) return;
  if (histAt === -1) { if (dir === 1) return; stash = draft; }
  const next = histAt === -1 ? history.length - 1 : histAt + dir;
  if (next >= history.length) { histAt = -1; draft = stash; }
  else if (next < 0) return;
  else { histAt = next; draft = history[next]!; }
  const input = document.getElementById("input") as HTMLInputElement | null;
  if (input) { input.value = draft; input.setSelectionRange(draft.length, draft.length); }
}

function submitDraft() {
  const text = draft.trim();
  if (!text) return;
  if (history[history.length - 1] !== text) {
    history = [...history, text].slice(-50);
    try { localStorage.setItem(LS_HISTORY, JSON.stringify(history)); } catch { /* fine */ }
  }
  histAt = -1;
  client.submit(text);
  draft = "";
  palette = [];
  sel = 0;
  paint();
}

/** `@wa` at the end of the draft → members of this channel as palette rows.
 *  ponytail: matched here rather than in the core; it is one prefix filter
 *  over a list the frontend already renders. Move it when iOS wants it. */
function matchMentions(text: string): Command[] {
  const m = /@(\w*)$/.exec(text);
  if (!m) return [];
  const q = m[1]!.toLowerCase();
  return (client.channel(client.active)?.members ?? [])
    .filter((x) => x.nick.toLowerCase().startsWith(q))
    .slice(0, 8)
    .map((x) => ({ name: `@${x.nick}`, usage: `@${x.nick}`, desc: x.away ? `away: ${x.away}` : x.online ? "online" : "offline", args: null, scope: "any", min_role: "member", source: "builtin" }));
}

function acceptPalette() {
  const cmd = palette[sel];
  if (!cmd) return;
  if (cmd.name.startsWith("@")) {
    draft = draft.replace(/@\w*$/, cmd.name + " ");
    palette = [];
    sel = 0;
    return paint();
  }
  const needsArgs = (cmd.args ?? []).length > 0;
  draft = `/${cmd.name}` + (needsArgs ? " " : "");
  palette = [];
  sel = 0;
  if (!needsArgs) return submitDraft();
  paint();
}

/** Blob surface wiring. Every gesture is a forward: the selection, the menu,
 *  the drafts and what enter means all live in core/src/gum.rs, so this client
 *  and the iOS one cannot disagree about them. */
function wireBlobs() {
  // switcher: chat first, then this channel's blobs
  for (const el of app.querySelectorAll<HTMLElement>("[data-blob]")) {
    el.addEventListener("click", () => client.openBlob(el.dataset["blob"]!));
  }
  if (!client.activeBlob) return;

  for (const el of app.querySelectorAll<HTMLElement>("[data-focus]")) {
    el.addEventListener("click", () => client.gumTap(Number(el.dataset["focus"])));
  }
  for (const el of app.querySelectorAll<HTMLElement>("[data-menu]")) {
    // mousedown, not click: the menu is gone by the time a click would land.
    el.addEventListener("mousedown", (e) => {
      e.preventDefault();
      client.gumTapMenu(el.dataset["menu"]!);
    });
  }

  const input = app.querySelector<HTMLInputElement>(".ginput input");
  input?.addEventListener("input", () => client.gumSetDraft(input.value));

  // Keep the selection on screen once a list or menu outgrows its box.
  (app.querySelector<HTMLElement>(client.surface()?.menu ? ".gmenu .sel" : ".gum > .sel"))
    ?.scrollIntoView({ block: "nearest" });
  app.querySelector<HTMLElement>(".gum")?.focus();
}

/** How tall each attachment preview is allowed to be, by file. A repaint throws
 *  the DOM away on every arriving message, so the size the reader chose has to
 *  live out here or it snaps back to half a screen mid-conversation. */
const attHeight = new Map<string, number>();

/** And how much room each one ended up taking, so a repaint can hold the space
 *  before the picture is back. */
const attShown = new Map<string, number>();

/** Grow or shrink one preview by a quarter of the viewport (`dir` 0 just
 *  re-applies what is already stored, after a load or a repaint). The ceiling
 *  is the picture itself: 100% is the whole thing, and there is no zoom past
 *  it — that is what the lightbox is for. */
function sizeAtt(el: HTMLElement, dir: number) {
  const img = el.querySelector("img");
  if (!img?.clientHeight) return;
  const stream = document.getElementById("stream");
  const url = el.dataset["url"] ?? "";
  const base = Math.round(window.innerHeight / 2);
  const want = (attHeight.get(url) ?? base) + dir * Math.round(window.innerHeight / 4);
  const h = Math.max(120, Math.min(want, img.clientHeight));
  if (dir) attHeight.set(url, h);
  // max-height counts the border under border-box, so hand it the frame too or
  // "the whole picture" is the whole picture minus two pixels.
  el.style.maxHeight = `${h + el.offsetHeight - el.clientHeight}px`;
  el.classList.toggle("cut", img.clientHeight > h + 1);
  // [ - ] is only worth showing once there is something to take back.
  el.classList.toggle("grown", attHeight.has(url));
  // Remember the room it took, so the next repaint reserves it up front
  // instead of collapsing to nothing until the image decodes again.
  const shown = Math.min(h, img.clientHeight);
  attShown.set(url, shown);
  el.style.minHeight = `${shown}px`;
  if (pinBottom && stream) stream.scrollTop = stream.scrollHeight;
}

/** Media cards: nothing here loads until asked, except when the reader has said
 *  "always" — which is a choice they made once, not a default we took. */
function wireCards() {
  // A card that finishes loading (or failing) after the paint grows the stream
  // under a reader parked at the bottom, and the pinned position goes stale —
  // the newest line ends up below the fold on every fresh open. sizeAtt does
  // this for images that decode; this covers everything else loadCard swaps in.
  const repin = () => {
    const s = document.getElementById("stream");
    if (pinBottom && s) s.scrollTop = s.scrollHeight;
  };
  for (const el of app.querySelectorAll<HTMLElement>("[data-load]")) {
    el.addEventListener("click", (e) => {
      e.stopPropagation();
      const card = el.closest<HTMLElement>("[data-card]");
      if (card) void loadCard(card).then(repin);
    });
  }
  for (const el of app.querySelectorAll<HTMLElement>("[data-auto]")) {
    void loadCard(el).then(repin);
  }
  // An attachment preview starts cut off at half the viewport. [ - ] and [ + ]
  // give it more or less room in the stream — never more than the whole
  // picture — and a click opens the lightbox.
  for (const el of app.querySelectorAll<HTMLElement>(".att")) {
    const seen = attShown.get(el.dataset["url"] ?? "");
    if (seen) el.style.minHeight = `${seen}px`;
    el.addEventListener("click", (e) => {
      const step = e.target instanceof HTMLElement ? e.target.dataset["att"] : undefined;
      if (step) {
        e.stopPropagation();
        return sizeAtt(el, step === "+" ? 1 : -1);
      }
      const img = el.querySelector("img");
      if (img) openLightbox(img.src, el.dataset["name"] ?? "", el.dataset["url"] ?? "");
    });
    // Capture, because load does not bubble and the image arrives after this
    // runs — and only then is there a height to measure.
    el.addEventListener("load", () => sizeAtt(el, 0), true);
    // A picture that was already decoded came straight back into the slot and
    // fires no load event, so measure it here instead. No-op until there is an
    // image with a height.
    sizeAtt(el, 0);
  }
  for (const el of app.querySelectorAll<HTMLElement>("[data-copy]")) {
    el.addEventListener("click", async () => {
      const code = el.closest(".card")?.querySelector("code")?.textContent ?? "";
      try {
        await navigator.clipboard.writeText(code);
        el.textContent = "copied";
        setTimeout(() => { el.textContent = "copy"; }, 1200);
      } catch {
        el.textContent = "copy failed";
      }
    });
  }
  // Every repaint throws away the previous DOM, so any blob URL whose card went
  // with it is now unreachable. Without this a long session leaks every image
  // it ever showed.
  reapBlobs(app);
}

/** Right-click on a message. Only the rows the renderer gave a handle to, so
 *  the browser's own menu still works everywhere else — including on a link,
 *  where "open in new tab" is what a right-click is for. */
function wireStreamMenu() {
  app.querySelector<HTMLElement>("#stream")?.addEventListener("contextmenu", (e) => {
    const row = (e.target as HTMLElement).closest<HTMLElement>("[data-ev]");
    if (!row || (e.target as HTMLElement).closest("a")) return;
    const ev = client.visibleEvents().find((x) => x.id === row.dataset["ev"]);
    if (!ev?.text) return;
    e.preventDefault();
    openMenu(e.clientX, e.clientY, [
      { label: "reply", run: () => client.setReply(ev.id) },
      { label: "copy text", run: () => void navigator.clipboard.writeText(ev.text!) },
    ]);
  });
}

/** A quote is a link back to what it answers. The row is already in the DOM —
 *  the renderer only makes a quote clickable when it is — so this scrolls to it
 *  and flashes it, which is the whole feature. */
function wireJumps() {
  for (const el of app.querySelectorAll<HTMLElement>("[data-jump]")) {
    el.addEventListener("click", () => {
      const row = app.querySelector<HTMLElement>(`[data-ev="${CSS.escape(el.dataset["jump"]!)}"]`);
      if (!row) return;
      row.scrollIntoView({ block: "center", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
      // Restarted rather than added: jumping twice to the same line should
      // flash twice, and a class that is already there does not re-animate.
      row.classList.remove("flash");
      void row.offsetWidth;
      row.classList.add("flash");
    });
  }
}

/** The MOTD and release-note cards: open one, or close it for good.
 *
 *  The "read more" button appears only when there is more — measured rather
 *  than guessed from the length of the text, because three lines is a number
 *  of lines, not a number of characters. */
function wireSystem() {
  for (const card of app.querySelectorAll<HTMLElement>(".sysmsg")) {
    const text = card.querySelector<HTMLElement>(".systext");
    const more = card.querySelector<HTMLButtonElement>(".sysmore");
    if (text && more) {
      const clipped = text.scrollHeight > text.clientHeight + 1;
      more.hidden = !clipped && !card.classList.contains("open");
      more.textContent = card.classList.contains("open") ? "show less" : "read more";
      more.addEventListener("click", () => {
        toggleSystem(card.dataset["sys"]!);
        paint();
      });
    }
    card.querySelector<HTMLElement>("[data-sysx]")?.addEventListener("click", () => {
      client.dismissSystem(card.dataset["sys"]!);
    });
  }
}

function wireApp() {
  // The reader's scrolling is the only thing that parks or un-parks the
  // stream; and reaching the top of a cut window pulls in the next 200 lines,
  // holding the line they were on in place.
  const stream = document.getElementById("stream");
  stream?.addEventListener("scroll", () => {
    if (client.activeBlob) return; // a blob's doc scrolls in the same node
    pinBottom = stream.scrollTop + stream.clientHeight >= stream.scrollHeight - 60;
    if (stream.scrollTop < 300 && document.getElementById("older")) {
      const fromEnd = stream.scrollHeight - stream.scrollTop;
      growStream();
      paint();
      const s = document.getElementById("stream");
      if (s) s.scrollTop = s.scrollHeight - fromEnd;
    }
  });

  for (const el of app.querySelectorAll<HTMLElement>("[data-chan]")) {
    el.addEventListener("click", () => openChannel(el.dataset["chan"]!));
  }

  for (const el of app.querySelectorAll<HTMLElement>("[data-cmd]")) {
    el.addEventListener("mousedown", (e) => {
      e.preventDefault();
      sel = palette.findIndex((c) => c.name === el.dataset["cmd"]);
      acceptPalette();
    });
  }

  const act = (name: string, fn: () => void) => {
    for (const el of app.querySelectorAll<HTMLElement>(`[data-act="${name}"]`)) {
      el.addEventListener("click", fn);
    }
  };

  if (isTauri()) {
    app.querySelector<HTMLElement>(".menubar")
      ?.addEventListener("mousedown", (e) => startDrag(e as MouseEvent));
    app.querySelector<HTMLElement>(".titlestrip")
      ?.addEventListener("mousedown", (e) => startDrag(e as MouseEvent));
  }
  // a chat stream is full of other people's links: never navigate in-app
  for (const a of app.querySelectorAll<HTMLAnchorElement>('a[href^="http"]')) {
    a.addEventListener("click", (e) => { e.preventDefault(); void openExternal(a.href); });
  }

  // ---- the server switcher ----
  act("servers", () => {
    const b = app.querySelector<HTMLElement>('.serverbar .me');
    const r = b?.getBoundingClientRect();
    openServerMenu(client, r ? r.left : 0, r ? r.bottom + 2 : 0, startAddServer);
  });
  act("addserver", startAddServer);
  for (const el of app.querySelectorAll<HTMLElement>("[data-server]")) {
    el.addEventListener("click", () => client.switchTo(el.dataset["server"] ?? ""));
  }

  wireStreamMenu();
  wireJumps();
  wireBlobs();
  wireCards();
  wireSystem();
  for (const el of app.querySelectorAll<HTMLElement>("[data-nick]")) {
    el.addEventListener("click", (e) => { e.preventDefault(); openDm(el.dataset["nick"] ?? ""); });
  }

  // The console is another page of this same bundle, so in a browser it opens
  // in a tab that is already signed in — same origin, same stored token. A
  // packaged shell has no tab to open one in, so it goes to
  // the real server in the system browser, where the console asks for the
  // password itself.
  act("manage", () => {
    void openExternal(isTauri()
      ? client.server.replace(/\/+$/, "") + "/admin"
      : new URL("/admin", location.href).href);
  });
  act("theme", () => setTheme(client.theme() === "creme" ? "midnight" : "creme"));
  act("logout", () => client.logout());
  act("rail", () => { railOpen = !railOpen; paint(); });
  act("help", () => { helpOpen = true; paint(); });
  act("update", runUpdate);
  act("list", () => client.runCommand("list", ""));
  act("room0", () => client.runCommand("room0", ""));
  // The room's own door, from the strip or from an invite. Joining a channel
  // you are not reading switches to it first: you cannot stand in a room and
  // be somewhere else.
  // Every join button carries the channel whose room it opens — the strip's
  // own, or the one a personal invite in a DM points back at. Read it off the
  // button that was clicked, not the first one on the page.
  for (const el of app.querySelectorAll<HTMLElement>('[data-act="room-join"]')) {
    el.addEventListener("click", () => {
      const chan = el.dataset["chan"] ?? "";
      if (chan && chan !== client.active) openChannel(chan);
      client.runCommand("room0", "");
    });
  }
  // Hiding is for this reading of the channel only: the strip is live state,
  // so it comes back the next time the room does.
  act("room-hide", () => { hiddenStrip = client.active; paint(); });
  act("room-dismiss", () => { hiddenStrip = client.active; paint(); });
  act("send", () => submitDraft());
  act("closehelp", () => { helpOpen = false; paint(); });
  // The download button's platform menu. Toggled in place, no state: the
  // next repaint (any event) rebuilds the stream and that closes it.
  act("appmenu", () => {
    const m = app.querySelector<HTMLElement>(".appmenu"), c = app.querySelector<HTMLElement>('[data-act="appmenu"]');
    if (!m || !c) return;
    m.hidden = !m.hidden;
    c.classList.toggle("open", !m.hidden);
    c.setAttribute("aria-expanded", String(!m.hidden));
    c.innerHTML = m.hidden ? "&#9660;" : "&#9650;";
  });
  act("closemenu", () => client.gumEscape());
  act("undo", () => {
    const back = takeUndo();
    if (back) {
      client.runCommand(back.name, back.args, { record: false });
      client.notice(`undid: ${back.label}`);
    }
    paint();
  });
  act("dismisstoast", () => { dismissToast(); paint(); });
  act("unreply", () => client.setReply(""));
  act("unfurl", () => {
    const next = { ask: "always", always: "never", never: "ask" } as const;
    setUnfurlMode(next[unfurlMode()]);
    paint();
  });
  act("alerts", () => {
    const next = { custom: "system", system: "none", none: "custom" } as const;
    setMode(next[notifyMode()]);
    paint();
  });
  app.querySelector<HTMLElement>("[data-alert-all]")
    ?.addEventListener("click", (e) => { e.stopPropagation(); dismissAll(); });
  for (const el of app.querySelectorAll<HTMLElement>("[data-alert-chan]")) {
    el.addEventListener("click", (e) => {
      if ((e.target as HTMLElement).closest("[data-alert-x]")) return;
      const chan = el.dataset["alertChan"]!;
      dismissChannel(chan);
      openChannel(chan);
    });
  }
  for (const el of app.querySelectorAll<HTMLElement>("[data-alert-x]")) {
    el.addEventListener("click", (e) => { e.stopPropagation(); dismissChannel(el.dataset["alertX"]!); });
  }
  // clicking inside the panel must not close it
  app.querySelector<HTMLElement>("[data-stop]")
    ?.addEventListener("click", (e) => e.stopPropagation());

  const fileInput = document.getElementById("file") as HTMLInputElement | null;
  act("attach", () => fileInput?.click());
  // /send <nick> needs to open the picker, and only the DOM can do that.
  client.pickFile = () => fileInput?.click();
  act("transfers", () => { xferOpen = true; paint(); });

  for (const el of app.querySelectorAll<HTMLElement>("[data-xaccept]")) {
    el.addEventListener("click", () => client.acceptTransfer(el.dataset.xaccept!));
  }
  for (const el of app.querySelectorAll<HTMLElement>("[data-xdecline]")) {
    el.addEventListener("click", () => client.declineTransfer(el.dataset.xdecline!));
  }
  app.querySelector<HTMLElement>('[data-overlay="transfers"]')?.addEventListener("click", (e) => {
    if (e.target === e.currentTarget) { xferOpen = false; paint(); }
  });
  fileInput?.addEventListener("change", async () => {
    const file = fileInput.files?.[0];
    if (file) await sendFile(file);
    fileInput.value = "";
  });

  const input = document.getElementById("input") as HTMLInputElement | null;
  // A screenshot on the clipboard pastes as a file, and goes the way a picked
  // one does. Text pastes fall through to the input as before.
  input?.addEventListener("paste", (e) => {
    const file = e.clipboardData?.files[0];
    if (!file) return;
    e.preventDefault();
    void sendFile(file);
  });
  input?.addEventListener("input", () => {
    draft = input.value;
    // ``` opens the code editor; whatever was typed before it becomes the
    // line above the block.
    if (draft.endsWith("```")) {
      const lead = draft.slice(0, -3).trim();
      draft = "";
      input.value = "";
      void openCodepad((lang, code) => {
        client.submit((lead ? lead + "\n" : "") + "```" + lang + "\n" + code + "\n```");
        paint();
      });
      return;
    }
    const next = draft.startsWith("/") ? client.matchCommands(draft) : matchMentions(draft);
    const changed = next.length !== palette.length ||
      next.some((c, i) => c.name !== palette[i]?.name);
    palette = next;
    if (changed) { sel = 0; paint(); }
  });

  input?.addEventListener("keydown", (e) => {
    if (palette.length) {
      if (e.key === "ArrowDown") { e.preventDefault(); sel = (sel + 1) % palette.length; return paint(); }
      if (e.key === "ArrowUp") { e.preventDefault(); sel = (sel - 1 + palette.length) % palette.length; return paint(); }
      // Enter takes the highlighted command like Tab does: `/upd⏎` is
      // /update, not a message saying "/upd".
      if (e.key === "Tab" || e.key === "Enter") { e.preventDefault(); return acceptPalette(); }
      if (e.key === "Escape") { palette = []; return paint(); }
    } else {
      if (e.key === "Escape" && client.replyTo) return client.setReply("");
      if (e.key === "ArrowUp") { e.preventDefault(); return walkHistory(-1); }
      if (e.key === "ArrowDown") { e.preventDefault(); return walkHistory(1); }
    }
    if (e.key === "Enter") { e.preventDefault(); submitDraft(); }
  });
}

/** Take the menu away without repainting. A repaint rebuilds the form, which
 *  loses the focused field — and closing on a keystroke has to leave the
 *  keystroke somewhere to land. */
function closeAuthMenu() {
  if (!authMenu) return;
  authMenu = false;
  app.querySelector(".authmenu")?.remove();
  const caret = app.querySelector<HTMLButtonElement>('[data-act="authmenu"]');
  caret?.classList.remove("open");
  caret?.setAttribute("aria-expanded", "false");
  if (caret) caret.innerHTML = "&#9660;";
}

// Typing closes the sign-in menu: start filling in the form and it should get
// out of the way rather than sit over the thing you are doing. It is the only
// overlay on that screen, so it has its own listener rather than a branch in
// the app-wide one, which never runs while there is no session.
document.addEventListener("keydown", (e) => {
  if (!authMenu || client.token || client.solo) return;
  // Typing closes it, navigating does not — e.key is a single character exactly
  // when the key produces text, so tab and the arrows can still reach the items.
  if (e.key === "Escape") e.preventDefault();
  else if (e.key.length !== 1 || e.ctrlKey || e.metaKey) return;
  closeAuthMenu();
});

// ctrl+1..9 flips between chat and this channel's apps, the way ctrl+N flipped
// between windows in mIRC. Which app is nth is the core's answer, not this
// file's — it is the same order the switcher renders.
document.addEventListener("keydown", (e) => {
  if ((!client.token && !client.solo) || helpOpen) return;
  if (!(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey) return;
  if (!/^[1-9]$/.test(e.key)) return;
  e.preventDefault();
  client.blobSwitch(Number(e.key));
});

// The blob surface's keys. Only active while a blob is open, and never while
// the user is typing in the blob's own input (except enter/escape). Which key
// does what is core/src/gum.rs's business, not this file's — see the tests
// there.
document.addEventListener("keydown", (e) => {
  if ((!client.token && !client.solo) || !client.activeBlob || helpOpen) return;
  const typing = document.activeElement instanceof HTMLInputElement;

  if (e.key === "Escape") { e.preventDefault(); return client.gumEscape(); }
  if (e.key === "Enter") { e.preventDefault(); return client.gumActivate(); }
  if (typing) return;

  if (e.key === "ArrowDown" || e.key === "j") { e.preventDefault(); return client.gumMove(1); }
  if (e.key === "ArrowUp" || e.key === "k") { e.preventDefault(); return client.gumMove(-1); }
  if (/^[1-9]$/.test(e.key)) { e.preventDefault(); return client.gumJump(Number(e.key)); }
});

// The global dispatcher. Every key it fires comes from the registry in
// shortcuts.ts, so the `?` overlay and this switch can never disagree.
document.addEventListener("keydown", (e) => {
  if (!client.token && !client.solo) return;

  // Same rule as the help overlay: while the transfer window is up it owns esc.
  if (xferOpen) {
    if (e.key === "Escape") { e.preventDefault(); xferOpen = false; paint(); }
    return;
  }
  // The overlay owns the keyboard while it is up: esc and ? close it, and
  // nothing else gets through.
  if (helpOpen) {
    if (e.key === "Escape" || e.key === "?") { e.preventDefault(); helpOpen = false; paint(); }
    return;
  }
  if (typing() || overlayOpen()) return;

  const chans = client.channels;
  const go = (i: number) => { if (chans[i]) openChannel(chans[i]!.id); };
  const here = chans.findIndex((c) => c.id === client.active);

  switch (resolve(e)) {
    case "focus-composer": {
      const input = document.getElementById("input") as HTMLInputElement | null;
      if (!input) return;
      e.preventDefault(); input.focus(); draft = "/"; paint();
      return;
    }
    case "help": e.preventDefault(); helpOpen = true; return paint();
    case "next-channel": e.preventDefault(); return go((here + 1) % (chans.length || 1));
    case "prev-channel": e.preventDefault(); return go((here - 1 + chans.length) % (chans.length || 1));
    case "channel-n": e.preventDefault(); return go(Number(e.code.slice(-1)) - 1);
    case "toggle-rail": e.preventDefault(); railOpen = !railOpen; return paint();
    case "toggle-theme":
      e.preventDefault();
      return setTheme(client.theme() === "creme" ? "midnight" : "creme");
    case "channel-list": e.preventDefault(); return client.runCommand("list", "");
    case "undo": {
      e.preventDefault();
      const back = takeUndo();
      if (!back) return client.notice("nothing to undo");
      // record:false — otherwise the undo records itself and ⌘Z flip-flops.
      client.runCommand(back.name, back.args, { record: false });
      return client.notice(`undid: ${back.label}`);
    }
    case "redo": {
      e.preventDefault();
      const again = takeRedo();
      if (!again) return client.notice("nothing to redo");
      client.runCommand(again.name, again.args, { record: false });
      return client.notice(`redid: ${again.label}`);
    }
  }
});

if (client.token) {
  client.connect();
}
paint();

// The bracket on every server we are NOT looking at. A poll, not a socket per
// server: a count a minute stale is the right accuracy for "something is
// waiting elsewhere". Repaints only when the core says something changed, so a
// quiet minute costs one request per server and no DOM work.
const pollServers = () => void client.pollServers().catch(() => {});
if (client.token) {
  pollServers();
  setInterval(pollServers, 60_000);
  // Coming back to a tab that slept for an hour: refresh before the reader has
  // time to notice the counts are stale.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") pollServers();
  });
}

// ponytail: dev-only look at the notification cards. /?demo fires one of each
// kind, staggered — and so does ctrl+alt+D, which is the only way to reach this
// from inside the desktop app, whose window has no address bar to put ?demo in.
if (import.meta.env.DEV) {
  const chan = () => client.channels[0]?.id ?? "demo";
  const seed: [string, "dm" | "mention" | "highlight" | "nearby", string, string, string][] = [
    ["d1", "dm", "ada", "", "the key rotation landed \u2014 every client picked up the new epoch on reconnect"],
    ["d2", "mention", "grace", "#ops", "@demo the freeze is on, hold the deploy until monday"],
    ["d3", "nearby", "hopper", "#studio", "anyone got a lightning cable"],
    ["d4", "dm", "linus", "", "sent you keys.tar.gz \u2014 2.1 MB"],
  ];
  let run = 0;
  const fire = () => {
    const n = ++run;
    seed.forEach(([id, event, nick, where, text], i) => setTimeout(
      () => showAlert(
        { id: `${id}-${n}`, channel: chan(), event, nick, where, text, at: Date.now() },
        openChannel),
      i * 1200));
  };
  if (new URLSearchParams(location.search).has("demo")) setTimeout(fire, 600);
  // e.code, not e.key: on macOS option+d is "∂", so the letter is gone by the
  // time the event arrives. The rest of the app reads e.code for the same reason.
  addEventListener("keydown", (e) => {
    if (e.ctrlKey && e.altKey && e.code === "KeyD") { e.preventDefault(); fire(); }
  });
}
