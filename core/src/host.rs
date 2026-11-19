//! One host-facing object, two bindings.
//!
//! `GliphClient` is the whole API surface. The wasm and UniFFI wrappers below
//! are generated from the same methods, so the web client and the iOS app
//! cannot drift apart by construction — adding a capability to one adds it to
//! the other or fails to compile.
//!
//! Every method that changes anything returns the effect list as JSON. The host
//! loop is always the same three lines: call in, run the effects, re-render if
//! one of them was `Changed`.

use std::cell::RefCell;

use crate::api;
use crate::client::{cache_key, Client};
use crate::servers::Servers;

/// Interior mutability because both bindings hand out a shared handle: JS holds
/// one object for the page's lifetime, and Swift holds one `Arc` for the app's.
/// A `RefCell` rather than a `Mutex` is honest about the fact that neither host
/// calls this from two threads — wasm has one, and the iOS app drives it from
/// the main actor.
pub struct GliphClient {
    inner: RefCell<Client>,
    /// The servers this account is signed in to. Deliberately beside the
    /// `Client` rather than inside it: a `Client` is ONE server session and
    /// `logout` resets it wholesale, while the list has to outlive that.
    servers: RefCell<Servers>,
}

// The iOS binding hands this across an `Arc`, which requires the promise below.
// It is sound for the same reason the RefCell is: the Swift side is annotated
// `@MainActor`, so every call arrives on one thread.
unsafe impl Send for GliphClient {}
unsafe impl Sync for GliphClient {}

impl Default for GliphClient {
    fn default() -> Self {
        Self::new()
    }
}

impl GliphClient {
    pub fn new() -> Self {
        GliphClient { inner: RefCell::new(Client::new()), servers: RefCell::new(Servers::default()) }
    }

    /// Hand back what was persisted last run. Empty strings are a fresh install.
    pub fn restore(&self, session: &str, cache: &str, device: &str) {
        self.inner.borrow_mut().restore(session, cache, device);
    }

    // ---- session ----

