//! Everything the UI renders from, and every decision about it.
//!
//! Sans-io: this owns the state and performs no IO. Frames arrive as strings,
//! actions arrive as method calls, and everything the world has to do about it
//! comes back as [`Effect`]s. The host owns the socket, the storage, the OS
//! notification and the pixels — and nothing else.
//!
//! Offline-first, because a phone is offline more often than a laptop: history
//! is cached, outgoing messages queue until the socket is back, and nearby mode
//! (F9) keeps working when there is no socket at all.

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::crypto::{fingerprint, seal_for, Identity, Keyring, Master, MASTER_INFO};
use crate::effect::{Effect, Effects, Push, Store};
use crate::gum::{BlobSpec, Doc, Gum, Outcome};
use crate::mesh::{is_nearby, nearby_commands, nearby_name, Nearby, Peer, NEARBY_PREFIX, NEARBY_TOPIC};
use crate::wire::{
    quote_line, split_quote, ChannelView, Command, DeviceKey, Ev, Frame, Member, Notice, RoomHere,
    RoomPerson, SysMsg, User,
};
use crate::xfer::Transfers;

mod export;
pub mod script;
pub mod theme;

/// How much scrollback is kept per channel. The cache is a convenience, not the
/// log — the server holds that.
const CACHE_PER_CHANNEL: usize = 500;
/// An arrival older than this is history, not news, however late we hear of it.
const ANNOUNCE_MAX_AGE_MS: i64 = 5 * 60_000;
/// How long a client-made system line stays in the scrollback.
const SYSTEM_LINE_TTL_MS: i64 = 5 * 60_000;
/// How long a socket has to stay down before the stream says so.
const SPLIT_GRACE_MS: i64 = 3_000;

/// #system: a channel this client makes up, holding the release notes
/// the server sent in `hello`. Nobody is in it and nothing is said in it, so
/// it never touches the socket — the id is ours and no server ever sends one.
pub const SYS_ID: &str = "sys";

/// Channels with no server behind them: the local system pane and anything
/// nearby. A read mark for one of these has nowhere to go and nobody to
/// reconcile with, so it stays on the device.
fn is_local_channel(cid: &str) -> bool {
    cid == SYS_ID || is_nearby(cid)
}
const SYS_NAME: &str = "system";
const SYS_TOPIC: &str = "what changed in Gliph";

pub const KEY_SESSION: &str = "gliph.session";
pub const KEY_CACHE: &str = "gliph.cache";

/// Each server gets its own cache blob, so switching servers does not throw
/// away the other one's scrollback and unsent queue. The address is hex
/// encoded, which localStorage, a plist and the Keychain all accept as a key.
///
/// ponytail: the device keypair is NOT split this way and stays global. Two
/// servers could collude to notice the same public key, but splitting it would
/// strand every sealed channel key already wrapped to the old one — see
/// `core/tests/webcrypto_compat.rs`. Per-server device keys need a migration,
/// not a rename.
pub fn cache_key(server: &str) -> String {
    if server.is_empty() {
        return KEY_CACHE.to_string();
    }
    // Hex of the whole origin, scheme and port included: reversible, so two
    // different servers can never share a key (flattening punctuation made
    // `a-b.example` and `a.b.example` one server).
    let hex: String = server.bytes().map(|b| format!("{b:02x}")).collect();
    format!("{KEY_CACHE}.{hex}")
}
pub const KEY_DEVICE: &str = "gliph.device";

// ---- persisted shapes ----

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Session {
    #[serde(default)]
    pub token: String,
    #[serde(default)]
    pub server: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct Queued {
    channel: String,
    body: String,
    nonce: String,
    /// Sealed at flush time, not queue time, so a key that rotates while
    /// offline is the one the message goes out under.
    #[serde(default)]
    enc: bool,
    /// The event this answers. Default so a cache written before replies
    /// existed still loads.
    #[serde(default)]
    reply_to: String,
    /// Typed inside the 3D room: which sub-room, and whether it was quiet.
    /// A message queued in the room keeps its mark when the socket returns.
    #[serde(default)]
    room: String,
    #[serde(default)]
    quiet: bool,
}

/// Everything worth surviving a relaunch that is not a secret.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct Cache {
    #[serde(default)]
    cursors: BTreeMap<String, i64>,
    #[serde(default)]
    read: BTreeMap<String, i64>,
    #[serde(default)]
    queue: Vec<Queued>,
    #[serde(default)]
    ignores: Vec<String>,
    #[serde(default)]
    events: BTreeMap<String, Vec<Ev>>,
    /// System messages this device has dismissed, so a MOTD stays dismissed
    /// across a restart and release notes are shown once.
    #[serde(default)]
    seen_sys: Vec<String>,
    /// User script hooks (`client/script.rs`), source and on/off.
    #[serde(default)]
    scripts: Vec<script::SavedScript>,
    /// The first-run tour is waiting in #general (see [`tour_msg`]).
    #[serde(default)]
    tour: bool,
}

/// The device's own key material. Separate from [`Cache`] because it goes to
/// the Keychain rather than to a plist.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct DeviceBlob {
    #[serde(default)]
    id: String,
    #[serde(default)]
    secret: String,
    /// server -> this account's master signing secret (hex). Per server,
    /// because the device key is global but each account signs its own.
    #[serde(default)]
    masters: BTreeMap<String, String>,
}

/// Where this device stands with its own account (docs/protocol.md
/// #Device trust). Rendered as the banner on an unapproved device and the
/// prompt on an approved one.
#[derive(Debug, Clone, Default, Serialize)]
pub struct Trust {
    /// The server has told us anything at all yet.
    pub known: bool,
    /// This device carries a signature from the account master key.
    pub signed: bool,
    /// This device's approval code, for the person to read off it.
    pub code: String,
    /// The same code as a QR code of `gliph://approve/<code>`, for a phone's
    /// camera: one string per row, '1' a dark module. Empty once signed.
    pub qr: Vec<String>,
    /// Other devices of this account waiting for approval.
    pub pending: Vec<PendingDevice>,
    #[serde(skip)]
    master: String,
    #[serde(skip)]
    devices: Vec<DeviceKey>,
    #[serde(skip)]
    told: BTreeSet<String>,
}

/// `gliph://approve/<code>` as QR rows ('1' dark), for the unapproved
/// device to show and a phone camera to open in Gliph, which then confirms and
/// runs `/approve <code>`. The code still travels by the person, not the server.
pub fn approve_qr(code: &str) -> Vec<String> {
    let Ok(q) = qrcode::QrCode::new(format!("gliph://approve/{code}")) else { return Vec::new() };
    let w = q.width();
    q.to_colors()
        .chunks(w)
        .map(|row| row.iter().map(|c| if *c == qrcode::Color::Dark { '1' } else { '0' }).collect())
        .collect()
}

#[derive(Debug, Clone, Serialize)]
pub struct PendingDevice {
    pub name: String,
    pub code: String,
}

pub struct Client {
    pub server: String,
    pub token: String,
    pub user: Option<User>,
    pub server_name: String,
    pub upload_max: i64,

    /// What the server last told us it has. The rendered list is this plus
    /// whatever nearby mode is holding — keeping the two apart is what stops a
    /// `state` frame from wiping the nearby channels.
    server_channels: Vec<ChannelView>,
    channels: Vec<ChannelView>,
    server_commands: Vec<Command>,
    commands: Vec<Command>,

    events: BTreeMap<String, Vec<Ev>>,
    notices: Vec<Notice>,
    pub active: String,
    pub online: bool,
    ignores: BTreeSet<String>,
    /// Decrypted bodies, keyed by event id; encrypted events render from here.
    plain: BTreeMap<String, String>,

    /// A session with no account and no server: nearby mode only (F9). There is
    /// nobody to authenticate against on a plane, so the nick is simply what
    /// this device calls itself, and nothing is stored.
    pub solo: bool,

    cursors: BTreeMap<String, i64>,
    /// How far the user has READ, per channel. Deliberately a different number
    /// from `cursors`, which is how far we have RECEIVED — that one advances
    /// the moment an event arrives, so using it as the read mark makes every
    /// badge permanently zero.
    read: BTreeMap<String, i64>,
    queue: Vec<Queued>,

    identity: Option<Identity>,
    /// server -> master signing secret, hex. See [`DeviceBlob::masters`].
    masters: BTreeMap<String, String>,
    pub trust: Trust,
    keys: Keyring,
    /// channel -> member ids last seen, so a departure (and, in a sealed
    /// channel, an arrival) can trigger a re-key
    member_sets: BTreeMap<String, BTreeSet<String>>,
    /// epochs already sealed and published, so we never republish in a loop
    published: BTreeSet<String>,

    pub aliases: BTreeMap<String, String>,
    pub highlights: Vec<String>,

    /// The operator's message of the day, and the notes for this build. Both
    /// arrive in `hello`; what has been dismissed is remembered here, because
    /// the server holds no per-user read state for either.
    motd: Option<SysMsg>,
    news: Vec<SysMsg>,
    seen_sys: BTreeSet<String>,
    /// A brand-new account's intro card in #general. Set by the hello of the
    /// account's first connection ever (`hello.first`) and gone for good once
    /// dismissed: no later hello says `first`, on this device or another.
    tour: Option<SysMsg>,

    /// Announcements are suppressed until catch-up has settled. Reconnecting
    /// after lunch must not fire forty banners for messages you are about to
    /// scroll past anyway.
    live: bool,
    /// One announcement per event, ever. Decryption can revisit an event after
    /// keys land, and a re-render is not a new arrival.
    announced: BTreeSet<String>,

    pub nearby: Nearby,
    pub xfer: Transfers,
    /// The blob surface (docs/plans/gliph-apps.md). Selection, menus, drafts
    /// and the keyboard model all live in the core, so a frontend only has to
    /// draw a tree and report taps.
    pub gum: Gum,
    /// Who `/send` is aimed at, until the file picker comes back.
    pub offer_to: String,
    /// The event the next message answers, as an event id. It lives here and
    /// not in a frontend so that "sending clears it" and "changing channel
    /// clears it" are one rule rather than one per client.
    reply_to: String,
    /// Nearby has no server to hand out sequence numbers, so the client counts.
    /// Ordering is arrival order per device, which is the only order a mesh
    /// with no clock authority can honestly claim.
    mesh_seq: BTreeMap<String, i64>,

    /// Host wall-clock, refreshed on every entry point that needs one. The core
    /// has no clock: wasm and iOS do not agree on one, and a core that reads a
    /// clock is a core that cannot be tested deterministically.
    now_ms: i64,

    /// Who is standing in each channel's 3D room. Seeded from the snapshot in
    /// every `state` frame and kept current by the `room.event` relays, which
    /// reach every member of the channel whether or not they are inside. This
    /// is what the live strip above the composer is drawn from.
    rooms: BTreeMap<String, RoomHere>,
    /// Your own lines from inside the room stay out of the stream while this
    /// is on. A rendering choice and nothing else: the messages are still
    /// posted, stored and searched. Per person, per session, never announced.
    pub room_quiet: bool,

    /// A netsplit line waiting out the grace window: the socket dropped at
    /// this clock, and the line is only worth writing if it is still down at
    /// the next tick past `SPLIT_GRACE_MS`. A blip that reconnects sooner
    /// never reaches the scrollback at all.
    split_pending: Option<(i64, String)>,

    /// User script hooks (F13), compiled. See `client/script.rs`.
    scripts: script::Scripts,
}

