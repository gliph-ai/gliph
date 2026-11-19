//! What the client DOES, asserted without a network, a filesystem or a
//! simulator. This is what sans-io was for: every one of these rules used to be
//! reachable only by driving a real app against a real server, which is why
//! most of them were never checked at all.

use gliph_core::effect::{Effect, Store};
use gliph_core::{Client, Effects};
use serde_json::{json, Value};

const NOW: i64 = 1_800_000_000_000; // a fixed clock; the core never reads one

fn sent(fx: &Effects) -> Vec<Value> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::Send { json } => serde_json::from_str(json).ok(),
            _ => None,
        })
        .collect()
}

/// The message frame out of a batch, chosen by type rather than position.
/// Sending also marks the channel read, and a read mark is its own frame, so
/// the message is not reliably first and never was guaranteed to be.
fn sent_msg(fx: &Effects) -> Value {
    sent(fx).into_iter().find(|f| f["t"] == "message.send").expect("a message.send frame")
}

fn sent_types(fx: &Effects) -> Vec<String> {
    sent(fx).iter().map(|f| f["t"].as_str().unwrap_or_default().to_string()).collect()
}

fn alerts(fx: &Effects) -> Vec<(String, String)> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::Alert { nick, text, .. } => Some((nick.clone(), text.clone())),
            _ => None,
        })
        .collect()
}

fn alert_events(fx: &Effects) -> Vec<String> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::Alert { event, .. } => Some(event.clone()),
            _ => None,
        })
        .collect()
}

fn persisted<'a>(fx: &'a Effects, want: &str) -> Option<&'a str> {
    fx.iter().find_map(|e| match e {
        Effect::Persist { key, value, .. } if key == want => Some(value.as_str()),
        _ => None,
    })
}

/// A signed-in client sitting in #general with one other member.
fn seated() -> Client {
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem","role":"admin"}"#);
    c.connected("test", NOW);
    c.handle_frame(
        &json!({"t":"hello","d":{
            "user":{"id":"u_1","nick":"waseem","role":"admin"},
            "commands":[
                {"name":"topic","usage":"/topic <text>","desc":"set the topic","scope":"channel","min_role":"op","source":"builtin"},
                {"name":"kick","usage":"/kick <nick>","desc":"remove someone","scope":"channel","min_role":"op","source":"builtin"},
                {"name":"deploy","usage":"/deploy <env>","desc":"ship it","scope":"channel","min_role":"member","source":"deploybot"}
            ],
            "server":{"name":"gliph.test","upload_max":1024}
        }}).to_string(),
        NOW,
    );
    c.handle_frame(
        &json!({"t":"state","d":{
            "channels":[{"id":"c_1","name":"general","topic":"hi","modes":"nt","is_dm":false,
                         "role":"op","seq":10,
                         "members":[{"user_id":"u_1","nick":"waseem","role":"op","kind":"user"},
                                    {"user_id":"u_2","nick":"robin","role":"member","kind":"user"}]}],
            "user":{"id":"u_1","nick":"waseem","role":"admin"},
            "prefs":{"aliases":{"gm":"/topic good morning"},"highlights":["ship"]}
        }}).to_string(),
        NOW,
    );
    c.set_active("c_1");
    c.go_live();
    c
}

fn event(seq: i64, actor: &str, body: &str) -> Value {
    json!({"t":"event","d":{
        "seq": seq, "id": format!("e_{seq}"), "channel":"c_1", "kind":"message",
        "actor": actor, "ts": "2027-01-15T12:00:00Z", "body": body
    }})
}

// ---- the command palette ----

#[test]
fn the_palette_is_the_servers_and_never_ours() {
    let c = seated();
    let names: Vec<&str> = c.match_commands("/").iter().map(|x| x.name.as_str()).collect();
    assert_eq!(names, vec!["topic", "kick", "deploy"], "every entry came from the manifest");

    // A plugin disconnecting shrinks the palette, with no client-side list to
    // fall out of step with it.
    let mut c = c;
    c.handle_frame(
        &json!({"t":"commands","d":{"commands":[
            {"name":"topic","usage":"/topic <text>","desc":"set the topic","scope":"channel","min_role":"op","source":"builtin"}
        ]}}).to_string(),
        NOW,
    );
    let names: Vec<&str> = c.match_commands("/").iter().map(|x| x.name.as_str()).collect();
    assert_eq!(names, vec!["topic"]);
}

#[test]
fn typing_arguments_closes_the_palette() {
    let c = seated();
    assert_eq!(c.match_commands("/to").len(), 1);
    assert!(c.match_commands("/topic ").is_empty(), "already typing arguments");
    assert!(c.match_commands("hello").is_empty(), "not a command at all");
}

// ---- local vs server commands ----

#[test]
fn ignore_is_local_and_silent() {
    let mut c = seated();
    let fx = c.submit("/ignore robin");
    assert!(sent(&fx).is_empty(), "/ignore must never reach the server");

    c.handle_frame(&event(11, "robin", "still here").to_string(), NOW);
    assert!(
        c.visible_events("c_1").iter().all(|e| e.actor != "robin"),
        "an ignored nick is filtered out of the stream"
    );

    c.submit("/unignore robin");
    c.handle_frame(&event(12, "robin", "back").to_string(), NOW);
    assert!(c.visible_events("c_1").iter().any(|e| e.actor == "robin"));
}

#[test]
fn an_alias_expands_once_and_is_not_a_loop() {
    let mut c = seated();
    let fx = c.submit("/gm");
    let frames = sent(&fx);
    assert_eq!(frames.len(), 1);
    assert_eq!(frames[0]["t"], "command.run");
    assert_eq!(frames[0]["d"]["name"], "topic");
    assert_eq!(frames[0]["d"]["args"], "good morning");
}

#[test]
fn an_unknown_command_goes_to_the_server_to_judge() {
    let mut c = seated();
    let fx = c.submit("/wibble now");
    let frames = sent(&fx);
    assert_eq!(frames[0]["d"]["name"], "wibble", "the client does not decide what exists");
}

// ---- unread ----

#[test]
fn unread_counts_from_the_servers_seq_not_from_rows_held() {
    let mut c = seated();
    assert_eq!(c.unread("c_1"), 0, "first sight of a channel is not a pile of unread");

    // The server says it is at 14; we hold none of those events.
    c.handle_frame(
        &json!({"t":"state","d":{"channels":[{"id":"c_1","name":"general","modes":"nt",
            "role":"op","seq":14,"members":[]}]}}).to_string(),
        NOW,
    );
    assert_eq!(c.unread("c_1"), 4, "the seq is authoritative, not the local cache");
    assert_eq!(c.unread_total(), 4);

    let mut fx = Effects::new();
    c.mark_read("c_1", &mut fx);
    assert_eq!(c.unread("c_1"), 0);
}

#[test]
fn an_event_moves_the_badge_and_a_reply_clears_it() {
    let mut c = seated();
    let ev = |seq: i64, actor: &str| json!({"t":"event","d":{"seq":seq,"id":format!("e{seq}"),
        "channel":"c_1","kind":"message","actor":actor,"body":"hi"}}).to_string();
    c.handle_frame(&ev(11, "rizwan"), NOW);
    assert_eq!(c.unread("c_1"), 1, "the badge follows the event, not the next state frame");

    c.set_active("c_2");
    c.handle_frame(&ev(12, "rizwan"), NOW);
    assert_eq!(c.unread("c_1"), 2);
    c.set_active("c_1");
    assert_eq!(c.unread("c_1"), 0, "opening the room reads it");

    c.handle_frame(&ev(13, "rizwan"), NOW);
    assert_eq!(c.unread("c_1"), 1);
    c.send_message("hello");
    assert_eq!(c.unread("c_1"), 0, "replying is reading");
    c.handle_frame(&ev(14, "waseem"), NOW);
    assert_eq!(c.unread("c_1"), 0, "your own line is never unread");

    c.set_active("c_2");
    c.handle_frame(&ev(15, "rizwan"), NOW);
    c.handle_frame(&ev(16, "waseem"), NOW);
    assert_eq!(c.unread("c_1"), 0, "a reply from another device reads the room here too");
}

