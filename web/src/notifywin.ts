/** The desktop notification window.
 *
 *  Its own webview, its own OS window: frameless, transparent, always on top,
 *  pinned to the top right of the screen the app is on, and — on macOS — a
 *  non-activating NSPanel, so clicking a card never yanks focus out of whatever
 *  the user was typing in. notify.ts creates and positions it; this file is
 *  what lives inside.
 *
 *  Ported from inbox2's NotificationPage. The three rules that make it work:
 *
 *   1. The window is PERSISTENT. Cards come and go inside it; the window itself
 *      is only hidden when the stack empties. It is converted to an NSPanel
 *      exactly once, and a destroyed-and-recreated window of the same label
 *      would leave the plugin holding a panel that no longer exists.
 *   2. The window auto-sizes to its content, watched by a ResizeObserver rather
 *      than computed — cards wrap, fonts land late, and a guessed height either
 *      clips the last card or leaves a transparent gap that still eats clicks.
 *   3. The first card arrives in the URL hash, not as an event. The window is
 *      created and the alert emitted in the same breath, and an event sent
 *      before this script has registered its listener is simply lost.
 */

import "./theme.css";
import { renderStack } from "./alertcard";
import { dismissAll, dismissChannel, onStackChange, push, visible, type Alert } from "./notify";

/** Must match WIN_W in notify.ts, which owns the window's width and position. */
const WIN_W = 380;
const PAD = 14;

const root = document.getElementById("stack")!;

/** The theme is the app's, and the app is not this window. It is written into
 *  the hash by whoever opened us and kept current by the main window. */
const params = new URLSearchParams(location.hash.slice(1));
document.documentElement.dataset["theme"] = params.get("t") ?? "creme";

// The first alert rides in the hash — see rule 3 above.
const first = params.get("a");
if (first) {
  try { push(JSON.parse(decodeURIComponent(first)) as Alert); } catch { /* malformed */ }
  // Consume it: a reload of this window (dev HMR, a webview crash) must not
  // replay a stale alert out of the hash.
  history.replaceState(null, "", `#t=${encodeURIComponent(params.get("t") ?? "creme")}`);
}

function paint() {
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- renderStack() escapes nick, text, title and channel; colours are constants
  root.innerHTML = renderStack(visible());
  root.querySelector<HTMLElement>("[data-alert-all]")
    ?.addEventListener("click", (e) => { e.stopPropagation(); dismissAll(); });
  for (const el of root.querySelectorAll<HTMLElement>("[data-alert-chan]")) {
    el.addEventListener("click", (e) => {
      if ((e.target as HTMLElement).closest("[data-alert-x]")) return;
      void open(el.dataset["alertChan"]!);
    });
  }
  for (const el of root.querySelectorAll<HTMLElement>("[data-alert-x]")) {
    el.addEventListener("click", (e) => { e.stopPropagation(); dismissChannel(el.dataset["alertX"]!); });
  }
}

/** Clicking a card is the one gesture that DOES want the app in front: raise
 *  the main window, tell it which channel, and clear the whole stack — one
 *  click deals with all of it, not just the card under the pointer. */
async function open(channel: string) {
  try {
    const { emitTo } = await import("@tauri-apps/api/event");
    await emitTo("main", "notify-open", { channel });
  } catch { /* not in Tauri */ }
  dismissAll();
}

// New alerts pushed into the window while it is already up.
void (async () => {
  const { listen } = await import("@tauri-apps/api/event");
  await listen<Alert>("notify-show", (e) => push(e.payload));
  await listen<{ theme: string }>("notify-theme", (e) => {
    document.documentElement.dataset["theme"] = e.payload.theme;
  });
})();

// Esc dismisses the newest group. The panel can never become key window, so
// this only ever fires when the user has deliberately clicked into it first.
addEventListener("keydown", (e) => {
  const top = visible()[0];
  if (e.key === "Escape" && top) dismissChannel(top.channel);
});

type Sizer = {
  win: import("@tauri-apps/api/window").Window;
  LogicalSize: typeof import("@tauri-apps/api/dpi").LogicalSize;
};
let sizer: Sizer | null = null;
let noSizer = false;

/** Grow the OS window to whatever the stack now measures, or hide it when there
 *  is nothing left. Hidden rather than closed: see rule 1. */
async function fit() {
  if (noSizer) return;
  if (!sizer) {
    try {
      const [w, d] = await Promise.all([
        import("@tauri-apps/api/window"),
        import("@tauri-apps/api/dpi"),
      ]);
      sizer = { win: w.getCurrentWindow(), LogicalSize: d.LogicalSize };
    } catch {
      noSizer = true; // a plain browser tab: there is no OS window to resize
      return;
    }
  }
  const h = root.offsetHeight;
  if (h <= 0) { await sizer.win.hide().catch(() => {}); return; }
  await sizer.win.setSize(new sizer.LogicalSize(WIN_W, h + PAD * 2)).catch(() => {});
}

// Watch the rendered stack rather than reacting to pushes: a card that wraps to
// a third line, or a font that lands after first paint, changes the height with
// no state change to hook.
new ResizeObserver(() => void fit()).observe(root);

onStackChange(() => { paint(); void fit(); });
paint();
void fit();
