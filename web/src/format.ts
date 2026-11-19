/** Message text rendering: code blocks, IRC formatting codes, links.
 *
 *  Everything here operates on text that has ALREADY been HTML-escaped. That
 *  ordering is load-bearing: escape once, at the front, then only ever add our
 *  own markup around it. A formatter that escapes late — or worse, in some
 *  branches — is how a chat client grows an XSS, and a message body is the most
 *  attacker-controlled string in the product.
 */

import { esc } from "./escape.ts";
// ---- IRC formatting codes ----

// mIRC's sixteen colours, mapped onto gliph's own palette rather than reproduced
// literally. Spec F12: colours come from the theme, so a line stays readable in
// both themes and nobody can paste white-on-white. Arbitrary hex is not a thing
// you can express here, deliberately.
const IRC_COLORS = [
  "var(--text)", "var(--chrome)", "var(--link)", "var(--join)",
  "var(--accent)", "var(--action)", "var(--action)", "var(--agent)",
  "var(--gold)", "var(--join)", "var(--mode)", "var(--link)",
  "var(--mode)", "var(--accent)", "var(--muted)", "var(--muted)",
];

const CTRL = {
  color: "\x03",
  bold: "\x02",
  italic: "\x1d",
  underline: "\x1f",
  reverse: "\x16",
  reset: "\x0f",
};

/** True when a line carries any IRC control code. Cheap enough to run on every
 *  message, and lets the common case skip the state machine entirely. */
export const hasIrcCodes = (s: string): boolean =>
  /[\x02\x03\x0f\x16\x1d\x1f]/.test(s);

/** Turn IRC control codes into spans. Operates on escaped text.
 *
 *  The open-tag stack is closed at end of line no matter what: an unterminated
 *  \x02 must not be able to bold the rest of the channel. */
function applyIrcCodes(escaped: string): string {
  let out = "";
  let open = 0;
  const push = (style: string) => { out += `<span style="${style}">`; open += 1; };
  const closeAll = () => { while (open > 0) { out += "</span>"; open -= 1; } };

  for (let i = 0; i < escaped.length; i += 1) {
    const ch = escaped[i]!;
    switch (ch) {
      case CTRL.bold: push("font-weight:800"); break;
      case CTRL.italic: push("font-style:italic"); break;
      case CTRL.underline: push("text-decoration:underline"); break;
      case CTRL.reverse: push("filter:invert(1)"); break;
      case CTRL.reset: closeAll(); break;
      case CTRL.color: {
        // \x03 with no digits is "stop colouring".
        const m = /^(\d{1,2})(?:,(\d{1,2}))?/.exec(escaped.slice(i + 1));
        if (!m) { closeAll(); break; }
        i += m[0].length;
        const fg = IRC_COLORS[Number(m[1]) % 16];
        const bg = m[2] ? IRC_COLORS[Number(m[2]) % 16] : null;
        push(`color:${fg}${bg ? `;background:${bg}` : ""}`);
        break;
      }
      default:
        out += ch;
    }
  }
  closeAll();
  return out;
}

// ---- links ----

const URL_RE = /https?:\/\/[^\s<>"']+/g;

/** Collect the URLs in a body, for the card layer to classify. */
export function urlsIn(text: string): string[] {
  return text.match(URL_RE) ?? [];
}

function linkify(escaped: string): string {
  return escaped.replace(URL_RE, (u) => {
    // Trailing punctuation almost always belongs to the sentence, not the URL.
    const trail = /[.,;:!?)\]}]+$/.exec(u);
    const href = trail ? u.slice(0, -trail[0].length) : u;
    const tail = trail ? trail[0] : "";
    // no-referrer: following a link must not tell the destination where the
    // reader came from.
    return `<a href="${href}" target="_blank" rel="noreferrer noopener">${href}</a>${tail}`;
  });
}

// ---- code blocks ----

