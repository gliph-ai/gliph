/** The code composer and the code colouring in the stream, both on Monaco.
 *
 *  Monaco is a few megabytes, so it is never in the first paint: `import()`
 *  fetches it the first time someone types ``` or a code block scrolls into
 *  view, and everything before that renders as a plain <pre>. The popup lives
 *  outside #app on purpose — paint() rebuilds #app's innerHTML on every event,
 *  and an editor that is torn down mid-keystroke is not an editor. */

import type * as Monaco from "monaco-editor/editor/editor.api.js";
import { icons } from "./icons";

let monacoP: Promise<typeof Monaco> | null = null;

export function loadMonaco(): Promise<typeof Monaco> {
  monacoP ??= (async () => {
    // The editor core plus the syntax grammars for every language, and not
    // the language *services* (TypeScript, CSS, HTML, JSON): those are a
    // 6 MB TypeScript compiler and friends, all so a pasted snippet could get
    // red squiggles nobody asked for.
    const [m, { default: EditorWorker }] = await Promise.all([
      import("monaco-editor/editor/editor.api.js"),
      import("monaco-editor/editor/editor.worker.js?worker"),
    ]);
    await import("monaco-editor/basic-languages/monaco.contribution.js");
    // One worker for every language: the per-language ones only add
    // diagnostics, and nobody wants TypeScript errors on a pasted snippet.
    (self as unknown as { MonacoEnvironment: unknown }).MonacoEnvironment = { getWorker: () => new EditorWorker() };
    // Code is dark in both app themes (the --code-* tokens in theme.css are
    // the same colours), so one Monaco theme and nothing to swap on retheme.
    m.editor.defineTheme("gliph-code", {
      base: "vs-dark", inherit: true, rules: [],
      colors: { "editor.background": "#171d33", "editor.foreground": "#e2ddcd", "editor.lineHighlightBackground": "#212843" },
    });
    m.editor.setTheme("gliph-code");
    return m;
  })();
  return monacoP;
}

// ---- the stream ----

const colored = new Map<string, string>();

/** Colourise one block. Resolves with HTML that is only valid inside an
 *  element carrying the `monaco-editor` class, which is where the theme's
 *  token styles are scoped. */
export async function colorize(id: string, code: string, lang: string): Promise<string> {
  const hit = colored.get(id);
  if (hit) return hit;
  const m = await loadMonaco();
  const html = await m.editor.colorize(code, lang, { tabSize: 2 });
  colored.set(id, html);
  return html;
}

// ---- the popup ----

let pad: { root: HTMLElement; editor: Monaco.editor.IStandaloneCodeEditor; lang: string } | null = null;

const PROSE = ["markdown", "plaintext"];
const langIcon = (id: string) => (PROSE.includes(id) ? icons.doc() : icons.code());

/** Open the editor. `onSend` gets the language id and the text; either
 *  button or ⌘/ctrl-enter sends, esc cancels. Only one is ever open. */
export async function openCodepad(onSend: (lang: string, code: string) => void,
  opts: { value?: string; lang?: string; verb?: string } = {}) {
  if (pad) return pad.editor.focus();
  const m = await loadMonaco();

  const root = document.createElement("div");
  root.className = "overlay codepad";
  root.setAttribute("data-overlay-open", "");
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- static template and a constant icon
  root.innerHTML = `<div class="panel">
    <div class="bar">
      <button class="lang" title="language"><span class="i"></span><span class="name"></span>${icons.chevron()}</button>
      <div class="langs" hidden><input placeholder="language" spellcheck="false"><div class="rows"></div></div>
      <span class="spacer"></span>
      <button data-act="cancel">cancel</button>
      <button class="send" data-act="send"><span class="verb"></span> <kbd>⌘↩</kbd></button>
    </div>
    <div class="editor"></div>
  </div>`;
  const host = root.querySelector<HTMLElement>(".editor")!;
  document.body.append(root);

  const editor = m.editor.create(host, {
    value: opts.value ?? "", language: "markdown", theme: "gliph-code",
    fontFamily: "JetBrains Mono, ui-monospace, Menlo, monospace", fontSize: 13,
    minimap: { enabled: false }, lineNumbers: "on", wordWrap: "on",
    scrollBeyondLastLine: false, automaticLayout: true, tabSize: 2,
    renderLineHighlight: "line", padding: { top: 8, bottom: 8 },
    // No overview ruler: its cursor mark is a stray bar beside the scrollbar.
    overviewRulerLanes: 0, hideCursorInOverviewRuler: true, overviewRulerBorder: false,
  });
  pad = { root, editor, lang: "markdown" };
  // A handle for scripted tests (the render check drives it from the console).
  (root as HTMLElement & { editor?: Monaco.editor.IStandaloneCodeEditor }).editor = editor;

  // The language picker: a button and a filterable list in the palette's
  // style, instead of the browser's own <select>.
  const langs = m.languages.getLanguages().map((l) => l.id).sort();
  const trigger = root.querySelector<HTMLElement>(".lang")!;
  const list = root.querySelector<HTMLElement>(".langs")!;
  const filter = list.querySelector("input")!;
  const rows = list.querySelector<HTMLElement>(".rows")!;
  const setLang = (id: string) => {
    pad!.lang = id;
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- constant icon SVG
    trigger.querySelector(".i")!.innerHTML = langIcon(id);
    trigger.querySelector(".name")!.textContent = id;
    const model = editor.getModel();
    if (model) m.editor.setModelLanguage(model, id);
  };
  const fill = () => {
    const q = filter.value.trim().toLowerCase();
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- language ids from Monaco's own registry, icons are constants
    rows.innerHTML = langs.filter((id) => id.includes(q))
      .map((id) => `<div class="row ${id === pad?.lang ? "sel" : ""}" data-lang="${id}">${langIcon(id)}<span>${id}</span></div>`).join("");
  };
  const openList = () => { list.hidden = false; filter.value = ""; fill(); filter.focus(); };
  const closeList = () => { list.hidden = true; editor.focus(); };
  trigger.addEventListener("click", () => (list.hidden ? openList() : closeList()));
  filter.addEventListener("input", fill);
  filter.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.stopPropagation(); closeList(); }
    if (e.key === "Enter") { const first = rows.querySelector<HTMLElement>(".row"); if (first) { setLang(first.dataset["lang"]!); closeList(); } }
  });
  rows.addEventListener("click", (e) => {
    const row = (e.target as HTMLElement).closest<HTMLElement>(".row");
    if (row) { setLang(row.dataset["lang"]!); closeList(); }
  });
  setLang(opts.lang ?? "markdown");
  root.querySelector(".verb")!.textContent = opts.verb ?? "send";
  const close = () => { editor.dispose(); root.remove(); pad = null; };
  const send = () => {
    const code = editor.getValue().replace(/\s+$/, "");
    const lang = pad!.lang;
    close();
    if (code) onSend(lang, code);
  };
  root.querySelector('[data-act="cancel"]')!.addEventListener("click", close);
  root.querySelector('[data-act="send"]')!.addEventListener("click", send);
  editor.addCommand(m.KeyMod.CtrlCmd | m.KeyCode.Enter, send);
  editor.addCommand(m.KeyCode.Escape, close);
  root.addEventListener("click", (e) => {
    if (e.target === root) close();
    else if (!list.hidden && !(e.target as HTMLElement).closest(".lang, .langs")) closeList();
  });
  editor.focus();
}

export const codepadOpen = () => pad !== null;
