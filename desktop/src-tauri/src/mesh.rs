// Nearby mesh (spec F9): serverless ad-hoc channels over the local network.
//
// Two moving parts:
//
//   discovery  Bonjour/mDNS. We register `_gliph._tcp` with our id, nick and
//              channels in the TXT record, and browse for everyone else's.
//   links      one TCP connection per peer pair, newline-delimited JSON.
//
// Discovery is mDNS and not a UDP beacon of our own because of iOS: raw
// multicast there needs Apple's com.apple.developer.networking.multicast
// entitlement (an application, not a checkbox), while Bonjour needs only the
// local-network prompt. The iPhone plugin therefore speaks Bonjour, and a Mac
// that spoke anything else would be in its own private mesh — which is not a
// mesh. Same service type, same TXT keys, same link frames: see
// mobile/Sources/Mesh.swift.
//
// Full mesh, no relaying: everyone on a LAN can reach everyone, so a message
// goes out on every link and nowhere else. That is also the whole reason this
// is not a routing protocol — see docs/protocol.md "Nearby mesh".
//
// ponytail: no multi-hop, no store-and-forward, no persistence. A peer that was
// not connected when a line was sent never sees it, and when the last peer
// leaves, the channel is gone. That is the spec's transient store, not a gap.
// Multi-hop is parked in gliph-spec.md's "not building" list.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::net::{IpAddr, Ipv4Addr, SocketAddr, TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use mdns_sd::{ServiceDaemon, ServiceEvent, ServiceInfo};
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Emitter};

/// The service every Gliph on the network registers and browses for.
pub const SERVICE: &str = "_gliph._tcp.local.";
/// Re-announce this often: the TXT record carries our channel list, and mDNS
/// has no "the record changed" push that every stack honours.
const ANNOUNCE_EVERY: Duration = Duration::from_secs(5);

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct Msg {
    pub id: String,
    pub chan: String,
    pub nick: String,
    pub body: String,
    pub ts: String,
    /// `message` or `action` (/me), and the id this answers. Both default, so a
    /// peer on an older build — which sends neither — is still a peer.
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub reply_to: String,
}

/// A frame on a peer link. Same envelope shape as the server protocol so the
/// client's reflexes carry over: `{"t":...,"d":{...}}`, one per line.
///
/// Only the three frames the mesh itself needs are typed here. Everything else
/// is forwarded to the client as-is (`mesh:frame`) and sent from it with
/// `mesh_send_to` — file transfer is built entirely up there, once, instead of
/// twice in two languages that would have to agree.
#[derive(Serialize, Deserialize, Clone, Debug)]
#[serde(tag = "t", content = "d")]
pub enum Frame {
    #[serde(rename = "hi")]
    Hi { id: String, nick: String, chans: Vec<String> },
    #[serde(rename = "chans")]
    Chans { id: String, chans: Vec<String> },
    #[serde(rename = "msg")]
    Msg(Msg),
}

/// A frame the mesh does not interpret, on its way up to the client.
#[derive(Serialize, Clone, Debug)]
pub struct Passthrough {
    pub from: String,
    pub nick: String,
    pub t: String,
    pub d: serde_json::Value,
}

#[derive(Serialize, Clone, Debug)]
pub struct PeerView {
    pub id: String,
    pub nick: String,
    pub chans: Vec<String>,
}

struct Peer {
    nick: String,
    chans: Vec<String>,
    addr: SocketAddr,
    seen: std::time::Instant,
    /// write half of the link, if one is up
    link: Option<TcpStream>,
}

pub struct Mesh {
    pub id: String,
    nick: Mutex<String>,
    chans: Mutex<Vec<String>>,
    peers: Mutex<HashMap<String, Peer>>,
    port: u16,
    stopped: AtomicBool,
    app: Option<AppHandle>,
    /// Received messages, kept only under test: with no AppHandle there is
    /// nowhere for a frame to go, and "the link is up" is a weaker claim than
    /// "the line arrived".
    #[cfg(test)]
    inbox: Mutex<Vec<Msg>>,
    /// Uninterpreted frames, under test only — the interop harness needs to see
    /// the accept come back before it starts pushing bytes.
    #[cfg(test)]
    frames: Mutex<Vec<Passthrough>>,
}

static MESH: OnceLock<Mutex<Option<Arc<Mesh>>>> = OnceLock::new();
fn slot() -> &'static Mutex<Option<Arc<Mesh>>> {
    MESH.get_or_init(|| Mutex::new(None))
}