    pub fn set_session(&self, server: &str, token: &str, user_json: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().set_session(server, token, user_json))
    }

    /// Signed out of the active server. The device key goes only if no other
    /// server in the list is still signed in (see `Client::logout`).
    pub fn logout(&self) -> String {
        let active = crate::servers::normalise(&self.inner.borrow().server);
        let last = self.servers.borrow().list.iter().all(|e| e.host == active);
        api::effects_json(&self.inner.borrow_mut().logout(last))
    }

    pub fn token(&self) -> String {
        self.inner.borrow().token.clone()
    }

    pub fn server(&self) -> String {
        self.inner.borrow().server.clone()
    }

    pub fn set_server(&self, server: &str) {
        self.inner.borrow_mut().server = server.trim_end_matches('/').to_string();
    }

    // ---- servers (the switcher) ----

    /// Hand back the server list persisted last run. Empty is a fresh install.
    pub fn restore_servers(&self, json: &str) {
        self.servers.borrow_mut().restore(json);
    }

    /// The whole registry, for the switcher to draw from.
    pub fn servers_json(&self) -> String {
        serde_json::to_string(&*self.servers.borrow()).unwrap_or_else(|_| "{}".into())
    }

    pub fn active_server(&self) -> String {
        self.servers.borrow().active.clone()
    }

    /// One address, one spelling. Exposed so a host never has to re-implement
    /// the rule to build a URL before the registry has seen the address.
    pub fn normalise_server(&self, host: &str) -> String {
        crate::servers::normalise(host)
    }

    /// Which storage key holds a given server's cache. The host needs this
    /// BEFORE it can switch, to read the right blob back in.
    pub fn cache_key_for(&self, server: &str) -> String {
        cache_key(&crate::servers::normalise(server))
    }

    /// Signed in somewhere — first time, or a fresh token for a server already
    /// listed. Signing in makes it the one on screen.
    pub fn add_server(&self, host: &str, token: &str, nick: &str) -> String {
        api::effects_json(&self.servers.borrow_mut().add(host, token, nick))
    }

    /// Signed out of one server. The others are untouched.
    pub fn forget_server(&self, host: &str) -> String {
        api::effects_json(&self.servers.borrow_mut().forget(host))
    }

    /// What a poll of that server's `/api/me` came back with.
    pub fn observe_server(
        &self,
        host: &str,
        name: &str,
        nick: &str,
        icon: &str,
        unread: i64,
        now_ms: i64,
    ) -> String {
        api::effects_json(&self.servers.borrow_mut().observed(host, name, nick, icon, unread, now_ms))
    }

    /// Put another server on screen. The host reads that server's cache with
    /// [`cache_key_for`](Self::cache_key_for) and passes it in; the token comes
    /// from the registry, so a caller can never point the session at a server
    /// it is not signed in to.
    pub fn switch_server(&self, host: &str, cache_json: &str, user_json: &str) -> String {
        let mut fx = self.servers.borrow_mut().activate(host);
        let entry = self.servers.borrow().get(host).cloned();
        if let Some(e) = entry {
            let mut moved = self.inner.borrow_mut().adopt_server(&e.host, &e.token, user_json, cache_json);
            fx.append(&mut moved);
        }
        api::effects_json(&fx)
    }

    /// Total unread on every server except the one on screen.
    pub fn unread_elsewhere(&self) -> i64 {
        self.servers.borrow().elsewhere()
    }

    // ---- socket lifecycle ----

    pub fn connected(&self, platform: &str, now_ms: i64) -> String {
        api::effects_json(&self.inner.borrow_mut().connected(platform, now_ms))
    }

    pub fn disconnected(&self, now_ms: i64) -> String {
        api::effects_json(&self.inner.borrow_mut().disconnected(now_ms))
    }

    /// Catch-up has landed; announcements from here on are news.
    pub fn go_live(&self) {
        self.inner.borrow_mut().go_live();
    }

    pub fn wake(&self) -> String {
        api::effects_json(&self.inner.borrow_mut().wake())
    }

    pub fn tick(&self, now_ms: i64) -> String {
        api::effects_json(&self.inner.borrow_mut().tick(now_ms))
    }

    pub fn handle_frame(&self, json: &str, now_ms: i64) -> String {
        api::effects_json(&self.inner.borrow_mut().handle_frame(json, now_ms))
    }

    // ---- user actions ----

    pub fn submit(&self, text: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().submit(text))
    }

    pub fn send_message(&self, body: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().send_message(body))
    }

    /// A line said inside the 3D room, marked with the sub-room it came from.
    pub fn send_room_message(&self, body: &str, room: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().send_room_message(body, room))
    }

    /// Aim the next message at an event; `""` cancels. Everything else about a
    /// reply — the quote, when it clears — is the core's business.
    pub fn set_reply(&self, event_id: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().set_reply(event_id))
    }

    pub fn set_active(&self, cid: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().set_active(cid))
    }

    pub fn mark_active_read(&self) -> String {
        api::effects_json(&self.inner.borrow_mut().mark_active_read())
    }

    /// The reader closed a system message (the MOTD, or a release note).
    pub fn dismiss_system(&self, id: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().dismiss_system(id))
    }

    pub fn notice(&self, text: &str, level: &str) -> String {
        let mut c = self.inner.borrow_mut();
        c.notice(text, level);
        api::effects_json(&vec![crate::Effect::Changed])
    }

    // ---- nearby mesh ----

    pub fn start_solo(&self, nick: &str, chan: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().start_solo(nick, chan))
    }

    pub fn mesh_peers(&self, json: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().mesh_peers(json))
    }

    pub fn mesh_msg(&self, json: &str, now_ms: i64) -> String {
        api::effects_json(&self.inner.borrow_mut().mesh_msg(json, now_ms))
    }

    pub fn mesh_frame(&self, json: &str, now_ms: i64) -> String {
        api::effects_json(&self.inner.borrow_mut().mesh_frame(json, now_ms))
    }

    // ---- transfers ----

    pub fn offer_file(&self, name: &str, mime: &str, bytes: Vec<u8>) -> String {
        api::effects_json(&self.inner.borrow_mut().offer_file(name, mime, bytes))
    }

    /// A transfer the host drove itself (a DM over WebRTC) finished: leave the
    /// line in the channel at this end (web and iOS). The mesh path posts its
    /// own line inside the core.
    pub fn p2p_file_line(&self, cid: &str, id: &str, name: &str, size: f64, sent: bool, peer: &str) -> String {
        self.inner.borrow_mut().p2p_file_line(cid, id, name, size, sent, peer);
        api::effects_json(&vec![crate::Effect::Changed])
    }

    pub fn accept_transfer(&self, id: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().accept_transfer(id))
    }

    pub fn decline_transfer(&self, id: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().decline_transfer(id))
    }

    // ---- blob surface (GUM) ----

    pub fn open_blob(&self, blob: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().open_blob(blob))
    }
    pub fn blob_switch(&self, n: u32) -> String {
        api::effects_json(&self.inner.borrow_mut().blob_switch(n))
    }
    pub fn gum_move(&self, delta: i32) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_move(delta))
    }
    pub fn gum_jump(&self, n: u32) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_jump(n))
    }
    pub fn gum_set_draft(&self, block: &str, text: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_set_draft(block, text))
    }
    pub fn gum_activate(&self) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_activate())
    }
    pub fn gum_tap(&self, index: u32) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_tap(index))
    }
    pub fn gum_tap_menu(&self, action_id: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_tap_menu(action_id))
    }
    pub fn gum_escape(&self) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_escape())
    }
    pub fn gum_answer(&self, ok: bool, value: &str) -> String {
        api::effects_json(&self.inner.borrow_mut().gum_answer(ok, value))
    }
    /// The block id of the input the selection sits on, or "" — a frontend
    /// needs it to know which field a keystroke belongs to.
    pub fn gum_focused_input(&self) -> String {
        let c = self.inner.borrow();
        c.gum.focused_input(&c.active).unwrap_or_default()
    }

    // ---- read models ----

    /// Everything the chrome renders from.
    pub fn snapshot(&self) -> String {
        api::snapshot_json(&self.inner.borrow())
    }

    /// The stream for one channel, decryption already resolved.
    pub fn events(&self, cid: &str) -> String {
        api::events_json(&self.inner.borrow(), cid)
    }

    /// The tail of a channel for the room's own log, quiet lines included.
    pub fn room_log(&self, cid: &str, limit: u32) -> String {
        api::room_log_json(&self.inner.borrow(), cid, limit)
    }

    /// The command palette for what is typed so far. Built entirely from the
    /// server's manifest — no client has a list of its own.
    pub fn palette(&self, text: &str) -> String {
        api::palette_json(&self.inner.borrow(), text)
    }

    /// The blob surface, ready to draw.
    pub fn surface(&self) -> String {
        api::surface_json(&self.inner.borrow())
    }
}

