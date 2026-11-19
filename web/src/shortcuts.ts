/** The keyboard registry — the single source of truth for every key gliph binds.
 *
 *  Three consumers read this one list: the dispatcher in main.ts, the `?` help
 *  overlay, and (for anything that also has a button) the hint next to it. Add a
 *  shortcut here and all three pick it up. The alternative — a keydown branch in
 *  one file and a hardcoded help table in another — is how the help screen ends
 *  up lying about what the app does.
 *
 *  A shortcut with no `match` is documentation only: a key some other code owns
 *  (the composer's own input handler, the blob surface) that the overlay still
 *  has to list, because from the user's side it is the same keyboard.
 */

import { esc } from "./escape.ts";
export type Scope = "global" | "composer" | "blob";

export type Shortcut = {
  id: string;
  /** what the overlay prints */
  display: string;
  label: string;
  scope: Scope;
  /** absent = handled elsewhere, listed for discoverability only */
  match?: (e: KeyboardEvent) => boolean;
};

/** macOS Option rewrites `event.key` into a special character (⌥m is "µ"), so
 *  every alt combo has to match on the physical `event.code` instead. Bare and
 *  shifted keys match on `key`, which is what respects the user's layout. */
const alt = (code: string) => (e: KeyboardEvent) =>
  e.altKey && !e.ctrlKey && !e.metaKey && e.code === code;

const bare = (key: string) => (e: KeyboardEvent) =>
  !e.altKey && !e.ctrlKey && !e.metaKey && e.key === key;

export const SHORTCUTS: Shortcut[] = [
  // ---- global ----
  { id: "focus-composer", display: "/", label: "focus the composer and start a command", scope: "global", match: bare("/") },
  { id: "help", display: "?", label: "this list", scope: "global", match: bare("?") },
  { id: "next-channel", display: "⌥↓", label: "next channel", scope: "global", match: alt("ArrowDown") },
  { id: "prev-channel", display: "⌥↑", label: "previous channel", scope: "global", match: alt("ArrowUp") },
  // mIRC bound Alt+N to window N for thirty years. Keep it.
  { id: "channel-n", display: "⌥1…⌥9", label: "jump to channel N", scope: "global", match: (e) => e.altKey && /^Digit[1-9]$/.test(e.code) },
  { id: "toggle-rail", display: "⌥M", label: "show/hide the member rail", scope: "global", match: alt("KeyM") },
  { id: "toggle-theme", display: "⌥T", label: "switch theme", scope: "global", match: alt("KeyT") },
  { id: "channel-list", display: "⌥L", label: "browse channels (/list)", scope: "global", match: alt("KeyL") },
  { id: "undo", display: "⌘Z", label: "undo the last op action", scope: "global", match: (e) => (e.metaKey || e.ctrlKey) && !e.shiftKey && e.key.toLowerCase() === "z" },
  { id: "redo", display: "⌘⇧Z", label: "redo it", scope: "global", match: (e) => (e.metaKey || e.ctrlKey) && e.shiftKey && e.key.toLowerCase() === "z" },

  // ---- composer (owned by the input's own keydown) ----
  { id: "composer-send", display: "enter", label: "send", scope: "composer" },
  { id: "composer-accept", display: "tab", label: "accept the selected command", scope: "composer" },
  { id: "composer-nav", display: "↑ ↓", label: "move through the command palette", scope: "composer" },
  { id: "composer-dismiss", display: "esc", label: "dismiss the palette", scope: "composer" },

  // ---- blob surface (owned by the blob keyboard model, spec F16) ----
  { id: "blob-nav", display: "j k / ↑ ↓", label: "move the selection", scope: "blob" },
  { id: "blob-jump", display: "1…9", label: "jump to an item", scope: "blob" },
  { id: "blob-activate", display: "enter", label: "open the item's menu, or fire the button", scope: "blob" },
  { id: "blob-back", display: "esc", label: "close the menu, then the input, then return to chat", scope: "blob" },
];

const byId = new Map(SHORTCUTS.map((s) => [s.id, s]));

/** The hint to print beside a button that does the same thing. */
export const hint = (id: string): string => byId.get(id)?.display ?? "";

/** True while a modal owns the keyboard. Any overlay marks its root with
 *  `data-overlay-open`, so this stays one query rather than a flag that some
 *  new overlay forgets to set. */
export const overlayOpen = (): boolean => !!document.querySelector("[data-overlay-open]");

/** True while the user is typing somewhere their keystrokes belong. */
export const typing = (): boolean => {
  const el = document.activeElement;
  return el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement ||
    (el instanceof HTMLElement && el.isContentEditable);
};

/** Resolve a keydown to a shortcut id, or null. Global shortcuts never fire
 *  while typing or while an overlay is up — except `?`/esc, which the caller
 *  handles before asking us. */
export function resolve(e: KeyboardEvent, scope: Scope = "global"): string | null {
  for (const s of SHORTCUTS) {
    if (s.scope !== scope || !s.match) continue;
    if (s.match(e)) return s.id;
  }
  return null;
}

const SCOPE_TITLE: Record<Scope, string> = {
  global: "anywhere",
  composer: "in the composer",
  blob: "in a blob",
};


/** The `?` overlay, rendered straight from the registry so it can never drift
 *  from what the dispatcher actually binds. */
export function renderHelp(): string {
  const group = (scope: Scope) => {
    const rows = SHORTCUTS.filter((s) => s.scope === scope);
    if (!rows.length) return "";
    return `<div class="boxtitle">${SCOPE_TITLE[scope]}</div>
      <table class="keys">${rows.map((s) =>
        `<tr><td class="k">${esc(s.display)}</td><td>${esc(s.label)}</td></tr>`).join("")}</table>`;
  };
  return `<div class="overlay" data-overlay-open data-act="closehelp">
    <div class="panel" data-stop>
      <h2 class="serif">keyboard</h2>
      ${group("global")}${group("composer")}${group("blob")}
      <div class="foot">esc or ? to close</div>
    </div>
  </div>`;
}
