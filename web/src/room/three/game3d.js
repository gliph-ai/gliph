// room0 3D — the game. Owns the Three.js renderer + scene, the local player
// (movement, jumping, carrying, sitting), dynamic pushable/carryable objects
// with mini-physics, remote coworkers (interpolated from presence events),
// chat bubbles and the third-person camera. React (Room0Game.jsx) only renders
// the HUD around it and relays presence/chat through the returned API.
import * as THREE from 'three';
import { CSS2DRenderer, CSS2DObject } from 'three/examples/jsm/renderers/CSS2DRenderer.js';
import { ROOMS, isWalkable, blockedCells, arrivalSpawn } from '../engine';
import { buildRoomScene, tickWorld, THEMES, daylightAt, paintSky } from './world3d';
import { buildWorldScene } from './worlds3d';
import { createAvatar } from './avatar3d';
import { DYN_BUILDERS, DYN_TYPES } from './props3d';
import { createAmbient } from './ambient3d';
import { createVoiceChat } from './voice3d';

// Quick-reaction emotes: key → broadcast kind → floating emoji (or animation).
export const EMOTES = [
  { kind: 'wave', emoji: '👋', key: 'Digit1' },
  { kind: 'dance', emoji: '🕺', key: 'Digit2' },
  { kind: 'heart', emoji: '❤️', key: 'Digit3' },
  { kind: 'laugh', emoji: '😂', key: 'Digit4' },
  { kind: 'thumbs', emoji: '👍', key: 'Digit5' },
];
const EMOTE_BY_KEY = Object.fromEntries(EMOTES.map((e) => [e.key, e.kind]));
const EMOTE_EMOJI = Object.fromEntries(EMOTES.map((e) => [e.kind, e.emoji]));

const WALK_SPEED = 3.1;
const RUN_SPEED = 5.4;
const PLAYER_R = 0.3;
// Low props you can jump over — and land/stand on top of (platform height).
// Anything shorter than the jump apex (~0.75) qualifies as a "smaller item".
const CLEARABLE = { bench: 0.55, lounger: 0.65, table: 0.72, plant: 0.6, chair: 0.72 };
const GRAVITY = 14;
const JUMP_V = 4.6;
const SEND_MS = 100; // outgoing presence throttle
const HEARTBEAT_MS = 20000; // idle re-announce so peers know we're alive
const STALE_MS = 60000; // no events for this long → treat the peer as gone

// Dynamic (pushable / carryable) objects per room: stable ids keep multiplayer
// state consistent across clients.
const DYNAMICS = {
  office: [
    { type: 'ball', x: 13, y: 10 },
    { type: 'box', x: 8, y: 4 },
    { type: 'box', x: 14, y: 18 },
    { type: 'mug', x: 4, y: 9 },
    { type: 'mug', x: 17, y: 12 },
  ],
  garden: [
    { type: 'ball', x: 5, y: 7 },
    { type: 'mug', x: 8, y: 9 },
  ],
  kitchen: [
    { type: 'mug', x: 4, y: 4 },
    { type: 'mug', x: 9, y: 4 },
    { type: 'box', x: 4, y: 10 },
  ],
  lounge: [
    { type: 'mug', x: 8, y: 4 },
    { type: 'box', x: 3, y: 10 },
  ],
  beach: [
    { type: 'beachball', x: 7, y: 7 },
    { type: 'mug', x: 10, y: 9 },
  ],
};

const KEY_DIR = {
  KeyW: 'up', ArrowUp: 'up',
  KeyS: 'down', ArrowDown: 'down',
  KeyA: 'left', ArrowLeft: 'left',
  KeyD: 'right', ArrowRight: 'right',
};

