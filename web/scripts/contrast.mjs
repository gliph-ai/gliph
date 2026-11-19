// WCAG contrast of the two shipped themes, on web and iOS.
//
//   node scripts/contrast.mjs
//
// Reads the tokens from web/src/theme.css and mobile/Sources/Theme.swift and
// checks every pair a reader sees as text against the AA floor (4.5:1, normal
// text). Exits 1 on a failure, so `npm run check` holds the line.

import { readFileSync } from "node:fs";

const here = new URL(".", import.meta.url);
const css = readFileSync(new URL("../src/theme.css", here), "utf8");
// The Docker build has web/ alone; there the iOS half is skipped.
let swift = "";
try { swift = readFileSync(new URL("../../mobile/Sources/Theme.swift", here), "utf8"); } catch { /* web only */ }

const block = (sel) => {
  const i = css.indexOf(sel);
  const body = css.slice(css.indexOf("{", i) + 1, css.indexOf("}", i));
  return Object.fromEntries([...body.matchAll(/--([\w-]+):\s*(#[0-9a-f]{6})/gi)].map((m) => [m[1], m[2]]));
};
const kebab = (s) => s.replace(/[A-Z0-9]/g, (c) => "-" + c.toLowerCase());
const swiftTheme = (name) => {
  const i = swift.indexOf(`static let ${name} = Theme(`);
  const body = swift.slice(i, swift.indexOf("\n    )", i));
  const t = Object.fromEntries([...body.matchAll(/(\w+): \.hex\(0x([0-9A-F]{6})\)/gi)].map((m) => [kebab(m[1]), "#" + m[2]]));
  const ac = swift.match(/var accentChrome: Color \{ isDark \? \.hex\(0x(\w{6})\) : \.hex\(0x(\w{6})\)/);
  if (ac) t["accent-chrome"] = "#" + (name === "midnight" ? ac[1] : ac[2]);
  return t;
};

const lum = (hex) => {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
export const ratio = (a, b) => {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
};

// Text tokens, and the grounds they are drawn on.
const INK = ["text", "text-2", "muted", "accent", "link", "join", "action", "mode", "agent", "gold", "azure", "teal", "olive", "magenta"];
const GROUND = ["bg", "surface", "surface-2", "rail"];
// On the chrome bar the accent is --accent-chrome (the wordmark, the offline
// mark): the page's accent is too dark for the navy.
const CHROME = [["chrome-text", "chrome"], ["chrome-dim", "chrome"], ["accent-chrome", "chrome"]];

const themes = {
  "web creme": block(':root[data-theme="creme"]'),
  "web midnight": block(':root[data-theme="midnight"]'),
  ...(swift && { "ios creme": swiftTheme("creme"), "ios midnight": swiftTheme("midnight") }),
};

let fails = 0;
for (const [name, t] of Object.entries(themes)) {
  const pairs = [...INK.flatMap((i) => GROUND.map((g) => [i, g])), ...CHROME];
  let worst = Infinity;
  for (const [fg, bg] of pairs) {
    if (!t[fg] || !t[bg]) continue;
    const r = ratio(t[fg], t[bg]);
    worst = Math.min(worst, r);
    if (r < 4.5) {
      fails++;
      console.log(`  FAIL ${name}: --${fg} ${t[fg]} on --${bg} ${t[bg]} = ${r.toFixed(2)}`);
    }
  }
  console.log(`${name}: lowest ${worst.toFixed(2)}:1 over ${pairs.length} pairs`);
}
if (fails) {
  console.log(`${fails} pairs under AA (4.5:1)`);
  process.exit(1);
}