// ---- wasm binding (the web client) ----

#[cfg(feature = "wasm")]
mod wasm {
    use super::GliphClient;
    use wasm_bindgen::prelude::*;

    #[wasm_bindgen(js_name = GliphCore)]
    pub struct WasmClient(GliphClient);

    #[wasm_bindgen(js_class = GliphCore)]
    impl WasmClient {
        #[wasm_bindgen(constructor)]
        pub fn new() -> WasmClient {
            WasmClient(GliphClient::new())
        }

        pub fn restore(&self, session: &str, cache: &str, device: &str) {
            self.0.restore(session, cache, device)
        }

        #[wasm_bindgen(js_name = restoreServers)]
        pub fn restore_servers(&self, json: &str) {
            self.0.restore_servers(json)
        }
        #[wasm_bindgen(js_name = serversJson)]
        pub fn servers_json(&self) -> String {
            self.0.servers_json()
        }
        #[wasm_bindgen(js_name = activeServer)]
        pub fn active_server(&self) -> String {
            self.0.active_server()
        }
        #[wasm_bindgen(js_name = normaliseServer)]
        pub fn normalise_server(&self, host: &str) -> String {
            self.0.normalise_server(host)
        }
        #[wasm_bindgen(js_name = cacheKeyFor)]
        pub fn cache_key_for(&self, server: &str) -> String {
            self.0.cache_key_for(server)
        }
        #[wasm_bindgen(js_name = addServer)]
        pub fn add_server(&self, host: &str, token: &str, nick: &str) -> String {
            self.0.add_server(host, token, nick)
        }
        #[wasm_bindgen(js_name = forgetServer)]
        pub fn forget_server(&self, host: &str) -> String {
            self.0.forget_server(host)
        }
        #[wasm_bindgen(js_name = observeServer)]
        pub fn observe_server(&self, host: &str, name: &str, nick: &str, icon: &str, unread: f64, now_ms: f64) -> String {
            self.0.observe_server(host, name, nick, icon, unread as i64, now_ms as i64)
        }
        #[wasm_bindgen(js_name = switchServer)]
        pub fn switch_server(&self, host: &str, cache_json: &str, user_json: &str) -> String {
            self.0.switch_server(host, cache_json, user_json)
        }
        #[wasm_bindgen(js_name = unreadElsewhere)]
        pub fn unread_elsewhere(&self) -> f64 {
            self.0.unread_elsewhere() as f64
        }

