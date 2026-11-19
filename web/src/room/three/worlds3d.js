// room0 3D — outdoor/procedural worlds. Each world is a room (engine.js,
// kind:'world') whose scene is generated here instead of the tile builder in
// world3d.js: seeded terrain, its own sky dome, fog and lights. Determinism is
// the multiplayer contract — everything is seeded from the channel (worldSeed)
// so every member sees the same dunes, pines and panels. The world's ground
// truth for movement is heightAt(x,z): the same function displaces the mesh
// and places avatars, so wheels/feet and pixels always agree.
//
// A world scene returns the same shape game3d expects from buildRoomScene,
// plus { custom:true, heightAt, update(t,dt), fog } — game3d skips the
// daylight/tile-theme pipeline for custom worlds and calls update() per frame.
import * as THREE from 'three';

/* ── seeded utilities ─────────────────────────────────────────────── */
function mulberry32(a) {
  return function () {
    a |= 0; a = a + 0x6D2B79F5 | 0;
    let t = Math.imul(a ^ a >>> 15, 1 | a);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  };
}
function strHash(s) {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); }
  return h >>> 0;
}
function makeNoise(rand) {
  const p = new Uint8Array(512);
  const src = [...Array(256).keys()];
  for (let i = 255; i > 0; i--) { const j = Math.floor(rand() * (i + 1)); [src[i], src[j]] = [src[j], src[i]]; }
  for (let i = 0; i < 512; i++) p[i] = src[i & 255];
  const fade = (t) => t * t * (3 - 2 * t);
  function n2(x, y) {
    const xi = Math.floor(x), yi = Math.floor(y), xf = x - xi, yf = y - yi;
    const h = (a, b) => p[(p[(a & 255)] + (b & 255)) & 255] / 255;
    const a = h(xi, yi), b = h(xi + 1, yi), c = h(xi, yi + 1), d = h(xi + 1, yi + 1);
    const u = fade(xf), v = fade(yf);
    return a + (b - a) * u + (c - a) * v + (a - b - c + d) * u * v;
  }
  function fbm(x, y, oct = 4) {
    let s = 0, amp = 0.5, f = 1;
    for (let i = 0; i < oct; i++) { s += amp * n2(x * f, y * f); amp *= 0.5; f *= 2.03; }
    return s / (1 - Math.pow(0.5, oct));
  }
  return { n2, fbm };
}
const ridge = (n) => Math.pow(1 - Math.abs(2 * n - 1), 1.6);

/* ── shared sky dome (radius must stay inside the world camera far) ── */
function makeSky(cfg) {
  const geo = new THREE.SphereGeometry(400, 32, 20);
  const mat = new THREE.ShaderMaterial({
    side: THREE.BackSide, depthWrite: false, fog: false,
    uniforms: {
      uZen: { value: new THREE.Color(cfg.zenith) }, uHor: { value: new THREE.Color(cfg.horizon) },
      uSunDir: { value: cfg.sunDir.clone() }, uSunCol: { value: new THREE.Color(cfg.sunTint) },
      uSunSize: { value: cfg.sunSize ?? 900 }, uStars: { value: cfg.stars ? 1 : 0 },
      uStarTh: { value: cfg.starDensity ?? 0.90 },
    },
    vertexShader: `varying vec3 vDir; void main(){ vDir=normalize(position);
      gl_Position=projectionMatrix*modelViewMatrix*vec4(position,1.0); }`,
    fragmentShader: `
      varying vec3 vDir; uniform vec3 uZen,uHor,uSunDir,uSunCol; uniform float uSunSize,uStars,uStarTh;
      float hash(vec2 c){ return fract(sin(dot(c,vec2(127.1,311.7)))*43758.5453); }
      void main(){
        vec3 d=normalize(vDir);
        float t=pow(clamp(d.y,0.0,1.0),0.55);
        vec3 col=mix(uHor,uZen,t);
        float s=max(dot(d,normalize(uSunDir)),0.0);
        col+=uSunCol*pow(s,uSunSize)*2.2;
        col+=uSunCol*pow(s,10.0)*0.28;
        if(uStars>0.5&&d.y>0.02){
          vec2 sc=vec2(atan(d.z,d.x)*9.5,d.y*34.0);
          vec2 cell=floor(sc); vec2 f=fract(sc)-0.5;
          float h=hash(cell);
          if(h>uStarTh){ float st=smoothstep(0.10,0.0,length(f-0.3*(vec2(hash(cell+7.0),hash(cell+13.0))-0.5)));
            col+=vec3(0.9,0.95,1.0)*st*(h-uStarTh)*(1.6/(1.0-uStarTh))*d.y; }
        }
        gl_FragColor=vec4(col,1.0);
      }`,
  });
  return new THREE.Mesh(geo, mat);
}
function sunDirFrom(az, elev) {
  const a = az * Math.PI / 180, e = elev * Math.PI / 180;
  return new THREE.Vector3(Math.cos(e) * Math.cos(a), Math.sin(e), Math.cos(e) * Math.sin(a));
}

// Manual distance haze for the custom shaders (scene.fog covers the standard
// materials; these two must agree, so both come from the world's fog values).
const HAZE = `
  uniform vec3 uFogCol; uniform float uFogN,uFogF;
  vec3 haze(vec3 col,vec3 wp){ float d=length(cameraPosition-wp);
    return mix(col,uFogCol,smoothstep(uFogN,uFogF,d)); }`;

// Return portal: glowing ring + light + a floating name sign (the tile rooms
// get theirs from buildRoomScene; worlds build the same look here).
function makePortal(door, trim, cx, cz) {
  const g = new THREE.Group();
  g.position.set(door.x, 0, door.y);
  const disc = new THREE.Mesh(
    new THREE.CircleGeometry(0.42, 24),
    new THREE.MeshBasicMaterial({ color: trim, transparent: true, opacity: 0.35 }),
  );
  disc.rotation.x = -Math.PI / 2;
  disc.position.y = 0.02;
  g.add(disc);
  const ring = new THREE.Mesh(
    new THREE.TorusGeometry(0.5, 0.04, 8, 32),
    new THREE.MeshStandardMaterial({ color: trim, emissive: trim, emissiveIntensity: 1.6, roughness: 0.3 }),
  );
  ring.rotation.x = -Math.PI / 2;
  ring.position.y = 0.06;
  g.add(ring);
  const light = new THREE.PointLight(trim, 6, 5, 1.8);
  light.position.y = 1.0;
  g.add(light);
  const c = document.createElement('canvas');
  c.width = 256; c.height = 64;
  const ctx = c.getContext('2d');
  ctx.fillStyle = 'rgba(8, 14, 28, 0.94)';
  ctx.fillRect(0, 0, 256, 64);
  ctx.strokeStyle = `#${new THREE.Color(trim).getHexString()}`;
  ctx.lineWidth = 5;
  ctx.strokeRect(3, 3, 250, 58);
  ctx.fillStyle = '#e8f2ff';
  ctx.font = '600 30px system-ui, sans-serif';
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillText(door.label, 128, 35);
  const tex = new THREE.CanvasTexture(c);
  tex.colorSpace = THREE.SRGBColorSpace;
  const sign = new THREE.Mesh(
    new THREE.PlaneGeometry(1.5, 0.38),
    new THREE.MeshStandardMaterial({ map: tex, emissive: 0xffffff, emissiveMap: tex, emissiveIntensity: 0.6, roughness: 0.6 }),
  );
  sign.position.y = 1.95;
  sign.rotation.y = Math.atan2(cx - door.x, cz - door.y) + Math.PI; // face the world centre
  g.add(sign);
  return { group: g, ring };
}