// ---- announcements ----

#[test]
fn a_highlight_announces_and_ordinary_chatter_does_not() {
    let mut c = seated();
    let fx = c.handle_frame(&event(11, "robin", "morning all").to_string(), NOW);
    assert!(alerts(&fx).is_empty(), "a plain line in a channel is not an interruption");

    let fx = c.handle_frame(&event(12, "robin", "can we SHIP this today").to_string(), NOW);
    assert_eq!(alerts(&fx).len(), 1, "a highlight word is");
    assert_eq!(alerts(&fx)[0].0, "robin");
    assert_eq!(alert_events(&fx), ["highlight"], "and it says why it fired");
}

#[test]
fn an_at_tag_is_a_mention_and_outranks_a_highlight_word() {
    let mut c = seated();
    let fx = c.handle_frame(&event(12, "robin", "@Waseem can we ship this").to_string(), NOW);
    assert_eq!(alert_events(&fx), ["mention"], "tagged beats a highlight word in the same line");

    let fx = c.handle_frame(&event(13, "robin", "mail @waseemx about it").to_string(), NOW);
    assert!(alerts(&fx).is_empty(), "@waseemx is someone else");

    let fx = c.handle_frame(&event(14, "robin", "hey @waseem?").to_string(), NOW);
    assert_eq!(alert_events(&fx), ["mention"], "punctuation ends the tag");
}

#[test]
fn my_own_line_never_announces() {
    let mut c = seated();
    let fx = c.handle_frame(&event(11, "waseem", "ship it").to_string(), NOW);
    assert!(alerts(&fx).is_empty());
}

#[test]
fn an_ignored_nick_cannot_announce() {
    let mut c = seated();
    c.submit("/ignore robin");
    let fx = c.handle_frame(&event(11, "robin", "ship it").to_string(), NOW);
    assert!(alerts(&fx).is_empty(), "ignore is silence, not just a filter");
}

#[test]
fn a_backlog_replayed_after_a_wake_stays_quiet() {
    let mut c = seated();
    // The event carries 12:00Z; tell the core it is now 14:00Z. Waking after a
    // suspend replays everything past our cursor, and that is history, not news.
    let event_ms = 1_800_014_400_000; // 2027-01-15T12:00:00Z
    let fx = c.handle_frame(&event(11, "robin", "ship it").to_string(), event_ms + 2 * 3_600_000);
    assert!(alerts(&fx).is_empty(), "a two-hour-old line is not an interruption");

    // The same line, heard as it happens, is.
    let fx = c.handle_frame(&event(12, "robin", "ship it now").to_string(), event_ms + 1_000);
    assert_eq!(alerts(&fx).len(), 1);
}

#[test]
fn nothing_announces_before_catch_up_has_settled() {
    let mut c = seated();
    c.disconnected(NOW);
    c.connected("test", NOW); // live is false again until go_live
    let fx = c.handle_frame(&event(11, "robin", "ship it").to_string(), NOW);
    assert!(alerts(&fx).is_empty(), "reconnecting must not fire forty banners");
    c.go_live();
    let fx = c.handle_frame(&event(12, "robin", "ship it now").to_string(), NOW);
    assert_eq!(alerts(&fx).len(), 1);
}

#[test]
fn a_netsplit_line_is_dated_and_ages_out_of_the_scrollback() {
    let mut c = seated();
    c.disconnected(NOW);
    c.tick(NOW + 5_000);

    let line = c
        .visible_events("c_1")
        .into_iter()
        .find(|e| e.body.starts_with("* disconnected"))
        .expect("the netsplit says so in the stream");
    assert_eq!(line.ts, "2027-01-15T08:00:05Z", "a made-up line still carries a real clock");

    c.tick(NOW + 60_000);
    assert!(
        c.visible_events("c_1").iter().any(|e| e.body.starts_with("* disconnected")),
        "a minute on, it is still news"
    );
    c.tick(NOW + 10 * 60_000);
    assert!(
        !c.visible_events("c_1").iter().any(|e| e.body.starts_with("* disconnected")),
        "ten minutes on, it is only clutter"
    );
}

#[test]
fn a_blip_that_reconnects_inside_the_grace_window_says_nothing() {
    let mut c = seated();
    c.disconnected(NOW);
    c.tick(NOW + 1_000);
    assert!(c.visible_events("c_1").is_empty(), "a second down is not yet a netsplit");
    c.connected("test", NOW + 2_000);
    c.tick(NOW + 10_000);
    assert!(c.visible_events("c_1").is_empty(), "neither line for a blip nobody noticed");

    c.disconnected(NOW + 20_000);
    c.tick(NOW + 25_000);
    c.connected("test", NOW + 26_000);
    let bodies: Vec<String> = c.visible_events("c_1").iter().map(|e| e.body.clone()).collect();
    assert!(bodies.iter().any(|b| b.starts_with("* disconnected")), "a real outage is still said");
    assert!(bodies.iter().any(|b| b == "* reconnected"), "and so is its end");
}

#[test]
fn an_undated_system_line_from_the_cache_is_swept_not_kept_forever() {
    // Builds before 0.1.x wrote netsplit lines with no clock; those came back
    // out of the cache on every launch as "[NaN:NaN]".
    let mut c = Client::new();
    let cache = json!({
        "cursors": {}, "read": {}, "queue": [], "ignores": [],
        "events": { "c_1": [{ "seq": 1.5, "id": "sys-old", "channel": "c_1", "kind": "notice",
            "actor": "*", "ts": "", "body": "* reconnected", "meta": { "local": true } }] }
    });
    c.restore("", &cache.to_string(), "");
    assert!(c.visible_events("c_1").iter().any(|e| e.id == "sys-old"), "restored as written");
    c.tick(NOW);
    assert!(c.visible_events("c_1").is_empty(), "the first sweep drops a line with no date");
}

// ---- the offline queue ----