        #[wasm_bindgen(js_name = setSession)]
        pub fn set_session(&self, server: &str, token: &str, user_json: &str) -> String {
            self.0.set_session(server, token, user_json)
        }
        pub fn logout(&self) -> String {
            self.0.logout()
        }
        pub fn token(&self) -> String {
            self.0.token()
        }
        pub fn server(&self) -> String {
            self.0.server()
        }
        #[wasm_bindgen(js_name = setServer)]
        pub fn set_server(&self, server: &str) {
            self.0.set_server(server)
        }

        pub fn connected(&self, platform: &str, now_ms: f64) -> String {
            self.0.connected(platform, now_ms as i64)
        }
        pub fn disconnected(&self, now_ms: f64) -> String {
            self.0.disconnected(now_ms as i64)
        }
        #[wasm_bindgen(js_name = goLive)]
        pub fn go_live(&self) {
            self.0.go_live()
        }
        pub fn wake(&self) -> String {
            self.0.wake()
        }
        pub fn tick(&self, now_ms: f64) -> String {
            self.0.tick(now_ms as i64)
        }
        #[wasm_bindgen(js_name = handleFrame)]
        pub fn handle_frame(&self, json: &str, now_ms: f64) -> String {
            self.0.handle_frame(json, now_ms as i64)
        }

        pub fn submit(&self, text: &str) -> String {
            self.0.submit(text)
        }
        #[wasm_bindgen(js_name = sendMessage)]
        pub fn send_message(&self, body: &str) -> String {
            self.0.send_message(body)
        }
        #[wasm_bindgen(js_name = sendRoomMessage)]
        pub fn send_room_message(&self, body: &str, room: &str) -> String {
            self.0.send_room_message(body, room)
        }
        #[wasm_bindgen(js_name = roomLog)]
        pub fn room_log(&self, cid: &str, limit: u32) -> String {
            self.0.room_log(cid, limit)
        }
        #[wasm_bindgen(js_name = setReply)]
        pub fn set_reply(&self, event_id: &str) -> String {
            self.0.set_reply(event_id)
        }
        #[wasm_bindgen(js_name = setActive)]
        pub fn set_active(&self, cid: &str) -> String {
            self.0.set_active(cid)
        }
        #[wasm_bindgen(js_name = markActiveRead)]
        pub fn mark_active_read(&self) -> String {
            self.0.mark_active_read()
        }
        #[wasm_bindgen(js_name = dismissSystem)]
        pub fn dismiss_system(&self, id: &str) -> String {
            self.0.dismiss_system(id)
        }
        pub fn notice(&self, text: &str, level: &str) -> String {
            self.0.notice(text, level)
        }

        #[wasm_bindgen(js_name = startSolo)]
        pub fn start_solo(&self, nick: &str, chan: &str) -> String {
            self.0.start_solo(nick, chan)
        }
        #[wasm_bindgen(js_name = meshPeers)]
        pub fn mesh_peers(&self, json: &str) -> String {
            self.0.mesh_peers(json)
        }
        #[wasm_bindgen(js_name = meshMsg)]
        pub fn mesh_msg(&self, json: &str, now_ms: f64) -> String {
            self.0.mesh_msg(json, now_ms as i64)
        }
        #[wasm_bindgen(js_name = meshFrame)]
        pub fn mesh_frame(&self, json: &str, now_ms: f64) -> String {
            self.0.mesh_frame(json, now_ms as i64)
        }

        #[wasm_bindgen(js_name = offerFile)]
        pub fn offer_file(&self, name: &str, mime: &str, bytes: Vec<u8>) -> String {
            self.0.offer_file(name, mime, bytes)
        }
        #[wasm_bindgen(js_name = p2pFileLine)]
        pub fn p2p_file_line(&self, cid: &str, id: &str, name: &str, size: f64, sent: bool, peer: &str) -> String {
            self.0.p2p_file_line(cid, id, name, size, sent, peer)
        }
        #[wasm_bindgen(js_name = acceptTransfer)]
        pub fn accept_transfer(&self, id: &str) -> String {
            self.0.accept_transfer(id)
        }
        #[wasm_bindgen(js_name = declineTransfer)]
        pub fn decline_transfer(&self, id: &str) -> String {
            self.0.decline_transfer(id)
        }

