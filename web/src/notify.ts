/** Notifications: what gliph does when something arrives that you asked to be
 *  told about — a DM, a line matching one of your highlight words, or anything
 *  said in a nearby room.
 *
 *  THE PRIVACY RULE. A notification is rendered from a body this client already
 *  decrypted, on this device. Nothing in this file reaches the network, and no
 *  notification can ever be produced from data the server holds — in a +e
 *  channel the server has ciphertext and could not write this text if it tried.
 *  That is the same shape the mobile app's push lane takes (spec F5): the wire
 *  carries a wake reference, the device decrypts, and only then is there
 *  anything to show. The phone renders its own — see mobile/lib/push.dart.
 *
 *  THREE MODES, TWO SURFACES.
 *
 *    custom  — gliph's own card. On the desktop that is a real OS window, top
 *              right of the screen, above everything, that never takes focus
 *              (notifywin.ts + the NSPanel conversion in the Tauri shell). In a
 *              browser there is no such thing, so the same cards render into the
 *              page instead. Default, because it is the one surface we control.
 *    system  — hand it to the OS notification centre and let it look like every
 *              other app's alert.
 *    none    — nothing on screen. The sound still has its own toggle.
 *
 *  The card model is ported from inbox2's notification store: an event kind
 *  carrying its own label, colour and priority; a priority-driven lifetime; a
 *  run of arrivals in one channel merged into a single card; and a fixed-depth
 *  stack whose newest member is the only one drawn in full.
 */

import { isTauri, log } from "./desktop";

export type Mode = "custom" | "system" | "none";

const LS_MODE = "gliph.notify.mode";
const LS_SOUND = "gliph.notify.sound";

export const mode = (): Mode => (localStorage.getItem(LS_MODE) as Mode) ?? "custom";
export const setMode = (m: Mode) => localStorage.setItem(LS_MODE, m);
/** Sound is independent of the surface: people want the ping with no banner,
 *  and the banner with no ping. Neither implies the other. */
export const soundOn = (): boolean => localStorage.getItem(LS_SOUND) !== "off";

// ---- event kinds ----------------------------------------------------------

/** Why the core fired. It classifies at the point of the decision (see
 *  Effect::Alert) — this file only decides what that looks like. */
export type Event = "dm" | "mention" | "highlight" | "nearby" | "join";
export type Priority = "low" | "normal" | "high";

/** Label, colour token and priority per kind. One table, so the card, its
 *  colour rail and its time-bar can never disagree about what an event is. */
const EVENT_META: Record<Event, { label: string; color: string; priority: Priority }> = {
  dm:        { label: "direct message", color: "var(--accent)", priority: "high" },
  mention:   { label: "tagged you",     color: "var(--accent)", priority: "high" },
  highlight: { label: "mentioned you",  color: "var(--action)", priority: "high" },
  nearby:    { label: "nearby",         color: "var(--agent)",  priority: "normal" },
  join:      { label: "new here",       color: "var(--join)",   priority: "low" },
};

/** How long a card stays up, by priority. Being named at you is worth a longer
 *  look than a room you happen to be standing in. */
const DISMISS_MS: Record<Priority, number> = { low: 6000, normal: 8000, high: 14000 };

/** The wire shape between the main window and the notification window. Plain
 *  data — it crosses a Tauri event boundary and a URL hash. */
export type Alert = {
  /** the event id — the dedup key, so one message never appears twice */
  id: string;
  /** channel id: where a click on the card goes */
  channel: string;
  event: Event;
  nick: string;
  /** "#ops", or "" for a DM — a DM's title is the nick alone */
  where: string;
  text: string;
  at: number;
};

/** A line in the stream. */
export type Entry = Alert & {
  label: string;
  color: string;
  priority: Priority;
};

/** Milliseconds this line still has to run — what the draining bar is told.
 *  Both surfaces re-render the whole stack on every change, which restarts the
 *  CSS animation, so it has to be the time REMAINING and not the full span. */
export const msLeft = (e: Entry): number =>
  Math.max(0, DISMISS_MS[e.priority] - (Date.now() - e.at));

export const enrich = (a: Alert): Entry => {
  const meta = EVENT_META[a.event] ?? EVENT_META.highlight;
  return { ...a, label: meta.label, color: meta.color, priority: meta.priority };
};

// ---- the stack ------------------------------------------------------------
//
// Owned by whichever page is rendering cards: the main window in a browser, the
// notification window on the desktop. Same code either way.

