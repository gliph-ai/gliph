//! The servers this account is signed in to, and which one is on screen.
//!
//! A Gliph server IS a team: its own database, its own accounts, its own
//! `#general`. So "switching teams" is switching servers, the way mIRC switched
//! networks — and your identity is per-server too. You are `<waseem>` on one and
//! `<wsm>` on another, because those are two rows in two different databases.
//!
//! This registry is deliberately NOT part of [`Client`](crate::client::Client).
//! A `Client` is one server session and `logout` resets it wholesale; the list
//! of servers has to outlive that. [`GliphClient`](crate::host::GliphClient)
//! owns one of each.
//!
//! What the host stores is one JSON blob under `gliph.servers`. The rules that
//! matter — how an address is normalised, that a host appears once, that
//! `active` always names a server that exists — live here so they are the same
//! on every platform and testable without a network.

use serde::{Deserialize, Serialize};

use crate::effect::{Effect, Effects, Push, Store};

pub const KEY_SERVERS: &str = "gliph.servers";

/// One signed-in server.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ServerEntry {
    /// Normalised origin, e.g. `https://acme.gliph.chat`. The identity of the
    /// entry: everything else is display.
    pub host: String,
    /// This session's token on that server.
    #[serde(default)]
    pub token: String,
    /// What the server calls itself (`/api/me`), e.g. `acme`. Falls back to the
    /// bare hostname until the first poll answers.
    #[serde(default)]
    pub name: String,
    /// Your nick THERE. Different servers, different rows, different nicks.
    #[serde(default)]
    pub nick: String,
    /// The server's own icon, as an absolute URL. Empty means draw the
    /// monogram instead.
    #[serde(default)]
    pub icon: String,
    /// Unread waiting on that server, from the last poll.
    ///
    /// Unread, not mentions. In a sealed channel the body is ciphertext to the
    /// server, so it cannot know whether your nick is in it — a count is the
    /// most any server can honestly report about a room it cannot read.
    #[serde(default)]
    pub unread: i64,
    /// When that count was last refreshed, host clock. 0 = never.
    #[serde(default)]
    pub checked_ms: i64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Servers {
    #[serde(default)]
    pub list: Vec<ServerEntry>,
    /// Host of the server on screen. Always names an entry in `list`, or is
    /// empty when there are none.
    #[serde(default)]
    pub active: String,
}

/// One address, one spelling. Without this, `https://acme.gliph.chat`,
/// `https://acme.gliph.chat/` and `HTTPS://Acme.Gliph.Chat` are three servers
/// in the switcher and three sockets on the wire.
pub fn normalise(host: &str) -> String {
    let h = host.trim().trim_end_matches('/');
    if h.is_empty() {
        return String::new();
    }
    // Only the scheme and authority are case-insensitive; a path never is. In
    // practice a server address has no path, but lowercasing one would be a
    // silent corruption, so split first.
    match h.split_once("://") {
        Some((scheme, rest)) => match rest.split_once('/') {
            Some((auth, path)) => format!("{}://{}/{}", scheme.to_lowercase(), auth.to_lowercase(), path),
            None => format!("{}://{}", scheme.to_lowercase(), rest.to_lowercase()),
        },
        // No scheme typed: assume the safe one rather than the working one.
        None => format!("https://{}", h.to_lowercase()),
    }
}

/// The display name to use before a server has told us its own: the bare
/// hostname, minus `www.` and the port.
fn fallback_name(host: &str) -> String {
    let h = host.split_once("://").map(|(_, rest)| rest).unwrap_or(host);
    let h = h.split('/').next().unwrap_or(h);
    let h = h.split(':').next().unwrap_or(h);
    h.strip_prefix("www.").unwrap_or(h).to_string()
}

impl Servers {
    pub fn restore(&mut self, json: &str) {
        if let Ok(s) = serde_json::from_str::<Servers>(json) {
            *self = s;
        }
        self.repair();
    }

    /// Invariants, enforced in one place because every mutator can break them:
    /// no blank hosts, no duplicates, and `active` names something real.
    fn repair(&mut self) {
        self.list.retain(|e| !e.host.is_empty());
        let mut seen: Vec<String> = Vec::new();
        self.list.retain(|e| {
            if seen.iter().any(|h| h == &e.host) {
                false
            } else {
                seen.push(e.host.clone());
                true
            }
        });
        if !self.list.iter().any(|e| e.host == self.active) {
            self.active = self.list.first().map(|e| e.host.clone()).unwrap_or_default();
        }
    }

