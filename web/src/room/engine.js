// room0 — isometric engine (pure logic, no React / no Canvas side-effects).
//
// Coordinate system
// -----------------
// World space is measured in tile units (gx, gy) and can be fractional — the
// player moves freely in pixel space, not tile-by-tile (Zelda-like). Screen
// space uses a true 2:1 isometric (diamond) projection: a tile is TILE_W wide
// and TILE_H tall (TILE_W = 2 * TILE_H).
//
//   sx = (gx - gy) * (TILE_W / 2)
//   sy = (gx + gy) * (TILE_H / 2)
//
// Depth (painter's algorithm) is (gx + gy): larger = nearer the viewer, drawn
// later so it occludes things behind it.

export const TILE_W = 64;
export const TILE_H = 32;
export const HALF_W = TILE_W / 2;
export const HALF_H = TILE_H / 2;

// Tile codes in a room's `tiles` grid.
export const VOID = 0; // nothing — not drawn, not walkable
export const FLOOR = 1; // walkable ground
export const WALL = 2; // solid raised wall — blocks movement, occludes

// World (tile) → screen (pixel) before camera offset.
export function worldToScreen(gx, gy) {
  return { sx: (gx - gy) * HALF_W, sy: (gx + gy) * HALF_H };
}

// Screen (pixel) → world (tile). Inverse of worldToScreen.
export function screenToWorld(sx, sy) {
  return {
    gx: (sx / HALF_W + sy / HALF_H) / 2,
    gy: (sy / HALF_H - sx / HALF_W) / 2,
  };
}

// The four on-screen movement directions mapped to world-axis deltas. Each maps
// to a pure screen-axis move (up = straight up the screen, etc.) so the controls
// feel natural despite the diagonal world grid. Combined keys give 8-way.
export const MOVE = {
  up: { dx: -1, dy: -1, facing: 'up' },
  down: { dx: 1, dy: 1, facing: 'down' },
  left: { dx: -1, dy: 1, facing: 'left' },
  right: { dx: 1, dy: -1, facing: 'right' },
};

// Pick a 4-way facing from a world-space velocity vector (screen-relative).
export function facingFromVelocity(vx, vy) {
  if (vx === 0 && vy === 0) return null;
  // Convert world delta back to screen delta to choose the dominant screen axis.
  const sdx = (vx - vy);
  const sdy = (vx + vy);
  if (Math.abs(sdy) >= Math.abs(sdx)) return sdy < 0 ? 'up' : 'down';
  return sdx < 0 ? 'left' : 'right';
}

// Is the tile at integer (gx, gy) walkable? Floors and doors are; walls, voids,
// and out-of-bounds are not. `blocked` is a Set of "x,y" prop-occupied cells.
// World rooms (kind:'world') have no tile grid: walkable is a radius around
// the centre, minus the obstacle cells worlds3d stamps into room._blocked
// (tree trunks, the fire pit, consoles) when it builds the seeded scene.
export function isWalkable(room, gx, gy, blocked) {
  if (room.kind === 'world') {
    const dx = gx - room.w / 2;
    const dy = gy - room.h / 2;
    if (dx * dx + dy * dy > room.radius * room.radius) return false;
    if (room._blocked && room._blocked.has(`${gx},${gy}`)) return false;
    return true;
  }
  if (gx < 0 || gy < 0 || gx >= room.w || gy >= room.h) return false;
  const t = room.tiles[gy][gx];
  if (t !== FLOOR) return false;
  if (blocked && blocked.has(`${gx},${gy}`)) return false;
  return true;
}

// Build the set of prop-occupied (blocked) cells for a room.
export function blockedCells(room) {
  const set = new Set();
  for (const p of room.props || []) {
    if (PROPS[p.type]?.solid !== false) set.add(`${p.x},${p.y}`);
  }
  return set;
}

// Find a door at the player's current tile, if any.
export function doorAt(room, gx, gy) {
  const ix = Math.round(gx);
  const iy = Math.round(gy);
  return (room.doors || []).find((d) => d.x === ix && d.y === iy) || null;
}