impl Default for Client {
    fn default() -> Self {
        Client {
            server: String::new(),
            token: String::new(),
            user: None,
            server_name: String::new(),
            upload_max: 1 << 30,
            server_channels: Vec::new(),
            channels: Vec::new(),
            server_commands: Vec::new(),
            commands: Vec::new(),
            events: BTreeMap::new(),
            notices: Vec::new(),
            active: String::new(),
            online: false,
            ignores: BTreeSet::new(),
            plain: BTreeMap::new(),
            solo: false,
            cursors: BTreeMap::new(),
            read: BTreeMap::new(),
            queue: Vec::new(),
            identity: None,
            masters: BTreeMap::new(),
            trust: Trust::default(),
            keys: Keyring::default(),
            member_sets: BTreeMap::new(),
            published: BTreeSet::new(),
            aliases: BTreeMap::new(),
            highlights: Vec::new(),
            motd: None,
            news: Vec::new(),
            seen_sys: BTreeSet::new(),
            tour: None,
            live: false,
            announced: BTreeSet::new(),
            nearby: Nearby::default(),
            xfer: Transfers::default(),
            gum: Gum::default(),
            offer_to: String::new(),
            reply_to: String::new(),
            mesh_seq: BTreeMap::new(),
            now_ms: 0,
            rooms: BTreeMap::new(),
            room_quiet: false,
            split_pending: None,
            scripts: script::Scripts::default(),
        }
    }
}

impl Client {
    pub fn new() -> Self {
        Self::default()
    }

    // ---- restore ----

    /// Hand back what was persisted last run. Every argument is optional: a
    /// fresh install passes empty strings and gets a working client.
    pub fn restore(&mut self, session: &str, cache: &str, device: &str) {
        if let Ok(s) = serde_json::from_str::<Session>(session) {
            self.token = s.token;
            self.server = s.server;
        }
        if let Ok(c) = serde_json::from_str::<Cache>(cache) {
            self.cursors = c.cursors;
            self.read = c.read;
            self.queue = c.queue;
            self.ignores = c.ignores.into_iter().collect();
            self.events = c.events;
            self.seen_sys = c.seen_sys.into_iter().collect();
            self.scripts.restore(c.scripts);
            self.tour = c.tour.then(tour_msg);
        }
        if let Ok(d) = serde_json::from_str::<DeviceBlob>(device) {
            if !d.id.is_empty() {
                self.identity = Identity::restore(&d.id, &d.secret).ok();
                self.masters = d.masters;
            }
        }
    }

    /// This device's keypair, minted on first use. The secret comes back as a
    /// `Persist` so the host can put it in the Keychain — the core never
    /// touches storage.
    fn identity(&mut self, fx: &mut Effects) -> Identity {
        if let Some(id) = &self.identity {
            return id.clone();
        }
        let fresh = Identity::generate();
        self.identity = Some(fresh.clone());
        self.save_device(fx);
        fresh
    }

    fn save_device(&self, fx: &mut Effects) {
        let Some(me) = &self.identity else { return };
        let blob =
            DeviceBlob { id: me.id.clone(), secret: me.secret_hex(), masters: self.masters.clone() };
        if let Ok(value) = serde_json::to_string(&blob) {
            fx.push(Effect::Persist { key: KEY_DEVICE.into(), value, store: Store::Secret });
        }
    }

    fn master(&self) -> Option<Master> {
        self.masters.get(&self.server).and_then(|h| Master::restore(h).ok())
    }

    fn save_session(&self, fx: &mut Effects) {
        let s = Session { token: self.token.clone(), server: self.server.clone() };
        if let Ok(value) = serde_json::to_string(&s) {
            fx.push(Effect::Persist { key: KEY_SESSION.into(), value, store: Store::Plain });
        }
    }

    /// The cache is written whole rather than per-channel. One blob is one
    /// host-side write to reason about, and at 500 events a channel it stays
    /// comfortably inside every platform's quota.
    fn save_cache(&self, fx: &mut Effects) {
        let mut events = BTreeMap::new();
        for (cid, evs) in &self.events {
            // A nearby channel is gone when the last peer leaves (F9).
            // Persisting its scrollback would quietly make that untrue.
            if is_nearby(cid) {
                continue;
            }
            let tail = if evs.len() > CACHE_PER_CHANNEL {
                evs[evs.len() - CACHE_PER_CHANNEL..].to_vec()
            } else {
                evs.clone()
            };
            events.insert(cid.clone(), tail);
        }
        let c = Cache {
            cursors: self.cursors.clone(),
            read: self.read.clone(),
            queue: self.queue.clone(),
            ignores: self.ignores.iter().cloned().collect(),
            events,
            // Bounded: a dismissal only has to outlive the message it hides,
            // and the server sends the newest five notes.
            seen_sys: self.seen_sys.iter().rev().take(40).cloned().collect(),
            scripts: self.scripts.saved(),
            tour: self.tour.is_some(),
        };
        if let Ok(value) = serde_json::to_string(&c) {
            fx.push(Effect::Persist { key: cache_key(&self.server), value, store: Store::Plain });
        }
    }

    // ---- session ----

    /// Take up a session the host just obtained (login, register, device link).
    pub fn set_session(&mut self, server: &str, token: &str, user_json: &str) -> Effects {
        let mut fx = Effects::new();
        self.server = server.trim_end_matches('/').to_string();
        self.token = token.to_string();
        if let Ok(u) = serde_json::from_str::<User>(user_json) {
            self.user = Some(u);
        }
        self.save_session(&mut fx);
        fx.changed();
        fx
    }

    /// Put this session on a different server. Everything the last one left
    /// here goes with it — channels, scrollback, cursors, the unsent queue —
    /// and the new server's own cache comes in.
    ///
    /// The device keypair does NOT go. It is this device's identity rather than
    /// that server's, and minting a fresh one per switch would strand every
    /// sealed channel key already wrapped to it.
    pub fn adopt_server(&mut self, server: &str, token: &str, user_json: &str, cache_json: &str) -> Effects {
        let identity = self.identity.take();
        let masters = std::mem::take(&mut self.masters);
        *self = Client::new();
        self.identity = identity;
        self.masters = masters;
        // An empty session and device blob are both a no-op inside restore, so
        // this loads the cache and nothing else.
        self.restore("", cache_json, "");
        self.set_session(server, token, user_json)
    }

    /// Signed out of this server. Its token and cached scrollback go with it.
    ///
    /// The device key is one per install and every signed-in server has
    /// channel keys wrapped to it, so it goes only with the LAST server
    /// (`last`): a phone that changes hands must not read the last account's
    /// channels, and signing out of one team must not make the others' sealed
    /// history unreadable.
    pub fn logout(&mut self, last: bool) -> Effects {
        let mut fx = Effects::new();
        let server = self.server.clone();
        let identity = self.identity.take();
        let mut masters = std::mem::take(&mut self.masters);
        *self = Client::new();
        // Where, but not who: the next sign-in should not have to retype the
        // server address.
        self.server = server;
        self.save_session(&mut fx);
        fx.push(Effect::Forget { key: cache_key(&self.server), store: Store::Plain });
        if last {
            fx.push(Effect::Forget { key: KEY_DEVICE.into(), store: Store::Secret });
        } else {
            // This account's signing key leaves with this account.
            masters.remove(&self.server);
            self.identity = identity;
            self.masters = masters;
            self.save_device(&mut fx);
        }
        fx.changed();
        fx
    }

    // ---- socket lifecycle ----

    /// The socket opened. Announce this device's public key before anything
    /// else: the server needs it on file before anyone can seal a channel key
    /// to us.
    pub fn connected(&mut self, platform: &str, now_ms: i64) -> Effects {
        let mut fx = Effects::new();
        self.now_ms = now_ms;
        let rejoined = !self.online && !self.channels.is_empty();
        self.online = true;
        // A blip nobody was told about needs no "reconnected" either.
        let blip = self.split_pending.take().is_some();
        if rejoined && !blip {
            self.system_line("* reconnected", &self.active.clone());
        }

        let me = self.identity(&mut fx);
        fx.send(Frame::new(
            "device.hello",
            json!({ "device": me.id, "name": platform, "pubkey": me.pubkey() }),
        ));
        fx.send(Frame::new("sync", json!({ "cursors": self.cursors })));
        self.flush_queue(&mut fx);
        // Catch-up arrives in the next few frames. `live` is re-armed by the
        // host calling `go_live` once it has settled.
        self.live = false;
        fx.changed();
        fx
    }

    /// Catch-up has landed; announcements from here on are news.
    pub fn go_live(&mut self) {
        self.live = true;
    }

    /// The socket closed. Netsplit weather (F9): the disconnect is a line in
    /// the scrollback, not a modal, and it says what can still be done — nearby
    /// mode does not need the server that just went away.
    pub fn disconnected(&mut self, now_ms: i64) -> Effects {
        let mut fx = Effects::new();
        self.now_ms = now_ms;
        let was_online = self.online;
        self.online = false;
        self.live = false;
        if was_online {
            let n = self.nearby.peers.len();
            let who = if self.server_name.is_empty() { "the server" } else { &self.server_name };
            let line = if n > 0 {
                format!(
                    "* disconnected from {who} — {n} peer{} nearby, /nearby #name still works",
                    if n == 1 { "" } else { "s" }
                )
            } else {
                format!("* disconnected from {who} — reconnecting")
            };
            self.split_pending = Some((now_ms, line));
        }
        fx.changed();
        fx
    }

    /// Write the held netsplit line once the grace window has passed.
    fn settle_split(&mut self) -> bool {
        match &self.split_pending {
            Some((at, _)) if !self.online && self.now_ms - at >= SPLIT_GRACE_MS => {
                let (_, line) = self.split_pending.take().unwrap();
                self.system_line(&line, &self.active.clone());
                true
            }
            _ => false,
        }
    }

    /// Woken by a push. The payload said nothing — that is the design, see
    /// server/push.go — so the only correct reaction is to get current.
    pub fn wake(&mut self) -> Effects {
        let mut fx = Effects::new();
        if self.online {
            fx.send(Frame::new("sync", json!({ "cursors": self.cursors })));
        }
        // A wake is the one case where catch-up IS the news, so announcements
        // are armed immediately; the age guard in `announce` still keeps a long
        // backlog quiet.
        self.live = true;
        fx
    }

    /// Periodic housekeeping. The only thing the core needs a clock for.
    pub fn tick(&mut self, now_ms: i64) -> Effects {
        let mut fx = Effects::new();
        self.now_ms = now_ms;
        if self.xfer.tick(now_ms) | self.sweep_system_lines() | self.settle_split() {
            fx.changed();
        }
        fx
    }

    // ---- inbound frames ----