    fn save(&self, fx: &mut Effects) {
        if let Ok(value) = serde_json::to_string(self) {
            fx.push(Effect::Persist { key: KEY_SERVERS.into(), value, store: Store::Plain });
        }
    }

    pub fn get(&self, host: &str) -> Option<&ServerEntry> {
        let h = normalise(host);
        self.list.iter().find(|e| e.host == h)
    }

    pub fn active_entry(&self) -> Option<&ServerEntry> {
        self.get(&self.active)
    }

    /// Sign-in on a server, or a fresh token for one already listed. Signing in
    /// makes it the one on screen — you did not type a password to stay where
    /// you were.
    pub fn add(&mut self, host: &str, token: &str, nick: &str) -> Effects {
        let mut fx = Effects::new();
        let h = normalise(host);
        if h.is_empty() {
            return fx;
        }
        match self.list.iter_mut().find(|e| e.host == h) {
            Some(e) => {
                e.token = token.to_string();
                if !nick.is_empty() {
                    e.nick = nick.to_string();
                }
            }
            None => self.list.push(ServerEntry {
                host: h.clone(),
                token: token.to_string(),
                name: fallback_name(&h),
                nick: nick.to_string(),
                ..Default::default()
            }),
        }
        self.active = h;
        self.repair();
        self.save(&mut fx);
        fx.changed();
        fx
    }

    /// Signed out of one server. The rest of the list is untouched — signing
    /// out of one team is not signing out of the product.
    pub fn forget(&mut self, host: &str) -> Effects {
        let mut fx = Effects::new();
        let h = normalise(host);
        self.list.retain(|e| e.host != h);
        self.repair();
        self.save(&mut fx);
        fx.changed();
        fx
    }

    /// Put a server on screen. Returns no effects when it is already the active
    /// one, so a host can call this on every click without churning storage.
    pub fn activate(&mut self, host: &str) -> Effects {
        let mut fx = Effects::new();
        let h = normalise(host);
        if h == self.active || !self.list.iter().any(|e| e.host == h) {
            return fx;
        }
        self.active = h;
        // Arriving somewhere is the moment its count is most obviously stale:
        // you are about to read it. Clearing here stops the bracket lingering
        // over the server you are looking at.
        if let Some(e) = self.list.iter_mut().find(|e| e.host == self.active) {
            e.unread = 0;
        }
        self.save(&mut fx);
        fx.changed();
        fx
    }

    /// What a poll of `/api/me` came back with. Only overwrites what the server
    /// actually said, so a sparse answer never blanks a name we already have.
    pub fn observed(
        &mut self,
        host: &str,
        name: &str,
        nick: &str,
        icon: &str,
        unread: i64,
        now_ms: i64,
    ) -> Effects {
        let mut fx = Effects::new();
        let h = normalise(host);
        let active = self.active.clone();
        let Some(e) = self.list.iter_mut().find(|e| e.host == h) else {
            return fx;
        };
        let before = e.clone();
        if !name.is_empty() {
            e.name = name.to_string();
        }
        if !nick.is_empty() {
            e.nick = nick.to_string();
        }
        if !icon.is_empty() {
            e.icon = icon.to_string();
        }
        // The server you are reading has no unread by definition; the socket,
        // not the poll, is the authority on what is left there.
        e.unread = if e.host == active { 0 } else { unread.max(0) };
        // checked_ms moves on every poll and nothing on screen shows it, so it
        // is deliberately not part of "did anything change" — persisting for it
        // would write storage once a minute forever.
        let changed = e.name != before.name
            || e.nick != before.nick
            || e.icon != before.icon
            || e.unread != before.unread;
        e.checked_ms = now_ms;
        if !changed {
            return fx;
        }
        self.save(&mut fx);
        fx.changed();
        fx
    }

    /// Total waiting everywhere else — what the status bar reports.
    pub fn elsewhere(&self) -> i64 {
        self.list.iter().filter(|e| e.host != self.active).map(|e| e.unread).sum()
    }
}
