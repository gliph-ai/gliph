// room0 3D — world builder. Turns a room definition from engine.js (tile grid +
// props + doors) into a Three.js group: themed floor, instanced walls with a
// glowing trim, door portals, prop placement (procedural now, generated GLB
// swapped in when it loads) and per-room accent lighting. 1 tile = 1 world unit;
// tile (gx, gy) → world (gx, 0, gy).
import * as THREE from 'three';
import { WALL } from '../engine';
import { buildProp, loadPropModel, mat } from './props3d';

export const WALL_H = 1.7;

// Per-room visual theme: background/fog, floor canvas painter, wall tint, trim
// glow, key-light colour.
export const THEMES = {
  office: {
    bg: 0x070b16, fog: [0x070b16, 18, 46], wall: 0x202c46, trim: 0x39c6ff,
    hemi: [0x35507a, 0x0a1020, 0.55], sun: [0xaac8ff, 0.85],
    floor: (c) => paintPanels(c, '#141f38', '#1a2744', 'rgba(70,160,235,0.28)'),
  },
  garden: {
    bg: 0x0a1810, fog: [0x0a1810, 14, 36], wall: 0x24402c, trim: 0x7ad9a0,
    hemi: [0x4a7a55, 0x0a180e, 0.6], sun: [0xcfe8b0, 0.9],
    floor: (c) => paintGrass(c),
  },
  kitchen: {
    bg: 0x171020, fog: [0x171020, 14, 36], wall: 0x3a3048, trim: 0xd8a13c,
    hemi: [0x6a5a80, 0x14101e, 0.6], sun: [0xffe8c8, 0.85],
    floor: (c) => paintChecker(c, '#241d33', '#2e2540'),
  },
  lounge: {
    bg: 0x160e12, fog: [0x160e12, 13, 34], wall: 0x33202a, trim: 0xff9a5c,
    hemi: [0x6a4a3a, 0x140c0a, 0.5], sun: [0xffc890, 0.7],
    floor: (c) => paintPlanks(c, '#2c1c14', '#38251a'),
  },
  beach: {
    bg: 0x12242e, fog: [0x12242e, 16, 40], wall: 0x8a7a5c, trim: 0xffd166,
    hemi: [0x7ab8d8, 0x1a2a30, 0.75], sun: [0xffe0b0, 1.0],
    floor: (c) => paintSand(c),
  },
};

// ── Floor texture painters (128px canvas, repeated per tile) ─────────────────
function canvasTexture(painter, repeatX, repeatY) {
  const c = document.createElement('canvas');
  c.width = 128;
  c.height = 128;
  painter(c.getContext('2d'));
  const t = new THREE.CanvasTexture(c);
  t.wrapS = THREE.RepeatWrapping;
  t.wrapT = THREE.RepeatWrapping;
  t.repeat.set(repeatX, repeatY);
  t.colorSpace = THREE.SRGBColorSpace;
  t.anisotropy = 4;
  return t;
}

function paintPanels(ctx, base, panel, seam) {
  ctx.fillStyle = base;
  ctx.fillRect(0, 0, 128, 128);
  ctx.fillStyle = panel;
  ctx.fillRect(6, 6, 116, 116);
  ctx.strokeStyle = seam;
  ctx.lineWidth = 2;
  ctx.strokeRect(6, 6, 116, 116);
  const g = ctx.createLinearGradient(0, 0, 128, 128);
  g.addColorStop(0, 'rgba(150,200,255,0.07)');
  g.addColorStop(0.5, 'rgba(0,0,0,0)');
  ctx.fillStyle = g;
  ctx.fillRect(6, 6, 116, 116);
}

function paintGrass(ctx) {
  ctx.fillStyle = '#1d3d24';
  ctx.fillRect(0, 0, 128, 128);
  for (let i = 0; i < 260; i++) {
    const x = Math.random() * 128, y = Math.random() * 128;
    ctx.fillStyle = ['#25492c', '#2c5434', '#193520'][i % 3];
    ctx.fillRect(x, y, 2, 3 + Math.random() * 3);
  }
}

function paintChecker(ctx, a, b) {
  for (let y = 0; y < 2; y++) {
    for (let x = 0; x < 2; x++) {
      ctx.fillStyle = (x + y) % 2 ? a : b;
      ctx.fillRect(x * 64, y * 64, 64, 64);
    }
  }
  ctx.strokeStyle = 'rgba(0,0,0,0.35)';
  ctx.lineWidth = 2;
  ctx.strokeRect(0, 0, 128, 128);
  ctx.strokeRect(0, 64, 128, 0);
}

function paintPlanks(ctx, a, b) {
  for (let i = 0; i < 4; i++) {
    ctx.fillStyle = i % 2 ? a : b;
    ctx.fillRect(0, i * 32, 128, 32);
    ctx.fillStyle = 'rgba(0,0,0,0.3)';
    ctx.fillRect(0, i * 32, 128, 2);
  }
  for (let i = 0; i < 26; i++) {
    ctx.fillStyle = 'rgba(0,0,0,0.12)';
    ctx.fillRect(Math.random() * 128, Math.random() * 128, 10 + Math.random() * 24, 1);
  }
}

function paintSand(ctx) {
  ctx.fillStyle = '#c8a86a';
  ctx.fillRect(0, 0, 128, 128);
  for (let i = 0; i < 300; i++) {
    ctx.fillStyle = ['#bfa062', '#d2b276', '#b89858'][i % 3];
    ctx.fillRect(Math.random() * 128, Math.random() * 128, 2, 2);
  }
}

// Deterministic per-tile hash (same as the 2D engine used) for variation.
function tileHash(gx, gy) {
  return (((gx * 73856093) ^ (gy * 19349663)) >>> 0);
}

// ── Time of day ──────────────────────────────────────────────────────────────
// daylightAt(hour) → { day, warm }. `day` ramps 0→1 over sunrise (6–9h) and
// back down over sunset (17–20:30); `warm` peaks around the golden hours.
function smoothstep(a, b, x) {
  const t = Math.min(1, Math.max(0, (x - a) / (b - a)));
  return t * t * (3 - 2 * t);
}

export function daylightAt(hour) {
  const day = smoothstep(6, 9, hour) * (1 - smoothstep(17, 20.5, hour));
  const warm = Math.min(1,
    Math.exp(-(((hour - 7.5) / 1.2) ** 2)) + Math.exp(-(((hour - 18.75) / 1.4) ** 2)));
  return { day, warm };
}