// Mark the integer cells covered by a solid obstacle so isWalkable's world
// branch (engine.js) blocks them. Movement rounds to ints, so cell-level
// blocking is the honest resolution here.
function block(room, x, z, r = 0.5) {
  if (!room._blocked) room._blocked = new Set();
  const keys = [];
  for (let gx = Math.round(x - r); gx <= Math.round(x + r); gx++) {
    for (let gz = Math.round(z - r); gz <= Math.round(z + r); gz++) {
      const k = `${gx},${gz}`;
      room._blocked.add(k);
      keys.push(k);
    }
  }
  return keys;
}

// Cheap tiling speckle texture — flat colour reads as plastic up close;
// this reads as organic ground. Doubles as map and bumpMap.
function speckleTex(rand, base, tones) {
  const c = document.createElement('canvas');
  c.width = c.height = 256;
  const ctx = c.getContext('2d');
  ctx.fillStyle = base;
  ctx.fillRect(0, 0, 256, 256);
  for (let i = 0; i < 7000; i++) {
    ctx.fillStyle = tones[Math.floor(rand() * tones.length)];
    ctx.globalAlpha = 0.05 + rand() * 0.14;
    const sz = 1 + rand() * 3;
    ctx.fillRect(rand() * 256, rand() * 256, sz, sz);
  }
  const tex = new THREE.CanvasTexture(c);
  tex.wrapS = tex.wrapT = THREE.RepeatWrapping;
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}

