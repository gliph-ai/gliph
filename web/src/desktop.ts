// Desktop-shell glue. Inert in a browser and on mobile: every function checks
// isTauri() first, so the same bundle ships to all three platforms.
//
// The approach here (root data-attributes for CSS to branch on, programmatic
// dragging instead of data-tauri-drag-region) follows what the inbox2 desktop
// app settled on after hitting the WKWebView bugs described below.

export const isTauri = () =>
  typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;

export function getOS(): "macos" | "windows" | "linux" {
  const nav = navigator as Navigator & { userAgentData?: { platform?: string } };
  const platform = nav.userAgentData?.platform || navigator.platform || "";
  if (/mac|iphone|ipad/i.test(platform)) return "macos";
  if (/win/i.test(platform)) return "windows";
  return "linux";
}

/** The server's installer page. Only meaningful outside the desktop app: in a
 *  browser this is the pitch. */
export const downloadsURL = (server: string, os?: "mac" | "windows" | "linux") =>
  new URL(os ? `/downloads/desktop/latest/${os}` : "/downloads/desktop/", server || location.href).href;

/** Drag the window from a chrome element. Used as a mousedown handler.
 *  data-tauri-drag-region is unreliable in WKWebView across multiple displays,
 *  so drive it programmatically instead. */
export function startDrag(e: MouseEvent) {
  if (e.button !== 0 || !isTauri()) return;
  // Never start a drag from a control. Besides being the correct behaviour,
  // start_dragging on a control click panics tao: the async IPC runs after the
  // click resolves, so [NSApp currentEvent] is nil.
  const t = e.target as HTMLElement | null;
  if (t?.closest?.("button, a, input, select, textarea, [role='button']")) return;
  import("@tauri-apps/api/window").then(({ getCurrentWindow }) =>
    getCurrentWindow().startDragging(),
  );
}

/** Open a link in the real browser rather than inside the app's webview. */
export async function openExternal(url: string) {
  const trimmed = (url || "").trim();
  if (!trimmed || trimmed.startsWith("#")) return;
  if (trimmed.toLowerCase().startsWith("javascript:")) return;
  if (isTauri()) {
    const { openUrl } = await import("@tauri-apps/plugin-opener");
    await openUrl(trimmed);
    return;
  }
  const a = document.createElement("a");
  a.href = trimmed;
  a.target = "_blank";
  a.rel = "noopener noreferrer";
  a.click();
}

type LogLevel = "error" | "warn" | "info" | "debug";

/** Log somewhere that survives a crash. Under Tauri the webview's console is
 *  gone the moment the app is launched from Finder, so route through the
 *  `js_log` command into the Rust log file; in a browser, console is all there
 *  is. Never rejects — logging must not be able to break a caller. */
export function log(level: LogLevel, msg: string) {
  if (isTauri()) {
    import("@tauri-apps/api/core")
      .then(({ invoke }) => invoke("js_log", { level, message: msg }))
      .catch(() => {});
    return;
  }
  console[level === "debug" ? "log" : level](msg);
}

/** Called once at boot. Tags the document so CSS can size the chrome around
 *  the native traffic lights, and picks up links the Rust side cancelled.
 *
 *  `onCloseTab` receives the rebound ⌘W; it does nothing until there is
 *  something tab-shaped to close. ⌘Q is the native Quit item. */
export function initDesktop(opts: { onCloseTab?: () => void } = {}) {
  const root = document.documentElement;
  // Outside Tauri too: the scrollbar styling keys off it, and a browser on
  // Windows draws the same in-layout scrollbar the desktop app does.
  root.setAttribute("data-os", getOS());
  if (!isTauri()) return;
  root.setAttribute("data-tauri", "");

  import("@tauri-apps/api/event").then(({ listen }) => {
    listen<string>("open-external-link", (e) => {
      if (e.payload) void openExternal(e.payload);
    });
    listen("menu:close-tab", () => opts.onCloseTab?.());
  });

  // The traffic lights disappear in fullscreen, so the chrome inset has to go
  // with them.
  import("@tauri-apps/api/window").then(({ getCurrentWindow }) => {
    const win = getCurrentWindow();
    const sync = () =>
      win.isFullscreen().then((fs) => root.toggleAttribute("data-tauri-fullscreen", fs));
    void sync();
    void win.onResized(sync);
  });
}

/** Start the "link this device" flow (spec F2 / server/devicelink.go).
 *
 *  The whole exchange runs in Rust because a bundled app's webview is served
 *  from a custom scheme, so there is no origin the system browser could redirect
 *  back to — the callback has to be a real loopback socket, which only the
 *  native side can bind. Resolves with a session token; rejects if the user
 *  declined or wandered off.
 */
export async function linkDevice(server: string): Promise<string> {
  if (!isTauri()) throw new Error("device linking is a desktop feature");
  const { invoke } = await import("@tauri-apps/api/core");
  return await invoke<string>("start_device_link", { server, web: server });
}

/** The in-app updater (desktop/src-tauri/src/updater.rs).
 *
 *  The Rust side polls the manifest and emits `update://available` with the
 *  version it found; `installUpdate` downloads, installs and relaunches, calling
 *  `onProgress` with a 0..1 fraction on the way (null when the server sent no
 *  length). Inert outside the desktop app. */
export function onUpdateAvailable(cb: (version: string) => void) {
  if (!isTauri()) return;
  import("@tauri-apps/api/event").then(({ listen }) => {
    listen<string>("update://available", (e) => cb(e.payload));
  });
}

export async function installUpdate(onProgress: (fraction: number | null) => void) {
  const { invoke } = await import("@tauri-apps/api/core");
  const { listen } = await import("@tauri-apps/api/event");
  // One paint per frame, not one per 16 KB chunk: the download is a few
  // hundred chunks in a second or two, and a repaint each left the menubar
  // showing "35%" when the app was already restarting.
  let latest: number | null = null;
  let queued = false;
  const stop = await listen<[number, number | null]>("update://progress", (e) => {
    const [done, total] = e.payload;
    latest = total ? Math.min(1, done / total) : null;
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => { queued = false; onProgress(latest); });
  });
  try {
    await invoke("install_update");
  } finally {
    stop();
  }
}

/** Ask the updater right now. Resolves with the waiting version, or null. */
export async function checkForUpdate(): Promise<string | null> {
  const { invoke } = await import("@tauri-apps/api/core");
  return await invoke<string | null>("check_for_updates");
}
