// Peer-to-peer file transfer in DMs (spec F7), client side.
//
// The file never touches the server: it goes over a WebRTC data channel, direct
// between the two peers when hole punching works and through a TURN relay when
// it does not. The chat socket carries only the handshake (see server/p2p.go for
// the frames and why the server refuses to relay them anywhere but a DM).
//
// This module is deliberately self-contained: it knows about frames and files
// and nothing about the UI. Wire it up by handing it a `send` callback and a
// context of callbacks; it never imports the client or touches the DOM.
//
// ponytail: NO RESUME. A dropped transfer is a failed transfer — every path out
// of here either finishes with a verified checksum or calls onError with a
// sentence a human can read. Nothing hangs: there is a stall watchdog on the
// data channel and a deadline on the accept prompt. Resume means server-side or
// peer-side offset state and a rendezvous protocol for reconnecting to a
// half-finished transfer; that is F7's "resume" bullet and it is not built. When
// it is, the place to put it is the byte loop in `pump` plus an offset field on
// p2p.begin.

export type Frame = { t: string; d?: any };
export type Send = (f: Frame) => void;

/** Which way the bytes are going, and what they are. */
export type TransferInfo = {
  id: string;
  channel: string;
  /** nick of the other side */
  peer: string;
  name: string;
  size: number;
  dir: "send" | "recv";
};

/** How the bytes are actually travelling, once the connection is up.
 *  `direct` = a host or server-reflexive (STUN) candidate pair, i.e. really
 *  peer-to-peer. `relayed` = a TURN server is forwarding, which is still E2E
 *  encrypted but is not what "peer-to-peer" promises — so the UI says so. */
export type Transport = "direct" | "relayed" | "unknown";

export type Progress = {
  id: string;
  bytes: number;
  total: number;
  /** bytes/second over the whole transfer so far */
  rate: number;
  transport: Transport;
};

/** Everything the surrounding client has to provide. Only `send` is needed for
 *  the sending path; the rest drive the receiving path and error reporting. */
export type P2PCtx = {
  send: Send;
  /** Ask the human. Resolve true to accept the file, false to reject it.
   *  Nothing is received until this resolves true — that is F7's "the receiver
   *  explicitly accepts before any bytes flow". */
  confirm: (t: TransferInfo) => Promise<boolean>;
  onProgress: (p: Progress) => void;
  /** A completed, checksum-verified file. */
  onDone: (t: TransferInfo, blob: Blob) => void;
  onError: (t: TransferInfo, message: string) => void;
  /** Override for self-hosted deployments; see DEFAULT_ICE. */
  iceServers?: RTCIceServer[];
};

// Google's public STUN server is a sane default so a fresh deployment works at
// all, but it is somebody else's infrastructure learning your users' IP
// addresses. A self-hosted Gliph should configure its own STUN and a TURN server
// (pion/turn, per docs/plans/gliph.md) and pass them as `iceServers` — without a
// TURN server the ~10-30% of peer pairs behind symmetric NAT simply cannot
// connect, and the transfer fails instead of falling back to a relay.
export const DEFAULT_ICE: RTCIceServer[] = [{ urls: "stun:stun.l.google.com:19302" }];

const CHUNK = 64 * 1024;
// Keep at most this much queued in the SCTP send buffer. Blasting a whole file
// into dc.send() buys nothing (the wire is the wire) and costs a copy of the
// file in browser memory, which kills the tab on a big one.
const HIGH_WATER = 1 << 20;
const LOW_WATER = 256 * 1024;
// No bytes and no handshake progress for this long = dead, not slow. 512KB/30s
// is under 20KB/s, well below any link that could finish a real file.
const STALL_MS = 30_000;
// How long the sender waits for a human to click accept before giving up.
const ACCEPT_MS = 5 * 60_000;