/* ══ THE SEA — golden hour, a jetty, a moored rowboat, a lighthouse ══ */
function buildSea(room, rand) {
  const cx = room.w / 2, cz = room.h / 2;
  const group = new THREE.Group();
  const fog = new THREE.Fog(new THREE.Color('#e8a76f'), 60, 230);
  const sunDir = sunDirFrom(200, 9);
  group.add(makeSky({ zenith: '#35619b', horizon: '#ffb36b', sunDir, sunTint: '#ffd9a0' }));

  const waves = [];
  let maxA = 0;
  for (let i = 0; i < 6; i++) {
    const ang = 4.2 + (rand() - 0.5) * 1.6;
    const L = 5 + rand() * 26, A = (0.065 + rand() * 0.12) * (1 - i * 0.09), k = 2 * Math.PI / L;
    waves.push({ dx: Math.cos(ang), dz: Math.sin(ang), A, k, w: Math.sqrt(9.8 * k) * 0.8 });
    maxA += A;
  }
  const wU = waves.map((w) => new THREE.Vector4(w.dx, w.dz, w.A, w.k));
  const sU = waves.map((w) => w.w);
  const geo = new THREE.PlaneGeometry(300, 300, 220, 220);
  geo.rotateX(-Math.PI / 2);
  geo.translate(cx, 0, cz);
  const waterMat = new THREE.ShaderMaterial({
    uniforms: {
      uTime: { value: 0 }, uWaves: { value: wU }, uSpeed: { value: sU },
      uDeep: { value: new THREE.Color('#0b3350') }, uShallow: { value: new THREE.Color('#2e8ba8') },
      uSky: { value: new THREE.Color('#ffc78a') }, uSunDir: { value: sunDir },
      uSunCol: { value: new THREE.Color('#ffd9a0') }, uMaxA: { value: maxA },
      uFogCol: { value: fog.color }, uFogN: { value: fog.near }, uFogF: { value: fog.far },
    },
    vertexShader: `
      uniform float uTime; uniform vec4 uWaves[6]; uniform float uSpeed[6];
      varying vec3 vWorld; varying float vH;
      void main(){
        vec3 p=position; vec2 xz=p.xz; float h=0.0; vec2 disp=vec2(0.0);
        for(int i=0;i<6;i++){ vec2 d=normalize(uWaves[i].xy); float A=uWaves[i].z,k=uWaves[i].w;
          float th=k*dot(d,xz)-uSpeed[i]*uTime; h+=A*sin(th); disp+=0.55*A*d*cos(th); }
        p.x+=disp.x; p.z+=disp.y; p.y+=h; vH=h;
        vec4 w=modelMatrix*vec4(p,1.0); vWorld=w.xyz;
        gl_Position=projectionMatrix*viewMatrix*w; }`,
    fragmentShader: `
      uniform float uTime; uniform vec4 uWaves[6]; uniform float uSpeed[6];
      uniform vec3 uDeep,uShallow,uSky,uSunDir,uSunCol; uniform float uMaxA;
      varying vec3 vWorld; varying float vH; ${HAZE}
      float hash(vec2 c){ return fract(sin(dot(c,vec2(127.1,311.7)))*43758.5453); }
      float vnoise(vec2 p){ vec2 i=floor(p),f=fract(p); f=f*f*(3.0-2.0*f);
        return mix(mix(hash(i),hash(i+vec2(1,0)),f.x),mix(hash(i+vec2(0,1)),hash(i+vec2(1,1)),f.x),f.y); }
      vec2 grad(vec2 p){ float e=0.4; float h=vnoise(p);
        return vec2(vnoise(p+vec2(e,0.0))-h,vnoise(p+vec2(0.0,e))-h)/e; }
      void main(){
        vec2 xz=vWorld.xz; vec3 n=vec3(0.0,1.0,0.0);
        for(int i=0;i<6;i++){ vec2 d=normalize(uWaves[i].xy); float A=uWaves[i].z,k=uWaves[i].w;
          float th=k*dot(d,xz)-uSpeed[i]*uTime;
          n.x-=d.x*k*A*cos(th); n.z-=d.y*k*A*cos(th); n.y-=0.55*k*A*sin(th); }
        // Fine ripples: three scrolling noise octaves bend the normal below
        // the vertex waves, faded with distance so the horizon doesn't shimmer.
        float dist=length(cameraPosition-vWorld);
        float dfade=smoothstep(120.0,18.0,dist);
        vec2 g=grad(xz*1.6+vec2(uTime*0.55,uTime*0.34))*0.5
              +grad(xz*4.8-vec2(uTime*0.9,uTime*0.5))*0.25
              +grad(xz*11.0+vec2(uTime*1.3,-uTime*0.8))*0.12;
        n.xz-=g*0.42*dfade;
        n=normalize(n);
        vec3 view=normalize(cameraPosition-vWorld);
        float fres=pow(1.0-max(dot(view,n),0.0),3.0);
        vec3 col=mix(uDeep,uShallow,clamp(vH/uMaxA*0.5+0.5,0.0,1.0));
        // Crests between the eye and the sun glow from inside.
        vec3 sd=normalize(uSunDir);
        float sss=pow(max(dot(view,-sd),0.0),3.0)*smoothstep(0.0,uMaxA,vH);
        col+=uShallow*sss*0.55;
        col=mix(col,uSky,fres*0.85);
        col+=uSunCol*pow(max(dot(reflect(-sd,n),view),0.0),380.0)*3.0;
        col+=uSunCol*pow(max(dot(reflect(-sd,n),view),0.0),24.0)*0.14;
        // Crest foam broken up by noise, plus faint lace in between.
        float foamN=vnoise(xz*3.1+vec2(uTime*0.22,uTime*0.15))*0.6+vnoise(xz*7.3-vec2(uTime*0.3))*0.4;
        float foam=smoothstep(uMaxA*0.55,uMaxA*0.95,vH)*smoothstep(0.42,0.75,foamN);
        foam+=smoothstep(0.80,0.93,foamN)*0.10*dfade;
        col=mix(col,vec3(0.94,0.97,0.98),clamp(foam,0.0,0.8));
        gl_FragColor=vec4(haze(col,vWorld),1.0); }`,
  });
  group.add(new THREE.Mesh(geo, waterMat));

  const sun = new THREE.DirectionalLight('#ffd9a0', 2.0);
  sun.position.set(cx + sunDir.x * 80, sunDir.y * 80, cz + sunDir.z * 80);
  group.add(sun, new THREE.HemisphereLight('#9fb8d8', '#0b3350', 0.9));

  // Jetty: planks from the door out over the water. The player wades in the
  // shallows (heightAt 0, ankle-deep) — the jetty is dressing, not a platform.
  const plankMat = new THREE.MeshStandardMaterial({ color: '#8a6038', roughness: 0.9 });
  for (let i = 0; i < 8; i++) {
    const plank = new THREE.Mesh(new THREE.BoxGeometry(2.2, 0.12, 0.9), plankMat);
    plank.position.set(cx, 0.28, cz + 23 - i * 1.0);
    group.add(plank);
    if (i % 2 === 0) {
      for (const s of [-1, 1]) {
        const pole = new THREE.Mesh(new THREE.CylinderGeometry(0.07, 0.09, 1.2, 8), plankMat);
        pole.position.set(cx + s * 1.0, -0.1, cz + 23 - i * 1.0);
        group.add(pole);
      }
    }
  }

  // The rowboat, moored beside the jetty — three sittable benches.
  const boat = new THREE.Group();
  boat.position.set(cx + 3.2, 0, cz + 19);
  boat.rotation.y = 0.4;
  const wood = new THREE.MeshStandardMaterial({ color: '#6e4a2e', roughness: 0.8 });
  const wood2 = new THREE.MeshStandardMaterial({ color: '#8a6038', roughness: 0.85 });
  const bottom = new THREE.Mesh(new THREE.BoxGeometry(1.3, 0.12, 3.6), wood2);
  bottom.position.y = 0.06;
  boat.add(bottom);
  for (const s of [-1, 1]) {
    const side = new THREE.Mesh(new THREE.BoxGeometry(0.12, 0.72, 3.6), wood);
    side.position.set(s * 0.72, 0.42, 0);
    side.rotation.z = -s * 0.22;
    boat.add(side);
  }
  for (const e of [-1, 1]) {
    const end = new THREE.Mesh(new THREE.BoxGeometry(1.42, 0.72, 0.14), wood);
    end.position.set(0, 0.42, e * 1.86);
    end.rotation.x = e * 0.5;
    boat.add(end);
  }
  const seats = [];
  for (let i = 0; i < 3; i++) {
    const b = new THREE.Mesh(new THREE.BoxGeometry(1.4, 0.09, 0.4), wood2);
    b.position.set(0, 0.55, -1.1 + i * 1.1);
    boat.add(b);
    seats.push({
      x: 0, z: 0, y: 0.62, ry: 0, pose: 'sit', tuck: null, key: `boat:${i}`,
      local: -1.1 + i * 1.1, label: 'board the boat',
    });
  }
  for (const s of [-1, 1]) {
    const oar = new THREE.Mesh(new THREE.CylinderGeometry(0.035, 0.05, 2.6), wood);
    oar.position.set(s * 1.15, 0.62, 0);
    oar.rotation.z = s * 1.2;
    boat.add(oar);
  }
  group.add(boat);
  // The boat is rideable: game3d rows it while someone sits in it and syncs
  // it over the object relay. sync() keeps the benches and the blocked cells
  // tracking the hull.
  const vehicle = {
    id: 'sea:boat', room: room.id, obj: boat, seats, speed: 3.4, turn: 1.4,
    cells: [],
    sync() {
      for (const s of seats) {
        s.x = boat.position.x + Math.sin(boat.rotation.y) * s.local;
        s.z = boat.position.z + Math.cos(boat.rotation.y) * s.local;
        s.ry = boat.rotation.y + Math.PI / 2;
      }
      for (const c of this.cells) room._blocked.delete(c);
      this.cells = block(room, boat.position.x, boat.position.z, 1.2);
    },
  };
  vehicle.sync();

  // Lighthouse islet on the horizon, its lamp pulsing.
  const isle = new THREE.Group();
  isle.position.set(cx - 38, 0, cz - 48);
  const rock = new THREE.Mesh(new THREE.ConeGeometry(9, 5, 7), new THREE.MeshStandardMaterial({ color: '#3a3f4a', roughness: 1, flatShading: true }));
  rock.position.y = 1.4;
  isle.add(rock);
  const tower = new THREE.Mesh(new THREE.CylinderGeometry(1.1, 1.7, 10, 10), new THREE.MeshStandardMaterial({ color: '#c9c2b4', roughness: 0.7 }));
  tower.position.y = 8.2;
  isle.add(tower);
  const lampMat = new THREE.MeshStandardMaterial({ color: '#1c2430', emissive: '#ffd98a', emissiveIntensity: 1.2 });
  const lamp = new THREE.Mesh(new THREE.CylinderGeometry(0.9, 0.9, 1.5, 10), lampMat);
  lamp.position.y = 14;
  isle.add(lamp);
  const cap = new THREE.Mesh(new THREE.ConeGeometry(1.3, 1.2, 10), new THREE.MeshStandardMaterial({ color: '#7a2f2f', roughness: 0.6 }));
  cap.position.y = 15.3;
  isle.add(cap);
  group.add(isle);

  const hAt = (x, z, t) => {
    let h = 0;
    for (const w of waves) h += w.A * Math.sin(w.k * (w.dx * x + w.dz * z) - w.w * t);
    return h;
  };
  return {
    custom: true, group, seats, fog, animated: [], heightAt: null, vehicle,
    update(t) {
      waterMat.uniforms.uTime.value = t;
      // The moored boat rides the same waves the shader draws.
      boat.position.y = hAt(boat.position.x, boat.position.z, t) * 0.6 + 0.06;
      boat.rotation.x = hAt(boat.position.x, boat.position.z + 0.8, t) * 0.2;
      lampMat.emissiveIntensity = 1.0 + Math.max(0, Math.sin(t * 1.4)) * 0.8;
    },
  };
}

