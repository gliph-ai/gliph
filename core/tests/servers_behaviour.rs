//! The rules the server switcher runs on, asserted without a network.
//!
//! Every one of these is a way the switcher could quietly go wrong on a real
//! account: the same server listed twice under two spellings, a bracket that
//! keeps counting the room you are reading, a sign-out that takes the other
//! teams with it.

use gliph_core::client::cache_key;
use gliph_core::effect::{Effect, Store};

use gliph_core::{Client, Effects, Servers};

const NOW: i64 = 1_800_000_000_000;

fn persisted(fx: &Effects) -> Vec<(String, String)> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::Persist { key, value, .. } => Some((key.clone(), value.clone())),
            _ => None,
        })
        .collect()
}

fn hosts(s: &Servers) -> Vec<&str> {
    s.list.iter().map(|e| e.host.as_str()).collect()
}

#[test]
fn one_address_has_one_spelling() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t1", "waseem");
    s.add("https://acme.gliph.chat/", "t2", "waseem");
    s.add("HTTPS://Acme.Gliph.Chat", "t3", "waseem");
    // A trailing slash and a capital letter are the same server, not three
    // entries and three sockets.
    assert_eq!(hosts(&s), vec!["https://acme.gliph.chat"]);
    assert_eq!(s.get("https://acme.gliph.chat").unwrap().token, "t3");
}

#[test]
fn a_typed_address_gets_the_safe_scheme() {
    let mut s = Servers::default();
    s.add("chat.vandelay.industries", "t", "waseem");
    assert_eq!(hosts(&s), vec!["https://chat.vandelay.industries"]);
}

#[test]
fn signing_in_puts_that_server_on_screen() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.add("https://hooli.gliph.chat", "t", "wsm");
    assert_eq!(s.active, "https://hooli.gliph.chat");
    // You did not type a password to stay where you were.
    assert_eq!(s.active_entry().unwrap().nick, "wsm");
}

#[test]
fn the_name_falls_back_to_the_hostname_until_the_server_says_otherwise() {
    let mut s = Servers::default();
    s.add("https://www.acme.gliph.chat:8443", "t", "waseem");
    assert_eq!(s.get("https://www.acme.gliph.chat:8443").unwrap().name, "acme.gliph.chat");
}

#[test]
fn signing_out_of_one_server_keeps_the_others() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.add("https://hooli.gliph.chat", "t", "wsm");
    let fx = s.forget("https://hooli.gliph.chat");

    assert_eq!(hosts(&s), vec!["https://acme.gliph.chat"]);
    // and the screen lands on something that still exists
    assert_eq!(s.active, "https://acme.gliph.chat");

    let writes = persisted(&fx);
    assert_eq!(writes.len(), 1);
    assert_eq!(writes[0].0, "gliph.servers");
    assert!(
        fx.iter().any(|e| matches!(e, Effect::Persist { store: Store::Plain, .. })),
        "the server list is not a secret store"
    );
}

#[test]
fn forgetting_the_last_server_leaves_nothing_active() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.forget("https://acme.gliph.chat");
    assert!(s.active.is_empty());
    assert!(s.active_entry().is_none());
}

#[test]
fn arriving_somewhere_clears_its_bracket() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.add("https://hooli.gliph.chat", "t", "wsm");
    s.activate("https://acme.gliph.chat");
    s.observed("https://hooli.gliph.chat", "hooli", "wsm", "", 7, NOW);
    assert_eq!(s.get("https://hooli.gliph.chat").unwrap().unread, 7);

    s.activate("https://hooli.gliph.chat");
    // You are about to read it; the bracket must not linger over the room on
    // screen.
    assert_eq!(s.get("https://hooli.gliph.chat").unwrap().unread, 0);
}

#[test]
fn activating_the_server_already_on_screen_changes_nothing() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    let fx = s.activate("https://acme.gliph.chat");
    // A host may call this on every click; it must not churn storage.
    assert!(fx.is_empty());
}

#[test]
fn activating_a_server_we_are_not_signed_in_to_does_nothing() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    let fx = s.activate("https://evil.example");
    assert!(fx.is_empty());
    assert_eq!(s.active, "https://acme.gliph.chat");
}

#[test]
fn a_poll_never_blanks_what_it_did_not_answer() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.add("https://hooli.gliph.chat", "t", "wsm");
    s.observed("https://acme.gliph.chat", "acme", "waseem", "https://acme.gliph.chat/icon.png", 0, NOW);
    // A sparse answer — an older server with no icon field — must leave the
    // name and icon we already had alone.
    s.observed("https://acme.gliph.chat", "", "", "", 0, NOW + 1000);

    let e = s.get("https://acme.gliph.chat").unwrap();
    assert_eq!(e.name, "acme");
    assert_eq!(e.nick, "waseem");
    assert_eq!(e.icon, "https://acme.gliph.chat/icon.png");
}

#[test]
fn the_server_on_screen_never_shows_a_count() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    // Even if a poll in flight comes back saying otherwise: the socket, not the
    // poll, is the authority on the room you are looking at.
    s.observed("https://acme.gliph.chat", "acme", "waseem", "", 12, NOW);
    assert_eq!(s.get("https://acme.gliph.chat").unwrap().unread, 0);
}