export function createRoom0Game({ container, roomId = 'office', screenName, selfKey = null, look = null, ambientMuted = false, worldSeed = 1, spawnAt = null, classes = {}, callbacks = {} }) {
  const cb = callbacks;
  const ambient = createAmbient({ muted: ambientMuted });
  let soundMuted = ambientMuted;

  // ── Renderer + CSS2D overlay ──
  // WebGL can be unavailable (old GPU, headless, blocked) — fail soft with a
  // message instead of tripping the app-wide error boundary.
  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true });
  } catch {
    cb.onError?.('3D is not available in this browser (WebGL could not start).');
    return null;
  }
  renderer.shadowMap.enabled = true;
  renderer.shadowMap.type = THREE.PCFShadowMap;
  renderer.outputColorSpace = THREE.SRGBColorSpace;
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.05;
  container.appendChild(renderer.domElement);

  const labelRenderer = new CSS2DRenderer();
  const labelEl = labelRenderer.domElement;
  labelEl.style.position = 'absolute';
  labelEl.style.inset = '0';
  labelEl.style.pointerEvents = 'none';
  labelEl.style.overflow = 'hidden';
  container.appendChild(labelEl);

  // Fade overlay for room transitions (plain DOM, driven by the loop).
  const fadeEl = document.createElement('div');
  fadeEl.style.position = 'absolute';
  fadeEl.style.inset = '0';
  fadeEl.style.background = '#04060c';
  fadeEl.style.opacity = '0';
  fadeEl.style.pointerEvents = 'none';
  fadeEl.style.zIndex = '5';
  container.appendChild(fadeEl);

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(50, 1, 0.1, 120);

  // ── Lighting rig (re-tinted per room) ──
  const hemi = new THREE.HemisphereLight(0xffffff, 0x223344, 0.6);
  scene.add(hemi);
  const sun = new THREE.DirectionalLight(0xffffff, 0.9);
  sun.castShadow = true;
  sun.shadow.mapSize.set(2048, 2048);
  sun.shadow.bias = -0.0004;
  scene.add(sun, sun.target);
  const fill = new THREE.AmbientLight(0x404860, 0.8);
  scene.add(fill);

  // ── State ──
  let room = ROOMS[roomId];
  let blocked = blockedCells(room);
  let clearHeights = new Map(); // "x,z" → platform height for CLEARABLE props
  let world = null; // {group, theme, animated, seats}
  let disposed = false;
  let presence = null; // controller from room0Presence
  let chatOpen = false;
  let myName = screenName || 'You';

  const player = {
    x: room.spawn.gx, z: room.spawn.gy, heading: Math.PI,
    vx: 0, vz: 0, jumpY: 0, jumpV: 0, onGround: true,
    sitting: null, carrying: null, pushT: 0,
    avatar: createAvatar(`self:${myName}`, look),
  };
  scene.add(player.avatar.root);
  const selfTag = makeTag(myName, true);
  player.avatar.labelAnchor.add(selfTag.obj);

  // Per-room voice channel (org sessions only — mesh peers are keyed and the
  // offer/answer roles decided by user key). game3d decides who to connect to
  // (voice-on peers in the same sub-room); voice3d owns WebRTC + spatial audio.
  // The auth session restores async, so selfId usually arrives AFTER mount via
  // setSelfKey() — the voice manager exists from the start and reads it lazily.
  let selfId = selfKey || null;
  const voice = createVoiceChat({
    getSelfKey: () => selfId,
    sendSignal: (to, kind, payload) => presence?.rtc?.({ to, kind, payload }),
    onState: (s) => {
      selfTag.mic.style.display = s.active ? '' : 'none';
      cb.onVoice?.(s);
    },
    onNotice: (text) => cb.onChatLog?.({ system: true, text }),
  });

  function voiceSync() {
    if (!voice) return;
    const desired = new Set();
    if (voice.isActive()) {
      for (const p of peers.values()) {
        if (p.voice && p.room === room.id) desired.add(p.id);
      }
    }
    voice.sync(desired);
  }

  async function toggleVoice() {
    if (!selfId || !presence) return;
    if (room.quiet && !voice.isActive()) {
      cb.onChatLog?.({ system: true, text: `${room.name} is a quiet room — leave a voice note instead 🎙️` });
      return;
    }
    if (voice.isActive()) {
      voice.leave();
      cb.onChatLog?.({ system: true, text: 'left the voice channel' });
    } else {
      const ok = await voice.join();
      if (!ok) return;
      cb.onChatLog?.({ system: true, text: `joined voice — ${room.name}` });
    }
    presence?.announce?.(selfState()); // broadcast the voice flag right away
    voiceSync();
  }

  const keys = new Set();
  let runHeld = false;
  // Bunny hop: tapping Space just before landing chains an instant re-jump
  // and stacks a speed boost (decays while grounded).
  let bhop = 1;
  let jumpQueued = false;
  const camRig = { yaw: Math.PI / 4, targetYaw: Math.PI / 4, dist: 9, targetDist: 9, pitch: 0.72, targetPitch: 0.72 };
  const fade = { phase: 'none', a: 0, onMid: null }; // onMid runs at full black
  const peers = new Map(); // uid → peer
  const objects = new Map(); // id → dynamic object
  const vehicleStates = new Map(); // vehicle id → last known {room, x, z, h, owned}
  const lastSent = { t: 0, x: 0, z: 0, h: 0, anim: '', room: '', carry: null };
  let lastPrompt;
  let lastNear;
  let lastOuchT = 0;
  let lastFrame = performance.now();
  let elapsed = 0;

  // Touch / mouse walking: tap → walk there (auto-interact if the tap was on
  // a pickup/seat), hold → steer toward the finger, two fingers → pinch zoom.
  let moveTarget = null; // {x, z, interact}
  let stuckT = 0;
  const pointers = new Map();
  let pinch0 = null; // {dist, cam}
  let tapInfo = null; // {x, y, t, moved}
  const ray = new THREE.Raycaster();
  const marker = new THREE.Mesh(
    new THREE.RingGeometry(0.16, 0.24, 24),
    new THREE.MeshBasicMaterial({ color: 0x39c6ff, transparent: true, opacity: 0.7, side: THREE.DoubleSide }),
  );
  marker.rotation.x = -Math.PI / 2;
  marker.visible = false;
  scene.add(marker);

  // Register every dynamic object across all rooms (meshes attach lazily).
  for (const [rid, defs] of Object.entries(DYNAMICS)) {
    defs.forEach((d, i) => {
      const id = `${rid}:${d.type}:${i}`;
      objects.set(id, {
        id, type: d.type, room: rid, x: d.x, z: d.y, vx: 0, vz: 0, y: 0, vy: 0,
        holder: null, mesh: null, owned: false, sleepT: 0,
      });
    });
  }

  // ── CSS2D helpers ──
  function makeTag(name, self) {
    const wrap = document.createElement('div');
    wrap.className = classes.overhead || '';
    const bubble = document.createElement('div');
    bubble.className = classes.bubble || '';
    bubble.style.display = 'none';
    const tag = document.createElement('div');
    tag.className = `${classes.nameTag || ''}${self ? ` ${classes.nameTagSelf || ''}` : ''}`;
    tag.textContent = name || '·';
    // Mic badge under the name — shown while this user is in the voice channel.
    const mic = document.createElement('div');
    mic.className = classes.micBadge || '';
    mic.textContent = '🎙️';
    mic.style.display = 'none';
    wrap.appendChild(bubble);
    wrap.appendChild(tag);
    wrap.appendChild(mic);
    const obj = new CSS2DObject(wrap);
    obj.center.set(0.5, 1);
    return { obj, tag, bubble, mic, timer: null };
  }

  // ttlMs 0 → the bubble stays until replaced (real chat messages remain;
  // status blurbs like drinking/ouch pass a timeout).
  function showBubble(tagRef, text, ttlMs = 0) {
    const b = tagRef.bubble;
    b.textContent = text;
    b.classList.remove(classes.bubbleTyping || 'typing');
    b.style.display = 'block';
    if (tagRef.timer) clearTimeout(tagRef.timer);
    tagRef.timer = ttlMs ? setTimeout(() => { b.style.display = 'none'; }, ttlMs) : null;
  }

  // "…" bubble while a peer is typing. Any chat/stop replaces or clears it.
  function showTyping(tagRef, on) {
    const b = tagRef.bubble;
    if (on) {
      b.textContent = '• • •';
      b.classList.add(classes.bubbleTyping || 'typing');
      b.style.display = 'block';
      if (tagRef.timer) clearTimeout(tagRef.timer);
      tagRef.timer = setTimeout(() => { b.style.display = 'none'; }, 12000);
    } else if (b.classList.contains(classes.bubbleTyping || 'typing')) {
      b.style.display = 'none';
      if (tagRef.timer) clearTimeout(tagRef.timer);
    }
  }

  // A one-shot emoji that floats up from above the avatar's head.
  function spawnEmoji(tagRef, emoji) {
    if (!emoji) return;
    const el = document.createElement('div');
    el.className = classes.emojiFloat || '';
    el.textContent = emoji;
    tagRef.obj.element.appendChild(el);
    setTimeout(() => el.remove(), 1900);
  }

  // Someone said something in this room, and it reached us as a channel
  // message. Find whose head it belongs over; a nick we have no avatar for
  // (they walked out between saying it and it landing) simply has no bubble.
  function sayAs(nick, text) {
    for (const p of peers.values()) {
      if (p.name !== nick) continue;
      if (p.room === room.id) {
        showBubble(p.tagRef, text);
        p.avatar.wave();
      } else {
        showTyping(p.tagRef, false);
      }
      return;
    }
  }

  function sendChatMsg(text) {
    const clean = (text || '').trim().slice(0, 200);
    if (!clean) return;
    showBubble(selfTag, clean);
    player.avatar.wave();
    presence?.chat?.({ text: clean });
    cb.onChatLog?.({ name: myName, text: clean, self: true });
  }

  function toggleSound() {
    soundMuted = !soundMuted;
    ambient.setMuted(soundMuted);
    cb.onSoundChange?.(soundMuted);
  }

  function doEmote(kind) {
    if (kind === 'dance') player.avatar.dance();
    else if (kind === 'wave') player.avatar.wave();
    else spawnEmoji(selfTag, EMOTE_EMOJI[kind]);
    presence?.emote?.({ kind });
  }

  // ── Room building / switching ──
  function enterRoom(id, fromId) {
    if (world) scene.remove(world.group);
    for (const o of objects.values()) {
      if (o.mesh) { scene.remove(o.mesh); o.mesh = null; }
    }
    room = ROOMS[id];
    blocked = blockedCells(room);
    clearHeights = new Map();
    if (room.kind === 'world') {
      // Outdoor world: seeded scene from worlds3d, which brings its own sky
      // dome, fog and lights — the tile rig steps back so they dominate.
      world = buildWorldScene(room, worldSeed);
      if (world.vehicle) {
        const st = vehicleStates.get(world.vehicle.id);
        if (st) {
          world.vehicle.obj.position.x = st.x;
          world.vehicle.obj.position.z = st.z;
          world.vehicle.obj.rotation.y = st.h;
          world.vehicle.sync();
        }
      }
      scene.add(world.group);
      scene.fog = world.fog || null;
      sun.intensity = 0;
      hemi.intensity = 0;
      fill.intensity = 0.3;
      camera.far = 900; // sky dome + planet sit far outside a tile room
    } else {
      for (const p of room.props || []) {
        if (CLEARABLE[p.type] !== undefined) clearHeights.set(`${p.x},${p.y}`, CLEARABLE[p.type]);
      }
      world = buildRoomScene(room);
      scene.add(world.group);

      const theme = THEMES[id] || THEMES.office;
      scene.fog = new THREE.Fog(theme.fog[0], theme.fog[1], theme.fog[2]);
      hemi.color.set(theme.hemi[0]);
      hemi.groundColor.set(theme.hemi[1]);
      sun.target.position.set(room.w / 2, 0, room.h / 2);
      const half = Math.max(room.w, room.h) * 0.72;
      sun.shadow.camera.left = -half;
      sun.shadow.camera.right = half;
      sun.shadow.camera.top = half;
      sun.shadow.camera.bottom = -half;
      sun.shadow.camera.far = 40;
      sun.shadow.camera.updateProjectionMatrix();
      camera.far = 120;
    }
    camera.updateProjectionMatrix();

    if (fromId) {
      const arr = arrivalSpawn(room, fromId);
      player.x = arr.gx;
      player.z = arr.gy;
    }
    player.sitting = null;
    // Carried object rides along into the new room.
    if (player.carrying) {
      const o = objects.get(player.carrying);
      if (o) { o.room = id; sendObject(o); }
    }
    for (const o of objects.values()) {
      if (o.room === id) attachObjectMesh(o);
    }
    for (const p of peers.values()) p.avatar.root.visible = p.room === id;
    moveTarget = null;
    ambient.setRoom(id);
    applyDaylight();
    cb.onRoomChange?.(id);
    // Quiet rooms (office/garden) have no live channel — walking into one
    // hangs up; the async voice-note wall takes over there.
    if (room.quiet && voice.isActive()) {
      voice.leave();
      cb.onChatLog?.({ system: true, text: `${room.name} is a quiet room — voice channel left` });
      presence?.announce?.(selfState());
    }
    voiceSync(); // voice channels are per-sub-room: reconnect to the members here
  }

  // ── Time of day ──
  // Lighting follows the local clock: bright at midday, warm at the golden
  // hours, dark with glowing trims and accent lights at night. Outdoor rooms
  // (garden/beach) swing much harder than the interiors. Override with
  // ?hour=22 or window.__room0.setHour(22) for testing.
  let hourOverride = parseFloat(new URLSearchParams(window.location.search).get('hour'));
  if (Number.isNaN(hourOverride)) hourOverride = null;
  const WARM_TINT = new THREE.Color(0xff9a5c);
  const MOON = new THREE.Color(0x8fb0e8);

  // Painted sky (gradient + sun/moon + clouds + stars) — doubles as the scene
  // backdrop and the office windows' glass, repainted by the daylight tick.
  const skyCanvas = document.createElement('canvas');
  skyCanvas.width = 512;
  skyCanvas.height = 512;
  const skyTex = new THREE.CanvasTexture(skyCanvas);
  skyTex.colorSpace = THREE.SRGBColorSpace;
  scene.background = skyTex;

  function currentHour() {
    if (hourOverride != null) return hourOverride;
    const now = new Date();
    return now.getHours() + now.getMinutes() / 60;
  }

  function applyDaylight() {
    if (!world || world.custom) return; // worlds carry their own fixed light
    const theme = world.theme;
    const hour = currentHour();
    const { day, warm } = daylightAt(hour);
    const night = 1 - day;
    const outdoor = room.id === 'garden' || room.id === 'beach';

    // Sky: repaint the canvas backdrop for this hour. Fog blends the room's
    // theme color toward the sky's horizon so walls fade into it naturally.
    const sky = paintSky(skyCanvas.getContext('2d'), skyCanvas.width, skyCanvas.height, { hour, day, warm });
    skyTex.needsUpdate = true;
    const fogBase = new THREE.Color(theme.bg)
      .multiplyScalar(outdoor ? 0.55 + day * 2.6 : 0.75 + day * 0.9)
      .lerp(WARM_TINT, warm * (outdoor ? 0.22 : 0.08));
    const horizon = new THREE.Color(sky.horizon[0] / 255, sky.horizon[1] / 255, sky.horizon[2] / 255);
    scene.fog.color.copy(fogBase.lerp(horizon, 0.4));

    // Sun ↔ moon: color, strength and position (east at dawn → west at dusk,
    // low sun = long shadows; parked at a fixed spot as the moon at night).
    sun.color.copy(MOON).lerp(new THREE.Color(theme.sun[0]), day).lerp(WARM_TINT, warm * 0.5);
    const sunBase = theme.sun[1] * 2 + 0.6;
    sun.intensity = sunBase * (outdoor ? 0.16 + day * 0.95 : 0.42 + day * 0.62);
    const cx = room.w / 2;
    const cz = room.h / 2;
    const az = day > 0.02 ? (Math.min(1, Math.max(0, (hour - 6) / 14)) - 0.5) * 1.8 : 0.7;
    const el = 0.25 + day * 0.95;
    const R = 20;
    sun.position.set(
      cx + Math.sin(az) * R * Math.cos(el),
      Math.max(4, R * Math.sin(el)),
      cz + Math.cos(el) * R * 0.45 + 2,
    );

    hemi.intensity = (theme.hemi[2] * 2 + 0.9) * (0.45 + day * 0.55);
    fill.intensity = 1.0 * (0.6 + day * 0.4);

    // After dark the rooms run on their own glow: trims + accent lights up,
    // and the warm night lamps (desk lamps + corner wall lights) fade in.
    world.trimMat.emissiveIntensity = 0.75 + night * 0.8;
    world.wallMat.emissiveIntensity = 0.95 + day * 0.55; // dimmer walls at night, never black
    for (const a of world.lights) a.light.intensity = a.base * (1 + night * 0.9);
    for (const nl of world.nightLights) nl.light.intensity = nl.base * (0.06 + 0.94 * night);
    world.lampGlowMat.emissiveIntensity = 0.5 + night * 2.2;

    // Window glass shows the actual painted sky (sun, clouds, stars included).
    if (world.windowMat) {
      if (world.windowMat.map !== skyTex) {
        world.windowMat.map = skyTex;
        world.windowMat.emissiveMap = skyTex;
        world.windowMat.color.set(0xffffff);
        world.windowMat.emissive.set(0xffffff);
        world.windowMat.needsUpdate = true;
      }
      world.windowMat.emissiveIntensity = 0.45 + day * 0.75 + warm * 0.2;
    }

    ambient.setDaylight?.(day);
  }

  const daylightTimer = setInterval(applyDaylight, 60000);

  function attachObjectMesh(o) {
    if (!o.mesh) {
      o.mesh = DYN_BUILDERS[o.type]();
      o.mesh.traverse((n) => { if (n.isMesh) n.castShadow = true; });
    }
    scene.add(o.mesh);
  }

  // ── Movement helpers ──
  // Height-aware: low props (benches, loungers) don't block when the player
  // is at/above their platform height — jump over them or stand on them.
  function canStand(x, z, atY = player.jumpY) {
    for (const [ox, oz] of [[-PLAYER_R, -PLAYER_R], [PLAYER_R, -PLAYER_R], [-PLAYER_R, PLAYER_R], [PLAYER_R, PLAYER_R]]) {
      const gx = Math.round(x + ox);
      const gz = Math.round(z + oz);
      if (!isWalkable(room, gx, gz, null)) return false;
      const k = `${gx},${gz}`;
      if (blocked.has(k)) {
        const clear = clearHeights.get(k);
        if (clear === undefined || atY < clear - 0.05) return false;
      }
    }
    return true;
  }

  // The floor height under the player: 0, or a platform top when overlapping one.
  function groundYAt(x, z) {
    let g = 0;
    for (const [ox, oz] of [[-PLAYER_R, -PLAYER_R], [PLAYER_R, -PLAYER_R], [-PLAYER_R, PLAYER_R], [PLAYER_R, PLAYER_R]]) {
      const c = clearHeights.get(`${Math.round(x + ox)},${Math.round(z + oz)}`);
      if (c !== undefined && c > g) g = c;
    }
    return g;
  }

  function objectCanStand(o, x, z, r) {
    for (const [ox, oz] of [[-r, -r], [r, -r], [-r, r], [r, r]]) {
      if (!isWalkable(room, Math.round(x + ox), Math.round(z + oz), blocked)) return false;
    }
    return true;
  }

  // ── Interaction targets ──
  function nearestCarryable() {
    let best = null;
    let bd = 0.95;
    for (const o of objects.values()) {
      if (o.room !== room.id || o.holder || !DYN_TYPES[o.type].carryable) continue;
      const d = Math.hypot(o.x - player.x, o.z - player.z);
      if (d < bd) { bd = d; best = o; }
    }
    return best;
  }

  // Something a coworker is carrying, close enough to snatch.
  function nearestStealable() {
    let best = null;
    let bd = 1.15;
    for (const o of objects.values()) {
      if (o.room !== room.id || !o.holder || o.holder === 'self') continue;
      const d = Math.hypot(o.x - player.x, o.z - player.z);
      if (d < bd) { bd = d; best = o; }
    }
    return best;
  }

  function seatTaken(s) {
    for (const p of peers.values()) {
      if (p.room !== room.id) continue;
      if ((p.anim === 'sit' || p.anim === 'type' || p.anim === 'lie' || p.anim === 'perch')
        && Math.hypot(p.x - s.x, p.z - s.z) < 0.4) return true;
    }
    return false;
  }

  function nearestSeat() {
    if (!world) return null;
    let best = null;
    // Seat tiles are solid, so the closest you can stand next to one is
    // ~0.9 — the reach has to be a bit beyond that. Spots a coworker is
    // already sitting on don't count (sofas have two).
    let bd = Infinity;
    for (const s of world.seats) {
      if (seatTaken(s)) continue;
      const d = Math.hypot(s.x - player.x, s.z - player.z);
      const reach = s.perch ? 1.4 : 1.05; // canopies are wide — climb from further out
      if (d < reach && d < bd) { bd = d; best = s; }
    }
    return best;
  }

  function nearestPeer() {
    let best = null;
    let bd = 2.6;
    for (const p of peers.values()) {
      if (p.room !== room.id) continue;
      const d = Math.hypot(p.x - player.x, p.z - player.z);
      if (d < bd) { bd = d; best = p; }
    }
    return best;
  }

  // ── Object interactions ──
  function pickUp(o) {
    o.holder = 'self';
    o.vx = 0; o.vz = 0; o.vy = 0; o.y = 0;
    player.carrying = o.id;
    player.avatar.carrying = true;
    sendObject(o);
  }

  function throwHeld() {
    const o = objects.get(player.carrying);
    player.carrying = null;
    player.avatar.carrying = false;
    if (!o) return;
    o.holder = null;
    o.x = player.x + Math.sin(player.heading) * 0.55;
    o.z = player.z + Math.cos(player.heading) * 0.55;
    o.y = 1.3;
    o.vx = Math.sin(player.heading) * 3.4 + player.vx * 0.6;
    o.vz = Math.cos(player.heading) * 3.4 + player.vz * 0.6;
    o.vy = 1.6;
    o.owned = true;
    o.selfThrowT = performance.now(); // don't "ouch" ourselves at release
    sendObject(o);
  }

  function nearestLandmark(spot, reach = 1.35) {
    return spot && Math.hypot(spot.x - player.x, spot.z - player.z) < reach;
  }

  // The two office whiteboards: left (smaller x) is the "This week" sticky
  // board, right is the draw.io sketch board.
  function nearestWhiteboard() {
    const boards = (room.props || []).filter((p) => p.type === 'whiteboard').sort((a, b) => a.x - b.x);
    let best = null;
    let bd = 1.35;
    for (let i = 0; i < boards.length; i++) {
      const d = Math.hypot(boards[i].x - player.x, boards[i].y - player.z);
      if (d < bd) { bd = d; best = { kind: i === 0 ? 'tasks' : 'drawing', p: boards[i] }; }
    }
    return best;
  }

  function interact() {
    if (player.sitting) { standUp(); return; }
    if (player.carrying) { throwHeld(); return; }
    const o = nearestCarryable();
    if (o) { pickUp(o); return; }
    const st = nearestStealable();
    if (st) { pickUp(st); return; } // snatch it — their client shows the loss
    const s = nearestSeat();
    if (s) { sitOn(s); return; }
    const wb = nearestWhiteboard();
    if (wb) { cb.onOpenBoard?.(wb.kind); return; }
    if (nearestLandmark(world?.tv)) { cb.onWatchTv?.(); return; }
    if (nearestLandmark(world?.fountain)) {
      ambient.gulp?.();
      showBubble(selfTag, 'ah that was refreshing, water is good for you', 4000);
    }
  }

  function sitOn(s) {
    player.sitting = s;
    player.avatar.sitY = s.y;
    player.avatar.typing = s.pose === 'type';
    player.avatar.lying = s.pose === 'lie';
    // Workstations pull you in toward the desk with the tucking chair;
    // tree perches sit dead-centre on the canopy.
    const fwd = s.pose === 'type' ? 0.3 : s.perch ? 0 : 0.1;
    player.x = s.x + Math.sin(s.ry) * fwd;
    player.z = s.z + Math.cos(s.ry) * fwd;
    player.heading = s.ry;
    player.avatar.setState('sit');
    // Climbing a tree announces the vibe.
    if (s.perch) sendChatMsg('digging this view dude');
  }

  // Walk over to a coworker from the roster: fade, land beside them (their
  // room if needed), face them and wave hello.
  function visitPeer(uid) {
    const target = peers.get(uid);
    if (!target || fade.phase !== 'none') return;
    if (player.sitting) standUp();
    fade.phase = 'out';
    fade.onMid = () => {
      if (target.room !== room.id) enterRoom(target.room, null);
      let spot = { x: target.x, z: target.z };
      for (let i = 0; i < 8; i++) {
        const a = (i / 8) * Math.PI * 2;
        const nx = target.x + Math.sin(a) * 0.95;
        const nz = target.z + Math.cos(a) * 0.95;
        if (canStand(nx, nz)) { spot = { x: nx, z: nz }; break; }
      }
      player.x = spot.x;
      player.z = spot.z;
      player.heading = Math.atan2(target.x - player.x, target.z - player.z);
      player.avatar.wave();
      presence?.emote?.({ kind: 'wave' });
      presence?.announce?.(selfState());
    };
  }

  function standUp() {
    const s = player.sitting;
    player.sitting = null;
    player.avatar.setState('idle');
    player.avatar.typing = false;
    player.avatar.lying = false;
    // Climbing down from a tree perch: hop off and fall from canopy height.
    const hopDown = () => {
      if (s.perch) {
        player.jumpY = s.y;
        player.jumpV = 0;
        player.onGround = false;
      }
    };
    // Step off the seat: try its facing first, then behind / the sides.
    // The step must fully clear the solid seat tile (≥1.0 — anything less
    // leaves the collision circle overlapping it and traps the player).
    // A vehicle bench sits at the middle of its hull's blocked patch —
    // clearing that takes a longer hop than any tile-room seat needs.
    const dists = world?.vehicle?.seats.includes(s) ? [1.0, 1.35, 2.0, 2.7] : [1.0, 1.35];
    for (const dist of dists) {
      for (const a of [s.ry, s.ry + Math.PI, s.ry + Math.PI / 2, s.ry - Math.PI / 2]) {
        const nx = s.x + Math.sin(a) * dist;
        const nz = s.z + Math.cos(a) * dist;
        if (canStand(nx, nz)) { player.x = nx; player.z = nz; hopDown(); return; }
      }
    }
    // Last resort: any walkable neighbouring tile centre.
    const tx = Math.round(s.x);
    const tz = Math.round(s.z);
    for (const [dx, dz] of [[0, 1], [0, -1], [1, 0], [-1, 0], [1, 1], [1, -1], [-1, 1], [-1, -1]]) {
      if (canStand(tx + dx, tz + dz)) { player.x = tx + dx; player.z = tz + dz; hopDown(); return; }
    }
  }

  // ── Presence / network ──
  function selfState() {
    // A hidden tab stops the render loop, so peers would see us frozen —
    // announce it as an explicit "away" state instead. Seated poses carry
    // their flavour (type/lie) so peers render the same animation.
    let anim = document.hidden ? 'away' : player.avatar.state;
    if (anim === 'sit') {
      if (player.sitting?.perch) anim = 'perch';
      else if (player.avatar.lying) anim = 'lie';
      else if (player.avatar.typing) anim = 'type';
    }
    return {
      room: room.id,
      x: +player.x.toFixed(2), y: +player.z.toFixed(2),
      h: +player.heading.toFixed(2),
      anim,
      carry: player.carrying,
      moving: !document.hidden && player.avatar.state === 'walk',
      voice: voice.isActive(),
      look: player.avatar.look || undefined,
    };
  }

  // Roster snapshot for the HUD (Room0Game renders the team panel from it).
  let rosterTimer = null;
  function emitRoster() {
    if (rosterTimer) return; // coalesce bursts
    rosterTimer = setTimeout(() => {
      rosterTimer = null;
      cb.onPeers?.([...peers.values()].map((p) => ({
        id: p.id, name: p.name, room: p.room, away: !!p.away,
      })));
    }, 250);
  }

  function removePeer(uid, announce) {
    const p = peers.get(uid);
    if (!p) return;
    for (const o of objects.values()) {
      if (o.holder === uid) { o.holder = null; o.x = p.x; o.z = p.z; }
    }
    scene.remove(p.avatar.root);
    peers.delete(uid);
    if (announce) cb.onChatLog?.({ system: true, text: `${p.name} left room0` });
    emitRoster();
    voiceSync();
  }

  function sendObject(o) {
    presence?.object?.({
      id: o.id, room: o.room,
      x: +o.x.toFixed(2), y: +o.z.toFixed(2),
      vx: +o.vx.toFixed(2), vy: +o.vz.toFixed(2),
      holder: o.holder === 'self' ? 'me' : o.holder, // server rewrites 'me' → user key
    });
  }

  function sendVehicleState(id, st) {
    presence?.object?.({
      id, room: st.room,
      x: +st.x.toFixed(2), y: +st.z.toFixed(2), h: +st.h.toFixed(2),
    });
  }

  function sendVehicle(v) {
    const st = { room: v.room, x: v.obj.position.x, z: v.obj.position.z, h: v.obj.rotation.y, owned: true };
    vehicleStates.set(v.id, st);
    sendVehicleState(v.id, st);
  }

  function ensurePeer(d) {
    let p = peers.get(d.user_id);
    if (!p) {
      p = {
        id: d.user_id, name: d.name || 'coworker', room: d.room || 'office',
        x: d.x ?? 10, z: d.y ?? 10, tx: d.x ?? 10, tz: d.y ?? 10,
        heading: d.h ?? 0, tHeading: d.h ?? 0, anim: d.anim || 'idle', moving: false,
        away: d.anim === 'away', voice: false, lastSeen: performance.now(),
        avatar: createAvatar(d.user_id, d.look || null),
      };
      p.tagRef = makeTag(p.name, false);
      p.avatar.labelAnchor.add(p.tagRef.obj);
      scene.add(p.avatar.root);
      p.avatar.root.position.set(p.x, 0, p.z);
      peers.set(d.user_id, p);
      emitRoster();
    }
    p.lastSeen = performance.now();
    return p;
  }

  function handlePresenceEvent(d) {
    if (disposed) return;
    if (d.type === 'rtc') {
      // Voice signaling — broadcast to the stream, addressed to one member.
      if (selfId && d.to === selfId) voice.handleSignal(d);
      return;
    }
    if (d.type === 'vnote') {
      // Voice-note wall update (new/del, broadcast by the API on create and
      // delete) — React owns the wall; must not fall through to ensurePeer.
      cb.onVoiceNote?.(d);
      return;
    }
    if (d.type === 'leave') {
      removePeer(d.user_id, true);
      return;
    }
    if (d.type === 'emote') {
      const p = ensurePeer(d);
      if (d.kind === 'wave') p.avatar.wave();
      else if (d.kind === 'dance') p.avatar.dance();
      else if (d.kind === 'ouch') { if (p.room === room.id) showBubble(p.tagRef, 'ouch ^$&*!', 4000); }
      else if (d.kind === 'typing') { if (p.room === room.id) showTyping(p.tagRef, true); }
      else if (d.kind === 'typing_stop') showTyping(p.tagRef, false);
      else if (d.kind === 'bubble_clear') { if (p.tagRef.timer) clearTimeout(p.tagRef.timer); p.tagRef.bubble.style.display = 'none'; }
      else if (EMOTE_EMOJI[d.kind] && p.room === room.id) spawnEmoji(p.tagRef, EMOTE_EMOJI[d.kind]);
      return;
    }
    if (d.type === 'object') {
      const o = objects.get(d.id);
      if (!o) {
        // A vehicle (the sea rowboat): remember where it is; if it's in view
        // the frame loop glides it there.
        if (typeof d.x === 'number') {
          const st = { room: d.room, x: d.x, z: d.y, h: d.h || 0, owned: false };
          vehicleStates.set(d.id, st);
          const v = world?.vehicle;
          if (v && v.id === d.id) { v.target = st; v.owned = false; }
        }
        return;
      }
      o.holder = d.holder || null;
      if (o.holder && o.holder !== 'self' && player.carrying === o.id) {
        // Someone snatched it out of our hands — they win.
        player.carrying = null;
        player.avatar.carrying = false;
        showBubble(selfTag, 'you have lost the lead', 4000);
        cb.onChatLog?.({ system: true, text: 'you have lost the lead' });
      }
      o.owned = false; // the sender simulates it now
      if (d.room && d.room !== o.room) {
        if (o.mesh && d.room !== room.id) { scene.remove(o.mesh); o.mesh = null; }
        o.room = d.room;
        if (o.room === room.id) attachObjectMesh(o);
      }
      if (typeof d.x === 'number') { o.x = d.x; o.z = d.y; }
      if (typeof d.vx === 'number') { o.vx = d.vx; o.vz = d.vy; }
      return;
    }
    // join / move
    const existed = peers.has(d.user_id);
    const p = ensurePeer(d);
    // Only a real join gets a notice — peers discovered through the
    // announce-on-join replay were already here.
    if (d.type === 'join' && !existed) {
      cb.onChatLog?.({ system: true, text: `${p.name} joined room0` });
    }
    if (d.name && d.name !== p.name) { p.name = d.name; p.tagRef.tag.textContent = d.name; emitRoster(); }
    if (d.look && d.look !== p.avatar.look) p.avatar.setLook(d.look);
    if (d.room !== undefined && d.room !== p.room) {
      p.room = d.room;
      p.avatar.root.visible = p.room === room.id;
      if (p.room === room.id) { p.x = d.x ?? p.x; p.z = d.y ?? p.z; } // no cross-room slide
      emitRoster();
      voiceSync(); // they changed sub-room → their voice channel changed too
    }
    const inVoice = !!d.voice;
    if (inVoice !== p.voice) {
      p.voice = inVoice;
      p.tagRef.mic.style.display = inVoice ? '' : 'none';
      voiceSync();
    }
    if (typeof d.x === 'number') { p.tx = d.x; p.tz = d.y; }
    if (typeof d.h === 'number') p.tHeading = d.h;
    p.anim = d.anim || (d.moving ? 'walk' : 'idle');
    p.moving = !!d.moving;
    p.carry = d.carry || null;
    const away = p.anim === 'away';
    if (away !== p.away) {
      p.away = away;
      p.tagRef.tag.classList.toggle(classes.nameTagAway || 'away', away);
      emitRoster();
    }
    // Late joiners learn who holds what from moves, not just object events.
    if (p.carry) {
      const held = objects.get(p.carry);
      if (held && !held.holder) held.holder = d.user_id;
    }
    if (d.type === 'join') {
      presence?.announce?.(selfState());
      for (const o of objects.values()) {
        // Re-announce objects we authoritatively moved so newcomers converge.
        if (o.owned || o.holder === 'self') sendObject(o);
      }
      for (const [id, st] of vehicleStates) {
        if (st.owned) sendVehicleState(id, st);
      }
    }
  }

  // ── Input ──
  // Only editable elements INSIDE the room overlay should swallow WASD (the
  // room's chat + board inputs). The app's own message composer, which opened
  // /room and sits focused behind the overlay — and which the app keeps
  // re-focusing — must not: otherwise the keyboard is dead the whole session.
  const overlayRoot = container.closest('.overlay') || container.parentElement;
  const editable = (el) => el && overlayRoot?.contains(el)
    && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable);
  function onKeyDown(e) {
    if (chatOpen || e.metaKey || e.ctrlKey || e.altKey || editable(document.activeElement)) return;
    ambient.userGesture(); // audio can only start after real input
    if (KEY_DIR[e.code]) { keys.add(e.code); e.preventDefault(); return; }
    if (EMOTE_BY_KEY[e.code]) { doEmote(EMOTE_BY_KEY[e.code]); return; }
    switch (e.code) {
      case 'ShiftLeft': case 'ShiftRight': runHeld = true; break;
      case 'Space':
        if (player.onGround && !player.sitting) {
          player.jumpV = JUMP_V;
          player.onGround = false;
        } else if (!player.onGround && player.jumpV < 0 && player.jumpY < 0.6) {
          jumpQueued = true; // buffered — fires the instant we touch down
        }
        e.preventDefault();
        break;
      case 'KeyF': interact(); e.preventDefault(); break;
      case 'KeyG':
        player.avatar.wave();
        presence?.emote?.({ kind: 'wave' });
        break;
      case 'KeyQ': camRig.targetYaw += Math.PI / 2; e.preventDefault(); break;
      case 'KeyE': camRig.targetYaw -= Math.PI / 2; e.preventDefault(); break;
      case 'KeyT': case 'Enter': cb.onOpenChat?.(); e.preventDefault(); break;
      case 'KeyP': cb.onToggleRoster?.(); break;
      case 'KeyM': toggleSound(); break;
      case 'Escape':
        // Clear our own chat bubble (they linger until replaced) — and tell
        // peers to drop their copy of it too.
        if (selfTag.timer) clearTimeout(selfTag.timer);
        selfTag.bubble.style.display = 'none';
        presence?.emote?.({ kind: 'bubble_clear' });
        break;
      case 'KeyV': toggleVoice(); break;
      case 'KeyR': cb.onRecordVoice?.(); break;
      default: break;
    }
  }
  function onKeyUp(e) {
    keys.delete(e.code);
    if (e.code === 'ShiftLeft' || e.code === 'ShiftRight') runHeld = false;
  }
  function onWheel(e) {
    e.preventDefault(); // ctrl+wheel would page-zoom otherwise
    if (e.ctrlKey || e.metaKey) {
      camRig.targetDist = Math.min(15, Math.max(5, camRig.targetDist + e.deltaY * 0.01));
    } else {
      camRig.targetPitch = Math.min(1.45, Math.max(0.15, camRig.targetPitch + e.deltaY * 0.002));
    }
  }
  // ── Tap / hold / pinch on the 3D canvas ──
  function screenToGround(clientX, clientY) {
    const rect = renderer.domElement.getBoundingClientRect();
    if (!rect.width || !rect.height) return null;
    ray.setFromCamera(new THREE.Vector2(
      ((clientX - rect.left) / rect.width) * 2 - 1,
      -((clientY - rect.top) / rect.height) * 2 + 1,
    ), camera);
    const t = -ray.ray.origin.y / ray.ray.direction.y;
    if (!(t > 0)) return null;
    const p = ray.ray.origin.clone().addScaledVector(ray.ray.direction, t);
    return {
      x: Math.min(room.w - 1.82, Math.max(0.82, p.x)),
      z: Math.min(room.h - 1.82, Math.max(0.82, p.z)),
    };
  }

  function setTargetFromScreen(clientX, clientY) {
    const g = screenToGround(clientX, clientY);
    if (!g) return;
    // Tapping on (or near) a pickup or a seat walks there and interacts.
    let interactHere = false;
    for (const o of objects.values()) {
      if (o.room !== room.id || o.holder || !DYN_TYPES[o.type].carryable) continue;
      if (Math.hypot(o.x - g.x, o.z - g.z) < 0.7) { g.x = o.x; g.z = o.z; interactHere = true; break; }
    }
    if (!interactHere && !player.carrying && world) {
      for (const s of world.seats) {
        if (Math.hypot(s.x - g.x, s.z - g.z) < 0.7) { g.x = s.x; g.z = s.z; interactHere = true; break; }
      }
    }
    moveTarget = { x: g.x, z: g.z, interact: interactHere };
    stuckT = 0;
  }

  function onPointerDown(e) {
    ambient.userGesture();
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      pinch0 = { dist: Math.hypot(a.x - b.x, a.y - b.y), cam: camRig.targetDist };
      moveTarget = null;
      tapInfo = null;
    } else if (pointers.size === 1 && !chatOpen) {
      tapInfo = { t: performance.now(), moved: 0 };
      setTargetFromScreen(e.clientX, e.clientY);
    }
  }
  function onPointerMove(e) {
    const p = pointers.get(e.pointerId);
    if (!p) return;
    if (tapInfo) tapInfo.moved += Math.hypot(e.clientX - p.x, e.clientY - p.y);
    p.x = e.clientX;
    p.y = e.clientY;
    if (pointers.size === 2 && pinch0) {
      const [a, b] = [...pointers.values()];
      const d = Math.max(40, Math.hypot(a.x - b.x, a.y - b.y));
      camRig.targetDist = Math.min(15, Math.max(5, (pinch0.cam * pinch0.dist) / d));
    } else if (pointers.size === 1 && tapInfo && tapInfo.moved > 10) {
      setTargetFromScreen(e.clientX, e.clientY); // hold → steer
    }
  }
  function onPointerUp(e) {
    pointers.delete(e.pointerId);
    if (pointers.size < 2) pinch0 = null;
    if (pointers.size === 0 && tapInfo) {
      // A tap = the finger didn't travel (any duration) → keep walking to
      // the destination. A steer-hold = the finger moved → releasing stops.
      // (Duration-based detection broke taps on slower devices: a deliberate
      // press easily exceeds any ms threshold and cancelled the walk.)
      if (tapInfo.moved >= 20) moveTarget = null;
      tapInfo = null;
    }
  }
  renderer.domElement.style.touchAction = 'none';
  renderer.domElement.addEventListener('pointerdown', onPointerDown);
  window.addEventListener('pointermove', onPointerMove);
  window.addEventListener('pointerup', onPointerUp);
  window.addEventListener('pointercancel', onPointerUp);

  window.addEventListener('keydown', onKeyDown, { capture: true });
  window.addEventListener('keyup', onKeyUp, { capture: true });
  container.addEventListener('wheel', onWheel, { passive: false });

  // ── Liveness ──
  // Heartbeat: re-announce even when idle (and from hidden tabs, where the
  // render loop is paused) so peers can tell "quiet" from "gone".
  const heartbeat = setInterval(() => { presence?.announce?.(selfState()); }, HEARTBEAT_MS);
  // Eviction: a peer whose leave was lost (killed tab, dropped connection)
  // stops heartbeating and gets swept here.
  const sweeper = setInterval(() => {
    const now = performance.now();
    for (const p of [...peers.values()]) {
      if (now - p.lastSeen > STALE_MS) removePeer(p.id, true);
    }
    voiceSync(); // also retries voice connections dropped by an ICE failure
  }, 5000);
  // Tab hidden/shown → tell peers + the roster immediately.
  const onVisibility = () => {
    presence?.move?.(selfState());
    cb.onSelfAway?.(document.hidden);
  };
  document.addEventListener('visibilitychange', onVisibility);

  // ── Sizing ──
  const ro = new ResizeObserver(() => {
    const w = container.clientWidth;
    const h = container.clientHeight;
    if (!w || !h) return;
    renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 1.75)); // lighting-heavy scene — keep fill rate sane on 3x phones
    renderer.setSize(w, h);
    labelRenderer.setSize(w, h);
    camera.aspect = w / h;
    camera.updateProjectionMatrix();
  });
  ro.observe(container);

  // ── Main loop ──
  function frame() {
    if (disposed) return;
    requestAnimationFrame(frame);
    const now = performance.now();
    // Clamp at 0.25s (not 0.1): on slower devices a tight clamp made game
    // time run below real time — walking felt slowed. Movement substeps and
    // bounded object velocities keep big steps collision-safe.
    const dt = Math.min((now - lastFrame) / 1000, 0.25);
    lastFrame = now;
    elapsed += dt;
    const t = elapsed;

    // Camera-relative move intent.
    let ix = 0, iz = 0;
    for (const code of keys) {
      const dir = KEY_DIR[code];
      if (dir === 'up') { ix -= Math.sin(camRig.yaw); iz -= Math.cos(camRig.yaw); }
      if (dir === 'down') { ix += Math.sin(camRig.yaw); iz += Math.cos(camRig.yaw); }
      if (dir === 'left') { ix -= Math.cos(camRig.yaw); iz += Math.sin(camRig.yaw); }
      if (dir === 'right') { ix += Math.cos(camRig.yaw); iz -= Math.sin(camRig.yaw); }
    }
    const keysMove = (ix || iz) && fade.phase === 'none' && !chatOpen;
    if (keysMove) moveTarget = null; // keyboard overrides tap-walking

    // Tap/hold target steering (auto-run for far destinations).
    let autoRun = false;
    let wantMove = keysMove;
    if (!keysMove && moveTarget && fade.phase === 'none' && !chatOpen) {
      const dx = moveTarget.x - player.x;
      const dz = moveTarget.z - player.z;
      const dist = Math.hypot(dx, dz);
      // Interactive targets (seats, pickups) sit on solid tiles you can't
      // stand on — trigger as soon as they're within reach.
      if (dist < 0.18 || (moveTarget.interact && dist < 0.95)) {
        if (moveTarget.interact) interact();
        moveTarget = null;
      } else {
        wantMove = true;
        ix = dx;
        iz = dz;
        autoRun = dist > 3.5;
      }
    }
    // Terrain base height (0 in tile rooms) — avatars, marker and camera all
    // ride the same heightAt the world's mesh was displaced with.
    const terrainY = (x, z) => (world?.heightAt ? world.heightAt(x, z) : 0);
    marker.visible = !!moveTarget;
    if (moveTarget) {
      marker.position.set(moveTarget.x, terrainY(moveTarget.x, moveTarget.z) + 0.03, moveTarget.z);
      const pulse = 1 + Math.sin(t * 6) * 0.15;
      marker.scale.setScalar(pulse);
    }

    // ── Vehicle (the rowboat): row it from a bench, glide it on remote updates ──
    const veh = world?.vehicle;
    const rowing = !!veh && veh.seats.includes(player.sitting);
    if (wantMove && player.sitting && !rowing) standUp();
    if (veh) {
      const obj = veh.obj;
      if (rowing && wantMove) {
        veh.target = null;
        veh.owned = true;
        veh.dirty = true;
        // Steer toward the intent direction, make way once roughly aligned.
        const want = Math.atan2(ix, iz);
        let dh = want - obj.rotation.y;
        while (dh > Math.PI) dh -= Math.PI * 2;
        while (dh < -Math.PI) dh += Math.PI * 2;
        obj.rotation.y += Math.max(-veh.turn * dt, Math.min(veh.turn * dt, dh));
        const align = Math.cos(dh);
        if (align > 0.15) {
          const step = veh.speed * align * dt;
          const nx = obj.position.x + Math.sin(obj.rotation.y) * step;
          const nz = obj.position.z + Math.cos(obj.rotation.y) * step;
          const ox = nx - room.w / 2;
          const oz = nz - room.h / 2;
          const max = room.radius - 1.5; // keep the hull inside the walkable water
          if (ox * ox + oz * oz < max * max) { obj.position.x = nx; obj.position.z = nz; }
        }
      } else if (veh.target && !veh.owned) {
        const k = Math.min(1, dt * 6);
        obj.position.x += (veh.target.x - obj.position.x) * k;
        obj.position.z += (veh.target.z - obj.position.z) * k;
        let dh = veh.target.h - obj.rotation.y;
        while (dh > Math.PI) dh -= Math.PI * 2;
        while (dh < -Math.PI) dh += Math.PI * 2;
        obj.rotation.y += dh * k;
      }
      veh.sync();
      if (veh.dirty && now - (veh.lastSent || 0) > SEND_MS) {
        sendVehicle(veh);
        veh.lastSent = now;
        veh.dirty = false;
      }
      if (rowing) {
        // The bench carries the sitter — same pin sitOn() applies.
        const s = player.sitting;
        player.x = s.x + Math.sin(s.ry) * 0.1;
        player.z = s.z + Math.cos(s.ry) * 0.1;
        player.heading = s.ry;
        player.vx = 0;
        player.vz = 0;
      }
    }

    const speed = ((runHeld || autoRun) ? RUN_SPEED : WALK_SPEED) * bhop;
    if (wantMove && !rowing) {
      const len = Math.hypot(ix, iz);
      player.vx = (ix / len) * speed;
      player.vz = (iz / len) * speed;
      player.heading = Math.atan2(player.vx, player.vz);
      // Substep so low-FPS frames don't take strides that jump over the
      // narrow walkable corridors (door pockets, wall glides).
      const sub = Math.max(1, Math.ceil(dt / 0.04));
      const sdt = dt / sub;
      let movedX = false, movedZ = false;
      for (let i = 0; i < sub; i++) {
        const nx = player.x + player.vx * sdt;
        const nz = player.z + player.vz * sdt;
        if (canStand(nx, player.z)) { player.x = nx; movedX = true; }
        if (canStand(player.x, nz)) { player.z = nz; movedZ = true; }
      }
      player.pushT = (!movedX && !movedZ) ? player.pushT + dt : 0;
      // Give up on an unreachable tap target instead of shoving the wall —
      // but if it was an interactive tap and we got within reach, interact.
      if (moveTarget && !movedX && !movedZ) {
        stuckT += dt;
        if (stuckT > 0.6) {
          if (moveTarget.interact
            && Math.hypot(moveTarget.x - player.x, moveTarget.z - player.z) < 1.05) {
            interact();
          }
          moveTarget = null;
        }
      } else {
        stuckT = 0;
      }
    } else {
      player.vx = 0; player.vz = 0; player.pushT = 0;
    }

    // Jump physics — with bunny hopping (buffered jumps chain + stack speed)
    // and platform landings (benches/loungers are standable tops).
    const gY = groundYAt(player.x, player.z);
    if (player.sitting) {
      // Seats handle their own height (sitY); don't let the platform ground
      // snap lift a lounger-lier to the lounger's own platform top.
      player.jumpY = 0;
      player.jumpV = 0;
      player.onGround = true;
      jumpQueued = false;
    } else if (!player.onGround) {
      player.jumpV -= GRAVITY * dt;
      player.jumpY += player.jumpV * dt;
      if (player.jumpV <= 0 && player.jumpY <= gY) {
        player.jumpY = gY;
        if (jumpQueued && wantMove) {
          jumpQueued = false;
          player.jumpV = JUMP_V * 0.95;
          bhop = Math.min(1.8, bhop + 0.18);
        } else {
          jumpQueued = false;
          player.jumpV = 0;
          player.onGround = true;
        }
      }
    } else if (player.jumpY > gY + 0.01) {
      // Walked off a platform edge — fall.
      player.onGround = false;
      player.jumpV = 0;
    } else {
      player.jumpY = gY;
      bhop = Math.max(1, bhop - dt * 2.5); // boost bleeds off on the ground
    }
    player.avatar.jumpY = player.jumpY;

    // Avatar state.
    if (player.sitting) player.avatar.setState('sit');
    else if (player.pushT > 0.15) player.avatar.setState('push');
    else if (wantMove) player.avatar.setState('walk');
    else player.avatar.setState('idle');
    const selfY = terrainY(player.x, player.z);
    player.avatar.root.position.set(player.x, selfY, player.z);
    player.avatar.root.rotation.y = player.heading;
    player.avatar.update(dt, wantMove ? speed : 0);

    // Door transitions — distance-based (tile-rounding is too strict: the
    // wall-collision radius stops you ~0.8 from a door tile's centre, so you
    // could brush a doorway forever without ever "standing on" it).
    if (fade.phase === 'none' && !player.sitting) {
      const door = (room.doors || []).find((d) => {
        const dx = d.x - player.x;
        const dz = d.y - player.z;
        const dist = Math.hypot(dx, dz);
        if (dist >= 1.0) return false;
        // Close: require walking toward it (or being right on top of it) so
        // brushing past a doorway while chasing a ball doesn't teleport you.
        return dist < 0.6 || dx * player.vx + dz * player.vz > 0;
      });
      if (door) {
        fade.phase = 'out';
        fade.onMid = () => enterRoom(door.to, room.id);
      }
    } else if (fade.phase === 'out') {
      fade.a = Math.min(1, fade.a + dt / 0.22);
      if (fade.a >= 1) {
        fade.onMid?.();
        fade.onMid = null;
        fade.phase = 'in';
      }
    } else if (fade.phase === 'in') {
      fade.a = Math.max(0, fade.a - dt / 0.22);
      if (fade.a <= 0) fade.phase = 'none';
    }
    fadeEl.style.opacity = String(fade.a);

    // ── Dynamic objects ──
    let rollLevel = 0;
    for (const o of objects.values()) {
      const meta = DYN_TYPES[o.type];
      if (o.holder) {
        // Carried: follow the holder's carry anchor.
        const holderAv = o.holder === 'self' ? player.avatar : peers.get(o.holder)?.avatar;
        if (o.mesh && holderAv) {
          holderAv.carryAnchor.getWorldPosition(o.mesh.position);
          o.mesh.rotation.y += dt * 1.2;
          const src = o.holder === 'self' ? player : peers.get(o.holder);
          if (src) { o.x = src.x; o.z = src.z; }
        }
        continue;
      }
      if (o.room !== room.id) continue;

      // Getting beaned: an airborne object (someone's throw) clipping us.
      // Each client detects its own hits and broadcasts the "ouch".
      if (o.y > 0.12 && Math.hypot(o.x - player.x, o.z - player.z) < meta.radius + 0.5
        && now - (o.selfThrowT || 0) > 1200 && now - lastOuchT > 1500) {
        lastOuchT = now;
        showBubble(selfTag, 'ouch ^$&*!', 4000);
        presence?.emote?.({ kind: 'ouch' });
      }

      // Player push/kick: overlap resolves away from the player and imparts
      // velocity scaled by the type's kick factor.
      const dx = o.x - player.x;
      const dz = o.z - player.z;
      const dist = Math.hypot(dx, dz);
      const minD = meta.radius + PLAYER_R + 0.02;
      if (dist < minD && dist > 0.0001 && !player.sitting) {
        const nx = dx / dist;
        const nz = dz / dist;
        const overlap = minD - dist;
        if (objectCanStand(o, o.x + nx * overlap, o.z + nz * overlap, meta.radius)) {
          o.x += nx * overlap;
          o.z += nz * overlap;
        }
        const pv = Math.hypot(player.vx, player.vz);
        if (pv > 0.1) {
          o.vx += nx * pv * meta.kick * dt * 4;
          o.vz += nz * pv * meta.kick * dt * 4;
          o.owned = true;
        }
      }

      if (meta.sfx === 'ball' && o.y === 0 && !o.holder) {
        rollLevel = Math.max(rollLevel, Math.min(1, Math.hypot(o.vx, o.vz) / 5));
      }
      // Integrate with friction + wall bounces.
      if (o.vx || o.vz || o.y > 0) {
        const f = Math.max(0, 1 - meta.friction * dt);
        o.vx *= f;
        o.vz *= f;
        if (Math.abs(o.vx) < 0.02) o.vx = 0;
        if (Math.abs(o.vz) < 0.02) o.vz = 0;
        const nx = o.x + o.vx * dt;
        const nz = o.z + o.vz * dt;
        if (objectCanStand(o, nx, o.z, meta.radius)) o.x = nx;
        else { o.vx = -o.vx * meta.restitution; if (Math.abs(o.vx) > 1.2) ambient.impact?.(meta.sfx, Math.min(1, Math.abs(o.vx) / 6)); }
        if (objectCanStand(o, o.x, nz, meta.radius)) o.z = nz;
        else { o.vz = -o.vz * meta.restitution; if (Math.abs(o.vz) > 1.2) ambient.impact?.(meta.sfx, Math.min(1, Math.abs(o.vz) / 6)); }
        if (o.y > 0 || o.vy) {
          o.vy -= GRAVITY * 0.55 * dt;
          o.y += o.vy * dt;
          if (o.y <= 0) {
            o.y = 0;
            const impact = Math.abs(o.vy);
            // Keep bouncing until the hop is genuinely small — reads as a
            // real bounce instead of a dead stop on the first landing.
            o.vy = impact > 0.5 ? impact * meta.restitution : 0;
            if (impact > 1.1) ambient.impact?.(meta.sfx, Math.min(1, impact / 5));
          }
        }
        // Owner streams position while it moves.
        if (o.owned && now - (o.lastSent || 0) > SEND_MS) {
          sendObject(o);
          o.lastSent = now;
        }
        if (o.owned && !o.vx && !o.vz && o.y === 0) {
          sendObject(o); // final rest position
          o.owned = false;
        }
      }
      // Object-object separation (cheap, same room only).
      for (const o2 of objects.values()) {
        if (o2 === o || o2.room !== room.id || o2.holder) continue;
        const ddx = o2.x - o.x;
        const ddz = o2.z - o.z;
        const dd = Math.hypot(ddx, ddz);
        const md = meta.radius + DYN_TYPES[o2.type].radius;
        if (dd < md && dd > 0.0001) {
          const push = (md - dd) / 2;
          const ux = ddx / dd;
          const uz = ddz / dd;
          if (objectCanStand(o2, o2.x + ux * push, o2.z + uz * push, DYN_TYPES[o2.type].radius)) {
            o2.x += ux * push;
            o2.z += uz * push;
          }
          if (objectCanStand(o, o.x - ux * push, o.z - uz * push, meta.radius)) {
            o.x -= ux * push;
            o.z -= uz * push;
          }
        }
      }
      if (o.mesh) {
        o.mesh.position.set(o.x, o.y, o.z);
        const spin = o.mesh.userData.spin;
        if (spin && (o.vx || o.vz)) {
          spin.rotation.x += o.vz * dt / meta.radius;
          spin.rotation.z -= o.vx * dt / meta.radius;
        }
      }
    }
    ambient.setRolling?.(rollLevel);

    // ── Remote peers: interpolate + animate ──
    for (const p of peers.values()) {
      const k = Math.min(1, dt * 11);
      p.x += (p.tx - p.x) * k;
      p.z += (p.tz - p.z) * k;
      let dh = p.tHeading - p.heading;
      while (dh > Math.PI) dh -= Math.PI * 2;
      while (dh < -Math.PI) dh += Math.PI * 2;
      p.heading += dh * k;
      p.avatar.root.position.set(p.x, p.room === room.id ? terrainY(p.x, p.z) : 0, p.z);
      p.avatar.root.rotation.y = p.heading;
      p.avatar.carrying = !!p.carry;
      const seated = p.anim === 'sit' || p.anim === 'type' || p.anim === 'lie' || p.anim === 'perch';
      p.avatar.typing = p.anim === 'type';
      p.avatar.lying = p.anim === 'lie';
      p.avatar.sitY = p.anim === 'perch' ? 2.0 : p.anim === 'lie' ? 0.34 : 0.38;
      p.avatar.setState(seated ? 'sit' : (p.moving ? 'walk' : 'idle'));
      const spd = Math.hypot(p.tx - p.x, p.tz - p.z) > 0.03 || p.moving ? WALK_SPEED : 0;
      if (seated && veh && p.room === room.id) {
        // A rider's 10 Hz move stream trails the gliding boat — pin the
        // avatar to the nearest bench instead (render only, p.x/p.z stay).
        let bs = null;
        let bd = 1.1;
        for (const s of veh.seats) {
          const d = Math.hypot(s.x - p.x, s.z - p.z);
          if (d < bd) { bd = d; bs = s; }
        }
        if (bs) {
          p.avatar.root.position.set(bs.x + Math.sin(bs.ry) * 0.1, terrainY(bs.x, bs.z), bs.z + Math.cos(bs.ry) * 0.1);
          p.avatar.root.rotation.y = bs.ry;
        }
      }
      p.avatar.update(dt, spd);
      p.avatar.root.visible = p.room === room.id;
      // CSS2D labels don't reliably inherit ancestor visibility — gate them.
      p.tagRef.obj.visible = p.room === room.id;
    }

    // ── Workstation chairs tuck in under the desk while occupied ──
    if (world) {
      for (const s of world.seats) {
        if (!s.tuck) continue;
        let occupied = player.sitting?.key === s.key;
        if (!occupied) {
          for (const p of peers.values()) {
            if (p.room === room.id && (p.anim === 'type' || p.anim === 'sit')
              && Math.hypot(p.x - s.x, p.z - s.z) < 0.55) { occupied = true; break; }
          }
        }
        const k = Math.min(1, dt * 5);
        s.tuck.holder.position.x += ((occupied ? s.tuck.tuckX : s.tuck.baseX) - s.tuck.holder.position.x) * k;
        s.tuck.holder.position.z += ((occupied ? s.tuck.tuckZ : s.tuck.baseZ) - s.tuck.holder.position.z) * k;
      }
    }

    // ── World FX + camera ──
    if (world) tickWorld(world.animated, t, dt);
    world?.update?.(t, dt, camera.position);
    camRig.yaw += (camRig.targetYaw - camRig.yaw) * Math.min(1, dt * 6);
    camRig.dist += (camRig.targetDist - camRig.dist) * Math.min(1, dt * 6);
    camRig.pitch += (camRig.targetPitch - camRig.pitch) * Math.min(1, dt * 6);
    const cx = player.x + Math.sin(camRig.yaw) * camRig.dist * Math.cos(camRig.pitch);
    const cz = player.z + Math.cos(camRig.yaw) * camRig.dist * Math.cos(camRig.pitch);
    // On terrain the rig rides the ground under the camera too, so a dune
    // between us and the player can't swallow the view.
    const cy = Math.max(1 + camRig.dist * Math.sin(camRig.pitch) + selfY, terrainY(cx, cz) + 1.4);
    camera.position.lerp(new THREE.Vector3(cx, cy, cz), Math.min(1, dt * 8));
    camera.lookAt(player.x, 1.0 + selfY, player.z);

    // ── Prompts + proximity ──
    let prompt = null;
    if (player.sitting) {
      prompt = player.sitting.perch ? 'F — climb down'
        : rowing ? 'WASD — row · F — step off' : 'F — stand up';
    }
    else if (player.carrying) prompt = 'F — throw';
    else if (nearestCarryable()) prompt = 'F — pick up';
    else if (nearestStealable()) prompt = 'F — steal it';
    else {
      const seat = nearestSeat();
      if (seat) {
        prompt = seat.label ? `F — ${seat.label}`
          : seat.perch ? 'F — climb the tree'
            : seat.pose === 'lie' ? 'F — lie down'
              : seat.pose === 'type' ? 'F — work at the desk' : 'F — sit';
      } else if (nearestWhiteboard()) {
        prompt = nearestWhiteboard().kind === 'tasks' ? 'F — open the This Week board' : 'F — open the sketch board';
      } else if (nearestLandmark(world?.tv)) prompt = 'F — watch TV';
      else if (nearestLandmark(world?.fountain)) prompt = 'F — drink from the fountain';
    }
    if (prompt !== lastPrompt) { lastPrompt = prompt; cb.onPrompt?.(prompt); }
    const near = nearestPeer();
    const nearName = near?.name || null;
    if (nearName !== lastNear) { lastNear = nearName; cb.onNearPeer?.(nearName); }

    // ── Spatial voice: ears follow the player, voices follow their avatars ──
    if (voice.isActive()) {
      voice.setListener(player.x, player.z, player.heading);
      for (const p of peers.values()) {
        if (p.voice) voice.setPeerPos(p.id, p.x, p.z);
      }
    }

    // ── Outgoing presence (throttled, on change) ──
    if (presence) {
      const s = selfState();
      const changed = s.x !== lastSent.x || s.y !== lastSent.z || s.h !== lastSent.h
        || s.anim !== lastSent.anim || s.room !== lastSent.room || s.carry !== lastSent.carry;
      if (changed && now - lastSent.t > SEND_MS) {
        presence.move(s);
        Object.assign(lastSent, { t: now, x: s.x, z: s.y, h: s.h, anim: s.anim, room: s.room, carry: s.carry });
      }
    }

    renderer.render(scene, camera);
    labelRenderer.render(scene, camera);
  }

  enterRoom(roomId, null);
  // Restore a saved position (room.ts persists it across open/close) — only
  // onto a cell that is still walkable in this seed's world.
  if (spawnAt && isWalkable(room, Math.round(spawnAt.x), Math.round(spawnAt.z), blocked)) {
    player.x = spawnAt.x;
    player.z = spawnAt.z;
  }
  requestAnimationFrame(frame);
  cb.onReady?.();

  // Debug/automation hook (harmless in prod; used by the smoke-test driver).
  window.__room0 = () => ({
    room: room.id,
    x: +player.x.toFixed(2), z: +player.z.toFixed(2),
    keys: [...keys], anim: player.avatar.state, fade: fade.phase,
    hour: +currentHour().toFixed(2),
    target: moveTarget ? { x: +moveTarget.x.toFixed(2), z: +moveTarget.z.toFixed(2), interact: moveTarget.interact } : null,
    jy: +player.jumpY.toFixed(2),
    falling: !player.onGround && player.jumpV < 0,
    ground: player.onGround,
    bhop: +bhop.toFixed(2),
  });
  window.__room0.ground = (x, y) => screenToGround(x, y);
  window.__room0.seats = () => (world?.seats || []).map((s) => ({
    x: +s.x.toFixed(2), z: +s.z.toFixed(2), pose: s.pose,
    d: +Math.hypot(s.x - player.x, s.z - player.z).toFixed(2),
  }));
  window.__room0.setHour = (h) => {
    hourOverride = h == null ? null : +h;
    applyDaylight();
  };
  window.__room0.go = (id) => {
    if (!ROOMS[id]) return;
    enterRoom(id, null);
    player.x = room.spawn.gx;
    player.z = room.spawn.gy;
    presence?.announce?.(selfState());
  };
  window.__room0.tp = (x, z) => { player.x = +x; player.z = +z; };

  // ── Public API ──
  return {
    getSelfState: selfState,
    setPresence(ctrl) { presence = ctrl; },
    // The auth session (and with it our user key) restores after mount.
    setSelfKey(key) { selfId = key || null; },
    handlePresenceEvent,
    sayAs,
    setChatOpen(open) {
      chatOpen = open;
      if (open) keys.clear();
      // Typing indicator for everyone in our room.
      presence?.emote?.({ kind: open ? 'typing' : 'typing_stop' });
    },
    emote: doEmote,
    visitPeer,
    interact,
    jump() {
      if (player.onGround && !player.sitting) {
        player.jumpV = JUMP_V;
        player.onGround = false;
      } else if (!player.onGround && player.jumpV < 0 && player.jumpY < 0.6) {
        jumpQueued = true; // bunny hop from the touch button too
      }
    },
    rotateCamera(dir) { camRig.targetYaw += dir * (Math.PI / 2); },
    toggleSound,
    toggleVoice,
    toggleMic() { voice?.toggleMic(); },
    // Character customizer: apply a look string to our avatar and tell peers.
    setLook(str) {
      player.avatar.setLook(str);
      presence?.announce?.(selfState());
    },
    sendChat: sendChatMsg,
    setScreenName(name) {
      myName = name || 'You';
      selfTag.tag.textContent = myName;
    },
    getRoomName() { return room.name; },
    dispose() {
      disposed = true;
      ro.disconnect();
      ambient.dispose();
      voice.dispose();
      clearInterval(heartbeat);
      clearInterval(sweeper);
      clearInterval(daylightTimer);
      if (rosterTimer) clearTimeout(rosterTimer);
      document.removeEventListener('visibilitychange', onVisibility);
      window.removeEventListener('keydown', onKeyDown, { capture: true });
      window.removeEventListener('keyup', onKeyUp, { capture: true });
      container.removeEventListener('wheel', onWheel);
      renderer.domElement.removeEventListener('pointerdown', onPointerDown);
      window.removeEventListener('pointermove', onPointerMove);
      window.removeEventListener('pointerup', onPointerUp);
      window.removeEventListener('pointercancel', onPointerUp);
      for (const p of peers.values()) scene.remove(p.avatar.root);
      peers.clear();
      renderer.dispose();
      container.removeChild(renderer.domElement);
      container.removeChild(labelEl);
      container.removeChild(fadeEl);
    },
  };
}
