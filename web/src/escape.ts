/** HTML escaping. One implementation, for the whole client.
 *
 *  This existed eight times — in format.ts, ui.ts, gum.ts, admin.ts, cards.ts,
 *  link.ts and shortcuts.ts — in two variants that disagreed about the
 *  apostrophe. Four of the copies escaped `'` and four did not, which was safe
 *  only because every template in the codebase happened to use double-quoted
 *  attributes. That is an invariant nobody was checking, held across seven
 *  files, guarding the most attacker-controlled string in the product: a chat
 *  message somebody else wrote.
 *
 *  So: one function, the strict variant, imported everywhere. `'` is escaped
 *  whether or not the caller's attribute quoting needs it, because the next
 *  template is not obliged to remember which one this was.
 *
 *  Imported as "./escape.ts", WITH the extension: scripts/check-render.mjs runs
 *  format.ts and cards.ts through node --experimental-strip-types so the
 *  escaping asserts test the shipping code rather than a copy, and node needs a
 *  real filename. tsc needs allowImportingTsExtensions for that (set in
 *  tsconfig.json); vite resolves it either way.
 */

const MAP: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

/** Escape for insertion into HTML text or a quoted attribute.
 *
 *  Takes `unknown` and coerces: two of the call sites it replaced were
 *  interpolating numbers and possibly-null API fields, and `esc(undefined)`
 *  printing "undefined" is better than it throwing halfway through a render. */
export const esc = (s: unknown): string =>
  String(s ?? "").replace(/[&<>"']/g, (c) => MAP[c]!);