#[test]
fn typing_while_offline_queues_and_flushes_in_order() {
    let mut c = seated();
    c.disconnected(NOW);

    let fx = c.send_message("first");
    assert!(sent(&fx).is_empty(), "nothing goes on a socket that is not there");
    assert!(
        c.visible_events("c_1").iter().any(|e| e.body == "first"),
        "but it shows locally — offline typing is not a black hole"
    );
    c.send_message("second");

    let fx = c.connected("test", NOW);
    let bodies: Vec<String> = sent(&fx)
        .iter()
        .filter(|f| f["t"] == "message.send")
        .map(|f| f["d"]["body"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(bodies, vec!["first", "second"], "the queue drains in order");

    // The local pending copies are gone; the server's echo replaces them.
    assert!(!c.visible_events("c_1").iter().any(|e| e.id.starts_with("pending-")));
}

// ---- encryption ----

/// The account's first device: the server has no master key on file and says
/// this device may mint one. Returns this device's `keys.peers` row, signed.
fn establish(c: &mut Client) -> Value {
    let hello = sent(&c.connected("test", NOW)).into_iter().find(|f| f["t"] == "device.hello").unwrap();
    let (id, pubkey) = (hello["d"]["device"].clone(), hello["d"]["pubkey"].clone());
    let fx = c.handle_frame(
        &json!({"t":"device.trust","d":{"master":"","can_establish":true,
            "devices":[{"id":id,"pubkey":pubkey,"user_id":"u_1"}]}})
        .to_string(),
        NOW,
    );
    let sign = sent(&fx).into_iter().find(|f| f["t"] == "device.sign").expect("first device signs itself");
    let master = sign["d"]["master"].clone();
    let sig = sign["d"]["sigs"][id.as_str().unwrap()].clone();
    let row = json!({"id":id,"pubkey":pubkey,"user_id":"u_1","sig":sig,"master":master});
    c.handle_frame(&json!({"t":"device.trust","d":{"master":master,"devices":[row.clone()]}}).to_string(), NOW);
    assert!(c.trust.signed);
    row
}

/// A second device of the same account, signed in and connected, unapproved.
fn second_device(master: &Value, first: &Value) -> (Client, Value) {
    let mut b = Client::new();
    b.set_session("https://gliph.test", "tok2", r#"{"id":"u_1","nick":"waseem","role":"admin"}"#);
    let hello = sent(&b.connected("phone", NOW)).into_iter().find(|f| f["t"] == "device.hello").unwrap();
    let row = json!({"id":hello["d"]["device"],"pubkey":hello["d"]["pubkey"],"user_id":"u_1",
        "name":"phone","master":master});
    b.handle_frame(
        &json!({"t":"device.trust","d":{"master":master,"devices":[first, row]}}).to_string(),
        NOW,
    );
    (b, row)
}

fn vault(history: &str, members: &[&str]) -> String {
    let members: Vec<Value> =
        members.iter().map(|u| json!({"user_id":u,"nick":u,"role":"op","kind":"user"})).collect();
    json!({"t":"state","d":{"channels":[{"id":"c_9","name":"vault","modes":"nte","history":history,
        "role":"op","seq":0,"members":members}]}})
    .to_string()
}

fn publishes(fx: &Effects) -> Vec<Value> {
    sent(fx).into_iter().filter(|f| f["t"] == "keys.publish").collect()
}

#[test]
fn a_second_device_reads_nothing_until_the_first_approves_it() {
    let mut a = seated();
    a.handle_frame(&vault("open", &["u_1"]), NOW);
    let first = establish(&mut a);
    let master = first["master"].clone();
    let (mut b, b_row) = second_device(&master, &first);

    // B knows it is waiting and shows its code; it will not mint keys.
    assert!(!b.trust.signed);
    assert_eq!(b.trust.code.len(), 14);
    // ...and the same code as a square QR code for a phone camera.
    assert!(!b.trust.qr.is_empty());
    assert!(b.trust.qr.iter().all(|r| r.len() == b.trust.qr.len()));
    assert_eq!(gliph_core::client::approve_qr(&b.trust.code), b.trust.qr);
    b.handle_frame(&vault("open", &["u_1"]), NOW);
    let fx = b.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,"devices":[b_row]}}).to_string(),
        NOW,
    );
    assert!(publishes(&fx).is_empty(), "an unapproved device mints nothing");

    // A sees B waiting, and seals nothing to it while it is unsigned.
    a.handle_frame(
        &json!({"t":"device.trust","d":{"master":master,"devices":[first, b_row]}}).to_string(),
        NOW,
    );
    assert_eq!(a.trust.pending.len(), 1);
    assert_eq!(a.trust.pending[0].code, b.trust.code);
    a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,"devices":[first]}}).to_string(),
        NOW,
    );
    let fx = a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":1,"max_epoch":1,"devices":[b_row]}}).to_string(),
        NOW,
    );
    assert!(publishes(&fx).is_empty(), "no key is sealed to an unsigned device");

    // A wrong code approves nothing.
    let fx = a.submit("/approve 0000-0000-0000");
    assert!(!sent_types(&fx).contains(&"device.sign".to_string()));

    // The right one signs B and seals the master key to it.
    let fx = a.submit(&format!("/approve {}", b.trust.code));
    let sign = sent(&fx).into_iter().find(|f| f["t"] == "device.sign").expect("device.sign");
    let b_id = b_row["id"].as_str().unwrap();
    let sig = sign["d"]["sigs"][b_id].clone();
    let sealed = sign["d"]["sealed"][b_id].clone();
    let mut b_signed = b_row.clone();
    b_signed["sig"] = sig;

    // B learns it is signed, and can now approve devices itself.
    b.handle_frame(
        &json!({"t":"device.trust","d":{"master":master,"sealed_master":sealed,
            "devices":[first, b_signed]}})
        .to_string(),
        NOW,
    );
    assert!(b.trust.signed);
    assert!(b.trust.qr.is_empty(), "a signed device shows no QR code");
    let c_row = json!({"id":"d_3","pubkey":first["pubkey"],"user_id":"u_1","master":master});
    b.handle_frame(
        &json!({"t":"device.trust","d":{"master":master,"devices":[first, b_signed, c_row]}}).to_string(),
        NOW,
    );
    let fx = b.submit(&format!("/approve {}", b.trust.pending[0].code));
    assert!(sent_types(&fx).contains(&"device.sign".to_string()), "the master key came with the approval");

    // And A now seals the channel key to B, which can read what A says.
    let fx = a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":1,"max_epoch":1,"devices":[b_signed]}}).to_string(),
        NOW,
    );
    let p = publishes(&fx);
    assert_eq!(p.len(), 1);
    let wrapped = p[0]["d"]["entries"][b_id].clone();
    a.set_active("c_9");
    let body = sent_msg(&a.send_message("hello from the laptop"))["d"]["body"].clone();
    b.handle_frame(
        &json!({"t":"keys","d":{"channel":"c_9","keys":[{"epoch":1,"wrapped":wrapped}]}}).to_string(),
        NOW,
    );
    b.handle_frame(
        &json!({"t":"event","d":{"seq":1,"id":"e_l","channel":"c_9","kind":"message",
            "actor":"waseem","ts":"2027-01-15T12:00:00Z","body":body,"enc":true,"epoch":1}})
        .to_string(),
        NOW,
    );
    assert_eq!(b.plain_body("e_l").as_deref(), Some("hello from the laptop"));
}

#[test]
fn a_device_signed_by_some_other_master_gets_no_key() {
    let mut a = seated();
    a.handle_frame(&vault("open", &["u_1"]), NOW);
    let first = establish(&mut a);
    a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,"devices":[first]}}).to_string(),
        NOW,
    );
    // A server that slips in a device of its own, signed by a key it made up.
    let mut rogue = Client::new();
    rogue.set_session("https://gliph.test", "t", r#"{"id":"u_1","nick":"waseem","role":"admin"}"#);
    let forged = establish(&mut rogue);
    let mut row = forged.clone();
    row["master"] = first["master"].clone();
    let fx = a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":1,"max_epoch":1,"devices":[row]}}).to_string(),
        NOW,
    );
    assert!(publishes(&fx).is_empty());
}

/// Two epochs held by A, and a newcomer device asking for the current one.
fn two_epochs(history: &str) -> (Client, Value) {
    let mut a = seated();
    a.handle_frame(&vault(history, &["u_1", "u_2"]), NOW);
    let first = establish(&mut a);
    a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,"devices":[first]}}).to_string(),
        NOW,
    );
    a.handle_frame(&vault(history, &["u_1"]), NOW); // u_2 leaves: epoch 2
    let mut dana = Client::new();
    dana.set_session("https://gliph.test", "t", r#"{"id":"u_3","nick":"dana","role":"member"}"#);
    let mut row = establish_bare(&mut dana);
    row["user_id"] = json!("u_3");
    (a, row)
}

/// `establish` for an account with no other devices on file.
fn establish_bare(c: &mut Client) -> Value {
    let hello = sent(&c.connected("test", NOW)).into_iter().find(|f| f["t"] == "device.hello").unwrap();
    let (id, pubkey) = (hello["d"]["device"].clone(), hello["d"]["pubkey"].clone());
    let fx = c.handle_frame(
        &json!({"t":"device.trust","d":{"master":"","can_establish":true,"devices":[]}}).to_string(),
        NOW,
    );
    let sign = sent(&fx).into_iter().find(|f| f["t"] == "device.sign").unwrap();
    json!({"id":id,"pubkey":pubkey,"sig":sign["d"]["sigs"][id.as_str().unwrap()],"master":sign["d"]["master"]})
}

#[test]
fn an_open_channel_hands_a_newcomer_every_epoch() {
    let (mut a, row) = two_epochs("open");
    let fx = a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":2,"max_epoch":2,"devices":[row]}}).to_string(),
        NOW,
    );
    let epochs: Vec<i64> = publishes(&fx).iter().map(|p| p["d"]["epoch"].as_i64().unwrap()).collect();
    assert_eq!(epochs, vec![1, 2], "history from before the join comes with it");
}