/* ══ THE DUNES — journey gold, glitter sand, an ambient buggy ══ */
function buildDunes(room, rand) {
  const cx = room.w / 2, cz = room.h / 2;
  const group = new THREE.Group();
  const fog = new THREE.Fog(new THREE.Color('#eda96e'), 60, 240);
  const sunDir = sunDirFrom(205, 14);
  group.add(makeSky({ zenith: '#6f95c9', horizon: '#ffc887', sunDir, sunTint: '#ffcf8f' }));

  const { fbm } = makeNoise(rand);
  const windA = rand() * Math.PI * 2, ca = Math.cos(windA), sa = Math.sin(windA);
  // Aeolian dunes: ridged noise stretched along the wind axis, over a rolling
  // base, with fine wind ripples. Flattened near the door so arrival is level.
  const H = (wx, wz) => {
    const x = wx - cx, z = wz - cz;
    const u = x * ca + z * sa, w = -x * sa + z * ca;
    let h = ridge(fbm(u * 0.010 + 3.7, w * 0.044)) * 8.5;
    h += ridge(fbm(u * 0.030 + 9.1, w * 0.095)) * 1.9;
    h += fbm(x * 0.012, z * 0.012, 3) * 2.2;
    h += Math.sin(u * 3.2 + fbm(u * 0.15, w * 0.15) * 7.0) * 0.05;
    // The walkable bowl stays gentle rolling ground; full drama builds toward
    // and past the horizon — on foot you wander dunes, you don't scale cliffs.
    const dc = Math.hypot(x, z);
    h *= 0.3 + 0.7 * Math.min(1, Math.max(0, (dc - 16) / 34));
    const doorD = Math.hypot(wx - room.doors[0].x, wz - room.doors[0].y);
    return h * Math.min(1, Math.max(0.05, (doorD - 2) / 8));
  };
  const SZ = 260, SEG = 240;
  const geo = new THREE.PlaneGeometry(SZ, SZ, SEG, SEG);
  geo.rotateX(-Math.PI / 2);
  geo.translate(cx, 0, cz);
  const pos = geo.attributes.position;
  for (let i = 0; i < pos.count; i++) pos.setY(i, H(pos.getX(i), pos.getZ(i)));
  geo.computeVertexNormals();
  const sandMat = new THREE.ShaderMaterial({
    uniforms: {
      uSunDir: { value: sunDir }, uSun: { value: new THREE.Color('#e8964a') },
      uWind: { value: new THREE.Vector2(ca, sa) },
      uShadow: { value: new THREE.Color('#68519e') }, uRim: { value: new THREE.Color('#ffe3ae') },
      uFogCol: { value: fog.color }, uFogN: { value: fog.near }, uFogF: { value: fog.far },
    },
    vertexShader: `
      varying vec3 vWorld,vNorm;
      void main(){ vec4 w=modelMatrix*vec4(position,1.0); vWorld=w.xyz;
        vNorm=normalize(mat3(modelMatrix)*normal);
        gl_Position=projectionMatrix*viewMatrix*w; }`,
    fragmentShader: `
      varying vec3 vWorld,vNorm; uniform vec3 uSunDir,uSun,uShadow,uRim; uniform vec2 uWind; ${HAZE}
      float hash(vec2 c){ return fract(sin(dot(c,vec2(127.1,311.7)))*43758.5453); }
      float vnoise(vec2 p){ vec2 i=floor(p),f=fract(p); f=f*f*(3.0-2.0*f);
        return mix(mix(hash(i),hash(i+vec2(1,0)),f.x),mix(hash(i+vec2(0,1)),hash(i+vec2(1,1)),f.x),f.y); }
      void main(){
        vec3 n=normalize(vNorm),sd=normalize(uSunDir);
        vec3 view=normalize(cameraPosition-vWorld);
        float dist=length(cameraPosition-vWorld);
        float dfade=smoothstep(110.0,14.0,dist);
        // Wind ripples: two frequencies of crests across the wind axis,
        // wandering with noise, fading out before they can moire at range.
        float u=dot(vWorld.xz,uWind);
        float rip=sin(u*9.0+vnoise(vWorld.xz*0.7)*9.0)*0.5
                 +sin(u*23.0+vnoise(vWorld.xz*1.9)*6.0)*0.5;
        n=normalize(n+vec3(uWind.x,0.0,uWind.y)*rip*0.10*dfade);
        float ndl=dot(n,sd)*0.5+0.5;
        vec3 col=mix(uShadow,uSun,pow(ndl,1.5));
        // Grain up close, patchy warm/cool tone at every distance.
        col*=1.0+(vnoise(vWorld.xz*34.0)-0.5)*0.30*dfade;
        col*=1.0+(vnoise(vWorld.xz*3.1)-0.5)*0.12;
        col=mix(col,col*vec3(1.06,0.98,0.90),vnoise(vWorld.xz*0.045));
        col+=uRim*pow(1.0-max(dot(n,view),0.0),4.0)*0.35;
        float h=hash(floor(vWorld.xz*90.0));
        if(h>0.94){ float sp=max(dot(reflect(-sd,n),view),0.0);
          col+=vec3(1.0)*pow(sp,26.0)*(h-0.94)*24.0*smoothstep(150.0,25.0,dist); }
        gl_FragColor=vec4(haze(col,vWorld),1.0); }`,
  });
  group.add(new THREE.Mesh(geo, sandMat));

  const sun = new THREE.DirectionalLight('#ffcf8f', 1.6);
  sun.position.set(cx + sunDir.x * 80, sunDir.y * 80, cz + sunDir.z * 80);
  group.add(sun, new THREE.HemisphereLight('#ffc887', '#68519e', 0.75));

  // An ambient buggy carving a seeded circuit through the far dunes.
  const buggy = new THREE.Group();
  const bodyMat = new THREE.MeshStandardMaterial({ color: '#c8452e', roughness: 0.5, metalness: 0.2 });
  const dark = new THREE.MeshStandardMaterial({ color: '#1c2026', roughness: 0.9 });
  const body = new THREE.Mesh(new THREE.BoxGeometry(1.5, 0.5, 2.6), bodyMat);
  body.position.y = 0.72;
  buggy.add(body);
  for (const [a, b] of [[-0.6, -0.9], [0.6, -0.9], [-0.6, 0.9], [0.6, 0.9]]) {
    const wh = new THREE.Mesh(new THREE.CylinderGeometry(0.42, 0.42, 0.34, 14), dark);
    wh.rotation.z = Math.PI / 2;
    wh.position.set(a, 0.42, b);
    buggy.add(wh);
  }
  const bar = new THREE.Mesh(new THREE.CylinderGeometry(0.05, 0.05, 1.15), dark);
  bar.rotation.z = Math.PI / 2;
  bar.position.set(0, 1.44, -0.42);
  buggy.add(bar);
  group.add(buggy);
  const bx = cx + (rand() - 0.5) * 20, bz = cz + (rand() - 0.5) * 20, br = 24 + rand() * 8;

  return {
    custom: true, group, seats: [], fog, animated: [], heightAt: H,
    update(t) {
      const a = t * 0.14, x = bx + Math.cos(a) * br, z = bz + Math.sin(a) * br;
      const e = 0.7;
      const y = Math.max(H(x - e, z - e), H(x + e, z - e), H(x - e, z + e), H(x + e, z + e));
      buggy.position.set(x, y - 0.04, z);
      const fx = bx + Math.cos(a + 0.06) * br, fz = bz + Math.sin(a + 0.06) * br;
      buggy.lookAt(fx, H(fx, fz) - 0.04, fz);
    },
  };
}