        pub fn snapshot(&self) -> String {
            self.0.snapshot()
        }
        pub fn events(&self, cid: &str) -> String {
            self.0.events(cid)
        }
        pub fn palette(&self, text: &str) -> String {
            self.0.palette(text)
        }
        pub fn surface(&self) -> String {
            self.0.surface()
        }

        #[wasm_bindgen(js_name = openBlob)]
        pub fn open_blob(&self, blob: &str) -> String {
            self.0.open_blob(blob)
        }
        #[wasm_bindgen(js_name = blobSwitch)]
        pub fn blob_switch(&self, n: u32) -> String {
            self.0.blob_switch(n)
        }
        #[wasm_bindgen(js_name = gumMove)]
        pub fn gum_move(&self, delta: i32) -> String {
            self.0.gum_move(delta)
        }
        #[wasm_bindgen(js_name = gumJump)]
        pub fn gum_jump(&self, n: u32) -> String {
            self.0.gum_jump(n)
        }
        #[wasm_bindgen(js_name = gumSetDraft)]
        pub fn gum_set_draft(&self, block: &str, text: &str) -> String {
            self.0.gum_set_draft(block, text)
        }
        #[wasm_bindgen(js_name = gumActivate)]
        pub fn gum_activate(&self) -> String {
            self.0.gum_activate()
        }
        #[wasm_bindgen(js_name = gumTap)]
        pub fn gum_tap(&self, index: u32) -> String {
            self.0.gum_tap(index)
        }
        #[wasm_bindgen(js_name = gumTapMenu)]
        pub fn gum_tap_menu(&self, action_id: &str) -> String {
            self.0.gum_tap_menu(action_id)
        }
        #[wasm_bindgen(js_name = gumEscape)]
        pub fn gum_escape(&self) -> String {
            self.0.gum_escape()
        }
        #[wasm_bindgen(js_name = gumAnswer)]
        pub fn gum_answer(&self, ok: bool, value: &str) -> String {
            self.0.gum_answer(ok, value)
        }
        #[wasm_bindgen(js_name = gumFocusedInput)]
        pub fn gum_focused_input(&self) -> String {
            self.0.gum_focused_input()
        }
    }

    impl Default for WasmClient {
        fn default() -> Self {
            Self::new()
        }
    }
}

// ---- UniFFI binding (the iOS app) ----

#[cfg(feature = "ffi")]
mod ffi {
    use super::GliphClient;
    use std::sync::Arc;

    #[derive(uniffi::Object)]
    pub struct Core {
        inner: GliphClient,
    }

    #[uniffi::export]
    impl Core {
        #[uniffi::constructor]
        pub fn new() -> Arc<Core> {
            Arc::new(Core { inner: GliphClient::new() })
        }

        pub fn restore(&self, session: String, cache: String, device: String) {
            self.inner.restore(&session, &cache, &device)
        }

        pub fn set_session(&self, server: String, token: String, user_json: String) -> String {
            self.inner.set_session(&server, &token, &user_json)
        }
        pub fn logout(&self) -> String {
            self.inner.logout()
        }

        // ---- servers (the switcher) ----

        pub fn restore_servers(&self, json: String) {
            self.inner.restore_servers(&json)
        }
        pub fn servers_json(&self) -> String {
            self.inner.servers_json()
        }
        pub fn active_server(&self) -> String {
            self.inner.active_server()
        }
        pub fn normalise_server(&self, host: String) -> String {
            self.inner.normalise_server(&host)
        }
        pub fn cache_key_for(&self, server: String) -> String {
            self.inner.cache_key_for(&server)
        }
        pub fn add_server(&self, host: String, token: String, nick: String) -> String {
            self.inner.add_server(&host, &token, &nick)
        }
        pub fn forget_server(&self, host: String) -> String {
            self.inner.forget_server(&host)
        }
        pub fn observe_server(&self, host: String, name: String, nick: String, icon: String, unread: i64, now_ms: i64) -> String {
            self.inner.observe_server(&host, &name, &nick, &icon, unread, now_ms)
        }
        pub fn switch_server(&self, host: String, cache_json: String, user_json: String) -> String {
            self.inner.switch_server(&host, &cache_json, &user_json)
        }
        pub fn unread_elsewhere(&self) -> i64 {
            self.inner.unread_elsewhere()
        }
        pub fn dismiss_system(&self, id: String) -> String {
            self.inner.dismiss_system(&id)
        }
        pub fn token(&self) -> String {
            self.inner.token()
        }
        pub fn server(&self) -> String {
            self.inner.server()
        }
        pub fn set_server(&self, server: String) {
            self.inner.set_server(&server)
        }

