// Assertions over the two things in message rendering that are dangerous to get
// wrong: escaping, and whether a rendered message can reach the network.
//
//   node --experimental-strip-types scripts/check-render.mjs
//
// Node strips the types and runs the real modules, so this checks the shipping
// code rather than a copy of it. No framework: it is a file of asserts.

import assert from "node:assert/strict";

// cards.ts reads a preference from localStorage. Stub it before importing.
globalThis.localStorage = {
  store: new Map(),
  getItem(k) { return this.store.get(k) ?? null; },
  setItem(k, v) { this.store.set(k, String(v)); },
};

const { renderInline, renderWithCodeBlocks, hasCodeBlock, urlsIn } =
  await import("../src/format.ts");
const { renderCards, classify, setUnfurlMode } = await import("../src/cards.ts");
const undo = await import("../src/undo.ts");

let checks = 0;
const check = (name, fn) => { fn(); checks += 1; console.log(`  ok  ${name}`); };

// ---- code ----

check("single backticks render inline code", () => {
  const out = renderInline("run `npm ci` first");
  assert.ok(out.includes('<code class="inl">npm ci</code>'), out);
});

check("a backtick pair across lines is not inline code", () => {
  const out = renderInline("a `b\nc` d");
  assert.ok(!out.includes("<code"), out);
});

check("a fenced block past seven lines starts folded, a short one does not", () => {
  const short = renderWithCodeBlocks("```js\n1\n2\n3\n```");
  assert.ok(!short.includes("tall") && !short.includes("data-expand"), short);
  const long = renderWithCodeBlocks("```js\n" + "x\n".repeat(8) + "```");
  assert.ok(long.includes("card code tall") && long.includes("expand · 8 lines"), long);
  assert.ok(long.includes('data-lang="js"'), long);
});

check("markdown and text fences wrap; code fences do not", () => {
  assert.ok(renderWithCodeBlocks("```markdown\nhello\n```").includes("wrap"));
  assert.ok(renderWithCodeBlocks("```\nhello\n```").includes("wrap"));
  assert.ok(!renderWithCodeBlocks("```js\nhello\n```").includes("wrap"));
});

check("@nick is a mention link, an email address is not", () => {
  const out = renderInline("hey @abdullah, mail me@x.io");
  assert.ok(out.includes('<a class="mention" data-nick="abdullah">@abdullah</a>'), out);
  assert.ok(!out.includes('data-nick="x'), out);
});

// ---- escaping: the one that must never regress ----

check("script tags in a message are inert", () => {
  const out = renderInline(`<script>alert(1)</script>`);
  assert.ok(!out.includes("<script"), out);
  assert.ok(out.includes("&lt;script&gt;"), out);
});

check("an attribute break-out is escaped", () => {
  const out = renderInline(`" onerror="alert(1)`);
  assert.ok(!out.includes(`onerror="`), out);
});

check("an apostrophe is escaped too", () => {
  // The client had two escape functions, and only one of them did this. There
  // is now one, in src/escape.ts, and it is the strict variant — so a template
  // that reaches for a single-quoted attribute cannot be broken out of either.
  const out = renderInline(`' onerror='alert(1)`);
  assert.ok(!out.includes(`onerror='`), out);
  assert.ok(out.includes("&#39;"), out);
});

check("an img tag pasted as text stays text", () => {
  const out = renderInline(`<img src=x onerror=alert(1)>`);
  assert.ok(!/<img/i.test(out), out);
});

check("a javascript: URL never becomes a link", () => {
  const out = renderInline(`javascript:alert(1)`);
  assert.ok(!out.includes("<a "), out);
});

// ---- IRC codes ----

check("an unterminated bold does not escape its own line", () => {
  const out = renderInline("\x02never closed");
  const opens = (out.match(/<span/g) ?? []).length;
  const closes = (out.match(/<\/span>/g) ?? []).length;
  assert.equal(opens, closes, out);
});

