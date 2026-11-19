// Nearby mesh, transport only (spec F9).
//
// The mesh's STATE — who is nearby, which channels we are in, what has been
// said, and the whole DCC transfer engine — moved into the Rust core, where the
// iOS app shares it. What is left here is the one thing the core cannot do:
// talk to the native side. On the desktop that is Tauri commands into
// desktop/src-tauri/src/mesh.rs; a browser tab has no transport at all, which
// is why `meshSupported` is just `isTauri`.

import { isTauri } from "./desktop";

/** Nearby channel ids are namespaced so nothing can confuse one with a server
 *  channel. The core routes on this prefix; the UI only needs to style it. */
export const NEARBY_PREFIX = "n_";
export const isNearby = (cid: string) => cid.startsWith(NEARBY_PREFIX);
export const nearbyName = (cid: string) => cid.slice(NEARBY_PREFIX.length);

export type MeshPeer = { id: string; nick: string; chans: string[] };
export type MeshMsg = {
  id: string; chan: string; nick: string; body: string; ts: string;
  /** "message" or "action" (/me), and the mesh id this answers. Both are
   *  optional on the wire: a peer on an older build sends neither. */
  kind?: string; reply_to?: string;
};
/** A frame the native side does not interpret — file transfer rides these. */
export type MeshFrame = { from: string; nick: string; t: string; d: unknown };

export const meshSupported = () => isTauri();

export type MeshTransport = {
  start(nick: string): Promise<void>;
  stop(): Promise<void>;
  join(chans: string[]): Promise<void>;
  send(chan: string, body: string, kind: string, replyTo: string): Promise<MeshMsg>;
  sendTo(peer: string, t: string, d: unknown): Promise<void>;
  listen(
    onPeers: (p: MeshPeer[]) => void,
    onMsg: (m: MeshMsg) => void,
    onFrame: (f: MeshFrame) => void,
  ): Promise<void>;
};

export async function meshTransport(): Promise<MeshTransport> {
  const { invoke } = await import("@tauri-apps/api/core");
  const { listen } = await import("@tauri-apps/api/event");
  return {
    start: async (nick) => { await invoke("mesh_start", { nick }); },
    stop: async () => { await invoke("mesh_stop"); },
    join: async (chans) => { await invoke("mesh_join", { chans }); },
    send: (chan, body, kind, replyTo) =>
      invoke<MeshMsg>("mesh_send", { chan, body, kind, replyTo }),
    sendTo: async (peer, t, d) => { await invoke("mesh_send_to", { peer, t, d }); },
    listen: async (onPeers, onMsg, onFrame) => {
      await listen<MeshPeer[]>("mesh:peers", (e) => onPeers(e.payload));
      await listen<MeshMsg>("mesh:msg", (e) => onMsg(e.payload));
      await listen<MeshFrame>("mesh:frame", (e) => onFrame(e.payload));
    },
  };
}