// Both sides dial. There WAS a rule here that only the lower id dialled, so a
// pair held exactly one link — until an iPhone showed what that costs: every
// one of its outbound connections stalled in Bonjour name resolution
// (`resolver:children_stall`, one per retry, forever) while the Mac could reach
// it on the first try. A rule that nominates one dialler makes that pair's
// entire conversation depend on the leg that happens to be broken.
//
// So: everyone dials everyone, the first link to land is the one we send on,
// and any second socket is simply read from. Duplicate delivery is free to
// tolerate — every message carries an id and the client already drops one it
// has seen (client.ts, ingestMesh).

/// Half-second browse ticks between re-asking mDNS (~8s). It has to be shorter
/// than PEER_TTL: a resolve event is what refreshes a peer's `seen`, and
/// without a fresh one every live peer we have not linked to would age out and
/// only come back if the network happened to re-announce it. That is exactly
/// how two of three peers went invisible while the third stayed.
const REBROWSE_TICKS: u32 = 16;

/// Three missed announcements and a peer is gone, unless a link is still up.
const PEER_TTL: Duration = Duration::from_secs(16);

fn io_err<E: std::fmt::Display>(e: E) -> std::io::Error {
    std::io::Error::other(e.to_string())
}

fn rand_id() -> String {
    let mut b = [0u8; 6];
    getrandom::fill(&mut b).expect("getrandom");
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn now_iso() -> String {
    // ponytail: no chrono. The only consumer is the client's timestamp column,
    // which parses ISO-8601, and SystemTime can produce one with arithmetic.
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let (days, rem) = ((secs / 86400) as i64, secs % 86400);
    let (h, mi, s) = (rem / 3600, (rem % 3600) / 60, rem % 60);
    // civil-from-days, Howard Hinnant's algorithm
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!("{y:04}-{m:02}-{d:02}T{h:02}:{mi:02}:{s:02}Z")
}

impl Mesh {
    pub fn start(nick: String, app: Option<AppHandle>) -> std::io::Result<Arc<Mesh>> {
        let listener = TcpListener::bind((Ipv4Addr::UNSPECIFIED, 0))?;
        let port = listener.local_addr()?.port();
        let mesh = Arc::new(Mesh {
            id: rand_id(),
            nick: Mutex::new(nick),
            chans: Mutex::new(Vec::new()),
            peers: Mutex::new(HashMap::new()),
            port,
            stopped: AtomicBool::new(false),
            app,
            #[cfg(test)]
            inbox: Mutex::new(Vec::new()),
            #[cfg(test)]
            frames: Mutex::new(Vec::new()),
        });

        let mdns = ServiceDaemon::new().map_err(io_err)?;
        mesh.announce(&mdns);

        let browser = mdns.clone();
        spawn(&mesh, "mesh-accept", move |m| m.accept_loop(listener));
        spawn(&mesh, "mesh-browse", move |m| m.browse_loop(browser));
        spawn(&mesh, "mesh-announce", move |m| m.announce_loop(mdns));
        Ok(mesh)
    }

    pub fn stop(&self) {
        self.stopped.store(true, Ordering::Relaxed);
        let mut peers = self.peers.lock().unwrap();
        for (_, p) in peers.iter() {
            if let Some(l) = &p.link {
                let _ = l.shutdown(std::net::Shutdown::Both);
            }
        }
        peers.clear();
    }

    fn running(&self) -> bool {
        !self.stopped.load(Ordering::Relaxed)
    }

    /// Joining or leaving a nearby channel changes both what we tell our linked
    /// peers and what a peer who has not linked yet reads off our TXT record.
    /// The next announce (within ANNOUNCE_EVERY) carries the new list.
    pub fn set_chans(&self, chans: Vec<String>) {
        *self.chans.lock().unwrap() = chans.clone();
        let me = self.id.clone();
        self.broadcast(&Frame::Chans { id: me, chans });
    }

    pub fn nick(&self) -> String {
        self.nick.lock().unwrap().clone()
    }

    /// Publish who we are. The TXT record is the beacon: id, nick, and the
    /// channels we are in, so a peer knows what to show before any TCP link
    /// exists.
    fn announce(&self, mdns: &ServiceDaemon) {
        let chans = self.chans.lock().unwrap().join(",");
        let props = [
            ("id", self.id.as_str()),
            ("nick", &self.nick()),
            ("chans", chans.as_str()),
        ];
        let info = match ServiceInfo::new(
            SERVICE,
            &self.id,          // instance name: unique per device by construction
            &format!("{}.local.", self.id),
            (),                // let mdns-sd fill in this host's addresses
            self.port,
            &props[..],
        ) {
            Ok(i) => i.enable_addr_auto(),
            Err(e) => {
                log::warn!("mesh: cannot build service info: {e}");
                return;
            }
        };
        if let Err(e) = mdns.register(info) {
            log::warn!("mesh: cannot register {SERVICE}: {e}");
        }
    }

    fn announce_loop(&self, mdns: ServiceDaemon) {
        while self.running() {
            std::thread::sleep(ANNOUNCE_EVERY);
            if !self.running() {
                break;
            }
            self.announce(&mdns);
            self.expire_peers();
        }
        let _ = mdns.unregister(&format!("{}.{}", self.id, SERVICE));
        let _ = mdns.shutdown();
    }

    fn browse_loop(self: &Arc<Self>, mdns: ServiceDaemon) {
        let mut browse = match mdns.browse(SERVICE) {
            Ok(b) => b,
            Err(e) => { log::warn!("mesh: cannot browse: {e}"); return; }
        };
        let mut ticks = 0u32;
        while self.running() {
            let ev = match browse.recv_timeout(Duration::from_millis(500)) {
                Ok(ev) => ev,
                Err(_) => {
                    // Ask mDNS again, always — not only when we know nobody.
                    // A browse only reports CHANGES, so a peer discovered once
                    // and never linked to would go quiet, age past PEER_TTL and
                    // vanish while it is sitting right there advertising.
                    ticks += 1;
                    if ticks >= REBROWSE_TICKS {
                        ticks = 0;
                        let _ = mdns.stop_browse(SERVICE);
                        if let Ok(b) = mdns.browse(SERVICE) {
                            browse = b;
                        }
                        // And try again to reach anyone we are supposed to dial.
                        // One failed connect at discovery time should not mean
                        // that peer is unreachable for the rest of the flight.
                        self.redial();
                    }
                    continue;
                }
            };
            ticks = 0;
            match ev {
                ServiceEvent::ServiceResolved(info) => {
                    let txt = |k: &str| {
                        info.get_property_val_str(k).unwrap_or_default().to_string()
                    };
                    let id = txt("id");
                    if id.is_empty() || id == self.id {
                        continue;
                    }
                    let nick = txt("nick");
                    let chans: Vec<String> = txt("chans")
                        .split(',')
                        .filter(|c| !c.is_empty())
                        .map(|c| c.to_string())
                        .collect();
                    // Prefer IPv4: a link-local IPv6 address needs a scope id
                    // that does not survive the round trip through mdns-sd.
                    let addrs = info.get_addresses();
                    let Some(ip) = addrs.iter().find(|a| a.is_ipv4()).or_else(|| addrs.iter().next()).copied()
                    else { continue };
                    let addr = SocketAddr::new(ip, info.get_port());
                    let dial = {
                        let mut peers = self.peers.lock().unwrap();
                        match peers.get_mut(&id) {
                            Some(p) => {
                                p.seen = std::time::Instant::now();
                                p.nick = nick;
                                p.chans = chans;
                                p.addr = addr;
                                p.link.is_none()
                            }
                            None => {
                                peers.insert(id.clone(), Peer {
                                    nick, chans, addr,
                                    seen: std::time::Instant::now(),
                                    link: None,
                                });
                                true
                            }
                        }
                    };
                    self.emit_peers();
                    if dial {
                        self.dial(id, addr);
                    }
                }
                ServiceEvent::ServiceRemoved(_, fullname) => {
                    // fullname is "<instance>.<service>", and our instance name
                    // IS the peer id — that is why it is the id and not the nick.
                    let id = fullname.split('.').next().unwrap_or("").to_string();
                    let mut gone = false;
                    if let Some(p) = self.peers.lock().unwrap().remove(&id) {
                        if let Some(l) = p.link {
                            let _ = l.shutdown(std::net::Shutdown::Both);
                        }
                        gone = true;
                    }
                    if gone {
                        self.emit_peers();
                    }
                }
                _ => {}
            }
        }
    }

    /// Dial every peer we should be dialling and are not connected to.
    fn redial(self: &Arc<Self>) {
        let todo: Vec<(String, SocketAddr)> = self
            .peers
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, p)| p.link.is_none())
            .map(|(id, p)| (id.clone(), p.addr))
            .collect();
        for (id, addr) in todo {
            self.dial(id, addr);
        }
    }

    fn expire_peers(&self) {
        let mut dropped = false;
        {
            let mut peers = self.peers.lock().unwrap();
            peers.retain(|_, p| {
                // mDNS removals are the primary signal; this only catches a peer
                // whose goodbye never arrived (a laptop lid, a dead battery).
                let alive = p.seen.elapsed() < PEER_TTL || p.link.is_some();
                if !alive {
                    dropped = true;
                    if let Some(l) = &p.link {
                        let _ = l.shutdown(std::net::Shutdown::Both);
                    }
                }
                alive
            });
        }
        if dropped {
            self.emit_peers();
        }
    }

    // ---- links ----

    fn dial(self: &Arc<Self>, id: String, addr: SocketAddr) {
        let me = self.clone();
        std::thread::Builder::new()
            .name("mesh-dial".into())
            .spawn(move || {
                let Ok(stream) = TcpStream::connect_timeout(&addr, Duration::from_secs(3)) else {
                    return;
                };
                me.adopt(Some(id), stream);
            })
            .ok();
    }

    fn accept_loop(self: &Arc<Self>, listener: TcpListener) {
        for stream in listener.incoming() {
            if !self.running() {
                return;
            }
            let Ok(stream) = stream else { continue };
            let me = self.clone();
            std::thread::Builder::new()
                .name("mesh-link".into())
                .spawn(move || me.adopt(None, stream))
                .ok();
        }
    }

    /// Own a fresh link: say hi, then read frames until it dies. `expect` is
    /// set when we dialled and already know who is on the other end.
    fn adopt(self: &Arc<Self>, expect: Option<String>, stream: TcpStream) {
        let _ = stream.set_nodelay(true);
        let hi = Frame::Hi {
            id: self.id.clone(),
            nick: self.nick(),
            chans: self.chans.lock().unwrap().clone(),
        };
        let mut w = stream.try_clone().expect("clone link");
        if write_frame(&mut w, &hi).is_err() {
            return;
        }

        let mut peer_id = expect;
        let reader = BufReader::new(stream.try_clone().expect("clone link"));
        for line in reader.lines() {
            if !self.running() {
                return;
            }
            let Ok(line) = line else { break };
            let Ok(frame) = serde_json::from_str::<Frame>(&line) else {
                // Not one of ours: hand it to the client rather than drop it.
                if let Some(p) = self.passthrough(&line, peer_id.as_deref()) {
                    #[cfg(test)]
                    self.frames.lock().unwrap().push(p.clone());
                    self.emit("mesh:frame", &p);
                }
                continue;
            };
            match frame {
                Frame::Hi { id, nick, chans } => {
                    if id == self.id {
                        break; // our own beacon looped back to us
                    }
                    peer_id = Some(id.clone());
                    let mut peers = self.peers.lock().unwrap();
                    let entry = peers.entry(id.clone()).or_insert_with(|| Peer {
                        nick: nick.clone(),
                        chans: chans.clone(),
                        addr: stream.peer_addr().unwrap_or(SocketAddr::new(
                            IpAddr::V4(Ipv4Addr::UNSPECIFIED),
                            0,
                        )),
                        seen: std::time::Instant::now(),
                        link: None,
                    });
                    entry.nick = nick;
                    entry.chans = chans;
                    entry.seen = std::time::Instant::now();
                    if entry.link.is_none() {
                        entry.link = stream.try_clone().ok();
                    }
                    drop(peers);
                    self.emit_peers();
                }
                Frame::Chans { id, chans } => {
                    if let Some(p) = self.peers.lock().unwrap().get_mut(&id) {
                        p.chans = chans;
                        p.seen = std::time::Instant::now();
                    }
                    self.emit_peers();
                }
                Frame::Msg(m) => {
                    #[cfg(test)]
                    self.inbox.lock().unwrap().push(m.clone());
                    self.emit("mesh:msg", &m);
                }
            }
        }

        if let Some(id) = peer_id {
            if let Some(p) = self.peers.lock().unwrap().get_mut(&id) {
                p.link = None;
            }
            self.emit_peers();
        }
    }

    /// Shape an uninterpreted line into something the client can act on. It
    /// needs to know WHO sent it, which the raw frame does not carry.
    fn passthrough(&self, line: &str, from: Option<&str>) -> Option<Passthrough> {
        let v: serde_json::Value = serde_json::from_str(line).ok()?;
        let t = v.get("t")?.as_str()?.to_string();
        let from = from?.to_string();
        let nick = self
            .peers
            .lock()
            .unwrap()
            .get(&from)
            .map(|p| p.nick.clone())
            .unwrap_or_default();
        Some(Passthrough { from, nick, t, d: v.get("d").cloned().unwrap_or(serde_json::Value::Null) })
    }

    // ---- sending ----

    /// Send an arbitrary frame to ONE peer. File chunks go here: a transfer
    /// concerns two people, and broadcasting it to the room would multiply the
    /// bytes by the number of peers for no reason.
    pub fn send_to(&self, peer: &str, t: &str, d: serde_json::Value) -> Result<(), String> {
        let mut line = serde_json::to_vec(&serde_json::json!({ "t": t, "d": d }))
            .map_err(|e| e.to_string())?;
        line.push(b'\n');
        let mut peers = self.peers.lock().unwrap();
        let p = peers.get_mut(peer).ok_or("no such peer")?;
        let link = p.link.as_mut().ok_or("no link to that peer")?;
        link.write_all(&line).map_err(|e| e.to_string())?;
        link.flush().map_err(|e| e.to_string())
    }

    pub fn say(&self, chan: String, body: String, kind: String, reply_to: String) -> Msg {
        let m = Msg {
            id: format!("n_{}", rand_id()),
            chan,
            nick: self.nick(),
            body,
            ts: now_iso(),
            kind,
            reply_to,
        };
        self.broadcast(&Frame::Msg(m.clone()));
        m
    }

    fn broadcast(&self, f: &Frame) {
        let mut dead = Vec::new();
        {
            let mut peers = self.peers.lock().unwrap();
            for (id, p) in peers.iter_mut() {
                let Some(link) = p.link.as_mut() else { continue };
                if write_frame(link, f).is_err() {
                    dead.push(id.clone());
                }
            }
            for id in &dead {
                if let Some(p) = peers.get_mut(id) {
                    p.link = None;
                }
            }
        }
        if !dead.is_empty() {
            self.emit_peers();
        }
    }

    /// Who is actually nearby — meaning who we hold a LINK to.
    ///
    /// Discovery alone is not evidence of a peer: mDNS is a cache, and a
    /// responder happily keeps handing out records for a laptop that closed
    /// ten minutes ago. Listing those made the member list fill with ghosts of
    /// people who had left. A live TCP connection cannot be stale.
    pub fn peer_views(&self) -> Vec<PeerView> {
        let peers = self.peers.lock().unwrap();
        let mut v: Vec<PeerView> = peers
            .iter()
            .filter(|(_, p)| p.link.is_some())
            .map(|(id, p)| PeerView {
                id: id.clone(),
                nick: p.nick.clone(),
                chans: p.chans.clone(),
            })
            .collect();
        v.sort_by(|a, b| a.nick.cmp(&b.nick));
        v
    }

    fn emit_peers(&self) {
        let peers = self.peer_views();
        self.emit("mesh:peers", &peers);
    }

    fn emit<T: Serialize + Clone>(&self, event: &str, payload: &T) {
        if let Some(app) = &self.app {
            let _ = app.emit(event, payload.clone());
        }
    }
}

