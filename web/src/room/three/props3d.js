// room0 3D — prop meshes. Every prop type has a procedural low-poly builder
// (always available) and, when a generated GLB exists under
// /room0/models/<type>.glb (listed in manifest.json), the real asset replaces
// the procedural stand-in as soon as it loads. All builders return a Group
// whose base sits at y=0, centred on x/z.
import * as THREE from 'three';
import { GLTFLoader } from 'three/examples/jsm/loaders/GLTFLoader.js';

// ── Shared materials (flat-shaded, low-poly look) ────────────────────────────
const MATS = new Map();
export function mat(color, opts = {}) {
  const key = `${color}|${opts.emissive || 0}|${opts.emissiveIntensity || 0}|${opts.metalness ?? ''}|${opts.roughness ?? ''}|${opts.transparent ? 1 : 0}|${opts.opacity ?? ''}`;
  if (!MATS.has(key)) {
    MATS.set(key, new THREE.MeshStandardMaterial({
      color,
      flatShading: true,
      roughness: opts.roughness ?? 0.85,
      metalness: opts.metalness ?? 0.05,
      emissive: opts.emissive || 0x000000,
      emissiveIntensity: opts.emissiveIntensity ?? 1,
      transparent: !!opts.transparent,
      opacity: opts.opacity ?? 1,
    }));
  }
  return MATS.get(key);
}

function box(w, h, d, material, x = 0, y = 0, z = 0) {
  const m = new THREE.Mesh(new THREE.BoxGeometry(w, h, d), material);
  m.position.set(x, y, z);
  m.castShadow = true;
  m.receiveShadow = true;
  return m;
}

function cyl(rt, rb, h, material, x = 0, y = 0, z = 0, seg = 10) {
  const m = new THREE.Mesh(new THREE.CylinderGeometry(rt, rb, h, seg), material);
  m.position.set(x, y, z);
  m.castShadow = true;
  m.receiveShadow = true;
  return m;
}

function sph(r, material, x = 0, y = 0, z = 0, seg = 10) {
  const m = new THREE.Mesh(new THREE.SphereGeometry(r, seg, Math.max(6, seg - 2)), material);
  m.position.set(x, y, z);
  m.castShadow = true;
  m.receiveShadow = true;
  return m;
}

// ── Procedural builders ──────────────────────────────────────────────────────
const NAVY = 0x22304e;
const STEEL = 0x3c4c6e;
const DARK = 0x1a2438;
const CYAN = 0x39c6ff;
const WOOD = 0x6b4a2f;