// One stream: the last few lines, at most a handful per channel. New lines
// stack in; the caps evict the oldest, which is what stops a busy room from
// filling the screen.
const MAX_LINES = 7;
const MAX_PER_CHANNEL = 5;

let stack: Entry[] = [];
let onStack: (() => void) | null = null;
const timers = new Map<string, number>();

export const visible = (): Entry[] => stack;
export const onStackChange = (fn: () => void) => { onStack = fn; };

export function push(a: Alert) {
  if (stack.some((x) => x.id === a.id)) return;
  const e = enrich(a);
  // An alert whose time is already up is not news — a page reload replaying
  // its URL hash, say. Without a timer it would sit in the stack forever,
  // holding the window open as an empty strip.
  if (msLeft(e) <= 0) return;
  const kept: Entry[] = [];
  const per = new Map<string, number>();
  for (const x of [e, ...stack]) {
    const n = (per.get(x.channel) ?? 0) + 1;
    per.set(x.channel, n);
    if (n <= MAX_PER_CHANNEL && kept.length < MAX_LINES) kept.push(x);
    else clearTimer(x.id);
  }
  stack = kept;
  // Every line runs its own clock down: it fades where it stands, and the
  // stream empties line by line rather than all at once.
  const left = msLeft(e);
  if (left > 0) timers.set(e.id, window.setTimeout(() => dismiss(e.id), left));
  onStack?.();
}

function clearTimer(id: string) {
  const t = timers.get(id);
  if (t) { clearTimeout(t); timers.delete(id); }
}

export function dismiss(id: string) {
  clearTimer(id);
  stack = stack.filter((x) => x.id !== id);
  onStack?.();
}

/** One gesture deals with a whole conversation's lines. */
export function dismissChannel(channel: string) {
  for (const x of stack) if (x.channel === channel) clearTimer(x.id);
  stack = stack.filter((x) => x.channel !== channel);
  onStack?.();
}

export function dismissAll() {
  for (const t of timers.values()) clearTimeout(t);
  timers.clear();
  stack = [];
  onStack?.();
}

// ---- sound ----------------------------------------------------------------

/** One AudioContext for the life of the page. WKWebView (and Chrome) create a
 *  context suspended until a user gesture has touched it, and a context made
 *  at alert time has never seen one — so it is created and resumed on the
 *  first click (see `unlockAudio`) and reused from then on. */
let audio: AudioContext | null = null;

export function unlockAudio() {
  try {
    audio ??= new (window.AudioContext ?? (window as any).webkitAudioContext)();
    void audio.resume();
  } catch { /* no audio device */ }
}

/** A short two-tone blip synthesised on the spot. An audio file would be one
 *  more asset to ship, cache-bust and license for eight notes. */
function ping() {
  if (!soundOn()) return;
  try {
    unlockAudio();
    const ctx = audio!;
    if (ctx.state !== "running") { log("warn", "notify: audio context is " + ctx.state); }
    const now = ctx.currentTime;
    for (const [i, hz] of [880, 1174].entries()) {
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.type = "sine";
      osc.frequency.value = hz;
      gain.gain.setValueAtTime(0.0001, now + i * 0.09);
      gain.gain.exponentialRampToValueAtTime(0.12, now + i * 0.09 + 0.01);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + i * 0.09 + 0.09);
      osc.connect(gain).connect(ctx.destination);
      osc.start(now + i * 0.09);
      osc.stop(now + i * 0.09 + 0.1);
    }
  } catch { /* no audio device, or autoplay not yet unlocked: never fatal */ }
}

// ---- the system surface ---------------------------------------------------

let permission: NotificationPermission | "unsupported" =
  typeof Notification === "undefined" ? "unsupported" : Notification.permission;

/** WKWebView ships no Notification API at all, so under Tauri the browser path
 *  has nothing to fall back on — the plugin is the only way to post a real OS
 *  notification from a bundled app. */
let tauriGranted: boolean | null = null;

/** Ask once, on a real user gesture. Asking at load is what gets a permission
 *  prompt denied forever before the user knows what it is for.
 *
 *  This is the polite path, not the only one: `systemNotify` asks again if a
 *  notification is due and nobody has asked yet. Relying on a stray click
 *  having happened first is how a build ends up silently never notifying.  */
export async function requestPermission(): Promise<void> {
  if (isTauri()) return void (await tauriPermission());
  if (permission !== "default") return;
  try { permission = await Notification.requestPermission(); } catch { /* denied */ }
}