fn spawn<F>(mesh: &Arc<Mesh>, name: &str, f: F)
where
    F: FnOnce(&Arc<Mesh>) + Send + 'static,
{
    let me = mesh.clone();
    std::thread::Builder::new()
        .name(name.into())
        .spawn(move || f(&me))
        .ok();
}

fn write_frame(w: &mut TcpStream, f: &Frame) -> std::io::Result<()> {
    let mut line = serde_json::to_vec(f)?;
    line.push(b'\n');
    w.write_all(&line)?;
    w.flush()
}

// ---- Tauri commands ----

#[derive(Serialize)]
pub struct Started {
    pub id: String,
}

#[tauri::command]
pub fn mesh_start(app: AppHandle, nick: String) -> Result<Started, String> {
    let mut cur = slot().lock().unwrap();
    if let Some(m) = cur.as_ref() {
        return Ok(Started { id: m.id.clone() });
    }
    let m = Mesh::start(nick, Some(app)).map_err(|e| e.to_string())?;
    let id = m.id.clone();
    *cur = Some(m);
    Ok(Started { id })
}

#[tauri::command]
pub fn mesh_stop() {
    if let Some(m) = slot().lock().unwrap().take() {
        m.stop();
    }
}

#[tauri::command]
pub fn mesh_join(chans: Vec<String>) {
    if let Some(m) = slot().lock().unwrap().as_ref() {
        m.set_chans(chans);
    }
}