type Transfer = {
  info: TransferInfo;
  ctx: P2PCtx;
  pc: RTCPeerConnection | null;
  dc: RTCDataChannel | null;
  sha256: string;
  transport: Transport;
  started: number;
  received: number;
  chunks: ArrayBuffer[];
  /** ICE that arrived before setRemoteDescription; adding it early throws. */
  pendingIce: RTCIceCandidateInit[];
  haveRemote: boolean;
  stall: ReturnType<typeof setTimeout> | null;
  settled: boolean;
  /** sender-side: resolved by p2p.accept, rejected by p2p.reject/cancel */
  accept?: (ok: boolean) => void;
  /** sender-side: resolved by the receiver's final checksum verdict */
  finish?: (err: string | null) => void;
};

// One map for both directions, keyed by the sender-minted transfer id. Module
// state rather than a class because there is exactly one of these per tab.
const transfers = new Map<string, Transfer>();

// ---- public API ----

/** Send a file to the other member of a DM. Resolves when the receiver has
 *  assembled the file AND verified its checksum; rejects, loudly, otherwise. */
export async function offerFile(
  send: Send,
  channel: string,
  file: File,
  onProgress: (p: Progress) => void,
  iceServers: RTCIceServer[] = DEFAULT_ICE,
): Promise<string> {
  const id = newID();
  const info: TransferInfo = { id, channel, peer: "", name: file.name, size: file.size, dir: "send" };
  // The context the frame handler will use for this transfer's replies. The
  // sending path resolves through promises, so confirm/onDone are never called.
  const ctx: P2PCtx = {
    send,
    confirm: async () => false,
    onProgress,
    onDone: () => {},
    onError: () => {},
    iceServers,
  };
  const t = track(info, ctx);

  try {
    // Hash first, so the digest can travel with the offer and the receiver can
    // reject a corrupt transfer instead of silently saving rubbish.
    // ponytail: hashes the whole file in memory (WebCrypto has no streaming
    // digest). Ceiling is roughly a GB before the tab suffers; the upgrade path
    // is a per-chunk Merkle-ish hash sent alongside each chunk, which also gets
    // you resume for free.
    t.sha256 = await sha256Hex(await file.arrayBuffer());

    send({ t: "p2p.begin", d: { channel, id, name: file.name, size: file.size, sha256: t.sha256 } });

    const accepted = await new Promise<boolean>((res, rej) => {
      t.accept = res;
      armStall(t, ACCEPT_MS, "the other side never answered");
      t.finish = (err) => rej(new Error(err ?? "transfer failed"));
    });
    if (!accepted) throw new Error("the other side declined the transfer");

    const pc = newPeer(t);
    const dc = pc.createDataChannel("gliph-file", { ordered: true });
    t.dc = dc;
    dc.binaryType = "arraybuffer";
    dc.bufferedAmountLowThreshold = LOW_WATER;

    const verdict = new Promise<void>((res, rej) => {
      // The receiver's last act is a one-word checksum verdict, so the sender
      // learns the file arrived intact rather than assuming it from "I sent the
      // last byte".
      dc.onmessage = (e) => (e.data === "ok" ? res() : rej(new Error("the receiver's checksum did not match")));
      t.finish = (err) => (err ? rej(new Error(err)) : res());
    });

    const open = new Promise<void>((res, rej) => {
      dc.onopen = () => res();
      dc.onerror = () => rej(new Error("data channel failed to open"));
    });

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    send({ t: "p2p.offer", d: { channel, id, sdp: JSON.stringify(pc.localDescription) } });

    armStall(t, STALL_MS, "could not reach the other client");
    await open;
    t.transport = await detectTransport(pc);
    t.started = Date.now();
    report(t, 0);

    await pump(t, file);
    // The last chunk is sent, but the transfer is not done until the receiver
    // says the checksum matched. Keep the watchdog armed so a receiver that
    // dies at the finish line fails us rather than hanging us.
    armStall(t, STALL_MS, "the receiver never confirmed the file");
    await verdict;
    cleanup(t);
    return id;
  } catch (err) {
    // One exit for every failure: tell the peer, tear down, surface the message.
    const msg = err instanceof Error ? err.message : String(err);
    if (!t.settled) send({ t: "p2p.cancel", d: { channel, id, reason: msg } });
    cleanup(t);
    throw err instanceof Error ? err : new Error(msg);
  }
}

