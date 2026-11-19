/** Installed custom themes (F12). The core checks a theme file once
 *  (core/src/client/theme.rs) and hands over name, base and tokens; this keeps
 *  them per device beside the creme/midnight choice and applies one as inline
 *  custom properties over its base. A token the theme leaves out is the base
 *  theme's, because theme.css still applies underneath. */

export type CustomTheme = { name: string; base: "creme" | "midnight"; tokens: Record<string, string> };

const LS_THEMES = "gliph.themes";
const LS_ON = "gliph.theme.custom";

const read = <T>(k: string, d: T): T => {
  try { return (JSON.parse(localStorage.getItem(k) ?? "") as T) ?? d; } catch { return d; }
};

export const installed = (): Record<string, CustomTheme> => read(LS_THEMES, {});
export const activeCustom = (): string => localStorage.getItem(LS_ON) ?? "";

let applied: string[] = [];

/** Put a custom theme on screen, or take it off with null. Returns its base so
 *  the caller can set data-theme to match. */
export function applyCustom(name: string | null): CustomTheme | null {
  const root = document.documentElement.style;
  for (const k of applied) root.removeProperty(k);
  applied = [];
  const t = name ? installed()[name] : undefined;
  if (!t) { localStorage.removeItem(LS_ON); return null; }
  for (const [k, v] of Object.entries(t.tokens)) {
    // Both already checked by the core: k is a known token, v is #rrggbb.
    if (!/^[a-z0-9-]+$/.test(k) || !/^#[0-9a-f]{6}$/.test(v)) continue;
    root.setProperty(`--${k}`, v);
    applied.push(`--${k}`);
  }
  localStorage.setItem(LS_ON, t.name);
  return t;
}

/** The `Theme` effect. Answers with what to say, and the base to switch to
 *  when a theme went on screen. */
export function themeEffect(op: string, name: string, json: string): { say: string; level: string; base?: "creme" | "midnight" } {
  const all = installed();
  const save = () => localStorage.setItem(LS_THEMES, JSON.stringify(all));
  switch (op) {
    case "add": {
      const t = JSON.parse(json) as CustomTheme;
      all[t.name] = t;
      save();
      applyCustom(t.name);
      return { say: `installed theme ${t.name}. /theme creme or /theme midnight goes back`, level: "info", base: t.base };
    }
    case "use":
      if (name === "creme" || name === "midnight") { applyCustom(null); return { say: `theme: ${name}`, level: "info", base: name }; }
      if (!all[name]) return { say: `no theme called ${name}. /theme add installs one`, level: "error" };
      applyCustom(name);
      return { say: `theme: ${name}`, level: "info", base: all[name].base };
    case "remove":
      if (!all[name]) return { say: `no theme called ${name}`, level: "error" };
      delete all[name];
      save();
      if (activeCustom() === name) applyCustom(null);
      return { say: `removed theme ${name}`, level: "info" };
    default: {
      const names = ["creme", "midnight", ...Object.keys(all)];
      return { say: `themes: ${names.join(", ")}. /theme <name> switches, /theme add installs`, level: "info" };
    }
  }
}
