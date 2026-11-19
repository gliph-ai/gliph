//! The host-facing surface, shared by the wasm and UniFFI bindings.
//!
//! Everything crosses as JSON. That is a deliberate choice: modelling the whole
//! type graph twice — once in `wasm_bindgen`, once in a UDL — is a second and a
//! third place for the wire to drift, which is the exact problem this crate
//! exists to end. One `serde` derive serves both, a host decodes it with
//! whatever is idiomatic there (`JSON.parse`, `Codable`), and adding a field is
//! one edit rather than three.
//!
//! The cost is a serialise on each call. For a chat client re-rendering a few
//! hundred rows that is not close to mattering; if it ever does, the boundary
//! to widen is the snapshot, not the effects.

use serde::Serialize;

use crate::client::Client;
use crate::effect::Effects;
use crate::wire::{split_quote, ChannelView, Command, Ev, Notice, SysMsg, User};
use crate::xfer::Transfer;

/// Everything the chrome renders from, in one call. Deliberately not the event
/// stream: that is per-channel and much bigger, so it has its own accessor.
#[derive(Serialize)]
pub struct Snapshot<'a> {
    pub server: &'a str,
    pub server_name: &'a str,
    pub signed_in: bool,
    pub online: bool,
    pub solo: bool,
    pub active: &'a str,
    pub user: Option<&'a User>,
    pub channels: Vec<ChannelRow<'a>>,
    pub unread_total: i64,
    pub notices: Vec<&'a Notice>,
    /// The MOTD and any unread release notes, drawn at the live end of the
    /// stream. Empty almost always.
    pub system: Vec<&'a SysMsg>,
    pub transfers: &'a [Transfer],
    pub nearby_on: bool,
    pub nearby_peers: usize,
    pub upload_max: i64,
    /// How big the palette is, and how much of it a plugin brought. Counted
    /// here so a UI that wants the number does not have to fetch the whole
    /// palette to length it — which is how the iOS rail ended up leaving the
    /// command list on screen.
    pub command_count: usize,
    pub plugin_command_count: usize,
    /// Which blob is open ("" = the chat stream). The chrome needs it to know
    /// whether to draw the composer.
    pub active_blob: &'a str,
    /// `<alice> what she said` while a reply is pending, "" otherwise. The
    /// composer draws this strip; the quote itself is put on the wire by the
    /// core when the message is sent.
    pub reply: String,
    /// One per channel with anybody in its 3D room: the live strip above the
    /// composer. Empty almost always.
    pub rooms: Vec<crate::client::RoomStrip>,
    /// This device's standing with its account: approved or waiting, its
    /// approval code, and any other device of the account waiting on us.
    pub device: &'a crate::client::Trust,
}

#[derive(Serialize)]
pub struct ChannelRow<'a> {
    #[serde(flatten)]
    pub view: &'a ChannelView,
    /// Folded in here rather than computed twice in two UIs, and it is the
    /// number people notice being wrong.
    pub unread: i64,
}

/// One event, with the decryption already resolved. A host must never have to
/// decide what to show for a body it cannot read.
#[derive(Serialize)]
pub struct EventRow<'a> {
    #[serde(flatten)]
    pub ev: &'a Ev,
    /// The readable text: the body for a plain event, the decrypted copy for an
    /// encrypted one, and `null` when this device has no key — which is what
    /// the UI renders its honest "cannot decrypt" line from.
    ///
    /// For a reply this is what was said, with the quote already lifted off
    /// into `quote`. A frontend never parses a body: it draws two strings.
    pub text: Option<String>,
    /// `<alice> what she said` when this event is a reply, "" otherwise.
    pub quote: String,
    pub highlight: bool,
}

pub fn snapshot_json(c: &Client) -> String {
    let snap = Snapshot {
        server: &c.server,
        server_name: &c.server_name,
        signed_in: !c.token.is_empty() || c.solo,
        online: c.online,
        solo: c.solo,
        active: &c.active,
        user: c.user.as_ref(),
        channels: c
            .channels()
            .iter()
            .map(|v| ChannelRow { view: v, unread: c.unread(&v.id) })
            .collect(),
        unread_total: c.unread_total(),
        notices: c.notices_for(&c.active),
        system: c.system_messages(),
        transfers: &c.xfer.list,
        nearby_on: c.nearby.on,
        nearby_peers: c.nearby.peers.len(),
        upload_max: c.upload_max,
        command_count: c.commands().len(),
        plugin_command_count: c.commands().iter().filter(|x| x.source != "builtin").count(),
        active_blob: &c.gum.active,
        reply: c.reply_display(),
        rooms: c.room_strips(),
        device: &c.trust,
    };
    serde_json::to_string(&snap).unwrap_or_else(|_| "{}".into())
}

pub fn events_json(c: &Client, cid: &str) -> String {
    let rows: Vec<EventRow> = c
        .visible_events(cid)
        .into_iter()
        .map(|ev| {
            let body = if ev.enc { c.plain_body(&ev.id) } else { Some(ev.body.clone()) };
            let (quote, text) = match &body {
                Some(b) => {
                    let (q, said) = split_quote(b);
                    (q.to_string(), Some(said.to_string()))
                }
                None => (String::new(), None),
            };
            let highlight = text
                .as_deref()
                .is_some_and(|t| ev.kind == "message" && c.is_highlight(t, &ev.actor));
            EventRow { ev, text, quote, highlight }
        })
        .collect();
    serde_json::to_string(&rows).unwrap_or_else(|_| "[]".into())
}

/// The tail of a channel as the 3D room's log draws it. Quiet lines included:
/// the room is where they are meant to be read.
pub fn room_log_json(c: &Client, cid: &str, limit: u32) -> String {
    let rows: Vec<EventRow> = c
        .room_log(cid, limit as usize)
        .into_iter()
        .map(|ev| {
            let body = if ev.enc { c.plain_body(&ev.id) } else { Some(ev.body.clone()) };
            let (quote, text) = match &body {
                Some(b) => {
                    let (q, said) = split_quote(b);
                    (q.to_string(), Some(said.to_string()))
                }
                None => (String::new(), None),
            };
            EventRow { ev, text, quote, highlight: false }
        })
        .collect();
    serde_json::to_string(&rows).unwrap_or_else(|_| "[]".into())
}

/// The blob surface, ready to draw: nodes with selection, numbering and menu
/// state already resolved. A frontend paints this and reports taps back.
pub fn surface_json(c: &Client) -> String {
    let surface = c.gum.surface(&c.active, c.blobs());
    serde_json::to_string(&surface).unwrap_or_else(|_| "{}".into())
}

pub fn palette_json(c: &Client, text: &str) -> String {
    let cmds: Vec<&Command> = c.match_commands(text);
    serde_json::to_string(&cmds).unwrap_or_else(|_| "[]".into())
}

pub fn effects_json(fx: &Effects) -> String {
    serde_json::to_string(fx).unwrap_or_else(|_| "[]".into())
}