const BUILDERS = {
  desk() {
    const g = new THREE.Group();
    g.add(box(1.1, 0.06, 0.6, mat(STEEL), 0, 0.72, 0));
    g.add(box(0.06, 0.72, 0.55, mat(DARK), -0.5, 0.36, 0));
    g.add(box(0.06, 0.72, 0.55, mat(DARK), 0.5, 0.36, 0));
    // Dual monitors with glowing screens.
    for (const sx of [-0.24, 0.24]) {
      g.add(box(0.4, 0.26, 0.03, mat(DARK), sx, 0.98, -0.12));
      g.add(box(0.36, 0.22, 0.012, mat(0x0c1626, { emissive: CYAN, emissiveIntensity: 0.9 }), sx, 0.98, -0.1));
      g.add(box(0.05, 0.12, 0.05, mat(DARK), sx, 0.81, -0.14));
    }
    g.add(box(0.34, 0.02, 0.12, mat(DARK), 0, 0.76, 0.14)); // keyboard
    return g;
  },
  chair() {
    const g = new THREE.Group();
    g.add(cyl(0.035, 0.05, 0.3, mat(DARK), 0, 0.15, 0));
    for (let i = 0; i < 5; i++) {
      const a = (i / 5) * Math.PI * 2;
      g.add(box(0.2, 0.03, 0.05, mat(DARK), Math.cos(a) * 0.12, 0.03, Math.sin(a) * 0.12).rotateY(-a));
    }
    g.add(box(0.42, 0.07, 0.42, mat(NAVY), 0, 0.34, 0));
    g.add(box(0.4, 0.45, 0.07, mat(NAVY), 0, 0.6, -0.19)); // backrest at -Z → faces +Z
    return g;
  },
  server() {
    const g = new THREE.Group();
    g.add(box(0.7, 1.5, 0.6, mat(DARK), 0, 0.75, 0));
    for (let i = 0; i < 5; i++) {
      g.add(box(0.6, 0.04, 0.02, mat(0x0a1420, { emissive: CYAN, emissiveIntensity: 1.4 }), 0, 0.25 + i * 0.27, 0.31));
    }
    return g;
  },
  plant() {
    const g = new THREE.Group();
    g.add(cyl(0.16, 0.12, 0.24, mat(NAVY), 0, 0.12, 0, 8));
    g.add(cyl(0.02, 0.03, 0.3, mat(0x2f5d3a), 0, 0.38, 0, 6));
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2;
      const leaf = box(0.3, 0.02, 0.14, mat(0x3a7d4f), Math.cos(a) * 0.16, 0.52 + (i % 2) * 0.08, Math.sin(a) * 0.16);
      leaf.rotation.set(0.3 * Math.sin(a), -a, 0.35);
      g.add(leaf);
    }
    return g;
  },
  cooler() {
    const g = new THREE.Group();
    g.add(box(0.36, 0.8, 0.36, mat(STEEL), 0, 0.4, 0));
    g.add(cyl(0.14, 0.14, 0.3, mat(0x9fd8ff, { transparent: true, opacity: 0.7, emissive: 0x2a7dc0, emissiveIntensity: 0.4 }), 0, 0.95, 0, 10));
    return g;
  },
  whiteboard() {
    const g = new THREE.Group();
    g.add(box(0.05, 0.06, 0.3, mat(DARK), -0.42, 0.03, 0));
    g.add(box(0.05, 0.06, 0.3, mat(DARK), 0.42, 0.03, 0));
    g.add(box(0.05, 0.9, 0.05, mat(STEEL), -0.42, 0.5, 0));
    g.add(box(0.05, 0.9, 0.05, mat(STEEL), 0.42, 0.5, 0));
    g.add(box(0.9, 0.6, 0.04, mat(0xe8eef8, { roughness: 0.6 }), 0, 0.75, 0));
    g.add(box(0.16, 0.14, 0.045, mat(0xffd166), -0.2, 0.82, 0.002));
    g.add(box(0.14, 0.12, 0.045, mat(0x7ad9a0), 0.15, 0.7, 0.002));
    return g;
  },
  holo() {
    const g = new THREE.Group();
    g.add(cyl(0.5, 0.6, 0.3, mat(DARK, { metalness: 0.3 }), 0, 0.15, 0, 12));
    g.add(cyl(0.42, 0.42, 0.04, mat(0x0a1420, { emissive: CYAN, emissiveIntensity: 1.6 }), 0, 0.33, 0, 12));
    const globe = sph(0.28, mat(0x39c6ff, { transparent: true, opacity: 0.35, emissive: CYAN, emissiveIntensity: 1.2 }), 0, 0.85, 0, 12);
    globe.name = 'holoGlobe'; // spun by the world animator
    g.add(globe);
    return g;
  },
  tree() {
    const g = new THREE.Group();
    g.add(cyl(0.08, 0.12, 0.7, mat(0x5a4030), 0, 0.35, 0, 7));
    g.add(sph(0.5, mat(0x3a7d45), 0, 0.95, 0, 8));
    g.add(sph(0.34, mat(0x45965a), 0.25, 1.15, 0.1, 8));
    return g;
  },
  bench() {
    const g = new THREE.Group();
    g.add(box(0.9, 0.05, 0.32, mat(WOOD), 0, 0.3, 0));
    g.add(box(0.08, 0.3, 0.3, mat(DARK), -0.36, 0.15, 0));
    g.add(box(0.08, 0.3, 0.3, mat(DARK), 0.36, 0.15, 0));
    return g;
  },
  fountain() {
    const g = new THREE.Group();
    g.add(cyl(0.55, 0.6, 0.25, mat(0x8a94a8, { roughness: 0.9 }), 0, 0.125, 0, 12));
    g.add(cyl(0.5, 0.5, 0.06, mat(0x5fc8ef, { transparent: true, opacity: 0.8, emissive: 0x2a8dc0, emissiveIntensity: 0.5 }), 0, 0.27, 0, 12));
    g.add(cyl(0.08, 0.1, 0.4, mat(0x8a94a8), 0, 0.45, 0, 8));
    g.add(cyl(0.25, 0.28, 0.08, mat(0x8a94a8), 0, 0.66, 0, 10));
    const jet = cyl(0.05, 0.02, 0.25, mat(0x9fe0ff, { transparent: true, opacity: 0.7, emissive: 0x4aa8d8, emissiveIntensity: 0.8 }), 0, 0.82, 0, 6);
    jet.name = 'fountainJet';
    g.add(jet);
    return g;
  },
  table() {
    const g = new THREE.Group();
    g.add(cyl(0.5, 0.52, 0.05, mat(0x6b4a2f), 0, 0.6, 0, 16));
    g.add(cyl(0.05, 0.06, 0.58, mat(0x4a3320), 0, 0.3, 0, 8));
    g.add(cyl(0.22, 0.26, 0.05, mat(0x4a3320), 0, 0.03, 0, 12));
    // A little vase so the table doesn't look bare.
    g.add(cyl(0.045, 0.06, 0.1, mat(0x39c6ff, { roughness: 0.35 }), 0.12, 0.68, 0.08, 8));
    g.add(sph(0.055, mat(0xe86a80), 0.12, 0.77, 0.08, 8));
    return g;
  },
  tv() {
    const g = new THREE.Group();
    const dark = mat(0x141a26, { roughness: 0.5 });
    g.add(box(0.5, 0.06, 0.32, dark, 0, 0.03, 0)); // foot
    g.add(box(0.08, 0.5, 0.08, dark, 0, 0.28, 0)); // column
    g.add(box(1.15, 0.68, 0.07, dark, 0, 0.95, 0)); // panel
    // Screen — unique material so the flicker animation doesn't leak into
    // the shared material cache.
    const screen = new THREE.Mesh(
      new THREE.BoxGeometry(1.05, 0.58, 0.015),
      new THREE.MeshStandardMaterial({
        color: 0x0a1626, emissive: 0x3a76c8, emissiveIntensity: 0.9, roughness: 0.25,
      }),
    );
    screen.position.set(0, 0.95, 0.04);
    screen.name = 'tvScreen';
    g.add(screen);
    return g;
  },
  counter() {
    const g = new THREE.Group();
    g.add(box(0.95, 0.7, 0.6, mat(NAVY), 0, 0.35, 0));
    g.add(box(1.02, 0.05, 0.66, mat(WOOD), 0, 0.72, 0));
    g.add(box(0.3, 0.02, 0.2, mat(0x8a6a45), -0.2, 0.75, 0.05));
    return g;
  },
  fridge() {
    const g = new THREE.Group();
    g.add(box(0.6, 1.4, 0.6, mat(STEEL, { metalness: 0.35, roughness: 0.5 }), 0, 0.7, 0));
    g.add(box(0.03, 0.4, 0.04, mat(0xc8d4e8), 0.26, 1.0, 0.31));
    g.add(box(0.58, 0.02, 0.58, mat(DARK), 0, 0.95, 0));
    return g;
  },
  sofa() {
    const g = new THREE.Group();
    const red = mat(0x6e2f3e);
    g.add(box(1.1, 0.3, 0.55, red, 0, 0.25, 0));
    g.add(box(1.1, 0.45, 0.15, red, 0, 0.55, -0.22)); // backrest at -Z → faces +Z
    g.add(box(0.15, 0.42, 0.5, red, -0.5, 0.42, 0));
    g.add(box(0.15, 0.42, 0.5, red, 0.5, 0.42, 0));
    g.add(box(0.48, 0.1, 0.42, mat(0x8a4152), -0.24, 0.44, 0.03));
    g.add(box(0.48, 0.1, 0.42, mat(0x8a4152), 0.24, 0.44, 0.03));
    return g;
  },
  bar() {
    const g = new THREE.Group();
    g.add(box(1.05, 0.85, 0.5, mat(0x4a3528), 0, 0.425, 0));
    g.add(box(1.15, 0.06, 0.6, mat(0x2c1f16), 0, 0.9, 0));
    g.add(box(0.08, 0.16, 0.08, mat(0xd8a13c, { emissive: 0xffb347, emissiveIntensity: 0.35 }), -0.3, 0.98, 0));
    g.add(cyl(0.04, 0.05, 0.18, mat(0x7ad9a0, { transparent: true, opacity: 0.8 }), 0.25, 1.02, 0.05, 8));
    return g;
  },
  fireplace() {
    const g = new THREE.Group();
    g.add(box(0.9, 0.9, 0.45, mat(0x3a3440), 0, 0.45, 0));
    g.add(box(0.55, 0.5, 0.4, mat(0x14100c), 0, 0.35, 0.05));
    const fire = cyl(0.12, 0.2, 0.3, mat(0xff7a3c, { emissive: 0xff5a1c, emissiveIntensity: 1.8, transparent: true, opacity: 0.9 }), 0, 0.3, 0.12, 7);
    fire.name = 'fire';
    g.add(fire);
    g.add(cyl(0.14, 0.14, 0.3, mat(0x2c1f16), 0, 0.12, 0.12, 7).rotateZ(Math.PI / 2));
    return g;
  },
  palm() {
    const g = new THREE.Group();
    const trunk = cyl(0.06, 0.1, 1.2, mat(0x8a6a45), 0.08, 0.6, 0, 7);
    trunk.rotation.z = -0.14;
    g.add(trunk);
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2;
      const frond = box(0.7, 0.02, 0.18, mat(0x3a9d5a), 0.16 + Math.cos(a) * 0.33, 1.24, Math.sin(a) * 0.33);
      frond.rotation.set(0, -a, -0.45);
      g.add(frond);
    }
    g.add(sph(0.06, mat(0x6b4a2f), 0.2, 1.16, 0.08, 7));
    return g;
  },
  umbrella() {
    const g = new THREE.Group();
    const pole = cyl(0.025, 0.03, 1.3, mat(0x8a6a45), 0, 0.65, 0, 7);
    pole.rotation.z = 0.12;
    g.add(pole);
    const top = cyl(0.02, 0.75, 0.3, mat(0xe86a80), -0.08, 1.35, 0, 10);
    top.rotation.z = 0.12;
    g.add(top);
    return g;
  },
  lounger() {
    const g = new THREE.Group();
    g.add(box(0.45, 0.06, 1.0, mat(WOOD), 0, 0.2, 0.1));
    const back = box(0.45, 0.06, 0.5, mat(WOOD), 0, 0.38, -0.4);
    back.rotation.x = -0.7;
    g.add(back);
    g.add(box(0.4, 0.05, 0.9, mat(0xd9c49a), 0, 0.25, 0.1));
    for (const [lx, lz] of [[-0.18, 0.5], [0.18, 0.5], [-0.18, -0.2], [0.18, -0.2]]) {
      g.add(box(0.05, 0.2, 0.05, mat(WOOD), lx, 0.1, lz));
    }
    return g;
  },
};