/* ══ THE CAMPFIRE — midnight clearing, pines, fire, seat logs ══ */
function buildCamp(room, rand) {
  const cx = room.w / 2, cz = room.h / 2;
  const group = new THREE.Group();
  const fog = new THREE.Fog(new THREE.Color('#0a1424'), 20, 110);
  const sunDir = sunDirFrom(210, 30);
  group.add(makeSky({ zenith: '#050b1c', horizon: '#11203a', sunDir, sunTint: '#bcd4f5', sunSize: 3200, stars: true }));

  const { fbm } = makeNoise(rand);
  const H = (wx, wz) => {
    const x = wx - cx, z = wz - cz;
    return fbm(x * 0.05 + 5, z * 0.05) * 1.4 * Math.min(1, Math.max(0, (Math.hypot(x, z) - 5) / 10));
  };
  const geo = new THREE.PlaneGeometry(180, 180, 100, 100);
  geo.rotateX(-Math.PI / 2);
  geo.translate(cx, 0, cz);
  const pos = geo.attributes.position;
  for (let i = 0; i < pos.count; i++) pos.setY(i, H(pos.getX(i), pos.getZ(i)));
  geo.computeVertexNormals();
  const grassTex = speckleTex(rand, '#182212', ['#0f180c', '#213018', '#2a3a1a', '#111a0e']);
  grassTex.repeat.set(60, 60);
  group.add(new THREE.Mesh(geo, new THREE.MeshStandardMaterial({ map: grassTex, bumpMap: grassTex, bumpScale: 0.35, roughness: 1 })));
  const dirtTex = speckleTex(rand, '#241c12', ['#1a140c', '#30261a', '#3a2e1e']);
  dirtTex.repeat.set(6, 6);
  const dirt = new THREE.Mesh(new THREE.CircleGeometry(4.2, 28), new THREE.MeshStandardMaterial({ map: dirtTex, bumpMap: dirtTex, bumpScale: 0.3, roughness: 1 }));
  dirt.rotation.x = -Math.PI / 2;
  dirt.position.set(cx, 0.02, cz);
  group.add(dirt);

  group.add(new THREE.HemisphereLight('#11203a', '#182212', 0.62));
  const moon = new THREE.DirectionalLight('#bcd4f5', 0.38);
  moon.position.set(cx + sunDir.x * 60, sunDir.y * 60, cz + sunDir.z * 60);
  group.add(moon);

  // Lantern posts marking the trail from the garden door to the fire.
  const postMat = new THREE.MeshStandardMaterial({ color: '#2c2018', roughness: 1 });
  const lampGlow = new THREE.MeshStandardMaterial({ color: '#ffe2b0', emissive: '#ffb35c', emissiveIntensity: 1.8 });
  for (let i = 0; i < 4; i++) {
    const lx = cx + (i % 2 ? 1.6 : -1.6), lz = cz + 20 - i * 5;
    const y = H(lx, lz);
    const pole = new THREE.Mesh(new THREE.CylinderGeometry(0.05, 0.07, 1.5, 6), postMat);
    pole.position.set(lx, y + 0.75, lz);
    group.add(pole);
    const orb = new THREE.Mesh(new THREE.SphereGeometry(0.11, 8, 6), lampGlow);
    orb.position.set(lx, y + 1.55, lz);
    group.add(orb);
    const l = new THREE.PointLight('#ffb35c', 4, 9, 1.7);
    l.position.set(lx, y + 1.6, lz);
    group.add(l);
  }

  const trunkMat = new THREE.MeshStandardMaterial({ color: '#2c2018', roughness: 1 });
  const pineMat = new THREE.MeshStandardMaterial({ color: '#14251b', roughness: 1, flatShading: true });
  for (let i = 0; i < 42; i++) {
    const a = rand() * Math.PI * 2, r = 11 + rand() * 32;
    const x = cx + Math.cos(a) * r, z = cz + Math.sin(a) * r, y = H(x, z);
    const s = 0.8 + rand() * 1.3;
    const tree = new THREE.Group();
    tree.position.set(x, y, z);
    tree.scale.setScalar(s);
    const trunk = new THREE.Mesh(new THREE.CylinderGeometry(0.14, 0.2, 1.4, 6), trunkMat);
    trunk.position.y = 0.7;
    tree.add(trunk);
    for (let c = 0; c < 3; c++) {
      const cone = new THREE.Mesh(new THREE.ConeGeometry(1.7 - c * 0.45, 1.9, 7), pineMat);
      cone.position.y = 1.9 + c * 1.15;
      tree.add(cone);
    }
    group.add(tree);
    block(room, x, z, 0.4);
  }

  // Fire pit: stones, crossed logs, additive flame cones, sparks, warm light.
  const stoneMat = new THREE.MeshStandardMaterial({ color: '#4a4a52', roughness: 1, flatShading: true });
  for (let i = 0; i < 9; i++) {
    const a = i / 9 * Math.PI * 2;
    const st = new THREE.Mesh(new THREE.IcosahedronGeometry(0.22 + rand() * 0.1, 0), stoneMat);
    st.position.set(cx + Math.cos(a) * 0.95, 0.12, cz + Math.sin(a) * 0.95);
    group.add(st);
  }
  block(room, cx, cz, 0.9);
  const logMat = new THREE.MeshStandardMaterial({ color: '#4a3322', roughness: 1 });
  for (let i = 0; i < 4; i++) {
    const a = i / 4 * Math.PI * 2 + 0.4;
    const log = new THREE.Mesh(new THREE.CylinderGeometry(0.13, 0.13, 1.5, 7), logMat);
    log.rotation.z = Math.PI / 2;
    log.rotation.y = a;
    log.position.set(cx + Math.cos(a) * 0.3, 0.28 + i * 0.07, cz + Math.sin(a) * 0.3);
    group.add(log);
  }
  const seats = [];
  for (let i = 0; i < 5; i++) {
    const a = i / 5 * Math.PI * 2 + 0.9;
    const seat = new THREE.Mesh(new THREE.CylinderGeometry(0.34, 0.34, 2.4, 9), logMat);
    seat.rotation.z = Math.PI / 2;
    seat.rotation.y = a + Math.PI / 2;
    const sx = cx + Math.cos(a) * 3.1, sz = cz + Math.sin(a) * 3.1;
    seat.position.set(sx, 0.34, sz);
    group.add(seat);
    // Face the fire.
    seats.push({ x: sx, z: sz, y: 0.62, ry: Math.atan2(cx - sx, cz - sz), pose: 'sit', tuck: null, key: `log:${i}` });
  }
  const flames = [];
  for (let i = 0; i < 3; i++) {
    const fm = new THREE.MeshBasicMaterial({
      color: i === 0 ? '#ffc46a' : '#ff9a4a', transparent: true,
      opacity: 0.7 - i * 0.18, blending: THREE.AdditiveBlending, depthWrite: false,
    });
    const fl = new THREE.Mesh(new THREE.ConeGeometry(0.5 - i * 0.12, 1.5 - i * 0.3, 7), fm);
    fl.position.set(cx, 0.9 + i * 0.35, cz);
    group.add(fl);
    flames.push(fl);
  }
  const fireLight = new THREE.PointLight('#ff9a4a', 3, 36, 1.5);
  fireLight.position.set(cx, 1.6, cz);
  group.add(fireLight);
  const NS = 60;
  const sp = new Float32Array(NS * 3);
  const sv = [];
  for (let i = 0; i < NS; i++) {
    sp[i * 3] = cx + (rand() - 0.5) * 0.5;
    sp[i * 3 + 1] = 1 + rand() * 3;
    sp[i * 3 + 2] = cz + (rand() - 0.5) * 0.5;
    sv.push(0.6 + rand() * 1.4);
  }
  const sparkGeo = new THREE.BufferGeometry();
  sparkGeo.setAttribute('position', new THREE.BufferAttribute(sp, 3));
  const sparks = new THREE.Points(sparkGeo, new THREE.PointsMaterial({
    color: '#ff9a4a', size: 0.09, transparent: true, opacity: 0.9,
    blending: THREE.AdditiveBlending, depthWrite: false,
  }));
  group.add(sparks);

  return {
    custom: true, group, seats, fog, animated: [], heightAt: H,
    update(t, dt) {
      fireLight.intensity = 2.6 + Math.sin(t * 11) * 0.5 + Math.sin(t * 23 + 1) * 0.4 + Math.random() * 0.5;
      flames.forEach((f, i) => {
        f.scale.set(1 + Math.sin(t * 9 + i * 2) * 0.15, 1 + Math.sin(t * 13 + i * 3) * 0.22, 1 + Math.cos(t * 8 + i) * 0.15);
        f.rotation.y = t * (1.5 + i);
      });
      const p = sparkGeo.attributes.position;
      for (let i = 0; i < NS; i++) {
        let y = p.array[i * 3 + 1] + sv[i] * dt * 0.9;
        if (y > 4.2) { y = 0.9; p.array[i * 3] = cx + (Math.random() - 0.5) * 0.5; p.array[i * 3 + 2] = cz + (Math.random() - 0.5) * 0.5; }
        p.array[i * 3 + 1] = y;
      }
      p.needsUpdate = true;
    },
  };
}