#[test]
fn a_sealed_channel_hands_a_newcomer_only_the_current_epoch_and_rotates_on_arrival() {
    let (mut a, row) = two_epochs("sealed");
    let fx = a.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":2,"max_epoch":2,"devices":[row]}}).to_string(),
        NOW,
    );
    let epochs: Vec<i64> = publishes(&fx).iter().map(|p| p["d"]["epoch"].as_i64().unwrap()).collect();
    assert_eq!(epochs, vec![2]);

    // Someone walks in: a sealed channel mints a key they never had.
    let fx = a.handle_frame(&vault("sealed", &["u_1", "u_3"]), NOW);
    assert!(sent(&fx).iter().any(|f| f["t"] == "keys.peers" && f["d"]["epoch"] == 3));
}

#[test]
fn an_open_channel_does_not_rotate_when_someone_joins() {
    let (mut a, _) = two_epochs("open");
    let fx = a.handle_frame(&vault("open", &["u_1", "u_3"]), NOW);
    assert!(!sent(&fx).iter().any(|f| f["t"] == "keys.peers" && f["d"]["epoch"] == 3));
}

#[test]
fn a_plus_e_channel_never_puts_plaintext_on_the_wire() {
    let mut c = seated();
    c.handle_frame(
        &json!({"t":"state","d":{"channels":[{"id":"c_9","name":"vault","modes":"nte",
            "role":"op","seq":0,"members":[{"user_id":"u_1","nick":"waseem","role":"op","kind":"user"}]}]}})
        .to_string(),
        NOW,
    );
    c.set_active("c_9");

    // No key yet: refuse rather than silently downgrading.
    let fx = c.send_message("the merger is off");
    assert!(sent(&fx).is_empty(), "no key must mean no send, never a plaintext fallback");

    // The server says nobody has keyed it and lists our device; we mint epoch 1.
    let me = establish(&mut c);
    let fx = c.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,
            "devices":[me]}}).to_string(),
        NOW,
    );
    assert!(sent_types(&fx).contains(&"keys.publish".to_string()));

    let fx = c.send_message("the merger is off");
    let msg = sent(&fx).into_iter().find(|f| f["t"] == "message.send").expect("message.send");
    assert_eq!(msg["d"]["enc"], true);
    assert_eq!(msg["d"]["epoch"], 1);
    let body = msg["d"]["body"].as_str().unwrap();
    assert!(!body.contains("merger"), "the server must never see the text");

    // And it comes back readable: the echo is ciphertext this device can open.
    c.handle_frame(
        &json!({"t":"event","d":{"seq":1,"id":"e_x","channel":"c_9","kind":"message",
            "actor":"waseem","ts":"2027-01-15T12:00:00Z","body":body,"enc":true,"epoch":1}})
        .to_string(),
        NOW,
    );
    assert_eq!(c.plain_body("e_x").as_deref(), Some("the merger is off"));
}

#[test]
fn a_member_leaving_rotates_the_epoch() {
    let mut c = seated();
    let two = json!({"t":"state","d":{"channels":[{"id":"c_9","name":"vault","modes":"nte",
        "role":"op","seq":0,"members":[
            {"user_id":"u_1","nick":"waseem","role":"op","kind":"user"},
            {"user_id":"u_2","nick":"robin","role":"member","kind":"user"}]}]}});
    c.handle_frame(&two.to_string(), NOW);

    let me = establish(&mut c);
    c.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,
            "devices":[me]}}).to_string(),
        NOW,
    );

    // robin leaves.
    let one = json!({"t":"state","d":{"channels":[{"id":"c_9","name":"vault","modes":"nte",
        "role":"op","seq":0,"members":[{"user_id":"u_1","nick":"waseem","role":"op","kind":"user"}]}]}});
    let fx = c.handle_frame(&one.to_string(), NOW);
    let peers: Vec<Value> = sent(&fx).into_iter().filter(|f| f["t"] == "keys.peers").collect();
    assert!(
        peers.iter().any(|f| f["d"]["epoch"] == 2),
        "a shrink mints the next epoch, so the key they hold stops working"
    );
}

#[test]
fn typing_offline_in_a_plus_e_channel_queues_and_seals_on_reconnect() {
    let mut c = seated();
    c.handle_frame(
        &json!({"t":"state","d":{"channels":[{"id":"c_9","name":"vault","modes":"nte",
            "role":"op","seq":0,"members":[{"user_id":"u_1","nick":"waseem","role":"op","kind":"user"}]}]}})
        .to_string(),
        NOW,
    );
    c.set_active("c_9");
    let me = establish(&mut c);
    c.handle_frame(
        &json!({"t":"keys.peers","d":{"channel":"c_9","epoch":0,"max_epoch":0,
            "devices":[me]}}).to_string(),
        NOW,
    );

    c.disconnected(NOW);
    let fx = c.send_message("the merger is off");
    assert!(sent(&fx).is_empty());
    assert!(c.visible_events("c_9").iter().any(|e| e.body == "the merger is off"), "shows as pending");

    let fx = c.connected("test", NOW);
    let msg = sent(&fx).into_iter().find(|f| f["t"] == "message.send").expect("queued message goes out");
    assert_eq!(msg["d"]["enc"], true);
    assert!(!msg["d"]["body"].as_str().unwrap().contains("merger"), "sealed, not plaintext");
    assert!(!c.visible_events("c_9").iter().any(|e| e.id.starts_with("pending-")));
}

// ---- persistence ----

#[test]
fn a_relaunch_restores_the_scrollback_and_the_read_mark() {
    let mut c = seated();
    // Ingesting an event is what writes the cache — `mark_read` deliberately
    // does not, because the read mark had not moved.
    let fx = c.handle_frame(&event(11, "robin", "before the crash").to_string(), NOW);
    // The cache is per-server now, so the switcher can hold one server's
    // scrollback while you read another's.
    let cache = persisted(&fx, &gliph_core::client::cache_key("https://gliph.test"))
        .expect("cache written")
        .to_string();

    let mut fresh = Client::new();
    fresh.restore("", &cache, "");
    assert!(
        fresh.visible_events("c_1").iter().any(|e| e.body == "before the crash"),
        "history survives a cold start"
    );
}

#[test]
fn the_device_secret_is_flagged_for_the_keychain() {
    let mut c = Client::new();
    let fx = c.connected("test", NOW);
    let secret = fx
        .iter()
        .find_map(|e| match e {
            Effect::Persist { key, store, .. } if key == gliph_core::client::KEY_DEVICE => {
                Some(*store)
            }
            _ => None,
        })
        .expect("the device key is persisted");
    assert_eq!(secret, Store::Secret, "the one secret must not land in a plist");
}

#[test]
fn signing_out_forgets_the_key_material() {
    let mut c = seated();
    let fx = c.logout(true);
    let forgotten: Vec<&str> = fx
        .iter()
        .filter_map(|e| match e {
            Effect::Forget { key, .. } => Some(key.as_str()),
            _ => None,
        })
        .collect();
    assert!(forgotten.contains(&gliph_core::client::KEY_DEVICE));
    // Only THIS server's cache. Signing out of one team must not wipe the
    // scrollback of the others.
    assert!(forgotten.contains(&gliph_core::client::cache_key("https://gliph.test").as_str()));
    assert!(!forgotten.contains(&gliph_core::client::KEY_CACHE));
    assert!(c.token.is_empty());
    assert_eq!(c.server, "https://gliph.test", "where, but not who");
}

// Signing out of one server while others stay signed in keeps the device key:
// their channel keys are wrapped to it, and a new key would leave their sealed
// history unreadable.
#[test]
fn signing_out_of_one_server_keeps_the_device_key() {
    let mut c = seated();
    let fx = c.logout(false);
    assert!(!fx.iter().any(|e| matches!(e, Effect::Forget { key, .. } if key == gliph_core::client::KEY_DEVICE)));
    assert!(fx.iter().any(|e| matches!(e, Effect::Forget { key, .. } if key == &gliph_core::client::cache_key("https://gliph.test"))));
    assert!(c.token.is_empty());
}

// ---- nearby (F9) ----

#[test]
fn a_nearby_channel_works_with_the_server_down() {
    let mut c = seated();
    c.disconnected(NOW);
    let fx = c.submit("/nearby flight");
    assert!(
        fx.iter().any(|e| matches!(e, Effect::MeshStart { .. })),
        "nearby starts without asking a server for permission"
    );
    assert!(c.active.starts_with("n_"));

    let fx = c.send_message("wheels up");
    assert!(sent(&fx).is_empty(), "a nearby line never touches the socket");
    assert!(fx.iter().any(|e| matches!(e, Effect::MeshSay { .. })));
}