// ── Carryable / pushable dynamic objects ─────────────────────────────────────
export const DYN_BUILDERS = {
  ball() {
    const g = new THREE.Group();
    const b = sph(0.22, mat(0xe8eef8, { roughness: 0.5 }), 0, 0, 0, 12);
    b.add(sph(0.222, mat(0x2266aa, { roughness: 0.5 }), 0, 0, 0, 6)); // faceted overlay → panel look
    b.children[0].scale.setScalar(0.996);
    g.add(b);
    g.userData.spin = b;
    return g;
  },
  beachball() {
    const g = new THREE.Group();
    const b = sph(0.3, mat(0xff8a5c, { roughness: 0.55 }), 0, 0, 0, 12);
    b.add(sph(0.302, mat(0xfff1dd, { roughness: 0.55 }), 0, 0, 0, 7));
    b.children[0].scale.setScalar(0.996);
    g.add(b);
    g.userData.spin = b;
    return g;
  },
  box() {
    const g = new THREE.Group();
    const b = box(0.42, 0.42, 0.42, mat(0xb08a5a, { roughness: 0.95 }), 0, 0.21, 0);
    b.add(new THREE.Mesh(new THREE.BoxGeometry(0.43, 0.05, 0.43), mat(0x8a6a45)));
    b.children[0].position.y = 0.19;
    g.add(b);
    return g;
  },
  mug() {
    const g = new THREE.Group();
    g.add(cyl(0.09, 0.08, 0.16, mat(0x39c6ff, { roughness: 0.4 }), 0, 0.08, 0, 10));
    g.add(cyl(0.075, 0.075, 0.02, mat(0x3a2a20), 0, 0.155, 0, 10));
    const handle = new THREE.Mesh(new THREE.TorusGeometry(0.05, 0.015, 6, 10), mat(0x39c6ff, { roughness: 0.4 }));
    handle.position.set(0.1, 0.08, 0);
    handle.castShadow = true;
    g.add(handle);
    return g;
  },
};

