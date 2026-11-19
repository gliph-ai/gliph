/** The right-click menu.
 *
 *  It lives on <body> rather than in the app's markup, because the app
 *  re-renders its whole DOM on every arriving message and a menu that vanishes
 *  under the pointer because someone else said hello is worse than no menu.
 *  Same reason it does not close on scroll: the stream scrolls itself on every
 *  arrival, and the item it acts on is captured when the menu opens, not read
 *  back off the row underneath. Click outside, or Escape.
 */

export type Item = {
  label: string;
  /** Omitted for a heading or a separator: it renders as a div, not a button. */
  run?: () => void;
  /** Pre-escaped markup, for rows that are more than a word — the server
   *  switcher's avatar, host and unread bracket. Caller escapes. */
  html?: string;
};

let open: (() => void) | null = null;

export function closeMenu() {
  open?.();
  open = null;
}

export function openMenu(x: number, y: number, items: Item[]) {
  closeMenu();
  // Whatever opened the menu gets focus back when it closes, if the re-render
  // has not replaced it in the meantime.
  const opener = document.activeElement as HTMLElement | null;
  const el = document.createElement("div");
  el.className = "ctxmenu";
  el.setAttribute("role", "menu");
  for (const it of items) {
    const { run, html } = it;
    const b = document.createElement(run ? "button" : "div");
    if (html) b.innerHTML = html;
    else b.textContent = it.label;
    if (!run) b.className = "head";
    else {
      b.setAttribute("role", "menuitem");
      // click, not mousedown: the away-listener below ignores anything inside
      // the menu, so the button survives long enough to be clicked properly.
      b.addEventListener("click", () => { closeMenu(); run(); });
    }
    el.append(b);
  }
  document.body.append(el);
  // Measured, then placed: a menu opened near the right or bottom edge would
  // otherwise open off-screen.
  el.style.left = `${Math.max(4, Math.min(x, innerWidth - el.offsetWidth - 4))}px`;
  el.style.top = `${Math.max(4, Math.min(y, innerHeight - el.offsetHeight - 4))}px`;

  // Keyboard: focus starts on the first item, the arrows walk the items and
  // Escape closes. Tab stays native, inside the menu's buttons.
  const buttons = [...el.querySelectorAll<HTMLButtonElement>("button")];
  buttons[0]?.focus();
  const away = (e: Event) => { if (!el.contains(e.target as Node)) closeMenu(); };
  const key = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      closeMenu();
    } else if ((e.key === "ArrowDown" || e.key === "ArrowUp") && buttons.length) {
      e.preventDefault();
      const i = buttons.indexOf(document.activeElement as HTMLButtonElement);
      const step = e.key === "ArrowDown" ? 1 : -1;
      buttons[(i + step + buttons.length) % buttons.length]?.focus();
    }
  };
  document.addEventListener("mousedown", away, true);
  document.addEventListener("keydown", key, true);
  open = () => {
    document.removeEventListener("mousedown", away, true);
    document.removeEventListener("keydown", key, true);
    const hadFocus = el.contains(document.activeElement);
    el.remove();
    if (hadFocus && opener?.isConnected) opener.focus();
  };
}