#[test]
fn a_nearby_arrival_announces_like_a_dm() {
    let mut c = seated();
    c.submit("/nearby flight");
    c.mesh_peers(r#"[{"id":"p1","nick":"robin","chans":["flight"]}]"#);
    let fx = c.mesh_msg(
        &json!({"id":"n_1","chan":"flight","nick":"robin","body":"anyone got a charger",
                "ts":"2027-01-15T12:00:00Z"}).to_string(),
        NOW,
    );
    assert_eq!(
        alerts(&fx).len(),
        1,
        "five people who joined a room on purpose should not need a highlight word"
    );
}

#[test]
fn nearby_scrollback_is_never_persisted() {
    let mut c = seated();
    c.submit("/nearby flight");
    c.mesh_peers(r#"[{"id":"p1","nick":"robin","chans":["flight"]}]"#);
    c.mesh_msg(
        &json!({"id":"n_1","chan":"flight","nick":"robin","body":"secret","ts":"2027-01-15T12:00:00Z"})
            .to_string(),
        NOW,
    );
    let fx = c.set_active("c_1");
    let cache = persisted(&fx, gliph_core::client::KEY_CACHE).unwrap_or("");
    assert!(
        !cache.contains("secret"),
        "\"gone when the last peer leaves\" has to be true on disk too"
    );
}

#[test]
fn nearby_commands_appear_and_disappear_with_the_mode() {
    let mut c = seated();
    assert!(!c.match_commands("/near").iter().any(|x| x.name == "nearby"));
    c.submit("/nearby");
    assert!(c.match_commands("/near").iter().any(|x| x.name == "nearby-leave"));
    c.submit("/nearby-off");
    assert!(
        !c.match_commands("/near").iter().any(|x| x.name == "nearby-leave"),
        "the palette shrinks the way it does when a plugin leaves"
    );
}

#[test]
fn a_new_accounts_arrival_announces_and_a_plain_join_does_not() {
    let mut c = seated();
    let join = |seq: i64, meta: Value| {
        json!({"t":"event","d":{
            "seq": seq, "id": format!("e_{seq}"), "channel":"c_1", "kind":"join",
            "actor":"newbie", "ts":"2027-01-15T12:00:00Z", "body":"joined the server", "meta": meta
        }})
    };
    let fx = c.handle_frame(&join(21, Value::Null).to_string(), NOW);
    assert!(alerts(&fx).is_empty(), "an existing member's /join is a stream line, not an interruption");

    let fx = c.handle_frame(&join(22, json!({"new": true})).to_string(), NOW);
    assert_eq!(alert_events(&fx), ["join"], "a first arrival on the server is announced");
    assert_eq!(alerts(&fx)[0], ("newbie".into(), "joined the server".into()));
}

// ---- replies ----

#[test]
fn a_reply_carries_the_quote_on_the_wire_and_clears_itself() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "the freeze is on for friday").to_string(), NOW);

    c.set_reply("e_11");
    assert_eq!(
        c.reply_display(), "<robin> the freeze is on for friday",
        "the composer strip is exactly the line that will be sent",
    );

    let fx = c.send_message("works for me");
    let frame = &sent_msg(&fx);
    assert_eq!(
        frame["d"]["body"].as_str().unwrap(),
        "> <robin> the freeze is on for friday\nworks for me",
    );
    assert_eq!(
        frame["d"]["reply_to"], "e_11",
        "the id rides alongside the quote: the quote is what you read, the id is what you follow",
    );
    assert_eq!(c.reply_display(), "", "sending is what ends a reply");

    let fx = c.send_message("and another thing");
    assert_eq!(
        sent_msg(&fx)["d"]["body"].as_str().unwrap(), "and another thing",
        "the next message is not quietly a reply too",
    );
    assert_eq!(sent_msg(&fx)["d"]["reply_to"], "", "nor does the id linger");
}

#[test]
fn a_reply_queued_offline_still_names_its_target_when_it_flushes() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "the freeze is on for friday").to_string(), NOW);
    c.disconnected(NOW);
    c.set_reply("e_11");
    c.send_message("works for me");

    let fx = c.connected("test", NOW);
    let send = sent(&fx).into_iter().find(|f| f["t"] == "message.send").expect("queued message");
    assert_eq!(send["d"]["reply_to"], "e_11", "the target survives the queue");
    assert_eq!(
        send["d"]["body"].as_str().unwrap(),
        "> <robin> the freeze is on for friday\nworks for me",
    );
}

#[test]
fn a_reply_splits_into_quote_and_what_was_said() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "the freeze is on for friday").to_string(), NOW);
    c.set_reply("e_11");
    c.send_message("works for me");
    // The echo the server sends back to everyone.
    c.handle_frame(
        &event(12, "waseem", "> <robin> the freeze is on for friday\nworks for me").to_string(),
        NOW,
    );

    let rows: Value = serde_json::from_str(&gliph_core::api::events_json(&c, "c_1")).unwrap();
    let row = rows.as_array().unwrap().iter().find(|r| r["id"] == "e_12").unwrap();
    assert_eq!(row["quote"], "<robin> the freeze is on for friday");
    assert_eq!(row["text"], "works for me", "a frontend draws two strings and parses nothing");
}

#[test]
fn replying_to_a_reply_quotes_what_it_said() {
    let mut c = seated();
    c.handle_frame(
        &event(11, "robin", "> <mika> ship it friday\nagreed, friday").to_string(),
        NOW,
    );
    c.set_reply("e_11");
    assert_eq!(
        c.reply_display(), "<robin> agreed, friday",
        "quoting a reply quotes the reply, not the thing it was answering",
    );
}

#[test]
fn a_quote_is_one_line_however_long_the_original() {
    let mut c = seated();
    let long = format!("a\nb\n{}", "x".repeat(400));
    c.handle_frame(&event(11, "robin", &long).to_string(), NOW);
    c.set_reply("e_11");
    let fx = c.send_message("ok");
    let body = sent_msg(&fx)["d"]["body"].as_str().unwrap().to_string();
    assert_eq!(body.lines().count(), 2, "the newline is the delimiter, so the quote holds none");
    assert!(body.starts_with("> <robin> a b x"), "{body}");
    assert!(body.contains('…'), "a long original is clipped: {body}");
}

#[test]
fn a_reply_belongs_to_the_channel_it_was_aimed_at() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "the freeze is on for friday").to_string(), NOW);
    c.set_reply("e_11");
    c.set_active("c_2");
    c.set_active("c_1");
    assert_eq!(c.reply_display(), "", "a reply does not follow you into another room");
}

#[test]
fn you_cannot_reply_to_something_that_is_not_there() {
    let mut c = seated();
    c.set_reply("e_nope");
    assert_eq!(c.reply_display(), "");
    let fx = c.send_message("hello");
    assert_eq!(sent_msg(&fx)["d"]["body"].as_str().unwrap(), "hello");
}

#[test]
fn a_quoted_mention_does_not_announce_a_second_time() {
    let mut c = seated();
    // robin answers a line of waseem's that named him. The quote repeats the
    // tag; being answered is not being tagged again.
    let fx = c.handle_frame(
        &event(11, "robin", "> <waseem> @waseem ship it\nlooks fine").to_string(),
        NOW,
    );
    assert!(alerts(&fx).is_empty(), "a quote must not re-announce what it quotes");

    let fx = c.handle_frame(&event(12, "robin", "@waseem take a look").to_string(), NOW);
    assert_eq!(alert_events(&fx), ["mention"], "a real tag still announces");
}

// ---- nearby parity with a served channel ----

/// The MeshSay a nearby line produced, as (body, kind, reply_to).
fn said(fx: &Effects) -> Option<(String, String, String)> {
    fx.iter().find_map(|e| match e {
        Effect::MeshSay { body, msg_kind, reply_to, .. } => {
            Some((body.clone(), msg_kind.clone(), reply_to.clone()))
        }
        _ => None,
    })
}