/** Every p2p.* frame from the server lands here. Frames for a transfer we are
 *  not driving are dropped: the server relays to all of a user's devices, so
 *  only the tab that answered the prompt should act. */
export function handleP2PFrame(frame: Frame, ctx: P2PCtx): void {
  const d = frame.d ?? {};
  const id: string = d.id ?? "";
  if (!id) return;
  const t = transfers.get(id);

  switch (frame.t) {
    case "p2p.begin": {
      if (t) return; // duplicate offer for a transfer already in flight
      void receive(d, ctx);
      return;
    }
    case "p2p.accept":
      if (t?.accept) {
        clearStall(t);
        t.info.peer = d.from ?? t.info.peer;
        t.accept(true);
      }
      return;
    case "p2p.reject":
      if (t?.accept) {
        clearStall(t);
        t.accept(false);
      }
      return;
    case "p2p.cancel":
      // notify:false — they already know, they are the ones who cancelled.
      if (t) fail(t, d.reason ? `the other side cancelled: ${d.reason}` : "the other side cancelled", false);
      return;
    case "p2p.answer":
      if (t?.pc) void applyRemote(t, d.sdp);
      return;
    case "p2p.offer":
      // Only the receiving side gets an offer, and only after it accepted.
      if (t && !t.haveRemote) void answerOffer(t, d.sdp);
      return;
    case "p2p.ice":
      if (t) void addIce(t, d.candidate, d.mid);
      return;
  }
}

/** Live transfers, for a UI that wants to list them. */
export function activeTransfers(): TransferInfo[] {
  return [...transfers.values()].map((t) => t.info);
}

// ---- receiving ----

async function receive(d: any, ctx: P2PCtx): Promise<void> {
  const info: TransferInfo = {
    id: d.id,
    channel: d.channel,
    peer: d.from ?? "",
    name: String(d.name ?? "file"),
    size: Number(d.size ?? 0),
    dir: "recv",
  };
  const t = track(info, ctx);
  t.sha256 = String(d.sha256 ?? "");

  let ok = false;
  try {
    ok = await ctx.confirm(info);
  } catch {
    ok = false;
  }
  if (!ok) {
    ctx.send({ t: "p2p.reject", d: { channel: info.channel, id: info.id, reason: "declined" } });
    transfers.delete(info.id);
    return;
  }
  ctx.send({ t: "p2p.accept", d: { channel: info.channel, id: info.id } });
  // From here the sender drives: it will send an offer. If it never does, the
  // watchdog fires rather than leaving a half-open prompt forever.
  armStall(t, STALL_MS, "the sender never connected");
}

async function answerOffer(t: Transfer, sdp: string): Promise<void> {
  try {
    const pc = newPeer(t);
    pc.ondatachannel = (e) => wireReceiver(t, e.channel);
    await pc.setRemoteDescription(JSON.parse(sdp));
    t.haveRemote = true;
    await flushIce(t);
    await pc.setLocalDescription(await pc.createAnswer());
    t.ctx.send({
      t: "p2p.answer",
      d: { channel: t.info.channel, id: t.info.id, sdp: JSON.stringify(pc.localDescription) },
    });
    armStall(t, STALL_MS, "could not reach the sender");
  } catch (err) {
    fail(t, `could not answer the transfer offer: ${msgOf(err)}`);
  }
}

