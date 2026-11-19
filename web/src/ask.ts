/** A yes/no in the app's own overlay rather than window.confirm, which the
 *  desktop shell renders as a bare OS alert. The question is the title, the
 *  consequence the body, the verb the button. Resolves true only on the verb:
 *  esc, the backdrop and cancel are all "no", and a stray key is nothing.
 *
 *  data-overlay-open stands the shortcut dispatcher down (shortcuts.ts). Only
 *  one is ever up: a second ask while one is open answers the first "no". */
import { esc } from "./escape.ts";

let open: ((ok: boolean) => void) | null = null;

export function ask(title: string, body: string, verb = "ok"): Promise<boolean> {
  open?.(false);
  const root = document.createElement("div");
  root.className = "overlay ask";
  root.setAttribute("data-overlay-open", "");
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- title and body are esc()'d
  root.innerHTML = `<div class="panel" role="alertdialog" aria-labelledby="ask-t">
    <h2 id="ask-t">${esc(title)}</h2>
    ${body ? `<p class="body">${esc(body)}</p>` : ""}
    <div class="row">
      <button data-ok="0">cancel</button>
      <button class="go" data-ok="1" autofocus>${esc(verb)}</button>
    </div>
  </div>`;
  document.body.append(root);
  root.querySelector<HTMLButtonElement>(".go")!.focus();
  return new Promise((resolve) => {
    const done = (ok: boolean) => { open = null; root.remove(); document.removeEventListener("keydown", key, true); resolve(ok); };
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape") { e.preventDefault(); done(false); }
      if (e.key === "Enter") { e.preventDefault(); done(true); }
      e.stopPropagation();
    };
    open = done;
    document.addEventListener("keydown", key, true);
    root.addEventListener("click", (e) => {
      const b = (e.target as HTMLElement).closest<HTMLElement>("[data-ok]");
      if (b) return done(b.dataset.ok === "1");
      if (e.target === root) done(false);
    });
  });
}