    pub fn handle_frame(&mut self, json: &str, now_ms: i64) -> Effects {
        let mut fx = Effects::new();
        self.now_ms = now_ms;
        let Ok(f) = serde_json::from_str::<Frame>(json) else { return fx };

        match f.t.as_str() {
            // The heartbeat's answer, every five seconds. Nothing about the UI
            // changed, so no Changed: the trailing fx.changed() below would
            // otherwise rebuild every frontend's whole DOM on an idle
            // connection — cards collapse to placeholders and pop back, and
            // the stream visibly jumps while nobody is doing anything.
            "pong" => return fx,
            "hello" => {
                if let Some(u) = f.d.get("user") {
                    self.user = serde_json::from_value(u.clone()).ok();
                }
                self.server_commands = commands_of(&f.d);
                self.rebuild_commands();
                if f.d.get("first") == Some(&json!(true)) && self.tour.is_none() {
                    self.tour = Some(tour_msg());
                    self.save_cache(&mut fx);
                }
                if let Some(s) = f.d.get("server") {
                    self.server_name =
                        s.get("name").and_then(Value::as_str).unwrap_or_default().into();
                    if let Some(m) = s.get("upload_max").and_then(Value::as_i64) {
                        self.upload_max = m;
                    }
                    self.motd = motd_of(s.get("motd"));
                    self.news = news_of(s.get("news"));
                    // The notes are a channel now, so the list has to be
                    // rebuilt around them.
                    self.rebuild_channels();
                }
            }

            // The 3D room's presence relays. They reach every member of the
            // channel, inside the room or not, which is what makes the live
            // strip possible without asking the server anything.
            "room.event" => {
                let cid = f.str("channel");
                let uid = f.str("user_id");
                if cid.is_empty() || uid.is_empty() {
                    return fx;
                }
                match f.str("type").as_str() {
                    "join" | "move" => {
                        let witnessed = fmt_ms(self.now_ms);
                        let here = self.rooms.entry(cid).or_default();
                        if here.since.is_empty() {
                            // The relay carries no clock, so the strip dates
                            // the room from the first arrival we witnessed. A
                            // reconnect corrects it from the server's snapshot.
                            here.since = witnessed;
                        }
                        let person = RoomPerson {
                            user_id: uid.clone(),
                            name: f.str("name"),
                            room: f.str("room"),
                            voice: f.d.get("voice").and_then(Value::as_bool).unwrap_or_default(),
                        };
                        match here.users.iter_mut().find(|p| p.user_id == uid) {
                            // A move carries no name on some paths; never
                            // overwrite a known one with an empty string.
                            Some(had) => {
                                if !person.name.is_empty() {
                                    had.name = person.name;
                                }
                                if !person.room.is_empty() {
                                    had.room = person.room;
                                }
                                had.voice = person.voice;
                            }
                            None => here.users.push(person),
                        }
                    }
                    "leave" => {
                        if let Some(here) = self.rooms.get_mut(&cid) {
                            here.users.retain(|p| p.user_id != uid);
                            if here.users.is_empty() {
                                self.rooms.remove(&cid);
                            }
                        }
                    }
                    _ => return fx,
                }
                fx.changed();
            }

            // /room0 quiet: the server asks, the client decides. Nothing about
            // this reaches anyone else.
            "room.quiet" => {
                self.room_quiet = !self.room_quiet;
                let msg = if self.room_quiet {
                    "quiet: your room lines stay out of the stream. they are still in the channel's history"
                } else {
                    "your room lines go to the channel again"
                };
                self.notice(msg, "info");
                fx.changed();
            }

            // The operator saved a new MOTD while we were connected.
            "motd" => {
                self.motd = motd_of(f.d.get("motd"));
                fx.changed();
            }

            // The palette is replaced wholesale whenever plugins come or go.
            "commands" => {
                self.server_commands = commands_of(&f.d);
                self.rebuild_commands();
            }

            // Another of this account's devices moved its read mark. Forward
            // only, same as everywhere else, so frames arriving out of order
            // cannot pull a badge back open.
            "read" => {
                if let Some(marks) = f.d.get("marks").and_then(Value::as_object) {
                    let mut moved = false;
                    for (cid, seq) in marks {
                        let Some(seq) = seq.as_i64() else { continue };
                        if seq > *self.read.get(cid).unwrap_or(&0) {
                            self.read.insert(cid.clone(), seq);
                            moved = true;
                        }
                    }
                    if moved {
                        self.save_cache(&mut fx);
                    }
                }
            }

            "state" => {
                self.server_channels = f
                    .d
                    .get("channels")
                    .and_then(|c| serde_json::from_value(c.clone()).ok())
                    .unwrap_or_default();
                self.rebuild_channels();
                self.reseed_rooms();
                if let Some(u) = f.d.get("user") {
                    if let Ok(u) = serde_json::from_value::<User>(u.clone()) {
                        self.user = Some(u);
                    }
                }
                if let Some(p) = f.d.get("prefs") {
                    if let Some(a) = p.get("aliases").and_then(Value::as_object) {
                        self.aliases = a
                            .iter()
                            .filter_map(|(k, v)| v.as_str().map(|s| (k.clone(), s.to_string())))
                            .collect();
                    }
                    if let Some(h) = p.get("highlights").and_then(Value::as_array) {
                        self.highlights =
                            h.iter().filter_map(|x| x.as_str().map(str::to_string)).collect();
                    }
                }
                // Read marks reconcile with the server here. The rule is the
                // same one the server uses: forward only, highest wins. That
                // makes this device adopt what was read on another one without
                // either side having to know which happened first.
                //
                // First sight of a channel is still not a pile of unread. With
                // no local mark and nothing from the server, anchor where the
                // server currently is; with a mark from the server, that IS
                // where we have read to.
                let seen: Vec<(String, i64, i64)> =
                    self.channels.iter().map(|c| (c.id.clone(), c.seq, c.read)).collect();
                // Only what the server is actually behind on goes back up. A
                // state frame arrives on every presence change, so replying to
                // each one with the whole map would be a frame per person
                // walking in or out, for marks the server already holds.
                let mut ahead: BTreeMap<String, i64> = BTreeMap::new();
                for (id, seq, server_read) in seen {
                    match self.read.get(&id) {
                        Some(&mine) if server_read > mine => {
                            self.read.insert(id, server_read);
                        }
                        // Read further here than the server knows, which is
                        // what reading offline looks like once we reconnect.
                        Some(&mine) if mine > server_read && !is_local_channel(&id) => {
                            ahead.insert(id, mine);
                        }
                        Some(_) => {}
                        // The first-sight anchor is a courtesy to THIS device,
                        // not a claim that anyone read anything, so it stays
                        // local. Sending it would clear the badge on a device
                        // that has genuine unread waiting.
                        None => {
                            self.read.insert(id, if server_read > 0 { server_read } else { seq });
                        }
                    }
                }
                if !ahead.is_empty() {
                    fx.send(Frame::new("read", json!({ "marks": ahead })));
                }
                if self.active.is_empty() {
                    if let Some(first) = self.channels.first() {
                        self.active = first.id.clone();
                    }
                }
                // Membership drives keys: a +e channel needs a key we can read,
                // and a member leaving means the key they hold must stop
                // working. In a sealed channel an arrival rotates too, so the
                // newcomer's first key is one minted after they walked in.
                let enc: Vec<(String, bool, BTreeSet<String>)> = self
                    .channels
                    .iter()
                    .filter(|c| c.encrypted())
                    .map(|c| {
                        let ids = c.members.iter().map(|m| m.user_id.clone()).collect();
                        (c.id.clone(), c.history == "sealed", ids)
                    })
                    .collect();
                for (cid, sealed, ids) in enc {
                    let was = self.member_sets.insert(cid.clone(), ids.clone());
                    let rotate = match &was {
                        Some(before) => {
                            before.difference(&ids).next().is_some()
                                || (sealed && ids.difference(before).next().is_some())
                        }
                        None => false,
                    };
                    if rotate {
                        self.rotate_key(&cid, &mut fx);
                    } else {
                        self.ensure_keys(&cid, &mut fx);
                    }
                }
            }

            "device.trust" => self.on_trust(&f, &mut fx),

            // Our own sealed copies of a channel's keys, one per epoch.
            "keys" => {
                let channel = f.str("channel");
                let me = self.identity(&mut fx);
                if let Some(list) = f.d.get("keys").and_then(Value::as_array) {
                    for k in list {
                        let epoch = k.get("epoch").and_then(Value::as_i64).unwrap_or(0);
                        // Opened even when we hold that epoch: two members
                        // rotating at once each mint one, the server keeps
                        // the first, and the server's copy is the one
                        // everybody else can read.
                        let wrapped =
                            k.get("wrapped").and_then(Value::as_str).unwrap_or_default();
                        // Not for us, or stale after a reinstall: both are
                        // ordinary, neither is an error worth showing.
                        let _ = self.keys.open_sealed(&me, &channel, epoch, wrapped);
                    }
                }
                self.redecrypt(&channel, &mut fx);
                // Deliberately NOT calling ensure_keys here: it sends
                // keys.fetch, whose reply is this very frame, which would loop.
            }

            // Someone published a new epoch; go and collect our copy.
            "keys.epoch" => fx.send(Frame::new("keys.fetch", json!({ "channel": f.str("channel") }))),

            "keys.rekey" => {
                let cid = f.str("channel");
                self.ensure_keys(&cid, &mut fx);
            }

            // The devices that still need a given epoch sealed to them.
            "keys.peers" => self.serve_keys(&f, &mut fx),

            "events" => {
                let cid = f.str("channel");
                let evs: Vec<Ev> = f
                    .d
                    .get("events")
                    .and_then(|e| serde_json::from_value(e.clone()).ok())
                    .unwrap_or_default();
                self.ingest(&cid, evs, &mut fx);
            }

            "event" => {
                let cid = f.str("channel");
                if let Ok(ev) = serde_json::from_value::<Ev>(f.d.clone()) {
                    self.ingest(&cid, vec![ev], &mut fx);
                }
            }

            // A provider rendered its surface, or popped a menu for one item.
            "blob.render" => {
                let doc: Doc = f
                    .d
                    .get("doc")
                    .and_then(|d| serde_json::from_value(d.clone()).ok())
                    .unwrap_or_default();
                self.gum.put_doc(&f.str("channel"), &f.str("blob"), doc);
            }

            // A command like /todos asked this client to switch surface. Only
            // the connection that ran it is told, so a phone in a pocket does
            // not change screen because a laptop typed a word.
            "blob.focus" => {
                let blob = f.str("blob");
                if self.blobs().iter().any(|b| b.id == blob) {
                    self.gum.open(&blob);
                    fx.send(Frame::new(
                        "blob.open",
                        json!({ "channel": self.active, "blob": blob }),
                    ));
                }
            }

            // /update ran on this connection: hand the actual download to the
            // host, which owns the updater. A phone or a browser tab has no
            // updater and says so.
            "client.update" => fx.push(Effect::Update),

            "blob.menu" => {
                let actions = f
                    .d
                    .get("actions")
                    .and_then(|a| serde_json::from_value(a.clone()).ok())
                    .unwrap_or_default();
                self.gum.put_menu(&f.str("item"), actions);
            }

            "notice" => {
                self.notices.push(Notice {
                    channel: f.str("channel"),
                    level: {
                        let l = f.str("level");
                        if l.is_empty() { "info".into() } else { l }
                    },
                    text: f.str("text"),
                });
                self.trim_notices();
            }

            "error" => {
                let text = f.str("message");
                self.notices.push(Notice {
                    channel: self.active.clone(),
                    level: "error".into(),
                    text: if text.is_empty() { "error".into() } else { text },
                });
                self.trim_notices();
            }

            _ => {}
        }
        fx.changed();
        fx
    }

    fn trim_notices(&mut self) {
        if self.notices.len() > 200 {
            self.notices.drain(..self.notices.len() - 200);
        }
    }

    // ---- events ----

    fn ingest(&mut self, cid: &str, evs: Vec<Ev>, fx: &mut Effects) {
        if evs.is_empty() {
            return;
        }
        let cur = self.events.entry(cid.to_string()).or_default();
        let seen: BTreeSet<String> = cur.iter().map(|e| e.id.clone()).collect();
        let fresh: Vec<Ev> = evs.into_iter().filter(|e| !seen.contains(&e.id)).collect();
        cur.extend(fresh.iter().cloned());
        cur.sort_by(|a, b| a.seq.partial_cmp(&b.seq).unwrap_or(std::cmp::Ordering::Equal));
        let newest = cur.last().map(|l| (l.seq.floor() as i64, l.actor.clone()));
        if let Some((seq, actor)) = newest {
            let slot = self.cursors.entry(cid.to_string()).or_insert(0);
            *slot = (*slot).max(seq);
            // The badge follows the events, not the next state frame.
            if let Some(ch) = self.channels.iter_mut().find(|c| c.id == cid) {
                ch.seq = ch.seq.max(seq);
            }
            // If the newest line is yours you have read everything above it —
            // from this device or another.
            if self.user.as_ref().is_some_and(|u| u.nick == actor) {
                self.mark_read(cid, fx);
            }
        }

        for e in &fresh {
            if !e.enc {
                self.announce(cid, e, fx);
            }
        }
        self.decrypt_new(cid, &fresh, fx);
        self.save_cache(fx);
    }