fn nearby_msg(id: &str, nick: &str, body: &str, extra: Value) -> String {
    let mut d = json!({"id": id, "chan":"flight", "nick": nick, "body": body,
                       "ts":"2027-01-15T12:00:00Z"});
    for (k, v) in extra.as_object().cloned().unwrap_or_default() {
        d[k] = v;
    }
    d.to_string()
}

#[test]
fn a_nearby_reply_carries_its_target_so_the_jump_works_off_a_server() {
    let mut c = seated();
    c.submit("/nearby flight");
    c.mesh_peers(r#"[{"id":"p1","nick":"robin","chans":["flight"]}]"#);
    c.mesh_msg(&nearby_msg("n_1", "robin", "wheels up in ten", json!({})), NOW);

    c.set_reply("n_1");
    assert_eq!(c.reply_display(), "<robin> wheels up in ten");
    let fx = c.send_message("see you at the gate");
    let (body, kind, reply_to) = said(&fx).expect("a nearby line");
    assert_eq!(body, "> <robin> wheels up in ten\nsee you at the gate");
    assert_eq!(kind, "message");
    assert_eq!(reply_to, "n_1", "the id rides the mesh too, or there is nothing to jump to");

    // What a peer receives, echoed back to us the way the transport does.
    c.mesh_msg(&nearby_msg("n_2", "waseem", &body, json!({"reply_to": "n_1"})), NOW);
    let rows: Value = serde_json::from_str(&gliph_core::api::events_json(&c, &c.active)).unwrap();
    let row = rows.as_array().unwrap().iter().find(|r| r["id"] == "n_2").unwrap();
    assert_eq!(row["quote"], "<robin> wheels up in ten");
    assert_eq!(row["text"], "see you at the gate");
    assert_eq!(row["meta"]["reply_to"], "n_1");
}

#[test]
fn me_works_in_a_nearby_channel_where_there_is_no_server_to_ask() {
    let mut c = seated();
    c.disconnected(NOW);
    c.submit("/nearby flight");

    let fx = c.submit("/me boards the plane");
    assert!(sent(&fx).is_empty(), "an action must not go looking for a socket");
    assert_eq!(said(&fx), Some(("boards the plane".into(), "action".into(), String::new())));

    c.mesh_msg(&nearby_msg("n_9", "robin", "waves back", json!({"kind": "action"})), NOW);
    let ev = c.visible_events(&c.active).into_iter().find(|e| e.id == "n_9").unwrap();
    assert_eq!(ev.kind, "action");
}

#[test]
fn a_peer_cannot_name_a_kind_the_stream_reads_as_the_room_talking() {
    let mut c = seated();
    c.submit("/nearby flight");
    c.mesh_msg(&nearby_msg("n_3", "robin", "robin was kicked by god", json!({"kind": "kick"})), NOW);
    let ev = c.visible_events(&c.active).into_iter().find(|e| e.id == "n_3").unwrap();
    assert_eq!(ev.kind, "message", "only the two kinds a person can produce");
}

#[test]
fn a_peer_on_an_older_build_is_still_a_peer() {
    let mut c = seated();
    c.submit("/nearby flight");
    // No kind, no reply_to — exactly what a build from before this shipped sends.
    c.mesh_msg(&nearby_msg("n_4", "robin", "hello", json!({})), NOW);
    let ev = c.visible_events(&c.active).into_iter().find(|e| e.id == "n_4").unwrap();
    assert_eq!(ev.kind, "message");
    assert_eq!(ev.body, "hello");
}

#[test]
fn a_finished_nearby_transfer_leaves_a_line_in_the_room() {
    let mut c = seated();
    c.submit("/nearby flight");
    c.mesh_peers(r#"[{"id":"p1","nick":"robin","chans":["flight"]}]"#);

    // robin offers, we accept, one chunk lands, done.
    let frame = |t: &str, d: Value| {
        json!({"from":"p1","nick":"robin","t": t, "d": d}).to_string()
    };
    c.mesh_frame(
        &frame("file.offer", json!({"id":"x_1","chan":"flight","name":"boarding.pdf",
                                    "size":3,"mime":"application/pdf"})),
        NOW,
    );
    c.accept_transfer("x_1");
    c.mesh_frame(&frame("file.chunk", json!({"id":"x_1","seq":0,"data":"aGkh"})), NOW);
    let fx = c.mesh_frame(&frame("file.done", json!({"id":"x_1"})), NOW);
    assert!(
        fx.iter().any(|e| matches!(e, Effect::SaveFile { .. })),
        "the bytes still reach the host"
    );

    let ev = c
        .visible_events(&c.active)
        .into_iter()
        .find(|e| e.kind == "file")
        .expect("a finished transfer belongs in the scrollback, not only in a window");
    assert_eq!(ev.actor, "robin");
    assert_eq!(ev.body, "boarding.pdf");
    assert_eq!(ev.meta["dir"], "recv");
    assert_eq!(ev.meta["p2p"], true);
    assert_eq!(ev.meta["peer"], "robin");
    assert!(ev.meta["file_id"].is_null(), "there is no server holding this to link to");
}

#[test]
fn the_sender_gets_the_same_line_worded_from_their_side() {
    let mut c = seated();
    c.submit("/nearby flight");
    c.mesh_peers(r#"[{"id":"p1","nick":"robin","chans":["flight"]}]"#);
    c.submit("/send robin");
    c.offer_file("map.png", "image/png", vec![1, 2, 3]);

    let id = c.xfer.list.first().expect("an offer").id.clone();
    let frame = |t: &str, d: Value| json!({"from":"p1","nick":"robin","t": t, "d": d}).to_string();
    c.mesh_frame(&frame("file.accept", json!({ "id": id })), NOW);
    c.mesh_frame(&frame("file.ack", json!({ "id": id, "seq": 0 })), NOW);

    let ev = c
        .visible_events(&c.active)
        .into_iter()
        .find(|e| e.kind == "file")
        .expect("sending is worth a line too");
    assert_eq!(ev.meta["dir"], "send");
    assert_eq!(ev.meta["peer"], "robin");
    assert_eq!(ev.body, "map.png");
}

/// A DM transfer goes over WebRTC, which the host drives, so the line for it
/// comes back through the core rather than out of the mesh. Both ends get one,
/// and it lands at the end of the channel rather than at the top of it.
#[test]
fn a_dm_transfer_leaves_the_same_line_in_the_dm() {
    let mut c = seated();
    in_general(&mut c);
    c.set_active("c_2");
    let tail = c.visible_events("c_2").last().map(|e| e.seq).unwrap_or(0.0);

    c.p2p_file_line("c_2", "x_9", "shot.png", 4096.0, false, "abdullah");

    let ev = c
        .visible_events("c_2")
        .into_iter()
        .find(|e| e.kind == "file")
        .expect("a finished DM transfer belongs in the scrollback");
    assert_eq!(ev.actor, "abdullah");
    assert_eq!(ev.body, "shot.png");
    assert_eq!(ev.meta["p2p"], true);
    assert_eq!(ev.meta["dir"], "recv");
    assert!(ev.seq > tail, "the line goes where the reader is, not above the history");
}

// ---- system messages: the MOTD in #general, the notes in #system ----

/// The MOTD only ever shows in #general, so every test that wants one has to
/// be sitting in it. The DM is here for the test that checks it is not shown
/// in one.
fn in_general(c: &mut Client) {
    c.handle_frame(
        &json!({"t":"state","d":{
            "channels":[{"id":"c_1","name":"general","is_dm":false,"role":"op","seq":10},
                        {"id":"c_2","name":"abdullah","is_dm":true,"role":"member","seq":3}],
            "user":{"id":"u_1","nick":"waseem","role":"admin"}
        }}).to_string(),
        NOW,
    );
    c.set_active("c_1");
}

fn hello_with(motd: Value, news: Value) -> String {
    json!({"t":"hello","d":{
        "user":{"id":"u_1","nick":"waseem","role":"admin"},
        "commands":[],
        "server":{"name":"gliph.test","upload_max":1024,"motd":motd,"news":news}
    }})
    .to_string()
}

const NEWS: fn() -> Value = || {
    json!([
        {"id":"r-1","date":"2026-09-01","title":"Replies","body":"Right-click a line."},
        {"id":"r-2","date":"2026-09-03","title":"Rooms","body":"Type /room."}
    ])
};

#[test]
fn the_release_notes_are_a_channel_and_not_someone_talking_in_general() {
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem"}"#);
    c.handle_frame(&hello_with(Value::Null, NEWS()), NOW);
    in_general(&mut c);
    assert!(c.system_messages().is_empty(), "the release notes talked over #general");

    let names: Vec<&str> = c.channels().iter().map(|ch| ch.name.as_str()).collect();
    assert!(names.contains(&"system"), "the notes have nowhere to be read: {names:?}");

    c.set_active(gliph_core::client::SYS_ID);
    let ids: Vec<&str> = c.system_messages().iter().map(|m| m.id.as_str()).collect();
    assert_eq!(ids, vec!["r-1", "r-2"], "oldest first, so the channel reads forwards");

    // Nothing to dismiss, and nothing that stops being there on the next run:
    // a log you can empty is not a log.
    c.dismiss_system("r-1");
    assert_eq!(c.system_messages().len(), 2, "a note was dismissed out of #system");
}

#[test]
fn a_server_with_no_notes_has_no_system_channel() {
    let mut c = Client::new();
    c.handle_frame(&hello_with(Value::Null, json!([])), NOW);
    in_general(&mut c);
    assert!(
        !c.channels().iter().any(|ch| ch.id == gliph_core::client::SYS_ID),
        "an empty #system still took a line in the rail",
    );
}

#[test]
fn nothing_is_said_to_the_system_channel() {
    let mut c = Client::new();
    c.handle_frame(&hello_with(Value::Null, NEWS()), NOW);
    in_general(&mut c);
    c.set_active(gliph_core::client::SYS_ID);
    let fx = c.send_message("hello?");
    assert!(sent(&fx).is_empty(), "a line went to the server for a channel it has never heard of");
    assert!(!c.notices_for(gliph_core::client::SYS_ID).is_empty(), "the line vanished silently");
}

#[test]
fn the_motd_leads_and_returns_when_it_is_edited() {
    let mut c = Client::new();
    c.restore("", &json!({"cursors":{"c_1":10}}).to_string(), "");
    let motd = |id: &str, text: &str| json!({"id":id,"text":text,"icon":"data:image/png;base64,AA"});
    c.handle_frame(&hello_with(motd("motd-aaa", "welcome aboard"), NEWS()), NOW);
    in_general(&mut c);

    let first = c.system_messages()[0];
    assert_eq!(first.kind, "motd", "the MOTD is what #general carries");
    assert_eq!(first.body, "welcome aboard");
    assert!(first.icon.starts_with("data:image/"), "the icon is inline bytes");

    c.dismiss_system("motd-aaa");
    assert!(c.system_messages().is_empty(), "a dismissed MOTD stayed up");

    // An operator edited the text, so the id changed with it and everybody sees
    // the new one. Pushed live, without waiting for a reconnect.
    c.handle_frame(&json!({"t":"motd","d":{"motd":motd("motd-bbb","we moved on Friday")}}).to_string(), NOW);
    assert_eq!(c.system_messages()[0].body, "we moved on Friday");

    // Clearing it takes the card away.
    c.handle_frame(&json!({"t":"motd","d":{"motd":null}}).to_string(), NOW);
    assert!(c.system_messages().is_empty());
}

#[test]
fn the_motd_stays_out_of_every_channel_but_general() {
    let mut c = Client::new();
    c.restore("", &json!({"cursors":{"c_1":10}}).to_string(), "");
    c.handle_frame(
        &hello_with(json!({"id":"motd-aaa","text":"welcome aboard","icon":""}), NEWS()),
        NOW,
    );
    in_general(&mut c);
    assert_eq!(c.system_messages().len(), 1, "the MOTD belongs in #general");

    c.set_active("c_2");
    assert!(c.system_messages().is_empty(), "a DM was interrupted by an announcement");
}

// ---- room0 in the stream ----

/// A room.event relay, the shape server/room.go rebuilds field by field.
fn relay(kind: &str, uid: &str, name: &str, room: &str, voice: bool) -> String {
    json!({"t":"room.event","d":{"channel":"c_1","type":kind,"user_id":uid,
                                 "name":name,"room":room,"voice":voice}})
    .to_string()
}

fn strips(c: &Client) -> Value {
    serde_json::from_str::<Value>(&gliph_core::api::snapshot_json(c)).unwrap()["rooms"].clone()
}

#[test]
fn the_strip_is_built_from_relays_everyone_receives() {
    let mut c = seated();
    assert!(strips(&c).as_array().unwrap().is_empty(), "an empty room draws no strip");

    c.handle_frame(&relay("join", "u_2", "robin", "office", false), NOW);
    let s = strips(&c);
    assert_eq!(s[0]["channel"], "c_1");
    assert_eq!(s[0]["room"], "office");
    assert_eq!(s[0]["people"][0]["name"], "robin");
    assert_eq!(s[0]["you_inside"], false, "watching from the channel is not being inside");

    assert_eq!(
        s[0]["since"], "2027-01-15T08:00:00Z",
        "a room we only ever saw people walk into is dated from the first arrival"
    );

    // a move keeps the seat current rather than adding a second person
    c.handle_frame(&relay("move", "u_2", "robin", "lounge", true), NOW);
    let s = strips(&c);
    assert_eq!(s[0]["people"].as_array().unwrap().len(), 1);
    assert_eq!(s[0]["room"], "lounge");
    assert_eq!(s[0]["voice_count"], 1);

    // your own arrival is the one that names the strip's room
    c.handle_frame(&relay("join", "u_1", "waseem", "office", false), NOW);
    let s = strips(&c);
    assert_eq!(s[0]["you_inside"], true);
    assert_eq!(s[0]["room"], "office", "the strip names the room you are standing in");

    // last one out and the strip goes with them
    c.handle_frame(&relay("leave", "u_1", "waseem", "", false), NOW);
    c.handle_frame(&relay("leave", "u_2", "robin", "", false), NOW);
    assert!(strips(&c).as_array().unwrap().is_empty(), "the strip outlived the room");
}

#[test]
fn the_servers_snapshot_replaces_what_the_relays_built() {
    let mut c = seated();
    c.handle_frame(&relay("join", "u_2", "robin", "office", false), NOW);

    // a reconnect: the state frame is the truth, including the start time the
    // relays could never carry
    c.handle_frame(
        &json!({"t":"state","d":{
            "channels":[{"id":"c_1","name":"general","modes":"nt","role":"op","seq":10,
                         "members":[],
                         "room":{"since":"2027-01-15T11:48:00Z",
                                 "users":[{"user_id":"u_3","name":"dana","room":"dunes","voice":true}]}}],
            "user":{"id":"u_1","nick":"waseem","role":"admin"}
        }}).to_string(),
        NOW,
    );
    let s = strips(&c);
    assert_eq!(s[0]["people"].as_array().unwrap().len(), 1);
    assert_eq!(s[0]["people"][0]["name"], "dana");
    assert_eq!(s[0]["since"], "2027-01-15T11:48:00Z");
}

#[test]
fn a_quiet_line_leaves_the_stream_and_stays_in_the_room() {
    let mut c = seated();
    let loud = json!({"t":"event","d":{"seq":11,"id":"e_11","channel":"c_1","kind":"message",
        "actor":"robin","ts":"2027-01-15T12:00:00Z","body":"east wall now",
        "meta":{"room":"office"}}});
    let quiet = json!({"t":"event","d":{"seq":12,"id":"e_12","channel":"c_1","kind":"message",
        "actor":"robin","ts":"2027-01-15T12:00:01Z","body":"between us",
        "meta":{"room":"office","quiet":true}}});
    c.handle_frame(&loud.to_string(), NOW);
    c.handle_frame(&quiet.to_string(), NOW);

    let stream: Vec<Value> = serde_json::from_str(&gliph_core::api::events_json(&c, "c_1")).unwrap();
    let ids: Vec<&str> = stream.iter().map(|e| e["id"].as_str().unwrap()).collect();
    assert!(ids.contains(&"e_11"), "the marked line belongs in the stream");
    assert!(!ids.contains(&"e_12"), "a quiet line was rendered in the channel");

    let log: Vec<Value> = serde_json::from_str(&gliph_core::api::room_log_json(&c, "c_1", 50)).unwrap();
    let ids: Vec<&str> = log.iter().map(|e| e["id"].as_str().unwrap()).collect();
    assert!(ids.contains(&"e_12"), "the room cannot see what was said in it");
}

#[test]
fn a_room_line_carries_its_room_and_quiet_carries_itself() {
    let mut c = seated();
    let fx = c.send_room_message("east wall now", "office");
    let f = &sent_msg(&fx);
    assert_eq!(f["t"], "message.send");
    assert_eq!(f["d"]["room"], "office");
    assert_eq!(f["d"]["quiet"], false);

    // the server asks, the client decides, and nobody else hears about it
    c.handle_frame(&json!({"t":"room.quiet","d":{"channel":"c_1"}}).to_string(), NOW);
    let fx = c.send_room_message("between us", "office");
    assert_eq!(sent_msg(&fx)["d"]["quiet"], true);
    assert!(sent_types(&fx).iter().all(|t| t == "message.send"), "quiet told the server");

    // and it is off the moment it is asked again
    c.handle_frame(&json!({"t":"room.quiet","d":{"channel":"c_1"}}).to_string(), NOW);
    assert_eq!(sent(&c.send_room_message("out loud", "office"))[0]["d"]["quiet"], false);

    // a line typed in the channel window is never marked
    assert_eq!(sent(&c.send_message("plain"))[0]["d"]["room"], "");
}

#[test]
fn an_invite_pings_like_a_mention() {
    let mut c = seated();
    let fx = c.handle_frame(
        &json!({"t":"event","d":{"seq":13,"id":"e_13","channel":"c_1","kind":"room_invite",
            "actor":"robin","ts":"2027-01-15T12:00:00Z","body":"office"}})
        .to_string(),
        NOW,
    );
    assert_eq!(alert_events(&fx), vec!["room_invite"]);
    assert!(alerts(&fx)[0].1.contains("office"), "the alert does not say where");
}

#[test]
fn a_pong_repaints_nothing() {
    // The heartbeat answers every five seconds forever. If it ever counts as a
    // change, every frontend rebuilds its DOM on an idle connection and the
    // stream jumps while nobody is doing anything.
    let mut c = seated();
    let fx = c.handle_frame(&json!({"t":"pong"}).to_string(), NOW);
    assert!(
        !fx.iter().any(|e| matches!(e, Effect::Changed)),
        "an idle heartbeat is not a reason to repaint"
    );
}

// ---- read marks across devices ----
//
// The rule everywhere: forward only, highest wins. That is what lets two
// devices reconcile without either having to know which of them read first.

/// The reported bug. You read on the phone, open the browser days later, and
/// the browser has to believe the server over its own stale cache.
#[test]
fn a_read_mark_from_the_server_clears_what_another_device_read() {
    let mut c = seated();
    for seq in 11..=14 {
        c.handle_frame(&event(seq, "robin", "while you were on your phone").to_string(), NOW);
    }
    assert_eq!(c.unread("c_1"), 4, "four arrived and this device has read none of them");

    // The phone read them; the server says so on the next state frame.
    c.handle_frame(
        &json!({"t":"state","d":{"channels":[
            {"id":"c_1","name":"general","seq":14,"read":14,"role":"op"}
        ]}})
        .to_string(),
        NOW,
    );
    assert_eq!(c.unread("c_1"), 0, "the server's mark wins over this device's stale one");
}

/// The live path: no reconnect and no state frame, just the other device
/// saying so while you are looking at both.
#[test]
fn a_read_frame_from_another_device_clears_the_badge_live() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "one").to_string(), NOW);
    assert_eq!(c.unread("c_1"), 1);

    c.handle_frame(&json!({"t":"read","d":{"marks":{"c_1":11}}}).to_string(), NOW);
    assert_eq!(c.unread("c_1"), 0, "reading on the desktop clears the phone");
}