// ── Prop catalogue ──────────────────────────────────────────────────────────
// Placeholder render hints until real sprites land (Phase 1 step 2). `h` is the
// pixel height of the iso box; `color` is the box top; `solid:false` = walk-thru.
export const PROPS = {
  desk: { h: 18, color: '#3a4a66', label: '🖥' },
  computer: { h: 26, color: '#1c2740', glow: '#39c6ff', solid: false },
  chair: { h: 16, color: '#2c3a52' },
  server: { h: 40, color: '#222d44', glow: '#39c6ff' },
  plant: { h: 22, color: '#2f5d4a' },
  cooler: { h: 30, color: '#3a4660', glow: '#6fd8ff' },
  whiteboard: { h: 34, color: '#46506a', glow: '#6fd8ff' },
  holo: { h: 18, color: '#243a5a', glow: '#39c6ff' },
  // Side-room flavour props (rendered the same way for now).
  tree: { h: 44, color: '#2f5d3a' },
  bench: { h: 14, color: '#4a3a2a' },
  fountain: { h: 20, color: '#3a5a7a', glow: '#6fd8ff' },
  counter: { h: 20, color: '#46506a' },
  table: { h: 18, color: '#5a4030' },
  tv: { h: 34, color: '#1c2430' },
  fridge: { h: 40, color: '#3a4660' },
  sofa: { h: 18, color: '#5a3344' },
  bar: { h: 22, color: '#4a3528', glow: '#ffb347' },
  fireplace: { h: 24, color: '#3a2330', glow: '#ff7a3c' },
  palm: { h: 46, color: '#3a6a4a' },
  umbrella: { h: 30, color: '#c2536a' },
  lounger: { h: 12, color: '#c9a86a' },
};

// ── Room definitions ──────────────────────────────────────────────────────────
// A small helper builds a rectangular room: a wall border with a floor interior.
function rect(w, h) {
  const tiles = [];
  for (let y = 0; y < h; y++) {
    const row = [];
    for (let x = 0; x < w; x++) {
      const edge = x === 0 || y === 0 || x === w - 1 || y === h - 1;
      row.push(edge ? WALL : FLOOR);
    }
    tiles.push(row);
  }
  return tiles;
}

// Carve a floor opening in a wall so a door is reachable.
function carve(tiles, x, y) {
  tiles[y][x] = FLOOR;
}