/* ══ THE SPACE DECK — a trim-ringed platform over a blue marble ══ */
function buildSpace(room, rand) {
  const cx = room.w / 2, cz = room.h / 2;
  const group = new THREE.Group();
  group.add(makeSky({
    zenith: '#010208', horizon: '#050a16', sunDir: sunDirFrom(140, 30),
    sunTint: '#dfe8ff', sunSize: 6000, stars: true, starDensity: 0.75,
  }));

  for (let i = 0; i < 6; i++) {
    const c = document.createElement('canvas');
    c.width = c.height = 256;
    const ctx = c.getContext('2d');
    const g = ctx.createRadialGradient(128, 128, 10, 128, 128, 128);
    g.addColorStop(0, ['#1a3a6a', '#123050'][i % 2]);
    g.addColorStop(1, 'rgba(0,0,0,0)');
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, 256, 256);
    const spr = new THREE.Sprite(new THREE.SpriteMaterial({
      map: new THREE.CanvasTexture(c), transparent: true, opacity: 0.25,
      blending: THREE.AdditiveBlending, depthWrite: false,
    }));
    const a = rand() * Math.PI * 2, el = (rand() - 0.2) * 1.1, r = 300;
    spr.position.set(cx + Math.cos(a) * Math.cos(el) * r, Math.sin(el) * r, cz + Math.sin(a) * Math.cos(el) * r);
    spr.scale.setScalar(160 + rand() * 180);
    group.add(spr);
  }

  const planet = new THREE.Mesh(new THREE.SphereGeometry(100, 56, 36), new THREE.ShaderMaterial({
    uniforms: {
      uA: { value: new THREE.Color('#2e6fba') }, uB: { value: new THREE.Color('#7ac8e8') },
      uAtmo: { value: new THREE.Color('#6fc3ff') }, uSun: { value: sunDirFrom(140, 60) },
      uSeed: { value: rand() * 10 }, uC: { value: new THREE.Vector3(cx, -120, cz) },
    },
    vertexShader: `varying vec3 vN,vP;
      void main(){ vN=normalize(mat3(modelMatrix)*normal); vP=(modelMatrix*vec4(position,1.0)).xyz;
        gl_Position=projectionMatrix*viewMatrix*vec4(vP,1.0); }`,
    fragmentShader: `varying vec3 vN,vP; uniform vec3 uA,uB,uAtmo,uSun,uC; uniform float uSeed;
      void main(){
        vec3 n=normalize(vN);
        float lat=asin(clamp(normalize(vP-uC).y,-1.0,1.0));
        float band=0.5+0.5*sin(lat*14.0+uSeed+sin(lat*31.0+uSeed*3.0)*0.8);
        vec3 col=mix(uA,uB,band);
        float ndl=clamp(dot(n,normalize(uSun)),0.0,1.0);
        col*=0.12+0.88*ndl;
        vec3 view=normalize(cameraPosition-vP);
        col+=uAtmo*pow(1.0-max(dot(n,view),0.0),3.0)*0.9;
        gl_FragColor=vec4(col,1.0); }`,
  }));
  planet.position.set(cx, -120, cz);
  group.add(planet);

  const astMat = new THREE.MeshStandardMaterial({ color: '#5a5f6a', roughness: 1, flatShading: true });
  const asts = [];
  for (let i = 0; i < 8; i++) {
    const g2 = new THREE.IcosahedronGeometry(0.4 + rand() * 1.1, 0);
    const cp = g2.attributes.position;
    for (let k = 0; k < cp.count; k++) { const j = 0.7 + rand() * 0.6; cp.setXYZ(k, cp.getX(k) * j, cp.getY(k) * j, cp.getZ(k) * j); }
    g2.computeVertexNormals();
    const m2 = new THREE.Mesh(g2, astMat);
    const aa = rand() * Math.PI * 2, rr = 22 + rand() * 34;
    m2.position.set(cx + Math.cos(aa) * rr, (rand() - 0.3) * 26, cz + Math.sin(aa) * rr);
    asts.push({ m: m2, r1: rand() * 2, r2: rand() * 2 });
    group.add(m2);
  }

  const deck = new THREE.Mesh(new THREE.CylinderGeometry(9, 9.6, 0.6, 48),
    new THREE.MeshStandardMaterial({ color: '#26344e', roughness: 0.55, metalness: 0.35 }));
  deck.position.set(cx, -0.3, cz);
  group.add(deck);
  const trimMat = new THREE.MeshStandardMaterial({ color: '#39c6ff', emissive: '#39c6ff', emissiveIntensity: 1.6 });
  const trim = new THREE.Mesh(new THREE.TorusGeometry(9.15, 0.09, 10, 64), trimMat);
  trim.rotation.x = Math.PI / 2;
  trim.position.set(cx, 0.04, cz);
  group.add(trim);
  const conMat = new THREE.MeshStandardMaterial({ color: '#38465e', roughness: 0.45, metalness: 0.3 });
  for (let i = 0; i < 4; i++) {
    const a = i / 4 * Math.PI * 2 + 0.5;
    const con = new THREE.Group();
    con.position.set(cx + Math.cos(a) * 6.4, 0, cz + Math.sin(a) * 6.4);
    con.lookAt(cx, 0, cz);
    const base = new THREE.Mesh(new THREE.BoxGeometry(1.5, 1, 0.5), conMat);
    base.position.y = 0.5;
    con.add(base);
    const scr = new THREE.Mesh(new THREE.PlaneGeometry(1.2, 0.7),
      new THREE.MeshBasicMaterial({ color: '#39c6ff', transparent: true, opacity: 0.75 }));
    scr.position.set(0, 1.15, 0.06);
    scr.rotation.x = -0.25;
    con.add(scr);
    group.add(con);
    block(room, con.position.x, con.position.z, 0.7);
  }
  group.add(new THREE.HemisphereLight('#4a5c80', '#0a0e18', 0.9));
  const key = new THREE.DirectionalLight('#dfe8ff', 1.7);
  key.position.set(cx + 30, 40, cz + 20);
  group.add(key);
  const glow = new THREE.PointLight('#39c6ff', 1.4, 26, 1.8);
  glow.position.set(cx, 2.5, cz);
  group.add(glow);

  return {
    custom: true, group, seats: [], fog: null, animated: [], heightAt: null,
    update(t) {
      planet.rotation.y = t * 0.01;
      for (const a of asts) { a.m.rotation.x = t * 0.2 + a.r1; a.m.rotation.y = t * 0.15 + a.r2; }
      trimMat.emissiveIntensity = 1.4 + Math.sin(t * 2) * 0.35;
    },
  };
}