    /// A line the client made up — a system notice, a netsplit weather report.
    /// It is not from the server, so it must not move the cursor (that number
    /// is a promise about what the SERVER has sent us) and it is not cached.
    fn ingest_local(&mut self, cid: &str, ev: Ev) {
        let cur = self.events.entry(cid.to_string()).or_default();
        if cur.iter().any(|e| e.id == ev.id) {
            return;
        }
        cur.push(ev);
        cur.sort_by(|a, b| a.seq.partial_cmp(&b.seq).unwrap_or(std::cmp::Ordering::Equal));
    }

    /// In-stream system line, IRC style: the netsplit is part of the
    /// scrollback, not a modal over it (F9).
    pub fn system_line(&mut self, text: &str, cid: &str) {
        if cid.is_empty() {
            return;
        }
        self.sweep_system_lines();
        let base = self
            .events
            .get(cid)
            .and_then(|e| e.last())
            .map(|e| e.seq)
            .unwrap_or(*self.cursors.get(cid).unwrap_or(&0) as f64);
        self.ingest_local(
            cid,
            Ev {
                seq: base + 0.5,
                id: format!("sys-{}", crate::crypto::uuid4()),
                channel: cid.to_string(),
                kind: "notice".into(),
                actor: "*".into(),
                ts: fmt_ms(self.now_ms),
                body: text.to_string(),
                meta: json!({ "local": true }),
                ..Default::default()
            },
        );
    }

    /// Netsplit weather is weather: it says what is happening now, and once it
    /// is old it is only clutter in the scrollback. Drop the ones past their
    /// window; the connection state itself lives in the status bar.
    fn sweep_system_lines(&mut self) -> bool {
        if self.now_ms == 0 {
            return false;
        }
        let cutoff = self.now_ms - SYSTEM_LINE_TTL_MS;
        let mut dropped = false;
        for events in self.events.values_mut() {
            let before = events.len();
            // A local line with no readable clock is one an older build wrote
            // before these carried one; it came back out of the cache and would
            // otherwise sit there as "[NaN:NaN]" forever.
            events.retain(|e| {
                e.meta.get("local") != Some(&json!(true))
                    || parse_ms(&e.ts).is_some_and(|at| at > cutoff)
            });
            dropped |= events.len() != before;
        }
        dropped
    }

    /// Announce an arrival, if it is one worth announcing.
    ///
    /// Called only once a body is READABLE — for a +e channel that means after
    /// this device decrypted it, never before. There is deliberately no path
    /// from a ciphertext to a notification: if we cannot read it, we have
    /// nothing to say about it.
    fn announce(&mut self, cid: &str, e: &Ev, fx: &mut Effects) {
        // Scripts see the same arrivals, once each, and only once readable.
        self.run_scripts(cid, e, fx);
        // A new account landing in #general is the one join worth announcing:
        // someone new is here. A /join by an existing member is not.
        let new_member = e.kind == "join" && e.meta.get("new") == Some(&json!(true));
        // An invite into the room is an ask, so it pings like a mention: named
        // or channel-wide, both, because someone opened a door for you.
        let invite = e.kind == "room_invite";
        if !self.live || (e.kind != "message" && e.kind != "action" && !new_member && !invite) {
            return;
        }
        // Belt to the `live` flag's braces. Waking after a long suspend replays
        // everything past our cursor, and a two-hour-old line is history.
        if let Some(at) = parse_ms(&e.ts) {
            if self.now_ms > 0 && self.now_ms - at > ANNOUNCE_MAX_AGE_MS {
                return;
            }
        }
        if Some(&e.actor) == self.user.as_ref().map(|u| &u.nick) || self.ignores.contains(&e.actor)
        {
            return;
        }
        if self.announced.contains(&e.id) {
            return;
        }
        let Some(ch) = self.channels.iter().find(|c| c.id == cid) else { return };
        let is_dm = ch.is_dm;
        let text = if e.enc {
            match self.plain.get(&e.id) {
                Some(t) => t.clone(),
                None => return, // not readable yet — try again when it is
            }
        } else {
            e.body.clone()
        };
        // An alert is about what someone said to you, not about the line they
        // were answering — matching on the quote would ping you a second time
        // for your own message every time anyone replied to it.
        let text = if invite {
            format!("asked you into room0 · {}", e.body)
        } else {
            split_quote(&text).1.to_string()
        };
        // A nearby channel announces like a DM. You joined a room of five
        // people standing near you, on purpose, a minute ago; waiting for a
        // highlight word there means the phone stays silent throughout.
        let highlight = self.is_highlight(&text, &e.actor) || invite;
        if !is_dm && !is_nearby(cid) && !highlight && !new_member {
            return;
        }
        self.announced.insert(e.id.clone());
        // Ordered by how much it is about you: being tagged beats being named
        // beats being in the room. A DM is all three, and reads as a DM.
        let event = if invite {
            "room_invite"
        } else if new_member {
            "join"
        } else if is_dm {
            "dm"
        } else if self.is_mention(&text) {
            "mention"
        } else if highlight {
            "highlight"
        } else {
            "nearby"
        };
        fx.push(Effect::Alert {
            id: e.id.clone(),
            channel: cid.to_string(),
            nick: e.actor.clone(),
            text,
            is_dm,
            event: event.to_string(),
        });
    }

    /// Decrypt whatever arrived that we can read.
    fn decrypt_new(&mut self, cid: &str, evs: &[Ev], fx: &mut Effects) {
        let mut opened = Vec::new();
        for e in evs {
            if !e.enc || self.plain.contains_key(&e.id) {
                continue;
            }
            if let Some(text) = self.keys.decrypt_body(cid, e.epoch, &e.body) {
                self.plain.insert(e.id.clone(), text);
                opened.push(e.clone());
            }
        }
        for e in &opened {
            self.announce(cid, e, fx);
        }
    }

    /// Re-run decryption over a channel's cached history, after new keys land.
    fn redecrypt(&mut self, cid: &str, fx: &mut Effects) {
        let evs = self.events.get(cid).cloned().unwrap_or_default();
        self.decrypt_new(cid, &evs, fx);
    }

    // ---- e2ee key management ----

    /// Make sure this channel has a key we can use, and that every member
    /// device has a sealed copy. Safe to call repeatedly.
    fn ensure_keys(&mut self, cid: &str, fx: &mut Effects) {
        if cid.is_empty() {
            return;
        }
        let _ = self.identity(fx);
        fx.send(Frame::new("keys.fetch", json!({ "channel": cid })));
        // Ask who is missing the newest epoch; the answer drives serve_keys.
        fx.send(Frame::new(
            "keys.peers",
            json!({ "channel": cid, "epoch": self.keys.latest_epoch(cid) }),
        ));
    }

    /// Answer to keys.peers: create the first epoch, or seal the current one to
    /// devices that do not have it yet (a new member, or a new device).
    fn serve_keys(&mut self, f: &Frame, fx: &mut Effects) {
        let cid = f.str("channel");
        if cid.is_empty() {
            return;
        }
        let asked = f.int("epoch");
        let max_epoch = f.int("max_epoch");
        let devices: Vec<DeviceKey> = f
            .d
            .get("devices")
            .and_then(|d| serde_json::from_value::<Vec<DeviceKey>>(d.clone()).ok())
            .unwrap_or_default()
            .into_iter()
            // The server already lists only approved devices. Checking the
            // signature here as well is what stops a server that lies from
            // getting a key sealed to a device of its own.
            .filter(DeviceKey::signed)
            .collect();
        let _ = self.identity(fx);

        // Nobody has ever keyed this channel: we are the first in, so mint
        // epoch 1.
        if max_epoch == 0 {
            // An unapproved device must not mint a key: nobody could be
            // sealed a copy by it, and the server refuses its publication.
            if !self.trust.signed {
                return;
            }
            if !self.published.insert(format!("{cid}:1")) {
                return;
            }
            self.keys.mint(&cid, 1);
            if let Ok(entries) = self.keys.seal_to(&cid, 1, &devices) {
                fx.send(Frame::new(
                    "keys.publish",
                    json!({ "channel": cid, "epoch": 1, "entries": entries }),
                ));
            }
            return;
        }

        // Someone is missing the current epoch and we can read it: seal to them.
        let epoch = if asked > 0 { asked } else { max_epoch };
        if devices.is_empty() || !self.keys.have(&cid, epoch) {
            return;
        }
        let mut ids: Vec<String> = devices.iter().map(|d| d.id.clone()).collect();
        ids.sort();
        let stamp = format!("{cid}:{epoch}:{}", ids.join(","));
        if self.published.contains(&stamp) {
            return;
        }
        self.published.insert(stamp);
        // An open channel hands a newcomer every epoch we hold, so they read
        // what was said before they came (the member-mediated handoff). A
        // sealed one hands over the asked epoch only, and the server refuses
        // anything older than the newcomer's join on top of that.
        let open = self.channel(&cid).map_or(true, |c| c.history != "sealed");
        let epochs: Vec<i64> = if open { (1..=epoch).collect() } else { vec![epoch] };
        for e in epochs {
            if !self.keys.have(&cid, e) {
                continue;
            }
            if let Ok(entries) = self.keys.seal_to(&cid, e, &devices) {
                if !entries.is_empty() {
                    fx.send(Frame::new(
                        "keys.publish",
                        json!({ "channel": cid, "epoch": e, "entries": entries }),
                    ));
                }
            }
        }
    }

    /// Someone left: mint the next epoch so their key stops working for
    /// anything said from now on.
    fn rotate_key(&mut self, cid: &str, fx: &mut Effects) {
        let _ = self.identity(fx);
        let latest = self.keys.latest_epoch(cid);
        if !self.keys.have(cid, latest) {
            // Nothing of ours to rotate: someone who holds the key will.
            self.ensure_keys(cid, fx);
            return;
        }
        let epoch = latest + 1;
        self.keys.mint(cid, epoch);
        fx.send(Frame::new("keys.peers", json!({ "channel": cid, "epoch": epoch })));
        self.notice("channel key rotated", "info");
    }

    // ---- device trust (F2) ----