check("colour codes come from the palette, not from the message", () => {
  const out = renderInline("\x0304red");
  assert.ok(out.includes("var(--"), out);
  assert.ok(!/#[0-9a-f]{3,6}/i.test(out), out);
});

// ---- code blocks ----

check("a fenced block cannot break out of its pre", () => {
  const out = renderWithCodeBlocks("```js\n</code></pre><script>alert(1)</script>\n```");
  assert.ok(!out.includes("<script"), out);
  assert.equal((out.match(/<\/pre>/g) ?? []).length, 1, out);
});

check("prose around a fence still renders", () => {
  assert.ok(hasCodeBlock("a ```x``` b"));
  const out = renderWithCodeBlocks("before ```sh\nls\n``` after");
  assert.ok(out.includes("before") && out.includes("after"), out);
});

// ---- the privacy rule ----

check("a rendered media card fetches nothing", () => {
  setUnfurlMode("ask");
  const html = renderCards(urlsIn("look https://tracker.example/pixel.png"));
  assert.ok(html.includes("tracker.example"), "the host should be named");
  // No element in the placeholder may carry a remote src/href/url().
  assert.ok(!/\bsrc\s*=/.test(html), `placeholder can fetch: ${html}`);
  assert.ok(!/url\(/.test(html), `placeholder has a css fetch: ${html}`);
  assert.ok(!/https?:\/\//.test(html.replace(/data-url="[^"]*"/g, "")),
    `placeholder names a remote URL outside data-url: ${html}`);
});

check("SVG is not treated as an image", () => {
  assert.equal(classify("https://x.example/a.svg")?.kind, "link");
  assert.equal(classify("https://x.example/a.png")?.kind, "image");
});

check("non-http schemes never classify", () => {
  assert.equal(classify("javascript:alert(1)"), null);
  assert.equal(classify("data:text/html,<script>"), null);
  assert.equal(classify("file:///etc/passwd"), null);
});

check("unfurling off means no cards at all", () => {
  setUnfurlMode("never");
  assert.equal(renderCards(urlsIn("https://x.example/a.png")), "");
  setUnfurlMode("ask");
});

check("one message cannot fill the viewport with cards", () => {
  const many = Array.from({ length: 12 }, (_, i) => `https://x.example/${i}.png`).join(" ");
  const html = renderCards(urlsIn(many));
  assert.equal((html.match(/data-card=/g) ?? []).length, 4, html);
});

// ---- undo ----

check("an op action round-trips through its inverse", () => {
  undo.reset();
  const e = undo.inverseOf("ban", "robin spamming", { });
  assert.equal(e.inverse.name, "unban");
  assert.equal(e.inverse.args, "robin", "the reason must not travel into /unban");
  undo.record(e);
  assert.equal(undo.takeUndo().name, "unban");
  // ...and redo replays the ORIGINAL, reason and all.
  const again = undo.takeRedo();
  assert.equal(again.name, "ban");
  assert.equal(again.args, "robin spamming");
});

check("a topic undo restores the topic that was there before", () => {
  undo.reset();
  const e = undo.inverseOf("topic", "new topic", { topic: "the old one" });
  assert.equal(e.inverse.args, "the old one");
  undo.record(e);
  assert.equal(undo.takeUndo().args, "the old one");
  // Redo puts the new one back, which deriving an inverse-of-an-inverse could
  // never have known.
  assert.equal(undo.takeRedo().args, "new topic");
});

check("a kick is shown but never offered as undoable", () => {
  undo.reset();
  const e = undo.inverseOf("kick", "robin", {});
  assert.equal(e.inverse, null);
  undo.record(e);
  assert.equal(undo.canUndo(), false, "nothing un-kicks a person");
  assert.equal(undo.currentToast().label, "kicked robin", "but you still see what you did");
});

check("a mixed mode string has no guessed inverse", () => {
  assert.equal(undo.inverseOf("mode", "+mi-t", {}), null);
  assert.equal(undo.inverseOf("mode", "+m", {}).inverse.args, "-m");
});

check("a new action abandons the redo branch", () => {
  undo.reset();
  undo.record(undo.inverseOf("op", "a", {}));
  undo.takeUndo();
  assert.equal(undo.canRedo(), true);
  undo.record(undo.inverseOf("op", "b", {}));
  assert.equal(undo.canRedo(), false);
});

check("commands with no inverse are not pushed onto the history", () => {
  undo.reset();
  assert.equal(undo.inverseOf("msg", "someone hello", {}), null);
  assert.equal(undo.canUndo(), false);
});

// The one seam in the nearby mesh that no test can exercise headlessly: the
// desktop client calls into Rust by NAME, and a typo there is only visible to
// someone holding the app. Compare the two lists instead.
//
// This is the one check that reads outside web/, so it is also the one that
// cannot always run: the server image builds the client from web/ alone (see
// .dockerignore), and there is no desktop tree in that context. Skip loudly
// rather than throw — a missing sibling tree is not a failing assertion, and
// printing "skip" keeps it from reading as a check that passed. Every local
// `npm run build` has the tree, which is where this actually earns its keep.
const libRsPath = new URL("../../desktop/src-tauri/src/lib.rs", import.meta.url);
const { existsSync, readFileSync } = await import("node:fs");

if (!existsSync(libRsPath)) {
  console.log("  skip  mesh command check — no desktop tree in this build context");
} else check("every mesh command the client invokes is registered in the Tauri handler", () => {
  const meshTs = readFileSync(new URL("../src/mesh.ts", import.meta.url), "utf8");
  const libRs = readFileSync(libRsPath, "utf8");
  const invoked = [...meshTs.matchAll(/invoke(?:<[^>]*>)?\("([a-z_]+)"/g)].map((m) => m[1]);
  assert.ok(invoked.length >= 5, `expected the mesh commands, found ${invoked}`);
  const handler = libRs.slice(libRs.indexOf("generate_handler!"));
  for (const name of invoked) {
    assert.ok(handler.includes(`mesh::${name}`), `${name} is invoked but not registered in lib.rs`);
  }
});

// ---- device linking: where a result may be delivered ----
//
// The deny path never reaches the server, so this predicate is the ONLY thing
// standing between a crafted redirect_uri and an open redirect on the server's
// own domain.
const { deliverable, nextPath } = await import("../src/redirect.ts");

check("a loopback callback is deliverable", () => {
  assert.ok(deliverable("http://127.0.0.1:53123/cb"));
  assert.ok(deliverable("http://[::1]:53123/cb"));
});

check("an off-machine redirect_uri is refused", () => {
  assert.equal(deliverable("https://evil.example/cb"), null);
  assert.equal(deliverable("http://evil.example/cb"), null);
  // Looks loopback, resolves wherever the host's name service says.
  assert.equal(deliverable("http://localhost:53123/cb"), null);
  // The classic near-miss: a hostname that merely starts with the loopback IP.
  assert.equal(deliverable("http://127.0.0.1.evil.example/cb"), null);
});

check("a non-http scheme is refused", () => {
  assert.equal(deliverable("javascript:alert(1)"), null);
  assert.equal(deliverable("https://127.0.0.1/cb"), null);
  assert.equal(deliverable("not a url"), null);
});

// The nearby transfer engine moved to core/src/xfer.rs when the client core
// became Rust, and its checks went with it — see core/tests. The pacing rules
// they cover (a slow receiver is waited for, a gap in the sequence fails the
// transfer rather than corrupting it) are the same rules; they are just
// asserted once now instead of once per language.


// ---- the session key, which the core owns and two other pages read ----
//
// The admin console and the device-link page both read the session out of
// localStorage without a core loaded, and both used to guess the key. They
// guessed "gliph.token", which nothing has ever written, so both concluded you
// were signed out no matter what. Now they share src/session.ts — and this
// asserts that file agrees with core/src/client.rs about where a session lives,
// because the failure mode of disagreeing is silent.
{
  const fs = await import("node:fs/promises");
  const ts = await fs.readFile(new URL("../src/session.ts", import.meta.url), "utf8");
  const key = ts.match(/const KEY = "([^"]+)"/)?.[1];

  const rsPath = new URL("../../core/src/client.rs", import.meta.url);
  if (!existsSync(rsPath)) {
    console.log("  skip  session key parity — no core tree in this build context");
  } else {
    const rs = await fs.readFile(rsPath, "utf8");
    const coreKey = rs.match(/KEY_SESSION: &str = "([^"]+)"/)?.[1];
    check("session.ts reads the key the core writes", () => {
      assert.ok(coreKey, "core/src/client.rs has no KEY_SESSION");
      assert.equal(key, coreKey, `session.ts reads ${key}, the core writes ${coreKey}`);
    });
  }

  check("no page guesses at a session key of its own", () => {
    // The keys that were wrong. They must not come back anywhere.
    //
    // `gliph.server` and not `gliph.servers`: the second is a real key the core
    // writes for the switcher, and a plain substring test cannot tell the
    // invented one from the real one.
    const invented = /gliph\.server(?!s)|gliph\.token/;
    for (const f of ["admin.ts", "link.ts", "main.ts", "client.ts"]) {
      const src = readFileSync(new URL("../src/" + f, import.meta.url), "utf8");
      const hit = src.match(invented);
      assert.ok(!hit, `${f} still reads ${hit?.[0]}`);
    }
  });

  const serversPath = new URL("../../core/src/servers.rs", import.meta.url);
  if (existsSync(serversPath)) {
    const srv = await fs.readFile(serversPath, "utf8");
    const coreKey = srv.match(/KEY_SERVERS: &str = "([^"]+)"/)?.[1];
    check("the switcher reads the key the core writes", () => {
      assert.ok(coreKey, "core/src/servers.rs has no KEY_SERVERS");
      const src = readFileSync(new URL("../src/client.ts", import.meta.url), "utf8");
      assert.ok(
        src.includes(`"${coreKey}"`),
        `client.ts restores from a different key than the core writes (${coreKey})`,
      );
    });
  }
}