/// The direction that must never happen. A device coming back from offline
/// holds an old mark, and applying it would reopen a badge on messages that
/// have already been read somewhere else.
#[test]
fn a_stale_mark_can_never_un_read_anything() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "one").to_string(), NOW);
    c.mark_active_read();
    assert_eq!(c.unread("c_1"), 0);

    c.handle_frame(&json!({"t":"read","d":{"marks":{"c_1":3}}}).to_string(), NOW);
    assert_eq!(c.unread("c_1"), 0, "an older mark is ignored, not applied");

    c.handle_frame(
        &json!({"t":"state","d":{"channels":[
            {"id":"c_1","name":"general","seq":11,"read":2,"role":"op"}
        ]}})
        .to_string(),
        NOW,
    );
    assert_eq!(c.unread("c_1"), 0, "and a state frame carrying an older mark is too");
}

/// Reading has to leave the device, or no other device ever finds out.
#[test]
fn reading_tells_the_server_where_you_got_to() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "one").to_string(), NOW);
    let fx = c.mark_active_read();

    let frames = sent(&fx);
    let marks: Vec<&Value> = frames.iter().filter(|f| f["t"] == "read").collect();
    assert_eq!(marks.len(), 1, "one read frame for the whole batch, not one per message");
    assert_eq!(marks[0]["d"]["marks"]["c_1"], 11, "and it names where we got to");
}