    /// The server's view of this account's devices. Three cases: nobody has
    /// minted the account's master key yet (we do, if allowed), we are signed
    /// (pick up the master key if it was just sealed to us, and prompt for
    /// anyone waiting), or we are not (show our code and wait).
    fn on_trust(&mut self, f: &Frame, fx: &mut Effects) {
        let me = self.identity(fx);
        let uid = self.user.as_ref().map(|u| u.id.clone()).unwrap_or_default();
        let master_pub = f.str("master");
        let devices: Vec<DeviceKey> = f
            .d
            .get("devices")
            .and_then(|d| serde_json::from_value(d.clone()).ok())
            .unwrap_or_default();
        let was_signed = self.trust.signed;
        self.trust.known = true;
        self.trust.code = fingerprint(&me.pubkey());
        self.trust.master = master_pub.clone();

        // A master we hold that the server no longer has was reset elsewhere.
        if self.master().is_some_and(|m| m.pubkey() != master_pub) {
            self.masters.remove(&self.server);
            self.save_device(fx);
        }

        if master_pub.is_empty() {
            self.trust.signed = false;
            self.trust.qr = approve_qr(&self.trust.code);
            if f.d.get("can_establish").and_then(Value::as_bool) == Some(true) && !uid.is_empty() {
                // The account's first device, or its first since this build:
                // mint the master key and sign ourselves plus every device the
                // account already trusted before signing existed.
                let m = Master::generate();
                let mut sigs = BTreeMap::new();
                sigs.insert(me.id.clone(), m.sign_device(&uid, &me.id, &me.pubkey()));
                for d in devices.iter().filter(|d| d.legacy && d.id != me.id) {
                    sigs.insert(d.id.clone(), m.sign_device(&uid, &d.id, &d.pubkey));
                }
                fx.send(Frame::new("device.sign", json!({ "master": m.pubkey(), "sigs": sigs })));
                self.masters.insert(self.server.clone(), m.secret_hex());
                self.save_device(fx);
            }
            self.trust.devices = devices;
            self.trust.pending.clear();
            return;
        }

        let mine = devices.iter().find(|d| d.id == me.id);
        self.trust.signed = mine.is_some_and(|d| {
            crate::crypto::verify_device(&master_pub, &uid, &me.id, &me.pubkey(), &d.sig)
        });
        self.trust.qr = if self.trust.signed { Vec::new() } else { approve_qr(&self.trust.code) };

        if self.trust.signed && self.master().is_none() {
            // Approved by another device: it sealed the master key to us.
            let sealed = f.str("sealed_master");
            if let Ok(raw) = me.open_for(MASTER_INFO, &sealed) {
                let hex: String = raw.iter().map(|b| format!("{b:02x}")).collect();
                if Master::restore(&hex).is_ok_and(|m| m.pubkey() == master_pub) {
                    self.masters.insert(self.server.clone(), hex);
                    self.save_device(fx);
                }
            }
        }

        self.trust.pending = devices
            .iter()
            .filter(|d| d.id != me.id && !d.signed())
            .map(|d| PendingDevice { name: d.name.clone(), code: fingerprint(&d.pubkey) })
            .collect();

        if !self.trust.signed {
            if self.trust.told.insert(format!("me:{}", self.trust.code)) {
                self.notice(
                    &format!(
                        "this device needs approval before it can read encrypted channels. on a signed-in device, type /approve {}",
                        self.trust.code
                    ),
                    "info",
                );
            }
        } else if self.master().is_some() {
            let fresh: Vec<PendingDevice> = self
                .trust
                .pending
                .iter()
                .filter(|p| !self.trust.told.contains(&p.code))
                .cloned()
                .collect();
            for p in fresh {
                self.trust.told.insert(p.code.clone());
                self.notice(
                    &format!(
                        "{} wants to read your encrypted channels. if it shows {}, type /approve {}",
                        p.name, p.code, p.code
                    ),
                    "info",
                );
            }
        }
        self.trust.devices = devices;

        // Newly approved: go and collect the keys we were refused.
        if self.trust.signed && !was_signed {
            let enc: Vec<String> =
                self.server_channels.iter().filter(|c| c.encrypted()).map(|c| c.id.clone()).collect();
            for cid in enc {
                self.ensure_keys(&cid, fx);
            }
        }
    }

    /// `/approve <code>`: sign another device of this account and seal the
    /// master key to it, so it can approve the next one.
    fn approve(&mut self, code: &str, fx: &mut Effects) {
        let want: String = code.chars().filter(|c| c.is_ascii_hexdigit()).collect::<String>().to_lowercase();
        if want.is_empty() {
            self.notice("usage: /approve <code shown on the new device>", "info");
            return;
        }
        let Some(m) = self.master() else {
            self.notice("this device is not approved itself, so it cannot approve another", "error");
            return;
        };
        let uid = self.user.as_ref().map(|u| u.id.clone()).unwrap_or_default();
        let me = self.identity(fx);
        let found = self
            .trust
            .devices
            .iter()
            .find(|d| d.id != me.id && fingerprint(&d.pubkey).replace('-', "") == want)
            .cloned();
        let Some(d) = found else {
            self.notice(&format!("no device of yours is waiting with code {code}"), "error");
            return;
        };
        let Some(sealed) = hex_bytes(&m.secret_hex()).and_then(|raw| seal_for(&d.pubkey, MASTER_INFO, &raw))
        else {
            self.notice("that device's key is unreadable", "error");
            return;
        };
        fx.send(Frame::new(
            "device.sign",
            json!({
                "sigs": { d.id.clone(): m.sign_device(&uid, &d.id, &d.pubkey) },
                "sealed": { d.id.clone(): sealed },
            }),
        ));
        self.notice(&format!("approved {}", if d.name.is_empty() { &d.id } else { &d.name }), "info");
    }

    /// `/devices reset`: mint a new master key with this device as its only
    /// signed device. Every other device has to be approved again. For when
    /// the last approved device is gone; the server refuses it while another
    /// approved device is online, which should be used instead.
    fn reset_trust(&mut self, fx: &mut Effects) {
        let uid = self.user.as_ref().map(|u| u.id.clone()).unwrap_or_default();
        if uid.is_empty() {
            return;
        }
        let me = self.identity(fx);
        let m = Master::generate();
        let sigs = json!({ me.id.clone(): m.sign_device(&uid, &me.id, &me.pubkey()) });
        fx.send(Frame::new("device.sign", json!({ "master": m.pubkey(), "sigs": sigs, "reset": true })));
        self.masters.insert(self.server.clone(), m.secret_hex());
        self.save_device(fx);
    }

    // ---- sending ----

    pub fn send_message(&mut self, body: &str) -> Effects {
        self.say(body, "message", "")
    }

    /// A line said inside the 3D room. The same message on the same wire as
    /// anything typed in the channel window, marked with the sub-room it was
    /// said in — and, while quiet is on, flagged to stay out of the stream.
    pub fn send_room_message(&mut self, body: &str, room: &str) -> Effects {
        self.say(body, "message", room)
    }

    /// One sender for both a line and an action. `/me` is a server command in a
    /// server channel and a `kind` on the wire in a nearby one, because there
    /// is no server there to turn it into an event.
    fn say(&mut self, body: &str, kind: &str, room: &str) -> Effects {
        let mut fx = Effects::new();
        let cid = self.active.clone();
        if cid.is_empty() || body.trim().is_empty() {
            return fx;
        }
        // Replying is reading.
        self.mark_read(&cid, &mut fx);

        // The quote goes on here rather than in a composer, so every frontend
        // puts the same body on the wire and none of them can forget to clear
        // the reply afterwards.
        //
        // Two things carry the reply, and they are not redundant. The quote is
        // in the body, which in a +e channel means inside the ciphertext, and
        // it is what you READ — it survives the original scrolling out of the
        // cache, being deleted, or arriving on a device with no key. The id in
        // `meta.reply_to` is what you FOLLOW, and it costs the server knowing
        // the reply graph, which is the price of a jump target.
        let quoted;
        let mut reply_to = String::new();
        let body = match self.reply_quote() {
            Some(line) => {
                quoted = line + body;
                reply_to = std::mem::take(&mut self.reply_to);
                quoted.as_str()
            }
            None => body,
        };

        if cid == SYS_ID {
            self.notice("#system is us talking, not a channel you can answer", "error");
            fx.changed();
            return fx;
        }

        // Nearby channels never touch the socket — that is the whole point of
        // them, and they work fine with the server down.
        if is_nearby(&cid) {
            if self.nearby.on {
                fx.push(Effect::MeshSay {
                    chan: nearby_name(&cid).to_string(),
                    body: body.to_string(),
                    msg_kind: kind.to_string(),
                    reply_to: reply_to.clone(),
                });
            } else {
                self.notice("nearby mode is not running", "error");
            }
            fx.changed();
            return fx;
        }

        let nonce = crate::crypto::uuid4();
        let encrypted = self.channels.iter().any(|c| c.id == cid && c.encrypted());
        debug_assert_eq!(kind, "message", "a server channel makes an action out of /me itself");

        if self.online {
            if let Some(f) = self.message_frame(&cid, body, &nonce, encrypted, &reply_to, room) {
                fx.send(f);
            }
        } else {
            self.queue.push(Queued {
                channel: cid.clone(),
                body: body.to_string(),
                nonce: nonce.clone(),
                enc: encrypted,
                reply_to: reply_to.clone(),
                room: room.to_string(),
                quiet: self.room_quiet && !room.is_empty(),
            });
            // Show it locally as pending so offline typing is not a black hole.
            let seq = *self.cursors.get(&cid).unwrap_or(&0) as f64 + 0.5;
            let actor = self.user.as_ref().map(|u| u.nick.clone()).unwrap_or_else(|| "you".into());
            self.ingest_local(
                &cid,
                Ev {
                    seq,
                    id: format!("pending-{nonce}"),
                    channel: cid.clone(),
                    kind: "message".into(),
                    actor,
                    ts: fmt_ms(self.now_ms),
                    body: body.to_string(),
                    meta: json!({ "pending": true, "reply_to": reply_to,
                                  "room": room, "quiet": self.room_quiet && !room.is_empty() }),
                    ..Default::default()
                },
            );
            self.save_cache(&mut fx);
        }
        fx.changed();
        fx
    }

    /// The wire frame for one message. +e: the server must never see the
    /// text, so it is sealed here and refused rather than sent in the clear.
    fn message_frame(
        &mut self, cid: &str, body: &str, nonce: &str, encrypted: bool, reply_to: &str, room: &str,
    ) -> Option<Frame> {
        // Where it was said, and whether the sender is keeping it out of the
        // stream. Both ride in the clear: the room relay already carries room
        // names, and a client with no key still has to draw the mark.
        let quiet = self.room_quiet && !room.is_empty();
        if !encrypted {
            return Some(Frame::new(
                "message.send",
                json!({ "channel": cid, "body": body, "nonce": nonce, "reply_to": reply_to,
                        "room": room, "quiet": quiet }),
            ));
        }
        match self.keys.encrypt_body(cid, body) {
            Ok((ct, epoch)) => Some(Frame::new(
                "message.send",
                json!({ "channel": cid, "body": ct, "nonce": nonce, "enc": true, "epoch": epoch,
                        "reply_to": reply_to, "room": room, "quiet": quiet }),
            )),
            Err(_) => {
                self.notice("no key for this channel yet — cannot send encrypted", "error");
                None
            }
        }
    }

    fn flush_queue(&mut self, fx: &mut Effects) {
        if self.queue.is_empty() {
            return;
        }
        for q in std::mem::take(&mut self.queue) {
            let was_quiet = std::mem::replace(&mut self.room_quiet, q.quiet);
            let frame = self.message_frame(&q.channel, &q.body, &q.nonce, q.enc, &q.reply_to, &q.room);
            self.room_quiet = was_quiet;
            if let Some(f) = frame {
                fx.send(f);
            }
            // Drop the local pending copy; the server echo replaces it.
            if let Some(evs) = self.events.get_mut(&q.channel) {
                evs.retain(|e| e.id != format!("pending-{}", q.nonce));
            }
        }
        self.save_cache(fx);
    }

    // ---- commands ----

