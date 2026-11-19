//! Nearby mesh, client side (spec F9).
//!
//! The transport is native and stays native — Rust over mdns-sd on desktop,
//! Swift over Network.framework on iOS — because discovery needs Bonjour and a
//! listening socket, and neither a browser tab nor this core has either. Both
//! speak the same wire (docs/protocol.md, "Nearby mesh"), so a Mac and an
//! iPhone are peers in ONE mesh.
//!
//! What lives here is the mesh's SIDE of the client: which channels we are in,
//! who is nearby, and the four commands nearby mode adds to the palette. It
//! knows nothing about sockets and nothing about the server.
//!
//! ponytail: nearby traffic is NOT encrypted. Everything here is one Wi-Fi hop,
//! ephemeral, and gone when the last peer leaves; the honest fix is a per-link
//! ECDH handshake (crypto.rs already has every primitive it needs) with
//! fingerprints in the member list. Until it lands the client says so in the
//! channel's topic — see [`NEARBY_TOPIC`].

use serde::{Deserialize, Serialize};

use crate::wire::{Arg, Command};

/// Nearby channel ids are namespaced so nothing can confuse one with a server
/// channel: the client routes on this prefix in exactly one place.
pub const NEARBY_PREFIX: &str = "n_";
pub const NEARBY_TOPIC: &str = "nearby — peer-to-peer, unencrypted, nothing is kept";

pub fn is_nearby(cid: &str) -> bool {
    cid.starts_with(NEARBY_PREFIX)
}

pub fn nearby_name(cid: &str) -> &str {
    cid.strip_prefix(NEARBY_PREFIX).unwrap_or(cid)
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Peer {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub nick: String,
    #[serde(default)]
    pub chans: Vec<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct MeshMsg {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub chan: String,
    #[serde(default)]
    pub nick: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub ts: String,
    /// `message` or `action` (/me). Defaulted, so a peer on an older build —
    /// which sends neither this nor `reply_to` — still reads as an ordinary
    /// line rather than dropping off the mesh.
    #[serde(default)]
    pub kind: String,
    /// The message this answers, by mesh id. The quote itself rides in `body`
    /// like everywhere else; this is only what makes it a jump.
    #[serde(default)]
    pub reply_to: String,
}

#[derive(Default)]
pub struct Nearby {
    pub on: bool,
    pub peers: Vec<Peer>,
    /// channels WE are in, in join order
    pub chans: Vec<String>,
}

impl Nearby {
    /// The peer behind a nick, for addressing a transfer at one person.
    pub fn by_nick(&self, nick: &str) -> Option<&Peer> {
        self.peers.iter().find(|p| p.nick == nick)
    }

    /// Peers advertising a channel we are also in, as nicks.
    pub fn peers_in(&self, chan: &str) -> Vec<String> {
        self.peers
            .iter()
            .filter(|p| p.chans.iter().any(|c| c == chan))
            .map(|p| p.nick.clone())
            .collect()
    }

    /// Returns the normalised channel name, and whether it was newly joined.
    pub fn join(&mut self, name: &str) -> String {
        let chan = name.trim().trim_start_matches('#').to_lowercase();
        if chan.is_empty() {
            return chan;
        }
        if !self.chans.iter().any(|c| *c == chan) {
            self.chans.push(chan.clone());
        }
        chan
    }

    pub fn leave(&mut self, name: &str) {
        self.chans.retain(|c| c != name);
    }

    pub fn stop(&mut self) {
        self.on = false;
        self.peers.clear();
        self.chans.clear();
    }
}

fn cmd(name: &str, usage: &str, desc: &str, scope: &str, args: Vec<Arg>) -> Command {
    Command {
        name: name.into(),
        usage: usage.into(),
        desc: desc.into(),
        args: Some(args),
        scope: scope.into(),
        min_role: "member".into(),
        source: "nearby".into(),
    }
}

/// The commands nearby mode adds to the palette. They are DATA, merged into the
/// same manifest the server sends (CLAUDE.md: clients never hardcode a command
/// list) — nearby is just another source, like a plugin, and when nearby mode is
/// off these are not in the palette at all.
pub fn nearby_commands() -> Vec<Command> {
    vec![
        cmd(
            "nearby",
            "/nearby [#channel]",
            "who is on this network — with a channel, join it peer-to-peer",
            "any",
            vec![Arg { name: "channel".into(), r#type: "channel".into(), required: false }],
        ),
        cmd("nearby-leave", "/nearby-leave", "leave this nearby channel", "channel", vec![]),
        cmd(
            "send",
            "/send <nick>",
            "offer a file to someone nearby, mIRC style",
            "channel",
            vec![Arg { name: "nick".into(), r#type: "nick".into(), required: true }],
        ),
        cmd(
            "nearby-off",
            "/nearby-off",
            "stop announcing this device on the local network",
            "any",
            vec![],
        ),
    ]
}