#[test]
fn a_poll_that_learned_nothing_writes_nothing() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.add("https://hooli.gliph.chat", "t", "wsm");
    s.activate("https://acme.gliph.chat");
    s.observed("https://hooli.gliph.chat", "hooli", "wsm", "", 7, NOW);

    let again = s.observed("https://hooli.gliph.chat", "hooli", "wsm", "", 7, NOW + 60_000);
    // Only checked_ms moved, and nothing on screen shows it. Persisting for
    // that would write storage once a minute forever.
    assert!(again.is_empty());
}

#[test]
fn elsewhere_counts_everywhere_but_here() {
    let mut s = Servers::default();
    s.add("https://acme.gliph.chat", "t", "waseem");
    s.add("https://hooli.gliph.chat", "t", "wsm");
    s.add("https://initech.gliph.chat", "t", "w");
    s.activate("https://acme.gliph.chat");
    s.observed("https://hooli.gliph.chat", "", "", "", 7, NOW);
    s.observed("https://initech.gliph.chat", "", "", "", 2, NOW);
    assert_eq!(s.elsewhere(), 9);
}

#[test]
fn a_restored_list_is_repaired_not_trusted() {
    let mut s = Servers::default();
    // Storage a previous version — or a hand edit — could plausibly leave:
    // a duplicate, a blank host, and an active naming none of them.
    s.restore(
        r#"{"list":[
             {"host":"https://acme.gliph.chat","token":"a"},
             {"host":"https://acme.gliph.chat","token":"b"},
             {"host":"","token":"c"}
           ],"active":"https://gone.example"}"#,
    );
    assert_eq!(hosts(&s), vec!["https://acme.gliph.chat"]);
    assert_eq!(s.get("https://acme.gliph.chat").unwrap().token, "a");
    assert_eq!(s.active, "https://acme.gliph.chat");
}

#[test]
fn unreadable_storage_is_an_empty_list_not_a_panic() {
    let mut s = Servers::default();
    s.restore("not json at all");
    assert!(s.list.is_empty());
    assert!(s.active.is_empty());
}

// ---- the session that follows the switch ----

#[test]
fn every_server_caches_under_its_own_key() {
    // The whole reason switching does not cost you the other server's
    // scrollback.
    assert_ne!(cache_key("https://acme.gliph.chat"), cache_key("https://hooli.gliph.chat"));
    assert_eq!(cache_key("https://acme.gliph.chat"), cache_key("https://acme.gliph.chat"));
    // and the key survives a round trip through storage naming rules
    assert!(cache_key("https://acme.gliph.chat").starts_with("gliph.cache."));
    assert!(!cache_key("https://acme.gliph.chat").contains('/'));
    // Punctuation and scheme are part of the identity: these are four servers.
    let keys = ["https://a-b.example", "https://a.b.example", "http://a.b.example", "https://a.b.example:8443"]
        .map(cache_key);
    for (i, a) in keys.iter().enumerate() {
        for b in &keys[i + 1..] {
            assert_ne!(a, b);
        }
    }
}

#[test]
fn switching_servers_leaves_nothing_of_the_last_one_behind() {
    let mut c = Client::new();
    c.set_session("https://acme.gliph.chat", "tok-acme", r#"{"id":"u1","nick":"waseem"}"#);
    c.handle_frame(
        &serde_json::json!({
            "t": "state",
            "d": {"channels": [{"id": "c1", "name": "dev", "topic": "", "modes": "nt",
                                "role": "member", "seq": 4, "members": []}]}
        })
        .to_string(),
        NOW,
    );
    assert!(!c.channels().is_empty(), "acme's channels are loaded");

    c.adopt_server("https://hooli.gliph.chat", "tok-hooli", r#"{"id":"u2","nick":"wsm"}"#, "");

    assert_eq!(c.server, "https://hooli.gliph.chat");
    assert_eq!(c.token, "tok-hooli");
    assert_eq!(c.user.as_ref().unwrap().nick, "wsm");
    // acme's rooms must not be on screen under hooli's name
    assert!(c.channels().is_empty(), "the last server's channels went with it");
}

#[test]
fn switching_servers_keeps_the_device_key() {
    let mut c = Client::new();
    c.set_session("https://acme.gliph.chat", "t", "{}");
    // Minting the keypair is the first thing `connected` does, and the secret
    // comes back as a Persist for the host to put in the Keychain.
    let first = c.connected("web", NOW);
    assert!(
        persisted(&first).iter().any(|(k, _)| k == "gliph.device"),
        "the first connection mints a device key"
    );

    c.adopt_server("https://hooli.gliph.chat", "t2", "{}", "");
    let after = c.connected("web", NOW + 1000);

    // No second Persist means no second keypair. A fresh one per server would
    // strand every sealed channel key already wrapped to the old one.
    assert!(
        !persisted(&after).iter().any(|(k, _)| k == "gliph.device"),
        "the switch reused this device's existing key"
    );
}