    /// Splits "/name rest" and routes it. Local-only commands (/ignore,
    /// /clear, /nearby) never reach the server.
    pub fn submit(&mut self, text: &str) -> Effects {
        let line = text.trim();
        if line.is_empty() {
            return Effects::new();
        }
        if !line.starts_with('/') {
            return self.send_message(line);
        }

        let (mut name, mut rest) = split_command(&line[1..]);

        // Aliases expand on the client, so /gm becomes whatever you defined
        // without the server ever seeing a command it does not have. One hop
        // only: an alias expanding to another alias would be a loop waiting to
        // happen.
        if let Some(expansion) = self.aliases.get(&name).cloned() {
            let full = if rest.is_empty() { expansion } else { format!("{expansion} {rest}") };
            let full = full.trim().to_string();
            if !full.starts_with('/') {
                return self.send_message(&full);
            }
            let (n, r) = split_command(&full[1..]);
            name = n;
            rest = r;
        }

        let mut fx = Effects::new();
        match name.as_str() {
            "ignore" | "unignore" => {
                if rest.is_empty() {
                    self.notice(&format!("usage: /{name} <nick>"), "info");
                } else if name == "ignore" {
                    self.ignores.insert(rest.clone());
                    self.notice(&format!("ignoring {rest} (local only)"), "info");
                    self.save_cache(&mut fx);
                } else {
                    self.ignores.remove(&rest);
                    self.notice(&format!("no longer ignoring {rest}"), "info");
                    self.save_cache(&mut fx);
                }
            }
            "nearby" | "nearby-leave" | "nearby-off" => self.nearby_command(&name, &rest, &mut fx),
            // Signing happens here because the master key never leaves the
            // device. The server lists both in its manifest, for the palette.
            "approve" => self.approve(&rest, &mut fx),
            "devices" if rest == "reset" => self.reset_trust(&mut fx),
            "send" if self.nearby.on => {
                self.offer_to = rest.trim_start_matches('@').to_string();
                if self.offer_to.is_empty() {
                    self.notice("usage: /send <nick>", "error");
                } else if self.nearby.by_nick(&self.offer_to).is_none() {
                    self.notice(&format!("{} is not nearby", self.offer_to), "error");
                    self.offer_to.clear();
                } else {
                    self.notice(&format!("pick a file to offer {}", self.offer_to), "info");
                    fx.push(Effect::PickFile);
                }
            }
            // /me is a server command in a server channel and a kind on the mesh
            // in a nearby one. Routed here rather than sent to a socket that,
            // in nearby mode, may not exist at all.
            "me" if is_nearby(&self.active) => {
                if rest.is_empty() {
                    self.notice("usage: /me <what you are doing>", "info");
                } else {
                    return self.say(&rest, "action", "");
                }
            }
            "export" => self.export_command(&rest, &mut fx),
            "theme" => self.theme_command(&rest, &mut fx),
            "script" => self.script_command(&rest, &mut fx),
            "clear" => {
                let active = self.active.clone();
                self.events.insert(active, Vec::new());
                self.save_cache(&mut fx);
            }
            _ => fx.send(Frame::new(
                "command.run",
                json!({ "channel": self.active, "name": name, "args": rest }),
            )),
        }
        fx.changed();
        fx
    }

    /// THE dispatcher for a command that did not come from typing — a palette
    /// pick, a button. Everything routes through `submit` so local commands
    /// stay local wherever they are invoked from.
    pub fn run_command(&mut self, name: &str, args: &str) -> Effects {
        self.submit(&format!("/{name} {args}").trim_end().to_string())
    }

    // ---- nearby mesh (F9) ----

    /// The channel list the UI renders is server + nearby. Rebuilt rather than
    /// mutated so a `state` frame and a peer arriving cannot race.
    fn rebuild_channels(&mut self) {
        let mine = self.user.as_ref().map(|u| u.nick.clone()).unwrap_or_default();
        let mut all = self.server_channels.clone();
        if !self.news.is_empty() {
            all.push(ChannelView {
                id: SYS_ID.into(),
                name: SYS_NAME.into(),
                topic: SYS_TOPIC.into(),
                // `n` for the same reason a nearby channel carries it: a mode
                // no server sets, on a channel no server knows about.
                modes: "n".into(),
                history: "open".into(),
                is_dm: false,
                role: "member".into(),
                seq: 0,
                read: 0,
                members: Vec::new(),
                blobs: Vec::new(),
                room: None,
            });
        }
        for name in self.nearby.chans.clone() {
            let mut members = Vec::new();
            if !mine.is_empty() {
                members.push(Member { user_id: "me".into(), nick: mine.clone(), ..Default::default() });
            }
            for nick in self.nearby.peers_in(&name) {
                members.push(Member {
                    user_id: format!("p_{nick}"),
                    nick,
                    ..Default::default()
                });
            }
            all.push(ChannelView {
                id: format!("{NEARBY_PREFIX}{name}"),
                name: name.clone(),
                topic: NEARBY_TOPIC.into(),
                // `n` is a client-side mode: no server ever sends it, and no
                // ops exist in a nearby channel to change it.
                modes: "n".into(),
                // Nobody stores a nearby channel: you hear what is said while
                // you are there, which is what sealed means.
                history: "sealed".into(),
                is_dm: false,
                role: "member".into(),
                seq: *self.mesh_seq.get(&name).unwrap_or(&0),
                // Nothing to sync a read mark with: nearby has no server. The
                // local mark in `read` is the only one there is.
                read: 0,
                members,
                // No apps in a nearby channel: there is no server to run a
                // provider, which is the whole point of the mode.
                blobs: Vec::new(),
                // ...and no 3D room either: it is a server surface.
                room: None,
            });
        }
        self.channels = all;
        self.rebuild_commands();
    }

    /// Nearby's commands are merged into the server's manifest exactly the way
    /// a plugin's are, and disappear with nearby mode.
    fn rebuild_commands(&mut self) {
        self.commands = self.server_commands.clone();
        if self.nearby.on {
            self.commands.extend(nearby_commands());
        }
    }

    /// The native side reported who it can see. Discovery is not membership:
    /// this list is peers with a live link, which is what the transport
    /// guarantees.
    pub fn mesh_peers(&mut self, json: &str) -> Effects {
        let mut fx = Effects::new();
        let next: Vec<Peer> = serde_json::from_str(json).unwrap_or_default();
        // A peer that vanished takes its transfers with it — a progress bar
        // with nothing behind it is worse than an error.
        for old in self.nearby.peers.clone() {
            if !next.iter().any(|p| p.id == old.id) {
                self.xfer.peer_gone(&old.id);
            }
        }
        self.nearby.peers = next;
        self.rebuild_channels();
        fx.changed();
        fx
    }

    pub fn mesh_msg(&mut self, json: &str, now_ms: i64) -> Effects {
        let mut fx = Effects::new();
        self.now_ms = now_ms;
        let Ok(m) = serde_json::from_str::<crate::mesh::MeshMsg>(json) else { return fx };
        if !self.nearby.chans.iter().any(|c| *c == m.chan) {
            return fx; // a channel we are not in
        }
        let cid = format!("{NEARBY_PREFIX}{}", m.chan);
        let seq = self.mesh_seq.entry(m.chan.clone()).or_insert(0);
        *seq += 1;
        let seq = *seq;
        if self.events.get(&cid).is_some_and(|e| e.iter().any(|x| x.id == m.id)) {
            return fx; // echo
        }
        // Only the two kinds a person can produce. A peer is not trusted to
        // name an event kind the renderer treats as a system line.
        let kind = if m.kind == "action" { "action" } else { "message" };
        let ev = Ev {
            seq: seq as f64,
            id: m.id,
            channel: cid.clone(),
            kind: kind.into(),
            actor: m.nick,
            ts: m.ts,
            body: m.body,
            meta: if m.reply_to.is_empty() {
                Value::Null
            } else {
                json!({ "reply_to": m.reply_to })
            },
            ..Default::default()
        };
        self.ingest_local(&cid, ev.clone());
        // ingest_local is deliberately quiet (it is also how system lines
        // land), so the announcement is made here, where we know a person said
        // this.
        self.announce(&cid, &ev, &mut fx);
        fx.changed();
        fx
    }

    /// A frame the mesh does not interpret — file transfer rides these.
    pub fn mesh_frame(&mut self, json: &str, now_ms: i64) -> Effects {
        let mut fx = Effects::new();
        self.now_ms = now_ms;
        let Ok(v) = serde_json::from_str::<Value>(json) else { return fx };
        let from = v.get("from").and_then(Value::as_str).unwrap_or_default();
        let nick = v.get("nick").and_then(Value::as_str).unwrap_or_default();
        let t = v.get("t").and_then(Value::as_str).unwrap_or_default();
        let empty = json!({});
        let d = v.get("d").unwrap_or(&empty);
        self.xfer.on_frame(from, nick, t, d, now_ms, &mut fx);
        for id in self.xfer.take_done() {
            self.post_transfer_line(&id);
        }
        fx.changed();
        fx
    }

    /// A finished nearby transfer gets a line in the room, the way a server
    /// upload does. Only the two ends see it — the bytes went peer to peer and
    /// the room was never copied on them — so it is worded from where you sat.
    fn post_transfer_line(&mut self, id: &str) {
        let Some(x) = self.xfer.list.iter().find(|x| x.id == id).cloned() else { return };
        let cid = format!("{NEARBY_PREFIX}{}", x.chan);
        let seq = self.mesh_seq.entry(x.chan.clone()).or_insert(0);
        *seq += 1;
        let seq = *seq as f64;
        let sent = x.dir == crate::xfer::Dir::Send;
        self.p2p_line(&cid, id, &x.name, x.size as f64, sent, &x.peer_nick, Some(seq));
    }

    /// The same line for a transfer the host drove itself — a DM goes over
    /// WebRTC, not the mesh, and lands here when the checksum matches. Both
    /// ends call this; `sent` is which end you are.
    pub fn p2p_file_line(&mut self, cid: &str, id: &str, name: &str, size: f64, sent: bool, peer: &str) {
        self.p2p_line(cid, id, name, size, sent, peer, None);
    }

    fn p2p_line(&mut self, cid: &str, id: &str, name: &str, size: f64, sent: bool, peer: &str, seq: Option<f64>) {
        let me = self.user.as_ref().map(|u| u.nick.clone()).unwrap_or_default();
        // A server channel numbers its own events; slot the line in behind the
        // last one the way a system line does.
        let seq = seq.unwrap_or_else(|| {
            self.events.get(cid).and_then(|e| e.last()).map(|e| e.seq)
                .unwrap_or(*self.cursors.get(cid).unwrap_or(&0) as f64) + 0.5
        });
        self.ingest_local(
            cid,
            Ev {
                seq,
                // Not the transfer id: an event id has to be unique in the
                // channel, and a re-offer of the same file reuses neither.
                id: format!("xf_{id}"),
                channel: cid.to_string(),
                kind: "file".into(),
                actor: if sent { me } else { peer.to_string() },
                ts: fmt_ms(self.now_ms),
                body: name.to_string(),
                // No file_id: there is no server holding this and nothing to
                // fetch. The receiver's copy was already handed to the host by
                // Effect::SaveFile, and the sender has the original.
                meta: json!({
                    "size": size,
                    "p2p": true,
                    "dir": if sent { "send" } else { "recv" },
                    "peer": peer,
                }),
                ..Default::default()
            },
        );
    }

    /// Enter nearby mode with no account: the sign-in screen's second door.
    /// Everyone lands in the same default channel so five people who just want
    /// to talk do not each have to invent the same name.
    pub fn start_solo(&mut self, nick: &str, chan: &str) -> Effects {
        let mut fx = Effects::new();
        let name = nick.trim();
        if name.is_empty() {
            self.notice("pick a nick", "error");
            fx.changed();
            return fx;
        }
        self.user = Some(User { id: "local".into(), nick: name.into(), role: "member".into() });
        self.solo = true;
        self.nearby.on = true;
        let chan = self.nearby.join(if chan.is_empty() { "nearby" } else { chan });
        fx.push(Effect::MeshStart { nick: name.to_string() });
        fx.push(Effect::MeshJoin { chans: self.nearby.chans.clone() });
        // `live` is what stops a reconnect replaying forty banners; it is set
        // when a socket finishes catching up. There is no socket here, so
        // nothing would ever set it and every nearby message would be silently
        // unannounceable.
        self.live = true;
        self.rebuild_channels();
        self.active = format!("{NEARBY_PREFIX}{chan}");
        self.system_line(&format!("nearby #{chan} as {name}"), &self.active.clone());
        fx.changed();
        fx
    }