/// Reading the same place twice is not news, and a frame per repaint would be.
#[test]
fn reading_the_same_place_twice_says_nothing() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "one").to_string(), NOW);
    c.mark_active_read();

    let fx = c.mark_active_read();
    assert!(
        !sent(&fx).iter().any(|f| f["t"] == "read"),
        "the mark did not move, so there is nothing to tell the server",
    );
}

/// Reading with no connection still has to reach the other devices, and the
/// state frame on reconnect is the moment it can.
#[test]
fn reading_offline_goes_up_when_the_server_comes_back() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "one").to_string(), NOW);
    c.mark_active_read();

    // Reconnect: the server is still back at 4 and has to be told.
    let fx = c.handle_frame(
        &json!({"t":"state","d":{"channels":[
            {"id":"c_1","name":"general","seq":11,"read":4,"role":"op"}
        ]}})
        .to_string(),
        NOW,
    );
    let frames = sent(&fx);
    let marks: Vec<&Value> = frames.iter().filter(|f| f["t"] == "read").collect();
    assert_eq!(marks.len(), 1, "the mark we hold and the server does not goes up");
    assert_eq!(marks[0]["d"]["marks"]["c_1"], 11);
    assert_eq!(c.unread("c_1"), 0, "and the older mark from the server is not applied");
}

/// A state frame arrives every time anyone walks in or out of a channel.
/// Answering each one with marks the server already holds would be a frame per
/// person per room, for nothing.
#[test]
fn a_state_frame_the_server_is_current_on_says_nothing() {
    let mut c = seated();
    c.handle_frame(&event(11, "robin", "one").to_string(), NOW);
    c.mark_active_read();

    let fx = c.handle_frame(
        &json!({"t":"state","d":{"channels":[
            {"id":"c_1","name":"general","seq":11,"read":11,"role":"op"}
        ]}})
        .to_string(),
        NOW,
    );
    assert!(
        !sent(&fx).iter().any(|f| f["t"] == "read"),
        "the server is already current; there is nothing to say",
    );
}

/// The anchor exists so a new device does not open to a wall of unread. It is a
/// courtesy to that device and not a claim that anybody read anything, so it
/// must not travel and clear badges elsewhere.
#[test]
fn the_first_sight_anchor_stays_on_the_device() {
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem","role":"admin"}"#);
    c.connected("test", NOW);
    let fx = c.handle_frame(
        &json!({"t":"state","d":{"channels":[
            {"id":"c_9","name":"newroom","seq":300,"read":0,"role":"member"}
        ]}})
        .to_string(),
        NOW,
    );
    assert_eq!(c.unread("c_9"), 0, "a room seen for the first time is not 300 unread");
    assert!(
        !sent(&fx).iter().any(|f| f["t"] == "read"),
        "but the server is not told we read 300 messages we never saw",
    );
}