#[tauri::command]
pub fn mesh_send(chan: String, body: String, kind: String, reply_to: String) -> Result<Msg, String> {
    let cur = slot().lock().unwrap();
    let m = cur.as_ref().ok_or("nearby mode is not running")?;
    Ok(m.say(chan, body, kind, reply_to))
}

#[tauri::command]
pub fn mesh_send_to(peer: String, t: String, d: serde_json::Value) -> Result<(), String> {
    let cur = slot().lock().unwrap();
    let m = cur.as_ref().ok_or("nearby mode is not running")?;
    m.send_to(&peer, &t, d)
}

#[tauri::command]
pub fn mesh_peers() -> Vec<PeerView> {
    slot()
        .lock()
        .unwrap()
        .as_ref()
        .map(|m| m.peer_views())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Pins the rule that replaced "only the lower id dials": a peer we have no
    /// link to is dialled, whoever it is. If this ever narrows again, a pair
    /// whose dial stalls on one side goes silent — which is exactly the bug it
    /// was written for.
    #[test]
    fn every_unlinked_peer_is_dialled() {
        let m = Mesh::start("dialler".into(), None).expect("mesh");
        {
            let mut peers = m.peers.lock().unwrap();
            for id in ["0000aaaa0000", "ffffbbbbffff"] {
                peers.insert(id.into(), Peer {
                    nick: id.into(), chans: vec![],
                    addr: "127.0.0.1:9".parse().unwrap(),
                    seen: std::time::Instant::now(), link: None,
                });
            }
        }
        let unlinked = m.peers.lock().unwrap().values().filter(|p| p.link.is_none()).count();
        assert_eq!(unlinked, 2, "both ids — one below ours and one above — are ours to dial");
        m.stop();
    }

    #[test]
    fn frames_round_trip_as_one_line() {
        // A reply's body ALWAYS contains a newline — the quote line is
        // delimited by one — so this is no longer a hypothetical.
        let f = Frame::Msg(Msg {
            id: "n_1".into(),
            chan: "general".into(),
            nick: "waseem".into(),
            body: "> <robin> hello\nthere".into(),
            ts: now_iso(),
            kind: "message".into(),
            reply_to: "n_0".into(),
        });
        let mut line = serde_json::to_vec(&f).unwrap();
        line.push(b'\n');
        assert_eq!(line.iter().filter(|c| **c == b'\n').count(), 1, "a body newline must not split the frame");
        let back: Frame = serde_json::from_slice(&line[..line.len() - 1]).unwrap();
        match back {
            Frame::Msg(m) => {
                assert_eq!(m.body, "> <robin> hello\nthere");
                assert_eq!(m.reply_to, "n_0");
            }
            _ => panic!("wrong frame"),
        }
    }

    // A mesh is mixed the moment one device updates. Neither direction may
    // drop a peer over a field the other has never heard of.
    #[test]
    fn a_frame_from_an_older_peer_still_parses() {
        let old = br#"{"t":"msg","d":{"id":"n_1","chan":"general","nick":"robin","body":"hi","ts":"2027-01-15T12:00:00Z"}}"#;
        match serde_json::from_slice::<Frame>(old).expect("an older peer is still a peer") {
            Frame::Msg(m) => {
                assert_eq!(m.body, "hi");
                assert_eq!(m.kind, "", "absent reads as absent, and the client defaults it");
                assert_eq!(m.reply_to, "");
            }
            _ => panic!("wrong frame"),
        }
    }

    #[test]
    fn timestamps_are_iso8601() {
        let ts = now_iso();
        assert_eq!(ts.len(), 20, "{ts}");
        assert!(ts.ends_with('Z'));
        assert!(ts.contains('T'));
    }

    /// A frame the mesh does not know about must reach the client instead of
    /// being dropped — that passthrough is the whole transport for file
    /// transfer, which is implemented once in TypeScript rather than twice.
    #[test]
    fn unknown_frames_are_forwarded_not_dropped() {
        let m = Mesh::start("host".into(), None).expect("mesh");
        m.peers.lock().unwrap().insert("aaaa11112222".into(), Peer {
            nick: "sender".into(), chans: vec!["nearby".into()],
            addr: "127.0.0.1:9".parse().unwrap(),
            seen: std::time::Instant::now(), link: None,
        });
        let line = r#"{"t":"file.chunk","d":{"id":"f_1","seq":3,"data":"aGk="}}"#;
        let p = m.passthrough(line, Some("aaaa11112222")).expect("forwarded");
        assert_eq!(p.t, "file.chunk");
        assert_eq!(p.nick, "sender", "the client needs to know who sent it");
        assert_eq!(p.d["seq"], 3);
        assert!(m.passthrough(line, None).is_none(), "a frame from nobody is not actionable");
        m.stop();
    }

    /// Offer a real file to a real phone, by hand:
    ///
    ///   cargo test -- --ignored --nocapture offer_a_file_to_the_phone
    ///
    /// Put the phone in nearby mode first. It offers a file, waits for the
    /// transfer window's accept, then streams it in 48 KB chunks the way
    /// xfer.ts does. Proves the passthrough carries a transfer end to end,
    /// which no in-process test can: the receiving half is TypeScript on iOS.
    #[test]
    #[ignore]
    fn offer_a_file_to_the_phone() {
        use base64::Engine;
        let m = Mesh::start("mac".into(), None).expect("mesh");
        m.set_chans(vec!["nearby".into()]);

        let mut peer = None;
        for _ in 0..60 {
            std::thread::sleep(Duration::from_millis(500));
            if let Some(p) = m.peer_views().into_iter().next() {
                peer = Some(p);
                break;
            }
        }
        let peer = peer.expect("no peer — is the phone in nearby mode?");
        println!("offering to {} [{}]", peer.nick, peer.id);

        let body: Vec<u8> = (0..140 * 1024).map(|i| (i % 251) as u8).collect();
        let id = format!("x_{}", rand_id());
        m.send_to(&peer.id, "file.offer", serde_json::json!({
            "id": id, "chan": "nearby", "name": "flight-plan.txt",
            "size": body.len(), "mime": "text/plain",
        })).expect("offer");

        println!("offered — accept it on the phone");
        let mut accepted = false;
        for _ in 0..120 {
            std::thread::sleep(Duration::from_millis(500));
            let seen = m.frames.lock().unwrap();
            if seen.iter().any(|f| f.t == "file.accept" && f.d["id"] == serde_json::json!(id)) {
                accepted = true;
                break;
            }
        }
        assert!(accepted, "no accept came back within a minute");

        // 48 KB raw per chunk, the same split xfer.ts uses, and paced the same
        // way: never more than WINDOW chunks ahead of what has been acked.
        const WINDOW: i64 = 8;
        let acked_through = |m: &Arc<Mesh>| -> i64 {
            m.frames
                .lock()
                .unwrap()
                .iter()
                .filter(|f| f.t == "file.ack" && f.d["id"] == serde_json::json!(id))
                .filter_map(|f| f.d["seq"].as_i64())
                .max()
                .unwrap_or(-1)
        };
        let total = body.chunks(48 * 1024).count();
        for (seq, chunk) in body.chunks(48 * 1024).enumerate() {
            let mut waited = 0;
            while acked_through(&m) < seq as i64 - WINDOW {
                std::thread::sleep(Duration::from_millis(50));
                waited += 1;
                assert!(waited < 400, "receiver stopped acknowledging at chunk {seq}");
            }
            let data = base64::engine::general_purpose::STANDARD.encode(chunk);
            m.send_to(&peer.id, "file.chunk", serde_json::json!({ "id": id, "seq": seq, "data": data }))
                .expect("chunk");
        }
        // Delivered means acknowledged, not "written to a socket".
        let mut waited = 0;
        while acked_through(&m) < total as i64 - 1 {
            std::thread::sleep(Duration::from_millis(50));
            waited += 1;
            assert!(waited < 400, "last chunks never acknowledged");
        }
        m.send_to(&peer.id, "file.done", serde_json::json!({ "id": id })).expect("done");
        println!("sent {} bytes in {} chunks, all acknowledged", body.len(), total);
        std::thread::sleep(Duration::from_secs(5));
        m.stop();
    }

    /// Cross-platform interop, run by hand because the other end is a phone:
    ///
    ///   cargo test -- --ignored --nocapture join_the_real_mesh
    ///
    /// Put an iPhone (or the simulator) into nearby mode while this runs. It
    /// proves the Swift plugin and this module are in ONE mesh — same Bonjour
    /// service, same TXT keys, same link frames — which no in-process test can.
    #[test]
    #[ignore]
    fn join_the_real_mesh() {
        let m = Mesh::start("rust-peer".into(), None).expect("mesh");
        m.set_chans(vec!["nearby".into()]);
        let mut seen = Vec::new();
        let mut linked = false;
        for i in 0..120 {
            std::thread::sleep(Duration::from_millis(500));
            seen = m.peer_views();
            linked = m.peers.lock().unwrap().values().any(|p| p.link.is_some());
            if i % 4 == 0 || linked {
                println!("peers: {seen:?} linked={linked}");
            }
            // Discovery is not a link: a peer appears in Bonjour a beat before
            // the TCP connection exists, and sending into no link is a message
            // that quietly goes nowhere.
            if linked {
                m.set_chans(vec!["nearby".into()]);
                std::thread::sleep(Duration::from_millis(300));
                let sent = m.say("nearby".into(), "hello from the Mac".into(), "message".into(), String::new());
                println!("sent {sent:?}");
                std::thread::sleep(Duration::from_secs(3));
                break;
            }
        }
        m.stop();
        assert!(!seen.is_empty(), "no peers found — is the phone in nearby mode on this Wi-Fi?");
        assert!(linked, "found {seen:?} but never linked — the TCP side is where to look");
    }

    /// Screenshot fixture. Not a test — it puts a few peers on the network and
    /// has them talk, so the App Store screenshots show a room with people in
    /// it instead of an empty one:
    ///
    ///   cargo test -- --ignored --nocapture staged_conversation
    ///
    /// Put a phone into nearby mode first. It holds the peers up for a while so
    /// there is time to capture; the lines are ordinary and deliberately so.
    #[test]
    #[ignore]
    fn staged_conversation() {
        let script = [
            ("meera", "boarding now, seat 14c"),
            ("leena", "we are at 30,000 feet and this still works"),
            ("amina", "no wifi, no signal, no account. wild"),
        ];
        let peers: Vec<Arc<Mesh>> = script
            .iter()
            .map(|(nick, _)| {
                let m = Mesh::start((*nick).into(), None).expect("mesh");
                m.set_chans(vec!["nearby".into()]);
                m
            })
            .collect();

        // Let discovery and the links settle before anyone speaks: a line sent
        // into no link is a line nobody hears. The link that matters is the one
        // to the PHONE — these three find each other in milliseconds, and
        // waiting on "has any link" is a check that passes without it.
        let staged: Vec<&str> = script.iter().map(|(n, _)| *n).collect();
        let linked_to_phone = |m: &Arc<Mesh>| {
            m.peers
                .lock()
                .unwrap()
                .values()
                .any(|p| p.link.is_some() && !staged.contains(&p.nick.as_str()))
        };
        for i in 0..60 {
            std::thread::sleep(Duration::from_millis(500));
            if peers.iter().all(linked_to_phone) {
                println!("all {} peers linked to the phone", peers.len());
                break;
            }
            if i % 6 == 0 {
                for m in &peers {
                    let ps = m.peers.lock().unwrap();
                    let names: Vec<String> = ps
                        .values()
                        .map(|p| format!("{}{}", p.nick, if p.link.is_some() { "*" } else { "" }))
                        .collect();
                    println!("  {} sees {:?}", m.nick(), names);
                }
            }
        }
        // Said twice, twenty seconds apart. There is no store-and-forward, so a
        // peer that linked a second after the first round simply was not there
        // for it — and a screenshot wants every line on screen.
        for round in 0..2 {
            for (m, (_, line)) in peers.iter().zip(script.iter()) {
                println!("{} says: {line}", m.nick());
                m.say("nearby".into(), (*line).into(), "message".into(), String::new());
                std::thread::sleep(Duration::from_millis(700));
            }
            if round == 0 {
                std::thread::sleep(Duration::from_secs(20));
            }
        }
        println!("holding {} peers up for 90s — capture now", peers.len());
        std::thread::sleep(Duration::from_secs(90));
        for m in &peers {
            m.stop();
        }
    }

    /// The real check: two meshes in one process find each other over the
    /// actual multicast group and a message crosses the link.
    #[test]
    fn two_peers_discover_and_talk() {
        let a = Mesh::start("ana".into(), None).expect("mesh a");
        let b = Mesh::start("bo".into(), None).expect("mesh b");
        a.set_chans(vec!["general".into()]);
        b.set_chans(vec!["general".into()]);

        let mut linked = false;
        for _ in 0..40 {
            std::thread::sleep(Duration::from_millis(250));
            let a_sees = a.peer_views().iter().any(|p| p.nick == "bo");
            let b_sees = b.peer_views().iter().any(|p| p.nick == "ana");
            let a_link = a.peers.lock().unwrap().values().any(|p| p.link.is_some());
            let b_link = b.peers.lock().unwrap().values().any(|p| p.link.is_some());
            if a_sees && b_sees && a_link && b_link {
                linked = true;
                break;
            }
        }
        assert!(linked, "peers never linked: a={:?} b={:?}", a.peer_views(), b.peer_views());

        let sent = a.say("general".into(), "wheels up".into(), "message".into(), String::new());
        assert_eq!(sent.nick, "ana");

        let mut got = None;
        for _ in 0..40 {
            std::thread::sleep(Duration::from_millis(100));
            if let Some(m) = b.inbox.lock().unwrap().first().cloned() {
                got = Some(m);
                break;
            }
        }
        let got = got.expect("bo never received ana's line");
        assert_eq!(got.body, "wheels up");
        assert_eq!(got.chan, "general");
        assert_eq!(got.id, sent.id, "the message id must survive the link — it is what dedupes it");
        a.stop();
        b.stop();
    }
}
