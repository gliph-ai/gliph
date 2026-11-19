//! Nearby file transfer (spec F9 + F7), the DCC lane.
//!
//! mIRC's model, kept on purpose: you OFFER a file, the other side ACCEPTS, and
//! the bytes then flow directly between the two of you while a transfer window
//! counts them. Nobody receives a file they did not agree to, and the room is
//! not copied on it.
//!
//!   offer   sender -> one peer   {id, chan, name, size, mime}
//!   accept  receiver -> sender   {id}
//!   chunk   sender -> receiver   {id, seq, data}   base64, CHUNK bytes raw
//!   ack     receiver -> sender   {id, seq}         every chunk, as it lands
//!   done    sender -> receiver   {id}
//!   cancel  either way           {id, reason}
//!
//! **The receiver sets the pace.** The sender keeps at most [`WINDOW`] chunks
//! unacknowledged. Without that a fast sender pushes the whole file into a
//! socket buffer the receiver has not drained, the progress bar reaches 100%
//! while nothing has arrived, and a link that then stalls loses megabytes
//! nobody can account for.
//!
//! Ordering and integrity come from TCP underneath, so `ack` is flow control
//! and not retransmission: a gap in seq means the link died, and the transfer
//! fails rather than papering over it.
//!
//! This was written three times — TypeScript, Dart, and very nearly a fourth in
//! Swift. It is written once now, and the pacing logic in particular is the
//! kind that is subtly wrong in a reimplementation and only shows up on a slow
//! link.
//!
//! ponytail: whole file in memory on both ends, capped at [`MAX_BYTES`], and no
//! resume — a transfer that breaks is one you start again, which is what mIRC
//! did too. The ceiling now is memory, not pacing: raising MAX_BYTES means
//! streaming chunks to disk on both ends, not more protocol.

use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::effect::{Effect, Effects};

pub const MAX_BYTES: usize = 10 * 1024 * 1024;
/// Raw bytes per chunk. Base64 inflates this by a third, and the frame is one
/// line of JSON on a socket, so 48 KB keeps a line comfortably under 64 KB.
const CHUNK: usize = 48 * 1024;
/// Chunks allowed in flight unacknowledged. Enough to keep the link busy over a
/// round trip, small enough that the receiver is never more than a few hundred
/// KB behind what the sender believes it has delivered.
const WINDOW: i64 = 8;
/// A peer that has acknowledged nothing for this long is gone. The core has no
/// clock of its own — wasm and iOS do not agree on one — so the host passes the
/// time in, and `tick` is what turns a silence into a failure.
const ACK_TIMEOUT_MS: i64 = 20_000;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Dir {
    Send,
    Recv,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum State {
    Offered,
    Running,
    Done,
    Declined,
    Failed,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Transfer {
    pub id: String,
    pub dir: Dir,
    /// the other end
    pub peer: String,
    pub peer_nick: String,
    pub chan: String,
    pub name: String,
    pub mime: String,
    pub size: usize,
    pub bytes: usize,
    pub state: State,
    #[serde(default)]
    pub error: Option<String>,
    /// Sender side: bytes the RECEIVER has confirmed. `bytes` is what went on
    /// the wire, which is always a little ahead; this is what actually arrived,
    /// and it is what the progress bar shows so the bar never lies.
    pub acked: usize,
    /// Host wall-clock of the last sign of life, for [`Transfers::tick`].
    #[serde(skip)]
    last_ms: i64,
}

impl Transfer {
    /// What the bar should show. See the note on `acked`.
    pub fn delivered(&self) -> usize {
        if self.dir == Dir::Send {
            self.acked
        } else {
            self.bytes
        }
    }
}

pub fn chunk_count(size: usize) -> i64 {
    if size == 0 {
        1
    } else {
        size.div_ceil(CHUNK) as i64
    }
}

#[derive(Default)]
pub struct Transfers {
    /// every transfer this session has seen, newest last
    pub list: Vec<Transfer>,
    outgoing: Vec<(String, Vec<u8>)>,
    incoming: Vec<(String, Vec<u8>)>,
    /// highest chunk seq the receiver has confirmed, per transfer
    acked_seq: Vec<(String, i64)>,
    /// receiver side: the next seq we expect, so a gap is caught, not ignored
    expect_seq: Vec<(String, i64)>,
    /// sender side: the next chunk to put on the wire
    next_seq: Vec<(String, i64)>,
    /// Transfers that reached Done since the last drain, so the client can put
    /// a line in the room's scrollback. Recorded rather than pushed as an
    /// effect: this module knows about bytes, not about events.
    done: Vec<String>,
}

fn get<'a, T>(v: &'a [(String, T)], id: &str) -> Option<&'a T> {
    v.iter().find(|(k, _)| k == id).map(|(_, x)| x)
}
fn set<T>(v: &mut Vec<(String, T)>, id: &str, val: T) {
    match v.iter_mut().find(|(k, _)| k == id) {
        Some(slot) => slot.1 = val,
        None => v.push((id.to_string(), val)),
    }
}
fn drop_key<T>(v: &mut Vec<(String, T)>, id: &str) {
    v.retain(|(k, _)| k != id);
}

