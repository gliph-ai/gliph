// Build Gliph and drop it into /Applications, signed with a STABLE identity.
//
//   node scripts/install-local.mjs          # production build  → Gliph.app
//   node scripts/install-local.mjs --dev    # dev build         → Gliph Dev.app
//
// Why this exists rather than `tauri build` on its own:
//
// `tauri build` with no identity ad-hoc signs, and an ad-hoc signature's cdhash
// changes on EVERY rebuild. macOS ties data-protection and Keychain grants to
// that hash, so each new build looks like a different app: you get re-prompted,
// and anything the app kept per-identity can look empty. Signing with a real
// certificate from the Keychain keeps the hash stable across rebuilds, so the
// grant survives and you are asked once.
//
// `tauri dev` is unaffected either way — it runs the binary straight out of
// target/ without bundling or signing, which is why the day-to-day loop
// (`npm run dev`) never touches the Keychain at all.
//
// Override the choice with APPLE_SIGNING_IDENTITY: an exact identity string, or
// "-" to force ad-hoc and accept the re-prompting.

import { execFileSync } from "node:child_process";
import { existsSync, rmSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const desktop = path.resolve(here, "..");
const isDev = process.argv.includes("--dev");

const plan = isDev
  ? { appName: "Gliph Dev.app", identifier: "chat.gliph.desktop.dev", config: ["-c", "src-tauri/tauri.dev.conf.json"] }
  : { appName: "Gliph.app", identifier: "chat.gliph.desktop", config: [] };

const log = (m) => console.log(`[install] ${m}`);

// A Developer ID cert is what you would hand to someone else; an Apple
// Development cert is enough for an app that only ever runs on this Mac, and is
// what a dev machine usually has. Either is stable, which is the point.
const IDENTITY_PREFERENCE = [/^Developer ID Application:/, /^Apple Development:/];

function availableIdentities() {
  try {
    const out = execFileSync("security", ["find-identity", "-v", "-p", "codesigning"], { encoding: "utf8" });
    return [...out.matchAll(/^\s*\d+\)\s+\S+\s+"([^"]+)"/gm)].map((m) => m[1]);
  } catch {
    return [];
  }
}

// Resolve BEFORE building: a missing certificate otherwise surfaces as an opaque
// "failed to bundle project: failed codesign application" after a full Rust
// compile, which is a long way to travel for a one-line problem.
function resolveSigningIdentity() {
  const available = availableIdentities();
  const requested = process.env.APPLE_SIGNING_IDENTITY;

  if (requested === "-") return "-";
  if (requested) {
    if (!available.includes(requested)) {
      console.error(
        `error: APPLE_SIGNING_IDENTITY is "${requested}", which is not in the keychain.\n` +
          `  Available:${available.length ? available.map((i) => `\n    ${i}`).join("") : " (none)"}\n` +
          '  Use APPLE_SIGNING_IDENTITY="-" to sign ad-hoc instead.',
      );
      process.exit(1);
    }
    return requested;
  }
  for (const pattern of IDENTITY_PREFERENCE) {
    const found = available.find((i) => pattern.test(i));
    if (found) return found;
  }
  console.error(
    "error: no usable code-signing identity in the keychain.\n" +
      `  Available:${available.length ? available.map((i) => `\n    ${i}`).join("") : " (none)"}\n` +
      "  Create one in Xcode → Settings → Accounts → Manage Certificates, or set\n" +
      '  APPLE_SIGNING_IDENTITY="-" to sign ad-hoc (expect repeat prompts after every rebuild).',
  );
  process.exit(1);
}

const identity = resolveSigningIdentity();
log(`building ${plan.appName} (${plan.identifier})${identity === "-" ? " [ad-hoc]" : ` signed: ${identity}`}`);

execFileSync("node_modules/.bin/tauri", ["build", "--bundles", "app", ...plan.config], {
  cwd: desktop,
  stdio: "inherit",
  env: { ...process.env, APPLE_SIGNING_IDENTITY: identity },
});

const built = path.join(desktop, "src-tauri/target/release/bundle/macos", plan.appName);
if (!existsSync(built)) {
  console.error(`error: expected a bundle at ${built}`);
  process.exit(1);
}

const dest = path.join("/Applications", plan.appName);
// Replace rather than merge: a stale binary left behind by a previous layout
// is the kind of thing that wastes an afternoon.
rmSync(dest, { recursive: true, force: true });
execFileSync("cp", ["-R", built, dest], { stdio: "inherit" });
log(`installed ${dest}`);