// Physical metadata for dynamic object types.
export const DYN_TYPES = {
  ball: { radius: 0.22, mass: 0.6, restitution: 0.62, friction: 2.2, kick: 3.4, carryable: true, sfx: 'ball' },
  beachball: { radius: 0.3, mass: 0.3, restitution: 0.72, friction: 1.4, kick: 4.2, carryable: true, sfx: 'ball' },
  box: { radius: 0.3, mass: 2.0, restitution: 0.18, friction: 6.5, kick: 1.2, carryable: true, sfx: 'box' },
  mug: { radius: 0.12, mass: 0.4, restitution: 0.3, friction: 5.0, kick: 1.6, carryable: true, sfx: 'mug' },
};

// How each prop type sits in the world: footprint fit box for GLB normalisation
// (w = max horizontal extent in tiles, h = height in world units). Tuned
// against the 1.28-unit avatar AND each generated mesh's real proportions —
// the fit applies min(h-scale, w-scale), so both axes matter. Reference
// points: desk surface ≈ waist, racks/fridge a head taller than the player,
// loungers person-length.
export const PROP_FIT = {
  desk: { w: 1.45, h: 1.0 }, chair: { w: 0.62, h: 0.95 }, server: { w: 0.85, h: 1.7 },
  plant: { w: 0.65, h: 0.9 }, cooler: { w: 0.45, h: 1.1 }, whiteboard: { w: 1.15, h: 1.35 },
  holo: { w: 1.15, h: 1.2 }, tree: { w: 1.5, h: 2.0 }, bench: { w: 1.0, h: 0.55 },
  fountain: { w: 1.25, h: 0.95 }, counter: { w: 1.05, h: 0.8 }, fridge: { w: 0.8, h: 1.6 },
  table: { w: 1.05, h: 0.78 },
  sofa: { w: 1.5, h: 0.78 }, bar: { w: 1.2, h: 0.95 }, fireplace: { w: 1.0, h: 0.95 },
  palm: { w: 1.6, h: 2.2 }, umbrella: { w: 1.5, h: 1.5 }, lounger: { w: 1.35, h: 0.75 },
  computer: { w: 0.4, h: 0.4 },
};