// The central office — a large open floor. 4 doors (one per side-room) punched
// into the walls, centred on each side.
function buildOffice() {
  const w = 22;
  const h = 22;
  const tiles = rect(w, h);
  // Door openings: top (garden, x=11), right (kitchen, y=11), bottom (lounge,
  // x=10), left (beach, y=10).
  carve(tiles, 11, 0);
  carve(tiles, w - 1, 11);
  carve(tiles, 10, h - 1);
  carve(tiles, 0, 10);
  return {
    id: 'office',
    name: 'The Office',
    w,
    h,
    tiles,
    spawn: { gx: 11, gy: 11 },
    floorTint: '#16213a', // deep navy
    // Desk pods fill the four quadrants. The door approach-lanes — columns 10 &
    // 11 and rows 10 & 11 — are deliberately kept clear so every door is reachable.
    props: [
      // Top-left quadrant.
      { type: 'desk', x: 3, y: 3 }, { type: 'computer', x: 3, y: 2 }, { type: 'chair', x: 3, y: 4 },
      { type: 'desk', x: 6, y: 6 }, { type: 'computer', x: 6, y: 5 }, { type: 'chair', x: 6, y: 7 },
      // Top-right quadrant.
      { type: 'desk', x: 18, y: 3 }, { type: 'computer', x: 18, y: 2 }, { type: 'chair', x: 18, y: 4 },
      { type: 'desk', x: 15, y: 6 }, { type: 'computer', x: 15, y: 5 }, { type: 'chair', x: 15, y: 7 },
      // Bottom-left quadrant.
      { type: 'desk', x: 3, y: 18 }, { type: 'computer', x: 3, y: 17 }, { type: 'chair', x: 3, y: 19 },
      { type: 'desk', x: 6, y: 15 }, { type: 'computer', x: 6, y: 14 }, { type: 'chair', x: 6, y: 16 },
      // Bottom-right quadrant.
      { type: 'desk', x: 18, y: 18 }, { type: 'computer', x: 18, y: 17 }, { type: 'chair', x: 18, y: 19 },
      { type: 'desk', x: 15, y: 15 }, { type: 'computer', x: 15, y: 14 }, { type: 'chair', x: 15, y: 16 },
      // Server racks along the top, off the door lane.
      { type: 'server', x: 8, y: 2 }, { type: 'server', x: 9, y: 2 },
      { type: 'server', x: 12, y: 2 }, { type: 'server', x: 13, y: 2 },
      // Plants — flanking the side walls, greening the corners and breaking
      // up the open floor between the desk pods.
      { type: 'plant', x: 2, y: 8 }, { type: 'plant', x: 19, y: 8 },
      { type: 'plant', x: 2, y: 13 }, { type: 'plant', x: 19, y: 13 },
      { type: 'plant', x: 2, y: 2 }, { type: 'plant', x: 19, y: 2 },
      { type: 'plant', x: 2, y: 19 }, { type: 'plant', x: 19, y: 19 },
      { type: 'plant', x: 5, y: 2 }, { type: 'plant', x: 16, y: 2 },
      { type: 'plant', x: 8, y: 12 }, { type: 'plant', x: 13, y: 12 },
      // Artefacts — water coolers, whiteboards, a hologram table centrepiece.
      { type: 'cooler', x: 2, y: 9 }, { type: 'cooler', x: 19, y: 12 },
      { type: 'whiteboard', x: 8, y: 8 }, { type: 'whiteboard', x: 15, y: 8 },
      { type: 'holo', x: 13, y: 13 },
    ],
    // Floor decals (glowing tech rings) — purely visual, walkable.
    decals: [[4, 4], [17, 4], [4, 17], [17, 17], [8, 13], [13, 8]],
    doors: [
      { x: 11, y: 0, to: 'garden', label: '🌿 Garden' },
      { x: w - 1, y: 11, to: 'kitchen', label: '🍳 Kitchen' },
      { x: 10, y: h - 1, to: 'lounge', label: '🥃 Lounge' },
      { x: 0, y: 10, to: 'beach', label: '🏖 Beach' },
    ],
  };
}

// A themed side-room with a single return door back to the office.
function buildSideRoom(id, name, floorTint, props, returnDoor) {
  const w = 14;
  const h = 14;
  const tiles = rect(w, h);
  carve(tiles, returnDoor.x, returnDoor.y);
  return {
    id,
    name,
    w,
    h,
    tiles,
    floorTint,
    spawn: { gx: 7, gy: 7 },
    props,
    doors: [{ ...returnDoor, to: 'office', label: '↩ Office' }],
  };
}

export const ROOMS = {
  office: buildOffice(),
  garden: buildSideRoom('garden', 'The Garden', '#16321f',
    [{ type: 'tree', x: 2, y: 2 }, { type: 'tree', x: 11, y: 3 }, { type: 'bench', x: 7, y: 9 },
     { type: 'fountain', x: 7, y: 5 }, { type: 'tree', x: 10, y: 10 }, { type: 'tree', x: 3, y: 10 }],
    { x: 7, y: 13 }),
  kitchen: buildSideRoom('kitchen', 'The Pantry', '#2a2438',
    [{ type: 'counter', x: 2, y: 2 }, { type: 'counter', x: 3, y: 2 }, { type: 'fridge', x: 11, y: 2 },
     { type: 'counter', x: 2, y: 11 }, { type: 'counter', x: 11, y: 11 },
     // Kitchen table with chairs around it (chairs auto-face adjacent tables).
     { type: 'table', x: 7, y: 6 }, { type: 'chair', x: 6, y: 6 },
     { type: 'chair', x: 8, y: 6 }, { type: 'chair', x: 7, y: 7 }],
    { x: 0, y: 7 }),
  lounge: buildSideRoom('lounge', 'Lounge', '#2a1c22',
    // Couch corner: two sofas facing each other across a coffee table, with
    // a TV to watch; bar off to the side (it used to block the entrance).
    // TV sits opposite the couches (bench moved over by the fireplace).
    [{ type: 'sofa', x: 6, y: 5 }, { type: 'table', x: 7, y: 5 }, { type: 'sofa', x: 8, y: 5 },
     { type: 'bar', x: 3, y: 2 }, { type: 'fireplace', x: 11, y: 11 }, { type: 'bench', x: 7, y: 10 },
     { type: 'tv', x: 7, y: 8 }],
    { x: 7, y: 0 }),
  beach: buildSideRoom('beach', 'Beachfront', '#243a44',
    [{ type: 'palm', x: 2, y: 2 }, { type: 'palm', x: 11, y: 11 }, { type: 'umbrella', x: 8, y: 5 },
     { type: 'lounger', x: 8, y: 6 }, { type: 'lounger', x: 5, y: 8 }, { type: 'palm', x: 3, y: 11 }],
    { x: 13, y: 7 }),
};