/* ══ THE TERMINAL — gliph's soul: phosphor grid, cursor monolith,
     floating chat-log panels ══ */
const TERM_LINES = [
  '* now talking in #ops', '<waseem> ship it', '<daemon> build green in 42s',
  '* gliph sets mode +E2E', '<nadia> /room', '<kv> the dunes are open',
  '* moss joined #ops', '<moss> who broke prod', '<daemon> not me. probably.',
  '<waseem> rebase and push', '* kv is now known as kv|afk', '<nadia> voice note on the wall',
  '<gliph> epoch rotated, 9 members', '<moss> lgtm', '* topic: text-first, forever',
];
function buildTerm(room, rand) {
  const cx = room.w / 2, cz = room.h / 2;
  const glowCol = '#39c6ff', bgCol = '#04070f';
  const group = new THREE.Group();
  const fog = new THREE.Fog(new THREE.Color(bgCol), 40, 170);
  group.add(makeSky({
    zenith: bgCol, horizon: '#081422', sunDir: new THREE.Vector3(0, -1, 0),
    sunTint: '#000000', stars: true, starDensity: 0.965,
  }));

  const gmat = new THREE.ShaderMaterial({
    uniforms: {
      uTime: { value: 0 }, uGlow: { value: new THREE.Color(glowCol) }, uBg: { value: new THREE.Color(bgCol) },
      uC: { value: new THREE.Vector2(cx, cz) },
      uFogCol: { value: fog.color }, uFogN: { value: fog.near }, uFogF: { value: fog.far },
    },
    vertexShader: `varying vec3 vWorld;
      void main(){ vec4 w=modelMatrix*vec4(position,1.0); vWorld=w.xyz;
        gl_Position=projectionMatrix*viewMatrix*w; }`,
    fragmentShader: `varying vec3 vWorld; uniform float uTime; uniform vec3 uGlow,uBg; uniform vec2 uC; ${HAZE}
      void main(){
        vec2 g=abs(fract(vWorld.xz*0.5)-0.5);
        float line=smoothstep(0.47,0.5,max(g.x,g.y));
        float d=length(vWorld.xz-uC);
        float pulse=0.5+0.5*sin(d*0.35-uTime*2.2);
        vec3 col=uBg+uGlow*line*(0.25+0.55*pulse*smoothstep(90.0,10.0,d));
        col+=uGlow*0.02;
        gl_FragColor=vec4(haze(col,vWorld),1.0); }`,
  });
  const grid = new THREE.Mesh(new THREE.PlaneGeometry(340, 340, 1, 1), gmat);
  grid.rotation.x = -Math.PI / 2;
  grid.position.set(cx, 0, cz);
  group.add(grid);

  // Wireframe hills on the horizon.
  const { fbm } = makeNoise(rand);
  const wire = new THREE.MeshBasicMaterial({ color: glowCol, wireframe: true, transparent: true, opacity: 0.16 });
  for (const side of [[-1, 0], [1, 0], [0, -1], [0, 1]]) {
    const hg = new THREE.PlaneGeometry(160, 60, 42, 14);
    hg.rotateX(-Math.PI / 2);
    const hp = hg.attributes.position;
    for (let i = 0; i < hp.count; i++) {
      const x = hp.getX(i), z = hp.getZ(i);
      hp.setY(i, ridge(fbm(x * 0.02, z * 0.05)) * 14 * Math.max(0, 1 - Math.abs(z) / 30));
    }
    const hm = new THREE.Mesh(hg, wire);
    hm.position.set(cx + side[0] * 120, -0.5, cz + side[1] * 120);
    if (side[1] !== 0) hm.rotation.y = Math.PI / 2;
    group.add(hm);
  }

  // The cursor monolith — a giant blinking block at the centre.
  const curMat = new THREE.MeshStandardMaterial({ color: bgCol, emissive: glowCol, emissiveIntensity: 1.5 });
  const cursor = new THREE.Mesh(new THREE.BoxGeometry(1.8, 4.2, 0.6), curMat);
  cursor.position.set(cx, 2.4, cz);
  group.add(cursor);
  block(room, cx, cz, 1.0);

  // Floating phosphor panels rendering a fake channel scrollback, billboarded
  // to the camera in game3d's update via lookAt (the camera position is
  // handed in through update's extra arg).
  const panels = [];
  for (let i = 0; i < 5; i++) {
    const a = i / 5 * Math.PI * 2 + rand(), r = 8 + rand() * 7;
    const scroll = i % 2 === 1;
    const c = document.createElement('canvas');
    c.width = 512;
    c.height = scroll ? 1024 : 640;
    const ctx = c.getContext('2d');
    ctx.fillStyle = 'rgba(2,4,6,0.92)';
    ctx.fillRect(0, 0, c.width, c.height);
    ctx.font = "26px 'IBM Plex Mono', ui-monospace, monospace";
    ctx.fillStyle = glowCol;
    ctx.globalAlpha = 0.92;
    const n = scroll ? 34 : 18;
    for (let l = 0; l < n; l++) ctx.fillText(TERM_LINES[Math.floor(rand() * TERM_LINES.length)], 24, 44 + l * 30);
    const tx = new THREE.CanvasTexture(c);
    if (scroll) tx.wrapT = THREE.RepeatWrapping;
    const m2 = new THREE.Mesh(new THREE.PlaneGeometry(5, i % 2 ? 6 : 4),
      new THREE.MeshBasicMaterial({ map: tx, transparent: true, opacity: 0.9, side: THREE.DoubleSide }));
    m2.position.set(cx + Math.cos(a) * r, 2.8 + rand() * 2.5, cz + Math.sin(a) * r);
    panels.push({ m: m2, ph: rand() * 6, scroll });
    group.add(m2);
  }
  group.add(new THREE.HemisphereLight(glowCol, '#000000', 0.45));
  const gl2 = new THREE.PointLight(glowCol, 2, 30, 1.6);
  gl2.position.set(cx, 5, cz);
  group.add(gl2);

  return {
    custom: true, group, seats: [], fog, animated: [], heightAt: null,
    update(t, dt, camPos) {
      gmat.uniforms.uTime.value = t;
      curMat.emissiveIntensity = (Math.sin(t * 3.6) > 0) ? 1.5 : 0.3; // cursor blink
      for (const p of panels) {
        p.m.position.y += Math.sin(t * 0.7 + p.ph) * 0.0025;
        if (camPos) p.m.lookAt(camPos.x, p.m.position.y, camPos.z); // screens face the viewer
        if (p.scroll) p.m.material.map.offset.y = t * 0.03 + p.ph;
      }
    },
  };
}

const BUILDERS = { sea: buildSea, dunes: buildDunes, camp: buildCamp, space: buildSpace, terminal: buildTerm };

// Entry point for game3d.enterRoom. `seed` is the per-channel world seed —
// XORed with the room id so each world rolls its own dice.
export function buildWorldScene(room, seed) {
  room._blocked = new Set(); // rebuilt fresh (obstacles are seed-dependent)
  const rand = mulberry32((seed ^ strHash(room.id)) >>> 0);
  const world = BUILDERS[room.id](room, rand);
  // Return portals for every door, matching the tile rooms' look.
  for (const d of room.doors || []) {
    const { group: portal, ring } = makePortal(d, 0x39c6ff, room.w / 2, room.h / 2);
    if (world.heightAt) portal.position.y = world.heightAt(d.x, d.y);
    world.group.add(portal);
    world.animated.push({ node: ring, kind: 'portal' });
  }
  return world;
}