impl Transfers {
    fn find_mut(&mut self, id: &str) -> Option<&mut Transfer> {
        self.list.iter_mut().find(|t| t.id == id)
    }
    fn find(&self, id: &str) -> Option<&Transfer> {
        self.list.iter().find(|t| t.id == id)
    }

    /// Anything still worth showing in the window.
    pub fn active(&self) -> Vec<&Transfer> {
        self.list.iter().filter(|t| matches!(t.state, State::Offered | State::Running)).collect()
    }

    /// Offer a file to one peer. Nothing moves until they accept.
    pub fn offer(
        &mut self,
        peer: &str,
        peer_nick: &str,
        chan: &str,
        name: &str,
        mime: &str,
        bytes: Vec<u8>,
        now_ms: i64,
        fx: &mut Effects,
    ) -> Result<(), String> {
        if bytes.len() > MAX_BYTES {
            return Err(format!(
                "{name} is {:.1} MB — nearby transfers stop at {} MB",
                bytes.len() as f64 / 1048576.0,
                MAX_BYTES / 1048576
            ));
        }
        let id = format!("x_{}", &crate::crypto::uuid4()[..8]);
        let size = bytes.len();
        self.outgoing.push((id.clone(), bytes));
        self.list.push(Transfer {
            id: id.clone(),
            dir: Dir::Send,
            peer: peer.into(),
            peer_nick: peer_nick.into(),
            chan: chan.into(),
            name: name.into(),
            mime: mime.into(),
            size,
            bytes: 0,
            state: State::Offered,
            error: None,
            acked: 0,
            last_ms: now_ms,
        });
        send_to(fx, peer, "file.offer", json!({
            "id": id, "chan": chan, "name": name, "size": size, "mime": mime
        }));
        Ok(())
    }

    /// Accept an offer someone made us.
    pub fn accept(&mut self, id: &str, fx: &mut Effects) {
        let Some(t) = self.find_mut(id) else { return };
        if t.dir != Dir::Recv || t.state != State::Offered {
            return;
        }
        t.state = State::Running;
        let peer = t.peer.clone();
        self.incoming.push((id.to_string(), Vec::new()));
        set(&mut self.expect_seq, id, 0);
        send_to(fx, &peer, "file.accept", json!({ "id": id }));
    }

    pub fn decline(&mut self, id: &str, reason: &str, fx: &mut Effects) {
        let Some(t) = self.find_mut(id) else { return };
        t.state = State::Declined;
        let peer = t.peer.clone();
        self.cleanup(id);
        send_to(fx, &peer, "file.cancel", json!({ "id": id, "reason": reason }));
    }

    fn cleanup(&mut self, id: &str) {
        drop_key(&mut self.incoming, id);
        drop_key(&mut self.outgoing, id);
        drop_key(&mut self.expect_seq, id);
        drop_key(&mut self.acked_seq, id);
        drop_key(&mut self.next_seq, id);
    }