    /// `/nearby`, `/nearby-leave`, `/nearby-off`. Local by definition: there is
    /// no server in the loop to run them.
    fn nearby_command(&mut self, name: &str, rest: &str, fx: &mut Effects) {
        match name {
            "nearby-off" => {
                self.nearby.stop();
                self.events.retain(|k, _| !is_nearby(k));
                fx.push(Effect::MeshStop);
                if self.solo {
                    // Nothing behind nearby mode in a solo session: turning it
                    // off is signing out, and the sign-in screen is where that
                    // lands.
                    self.solo = false;
                    self.user = None;
                    self.active.clear();
                } else {
                    self.active =
                        self.server_channels.first().map(|c| c.id.clone()).unwrap_or_default();
                }
                self.rebuild_channels();
                self.notice("nearby mode off — this device is no longer announcing itself", "info");
            }
            "nearby-leave" => {
                let cid = self.active.clone();
                if !is_nearby(&cid) {
                    self.notice("not in a nearby channel", "error");
                    return;
                }
                self.nearby.leave(nearby_name(&cid));
                self.events.remove(&cid); // transient store: leaving IS the delete
                fx.push(Effect::MeshJoin { chans: self.nearby.chans.clone() });
                self.rebuild_channels();
                self.active = self
                    .server_channels
                    .first()
                    .map(|c| c.id.clone())
                    .or_else(|| self.channels.first().map(|c| c.id.clone()))
                    .unwrap_or_default();
            }
            _ => {
                if !self.nearby.on {
                    self.nearby.on = true;
                    let nick =
                        self.user.as_ref().map(|u| u.nick.clone()).unwrap_or_else(|| "someone".into());
                    fx.push(Effect::MeshStart { nick });
                    // Nearby is most useful exactly when the socket is down,
                    // and a socket that never connected leaves `live` false —
                    // which would silence every nearby arrival. Nearby traffic
                    // is live by definition: it has no backlog to replay.
                    self.live = true;
                }
                if !rest.trim().is_empty() {
                    let chan = self.nearby.join(rest);
                    fx.push(Effect::MeshJoin { chans: self.nearby.chans.clone() });
                    self.rebuild_channels();
                    self.active = format!("{NEARBY_PREFIX}{chan}");
                    self.system_line(&format!("nearby #{chan}"), &self.active.clone());
                    return;
                }
                let peers = &self.nearby.peers;
                let msg = if peers.is_empty() {
                    "nearby mode on — no peers yet, they appear within a few seconds".to_string()
                } else {
                    format!(
                        "{} nearby: {} — /nearby #name to open a channel",
                        peers.len(),
                        peers.iter().map(|p| p.nick.clone()).collect::<Vec<_>>().join(", ")
                    )
                };
                self.notice(&msg, "info");
                self.rebuild_channels();
            }
        }
    }

    // ---- transfers ----

    /// The host picked a file. Where it goes depends on where we are: a nearby
    /// channel means one peer over the mesh link, DCC style.
    pub fn offer_file(&mut self, name: &str, mime: &str, bytes: Vec<u8>) -> Effects {
        let mut fx = Effects::new();
        let cid = self.active.clone();
        if !is_nearby(&cid) {
            // A server channel uploads instead, which the host does over HTTP —
            // it has the token and the progress bar.
            self.notice("upload this file through the server", "info");
            fx.changed();
            return fx;
        }
        let nick = std::mem::take(&mut self.offer_to);
        let peer = self.nearby.by_nick(&nick).cloned();
        let Some(peer) = peer else {
            let msg = if nick.is_empty() {
                "say who it is for: /send <nick>, then pick the file".to_string()
            } else {
                format!("{nick} is not nearby")
            };
            self.notice(&msg, "error");
            fx.changed();
            return fx;
        };
        let chan = nearby_name(&cid).to_string();
        match self.xfer.offer(
            &peer.id, &peer.nick, &chan, name, mime, bytes, self.now_ms, &mut fx,
        ) {
            Ok(()) => self.notice(
                &format!("offered {name} to {} — waiting for them to accept", peer.nick),
                "info",
            ),
            Err(e) => self.notice(&e, "error"),
        }
        fx.changed();
        fx
    }

    pub fn accept_transfer(&mut self, id: &str) -> Effects {
        let mut fx = Effects::new();
        self.xfer.accept(id, &mut fx);
        fx.changed();
        fx
    }

    pub fn decline_transfer(&mut self, id: &str) -> Effects {
        let mut fx = Effects::new();
        self.xfer.decline(id, "declined", &mut fx);
        fx.changed();
        fx
    }

    // ---- blob surface (GUM) ----

    /// Open a blob, or "" to go back to the chat stream.
    pub fn open_blob(&mut self, blob: &str) -> Effects {
        let mut fx = Effects::new();
        self.gum.open(blob);
        if !blob.is_empty() {
            fx.send(Frame::new("blob.open", json!({ "channel": self.active, "blob": blob })));
        }
        fx.changed();
        fx
    }

    /// Jump to the nth entry in the switcher, 1-based, with chat as 1 — the
    /// order the switcher renders. `ctrl+1..9` on a desktop; the numbers inside
    /// a blob belong to its list, which is why this one is a separate door.
    pub fn blob_switch(&mut self, n: u32) -> Effects {
        if n == 0 {
            return Effects::new();
        }
        if n == 1 {
            return self.open_blob("");
        }
        match self.blobs().get(n as usize - 2) {
            Some(b) => {
                let id = b.id.clone();
                self.open_blob(&id)
            }
            None => Effects::new(),
        }
    }

    /// Every blob interaction lands here. The keyboard model is the core's, so
    /// "what does enter do" has one answer for every client.
    pub fn gum_move(&mut self, delta: i32) -> Effects {
        let mut fx = Effects::new();
        let cid = self.active.clone();
        self.gum.move_by(&cid, delta as isize);
        fx.changed();
        fx
    }

    pub fn gum_jump(&mut self, n: u32) -> Effects {
        let mut fx = Effects::new();
        let cid = self.active.clone();
        self.gum.jump(&cid, n as usize);
        fx.changed();
        fx
    }

    pub fn gum_set_draft(&mut self, block: &str, text: &str) -> Effects {
        let mut fx = Effects::new();
        self.gum.set_draft(block, text);
        fx.changed();
        fx
    }

    pub fn gum_activate(&mut self) -> Effects {
        let cid = self.active.clone();
        let out = self.gum.activate(&cid);
        self.apply(out)
    }

    pub fn gum_tap(&mut self, index: u32) -> Effects {
        let cid = self.active.clone();
        let out = self.gum.tap(&cid, index as usize);
        self.apply(out)
    }

    pub fn gum_tap_menu(&mut self, action_id: &str) -> Effects {
        let out = self.gum.tap_menu(action_id);
        self.apply(out)
    }

    pub fn gum_escape(&mut self) -> Effects {
        let out = self.gum.escape();
        self.apply(out)
    }

    /// The host answered an [`Effect::AskConfirm`] or [`Effect::AskPrompt`].
    pub fn gum_answer(&mut self, ok: bool, value: &str) -> Effects {
        let out = self.gum.answer(ok, value);
        self.apply(out)
    }

    fn apply(&mut self, out: Outcome) -> Effects {
        let mut fx = Effects::new();
        match out {
            Outcome::Nothing => {}
            Outcome::Fire { action, item, value } => fx.send(Frame::new(
                "blob.event",
                json!({
                    "channel": self.active, "blob": self.gum.active,
                    "action": action, "item": item, "value": value
                }),
            )),
            Outcome::Confirm(message) => fx.push(Effect::AskConfirm { message }),
            Outcome::Prompt(message) => fx.push(Effect::AskPrompt { message }),
            Outcome::Leave => self.gum.open(""),
        }
        fx.changed();
        fx
    }

    /// The apps in the channel being looked at.
    pub fn blobs(&self) -> Vec<BlobSpec> {
        self.channel(&self.active).map(|c| c.blobs.clone()).unwrap_or_default()
    }

    // ---- selectors ----

    pub fn notice(&mut self, text: &str, level: &str) {
        self.notices.push(Notice {
            channel: self.active.clone(),
            level: level.into(),
            text: text.into(),
        });
        self.trim_notices();
    }

    pub fn set_active(&mut self, cid: &str) -> Effects {
        let mut fx = Effects::new();
        // A blob belongs to the channel it is attached to, so changing rooms
        // goes back to the chat stream rather than showing the last room's app.
        if self.active != cid {
            self.gum.open("");
            // A reply is aimed at one message in one room. Carrying it across a
            // channel switch would quote a line nobody there can see.
            self.reply_to.clear();
        }
        self.active = cid.to_string();
        self.mark_read(cid, &mut fx);
        fx.changed();
        fx
    }

    /// Aim the next message at an event in this channel; `""` cancels. Checked
    /// here, so a frontend cannot leave the composer claiming to answer
    /// something this device cannot even read.
    pub fn set_reply(&mut self, event_id: &str) -> Effects {
        let mut fx = Effects::new();
        self.reply_to = match self.readable(event_id) {
            Some(_) => event_id.to_string(),
            None => String::new(),
        };
        fx.changed();
        fx
    }

    /// An event in the active channel, as (actor, readable body). `None` when
    /// it is gone or this device has no key for it — which is exactly when
    /// there is nothing to quote.
    fn readable(&self, event_id: &str) -> Option<(String, String)> {
        let ev = self.events.get(&self.active)?.iter().find(|e| e.id == event_id)?;
        let body = if ev.enc { self.plain.get(&ev.id).cloned()? } else { ev.body.clone() };
        Some((ev.actor.clone(), body))
    }

    /// The quote line for the pending reply, ready to prepend.
    fn reply_quote(&self) -> Option<String> {
        let (actor, body) = self.readable(&self.reply_to)?;
        Some(quote_line(&actor, &body))
    }

    /// What the composer shows above the input: `<alice> what she said`,
    /// clipped exactly as the line that will be sent. Empty when none pends.
    pub fn reply_display(&self) -> String {
        self.reply_quote()
            .map(|l| l.trim_start_matches("> ").trim_end().to_string())
            .unwrap_or_default()
    }

    /// The host says the active channel is actually on a focused screen.
    pub fn mark_active_read(&mut self) -> Effects {
        let mut fx = Effects::new();
        let cid = self.active.clone();
        self.mark_read(&cid, &mut fx);
        fx
    }

    /// The only writer of the read mark. One writer is the whole design: the
    /// recount is local and exact, so there is no optimistic second path to
    /// race with it. Keep it that way — a second writer is how this counter
    /// gets stuck.
    pub fn mark_read(&mut self, cid: &str, fx: &mut Effects) {
        let Some(ch) = self.channels.iter().find(|c| c.id == cid) else { return };
        if self.read.get(cid) == Some(&ch.seq) {
            return;
        }
        let seq = ch.seq;
        self.read.insert(cid.to_string(), seq);
        self.save_cache(fx);
        // Straight up to the server, which relays it to this account's other
        // devices. Reading on the phone is what clears the badge on the desktop.
        if self.online && !is_local_channel(cid) {
            fx.send(Frame::new("read", json!({ "marks": { cid: seq } })));
        }
    }


    /// Unread is (the server's seq) minus (the read mark) — never a count of
    /// the events we happen to hold. The cache keeps the last 500 per channel,
    /// so counting rows would silently cap the badge at the cache size.
    pub fn unread(&self, cid: &str) -> i64 {
        let Some(ch) = self.channels.iter().find(|c| c.id == cid) else { return 0 };
        let Some(mark) = self.read.get(cid) else { return 0 };
        (ch.seq - mark).max(0)
    }

    pub fn unread_total(&self) -> i64 {
        self.channels.iter().map(|c| self.unread(&c.id)).sum()
    }

