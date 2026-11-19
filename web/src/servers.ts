/** The server switcher: who you are, where, and what is waiting elsewhere.
 *
 *  A Gliph server IS a team — its own database, its own accounts, its own
 *  #general — so switching teams is switching servers, the way mIRC switched
 *  networks. Your identity is per-server too: <waseem> on one, <wsm> on
 *  another, because those are two rows in two different databases.
 *
 *  The rules (one spelling per address, which server is on screen, what the
 *  bracket says) all live in core/src/servers.rs. This file only draws them.
 */

import type { Client, ServerEntry } from "./client";
import { esc } from "./escape";
import { openMenu, type Item } from "./menu";

/** The theme's accent tokens, in the order a server picks one. Deterministic
 *  from the address, so a server keeps its colour on every device without
 *  anyone configuring anything, and follows the theme (midnight included). */
const INK: readonly [string, ...string[]] = ["var(--link)", "var(--agent)", "var(--join)", "var(--action)", "var(--gold)", "var(--accent)", "var(--teal)"];

export function serverInk(host: string): string {
  let h = 0;
  for (let i = 0; i < host.length; i++) h = (h * 31 + host.charCodeAt(i)) >>> 0;
  return INK[h % INK.length] ?? INK[0];
}

/** Two letters for a server with no icon of its own. */
export function monogram(e: ServerEntry): string {
  const src = (e.name || e.host.replace(/^https?:\/\//, "")).replace(/[^a-z0-9]/gi, "");
  return (src.slice(0, 2) || "??").toUpperCase();
}

/** The avatar: the server's own icon when it has uploaded one, its monogram
 *  when it has not. */
function avatar(e: ServerEntry, px: number): string {
  const style = `width:${px}px;height:${px}px;background:${serverInk(e.host)}`;
  return e.icon
    ? `<span class="av" style="${style}"><img src="${esc(e.icon)}" alt=""></span>`
    : `<span class="av" style="${style}">${esc(monogram(e))}</span>`;
}

/** An unread count in mIRC brackets. Unread, never mentions: in a sealed
 *  channel the body is ciphertext to the server, so a count is the most it can
 *  honestly report about a room it cannot read. */
const bracket = (n: number) => (n > 0 ? `<span class="ct">[${n > 99 ? "99+" : n}]</span>` : "");

/** The strip that lives in the menubar: where you are, then one avatar per
 *  other server. */
export function renderServerBar(c: Client): string {
  const { list, active } = c.servers();
  const here = list.find((e) => e.host === active);
  const nick = c.user?.nick ?? here?.nick ?? "";
  const name = c.serverName || here?.name || "gliph";
  const others = list.filter((e) => e.host !== active);

  // The avatar row appears at the SECOND server. One server means the chip
  // alone — no strip, no divider, nothing to explain until there is somewhere
  // to switch to.
  const rest = others.length
    ? `<span class="sep"></span>` +
      others
        .map(
          (e) =>
            `<button class="srv" data-server="${esc(e.host)}"` +
            ` title="${esc(e.name || e.host)} — ${e.unread > 0 ? `${e.unread} waiting` : "nothing waiting"}` +
            `${e.nick ? ` · you are <${esc(e.nick)}>` : ""}">` +
            `${avatar(e, 22)}${bracket(e.unread)}</button>`,
        )
        .join("") +
      `<button class="srv add" data-act="addserver" title="add a server">+</button>`
    : "";

  return `<span class="serverbar">
    <button class="me" data-act="servers" title="${esc(nick)} on ${esc(name)} — switch server">
      <span class="live"></span>
      <span class="who hide-sm">${esc(nick)} <i>@</i> ${esc(name)}</span>
      <span class="who only-sm">${esc(monogram({ ...(here ?? ({} as ServerEntry)), name }))}</span>
      ${bracket(c.unreadHere)}
    </button>${rest}
  </span>`;
}

/** The full list, with hosts, your nick on each, and the ways in and out. */
export function openServerMenu(c: Client, x: number, y: number, add: () => void) {
  const { list, active } = c.servers();
  const items: Item[] = [];

  const me = c.user?.nick ?? "";
  items.push({
    label: me,
    html: `<span class="acct"><b>${esc(me)}</b><span>${esc(c.user?.id ?? "")}</span></span>`,
  });

  for (const e of list) {
    const on = e.host === active;
    items.push({
      label: e.name || e.host,
      html:
        `<span class="srvrow${on ? " on" : ""}">${avatar(e, 26)}` +
        `<span class="txt"><b>${esc(e.name || e.host)}</b>` +
        `<span>${esc(e.host.replace(/^https?:\/\//, ""))}${e.nick ? ` · &lt;${esc(e.nick)}&gt;` : ""}</span></span>` +
        (on ? `<span class="ct here">here</span>` : bracket(e.unread)) +
        `</span>`,
      run: () => c.switchTo(e.host),
    });
  }

  items.push({ label: "+ add a server", run: add });
  return openMenu(x, y, items);
}
