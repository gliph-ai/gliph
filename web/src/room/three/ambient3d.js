// room0 — procedural ambient soundscapes (WebAudio, no asset files). Each room
// gets a very quiet bed built from filtered noise plus small per-room touches:
// office/kitchen hum, garden breeze + occasional birdsong, lounge fire
// crackle, beach waves (slow LFO over the noise bed). Everything hangs off one
// master gain so mute is a single ramp. The AudioContext can only start after
// a user gesture — call userGesture() from any input handler.

const ROOM_SOUND = {
  office: { filter: 240, gain: 0.05, hum: 96 },
  kitchen: { filter: 320, gain: 0.045, hum: 120 },
  garden: { filter: 900, gain: 0.05, birds: true },
  lounge: { filter: 420, gain: 0.035, crackle: true },
  beach: { filter: 620, gain: 0.09, waves: true },
  // Worlds: open water, desert wind, night forest + fire, station hum, grid hum.
  sea: { filter: 560, gain: 0.1, waves: true },
  dunes: { filter: 760, gain: 0.06 },
  camp: { filter: 820, gain: 0.045, birds: true, crackle: true },
  space: { filter: 180, gain: 0.04, hum: 60 },
  terminal: { filter: 260, gain: 0.045, hum: 110 },
};

export function createAmbient({ muted = false } = {}) {
  let ctx = null;
  let master = null;
  let bedGain = null;
  let bedFilter = null;
  let lfoOsc = null;
  let lfoGain = null;
  let humOsc = null;
  let humGain = null;
  let birdTimer = null;
  let crackleTimer = null;
  let roomId = 'office';
  let isMuted = muted;
  let rollGain = null; // shared rolling-ball loop (created lazily)
  let disposed = false;
  let dayness = 1; // 0 = night, 1 = midday (birds ↔ crickets in the garden)

  function ensure() {
    if (ctx || disposed) return false;
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return false;
    ctx = new AC();

    master = ctx.createGain();
    master.gain.value = isMuted ? 0 : 1;
    master.connect(ctx.destination);

    // Noise bed: 2s brown-noise loop → lowpass → gain.
    const len = ctx.sampleRate * 2;
    const buf = ctx.createBuffer(1, len, ctx.sampleRate);
    const data = buf.getChannelData(0);
    let last = 0;
    for (let i = 0; i < len; i++) {
      const white = Math.random() * 2 - 1;
      last = (last + 0.02 * white) / 1.02;
      data[i] = last * 3.5;
    }
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.loop = true;
    bedFilter = ctx.createBiquadFilter();
    bedFilter.type = 'lowpass';
    bedGain = ctx.createGain();
    bedGain.gain.value = 0;
    src.connect(bedFilter).connect(bedGain).connect(master);
    src.start();

    // Slow LFO for the beach's wave swell (depth 0 elsewhere).
    lfoOsc = ctx.createOscillator();
    lfoOsc.frequency.value = 0.08;
    lfoGain = ctx.createGain();
    lfoGain.gain.value = 0;
    lfoOsc.connect(lfoGain).connect(bedGain.gain);
    lfoOsc.start();

    // Faint machine-room hum for indoor rooms.
    humOsc = ctx.createOscillator();
    humOsc.type = 'triangle';
    humGain = ctx.createGain();
    humGain.gain.value = 0;
    humOsc.connect(humGain).connect(master);
    humOsc.start();

    applyRoom();
    return true;
  }

  function clearTimers() {
    if (birdTimer) { clearTimeout(birdTimer); birdTimer = null; }
    if (crackleTimer) { clearTimeout(crackleTimer); crackleTimer = null; }
  }

  // A tiny two-note descending chirp.
  function chirp() {
    if (!ctx || disposed || isMuted) return;
    const t0 = ctx.currentTime;
    for (let n = 0; n < 2 + Math.floor(Math.random() * 2); n++) {
      const osc = ctx.createOscillator();
      const g = ctx.createGain();
      const start = t0 + n * 0.16 + Math.random() * 0.04;
      const f = 2400 + Math.random() * 900;
      osc.frequency.setValueAtTime(f, start);
      osc.frequency.exponentialRampToValueAtTime(f * 0.7, start + 0.1);
      g.gain.setValueAtTime(0, start);
      g.gain.linearRampToValueAtTime(0.02, start + 0.02);
      g.gain.exponentialRampToValueAtTime(0.0001, start + 0.13);
      osc.connect(g).connect(master);
      osc.start(start);
      osc.stop(start + 0.15);
    }
  }

  // Cricket trill for garden nights: short bursts of rapid high pulses.
  function cricket() {
    if (!ctx || disposed || isMuted) return;
    const t0 = ctx.currentTime;
    for (let b = 0; b < 3; b++) {
      const osc = ctx.createOscillator();
      osc.type = 'sine';
      osc.frequency.value = 4100 + Math.random() * 400;
      const g = ctx.createGain();
      g.gain.value = 0;
      const start = t0 + b * 0.1;
      for (let i = 0; i < 4; i++) {
        g.gain.setValueAtTime(0.007, start + i * 0.02);
        g.gain.setValueAtTime(0, start + i * 0.02 + 0.011);
      }
      osc.connect(g).connect(master);
      osc.start(start);
      osc.stop(start + 0.1);
    }
  }

  function scheduleBirds() {
    if (disposed || !ROOM_SOUND[roomId]?.birds) return;
    birdTimer = setTimeout(() => {
      if (dayness > 0.4) chirp();
      else cricket();
      scheduleBirds();
    }, (dayness > 0.4 ? 3500 : 2200) + Math.random() * 6000);
  }

  // A soft fire pop for the fireplace — low, quiet and with a gentle attack
  // (the first version was sharp and frequent enough to read as "ticking").
  function crackle() {
    if (!ctx || disposed || isMuted) return;
    const t0 = ctx.currentTime;
    const dur = 0.05 + Math.random() * 0.09;
    const len = Math.ceil(ctx.sampleRate * dur);
    const buf = ctx.createBuffer(1, len, ctx.sampleRate);
    const d = buf.getChannelData(0);
    for (let i = 0; i < len; i++) {
      const env = Math.sin((i / len) * Math.PI); // smooth in AND out — no click
      d[i] = (Math.random() * 2 - 1) * env;
    }
    const src = ctx.createBufferSource();
    src.buffer = buf;
    const bp = ctx.createBiquadFilter();
    bp.type = 'bandpass';
    bp.frequency.value = 500 + Math.random() * 900;
    bp.Q.value = 0.8;
    const g = ctx.createGain();
    g.gain.value = 0.01 + Math.random() * 0.014;
    src.connect(bp).connect(g).connect(master);
    src.start(t0);
  }

  function scheduleCrackle() {
    if (disposed || !ROOM_SOUND[roomId]?.crackle) return;
    crackleTimer = setTimeout(() => { crackle(); scheduleCrackle(); }, 500 + Math.random() * 1600);
  }

  function applyRoom() {
    if (!ctx) return;
    const cfg = ROOM_SOUND[roomId] || ROOM_SOUND.office;
    const t = ctx.currentTime;
    bedFilter.frequency.setTargetAtTime(cfg.filter, t, 0.6);
    bedGain.gain.setTargetAtTime(cfg.gain, t, 0.8);
    lfoGain.gain.setTargetAtTime(cfg.waves ? cfg.gain * 0.7 : 0, t, 0.8);
    if (cfg.hum) {
      humOsc.frequency.setTargetAtTime(cfg.hum, t, 0.4);
      humGain.gain.setTargetAtTime(0.012, t, 0.8);
    } else {
      humGain.gain.setTargetAtTime(0, t, 0.4);
    }
    clearTimers();
    if (cfg.birds) scheduleBirds();
    if (cfg.crackle) scheduleCrackle();
  }

  return {
    // Must be called from a user-input handler at least once.
    userGesture() {
      if (disposed) return;
      const created = ensure();
      if (!created && ctx?.state === 'suspended') ctx.resume().catch(() => {});
      if (created && ctx.state === 'suspended') ctx.resume().catch(() => {});
    },
    setRoom(id) {
      roomId = id;
      applyRoom();
    },
    setMuted(m) {
      isMuted = m;
      if (ctx) master.gain.setTargetAtTime(m ? 0 : 1, ctx.currentTime, 0.25);
    },
    setDaylight(day) {
      dayness = day;
    },
    // Object impact SFX — ball bounce / box thud / mug clink. `strength` 0..1.
    impact(kind, strength = 1) {
      if (!ctx || disposed || isMuted) return;
      const t0 = ctx.currentTime;
      const g = ctx.createGain();
      g.connect(master);
      if (kind === 'mug') {
        // Ceramic clink: two detuned high partials, very fast decay.
        for (const f of [1850, 2410]) {
          const o = ctx.createOscillator();
          o.type = 'sine';
          o.frequency.value = f * (0.97 + Math.random() * 0.06);
          o.connect(g);
          o.start(t0);
          o.stop(t0 + 0.13);
        }
        g.gain.setValueAtTime(0.14 * strength, t0);
        g.gain.exponentialRampToValueAtTime(0.0001, t0 + 0.12);
      } else if (kind === 'box') {
        // Cardboard thud: a low noise burst through a lowpass.
        const dur = 0.09;
        const len = Math.ceil(ctx.sampleRate * dur);
        const buf = ctx.createBuffer(1, len, ctx.sampleRate);
        const d = buf.getChannelData(0);
        for (let i = 0; i < len; i++) d[i] = (Math.random() * 2 - 1) * (1 - i / len);
        const src = ctx.createBufferSource();
        src.buffer = buf;
        const lp = ctx.createBiquadFilter();
        lp.type = 'lowpass';
        lp.frequency.value = 260;
        src.connect(lp).connect(g);
        g.gain.value = 0.5 * strength;
        src.start(t0);
      } else {
        // Ball bounce: a sine thump with a fast pitch drop.
        const o = ctx.createOscillator();
        o.type = 'sine';
        o.frequency.setValueAtTime(190, t0);
        o.frequency.exponentialRampToValueAtTime(70, t0 + 0.09);
        o.connect(g);
        g.gain.setValueAtTime(0.28 * strength, t0);
        g.gain.exponentialRampToValueAtTime(0.0001, t0 + 0.13);
        o.start(t0);
        o.stop(t0 + 0.15);
      }
    },
    // Shared rolling-ball loop. Call each frame with the loudest roller's
    // level (0..1, 0 → silent); the gain chases it smoothly.
    setRolling(level) {
      if (!ctx || disposed) return;
      if (!rollGain) {
        if (level <= 0) return; // don't build the loop until a ball rolls
        const len = Math.ceil(ctx.sampleRate * 1.2);
        const buf = ctx.createBuffer(1, len, ctx.sampleRate);
        const d = buf.getChannelData(0);
        for (let i = 0; i < len; i++) d[i] = Math.random() * 2 - 1;
        const src = ctx.createBufferSource();
        src.buffer = buf;
        src.loop = true;
        const bp = ctx.createBiquadFilter();
        bp.type = 'bandpass';
        bp.frequency.value = 380;
        bp.Q.value = 0.6;
        rollGain = ctx.createGain();
        rollGain.gain.value = 0;
        src.connect(bp).connect(rollGain).connect(master);
        src.start();
      }
      const target = isMuted ? 0 : Math.max(0, Math.min(1, level)) * 0.1;
      rollGain.gain.setTargetAtTime(target, ctx.currentTime, 0.08);
    },
    // Three descending "glug" blips — drinking from the fountain.
    gulp() {
      if (!ctx || disposed || isMuted) return;
      const t0 = ctx.currentTime;
      for (let i = 0; i < 3; i++) {
        const osc = ctx.createOscillator();
        osc.type = 'sine';
        const start = t0 + i * 0.18;
        const f = 340 - i * 40;
        osc.frequency.setValueAtTime(f, start);
        osc.frequency.exponentialRampToValueAtTime(f * 0.45, start + 0.13);
        const g = ctx.createGain();
        g.gain.setValueAtTime(0, start);
        g.gain.linearRampToValueAtTime(0.09, start + 0.025);
        g.gain.exponentialRampToValueAtTime(0.0001, start + 0.15);
        osc.connect(g).connect(master);
        osc.start(start);
        osc.stop(start + 0.16);
      }
    },
    dispose() {
      disposed = true;
      clearTimers();
      if (ctx) ctx.close().catch(() => {});
      ctx = null;
    },
  };
}
