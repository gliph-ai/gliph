// GUM renderer: turns the core's surface tree into DOM. That is all it does.
//
// The document model, the focus order, the selection, the open menu, the input
// drafts and the whole keyboard model live in core/src/gum.rs, where the iOS
// app shares them. What arrives here is a flat list of nodes with "this row is
// selected", "this one is number 3", "this one has a submenu" already decided —
// so a frontend cannot disagree with another frontend about what enter does.
//
// Design: docs/plans/gliph-apps.md.

import { icons } from "./icons";
import { esc } from "./escape.ts";

export type Node =
  | { kind: "Text"; body: string; style: string }
  | { kind: "Divider"; label: string }
  | { kind: "Row"; index: number; number: number; label: string; hint: string;
      state: string; has_actions: boolean; selected: boolean }
  | { kind: "Table"; cols: string[]; rows: string[][] }
  | { kind: "Input"; index: number; label: string; placeholder: string;
      value: string; selected: boolean }
  | { kind: "Buttons"; buttons: { index: number; label: string; selected: boolean }[] }
  | { kind: "Bar"; body: string };

export type MenuEntry = { id: string; label: string; selected: boolean };
export type BlobSpec = { id: string; title: string; mode: string; source: string };

export type Surface = {
  blob: string;
  title: string;
  loaded: boolean;
  nodes: Node[];
  menu: MenuEntry[] | null;
  blobs: BlobSpec[];
};


const URL_RE = /https?:\/\/[^\s]+/g;

/** Bodies are plain text. Linkify, escape everything else. The server never
 *  fetches a URL on the user's behalf; unfurling stays client-side. */
const linkify = (s: string) =>
  esc(s).replace(URL_RE, (u) => `<a href="${esc(u)}" target="_blank" rel="noopener noreferrer">${esc(u)}</a>`);

const stateIcon = (state: string) => {
  switch (state) {
    case "ok": return `<span class="gs ok">${icons.dot(true, 8)}</span>`;
    case "warn": return `<span class="gs warn">${icons.dot(true, 8)}</span>`;
    case "error": return `<span class="gs err">${icons.dot(true, 8)}</span>`;
    case "done": return `<span class="gs ok">${icons.check(11)}</span>`;
    case "open": return `<span class="gs dim">${icons.dot(false, 8)}</span>`;
    default: return "";
  }
};

export function renderDoc(s: Surface | null): string {
  if (!s || !s.loaded) return `<div class="gum"><div class="gtext dim">loading…</div></div>`;
  const parts: string[] = [];

  for (const n of s.nodes) {
    switch (n.kind) {
      case "Text":
        parts.push(`<div class="gtext ${esc(n.style)}">${linkify(n.body)}</div>`);
        break;

      case "Divider":
        parts.push(`<div class="gdiv">${esc(n.label)}</div>`);
        break;

      case "Row":
        parts.push(
          `<div class="grow ${n.selected ? "sel" : ""}" data-focus="${n.index}">` +
            `<span class="gnum">${n.number || ""}</span>` +
            stateIcon(n.state) +
            `<span class="glabel">${linkify(n.label)}</span>` +
            (n.hint ? `<span class="ghint">${linkify(n.hint)}</span>` : "") +
            (n.has_actions ? `<span class="gmore">›</span>` : "") +
            `</div>`,
        );
        break;

      case "Table":
        parts.push(
          `<div class="gtable"><table><thead><tr>` +
            n.cols.map((c) => `<th>${esc(c)}</th>`).join("") +
            `</tr></thead><tbody>` +
            n.rows.map((r) => `<tr>${r.map((c) => `<td>${linkify(c)}</td>`).join("")}</tr>`).join("") +
            `</tbody></table></div>`,
        );
        break;

      case "Input":
        parts.push(
          `<div class="ginput ${n.selected ? "sel" : ""}" data-focus="${n.index}">` +
            (n.label ? `<span class="glabel">${esc(n.label)}</span>` : "") +
            `<input id="gum-input" placeholder="${esc(n.placeholder)}"` +
            ` value="${esc(n.value)}" autocomplete="off">` +
            `</div>`,
        );
        break;

      case "Buttons":
        parts.push(
          `<div class="gbtns">` +
            n.buttons
              .map((b) =>
                `<button class="gbtn ${b.selected ? "sel" : ""}" data-focus="${b.index}">${esc(b.label)}</button>`)
              .join("") +
            `</div>`,
        );
        break;

      case "Bar":
        parts.push(`<div class="gbar">${esc(n.body)}</div>`);
        break;
    }
  }

  // The scrim is the only way off this menu on a touch device: esc closes it,
  // and a phone has no esc. Without it the only exits were the actions
  // themselves — every one of which changes something.
  const menu = s.menu
    ? `<div class="scrim menuscrim" data-act="closemenu"></div>` +
      `<div class="gmenu">${s.menu
        .map((a) =>
          `<div class="grow ${a.selected ? "sel" : ""}" data-menu="${esc(a.id)}">` +
          `<span class="glabel">${esc(a.label)}</span></div>`)
        .join("")}</div>`
    : "";

  return `<div class="gum">${parts.join("")}${menu}</div>`;
}

/** The switcher lives inline at the right of the channel's title strip — no tab
 *  row, no chrome. Chat first, then this channel's blobs, marked with the apps
 *  glyph so it never reads as another list of channels. */
export function renderSwitcher(blobs: BlobSpec[], active: string): string {
  if (!blobs.length) return "";
  const item = (id: string, label: string, src?: string) =>
    `<button class="gpick ${active === id ? "sel" : ""}" data-blob="${esc(id)}"` +
    (src && src !== "builtin" ? ` title="from ${esc(src)}"` : "") +
    `>${esc(label)}</button>`;
  return (
    `<span class="gswitch">` +
    `<span class="gmark" title="apps in this channel">${icons.apps(11)}</span>` +
    item("", "chat") +
    // room0 is not a blob — it opens over the app rather than in the stream —
    // but it belongs in the same row of places this channel can be.
    `<button class="gpick" data-act="room0" title="the channel's 3D office">room0</button>` +
    blobs.map((b) => item(b.id, b.title, b.source)).join("") +
    `</span>`
  );
}