// ── Sky ──────────────────────────────────────────────────────────────────────
// Paint a sky onto a canvas for the given time of day: vertical gradient,
// sun/moon tracking the hour, soft clouds by day, stars at night. Used as the
// scene background AND as the office windows' glass texture.
const SKY = {
  dayTop: [70, 140, 216], dayHorizon: [174, 224, 255],
  nightTop: [4, 6, 14], nightHorizon: [16, 26, 46],
  warmTint: [255, 154, 92],
};

function mixc(a, b, t) {
  return [
    a[0] + (b[0] - a[0]) * t,
    a[1] + (b[1] - a[1]) * t,
    a[2] + (b[2] - a[2]) * t,
  ];
}
const css = (c, a = 1) => `rgba(${c[0] | 0},${c[1] | 0},${c[2] | 0},${a})`;

export function paintSky(ctx, w, h, { hour, day, warm }) {
  const night = 1 - day;
  let top = mixc(SKY.nightTop, SKY.dayTop, day);
  let horizon = mixc(SKY.nightHorizon, SKY.dayHorizon, day);
  horizon = mixc(horizon, SKY.warmTint, warm * 0.55);
  top = mixc(top, SKY.warmTint, warm * 0.12);

  const g = ctx.createLinearGradient(0, 0, 0, h);
  g.addColorStop(0, css(top));
  g.addColorStop(0.72, css(mixc(top, horizon, 0.6)));
  g.addColorStop(1, css(horizon));
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, w, h);

  // Stars — deterministic scatter, fading in after dusk.
  if (night > 0.25) {
    const alpha = (night - 0.25) / 0.75;
    for (let i = 0; i < 90; i++) {
      const hsh = ((i * 73856093) ^ 0x9e3779b9) >>> 0;
      const x = (hsh % 997) / 997 * w;
      const y = ((hsh >>> 10) % 883) / 883 * h * 0.7;
      const r = 0.5 + ((hsh >>> 20) % 10) / 10;
      ctx.fillStyle = `rgba(220,230,255,${alpha * (0.35 + ((hsh >>> 5) % 10) / 16)})`;
      ctx.beginPath();
      ctx.arc(x, y, r, 0, Math.PI * 2);
      ctx.fill();
    }
  }

  // Sun (day) sweeping east→west, or a pale moon parked high at night.
  const t = Math.min(1, Math.max(0, (hour - 6) / 14.5));
  if (day > 0.03) {
    const sx = w * (0.1 + t * 0.8);
    const sy = h * (0.62 - Math.sin(t * Math.PI) * 0.42);
    const col = mixc([255, 244, 214], SKY.warmTint, warm * 0.8);
    const glow = ctx.createRadialGradient(sx, sy, 2, sx, sy, w * 0.16);
    glow.addColorStop(0, css(col, 0.95 * day));
    glow.addColorStop(0.25, css(col, 0.4 * day));
    glow.addColorStop(1, css(col, 0));
    ctx.fillStyle = glow;
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = css(col, day);
    ctx.beginPath();
    ctx.arc(sx, sy, w * 0.035, 0, Math.PI * 2);
    ctx.fill();
  } else {
    const mx = w * 0.72;
    const my = h * 0.2;
    ctx.fillStyle = 'rgba(200,214,240,0.85)';
    ctx.beginPath();
    ctx.arc(mx, my, w * 0.03, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = css(SKY.nightTop, 0.9); // crescent bite
    ctx.beginPath();
    ctx.arc(mx - w * 0.014, my - w * 0.008, w * 0.026, 0, Math.PI * 2);
    ctx.fill();
  }

  // Clouds — soft layered blobs, only meaningful in daylight.
  if (day > 0.12) {
    ctx.fillStyle = `rgba(255,255,255,${0.16 * day})`;
    for (let i = 0; i < 7; i++) {
      const hsh = ((i * 19349663) ^ 0x85ebca6b) >>> 0;
      const cx = ((hsh % 811) / 811 + t * 0.08) % 1 * w;
      const cy = h * (0.18 + ((hsh >>> 8) % 500) / 500 * 0.42);
      const cw = w * (0.1 + ((hsh >>> 16) % 100) / 100 * 0.12);
      for (let b = 0; b < 4; b++) {
        ctx.beginPath();
        ctx.ellipse(cx + (b - 1.5) * cw * 0.32, cy + (b % 2) * cw * 0.08, cw * 0.42, cw * 0.2, 0, 0, Math.PI * 2);
        ctx.fill();
      }
    }
  }

  return { horizon, top };
}

// ── Wall decorations ─────────────────────────────────────────────────────────
// One-off (non-repeating) canvas texture for posters/paintings/vines.
function artTexture(w, h, painter) {
  const c = document.createElement('canvas');
  c.width = w;
  c.height = h;
  painter(c.getContext('2d'), w, h);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  t.anisotropy = 4;
  return t;
}