    /// A frame arrived from `from` (peer id, with their nick for the window).
    pub fn on_frame(
        &mut self,
        from: &str,
        nick: &str,
        t: &str,
        d: &Value,
        now_ms: i64,
        fx: &mut Effects,
    ) {
        let id = d.get("id").and_then(Value::as_str).unwrap_or_default().to_string();
        if let Some(x) = self.find_mut(&id) {
            x.last_ms = now_ms;
        }
        match t {
            "file.offer" => {
                if self.find(&id).is_some() {
                    return;
                }
                let size = d.get("size").and_then(Value::as_u64).unwrap_or(u64::MAX) as usize;
                if size > MAX_BYTES {
                    send_to(fx, from, "file.cancel", json!({ "id": id, "reason": "too large" }));
                    return;
                }
                self.list.push(Transfer {
                    id,
                    dir: Dir::Recv,
                    peer: from.into(),
                    peer_nick: nick.into(),
                    chan: str_of(d, "chan"),
                    name: some_or(str_of(d, "name"), "file"),
                    mime: some_or(str_of(d, "mime"), "application/octet-stream"),
                    size,
                    bytes: 0,
                    state: State::Offered,
                    error: None,
                    acked: 0,
                    last_ms: now_ms,
                });
            }

            "file.accept" => {
                let running = match self.find_mut(&id) {
                    Some(x) if x.dir == Dir::Send => {
                        x.state = State::Running;
                        true
                    }
                    _ => false,
                };
                if running {
                    set(&mut self.acked_seq, &id, -1);
                    set(&mut self.next_seq, &id, 0);
                    self.pump(&id, fx);
                }
            }

            "file.chunk" => {
                let seq = d.get("seq").and_then(Value::as_i64).unwrap_or(-1);
                let want = *get(&self.expect_seq, &id).unwrap_or(&0);
                let ok = matches!(self.find(&id), Some(x) if x.state == State::Running)
                    && get(&self.incoming, &id).is_some();
                if !ok {
                    return;
                }
                if seq != want {
                    // TCP does not reorder, so a gap means the link dropped
                    // frames or a sender is confused. Either way the file would
                    // be wrong; say so instead of assembling something corrupt
                    // and calling it done.
                    let why = format!("chunk {seq} arrived where {want} was expected");
                    if let Some(x) = self.find_mut(&id) {
                        x.state = State::Failed;
                        x.error = Some(why.clone());
                    }
                    self.cleanup(&id);
                    send_to(fx, from, "file.cancel", json!({ "id": id, "reason": why }));
                    return;
                }
                let part = B64.decode(str_of(d, "data")).unwrap_or_default();
                if let Some(slot) = self.incoming.iter_mut().find(|(k, _)| *k == id) {
                    slot.1.extend_from_slice(&part);
                }
                if let Some(x) = self.find_mut(&id) {
                    x.bytes += part.len();
                }
                set(&mut self.expect_seq, &id, want + 1);
                // The pace signal. Sent once the bytes are safely in our buffer,
                // so it means "landed", not "seen".
                send_to(fx, from, "file.ack", json!({ "id": id, "seq": seq }));
            }

            "file.ack" => {
                let seq = d.get("seq").and_then(Value::as_i64).unwrap_or(-1);
                let is_send = matches!(self.find(&id), Some(x) if x.dir == Dir::Send);
                if !is_send {
                    return;
                }
                set(&mut self.acked_seq, &id, seq);
                if let Some(x) = self.find_mut(&id) {
                    x.acked = (((seq + 1) as usize) * CHUNK).min(x.size);
                }
                // An ack opens the window: put the next chunks on the wire.
                self.pump(&id, fx);
            }

            "file.done" => {
                let Some(buf) = get(&self.incoming, &id).cloned() else { return };
                let Some(x) = self.find_mut(&id) else { return };
                if buf.len() != x.size {
                    x.state = State::Failed;
                    x.error = Some(format!("expected {} bytes, got {}", x.size, buf.len()));
                } else {
                    x.state = State::Done;
                    x.bytes = buf.len();
                    let (name, mime) = (x.name.clone(), x.mime.clone());
                    fx.push(Effect::SaveFile { name, mime, bytes: buf });
                    self.done.push(id.clone());
                }
                drop_key(&mut self.incoming, &id);
            }

            "file.cancel" => {
                let Some(x) = self.find_mut(&id) else { return };
                if x.state == State::Done {
                    return;
                }
                x.state = if x.dir == Dir::Send { State::Declined } else { State::Failed };
                x.error = Some(some_or(str_of(d, "reason"), "cancelled"));
                self.cleanup(&id);
            }
            _ => {}
        }
    }

