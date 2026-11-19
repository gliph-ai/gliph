// room0 3D — procedurally-animated low-poly avatar. Built from primitives with
// real joints (hips, shoulders, knees-free legs, neck) so animation is pure
// code: walk cycle, idle breathing, carry, sit, push and a timed wave overlay.
//
// Looks: every cosmetic (head shape, outfit, pants, shoes, hair/skin color) is
// driven by a compact look string ("1.3.0.2.1.4.2.0"). Without one, a
// deterministic look is derived from a hash of the user key — so legacy peers
// still render consistently everywhere. Joints are permanent groups; dress()
// rebuilds only the cosmetic meshes inside them, so setLook() is animation-safe.
import * as THREE from 'three';

// ── Look catalog (exported for the customizer UI) ──
export const LOOKS = {
  heads: [
    { name: 'Classic', w: 0.26, h: 0.3, d: 0.24 },
    { name: 'Round', r: 0.165 },
    { name: 'Tall', w: 0.23, h: 0.37, d: 0.22 },
    { name: 'Broad', w: 0.31, h: 0.25, d: 0.26 },
  ],
  torsos: ['Hoodie', 'Tee', 'Suit', 'Tank'],
  pants: ['Long', 'Shorts'],
  shirtColors: [0x2f6fd0, 0xd05a5a, 0x3fae6a, 0xb98a2f, 0x8a5ad0, 0xd0589a, 0x2fa8b8, 0xde7b32, 0x5a68d8, 0x4a8a3a],
  pantsColors: [0x26344f, 0x3a3a44, 0x4a3a2e, 0x2e4a3a, 0x6b2e2e, 0x1f5a6b, 0x8b8fa0, 0xc9c2b2],
  shoeColors: [0xe8e8e8, 0x1a1a1e, 0xb8352f, 0x2b5fc2, 0x5a4030],
  hairColors: [0x2a2018, 0x4a3018, 0x7a5a2a, 0x1a1a20, 0x8a8a92, 0xb84a2a, 0xd8b84a, 0x3a6ab8],
  skinColors: [0xf4dcc0, 0xe8c9a8, 0xd4a97c, 0xb08355, 0x8a5f3c, 0x6e482c, 0x54331e],
};

// Look string field order (indices into the LOOKS arrays above).
const FIELDS = ['head', 'torso', 'shirtColor', 'pants', 'pantsColor', 'shoeColor', 'hairColor', 'skinColor'];
const FIELD_SIZES = [
  LOOKS.heads.length, LOOKS.torsos.length, LOOKS.shirtColors.length,
  LOOKS.pants.length, LOOKS.pantsColors.length, LOOKS.shoeColors.length,
  LOOKS.hairColors.length, LOOKS.skinColors.length,
];