function wireReceiver(t: Transfer, dc: RTCDataChannel): void {
  t.dc = dc;
  dc.binaryType = "arraybuffer";
  dc.onopen = async () => {
    t.transport = await detectTransport(t.pc!);
    t.started = Date.now();
    report(t, 0);
  };
  dc.onmessage = (e) => {
    if (typeof e.data === "string") return; // control words only travel the other way
    const buf: ArrayBuffer = e.data;
    t.chunks.push(buf);
    t.received += buf.byteLength;
    report(t, t.received);
    armStall(t, STALL_MS, "the transfer stalled");
    // The size from p2p.begin is the only end-of-file marker: the data channel
    // is ordered, so byte count is enough and no terminator frame is needed.
    if (t.received >= t.info.size) void complete(t);
  };
  dc.onclose = () => {
    if (!t.settled) fail(t, "the connection dropped before the file finished");
  };
}

async function complete(t: Transfer): Promise<void> {
  clearStall(t);
  const blob = new Blob(t.chunks);
  const got = await sha256Hex(await blob.arrayBuffer());
  const good = got === t.sha256.toLowerCase();
  // Tell the sender the verdict before tearing down, so a corrupt transfer is
  // an error on both screens rather than a success on one of them.
  try {
    t.dc?.send(good ? "ok" : "bad");
  } catch {
    /* channel already gone; the local verdict still stands */
  }
  if (good) {
    t.settled = true;
    t.ctx.onDone(t.info, blob);
    cleanup(t);
  } else {
    fail(t, "the file arrived corrupted (checksum mismatch) — nothing was saved");
  }
}

// ---- sending ----

async function pump(t: Transfer, file: File): Promise<void> {
  const dc = t.dc!;
  let offset = 0;
  while (offset < file.size) {
    if (t.settled) throw new Error("transfer cancelled");
    if (dc.readyState !== "open") throw new Error("the connection dropped mid-transfer");
    // Backpressure: wait for the buffer to drain rather than queueing the whole
    // file. `bufferedamountlow` fires once we are back under LOW_WATER.
    if (dc.bufferedAmount > HIGH_WATER) {
      await Promise.race([
        new Promise<void>((res) => dc.addEventListener("bufferedamountlow", () => res(), { once: true })),
        timeout(STALL_MS, "the transfer stalled"),
      ]);
    }
    const buf = await file.slice(offset, offset + CHUNK).arrayBuffer();
    dc.send(buf);
    offset += buf.byteLength;
    report(t, offset);
    armStall(t, STALL_MS, "the transfer stalled");
  }
  clearStall(t);
}

// ---- plumbing ----

function track(info: TransferInfo, ctx: P2PCtx): Transfer {
  const t: Transfer = {
    info, ctx, pc: null, dc: null, sha256: "", transport: "unknown",
    started: Date.now(), received: 0, chunks: [], pendingIce: [],
    haveRemote: false, stall: null, settled: false,
  };
  transfers.set(info.id, t);
  return t;
}

function newPeer(t: Transfer): RTCPeerConnection {
  const pc = new RTCPeerConnection({ iceServers: t.ctx.iceServers ?? DEFAULT_ICE });
  t.pc = pc;
  // Trickle ICE: each candidate goes out as it is discovered, so the connection
  // comes up without waiting for gathering to finish.
  pc.onicecandidate = (e) => {
    if (!e.candidate) return;
    t.ctx.send({
      t: "p2p.ice",
      d: {
        channel: t.info.channel, id: t.info.id,
        candidate: e.candidate.candidate, mid: e.candidate.sdpMid ?? "",
      },
    });
  };
  pc.onconnectionstatechange = () => {
    if (pc.connectionState === "failed") {
      // No silent hang: a failed ICE negotiation usually means both peers are
      // behind symmetric NAT and the deployment has no TURN server.
      fail(t, "could not establish a peer-to-peer connection (no route, and no TURN relay configured)");
    }
  };
  return pc;
}

