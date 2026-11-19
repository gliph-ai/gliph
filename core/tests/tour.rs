//! The first-run tour: a new account's first hello puts a MOTD-styled card in
//! #general that teaches `/`. Dismissed, it never comes back.

use gliph_core::effect::Effect;
use gliph_core::{Client, Effects};
use serde_json::json;

const NOW: i64 = 1_800_000_000_000;

fn hello(first: bool) -> String {
    json!({"t":"hello","d":{
        "first": first,
        "user":{"id":"u_1","nick":"waseem","role":"member"},
        "commands":[],
        "server":{"name":"gliph.test","upload_max":1024,"motd":null,"news":[]}
    }})
    .to_string()
}

fn in_general(c: &mut Client) {
    c.handle_frame(
        &json!({"t":"state","d":{
            "channels":[{"id":"c_1","name":"general","is_dm":false,"role":"member","seq":1},
                        {"id":"c_2","name":"ops","is_dm":false,"role":"member","seq":1}],
            "user":{"id":"u_1","nick":"waseem","role":"member"}
        }}).to_string(),
        NOW,
    );
    c.set_active("c_1");
}

fn cache(fx: &Effects) -> Option<String> {
    fx.iter().rev().find_map(|e| match e {
        Effect::Persist { key, value, .. } if key.starts_with("gliph.cache") => Some(value.clone()),
        _ => None,
    })
}

fn tour_shown(c: &Client) -> bool {
    c.system_messages().iter().any(|m| m.id == "tour" && m.kind == "motd" && m.body.contains('/'))
}

#[test]
fn a_new_account_gets_the_tour_in_general_only() {
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem"}"#);
    c.handle_frame(&hello(true), NOW);
    in_general(&mut c);
    assert!(tour_shown(&c), "a brand-new account landed in #general with no tour");
    c.set_active("c_2");
    assert!(!tour_shown(&c), "the tour followed the reader out of #general");
}

#[test]
fn an_existing_account_never_sees_the_tour() {
    let mut c = Client::new();
    c.handle_frame(&hello(false), NOW);
    in_general(&mut c);
    assert!(!tour_shown(&c));
}

#[test]
fn the_tour_survives_a_reload_until_dismissed_and_never_repeats() {
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem"}"#);
    let saved = cache(&c.handle_frame(&hello(true), NOW)).expect("the tour was not saved");

    // A reload before reading it: the next hello no longer says first.
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem"}"#);
    c.restore("", &saved, "");
    c.handle_frame(&hello(false), NOW);
    in_general(&mut c);
    assert!(tour_shown(&c), "a reload lost the tour before it was read");

    let saved = cache(&c.dismiss_system("tour")).expect("the dismissal was not saved");
    assert!(!tour_shown(&c), "the tour stayed up after ×");

    let mut c = Client::new();
    c.restore("", &saved, "");
    c.handle_frame(&hello(false), NOW);
    in_general(&mut c);
    assert!(!tour_shown(&c), "a dismissed tour came back");
}