export function buildProp(type) {
  const b = BUILDERS[type];
  if (!b) return null;
  return b();
}

// ── Generated GLB assets ─────────────────────────────────────────────────────
// manifest.json lists which prop types have a generated model. Models load
// lazily and replace procedural stand-ins in place via the onLoad callback.
let manifestPromise = null;
const modelCache = new Map();
const loader = new GLTFLoader();

function getManifest() {
  if (!manifestPromise) {
    manifestPromise = fetch('/room0/models/manifest.json')
      .then((r) => (r.ok ? r.json() : { models: [] }))
      .catch(() => ({ models: [] }));
  }
  return manifestPromise;
}

// Placement convention: a prop's visual front points down +Z at rotation 0
// (the world builder rotates +Z toward the room centre / the paired desk or
// chair). Generated meshes come out of Meshy facing whatever way the source
// image implied — these per-type yaw offsets bake them back to the +Z
// convention. Determined empirically; add entries when a new asset lands
// facing sideways.
export const MODEL_YAW = {
  chair: -Math.PI / 2, // mesh seat opens toward +X (verified sitting at a table)
  // desk needs no offset: its screens already face +Z along the long side.
};

// Normalise a loaded GLB scene: uniform-scale into the prop's fit box, yaw to
// the +Z front convention, centre on x/z, drop the base to y=0, enable shadows.
function normalizeModel(scene, fit, yaw = 0) {
  const bb = new THREE.Box3().setFromObject(scene);
  const size = bb.getSize(new THREE.Vector3());
  const scale = Math.min(fit.h / (size.y || 1), fit.w / (Math.max(size.x, size.z) || 1));
  const wrap = new THREE.Group();
  wrap.add(scene);
  scene.scale.setScalar(scale);
  scene.rotation.y = yaw;
  const bb2 = new THREE.Box3().setFromObject(scene);
  const c = bb2.getCenter(new THREE.Vector3());
  scene.position.x -= c.x;
  scene.position.z -= c.z;
  scene.position.y -= bb2.min.y;
  scene.traverse((o) => {
    if (o.isMesh) { o.castShadow = true; o.receiveShadow = true; }
  });
  return wrap;
}

// Load the generated model for a prop type (or null when none exists).
// Returns a template Group; callers clone() it per placement.
export async function loadPropModel(type) {
  const manifest = await getManifest();
  if (!manifest.models?.includes(type)) return null;
  if (!modelCache.has(type)) {
    modelCache.set(type, new Promise((resolve) => {
      loader.load(
        `/room0/models/${type}.glb`,
        (gltf) => resolve(normalizeModel(gltf.scene, PROP_FIT[type] || { w: 1, h: 1 }, MODEL_YAW[type] || 0)),
        undefined,
        () => resolve(null),
      );
    }));
  }
  return modelCache.get(type);
}
