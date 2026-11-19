//! Wire types. Mirrors docs/protocol.md — keep the two in step.
//!
//! Everything is JSON on one WebSocket: `{"t": "<type>", "d": {...}}`. These
//! types are deliberately permissive: the server owns the shape, and a field
//! this client has never heard of must survive rather than be dropped by an
//! over-eager model. Hence `serde_json::Value` for `d` and `meta`.

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Frame {
    pub t: String,
    #[serde(default)]
    pub d: Value,
}

impl Frame {
    pub fn new(t: &str, d: Value) -> Self {
        Frame { t: t.to_string(), d }
    }
    /// A field out of `d`, as a string. Absent and wrong-typed are the same
    /// thing to a client: nothing usable.
    pub fn str(&self, key: &str) -> String {
        self.d.get(key).and_then(Value::as_str).unwrap_or_default().to_string()
    }
    pub fn int(&self, key: &str) -> i64 {
        self.d.get(key).and_then(Value::as_i64).unwrap_or_default()
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Arg {
    #[serde(default)]
    pub name: String,
    #[serde(default = "text")]
    pub r#type: String,
    #[serde(default)]
    pub required: bool,
}

fn text() -> String {
    "text".into()
}

/// One palette entry. The client NEVER hardcodes commands: this whole list
/// arrives from the server and is replaced whenever a plugin comes or goes.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Command {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub usage: String,
    #[serde(default)]
    pub desc: String,
    #[serde(default)]
    pub args: Option<Vec<Arg>>,
    #[serde(default = "any")]
    pub scope: String,
    #[serde(default = "member", rename = "min_role")]
    pub min_role: String,
    #[serde(default = "builtin")]
    pub source: String,
}

fn any() -> String {
    "any".into()
}
fn member() -> String {
    "member".into()
}
fn builtin() -> String {
    "builtin".into()
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Member {
    #[serde(default, rename = "user_id")]
    pub user_id: String,
    #[serde(default)]
    pub nick: String,
    #[serde(default = "member")]
    pub role: String,
    #[serde(default)]
    pub away: Option<String>,
    #[serde(default = "user")]
    pub kind: String,
    /// Holds a socket right now. Filled by the server, never stored.
    #[serde(default)]
    pub online: bool,
    /// Has a device nobody approved yet. Peers seal it nothing; the UI flags it.
    #[serde(default)]
    pub unverified: bool,
}

fn open() -> String {
    "open".into()
}

fn user() -> String {
    "user".into()
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ChannelView {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub topic: String,
    #[serde(default)]
    pub modes: String,
    /// "open" (joiners read what was said before) or "sealed" (they start at
    /// their join). Chosen at creation and never changed.
    #[serde(default = "open")]
    pub history: String,
    #[serde(default, rename = "is_dm")]
    pub is_dm: bool,
    #[serde(default = "member")]
    pub role: String,
    #[serde(default)]
    pub seq: i64,
    /// How far this user has read in this channel, according to the server,
    /// across every device they own. Absent from an older server, which
    /// deserialises to 0 and simply leaves the local mark alone.
    #[serde(default)]
    pub read: i64,
    #[serde(default)]
    pub members: Vec<Member>,
    /// The mini-apps attached to this channel, rendered in the switcher. Kept
    /// on the channel because that is where the server puts them, and because
    /// which apps exist is a property of the room.
    #[serde(default)]
    pub blobs: Vec<crate::gum::BlobSpec>,
    /// Who is standing in this channel's 3D room right now. The server sends
    /// it only while somebody is inside; the comings and goings after this
    /// snapshot arrive as `room.event` relays.
    #[serde(default)]
    pub room: Option<RoomHere>,
}

/// A channel's room, as the server last described it.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct RoomHere {
    /// When the room went from empty to occupied, RFC3339. The elapsed time is
    /// the frontends' to format: the core has no clock of its own.
    #[serde(default)]
    pub since: String,
    #[serde(default)]
    pub users: Vec<RoomPerson>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct RoomPerson {
    #[serde(default)]
    pub user_id: String,
    #[serde(default)]
    pub name: String,
    /// Which sub-room: office, lounge, dunes...
    #[serde(default)]
    pub room: String,
    #[serde(default)]
    pub voice: bool,
}

impl ChannelView {
    pub fn encrypted(&self) -> bool {
        self.modes.contains('e')
    }
}

/// `seq` is a float rather than an integer on purpose: locally-made lines (a
/// netsplit report, an unsent message) sit at `n + 0.5` so they sort into place
/// without ever colliding with a number the server issued.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Ev {
    #[serde(default)]
    pub seq: f64,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub channel: String,
    #[serde(default = "message")]
    pub kind: String,
    #[serde(default)]
    pub actor: String,
    #[serde(default)]
    pub ts: String,
    /// Plaintext for server-generated events; base64 ciphertext when `enc`.
    #[serde(default)]
    pub body: String,
    #[serde(default, skip_serializing_if = "is_false")]
    pub enc: bool,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub epoch: i64,
    #[serde(default, skip_serializing_if = "Value::is_null")]
    pub meta: Value,
}

fn message() -> String {
    "message".into()
}
fn is_false(b: &bool) -> bool {
    !*b
}
fn is_zero(n: &i64) -> bool {
    *n == 0
}

impl Ev {
    pub fn meta_str(&self, key: &str) -> String {
        self.meta.get(key).and_then(Value::as_str).unwrap_or_default().to_string()
    }
    pub fn meta_bool(&self, key: &str) -> bool {
        self.meta.get(key).and_then(Value::as_bool).unwrap_or(false)
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct User {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub nick: String,
    #[serde(default = "member")]
    pub role: String,
}

/// A system message: the MOTD an operator set, or the notes for the release
/// this client is running. Not chat, and deliberately not an [`Ev`] — it has
/// no author, no channel and no sequence number, and putting it in the event
/// stream would have meant inventing all three.
///
/// `icon` is a data URL (the operator's own icon) or "" for ours. The bytes
/// are inline because a chat client must not fetch a remote image to render a
/// line: see web/src/cards.ts for what that costs a reader.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SysMsg {
    pub id: String,
    /// `motd` or `news`. The frontends draw one card either way; the kind is
    /// what tells a reader which of the two they are looking at.
    pub kind: String,
    #[serde(default)]
    pub icon: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub date: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Notice {
    pub channel: String,
    pub level: String,
    pub text: String,
}

/// A device's public key, as `keys.peers` lists it.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct DeviceKey {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub pubkey: String,
    #[serde(default, rename = "user_id")]
    pub user_id: String,
    #[serde(default)]
    pub name: String,
    /// The owner's master key's signature over this device. Empty until the
    /// device is approved; a peer seals nothing to a device without one.
    #[serde(default)]
    pub sig: String,
    /// The owner's master public key, as the server has it on file.
    #[serde(default)]
    pub master: String,
    /// Registered before device signing existed, so the account's first
    /// signer may vouch for it.
    #[serde(default)]
    pub legacy: bool,
}

impl DeviceKey {
    pub fn signed(&self) -> bool {
        crate::crypto::verify_device(&self.master, &self.user_id, &self.id, &self.pubkey, &self.sig)
    }
}

pub fn frame(t: &str, d: Value) -> Frame {
    Frame::new(t, d)
}

/// `{"channel": cid}` — the shape half the key frames take.
pub fn chan(cid: &str) -> Value {
    json!({ "channel": cid })
}

// ---- replies ----

/// A reply carries what it answers on its own first line, inside the body:
///
/// ```text
/// > <alice> what she said
/// my reply
/// ```
///
/// ponytail: no `meta.reply_to`. That means no protocol, server or database
/// change — and in an end-to-end encrypted product it means the server never
/// learns who answers whom, because the quote is inside the ciphertext with
/// everything else. The cost is that the quote is a copy: it cannot scroll to
/// the original and it does not follow an edit. Add `meta.reply_to` the day
/// either of those is wanted.
///
/// It lives here, in the wire module, because it is a format both clients have
/// to agree on exactly — the one thing this crate exists to stop them
/// disagreeing about.
const QUOTE_MAX: usize = 140;

/// Split a body into its quote line and what was actually said. The quote is
/// `""` for an ordinary message, which is every message that is not a reply.
pub fn split_quote(body: &str) -> (&str, &str) {
    match body.strip_prefix("> ").and_then(|rest| rest.split_once('\n')) {
        Some((quote, said)) => (quote, said),
        None => ("", body),
    }
}

/// The quote line to put in front of a reply, newline included. Always exactly
/// one line — the newline is the delimiter — and replying to a reply quotes
/// what it said, not what it was quoting.
pub fn quote_line(actor: &str, body: &str) -> String {
    let said = split_quote(body).1.split_whitespace().collect::<Vec<_>>().join(" ");
    let mut short: String = said.chars().take(QUOTE_MAX).collect();
    if short.chars().count() < said.chars().count() {
        short.push('…');
    }
    format!("> <{actor}> {short}\n")
}
