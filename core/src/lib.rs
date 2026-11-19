//! Gliph client core.
//!
//! One implementation of the protocol, the crypto, the sync and the offline
//! state, shared by every client: the web client (wasm), the desktop app
//! (through the web bundle it embeds), and the iOS app (uniffi).
//!
//! The core is **sans-io**. It performs no network, storage or clock access —
//! it cannot, because it has to run inside a browser's wasm sandbox and inside
//! an iOS process, and those agree on almost nothing. Instead every entry point
//! returns a list of [`effect::Effect`]s and the host carries them out.
//!
//! That constraint bought three things:
//!
//!  - the E2EE exists once instead of once per language, which is the whole
//!    reason this crate exists — see `crypto.rs`;
//!  - every rule about what the user sees is testable with no network, no
//!    filesystem and no simulator;
//!  - a new platform is a host, not a rewrite.
//!
//! `docs/protocol.md` is the wire contract. Change it and this changes with it.

pub mod api;
pub mod client;
pub mod crypto;
pub mod effect;
pub mod gum;
pub mod host;
pub mod mesh;
pub mod servers;
pub mod wire;
pub mod xfer;

pub use client::Client;
pub use servers::{ServerEntry, Servers};
pub use host::GliphClient;
pub use effect::{Effect, Effects, Store};

#[cfg(feature = "ffi")]
uniffi::setup_scaffolding!();
