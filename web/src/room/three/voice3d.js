// room0 3D — per-room voice channels. A full-mesh WebRTC audio overlay: every
// voice member holds a direct RTCPeerConnection to every other voice member in
// the same sub-room. At room0 scale (≤10 people, Opus audio only) a mesh is
// cheap — no media server, best-possible latency. Signaling (SDP + ICE) rides
// the existing RoomPresenceChannel `rtc` relay; who-is-in-voice rides the
// `voice` flag on move/announce heartbeats. game3d owns the policy (who to
// connect to) and drives sync(); this module owns the connections + audio.
//
// Remote voices are spatialised: each stream feeds a WebAudio PannerNode kept
// at the speaker's avatar position while the listener tracks the player, so
// coworkers get louder as you walk over and pan as you turn.

// Public-STUN fallback only. The real list comes from the server: room.ts
// fetches GET /api/ice — the same coturn-backed endpoint the p2p transfers
// use, with ephemeral use-auth-secret credentials — and swaps it in here. A
// server configured with neither STUN nor TURN returns an empty list, and
// these defaults keep the easy (non-symmetric-NAT) pairs working.
let ICE_SERVERS = [
  { urls: 'stun:stun.l.google.com:19302' },
  { urls: 'stun:stun.cloudflare.com:3478' },
];

export function setIceServers(list) {
  if (Array.isArray(list) && list.length) ICE_SERVERS = list;
}
const HEAD_Y = 1.5; // ear/mouth height in world units (avatar is 1.28 + tag)