// ---- room0's boards render values other people wrote ----
//
// The task board is one JSON blob that any member of the channel can PUT and
// the server stores without looking inside it (`PUT /api/room/board`). Its lane
// and note ids used to go straight into `data-lane="..."` unescaped, which made
// a sticky note a way to run script in every other member's app origin. The
// fix is esc() on every one of them; this check is what keeps it there.

check("every attribute in room.ts is escaped or a local literal", () => {
  const src = readFileSync(new URL("../src/room/room.ts", import.meta.url), "utf8");
  // Names that are this module's own constants or loop bookkeeping, never wire
  // data. Anything else opening an attribute value has to go through esc().
  const LOCAL = ["esc", "isPlaying", "field", "label", "name", "EMBED_URL", "e.kind"];
  for (const [, expr] of src.matchAll(/="\$\{([A-Za-z_.$][\w.$]*)/g)) {
    assert.ok(LOCAL.includes(expr),
      `room.ts opens an attribute with \`${expr}\` — wrap it in esc() or add it to LOCAL`);
  }
});

// ---- where a ?next= may send someone after they sign in ----
//
// The admin console dropped its own login form and bounces to the client's with
// ?next=/admin/users. That address arrives in a URL, so it is attacker-typed:
// the whole value of nextPath is that it says no to everything off this origin.

check("a path on this origin is a valid return address", () => {
  assert.equal(nextPath("/admin/users"), "/admin/users");
  assert.equal(nextPath("/admin"), "/admin");
});

check("a return address can never leave this origin", () => {
  for (const bad of [
    "//evil.example",            // protocol-relative — a host, not a path
    "/\\evil.example",           // the same thing, spelled the way browsers also accept
    "https://evil.example",
    "http://evil.example",
    "javascript:alert(1)",
    "/admin/../../evil",
    "evil.example",              // no leading slash at all
    "", null, undefined,
  ]) {
    assert.equal(nextPath(bad), null, `nextPath accepted ${JSON.stringify(bad)}`);
  }
});

check("a control character cannot smuggle a host past the check", () => {
  // Browsers strip \t \r \n out of a URL before resolving it, so "/\t/evil"
  // would be read as "//evil" — protocol-relative — after this function said yes.
  assert.equal(nextPath("/\t/evil.example"), null);
  assert.equal(nextPath("/\n/evil.example"), null);
});

// ---- the admin console's generated passwords ----
//
// Asserted off the source text, not by importing admin.ts: that module wires
// itself to a live #app on load, and a DOM stub to reach one const is more code
// than the const. What must hold is that the shortest passphrase this can emit
// still clears the store's 8-character floor (server/store.go), and that the
// list is what its comment claims — 128 distinct words, which is what makes the
// modulo unbiased and the ~34-bit figure true.
{
  const fs = await import("node:fs/promises");
  const src = await fs.readFile(new URL("../src/admin.ts", import.meta.url), "utf8");
  const block = src.slice(src.indexOf("const WORDS = ("), src.indexOf(").split"));
  const words = [...block.matchAll(/"([a-z ]+) ?"/g)].flatMap(([, s]) => s.trim().split(" "));

  check("the admin wordlist is 128 distinct speakable words", () => {
    assert.equal(words.length, 128, `wordlist has ${words.length} words`);
    assert.equal(new Set(words).size, 128, "wordlist repeats a word");
    for (const w of words) assert.match(w, /^[a-z]{3,9}$/, `bad word: ${w}`);
  });

  check("the shortest generated password clears the 8-character floor", () => {
    // four words joined by hyphens, plus "-" and two digits
    const shortest = Math.min(...words.map((w) => w.length)) * 4 + 3 + 3;
    assert.ok(shortest >= 8, `shortest passphrase is ${shortest} characters`);
  });
}

// ---- the palette exists twice, so assert the two copies agree ----
//
// mobile/Sources/Theme.swift copies its hex values from src/theme.css by hand
// and says so in its header: "a phone next to a laptop should be the same
// product". Nothing was keeping that true. Forty numbers maintained in two
// files drift, and the way you find out is a screenshot months later.
//
// Not generated — asserted. Generating Swift from CSS is a build step to own;
// this is nine lines that fail the build the day the two disagree.
//
// Like the mesh check above, this reads outside web/ — and mobile/ is in
// .dockerignore, so the server image builds without it. Skip loudly there.
const themeSwiftPath = new URL("../../mobile/Sources/Theme.swift", import.meta.url);
if (!existsSync(themeSwiftPath)) {
  console.log("  skip  palette parity check — no mobile tree in this build context");
} else {
  const fs = await import("node:fs/promises");
  const css = await fs.readFile(new URL("../src/theme.css", import.meta.url), "utf8");
  const swift = await fs.readFile(themeSwiftPath, "utf8");

  // Theme.swift field -> theme.css custom property.
  const FIELDS = {
    chrome: "chrome", chromeText: "chrome-text", chromeDim: "chrome-dim",
    bg: "bg", surface: "surface", surface2: "surface-2", rail: "rail",
    text: "text", text2: "text-2", muted: "muted",
    border: "border", border2: "border-2",
    accent: "accent", link: "link", join: "join",
    action: "action", mode: "mode", agent: "agent", gold: "gold",
  };

  const cssBlock = (sel) => {
    const i = css.indexOf(sel);
    assert.ok(i >= 0, `theme.css has no ${sel} block`);
    return css.slice(i, css.indexOf("}", i));
  };
  const swiftBlock = (name) => {
    const i = swift.indexOf(`static let ${name} = Theme(`);
    assert.ok(i >= 0, `Theme.swift has no ${name}`);
    return swift.slice(i, swift.indexOf("\n    )", i));
  };

  const cssVars = (block) =>
    Object.fromEntries([...block.matchAll(/--([a-z0-9-]+):\s*#([0-9a-fA-F]{6})/g)]
      .map(([, k, v]) => [k, v.toLowerCase()]));
  const swiftVars = (block) =>
    Object.fromEntries([...block.matchAll(/(\w+):\s*\.hex\(0x([0-9A-Fa-f]{6})\)/g)]
      .map(([, k, v]) => [k, v.toLowerCase()]));

  for (const [theme, sel] of [["creme", ':root[data-theme="creme"]'],
                              ["midnight", ':root[data-theme="midnight"]']]) {
    const web = cssVars(cssBlock(sel));
    const app = swiftVars(swiftBlock(theme));
    check(`the ${theme} palette matches between theme.css and Theme.swift`, () => {
      for (const [field, cssName] of Object.entries(FIELDS)) {
        assert.equal(app[field], web[cssName],
          `${theme}.${field} is #${app[field]}, theme.css --${cssName} is #${web[cssName]}`);
      }
    });
  }
}

// room0's GLB props embed their JPEG textures, so GLTFLoader hands the bytes to
// ImageBitmapLoader as a blob: URL — and that load is a fetch(), which CSP
// governs under connect-src, not img-src. CSP's `*` covers network schemes
// only, so blob: has to be spelled out. Miss it and three.js swallows the
// failure (GLTFLoader catches the texture error and returns null): every prop
// renders with full geometry and no texture, pure white, nothing in the app.
{
  for (const f of ["../index.html", "../../desktop/src-tauri/tauri.conf.json"]) {
    const path = new URL(f, import.meta.url);
    const name = f.split("/").pop();
    // The Docker web stage copies web/ alone, so the Tauri config is out of
    // reach there — same as the mesh check above.
    if (!existsSync(path)) {
      console.log(`  skip  ${name} connect-src — not in this build context`);
      continue;
    }
    const connect = readFileSync(path, "utf8").match(/connect-src ([^;"]*)/)?.[1];
    check(`${name} allows blob: in connect-src (room0 GLB textures)`, () => {
      assert.ok(connect, `no connect-src found in ${f}`);
      assert.ok(/\bblob:/.test(connect),
        `connect-src is "${connect}" — without blob: every room0 prop renders untextured white`);
    });
  }
}

console.log(`\n${checks} checks passed`);
