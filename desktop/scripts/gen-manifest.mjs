// Build the Tauri updater manifest (`latest.json`) from the bundles present.
//
//   node scripts/gen-manifest.mjs --artifacts <dir> --version 0.2.0 \
//        --base-url https://chat.example.com/downloads/desktop/0.2.0
//
// Run by bin/release-desktop once both OS builds are in one folder, and by hand when
// bundles from several machines have been gathered into one folder — Tauri
// cannot cross-compile its installers, so a release is assembled from one
// build per OS.
//
// The generator keys off which `.sig` files are actually present, so a manifest
// only ever claims platforms that really made it into the folder. A missing OS
// is simply absent rather than a broken download.
//
// TWO THINGS THAT LOOK LIKE DETAILS AND ARE NOT:
//
//   * `signature` is the CONTENT of the .sig file, not a path or a URL. Getting
//     this wrong produces a manifest that parses and then fails verification on
//     every client.
//   * each platform needs both the bare `{target}-{arch}` key and the
//     `{target}-{arch}-{installer}` key. The bare one is what older clients
//     read; the suffixed ones are what multi-installer updates (plugin >= 2.10)
//     need. Emitting only one of the two silently strands half the userbase.

import { readdirSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import path from "node:path";

const argv = process.argv.slice(2);
const valueOf = (f, d = null) => {
  const i = argv.indexOf(f);
  return i === -1 ? d : argv[i + 1] ?? d;
};

const artifacts = valueOf("--artifacts");
const outDir = valueOf("--out", artifacts);
const version = valueOf("--version");
const baseUrl = (valueOf("--base-url") ?? "").replace(/\/$/, "");
// macOS `.app.tar.gz` carries no architecture in its name, so the build has to
// say which one it is. Default `universal` claims both; our builds are native.
const macArch = valueOf("--mac-arch", "universal");
const notes = valueOf("--notes", "");
const pubDate = valueOf("--pub-date", null);

if (!artifacts || !version || !baseUrl) {
  console.error("usage: gen-manifest.mjs --artifacts <dir> --version <v> --base-url <url> [--out <dir>] [--mac-arch <arch>]");
  process.exit(2);
}

// A bundle names its version; one naming another version is a stray from an
// earlier build and must not be published as this one.
const files = readdirSync(artifacts).filter((f) => !f.includes("_") || f.includes(`_${version}_`));
const sigs = files.filter((f) => f.endsWith(".sig"));

/** Which platform keys a given bundle should be published under. */
function keysFor(file) {
  if (file.endsWith(".app.tar.gz")) {
    const arches = macArch === "universal" ? ["x86_64", "aarch64"] : [macArch];
    return arches.flatMap((a) => [`darwin-${a}`, `darwin-${a}-app`]);
  }
  if (file.endsWith(".exe")) return ["windows-x86_64", "windows-x86_64-nsis"];
  if (file.endsWith(".AppImage")) return ["linux-x86_64", "linux-x86_64-appimage"];
  // The bare linux key belongs to the AppImage when there is one (it updates in
  // place); with only a .deb published, the .deb takes it.
  if (file.endsWith(".deb")) return files.some((f) => f.endsWith(".AppImage")) ? ["linux-x86_64-deb"] : ["linux-x86_64", "linux-x86_64-deb"];
  if (file.endsWith(".rpm")) return ["linux-x86_64-rpm"];
  return [];
}

const platforms = {};
for (const sig of sigs) {
  const bundle = sig.slice(0, -4); // strip .sig
  if (!files.includes(bundle)) {
    console.warn(`[manifest] ${sig} has no matching bundle — skipping`);
    continue;
  }
  const signature = readFileSync(path.join(artifacts, sig), "utf8").trim();
  const url = `${baseUrl}/${encodeURIComponent(bundle)}`;
  for (const key of keysFor(bundle)) {
    // The bare key is claimed by the primary installer. If two bundles want it
    // (an AppImage and a .deb, say) keysFor has already decided which.
    platforms[key] = { signature, url };
  }
}

if (Object.keys(platforms).length === 0) {
  console.warn("[manifest] no signed bundles found — is TAURI_SIGNING_PRIVATE_KEY set?");
}

const manifest = {
  version,
  notes,
  // Reproducible when asked: a build that pins its date can be re-run and
  // compared byte for byte.
  pub_date: pubDate ?? new Date().toISOString().replace(/\.\d+Z$/, "Z"),
  platforms,
};

mkdirSync(outDir, { recursive: true });
const out = path.join(outDir, "latest.json");
writeFileSync(out, JSON.stringify(manifest, null, 2) + "\n");
console.log(`[manifest] ${out} — ${Object.keys(platforms).sort().join(", ") || "(empty)"}`);