export function createVoiceChat({ getSelfKey, sendSignal, onState, onNotice }) {
  let active = false;
  let micMuted = false;
  let localStream = null;
  let ctx = null; // AudioContext — created on join (a user gesture, so it may start)
  let joinSeq = 0; // invalidates a getUserMedia grant that resolves after leave()
  const conns = new Map(); // uid → {pc, audioEl, panner, gain, pos, pending[]}

  function emitState() {
    let connected = 0;
    for (const c of conns.values()) {
      if (c.pc.connectionState === 'connected') connected += 1;
    }
    onState?.({ active, micMuted, peers: conns.size, connected });
  }

  // ── Spatial audio helpers ──
  // Safari still lacks the AudioParam form of listener/panner positions.
  function setPannerPos(panner, x, z) {
    if (panner.positionX) {
      panner.positionX.value = x;
      panner.positionY.value = HEAD_Y;
      panner.positionZ.value = z;
    } else {
      panner.setPosition(x, HEAD_Y, z);
    }
  }

  function setListener(x, z, heading) {
    if (!ctx) return;
    const l = ctx.listener;
    const fx = Math.sin(heading);
    const fz = Math.cos(heading);
    if (l.positionX) {
      l.positionX.value = x;
      l.positionY.value = HEAD_Y;
      l.positionZ.value = z;
      l.forwardX.value = fx;
      l.forwardY.value = 0;
      l.forwardZ.value = fz;
      l.upX.value = 0;
      l.upY.value = 1;
      l.upZ.value = 0;
    } else {
      l.setPosition(x, HEAD_Y, z);
      l.setOrientation(fx, 0, fz, 0, 1, 0);
    }
  }

  function attachRemote(c, stream) {
    // Chromium quirk: a remote WebRTC stream is silent inside WebAudio unless
    // it is also attached to a media element — keep a muted one around.
    const el = new Audio();
    el.srcObject = stream;
    el.muted = true;
    el.play().catch(() => {});
    c.audioEl = el;
    try {
      const src = ctx.createMediaStreamSource(stream);
      const panner = ctx.createPanner();
      panner.panningModel = 'HRTF';
      panner.distanceModel = 'inverse';
      panner.refDistance = 1.4; // conversation distance = full volume
      panner.rolloffFactor = 1.1;
      panner.maxDistance = 40;
      const gain = ctx.createGain();
      src.connect(panner).connect(gain).connect(ctx.destination);
      c.panner = panner;
      c.gain = gain;
      if (c.pos) setPannerPos(panner, c.pos.x, c.pos.z);
    } catch {
      el.muted = false; // no WebAudio for this stream — flat playback beats silence
    }
    emitState();
  }

  // ── Connections ──
  function ensureConn(uid) {
    let c = conns.get(uid);
    if (c) return c;
    const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });
    c = { uid, pc, audioEl: null, panner: null, gain: null, pos: null, pending: [] };
    conns.set(uid, c);
    for (const track of localStream.getTracks()) pc.addTrack(track, localStream);
    pc.onicecandidate = (e) => {
      if (e.candidate) sendSignal(uid, 'candidate', e.candidate.toJSON());
    };
    pc.ontrack = (e) => attachRemote(c, e.streams[0] || new MediaStream([e.track]));
    pc.onconnectionstatechange = () => {
      // 'disconnected' often self-heals; a hard failure gets dropped and the
      // next sync() pass (presence event / 5s sweep) re-establishes it.
      if (pc.connectionState === 'failed') dropConn(uid);
      else emitState();
    };
    return c;
  }

  function dropConn(uid) {
    const c = conns.get(uid);
    if (!c) return;
    conns.delete(uid);
    try { c.pc.close(); } catch { /* already gone */ }
    if (c.audioEl) { c.audioEl.srcObject = null; c.audioEl = null; }
    try { c.gain?.disconnect(); } catch { /* node already detached */ }
    emitState();
  }

  async function initiate(uid) {
    const c = ensureConn(uid);
    try {
      const offer = await c.pc.createOffer();
      await c.pc.setLocalDescription(offer);
      sendSignal(uid, 'offer', { type: offer.type, sdp: offer.sdp });
    } catch {
      dropConn(uid);
    }
  }

  async function flushCandidates(c) {
    const pending = c.pending.splice(0);
    for (const cand of pending) {
      await c.pc.addIceCandidate(cand).catch(() => {});
    }
  }

  async function handleSignalAsync(d) {
    const uid = d.user_id;
    if (d.kind === 'offer') {
      // Only the lower-key side initiates, so an incoming offer is either the
      // first contact or the initiator rebuilding after a failure — a fresh
      // connection is correct in both cases.
      if (conns.has(uid)) dropConn(uid);
      const c = ensureConn(uid);
      await c.pc.setRemoteDescription(d.payload);
      await flushCandidates(c);
      const answer = await c.pc.createAnswer();
      await c.pc.setLocalDescription(answer);
      sendSignal(uid, 'answer', { type: answer.type, sdp: answer.sdp });
    } else if (d.kind === 'answer') {
      const c = conns.get(uid);
      if (!c || c.pc.signalingState !== 'have-local-offer') return;
      await c.pc.setRemoteDescription(d.payload);
      await flushCandidates(c);
    } else if (d.kind === 'candidate') {
      const c = conns.get(uid);
      if (!c || !d.payload) return;
      if (c.pc.remoteDescription) await c.pc.addIceCandidate(d.payload).catch(() => {});
      else c.pending.push(d.payload);
    }
  }

  function applyMicMute() {
    for (const t of localStream?.getAudioTracks() || []) t.enabled = !micMuted;
  }

  return {
    isActive: () => active,

    async join() {
      if (active) return true;
      const seq = ++joinSeq;
      let stream;
      try {
        stream = await navigator.mediaDevices.getUserMedia({
          audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        });
      } catch {
        onNotice?.('voice needs the microphone — permission was denied or no mic found');
        return false;
      }
      if (seq !== joinSeq) { // left (or re-toggled) while the permission prompt was up
        for (const t of stream.getTracks()) t.stop();
        return false;
      }
      localStream = stream;
      if (!ctx) ctx = new AudioContext();
      ctx.resume?.().catch(() => {});
      active = true;
      applyMicMute();
      emitState();
      return true;
    },

    leave() {
      joinSeq += 1;
      active = false;
      for (const uid of [...conns.keys()]) dropConn(uid);
      for (const t of localStream?.getTracks() || []) t.stop();
      localStream = null;
      ctx?.suspend?.().catch(() => {});
      emitState();
    },

    toggleMic() {
      micMuted = !micMuted;
      applyMicMute();
      emitState();
    },

    // Reconcile connections with the peers we should hear (voice-on, same
    // sub-room — computed by game3d). Deterministic initiator: the lower user
    // key offers, the higher one answers — no glare, ever.
    sync(desired) {
      for (const uid of [...conns.keys()]) {
        if (!active || !desired.has(uid)) dropConn(uid);
      }
      if (!active) return;
      const me = getSelfKey();
      if (!me) return;
      for (const uid of desired) {
        if (!conns.has(uid) && me < uid) initiate(uid);
      }
    },

    handleSignal(d) {
      if (!active || !d?.kind) return;
      handleSignalAsync(d).catch(() => { dropConn(d.user_id); });
    },

    setListener,

    setPeerPos(uid, x, z) {
      const c = conns.get(uid);
      if (!c) return;
      c.pos = { x, z };
      if (c.panner) setPannerPos(c.panner, x, z);
    },

    dispose() {
      this.leave();
      ctx?.close?.().catch(() => {});
      ctx = null;
    },
  };
}