export function hashString(s) {
  let h = 2166136261;
  for (let i = 0; i < (s || '').length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

// Hash-derived defaults (matches the pre-customizer per-user styling).
export function defaultLook(hash) {
  return {
    head: hash % LOOKS.heads.length,
    torso: 0,
    shirtColor: hash % LOOKS.shirtColors.length,
    pants: 0,
    pantsColor: (hash >>> 4) % LOOKS.pantsColors.length,
    shoeColor: (hash >>> 16) % LOOKS.shoeColors.length,
    hairColor: (hash >>> 12) % LOOKS.hairColors.length,
    skinColor: 1 + ((hash >>> 8) % 5), // the original 5 tones sit at 1..5
  };
}

export function parseLook(str, hash = 0) {
  const cfg = defaultLook(hash);
  const parts = String(str || '').split('.');
  FIELDS.forEach((f, i) => {
    const v = parseInt(parts[i], 10);
    if (Number.isFinite(v) && v >= 0 && v < FIELD_SIZES[i]) cfg[f] = v;
  });
  return cfg;
}

export function lookToString(cfg) {
  return FIELDS.map((f) => cfg[f] ?? 0).join('.');
}

// Body proportions (world units). Total standing height ≈ 1.28.
const LEG = 0.4;
const TORSO = 0.46;
const HEAD = 0.3;
const SHOULDER_Y = LEG + TORSO - 0.05;

export function createAvatar(seedKey, look = null) {
  const hash = hashString(seedKey || 'anon');
  // Per-avatar materials (never the shared prop-material cache — recoloring
  // one player must not repaint everyone else's clothes).
  const own = (color, rough) => new THREE.MeshStandardMaterial({
    color, flatShading: true, roughness: rough, metalness: 0.05,
  });
  const matShirt = own(0xffffff, 0.9);
  const matPants = own(0xffffff, 0.95);
  const matSkin = own(0xffffff, 0.7);
  const matHair = own(0xffffff, 0.85);
  const matShoe = own(0xffffff, 0.8);
  const matStripe = own(0xe8eef8, 0.8);
  const matTie = own(0x8a2430, 0.85);
  const matEye = own(0x14181f, 0.3);
  const accent = new THREE.Color(0x2f6fd0);

  const root = new THREE.Group(); // world position + heading (rotation.y)
  const body = new THREE.Group(); // vertical offset for jump / sit / bob
  root.add(body);

  const mesh = (geo, m) => {
    const o = new THREE.Mesh(geo, m);
    o.castShadow = true;
    return o;
  };

  // ── Permanent joints; dress() fills them with cosmetic meshes ──
  const legL = new THREE.Group();
  const legR = new THREE.Group();
  legL.position.set(-0.09, LEG, 0);
  legR.position.set(0.09, LEG, 0);
  body.add(legL, legR);

  const torso = new THREE.Group();
  torso.position.y = LEG + TORSO / 2;
  body.add(torso);

  const armL = new THREE.Group();
  const armR = new THREE.Group();
  armL.position.set(-0.23, SHOULDER_Y, 0);
  armR.position.set(0.23, SHOULDER_Y, 0);
  body.add(armL, armR);

  const neck = new THREE.Group();
  neck.position.y = LEG + TORSO;
  body.add(neck);

  const disposeInto = (group) => {
    for (const c of [...group.children]) {
      group.remove(c);
      c.traverse?.((n) => n.geometry?.dispose?.());
    }
  };

  function buildLeg(group, cfg) {
    disposeInto(group);
    // Bare leg (skin) under a pant overlay; shorts stop above the knee.
    const bare = new THREE.BoxGeometry(0.125, LEG, 0.145);
    bare.translate(0, -LEG / 2, 0);
    group.add(mesh(bare, matSkin));
    const pl = LOOKS.pants[cfg.pants] === 'Shorts' ? 0.55 : 1.0;
    const pant = new THREE.BoxGeometry(0.145, LEG * pl, 0.165);
    pant.translate(0, -LEG * pl / 2, 0);
    group.add(mesh(pant, matPants));
    const shoe = mesh(new THREE.BoxGeometry(0.15, 0.09, 0.19), matShoe);
    shoe.position.set(0, -LEG + 0.045, 0.02);
    group.add(shoe);
  }

  function buildArm(group, cfg) {
    disposeInto(group);
    const bare = new THREE.BoxGeometry(0.1, 0.4, 0.12);
    bare.translate(0, -0.2 + 0.02, 0);
    group.add(mesh(bare, matSkin));
    const t = LOOKS.torsos[cfg.torso];
    const sl = (t === 'Hoodie' || t === 'Suit') ? 1.0 : t === 'Tee' ? 0.45 : 0;
    if (sl > 0) {
      const sleeve = new THREE.BoxGeometry(0.115, 0.4 * sl, 0.135);
      sleeve.translate(0, 0.02 - 0.4 * sl / 2, 0);
      group.add(mesh(sleeve, matShirt));
    }
    const hand = mesh(new THREE.BoxGeometry(0.09, 0.08, 0.1), matSkin);
    hand.position.y = -0.4;
    group.add(hand);
  }

  function buildTorso(cfg) {
    disposeInto(torso);
    const t = LOOKS.torsos[cfg.torso];
    const chest = mesh(new THREE.BoxGeometry(0.36, TORSO, 0.22), matShirt);
    torso.add(chest);
    if (t === 'Hoodie') {
      const stripe = mesh(new THREE.BoxGeometry(0.37, 0.07, 0.23), matStripe);
      stripe.position.y = 0.08;
      torso.add(stripe);
    } else if (t === 'Suit') {
      const knot = mesh(new THREE.BoxGeometry(0.07, 0.05, 0.02), matTie);
      knot.position.set(0, TORSO / 2 - 0.05, 0.115);
      const blade = mesh(new THREE.BoxGeometry(0.06, 0.2, 0.02), matTie);
      blade.position.set(0, TORSO / 2 - 0.18, 0.115);
      torso.add(knot, blade);
    }
  }

  function buildHead(cfg) {
    disposeInto(neck);
    const shape = LOOKS.heads[cfg.head];
    let head;
    let topY;
    let eyeZ;
    if (shape.r) {
      head = mesh(new THREE.SphereGeometry(shape.r, 8, 6), matSkin);
      head.position.y = shape.r + 0.02;
      topY = shape.r * 0.62;
      eyeZ = shape.r * 0.86;
    } else {
      head = mesh(new THREE.BoxGeometry(shape.w, shape.h, shape.d), matSkin);
      head.position.y = shape.h / 2 + 0.02;
      topY = shape.h / 2;
      eyeZ = shape.d / 2 + 0.005;
    }
    // Hair cap — slimmer on round heads so the box doesn't swallow the sphere.
    const cap = shape.r
      ? mesh(new THREE.BoxGeometry(shape.r * 1.7, 0.1, shape.r * 1.7), matHair)
      : mesh(new THREE.BoxGeometry(shape.w + 0.02, 0.12, shape.d + 0.02), matHair);
    cap.position.y = shape.r ? shape.r * 0.72 : topY; // overlaps the head top
    head.add(cap);
    const eyeGeo = new THREE.BoxGeometry(0.035, 0.05, 0.02);
    const eyeL = mesh(eyeGeo, matEye);
    const eyeR = mesh(eyeGeo.clone(), matEye);
    eyeL.position.set(-0.06, 0.02, eyeZ);
    eyeR.position.set(0.06, 0.02, eyeZ);
    head.add(eyeL, eyeR);
    neck.add(head);
  }

  function dress(cfg) {
    matShirt.color.setHex(LOOKS.shirtColors[cfg.shirtColor]);
    matPants.color.setHex(LOOKS.pantsColors[cfg.pantsColor]);
    matSkin.color.setHex(LOOKS.skinColors[cfg.skinColor]);
    matHair.color.setHex(LOOKS.hairColors[cfg.hairColor]);
    matShoe.color.setHex(LOOKS.shoeColors[cfg.shoeColor]);
    accent.setHex(LOOKS.shirtColors[cfg.shirtColor]);
    buildLeg(legL, cfg);
    buildLeg(legR, cfg);
    buildArm(armL, cfg);
    buildArm(armR, cfg);
    buildTorso(cfg);
    buildHead(cfg);
  }

  // Carry anchor — where a held object sits (above the head, AC-style).
  const carryAnchor = new THREE.Group();
  carryAnchor.position.set(0, LEG + TORSO + HEAD + 0.3, 0.05);
  body.add(carryAnchor);

  // Label anchor — name tag / chat bubble attach point.
  const labelAnchor = new THREE.Group();
  labelAnchor.position.set(0, LEG + TORSO + HEAD + 0.34, 0);
  body.add(labelAnchor);

  const av = {
    root,
    body,
    carryAnchor,
    labelAnchor,
    accent,
    look: null, // the applied look string (null → hash default)
    // Animation state
    state: 'idle', // idle | walk | sit | push
    carrying: false,
    typing: false, // seated at a workstation → hands on the keyboard
    lying: false, // reclined on a lounger
    walkPhase: 0,
    waveT: 0, // >0 → wave overlay on the right arm
    danceT: 0, // >0 → full-body dance overlay
    sitBase: null, // {x, z, ry, y} while seated
    _t: Math.random() * 10,
    setState(s) { this.state = s; },
    wave() { this.waveT = 1.6; },
    dance() { this.danceT = 3.2; },
    setLook(str) {
      if (str === this.look) return;
      this.look = str || null;
      dress(parseLook(this.look, hash));
    },
  };

  dress(parseLook(look, hash));
  av.look = look || null;

  const lerpTo = (obj, prop, target, k) => {
    obj[prop] += (target - obj[prop]) * k;
  };

  // Advance the skeleton one frame. `speed` is horizontal speed (units/s).
  av.update = (dt, speed = 0) => {
    av._t += dt;
    const k = Math.min(1, dt * 14); // smoothing toward pose targets
    const moving = av.state === 'walk' && speed > 0.05;
    if (moving) av.walkPhase += dt * (4.5 + speed * 2.2);
    const ph = av.walkPhase;

    let tLegL = 0, tLegR = 0, tArmL = 0, tArmR = 0, tArmLZ = 0.06, tArmRZ = -0.06;
    let bob = Math.sin(av._t * 1.7) * 0.008; // idle breathing
    let lean = 0;
    let tBodyX = 0; // whole-body recline (lying)
    let tBodyZ = 0;
    let tNeck = null; // explicit head tilt override

    if (av.state === 'sit') {
      bob = 0;
      if (av.lying) {
        // Recline the WHOLE body as a plank (~57°) against the lounger's
        // backrest; legs level out along the seat, arms rest at the sides,
        // head lifts off the plank. Rotating only the torso looked like
        // sitting bolt upright on the cushion.
        tBodyX = -1.0;
        tBodyZ = 0.22; // keep the head over the backrest, feet on the seat
        tLegL = -0.5;
        tLegR = -0.5;
        tArmL = -0.3;
        tArmR = -0.3;
        tArmLZ = 0.25;
        tArmRZ = -0.25;
        tNeck = 0.55;
      } else if (av.typing) {
        // At the keyboard: hunched slightly, hands forward, alternating taps.
        tLegL = -Math.PI / 2.2;
        tLegR = -Math.PI / 2.2;
        const tap = Math.sin(av._t * 12);
        tArmL = -1.05 + tap * 0.07;
        tArmR = -1.05 - tap * 0.07;
        tArmLZ = 0.12;
        tArmRZ = -0.12;
        lean = 0.14;
      } else {
        tLegL = -Math.PI / 2.2;
        tLegR = -Math.PI / 2.2;
        tArmL = -0.25;
        tArmR = -0.25;
      }
    } else if (moving) {
      const swing = Math.min(1, 0.45 + speed * 0.16);
      tLegL = Math.sin(ph) * swing;
      tLegR = -Math.sin(ph) * swing;
      tArmL = -Math.sin(ph) * swing * 0.7;
      tArmR = Math.sin(ph) * swing * 0.7;
      bob = Math.abs(Math.sin(ph)) * 0.035;
      lean = 0.08 + speed * 0.012;
    }
    if (av.state === 'push') {
      tArmL = -1.25; tArmR = -1.25; tArmLZ = 0.12; tArmRZ = -0.12;
      lean = 0.22;
    }
    if (av.carrying) {
      tArmL = Math.PI - 0.35; tArmR = Math.PI - 0.35; tArmLZ = -0.25; tArmRZ = 0.25;
    }

    // Dance overlay — arms pumping alternately overhead, hip sway, bounce.
    let dancing = false;
    if (av.danceT > 0 && av.state !== 'sit') {
      av.danceT -= dt;
      dancing = true;
      const dtm = av._t * 7;
      tArmL = Math.PI - 0.5 + Math.sin(dtm) * 0.55;
      tArmR = Math.PI - 0.5 + Math.sin(dtm + Math.PI) * 0.55;
      tArmLZ = -0.35;
      tArmRZ = 0.35;
      tLegL = Math.sin(dtm) * 0.18;
      tLegR = -Math.sin(dtm) * 0.18;
      bob = Math.abs(Math.sin(dtm)) * 0.07;
    }
    lerpTo(body.rotation, 'y', dancing ? Math.sin(av._t * 3.5) * 0.4 : 0, k);

    lerpTo(legL.rotation, 'x', tLegL, k);
    lerpTo(legR.rotation, 'x', tLegR, k);
    lerpTo(armL.rotation, 'x', tArmL, k);
    lerpTo(armL.rotation, 'z', tArmLZ, k);
    lerpTo(torso.rotation, 'x', lean, k);
    lerpTo(neck.rotation, 'x', tNeck ?? -lean * 0.6, k);
    lerpTo(body.rotation, 'x', tBodyX, k);
    lerpTo(body.position, 'z', tBodyZ, k);

    // Wave overlay commandeers the right arm (dance wins if both run).
    if (av.waveT > 0 && !dancing) {
      av.waveT -= dt;
      lerpTo(armR.rotation, 'x', Math.PI - 0.2, Math.min(1, dt * 20));
      armR.rotation.z = Math.sin(av._t * 14) * 0.35 - 0.15;
    } else {
      lerpTo(armR.rotation, 'x', tArmR, k);
      lerpTo(armR.rotation, 'z', tArmRZ, k);
    }

    // Vertical: seat height (av.sitY = seat surface height, hips land on it)
    // or walk bob; the jump offset stacks on top. Lying compensates for the
    // recline shortening the hip height (LEG·cos 1.0 ≈ 0.54·LEG).
    const baseY = av.state === 'sit'
      ? (av.sitY ?? 0.32) - LEG * (av.lying ? 0.54 : 1)
      : 0;
    body.position.y = baseY + bob + (av.jumpY || 0);
  };

  return av;
}