    /// Put chunks on the wire up to the window, then stop and wait for acks.
    ///
    /// This is a step function rather than a loop with a sleep in it: the core
    /// has no clock and no runtime, so "wait" means "return, and be called
    /// again when an ack arrives". That is what `file.ack` calling back into
    /// pump is for.
    fn pump(&mut self, id: &str, fx: &mut Effects) {
        let Some(t) = self.find(id) else { return };
        if t.state != State::Running {
            return;
        }
        let (peer, size) = (t.peer.clone(), t.size);
        let total = chunk_count(size);
        let acked = *get(&self.acked_seq, id).unwrap_or(&-1);
        let mut seq = *get(&self.next_seq, id).unwrap_or(&0);

        while seq < total && seq - acked <= WINDOW {
            let Some(bytes) = get(&self.outgoing, id) else { return };
            let off = seq as usize * CHUNK;
            let end = (off + CHUNK).min(bytes.len());
            let slice = B64.encode(&bytes[off..end]);
            send_to(fx, &peer, "file.chunk", json!({ "id": id, "seq": seq, "data": slice }));
            if let Some(x) = self.find_mut(id) {
                x.bytes = end;
            }
            seq += 1;
        }
        set(&mut self.next_seq, id, seq);

        // Everything must be acknowledged before we claim it is delivered.
        if seq >= total && acked >= total - 1 {
            send_to(fx, &peer, "file.done", json!({ "id": id }));
            if let Some(x) = self.find_mut(id) {
                x.state = State::Done;
                x.acked = x.size;
            }
            self.done.push(id.to_string());
            self.cleanup(id);
        }
    }

    /// Transfers that completed since this was last called. The client turns
    /// each into a `file` line; draining is what stops it posting twice.
    pub fn take_done(&mut self) -> Vec<String> {
        std::mem::take(&mut self.done)
    }

    /// A link died. Everything still moving on it fails — silence is not a
    /// state a transfer window may sit in forever.
    pub fn peer_gone(&mut self, peer: &str) {
        for t in self.list.iter_mut() {
            if t.peer == peer && matches!(t.state, State::Offered | State::Running) {
                t.state = State::Failed;
                t.error = Some("the link to that peer closed".into());
            }
        }
    }

    /// Fail anything the other side has gone quiet on. A running transfer that
    /// stops being acknowledged would otherwise sit at a percentage forever,
    /// which reads as "still going" and is not.
    pub fn tick(&mut self, now_ms: i64) -> bool {
        let mut stale = Vec::new();
        for t in self.list.iter_mut() {
            if t.state == State::Running && now_ms - t.last_ms > ACK_TIMEOUT_MS {
                t.state = State::Failed;
                t.error = Some("the other side stopped acknowledging".into());
                stale.push(t.id.clone());
            }
        }
        for id in &stale {
            self.cleanup(id);
        }
        !stale.is_empty()
    }
}

fn send_to(fx: &mut Effects, peer: &str, t: &str, d: Value) {
    if let Ok(json) = serde_json::to_string(&d) {
        fx.push(Effect::MeshSendTo { peer: peer.into(), t: t.into(), json });
    }
}

fn str_of(d: &Value, key: &str) -> String {
    d.get(key).and_then(Value::as_str).unwrap_or_default().to_string()
}

fn some_or(s: String, fallback: &str) -> String {
    if s.is_empty() {
        fallback.to_string()
    } else {
        s
    }
}