const FENCE = /```([a-z0-9+#-]{0,20})\n?([\s\S]*?)```/gi;

/** Past this many lines a block starts folded, Discord style: the stream is a
 *  conversation, not a file viewer, and one paste must not push everyone's
 *  last five messages off the screen. */
export const FOLD_LINES = 7;

/** A short stable id for a block, so the expanded state survives repaints. */
/** Languages that are prose in a fence: those wrap like text instead of
 *  scrolling sideways, because nobody reads a paragraph through a scrollbar. */
export const wrapsLines = (lang: string) =>
  ["", "md", "markdown", "txt", "text", "plaintext", "plain"].includes(lang.toLowerCase());

export function codeId(lang: string, code: string): string {
  let h = 5381;
  const s = lang + "\0" + code;
  for (let i = 0; i < s.length; i += 1) h = ((h * 33) ^ s.charCodeAt(i)) >>> 0;
  return h.toString(36);
}

/** True when the body contains a fenced block, so the caller can pick the
 *  block-level path instead of the inline one. */
export const hasCodeBlock = (s: string): boolean => /```[\s\S]*?```/.test(s);

/** Split a body into prose and fenced code, rendering each appropriately.
 *
 *  The block comes out plain and escaped; colouring is a second pass
 *  (codepad.ts) that main.ts runs after paint, once Monaco is loaded. The
 *  `data-lang`/`data-code` attributes are what that pass keys on. */
export function renderWithCodeBlocks(text: string): string {
  let out = "";
  let last = 0;
  FENCE.lastIndex = 0;
  for (let m = FENCE.exec(text); m; m = FENCE.exec(text)) {
    out += renderInline(text.slice(last, m.index));
    const lang = m[1] || "";
    const code = (m[2] ?? "").replace(/\n$/, "");
    const lines = code.split("\n").length;
    const tall = lines > FOLD_LINES;
    // One line of markup, no indentation: the stream is `white-space:
    // pre-wrap`, so any newline in here would render as a blank line.
    out += `<div class="card code ${tall ? "tall" : ""} ${wrapsLines(lang) ? "wrap" : ""}" data-code="${codeId(lang, code)}">`
      + `<div class="head"><span class="k">${esc(lang || "code")}</span><span class="spacer"></span>`
      + (tall ? `<button data-expand>expand · ${lines} lines</button>` : "")
      + `<button data-copy>copy</button></div>`
      + `<pre><code class="monaco-editor" data-lang="${esc(lang || "plaintext")}">${esc(code)}</code></pre></div>`;
    last = m.index + m[0].length;
  }
  out += renderInline(text.slice(last));
  return out;
}

/** One run of ordinary message text: escape, then formatting codes, then links.
 *
 *  ASCII art (spec F10) needs nothing special beyond this — the escape leaves
 *  every space and box-drawing character exactly where it was, and the stream's
 *  `white-space: pre-wrap` in a monospace face keeps the columns lined up. The
 *  thing that would break it is a formatter that collapses runs of whitespace,
 *  which is why there is no such step here. */
export function renderInline(text: string): string {
  if (!text) return "";
  const escaped = mentions(inlineCode(esc(text)));
  const withCodes = hasIrcCodes(text) ? applyIrcCodes(escaped) : escaped;
  return linkify(withCodes);
}

/** @nick becomes a link into that person's DM (main.ts wires `data-nick`).
 *  Only at a word start, so an email address is not half a mention. */
function mentions(escaped: string): string {
  return escaped.replace(/(^|[\s(])@([\w-]{1,32})\b/g, '$1<a class="mention" data-nick="$2">@$2</a>');
}

/** `like this` — one line only. A backtick pair spanning lines is left alone:
 *  that is either a fence someone half-typed or two stray characters, and
 *  neither wants a whole paragraph set in mono. Operates on escaped text. */
function inlineCode(escaped: string): string {
  return escaped.replace(/`([^`\n]+)`/g, '<code class="inl">$1</code>');
}