async function tauriPermission(): Promise<boolean> {
  if (tauriGranted !== null) return tauriGranted;
  try {
    const n = await import("@tauri-apps/plugin-notification");
    tauriGranted = (await n.isPermissionGranted()) ||
      (await n.requestPermission()) === "granted";
  } catch { tauriGranted = false; }
  return tauriGranted;
}

const titleOf = (a: Alert) => (a.where ? `${a.nick} in ${a.where}` : a.nick);

/** Post to the OS. Returns false when nothing was shown, so the caller can fall
 *  back to a card rather than losing the message entirely. */
async function systemNotify(a: Alert, open: (channel: string) => void): Promise<boolean> {
  if (isTauri()) {
    if (!(await tauriPermission())) return false;
    try {
      const n = await import("@tauri-apps/plugin-notification");
      n.sendNotification({ title: titleOf(a), body: a.text });
      return true;
    } catch { return false; }
  }
  if (permission !== "granted") return false;
  try {
    // Tagging by channel collapses a burst into one banner per conversation
    // rather than a column of them.
    const n = new Notification(titleOf(a), { body: a.text, tag: "gliph-" + a.channel, silent: true });
    n.onclick = () => { window.focus(); open(a.channel); n.close(); };
    setTimeout(() => n.close(), 6000);
    return true;
  } catch {
    return false;
  }
}

// ---- the entry point ------------------------------------------------------

/** Show an alert. `open` is what a click should do.
 *
 *  Callers decide WHETHER something is worth announcing (a DM, a highlight, and
 *  not while you are already looking at it); this decides WHERE it goes. */
export function alert(a: Alert, open: (channel: string) => void) {
  const m = mode();
  log("info", `notify: mode=${m} sound=${soundOn()} tauri=${isTauri()}`);
  ping();
  if (m === "none") return;
  if (m === "custom") {
    // Desktop: a real window, so the card is visible with gliph buried behind
    // four other apps — which is the only time a notification matters. The
    // browser cannot have one, and falls back to the page.
    if (isTauri()) return void showInWindow(a);
    return push(a);
  }
  // The OS surface is async (permission may still need asking), so a card is
  // the fallback only once we know the OS did not take it. A denied permission
  // never means silent message loss.
  void systemNotify(a, open).then((shown) => { if (!shown) push(a); });
}

/** True when a notification is warranted at all: something arrived that is not
 *  mine, and I am not already looking straight at it. */
export function shouldAlert(opts: { mine: boolean; channelActive: boolean; focused: boolean }): boolean {
  if (opts.mine) return false;
  return !(opts.channelActive && opts.focused);
}

/** Tell the notification window the app changed theme. It is a separate
 *  document with its own copy of theme.css, so nothing else would reach it. */
export async function retheme(theme: string): Promise<void> {
  if (!isTauri()) return;
  try {
    const { emitTo } = await import("@tauri-apps/api/event");
    await emitTo(WIN, "notify-theme", { theme });
  } catch { /* the window may not exist yet; it reads the theme from its URL */ }
}

// ---- the desktop notification window --------------------------------------
//
// A frameless, transparent, always-on-top window pinned to the top-right of the
// screen the app is on. Created once and then kept alive — hidden between
// alerts, never destroyed — because on macOS it is converted to a
// non-activating NSPanel exactly once, and converting a recreated window of the
// same label would leave the plugin holding a dangling panel.

const WIN = "notify";
/** Must match WIN_W in notifywin.ts: that page auto-sizes the HEIGHT to its card
 *  stack, and assumes this width. */
const WIN_W = 380;
const WIN_H = 160;
// Breathing room from the screen edges — the window no longer hugs the corner.
const PAD = 16;

/** A notification window that silently fails to appear is the worst failure
 *  this file has: the user is told nothing, and nothing on screen says why. The
 *  webview console is not reachable in a shipped app, so complaints go to the
 *  Rust log, which is. */
async function warn(what: string): Promise<void> {
  try {
    const { invoke } = await import("@tauri-apps/api/core");
    await invoke("js_log", { level: "warn", message: `notify: ${what}` });
  } catch { /* not in Tauri, or the log itself is gone */ }
}

/** Serialise window handling. "Does it exist?" followed by an async create is
 *  not atomic: two messages arriving back to back would both see "no window
 *  yet" and both create one, leaving an orphan that outlives every dismiss. */
let chain: Promise<void> = Promise.resolve();
const showInWindow = (a: Alert) =>
  (chain = chain.catch(() => {}).then(() => openWindow(a)));

