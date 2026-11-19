#!/usr/bin/env node
// Shrink a Meshy-style GLB game prop for web delivery, without native deps:
// - keeps geometry untouched
// - drops PBR maps (normal / metallicRoughness / occlusion / emissive) — the
//   room0 props are stylized low-poly, flat materials read better anyway
// - re-encodes the base-color texture to max-1024px JPEG via macOS `sips`
// - rebuilds the GLB binary buffer with only the bytes still referenced
//
// Usage: node scripts/shrink-glb.mjs in.glb out.glb
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const [, , inPath, outPath] = process.argv;
if (!inPath || !outPath) {
  console.error('usage: shrink-glb.mjs in.glb out.glb');
  process.exit(1);
}

const buf = readFileSync(inPath);
if (buf.readUInt32LE(0) !== 0x46546c67) throw new Error('not a GLB');
const jsonLen = buf.readUInt32LE(12);
const json = JSON.parse(buf.subarray(20, 20 + jsonLen).toString('utf8'));
const binStart = 20 + jsonLen + 8;
const bin = buf.subarray(binStart, binStart + buf.readUInt32LE(20 + jsonLen));

// ── 1. Strip everything but baseColorTexture from materials ──
const keptTextures = new Set();
for (const m of json.materials || []) {
  delete m.normalTexture;
  delete m.occlusionTexture;
  // Without its texture, a non-zero emissiveFactor would glow solid white.
  if (m.emissiveTexture) m.emissiveFactor = [0, 0, 0];
  delete m.emissiveTexture;
  const pbr = m.pbrMetallicRoughness;
  if (pbr) {
    delete pbr.metallicRoughnessTexture;
    pbr.metallicFactor = 0;
    pbr.roughnessFactor = 0.95;
    if (pbr.baseColorTexture) keptTextures.add(pbr.baseColorTexture.index);
  }
}

// ── 2. Re-encode kept images (PNG/whatever → ≤1024px JPEG via sips) ──
const tmp = mkdtempSync(join(tmpdir(), 'glbshrink-'));
const keptImages = new Map(); // old image index → jpeg Buffer
for (const ti of keptTextures) {
  const tex = json.textures[ti];
  const img = json.images[tex.source];
  const bv = json.bufferViews[img.bufferView];
  const bytes = bin.subarray(bv.byteOffset || 0, (bv.byteOffset || 0) + bv.byteLength);
  const src = join(tmp, `img${tex.source}.png`);
  const dst = join(tmp, `img${tex.source}.jpg`);
  writeFileSync(src, bytes);
  execFileSync('sips', ['-s', 'format', 'jpeg', '-s', 'formatOptions', '82',
    '--resampleHeightWidthMax', '1024', src, '--out', dst], { stdio: 'ignore' });
  keptImages.set(tex.source, readFileSync(dst));
}
rmSync(tmp, { recursive: true, force: true });

// ── 3. Rebuild bufferViews/images/textures with only what's referenced ──
const imageBVs = new Set((json.images || []).map((i) => i.bufferView));
const bvRemap = new Map();
const newBVs = [];
const chunks = [];
let offset = 0;
const align4 = () => {
  const pad = (4 - (offset % 4)) % 4;
  if (pad) { chunks.push(Buffer.alloc(pad)); offset += pad; }
};
json.bufferViews.forEach((bv, i) => {
  if (imageBVs.has(i)) return; // images re-added below
  align4();
  const bytes = bin.subarray(bv.byteOffset || 0, (bv.byteOffset || 0) + bv.byteLength);
  bvRemap.set(i, newBVs.length);
  const nv = { ...bv, byteOffset: offset };
  newBVs.push(nv);
  chunks.push(bytes);
  offset += bytes.length;
});

const imgRemap = new Map();
const newImages = [];
for (const [oldIdx, jpeg] of keptImages) {
  align4();
  imgRemap.set(oldIdx, newImages.length);
  newImages.push({ mimeType: 'image/jpeg', bufferView: newBVs.length });
  newBVs.push({ buffer: 0, byteOffset: offset, byteLength: jpeg.length });
  chunks.push(jpeg);
  offset += jpeg.length;
}

const texRemap = new Map();
const newTextures = [];
for (const ti of keptTextures) {
  const t = json.textures[ti];
  texRemap.set(ti, newTextures.length);
  newTextures.push({ ...t, source: imgRemap.get(t.source) });
}
for (const m of json.materials || []) {
  const bct = m.pbrMetallicRoughness?.baseColorTexture;
  if (bct) bct.index = texRemap.get(bct.index);
}
for (const a of json.accessors || []) {
  if (a.bufferView !== undefined) a.bufferView = bvRemap.get(a.bufferView);
}

json.bufferViews = newBVs;
json.images = newImages;
json.textures = newTextures;
json.samplers = json.samplers || [];
json.buffers = [{ byteLength: offset }];

// ── 4. Write the new GLB ──
let jsonBuf = Buffer.from(JSON.stringify(json), 'utf8');
if (jsonBuf.length % 4) jsonBuf = Buffer.concat([jsonBuf, Buffer.alloc(4 - (jsonBuf.length % 4), 0x20)]);
let binBuf = Buffer.concat(chunks);
if (binBuf.length % 4) binBuf = Buffer.concat([binBuf, Buffer.alloc(4 - (binBuf.length % 4))]);

const out = Buffer.alloc(12 + 8 + jsonBuf.length + 8 + binBuf.length);
out.writeUInt32LE(0x46546c67, 0);
out.writeUInt32LE(2, 4);
out.writeUInt32LE(out.length, 8);
out.writeUInt32LE(jsonBuf.length, 12);
out.writeUInt32LE(0x4e4f534a, 16); // 'JSON'
jsonBuf.copy(out, 20);
out.writeUInt32LE(binBuf.length, 20 + jsonBuf.length);
out.writeUInt32LE(0x004e4942, 24 + jsonBuf.length); // 'BIN\0'
binBuf.copy(out, 28 + jsonBuf.length);
writeFileSync(outPath, out);
console.log(`${inPath}: ${(buf.length / 1e6).toFixed(1)}MB → ${(out.length / 1e6).toFixed(1)}MB`);