// Abstract poster art (no readable text — just shapes and "text" bars).
const POSTER_PAINTERS = [
  (ctx, w, h) => { // sunrise circle
    const g = ctx.createLinearGradient(0, 0, 0, h);
    g.addColorStop(0, '#1c2c50');
    g.addColorStop(1, '#452b60');
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = '#ffb35c';
    ctx.beginPath();
    ctx.arc(w / 2, h * 0.42, w * 0.24, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = 'rgba(255,255,255,0.75)';
    ctx.fillRect(w * 0.2, h * 0.72, w * 0.6, 6);
    ctx.fillRect(w * 0.3, h * 0.8, w * 0.4, 4);
  },
  (ctx, w, h) => { // diagonal stripes
    ctx.fillStyle = '#14323a';
    ctx.fillRect(0, 0, w, h);
    ctx.strokeStyle = '#39c6ff';
    ctx.lineWidth = 9;
    for (let i = -h; i < w + h; i += 26) {
      ctx.beginPath();
      ctx.moveTo(i, h);
      ctx.lineTo(i + h, 0);
      ctx.stroke();
    }
    ctx.fillStyle = '#eaf4ff';
    ctx.fillRect(w * 0.16, h * 0.78, w * 0.68, 7);
  },
  (ctx, w, h) => { // "metrics" chart
    ctx.fillStyle = '#0f1d33';
    ctx.fillRect(0, 0, w, h);
    ctx.strokeStyle = 'rgba(140,180,220,0.25)';
    for (let y = 20; y < h - 30; y += 22) {
      ctx.beginPath();
      ctx.moveTo(12, y);
      ctx.lineTo(w - 12, y);
      ctx.stroke();
    }
    ctx.strokeStyle = '#3fd68f';
    ctx.lineWidth = 5;
    ctx.beginPath();
    ctx.moveTo(14, h * 0.7);
    ctx.quadraticCurveTo(w * 0.45, h * 0.75, w * 0.6, h * 0.4);
    ctx.quadraticCurveTo(w * 0.78, h * 0.18, w - 14, h * 0.22);
    ctx.stroke();
    ctx.fillStyle = 'rgba(255,255,255,0.7)';
    ctx.fillRect(14, h - 22, w * 0.5, 6);
  },
  (ctx, w, h) => { // rocket blocks
    ctx.fillStyle = '#3a2148';
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = '#ff7a6e';
    ctx.fillRect(w * 0.2, h * 0.18, w * 0.6, h * 0.12);
    ctx.fillStyle = '#ffd166';
    ctx.fillRect(w * 0.2, h * 0.36, w * 0.38, h * 0.12);
    ctx.fillStyle = '#39c6ff';
    ctx.fillRect(w * 0.2, h * 0.54, w * 0.5, h * 0.12);
    ctx.fillStyle = 'rgba(255,255,255,0.75)';
    ctx.fillRect(w * 0.2, h * 0.8, w * 0.6, 5);
  },
];

const PAINTING_PAINTERS = [
  (ctx, w, h) => { // dusk mountains
    const g = ctx.createLinearGradient(0, 0, 0, h);
    g.addColorStop(0, '#f2b56a');
    g.addColorStop(0.6, '#b0526a');
    g.addColorStop(1, '#3a2140');
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = '#2a1830';
    ctx.beginPath();
    ctx.moveTo(0, h);
    ctx.lineTo(w * 0.3, h * 0.45);
    ctx.lineTo(w * 0.52, h * 0.75);
    ctx.lineTo(w * 0.72, h * 0.35);
    ctx.lineTo(w, h * 0.8);
    ctx.lineTo(w, h);
    ctx.closePath();
    ctx.fill();
  },
  (ctx, w, h) => { // abstract blobs
    ctx.fillStyle = '#20242e';
    ctx.fillRect(0, 0, w, h);
    for (const [x, y, r, col] of [
      [w * 0.3, h * 0.4, 26, '#c26a4a'], [w * 0.62, h * 0.55, 34, '#8a5ad0'],
      [w * 0.72, h * 0.3, 16, '#ffd166'], [w * 0.42, h * 0.68, 20, '#3fae6a'],
    ]) {
      ctx.fillStyle = col;
      ctx.beginPath();
      ctx.arc(x, y, r, 0, Math.PI * 2);
      ctx.fill();
    }
  },
  (ctx, w, h) => { // still-life bottle
    ctx.fillStyle = '#33261c';
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = '#1c1410';
    ctx.fillRect(0, h * 0.68, w, h * 0.32);
    ctx.fillStyle = '#7a3a2a';
    ctx.fillRect(w * 0.42, h * 0.3, w * 0.16, h * 0.4);
    ctx.fillRect(w * 0.46, h * 0.2, w * 0.08, h * 0.12);
    ctx.fillStyle = '#d8a13c';
    ctx.beginPath();
    ctx.arc(w * 0.68, h * 0.62, w * 0.09, 0, Math.PI * 2);
    ctx.fill();
  },
];

function paintVines(ctx, w, h) {
  ctx.clearRect(0, 0, w, h);
  for (let s = 0; s < 5; s++) {
    let x = w * (0.12 + s * 0.19) + (Math.random() - 0.5) * 8;
    ctx.strokeStyle = '#2a5a34';
    ctx.lineWidth = 3;
    ctx.beginPath();
    ctx.moveTo(x, 0);
    const len = h * (0.5 + Math.random() * 0.5);
    for (let y = 0; y < len; y += 8) {
      x += (Math.random() - 0.5) * 6;
      ctx.lineTo(x, y);
    }
    ctx.stroke();
    for (let y = 6; y < len; y += 10 + Math.random() * 8) {
      ctx.fillStyle = ['#3a7d45', '#45965a', '#2f6b3a'][(s + (y | 0)) % 3];
      ctx.beginPath();
      ctx.ellipse(x + (Math.random() - 0.5) * 14, y, 6, 4, Math.random() * Math.PI, 0, Math.PI * 2);
      ctx.fill();
    }
  }
}

function paintChalkboard(ctx, w, h) {
  ctx.fillStyle = '#1e2b24';
  ctx.fillRect(0, 0, w, h);
  ctx.strokeStyle = 'rgba(240,240,220,0.7)';
  ctx.lineWidth = 3;
  for (const [y, len] of [[0.22, 0.55], [0.38, 0.4], [0.54, 0.62], [0.7, 0.35]]) {
    ctx.beginPath();
    ctx.moveTo(w * 0.14, h * y);
    ctx.lineTo(w * (0.14 + len), h * y);
    ctx.stroke();
  }
  ctx.strokeStyle = 'rgba(255,200,120,0.8)';
  ctx.beginPath();
  ctx.arc(w * 0.78, h * 0.3, 10, 0, Math.PI * 2);
  ctx.stroke();
}

// Decorate the interior faces of a room's walls: windows (glass shared via
// windowMat so the day/night cycle can tint it), posters, a working clock,
// pantry shelves, paintings, vines, towels… Purely cosmetic, all procedural.
function decorateWalls(room, theme, group, animated) {
  const doors = room.doors || [];
  const nearDoor = (x, z) => doors.some((d) => Math.abs(d.x - x) + Math.abs(d.y - z) <= 1);
  // Interior-facing wall cells, one contiguous run per wall (corners excluded)
  // so the placement rhythm (every Nth cell) distributes along each wall.
  const sides = [];
  for (let gx = 1; gx < room.w - 1; gx++) {
    if (!nearDoor(gx, 0)) sides.push({ x: gx, z: 0, ry: 0, nx: 0, nz: 1 });
  }
  for (let gx = 1; gx < room.w - 1; gx++) {
    if (!nearDoor(gx, room.h - 1)) sides.push({ x: gx, z: room.h - 1, ry: Math.PI, nx: 0, nz: -1 });
  }
  for (let gz = 1; gz < room.h - 1; gz++) {
    if (!nearDoor(0, gz)) sides.push({ x: 0, z: gz, ry: Math.PI / 2, nx: 1, nz: 0 });
  }
  for (let gz = 1; gz < room.h - 1; gz++) {
    if (!nearDoor(room.w - 1, gz)) sides.push({ x: room.w - 1, z: gz, ry: -Math.PI / 2, nx: -1, nz: 0 });
  }

  const windowMat = new THREE.MeshStandardMaterial({
    color: 0xbfe3ff, emissive: 0x9fc8ff, emissiveIntensity: 1.0, roughness: 0.2,
  });

  const mount = (cell, obj, y) => {
    obj.position.set(cell.x + cell.nx * 0.52, y, cell.z + cell.nz * 0.52);
    obj.rotation.y = cell.ry;
    group.add(obj);
  };

  const plane = (w, h, material) => new THREE.Mesh(new THREE.PlaneGeometry(w, h), material);

  const makeWindow = () => {
    const g = new THREE.Group();
    g.add(plane(1.1, 0.7, windowMat));
    const frame = mat(0x2a3a55, { roughness: 0.7 });
    for (const [w, h, x, y] of [[1.2, 0.06, 0, 0.38], [1.2, 0.06, 0, -0.38], [0.06, 0.82, -0.57, 0], [0.06, 0.82, 0.57, 0], [0.04, 0.76, 0, 0], [1.14, 0.04, 0, 0]]) {
      const bar = new THREE.Mesh(new THREE.BoxGeometry(w, h, 0.05), frame);
      bar.position.set(x, y, 0.01);
      g.add(bar);
    }
    return g;
  };

  const makePoster = (variant) => {
    const t = artTexture(128, 170, POSTER_PAINTERS[variant % POSTER_PAINTERS.length]);
    const p = plane(0.62, 0.82, new THREE.MeshStandardMaterial({ map: t, roughness: 0.85 }));
    p.rotation.z = ((variant % 3) - 1) * 0.03; // slightly crooked, like real offices
    return p;
  };

  const makeClock = () => {
    const g = new THREE.Group();
    const face = new THREE.Mesh(new THREE.CircleGeometry(0.22, 24), mat(0xe8eef8, { roughness: 0.4 }));
    g.add(face);
    const rim = new THREE.Mesh(new THREE.TorusGeometry(0.22, 0.025, 8, 24), mat(0x2a3a55));
    g.add(rim);
    const hourHand = new THREE.Mesh(new THREE.BoxGeometry(0.02, 0.1, 0.01), mat(0x1a2438));
    hourHand.geometry.translate(0, 0.05, 0);
    hourHand.position.z = 0.012;
    const minHand = new THREE.Mesh(new THREE.BoxGeometry(0.014, 0.16, 0.01), mat(0x1a2438));
    minHand.geometry.translate(0, 0.08, 0);
    minHand.position.z = 0.016;
    g.add(hourHand, minHand);
    animated.push({ node: hourHand, kind: 'clockHour' });
    animated.push({ node: minHand, kind: 'clockMin' });
    return g;
  };

  const makeShelf = (seed) => {
    const g = new THREE.Group();
    const board = new THREE.Mesh(new THREE.BoxGeometry(0.95, 0.05, 0.2), mat(0x6b4a2f));
    g.add(board);
    const jarCols = [0xd8a13c, 0x7ad9a0, 0xc26a4a, 0x9fc8ff];
    for (let i = 0; i < 3 + (seed % 2); i++) {
      const jar = new THREE.Mesh(
        new THREE.CylinderGeometry(0.06, 0.06, 0.14 + (i % 2) * 0.05, 10),
        mat(jarCols[(seed + i) % jarCols.length], { transparent: true, opacity: 0.85, roughness: 0.3 }),
      );
      jar.position.set(-0.32 + i * 0.22, 0.1 + (i % 2) * 0.025, 0.02);
      g.add(jar);
    }
    return g;
  };

  const makePainting = (variant) => {
    const g = new THREE.Group();
    const t = artTexture(160, 120, PAINTING_PAINTERS[variant % PAINTING_PAINTERS.length]);
    g.add(plane(0.72, 0.54, new THREE.MeshStandardMaterial({ map: t, roughness: 0.9 })));
    const frame = mat(0x8a6a35, { metalness: 0.4, roughness: 0.5 });
    for (const [w, h, x, y] of [[0.8, 0.05, 0, 0.29], [0.8, 0.05, 0, -0.29], [0.05, 0.62, -0.38, 0], [0.05, 0.62, 0.38, 0]]) {
      const bar = new THREE.Mesh(new THREE.BoxGeometry(w, h, 0.04), frame);
      bar.position.set(x, y, 0.01);
      g.add(bar);
    }
    return g;
  };

  const makeSconce = () => {
    const g = new THREE.Group();
    const cup = new THREE.Mesh(new THREE.CylinderGeometry(0.09, 0.05, 0.14, 8, 1, true), mat(0x3a2a20));
    g.add(cup);
    const glow = new THREE.Mesh(
      new THREE.SphereGeometry(0.05, 8, 6),
      mat(0xffd9a0, { emissive: 0xffb35c, emissiveIntensity: 2.2 }),
    );
    glow.position.y = 0.06;
    g.add(glow);
    return g;
  };

  const makeVines = () => {
    const t = artTexture(96, 128, paintVines);
    const m = new THREE.MeshStandardMaterial({ map: t, transparent: true, roughness: 0.9, side: THREE.DoubleSide });
    const p = plane(0.95, 1.15, m);
    return p;
  };

  const makeLattice = () => {
    const g = new THREE.Group();
    const wood = mat(0x8a6a45, { roughness: 0.9 });
    for (let i = -2; i <= 2; i++) {
      const a = new THREE.Mesh(new THREE.BoxGeometry(0.04, 1.0, 0.02), wood);
      a.position.x = i * 0.18;
      a.rotation.z = 0.5;
      const b = new THREE.Mesh(new THREE.BoxGeometry(0.04, 1.0, 0.02), wood);
      b.position.x = i * 0.18;
      b.rotation.z = -0.5;
      g.add(a, b);
    }
    return g;
  };

  const makeTowel = (seed) => {
    const cols = [0xe86a80, 0x39c6ff, 0xffd166, 0x7ad9a0];
    const g = new THREE.Group();
    const rail = new THREE.Mesh(new THREE.CylinderGeometry(0.02, 0.02, 0.7, 8), mat(0x8a6a45));
    rail.rotation.z = Math.PI / 2;
    rail.position.y = 0.3;
    g.add(rail);
    const towel = plane(0.5, 0.55, mat(cols[seed % cols.length], { roughness: 0.95 }));
    towel.position.y = 0.02;
    g.add(towel);
    return g;
  };

  const makeLifeRing = () => {
    const g = new THREE.Group();
    const ring = new THREE.Mesh(new THREE.TorusGeometry(0.24, 0.08, 10, 24), mat(0xf2f2ea, { roughness: 0.6 }));
    g.add(ring);
    for (let i = 0; i < 4; i++) {
      const stripe = new THREE.Mesh(new THREE.BoxGeometry(0.1, 0.1, 0.17), mat(0xd0453a, { roughness: 0.6 }));
      const a = (i / 4) * Math.PI * 2 + Math.PI / 4;
      stripe.position.set(Math.cos(a) * 0.24, Math.sin(a) * 0.24, 0);
      g.add(stripe);
    }
    return g;
  };

  const makePlankSign = () => {
    const g = new THREE.Group();
    const wood = mat(0xb08a5a, { roughness: 0.95 });
    for (let i = 0; i < 3; i++) {
      const p = new THREE.Mesh(new THREE.BoxGeometry(0.8, 0.16, 0.03), wood);
      p.position.y = (i - 1) * 0.19;
      p.rotation.z = (i - 1) * 0.06;
      g.add(p);
    }
    const shell = new THREE.Mesh(new THREE.CircleGeometry(0.07, 8), mat(0xe86a80));
    shell.position.z = 0.03;
    g.add(shell);
    return g;
  };

  // Per-room placement: walk the candidate cells, drop decorations on a beat.
  let clockPlaced = false;
  let boardPlaced = false;
  let ringPlaced = false;
  let signPlaced = false;
  sides.forEach((cell, i) => {
    const seed = tileHash(cell.x, cell.z);
    switch (room.id) {
      case 'office':
        if (i % 4 === 1) mount(cell, makeWindow(), 1.05);
        else if (i % 4 === 3) {
          if (!clockPlaced && cell.z === 0) { mount(cell, makeClock(), 1.15); clockPlaced = true; }
          else mount(cell, makePoster(seed), 1.05);
        }
        break;
      case 'kitchen':
        if (i % 3 === 0) mount(cell, makeShelf(seed), 1.1);
        else if (i % 6 === 1) {
          if (!boardPlaced) { mount(cell, plane(0.9, 0.62, new THREE.MeshStandardMaterial({ map: artTexture(140, 96, paintChalkboard), roughness: 0.9 })), 1.05); boardPlaced = true; }
          else mount(cell, makeWindow(), 1.05);
        }
        break;
      case 'lounge':
        if (i % 4 === 0) mount(cell, makePainting(seed), 1.05);
        else if (i % 4 === 2) mount(cell, makeSconce(), 1.25);
        break;
      case 'garden':
        if (i % 2 === 0) mount(cell, makeVines(), 1.12);
        else if (i % 6 === 3) mount(cell, makeLattice(), 0.85);
        break;
      case 'beach':
        if (!ringPlaced && i % 5 === 2) { mount(cell, makeLifeRing(), 1.1); ringPlaced = true; }
        else if (!signPlaced && i % 5 === 4) { mount(cell, makePlankSign(), 1.1); signPlaced = true; }
        else if (i % 5 === 0) mount(cell, makeTowel(seed), 0.95);
        break;
      default:
        break;
    }
  });

  return windowMat;
}

// Sittable prop types → seat height + pose. Facing is stored per placement
// (props face the room centre; a chair next to a desk faces the desk and
// becomes a typing workstation instead).
export const SEATS = {
  chair: { y: 0.38, pose: 'sit' },
  sofa: { y: 0.32, pose: 'sit' },
  bench: { y: 0.32, pose: 'sit' },
  lounger: { y: 0.34, pose: 'lie' },
};

// Build the full static scene group for a room. Returns everything the game
// loop needs: the group, animated node list, seat list and the door lights.
export function buildRoomScene(room) {
  const theme = THEMES[room.id] || THEMES.office;
  const group = new THREE.Group();
  const animated = []; // {node, kind} — spun/flickered/bobbed per frame
  const seats = []; // {x, z, y, ry, key}
  const lights = []; // {light, base} — accent lights, boosted after dark
  const nightLights = []; // {light, base} — lamps that fade IN after dark
  // Warm lamp glow shared by desk lamps + wall lights (intensity follows night).
  const lampGlowMat = new THREE.MeshStandardMaterial({
    color: 0xffe2b0, emissive: 0xffc880, emissiveIntensity: 0.5, roughness: 0.4,
  });

  // ── Floor: one plane per room with a repeating themed canvas texture ──
  const floorTex = canvasTexture(theme.floor, room.w, room.h);
  const floorMat = new THREE.MeshStandardMaterial({ map: floorTex, roughness: 0.92, metalness: 0.02 });
  const floor = new THREE.Mesh(new THREE.PlaneGeometry(room.w, room.h), floorMat);
  floor.rotation.x = -Math.PI / 2;
  // Tile (0,0) centre sits at world (0,0) → the plane spans [-0.5, w-0.5].
  floor.position.set(room.w / 2 - 0.5, 0, room.h / 2 - 0.5);
  floor.receiveShadow = true;
  group.add(floor);

  // ── Walls: instanced boxes + an emissive trim strip on top ──
  const wallCells = [];
  for (let gy = 0; gy < room.h; gy++) {
    for (let gx = 0; gx < room.w; gx++) {
      if (room.tiles[gy][gx] === WALL) wallCells.push([gx, gy]);
    }
  }
  const wallGeo = new THREE.BoxGeometry(1, WALL_H, 1);
  // Slight self-glow so interior wall faces never read as pitch black — the
  // sun/hemisphere barely graze vertical faces at this camera angle.
  const wallMat = new THREE.MeshStandardMaterial({
    color: theme.wall, roughness: 0.9, flatShading: true,
    emissive: new THREE.Color(theme.wall).lerp(new THREE.Color(0xffffff), 0.12),
    emissiveIntensity: 1.5,
  });
  const walls = new THREE.InstancedMesh(wallGeo, wallMat, wallCells.length);
  walls.castShadow = true;
  walls.receiveShadow = true;
  const trimGeo = new THREE.BoxGeometry(1.02, 0.05, 1.02);
  const trimMat = new THREE.MeshStandardMaterial({
    color: theme.trim, emissive: theme.trim, emissiveIntensity: 0.75, roughness: 0.4,
  });
  const trims = new THREE.InstancedMesh(trimGeo, trimMat, wallCells.length);
  const m4 = new THREE.Matrix4();
  wallCells.forEach(([gx, gy], i) => {
    // Slight deterministic height variation so long runs don't read as one slab.
    const hVar = (tileHash(gx, gy) % 5) * 0.015;
    m4.makeScale(1, 1 + hVar, 1);
    m4.setPosition(gx, (WALL_H * (1 + hVar)) / 2, gy);
    walls.setMatrixAt(i, m4);
    m4.makeTranslation(gx, WALL_H * (1 + hVar) + 0.025, gy);
    trims.setMatrixAt(i, m4);
  });
  group.add(walls, trims);

  // ── Door portals: glowing floor disc + standing ring + point light,
  // plus a glowing name sign above the doorway saying where it leads ──
  const doorSign = (label) => {
    const c = document.createElement('canvas');
    c.width = 256;
    c.height = 64;
    const ctx = c.getContext('2d');
    ctx.fillStyle = 'rgba(8, 14, 28, 0.94)';
    ctx.fillRect(0, 0, 256, 64);
    ctx.strokeStyle = `#${theme.trim.toString(16).padStart(6, '0')}`;
    ctx.lineWidth = 5;
    ctx.strokeRect(3, 3, 250, 58);
    ctx.fillStyle = '#e8f2ff';
    ctx.font = '600 30px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(label, 128, 35);
    const t = new THREE.CanvasTexture(c);
    t.colorSpace = THREE.SRGBColorSpace;
    t.anisotropy = 4;
    return t;
  };
  for (const d of room.doors || []) {
    const portal = new THREE.Group();
    portal.position.set(d.x, 0, d.y);
    const disc = new THREE.Mesh(
      new THREE.CircleGeometry(0.42, 24),
      new THREE.MeshBasicMaterial({ color: theme.trim, transparent: true, opacity: 0.35 }),
    );
    disc.rotation.x = -Math.PI / 2;
    disc.position.y = 0.02;
    portal.add(disc);
    const ring = new THREE.Mesh(
      new THREE.TorusGeometry(0.5, 0.04, 8, 32),
      new THREE.MeshStandardMaterial({ color: theme.trim, emissive: theme.trim, emissiveIntensity: 1.6, roughness: 0.3 }),
    );
    ring.rotation.x = -Math.PI / 2;
    ring.position.y = 0.06;
    ring.name = 'portalRing';
    portal.add(ring);
    const light = new THREE.PointLight(theme.trim, 6, 5, 1.8);
    light.position.y = 1.0;
    portal.add(light);
    lights.push({ light, base: 6 });
    animated.push({ node: ring, kind: 'portal' });

    // Name sign floating above the doorway, facing into the room.
    const inward = d.y === 0 ? [0, 1, 0]
      : d.y === room.h - 1 ? [0, -1, Math.PI]
        : d.x === 0 ? [1, 0, Math.PI / 2] : [-1, 0, -Math.PI / 2];
    const tex = doorSign(d.label);
    const sign = new THREE.Mesh(
      new THREE.PlaneGeometry(1.5, 0.38),
      new THREE.MeshStandardMaterial({
        map: tex, emissive: 0xffffff, emissiveMap: tex, emissiveIntensity: 0.6, roughness: 0.6,
      }),
    );
    sign.position.set(inward[0] * 0.62, 1.95, inward[1] * 0.62);
    sign.rotation.y = inward[2];
    portal.add(sign);
    group.add(portal);
  }

  // ── Floor decals: emissive rings ──
  for (const [dx, dy] of room.decals || []) {
    const ring = new THREE.Mesh(
      new THREE.RingGeometry(0.28, 0.36, 28),
      new THREE.MeshBasicMaterial({ color: theme.trim, transparent: true, opacity: 0.28, side: THREE.DoubleSide }),
    );
    ring.rotation.x = -Math.PI / 2;
    ring.position.set(dx, 0.015, dy);
    animated.push({ node: ring, kind: 'decal', phase: tileHash(dx, dy) % 100 });
    group.add(ring);
  }

  // ── Props: procedural build now, generated GLB swap-in when ready ──
  const cx = room.w / 2;
  const cz = room.h / 2;
  const placed = []; // {p, holder, ry} — for the seat pass below
  for (const p of room.props || []) {
    if (p.type === 'computer') continue; // folded into the desk model
    const holder = new THREE.Group();
    holder.position.set(p.x, 0, p.y);
    // Face the room centre (quantised to 90° so furniture stays orderly).
    const ry = Math.round(Math.atan2(cx - p.x, cz - p.y) / (Math.PI / 2)) * (Math.PI / 2);
    // Chairs get a small deterministic swivel so seating doesn't look stamped.
    const jitter = p.type === 'chair' && tileHash(p.x, p.y) % 2 === 0 ? Math.PI / 12 : 0;
    holder.rotation.y = ry + jitter;
    const proc = buildProp(p.type);
    if (proc) holder.add(proc);
    group.add(holder);
    placed.push({ p, holder, ry });

    const spinNode = proc?.getObjectByName('holoGlobe');
    if (spinNode) animated.push({ node: spinNode, kind: 'spin' });
    const tvScreen = proc?.getObjectByName('tvScreen');
    if (tvScreen) animated.push({ node: tvScreen, kind: 'tv' });
    const fire = proc?.getObjectByName('fire');
    if (fire) animated.push({ node: fire, kind: 'flicker' });
    const jet = proc?.getObjectByName('fountainJet');
    if (jet) animated.push({ node: jet, kind: 'bob' });

    // Swap in the generated asset when (if) it loads — replace only the
    // procedural stand-in; the holder may also carry a desk lamp + its light.
    loadPropModel(p.type).then((tpl) => {
      if (!tpl || !holder.parent) return;
      if (proc) holder.remove(proc);
      holder.add(tpl.clone(true));
    });
  }

  // ── Night lighting: a small warm desk lamp on every desk, and wall lamps
  // near the room corners (the darkest spots after sundown). Their point
  // lights are ~off by day and fade in with the night factor.
  const makeDeskLamp = () => {
    const g = new THREE.Group();
    const dark = new THREE.MeshStandardMaterial({ color: 0x1a2438, roughness: 0.6 });
    const base = new THREE.Mesh(new THREE.CylinderGeometry(0.06, 0.07, 0.03, 10), dark);
    g.add(base);
    const arm = new THREE.Mesh(new THREE.CylinderGeometry(0.012, 0.012, 0.26, 6), dark);
    arm.position.set(0.05, 0.12, 0);
    arm.rotation.z = -0.45;
    g.add(arm);
    const head = new THREE.Mesh(new THREE.ConeGeometry(0.06, 0.09, 10), dark);
    head.position.set(0.14, 0.24, 0);
    head.rotation.z = 1.9;
    g.add(head);
    const bulb = new THREE.Mesh(new THREE.SphereGeometry(0.035, 8, 6), lampGlowMat);
    bulb.position.set(0.17, 0.22, 0);
    g.add(bulb);
    return g;
  };
  for (const e of placed) {
    if (e.p.type !== 'desk') continue;
    const lamp = makeDeskLamp();
    lamp.position.set(0.55, 0.58, 0.1);
    e.holder.add(lamp);
    const l = new THREE.PointLight(0xffd9a0, 0, 4.5, 1.7);
    l.position.set(0.55, 0.95, 0.1);
    e.holder.add(l);
    nightLights.push({ light: l, base: 10 });
  }
  // Pendant lamp hanging over every table (kitchen table, lounge coffee
  // table) — cord + shade + warm bulb, with a night point light under it.
  const makePendant = () => {
    const g = new THREE.Group();
    const dark = new THREE.MeshStandardMaterial({ color: 0x1a2438, roughness: 0.6 });
    const cord = new THREE.Mesh(new THREE.CylinderGeometry(0.014, 0.014, 1.1, 6), dark);
    cord.position.y = 2.2;
    g.add(cord);
    const shade = new THREE.Mesh(new THREE.ConeGeometry(0.24, 0.2, 12, 1, true), dark);
    shade.position.y = 1.62;
    g.add(shade);
    const bulb = new THREE.Mesh(new THREE.SphereGeometry(0.06, 10, 8), lampGlowMat);
    bulb.position.y = 1.52;
    g.add(bulb);
    return g;
  };
  for (const e of placed) {
    if (e.p.type !== 'table') continue;
    const pendant = makePendant();
    pendant.position.set(e.p.x, 0, e.p.y);
    group.add(pendant);
    const l = new THREE.PointLight(0xffd9a0, 0, 5.5, 1.6);
    l.position.set(e.p.x, 1.4, e.p.y);
    group.add(l);
    nightLights.push({ light: l, base: 16 });
  }
  // Standing lamps for open floor with nothing to anchor light: tiki-style
  // lanterns on the beach, slim modern floor lamps flanking the office's
  // central cross (its desks all sit in the quadrants, leaving the middle
  // pitch black at night).
  const FLOOR_LAMPS = {
    beach: { spots: [[5, 5], [9, 9]], style: 'tiki' },
    // One lamp at the inner corner of each quadrant's desk pod — each reads
    // as the pod's lamp, and the four together light the central plaza.
    office: { spots: [[7.2, 7.2], [13.8, 7.2], [7.2, 13.8], [13.8, 13.8]], style: 'modern' },
  };
  const lampCfg = FLOOR_LAMPS[room.id];
  for (const [lx, lz] of lampCfg?.spots || []) {
    const g = new THREE.Group();
    if (lampCfg.style === 'tiki') {
      const pole = new THREE.Mesh(
        new THREE.CylinderGeometry(0.03, 0.045, 1.45, 8),
        new THREE.MeshStandardMaterial({ color: 0x6b4a2f, roughness: 0.9 }),
      );
      pole.position.y = 0.72;
      g.add(pole);
      const cage = new THREE.Mesh(new THREE.BoxGeometry(0.2, 0.24, 0.2), lampGlowMat);
      cage.position.y = 1.55;
      g.add(cage);
      const cap = new THREE.Mesh(
        new THREE.ConeGeometry(0.18, 0.12, 4),
        new THREE.MeshStandardMaterial({ color: 0x3a2a20, roughness: 0.8 }),
      );
      cap.position.y = 1.72;
      g.add(cap);
    } else {
      const steel = new THREE.MeshStandardMaterial({ color: 0x2a3450, roughness: 0.6, metalness: 0.3 });
      const foot = new THREE.Mesh(new THREE.CylinderGeometry(0.16, 0.18, 0.04, 12), steel);
      foot.position.y = 0.02;
      g.add(foot);
      const pole = new THREE.Mesh(new THREE.CylinderGeometry(0.02, 0.025, 1.7, 8), steel);
      pole.position.y = 0.87;
      g.add(pole);
      const shade = new THREE.Mesh(new THREE.ConeGeometry(0.2, 0.22, 12, 1, true), steel);
      shade.position.y = 1.78;
      g.add(shade);
      const bulb = new THREE.Mesh(new THREE.SphereGeometry(0.07, 10, 8), lampGlowMat);
      bulb.position.y = 1.68;
      g.add(bulb);
    }
    g.position.set(lx, 0, lz);
    group.add(g);
    const l = new THREE.PointLight(0xffe2c0, 0, lampCfg.style === 'modern' ? 8 : 7, 1.6);
    l.position.set(lx, 1.6, lz);
    group.add(l);
    nightLights.push({ light: l, base: lampCfg.style === 'modern' ? 16 : 14 });
  }
  const cornerCells = [
    [1, 1, 1, 1], [room.w - 2, 1, -1, 1],
    [1, room.h - 2, 1, -1], [room.w - 2, room.h - 2, -1, -1],
  ];
  for (const [cxc, czc, sx, sz] of cornerCells) {
    const lampG = new THREE.Group();
    // Bracket + glowing orb, tucked into the corner up high.
    const bracket = new THREE.Mesh(
      new THREE.BoxGeometry(0.1, 0.22, 0.1),
      new THREE.MeshStandardMaterial({ color: 0x2a3450, roughness: 0.7 }),
    );
    lampG.add(bracket);
    const orb = new THREE.Mesh(new THREE.SphereGeometry(0.09, 10, 8), lampGlowMat);
    orb.position.y = 0.16;
    lampG.add(orb);
    lampG.position.set(cxc - sx * 0.38, 1.45, czc - sz * 0.38);
    group.add(lampG);
    const l = new THREE.PointLight(0xffe2c0, 0, 9, 1.6);
    l.position.set(cxc + sx * 0.4, 1.7, czc + sz * 0.4);
    group.add(l);
    nightLights.push({ light: l, base: 20 });
  }

  // ── Seats: a chair next to a desk becomes a typing workstation (faces the
  // desk, slides in under it while occupied); chairs and sofas next to a
  // table face the table. Sofas seat TWO, side by side. Everything else
  // keeps its room-centre facing and plain sit/lie pose.
  for (const e of placed) {
    // Trees and palms are climbable: a perch seat on top of the canopy.
    if (e.p.type === 'tree' || e.p.type === 'palm') {
      seats.push({
        x: e.p.x, z: e.p.y, y: e.p.type === 'palm' ? 2.15 : 2.0, ry: e.ry,
        pose: 'sit', perch: true, tuck: null, key: `${e.p.x},${e.p.y}`,
      });
      continue;
    }
    const cfg = SEATS[e.p.type];
    if (!cfg) continue;
    let { pose } = cfg;
    let ry = e.ry;
    let tuck = null;
    if (e.p.type === 'chair' || e.p.type === 'sofa') {
      const host = placed.find(
        (d) => (d.p.type === 'desk' || d.p.type === 'table')
          && Math.abs(d.p.x - e.p.x) + Math.abs(d.p.y - e.p.y) === 1,
      );
      if (host) {
        ry = Math.atan2(host.p.x - e.p.x, host.p.y - e.p.y);
        e.holder.rotation.y = ry;
        if (host.p.type === 'desk' && e.p.type === 'chair') {
          pose = 'type';
          // The desk turns to face its chair too, so the screens face the sitter.
          host.holder.rotation.y = Math.atan2(e.p.x - host.p.x, e.p.y - host.p.y);
        }
        if (e.p.type === 'chair') {
          tuck = {
            holder: e.holder,
            baseX: e.p.x, baseZ: e.p.y,
            tuckX: e.p.x + Math.sin(ry) * 0.32, tuckZ: e.p.y + Math.cos(ry) * 0.32,
          };
        }
      }
    }
    if (e.p.type === 'sofa') {
      // Two seat spots along the sofa's width (perpendicular to its facing).
      const px = Math.cos(ry);
      const pz = -Math.sin(ry);
      for (const off of [-0.33, 0.33]) {
        seats.push({
          x: e.p.x + px * off, z: e.p.y + pz * off,
          y: cfg.y, ry, pose, tuck: null,
          key: `${e.p.x},${e.p.y}:${off < 0 ? 0 : 1}`,
        });
      }
    } else {
      seats.push({ x: e.p.x, z: e.p.y, y: cfg.y, ry, pose, tuck, key: `${e.p.x},${e.p.y}` });
    }
  }

  // ── Accent lights near glowing props (cheap: a few point lights per room) ──
  const glowTypes = { server: 0x39c6ff, holo: 0x39c6ff, fireplace: 0xff7a3c, fountain: 0x5fc8ef, bar: 0xffb347 };
  let accents = 0;
  for (const p of room.props || []) {
    const col = glowTypes[p.type];
    if (!col || accents >= 4) continue;
    const l = new THREE.PointLight(col, 14, 7, 1.6);
    l.position.set(p.x, 1.1, p.y);
    group.add(l);
    lights.push({ light: l, base: 14 });
    accents++;
  }

  // Wall dressing: windows / posters / shelves / paintings / vines / towels.
  const windowMat = decorateWalls(room, theme, group, animated);

  // Interactable landmarks (watch TV, drink from the fountain).
  const tvE = placed.find((e) => e.p.type === 'tv');
  const fountainE = placed.find((e) => e.p.type === 'fountain');

  return {
    group, theme, animated, seats, lights, nightLights, lampGlowMat, trimMat, wallMat, windowMat,
    tv: tvE ? { x: tvE.p.x, z: tvE.p.y } : null,
    fountain: fountainE ? { x: fountainE.p.x, z: fountainE.p.y } : null,
  };
}

// Per-frame animation for the world's animated nodes.
export function tickWorld(animated, t, dt) {
  for (const a of animated) {
    if (a.kind === 'spin') a.node.rotation.y += dt * 0.9;
    else if (a.kind === 'portal') {
      a.node.rotation.z += dt * 0.6;
      a.node.position.y = 0.06 + Math.sin(t * 2.2) * 0.025;
    } else if (a.kind === 'decal') {
      a.node.material.opacity = 0.18 + (Math.sin(t * 1.8 + (a.phase || 0)) + 1) * 0.09;
    } else if (a.kind === 'flicker') {
      a.node.scale.setScalar(0.92 + Math.sin(t * 11) * 0.06 + Math.sin(t * 23 + 1.7) * 0.04);
    } else if (a.kind === 'bob') {
      a.node.position.y = 0.82 + Math.sin(t * 3.1) * 0.04;
      a.node.rotation.y += dt * 2;
    } else if (a.kind === 'tv') {
      a.node.material.emissiveIntensity = 0.75 + Math.sin(t * 9) * 0.2 + Math.sin(t * 2.3) * 0.15;
    } else if (a.kind === 'clockHour') {
      const now = new Date();
      a.node.rotation.z = -((now.getHours() % 12) + now.getMinutes() / 60) * (Math.PI / 6);
    } else if (a.kind === 'clockMin') {
      a.node.rotation.z = -new Date().getMinutes() * (Math.PI / 30);
    }
  }
}