async function openWindow(a: Alert): Promise<void> {
  try {
    const [{ WebviewWindow }, { emitTo }, win, { PhysicalPosition }] = await Promise.all([
      import("@tauri-apps/api/webviewWindow"),
      import("@tauri-apps/api/event"),
      import("@tauri-apps/api/window"),
      import("@tauri-apps/api/dpi"),
    ]);

    // Which screen? The one the MAIN window is on. The free currentMonitor() is
    // unreliable with more than one display — it can answer with a monitor the
    // window is not on — so resolve it from the window's own centre point and
    // keep currentMonitor as the fallback.
    const main = win.getCurrentWindow();
    let monitor = null;
    try {
      const [pos, size] = await Promise.all([main.outerPosition(), main.outerSize()]);
      monitor = await win.monitorFromPoint(pos.x + size.width / 2, pos.y + size.height / 2);
    } catch { /* fall through */ }
    if (!monitor) monitor = await win.currentMonitor();

    // Position in PHYSICAL pixels. Logical coordinates live in one global space
    // whose scale mapping is ambiguous across monitors of different densities,
    // which is exactly what lands a popup on the wrong screen. workArea excludes
    // the menu bar and the dock.
    const scale = monitor?.scaleFactor || 1;
    const area = monitor?.workArea ?? { position: monitor?.position, size: monitor?.size };
    const x = Math.round((area?.position?.x ?? 0) + (area?.size?.width ?? 1920 * scale) - (WIN_W + PAD) * scale);
    const y = Math.round((area?.position?.y ?? 0) + PAD * scale);

    const existing = await WebviewWindow.getByLabel(WIN);
    if (existing) {
      // Reposition first: the main window may have moved to another screen.
      try {
        await existing.setPosition(new PhysicalPosition(x, y));
        await emitTo(WIN, "notify-show", a);
        await existing.show();
        log("info", `notify: reused the window at ${x},${y}, visible=${await existing.isVisible().catch(() => "?")}`);
      } catch (e) {
        // Never swallow this. A window that exists but cannot be shown swallows
        // every notification from here on, silently, which is exactly the bug
        // this whole file exists to avoid.
        void warn(`could not reuse the window: ${e}`);
      }
      return;
    }

    // First alert of the session. Created hidden so it can be placed on the
    // right screen before it appears — otherwise it flashes on the wrong one.
    // The payload rides in the URL hash rather than an event, because an event
    // emitted now would arrive before the new webview has a listener.
    // The window is its own document and inherits nothing, so the app's theme
    // travels with the payload — otherwise the first card of a midnight session
    // paints creme until something else tells it.
    const theme = document.documentElement.dataset["theme"] ?? "creme";
    const w = new WebviewWindow(WIN, {
      url: `/notify.html#a=${encodeURIComponent(JSON.stringify(a))}&t=${encodeURIComponent(theme)}`,
      title: "",
      width: WIN_W,
      height: WIN_H,
      visible: false,
      decorations: false,
      transparent: true,
      alwaysOnTop: true,
      focus: false,
      skipTaskbar: true,
      resizable: false,
      shadow: false,
    });

    // Wait for the window to exist before touching it — and do NOT depend on
    // `tauri://created` arriving to say so. It does not fire for a window
    // created this way on macOS, and waiting for it forever is how the window
    // ends up created-but-never-shown: invisible, and claimed by getByLabel on
    // every later alert, so nothing is ever seen again. The timeout is the real
    // path, not the safety net.
    await Promise.race([
      new Promise<void>((r) => w.once("tauri://created", () => r())),
      new Promise<void>((r) => {
        w.once("tauri://error", (e) => { void warn(`create failed: ${JSON.stringify(e)}`); r(); });
      }),
      new Promise<void>((r) => setTimeout(r, 400)),
    ]);
    try {
      await w.setPosition(new PhysicalPosition(x, y));
      await w.show();
      log("info", `notify: created the window at ${x},${y} on ${monitor?.name ?? "?"}`);
    } catch (e) {
      void warn(`could not place the window: ${e}`);
    }
    try {
      // macOS: become a non-activating NSPanel, so clicking a card cannot pull
      // gliph in front of whatever the user was typing in. On Windows/Linux
      // this command is a no-op — focus:false + skipTaskbar + alwaysOnTop
      // already behave that way there.
      const { invoke } = await import("@tauri-apps/api/core");
      await invoke("convert_notify_to_panel");
    } catch (e) {
      // Not fatal: the window is up, it may just take focus on a click.
      void warn(`panel conversion failed: ${e}`);
    }
  } catch (e) {
    void warn(`no window: ${e}`);
    // Better a card in the page than a lost message.
    push(a);
  }
}
