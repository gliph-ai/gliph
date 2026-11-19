//! What the core asks its host to do.
//!
//! The core is sans-io: it owns every piece of state and every decision, and
//! performs no IO at all. It cannot — it has to run in a browser's wasm sandbox
//! and in an iOS process, and those agree on almost nothing. So instead of
//! calling a socket, it returns a list of [`Effect`]s and the host carries them
//! out.
//!
//! That is what makes the whole thing testable without a network, a filesystem
//! or a simulator: a test feeds frames in and asserts on the effects that come
//! back. It is also what keeps the hosts thin, which was the point.

use serde::{Deserialize, Deserializer, Serialize, Serializer};

/// File bytes cross the host boundary as base64 rather than as a JSON array of
/// numbers. A 10 MB transfer would otherwise serialise to about 40 MB of ASCII
/// digits, which is the kind of detail that turns a working feature into a
/// mysterious hang on a phone.
mod b64bytes {
    use super::*;
    use base64::engine::general_purpose::STANDARD as B64;
    use base64::Engine;

    pub fn serialize<S: Serializer>(v: &[u8], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&B64.encode(v))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        let s = String::deserialize(d)?;
        B64.decode(s).map_err(serde::de::Error::custom)
    }
}

/// Where a persisted value belongs. The device secret is the one thing in the
/// product that must never leave the device, so it is flagged rather than left
/// to each host to remember.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Store {
    /// Ordinary storage: localStorage on the web, a plist on iOS.
    Plain,
    /// The OS secret store: IndexedDB behind the origin on the web, the
    /// Keychain on iOS.
    Secret,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind")]
pub enum Effect {
    /// Put this JSON frame on the server socket. Dropped silently by the host
    /// when there is no socket — the core has already queued anything that
    /// needed queueing.
    Send { json: String },

    /// Write this value under this key. Same key overwrites.
    Persist { key: String, value: String, store: Store },

    /// Forget everything under this key (sign-out).
    Forget { key: String, store: Store },

    /// Something arrived that is worth telling the user about. The core decided
    /// WHETHER — a DM, or a highlight word, and readable; the host decides
    /// WHERE it lands (a banner, an OS notification, nothing if it is already
    /// on screen).
    ///
    /// `event` is WHY it fired — `dm`, `mention`, `highlight`, `nearby` or `join`
    /// (a new account's first arrival, never a /join). The reason is
    /// known here, at the point of the decision, and nowhere else: a host that
    /// re-derived it from `text` would be guessing at the highlight rules and
    /// would guess differently on each platform.
    Alert {
        id: String,
        channel: String,
        nick: String,
        text: String,
        is_dm: bool,
        event: String,
    },

    /// State changed; re-render.
    Changed,

    // ---- nearby mesh (F9). The transport is native on every platform. ----
    MeshStart { nick: String },
    MeshStop,
    MeshJoin { chans: Vec<String> },
    /// Say something in a nearby channel. `msg_kind` is `message` or `action`
    /// and is spelled `kind` on the mesh wire — it cannot be called that here,
    /// because this enum is itself tagged `kind`.
    MeshSay { chan: String, body: String, msg_kind: String, reply_to: String },
    /// An uninterpreted frame for one peer — file transfer rides these.
    MeshSendTo { peer: String, t: String, json: String },

    /// A nearby transfer completed. The bytes are in memory here and nowhere
    /// else, so the host writes them somewhere the user can reach.
    SaveFile {
        name: String,
        mime: String,
        #[serde(with = "b64bytes")]
        bytes: Vec<u8>,
    },

    /// Open the file picker — `/send <nick>` decided who, not what.
    PickFile,

    /// A blob action wants a yes/no before it fires. The core cannot draw a
    /// dialog, so it asks; the host answers with `gum_answer`. Only one can be
    /// outstanding, because a modal is a modal.
    AskConfirm { message: String },
    /// A blob action wants one line of text before it fires.
    AskPrompt { message: String },

    /// `/update`: check for a newer build, install it, restart. Only the
    /// desktop host has an updater; the others tell the user so.
    Update,

    /// Open a multi-line editor holding `text` (a theme, a script). On save
    /// the host calls `submit(submit + "\n" + edited)`; on cancel, nothing.
    /// The composer is one line on the web, and a pasted file is not.
    Compose { title: String, lang: String, text: String, submit: String },

    /// `/theme`: the installed themes live with the host's theme preference,
    /// so the core checks the file and the host keeps and applies it. `op` is
    /// `add` (`theme` is the checked JSON: name, base, tokens), `use`,
    /// `remove` or `list`.
    Theme { op: String, name: String, theme: String },
}

/// The effect list a call returns. A plain Vec; the ordering is meaningful
/// (a Persist before the Changed that renders from it).
pub type Effects = Vec<Effect>;

pub(crate) trait Push {
    fn changed(&mut self);
    fn send(&mut self, frame: crate::wire::Frame);
}

impl Push for Effects {
    fn changed(&mut self) {
        // One Changed per call is enough: the host re-renders once at the end.
        if !self.iter().any(|e| matches!(e, Effect::Changed)) {
            self.push(Effect::Changed);
        }
    }
    fn send(&mut self, frame: crate::wire::Frame) {
        if let Ok(json) = serde_json::to_string(&frame) {
            self.push(Effect::Send { json });
        }
    }
}