async function applyRemote(t: Transfer, sdp: string): Promise<void> {
  try {
    await t.pc!.setRemoteDescription(JSON.parse(sdp));
    t.haveRemote = true;
    await flushIce(t);
  } catch (err) {
    fail(t, `bad answer from the other client: ${msgOf(err)}`);
  }
}

async function addIce(t: Transfer, candidate: string, mid: string): Promise<void> {
  const init: RTCIceCandidateInit = { candidate, sdpMid: mid || null };
  // Candidates can arrive before the description they belong to; queue those
  // rather than throwing them away, or a race costs us the connection.
  if (!t.haveRemote || !t.pc) {
    t.pendingIce.push(init);
    return;
  }
  try {
    await t.pc.addIceCandidate(init);
  } catch {
    /* a candidate the browser rejects is not fatal: others may still work */
  }
}

async function flushIce(t: Transfer): Promise<void> {
  const queued = t.pendingIce.splice(0);
  for (const c of queued) {
    try {
      await t.pc!.addIceCandidate(c);
    } catch {
      /* see addIce */
    }
  }
}

/** Ask the connection how the bytes are actually travelling. The selected
 *  candidate pair names a local and remote candidate; `relay` on either end
 *  means a TURN server is in the path, so the UI must not claim "peer-to-peer". */
async function detectTransport(pc: RTCPeerConnection): Promise<Transport> {
  try {
    const stats = await pc.getStats();
    const all = new Map<string, any>();
    stats.forEach((r: any) => all.set(r.id, r));
    let pair: any = null;
    for (const r of all.values()) {
      if (r.type === "candidate-pair" && (r.selected || r.nominated) && r.state === "succeeded") pair = r;
    }
    if (!pair) return "unknown";
    const local = all.get(pair.localCandidateId);
    const remote = all.get(pair.remoteCandidateId);
    if (!local || !remote) return "unknown";
    return local.candidateType === "relay" || remote.candidateType === "relay" ? "relayed" : "direct";
  } catch {
    return "unknown";
  }
}

function report(t: Transfer, bytes: number): void {
  const secs = Math.max((Date.now() - t.started) / 1000, 0.001);
  t.ctx.onProgress({
    id: t.info.id, bytes, total: t.info.size,
    rate: bytes / secs, transport: t.transport,
  });
}

function armStall(t: Transfer, ms: number, why: string): void {
  clearStall(t);
  t.stall = setTimeout(() => fail(t, why), ms);
}

function clearStall(t: Transfer): void {
  if (t.stall) clearTimeout(t.stall);
  t.stall = null;
}

/** The one way a transfer dies. Always reports; never leaves a promise pending. */
function fail(t: Transfer, why: string, notify = true): void {
  if (t.settled) return;
  t.settled = true;
  clearStall(t);
  // The other end is sitting on a half-open connection; tell it why it stopped
  // instead of leaving it to its own watchdog.
  if (notify) t.ctx.send({ t: "p2p.cancel", d: { channel: t.info.channel, id: t.info.id, reason: why } });
  // The sending path is waiting on promises; the receiving path on callbacks.
  if (t.finish) t.finish(why);
  else if (t.accept) t.accept(false);
  if (t.info.dir === "recv") t.ctx.onError(t.info, why);
  closePeer(t);
  transfers.delete(t.info.id);
}

function cleanup(t: Transfer): void {
  t.settled = true;
  clearStall(t);
  closePeer(t);
  transfers.delete(t.info.id);
}

function closePeer(t: Transfer): void {
  try {
    t.dc?.close();
    t.pc?.close();
  } catch {
    /* already closed */
  }
  t.chunks = [];
}

function timeout(ms: number, why: string): Promise<never> {
  return new Promise((_, rej) => setTimeout(() => rej(new Error(why)), ms));
}

async function sha256Hex(buf: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", buf);
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function newID(): string {
  return crypto.randomUUID ? crypto.randomUUID() : Math.random().toString(36).slice(2) + Date.now().toString(36);
}

function msgOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