// ── Outdoor worlds ────────────────────────────────────────────────────────────
// kind:'world' rooms have no tile grid. Their scene is generated (seeded per
// channel) by three/worlds3d.js; movement uses the radius rule in isWalkable
// and the terrain height from the world's heightAt. Coordinates stay in the
// same tile-unit space, centred at (w/2, h/2).
function world(id, name, size, radius, door) {
  return {
    id, name, kind: 'world', w: size, h: size, radius,
    spawn: { gx: door.x, gy: door.y - 2 },
    doors: [door], props: [],
  };
}
ROOMS.sea = world('sea', 'The Open Sea', 60, 26, { x: 30, y: 54, to: 'beach', label: '↩ Beachfront' });
ROOMS.dunes = world('dunes', 'The Dunes', 80, 36, { x: 40, y: 74, to: 'kitchen', label: '↩ Pantry' });
ROOMS.camp = world('camp', 'The Campfire', 60, 26, { x: 30, y: 54, to: 'garden', label: '↩ Garden' });
ROOMS.space = world('space', 'The Space Deck', 24, 8.4, { x: 12, y: 19, to: 'lounge', label: '↩ Lounge' });
ROOMS.terminal = world('terminal', 'The Terminal', 60, 26, { x: 30, y: 54, to: 'office', label: '↩ Office' });

// Doors onward to the worlds, punched into the existing rooms. Each sits on a
// wall stretch whose approach lane is clear of props.
ROOMS.beach.doors.push({ x: 0, y: 7, to: 'sea', label: '⛵ Open Sea' });
carve(ROOMS.beach.tiles, 0, 7);
ROOMS.kitchen.doors.push({ x: 13, y: 7, to: 'dunes', label: '🏜 Dunes' });
carve(ROOMS.kitchen.tiles, 13, 7);
ROOMS.garden.doors.push({ x: 7, y: 0, to: 'camp', label: '🔥 Campfire' });
carve(ROOMS.garden.tiles, 7, 0);
ROOMS.lounge.doors.push({ x: 4, y: 13, to: 'space', label: '🌌 Space Deck' });
carve(ROOMS.lounge.tiles, 4, 13);
ROOMS.office.doors.push({ x: 13, y: 21, to: 'terminal', label: '🖥 Terminal' });
carve(ROOMS.office.tiles, 13, 21);

// Quiet rooms (the work area + garden): no live voice channel there — voice is
// async instead, via the floating voice-note wall (docs/Room0-Voice.md).
ROOMS.office.quiet = true;
ROOMS.garden.quiet = true;
ROOMS.terminal.quiet = true;

// Where the player appears when arriving in a room (one tile inside its door so
// they don't immediately re-trigger it).
export function arrivalSpawn(room, fromRoomId) {
  const door = (room.doors || []).find((d) => d.to === fromRoomId);
  if (!door) return room.spawn;
  // Step one tile toward room centre.
  const cx = room.w / 2;
  const cy = room.h / 2;
  const gx = door.x + Math.sign(cx - door.x);
  const gy = door.y + Math.sign(cy - door.y);
  return { gx, gy };
}
