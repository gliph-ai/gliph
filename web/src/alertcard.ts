/** The notification stream, and the one window it lives in.
 *
 *  Shared on purpose. The desktop app renders this into a separate always-on-
 *  top OS window (notifywin.ts) and the browser renders it into the page
 *  itself (main.ts) — same markup, same CSS, so the two surfaces cannot drift
 *  into looking like two different products.
 *
 *  One window, a message stream: lines grouped by channel, newest group at the
 *  BOTTOM (the window grows downward from the top-right corner, so the newest
 *  group is the one that just moved). Each line fades out as its time drains;
 *  the countdown line beside a group's title is what its newest line has left.
 *  When the last line fades, the stream is empty and the window goes with it.
 */

import { esc } from "./escape.ts";
import { type Entry, msLeft } from "./notify";

const clock = (at: number) => {
  const d = new Date(at);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
};

/** Stable per-nick colour, mIRC style — the stream speaks the app's language.
 *  ui.ts imports this rather than keeping its own copy. */
export const nickColor = (nick: string) => {
  // Ten hues, same list and hash as mobile/Sources/Theme.swift so a nick is
  // one colour on every device.
  const palette = ["--accent", "--link", "--join", "--action", "--agent", "--gold", "--azure", "--teal", "--olive", "--magenta"];
  let h = 0;
  for (let i = 0; i < nick.length; i++) h = (h * 31 + nick.charCodeAt(i)) & 0x7fffffff;
  return `var(${palette[h % palette.length]})`;
};

/** Ids this page has already drawn. A line animates IN only on its first
 *  render — both surfaces repaint the whole stream on every change, and
 *  replaying the entrance on every repaint would make the window twitch. */
const seen = new Set<string>();

const line = (e: Entry): string =>
  `<div class="aline${seen.has(e.id) ? "" : " in"}" style="--ms:${msLeft(e)}ms">` +
  `<span class="t">[${clock(e.at)}]</span>` +
  `<span class="b"><span class="anick" style="color:${nickColor(e.nick)}">&lt;${esc(e.nick)}&gt;</span> ${esc(e.text)}</span>` +
  `</div>`;

/** `lines` is oldest-first. The header carries the group's colour, its
 *  countdown line, and the newest line's clock. */
function group(lines: Entry[]): string {
  const newest = lines[lines.length - 1]!;
  const title = newest.where || `${newest.nick} — direct`;
  return `
  <div class="agroup" style="--ev:${newest.color};--ms:${msLeft(newest)}ms" data-alert-chan="${esc(newest.channel)}">
    <div class="ahead">
      <span class="achan">${esc(title)}</span>
      <span class="adrain"><span></span></span>
      <span class="atime">${clock(newest.at)}</span>
      <button class="x" data-alert-x="${esc(newest.channel)}" title="dismiss">×</button>
    </div>
    ${lines.map(line).join("")}
  </div>`;
}

/** `stack` is newest-first. Grouped by channel; groups ordered so the one with
 *  the newest line lands at the bottom. */
export function renderStack(stack: Entry[]): string {
  if (!stack.length) { seen.clear(); return ""; }
  const byChan = new Map<string, Entry[]>();
  for (let i = stack.length - 1; i >= 0; i--) {
    const e = stack[i]!;
    const g = byChan.get(e.channel);
    if (g) g.push(e); else byChan.set(e.channel, [e]);
  }
  const ordered = [...byChan.values()]
    .sort((a, b) => a[a.length - 1]!.at - b[b.length - 1]!.at);
  const html = `<div class="alerts"><div class="ahdr">` +
    `<span class="serif">gliph</span><span class="asub">new messages</span>` +
    `<button class="x" data-alert-all title="dismiss all">×</button></div>` +
    ordered.map(group).join("") + `</div>`;
  for (const e of stack) seen.add(e.id);
  return html;
}