        pub fn connected(&self, platform: String, now_ms: i64) -> String {
            self.inner.connected(&platform, now_ms)
        }
        pub fn disconnected(&self, now_ms: i64) -> String {
            self.inner.disconnected(now_ms)
        }
        pub fn go_live(&self) {
            self.inner.go_live()
        }
        pub fn wake(&self) -> String {
            self.inner.wake()
        }
        pub fn tick(&self, now_ms: i64) -> String {
            self.inner.tick(now_ms)
        }
        pub fn handle_frame(&self, json: String, now_ms: i64) -> String {
            self.inner.handle_frame(&json, now_ms)
        }

        pub fn submit(&self, text: String) -> String {
            self.inner.submit(&text)
        }
        pub fn send_message(&self, body: String) -> String {
            self.inner.send_message(&body)
        }
        pub fn send_room_message(&self, body: String, room: String) -> String {
            self.inner.send_room_message(&body, &room)
        }
        pub fn room_log(&self, cid: String, limit: u32) -> String {
            self.inner.room_log(&cid, limit)
        }
        pub fn set_reply(&self, event_id: String) -> String {
            self.inner.set_reply(&event_id)
        }
        pub fn set_active(&self, cid: String) -> String {
            self.inner.set_active(&cid)
        }
        pub fn mark_active_read(&self) -> String {
            self.inner.mark_active_read()
        }
        pub fn notice(&self, text: String, level: String) -> String {
            self.inner.notice(&text, &level)
        }

        pub fn start_solo(&self, nick: String, chan: String) -> String {
            self.inner.start_solo(&nick, &chan)
        }
        pub fn mesh_peers(&self, json: String) -> String {
            self.inner.mesh_peers(&json)
        }
        pub fn mesh_msg(&self, json: String, now_ms: i64) -> String {
            self.inner.mesh_msg(&json, now_ms)
        }
        pub fn mesh_frame(&self, json: String, now_ms: i64) -> String {
            self.inner.mesh_frame(&json, now_ms)
        }

        pub fn offer_file(&self, name: String, mime: String, bytes: Vec<u8>) -> String {
            self.inner.offer_file(&name, &mime, bytes)
        }
        pub fn p2p_file_line(&self, cid: String, id: String, name: String, size: f64, sent: bool, peer: String) -> String {
            self.inner.p2p_file_line(&cid, &id, &name, size, sent, &peer)
        }
        pub fn accept_transfer(&self, id: String) -> String {
            self.inner.accept_transfer(&id)
        }
        pub fn decline_transfer(&self, id: String) -> String {
            self.inner.decline_transfer(&id)
        }

        pub fn snapshot(&self) -> String {
            self.inner.snapshot()
        }
        pub fn events(&self, cid: String) -> String {
            self.inner.events(&cid)
        }
        pub fn palette(&self, text: String) -> String {
            self.inner.palette(&text)
        }
        pub fn surface(&self) -> String {
            self.inner.surface()
        }

        pub fn open_blob(&self, blob: String) -> String {
            self.inner.open_blob(&blob)
        }
        pub fn blob_switch(&self, n: u32) -> String {
            self.inner.blob_switch(n)
        }
        pub fn gum_move(&self, delta: i32) -> String {
            self.inner.gum_move(delta)
        }
        pub fn gum_jump(&self, n: u32) -> String {
            self.inner.gum_jump(n)
        }
        pub fn gum_set_draft(&self, block: String, text: String) -> String {
            self.inner.gum_set_draft(&block, &text)
        }
        pub fn gum_activate(&self) -> String {
            self.inner.gum_activate()
        }
        pub fn gum_tap(&self, index: u32) -> String {
            self.inner.gum_tap(index)
        }
        pub fn gum_tap_menu(&self, action_id: String) -> String {
            self.inner.gum_tap_menu(&action_id)
        }
        pub fn gum_escape(&self) -> String {
            self.inner.gum_escape()
        }
        pub fn gum_answer(&self, ok: bool, value: String) -> String {
            self.inner.gum_answer(ok, &value)
        }
        pub fn gum_focused_input(&self) -> String {
            self.inner.gum_focused_input()
        }
    }
}