    /// Does this line tag me: `@nick` as a whole word, any case?
    pub fn is_mention(&self, body: &str) -> bool {
        let Some(u) = &self.user else { return false };
        let hay = body.to_lowercase();
        let tag = format!("@{}", u.nick.to_lowercase());
        hay.match_indices(&tag).any(|(i, _)| {
            hay[i + tag.len()..].chars().next().is_none_or(|c| !c.is_alphanumeric() && c != '_')
        })
    }

    /// Does this line match one of my highlight words, or tag me?
    pub fn is_highlight(&self, body: &str, actor: &str) -> bool {
        if Some(actor) == self.user.as_ref().map(|u| u.nick.as_str()) {
            return false;
        }
        let hay = body.to_lowercase();
        self.is_mention(body) || self.highlights.iter().any(|w| hay.contains(&w.to_lowercase()))
    }

    /// Commands the palette should offer here, filtered by scope and role.
    pub fn available_commands(&self) -> Vec<&Command> {
        let ch = self.channels.iter().find(|c| c.id == self.active);
        let rank = |r: &str| match r {
            "op" => 1,
            "admin" => 2,
            _ => 0,
        };
        let have = rank(ch.map(|c| c.role.as_str()).unwrap_or(""))
            .max(rank(self.user.as_ref().map(|u| u.role.as_str()).unwrap_or("member")));
        self.commands
            .iter()
            .filter(|c| {
                if c.scope == "channel" && ch.is_none_or(|x| x.is_dm) {
                    return false;
                }
                if c.scope == "dm" && !ch.is_some_and(|x| x.is_dm) {
                    return false;
                }
                have >= rank(&c.min_role)
            })
            .collect()
    }

    /// The palette is built entirely from the server-pushed manifest.
    pub fn match_commands(&self, text: &str) -> Vec<&Command> {
        if !text.starts_with('/') || text.contains(' ') {
            return Vec::new(); // not a command, or already typing arguments
        }
        let q = text[1..].to_lowercase();
        self.available_commands().into_iter().filter(|c| c.name.starts_with(&q)).take(8).collect()
    }

    /// The stream: what the channel window draws. A quiet line is skipped
    /// here and nowhere else — it is stored, synced and searched like any
    /// other message, and the room's own log still shows it.
    pub fn visible_events(&self, cid: &str) -> Vec<&Ev> {
        self.events
            .get(cid)
            .map(|e| {
                e.iter()
                    .filter(|x| !self.ignores.contains(&x.actor) && !is_quiet(x))
                    .collect()
            })
            .unwrap_or_default()
    }

    /// The tail of the channel as the 3D room's log wants it: quiet lines
    /// included, newest last. The room shows the last few on arrival so you
    /// walk in with the conversation rather than into the middle of it.
    pub fn room_log(&self, cid: &str, limit: usize) -> Vec<&Ev> {
        let all = self
            .events
            .get(cid)
            .map(|e| e.iter().filter(|x| !self.ignores.contains(&x.actor)).collect::<Vec<_>>())
            .unwrap_or_default();
        let from = all.len().saturating_sub(limit);
        all[from..].to_vec()
    }

    /// Fold the occupancy snapshot out of the channel list the server just
    /// sent. Wholesale: the server's list is the truth, and the relays that
    /// arrive after it are the deltas.
    fn reseed_rooms(&mut self) {
        self.rooms = self
            .server_channels
            .iter()
            .filter_map(|c| c.room.clone().map(|r| (c.id.clone(), r)))
            .filter(|(_, r)| !r.users.is_empty())
            .collect();
    }

    /// The live strip, one per channel with anybody in its room.
    pub fn room_strips(&self) -> Vec<RoomStrip> {
        let me = self.user.as_ref().map(|u| u.id.as_str()).unwrap_or("");
        self.rooms
            .iter()
            .filter(|(_, here)| !here.users.is_empty())
            .map(|(cid, here)| {
                // Which sub-room the strip names: where you are if you are in
                // there, otherwise where most of them are.
                let mine = here.users.iter().find(|p| p.user_id == me).map(|p| p.room.clone());
                let room = mine.clone().unwrap_or_else(|| busiest_room(&here.users));
                RoomStrip {
                    channel: cid.clone(),
                    room,
                    since: here.since.clone(),
                    people: here.users.clone(),
                    voice_count: here.users.iter().filter(|p| p.voice).count(),
                    you_inside: mine.is_some(),
                    you_quiet: self.room_quiet,
                }
            })
            .collect()
    }

    pub fn channel(&self, cid: &str) -> Option<&ChannelView> {
        self.channels.iter().find(|c| c.id == cid)
    }

    pub fn channels(&self) -> &[ChannelView] {
        &self.channels
    }

    pub fn commands(&self) -> &[Command] {
        &self.commands
    }

    /// The decrypted text of an encrypted event, if this device could read it.
    /// `None` is not a failure to be retried — it is what the UI renders an
    /// honest "cannot decrypt" line from, because a device with no key for an
    /// epoch never gets one for those messages.
    pub fn plain_body(&self, event_id: &str) -> Option<String> {
        self.plain.get(event_id).cloned()
    }

    pub fn notices_for(&self, cid: &str) -> Vec<&Notice> {
        self.notices.iter().filter(|n| n.channel == cid || n.channel.is_empty()).collect()
    }

    // ---- system messages (the MOTD, and this build's release notes) ----

    /// What to draw at the live end of the stream, and where.
    ///
    /// The MOTD is the operator talking to the team, so it goes in #general,
    /// once, until it is dismissed or edited. The release notes are us talking
    /// about the build, which is nobody's conversation — they live in
    /// #system, all of them, oldest first, with nothing to dismiss because
    /// a log you can empty is not a log.
    pub fn system_messages(&self) -> Vec<&SysMsg> {
        if self.active == SYS_ID {
            return self.news.iter().collect();
        }
        if self.channel(&self.active).map(|c| c.name.as_str()) != Some("general") {
            return Vec::new();
        }
        self.motd.iter().filter(|m| !self.seen_sys.contains(&m.id)).chain(&self.tour).collect()
    }

    /// The reader closed one. It stays closed on this device until the message
    /// itself changes — a MOTD's id is a digest of its text, so editing it
    /// brings it back and re-saving the same words does not.
    pub fn dismiss_system(&mut self, id: &str) -> Effects {
        let mut fx = Effects::new();
        if self.tour.as_ref().is_some_and(|t| t.id == id) {
            self.tour = None;
            self.save_cache(&mut fx);
            fx.changed();
            return fx;
        }
        if self.seen_sys.insert(id.to_string()) {
            self.save_cache(&mut fx);
            fx.changed();
        }
        fx
    }
}

fn commands_of(d: &Value) -> Vec<Command> {
    d.get("commands").and_then(|c| serde_json::from_value(c.clone()).ok()).unwrap_or_default()
}

/// The first-run tour: a MOTD-styled card that teaches `/`. It is drawn by
/// the same card as the operator's MOTD, so every frontend already has it.
fn tour_msg() -> SysMsg {
    SysMsg {
        id: "tour".into(),
        kind: "motd".into(),
        icon: String::new(),
        title: "Welcome to gliph".into(),
        body: "Type / to see every command on this server. Try /list for the channels, then /join #name.".into(),
        date: String::new(),
    }
}

/// `hello.server.motd`, or null when the operator has not set one.
fn motd_of(v: Option<&Value>) -> Option<SysMsg> {
    let m = v?;
    let text = m.get("text").and_then(Value::as_str).unwrap_or_default();
    if text.trim().is_empty() {
        return None;
    }
    Some(SysMsg {
        id: m.get("id").and_then(Value::as_str).unwrap_or("motd").to_string(),
        kind: "motd".into(),
        icon: m.get("icon").and_then(Value::as_str).unwrap_or_default().to_string(),
        title: String::new(),
        body: text.to_string(),
        date: String::new(),
    })
}

/// `hello.server.news`: the newest few release notes, oldest first.
fn news_of(v: Option<&Value>) -> Vec<SysMsg> {
    let Some(list) = v.and_then(Value::as_array) else { return Vec::new() };
    list.iter()
        .filter_map(|n| {
            let id = n.get("id").and_then(Value::as_str)?;
            Some(SysMsg {
                id: id.to_string(),
                kind: "news".into(),
                icon: String::new(),
                title: n.get("title").and_then(Value::as_str).unwrap_or_default().to_string(),
                body: n.get("body").and_then(Value::as_str).unwrap_or_default().to_string(),
                date: n.get("date").and_then(Value::as_str).unwrap_or_default().to_string(),
            })
        })
        .collect()
}

fn hex_bytes(s: &str) -> Option<Vec<u8>> {
    (0..s.len()).step_by(2).map(|i| s.get(i..i + 2).and_then(|b| u8::from_str_radix(b, 16).ok())).collect()
}

fn split_command(s: &str) -> (String, String) {
    // Any whitespace ends the name: `/script add x` followed by a newline and
    // a pasted body is still the `script` command.
    match s.find(char::is_whitespace) {
        Some(i) => (s[..i].to_lowercase(), s[i..].trim().to_string()),
        None => (s.to_lowercase(), String::new()),
    }
}

/// Milliseconds since the epoch for an RFC 3339 timestamp, without pulling in a
/// date library: only the arithmetic the announce age guard needs.
/// The inverse of `parse_ms`: epoch millis to the same UTC RFC 3339 spelling
/// the server uses, so a locally-made line dates like every other one.
fn fmt_ms(ms: i64) -> String {
    if ms <= 0 {
        return String::new();
    }
    let (days, rem) = (ms.div_euclid(86_400_000), ms.rem_euclid(86_400_000) / 1_000);
    // Civil-from-days, the exact inverse of the algorithm in `parse_ms`.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let mo = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(mo <= 2);
    format!(
        "{y:04}-{mo:02}-{d:02}T{:02}:{:02}:{:02}Z",
        rem / 3_600,
        (rem / 60) % 60,
        rem % 60
    )
}

fn parse_ms(ts: &str) -> Option<i64> {
    if ts.len() < 19 {
        return None;
    }
    let b = ts.as_bytes();
    let num = |from: usize, to: usize| ts[from..to].parse::<i64>().ok();
    if b[4] != b'-' || b[7] != b'-' || b[10] != b'T' {
        return None;
    }
    let (y, mo, d) = (num(0, 4)?, num(5, 7)?, num(8, 10)?);
    let (h, mi, s) = (num(11, 13)?, num(14, 16)?, num(17, 19)?);
    // Days since the Unix epoch, by the civil-from-days algorithm (Howard
    // Hinnant's), which is exact for every date this will ever see.
    let y = if mo <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (mo + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    Some(((days * 86_400) + h * 3_600 + mi * 60 + s) * 1_000)
}

/// A line the sender kept out of the stream. Stored, searched, and shown in
/// the room it was said in — just not in the channel window.
fn is_quiet(e: &Ev) -> bool {
    e.meta.get("quiet").and_then(Value::as_bool).unwrap_or(false)
}

/// Where most of a room's people are standing. Ties go to whichever the
/// server listed first, which is alphabetical by nick.
fn busiest_room(users: &[RoomPerson]) -> String {
    let mut best = ("", 0usize);
    for u in users {
        let n = users.iter().filter(|x| x.room == u.room).count();
        if n > best.1 {
            best = (&u.room, n);
        }
    }
    best.0.to_string()
}

/// One channel's room, ready to draw: the pinned strip above the composer.
#[derive(Debug, Clone, serde::Serialize)]
pub struct RoomStrip {
    pub channel: String,
    /// The sub-room the strip names: yours if you are inside, else the busiest.
    pub room: String,
    /// RFC3339, from the server's snapshot. Empty until a `state` frame dates
    /// a room this client only ever saw people walk into.
    pub since: String,
    pub people: Vec<RoomPerson>,
    pub voice_count: usize,
    pub you_inside: bool,
    pub you_quiet: bool,
}
