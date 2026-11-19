//! Export, themes and user scripts: the client-side extensibility rules,
//! asserted with no host at all.

use gliph_core::effect::Effect;
use gliph_core::{Client, Effects};
use serde_json::{json, Value};

const NOW: i64 = 1_800_000_000_000;

fn seated() -> Client {
    seated_from(Client::new())
}

fn seated_from(mut c: Client) -> Client {
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem","role":"admin"}"#);
    c.connected("test", NOW);
    c.handle_frame(
        &json!({"t":"hello","d":{"user":{"id":"u_1","nick":"waseem","role":"admin"},
            "commands":[], "server":{"name":"gliph.test","upload_max":1024}}})
        .to_string(),
        NOW,
    );
    c.handle_frame(
        &json!({"t":"state","d":{
            "channels":[{"id":"c_1","name":"general","topic":"","modes":"nt","is_dm":false,
                         "role":"op","seq":0,
                         "members":[{"user_id":"u_1","nick":"waseem","role":"op","kind":"user"},
                                    {"user_id":"u_2","nick":"robin","role":"member","kind":"user"}]}],
            "user":{"id":"u_1","nick":"waseem","role":"admin"},
            "prefs":{"aliases":{},"highlights":["ship"]}
        }})
        .to_string(),
        NOW,
    );
    c.set_active("c_1");
    c.go_live();
    c
}

fn ev(seq: i64, kind: &str, actor: &str, body: &str) -> String {
    json!({"t":"event","d":{"seq":seq,"id":format!("e_{seq}"),"channel":"c_1","kind":kind,
        "actor":actor,"ts":"2027-01-15T12:00:00Z","body":body}})
    .to_string()
}

fn files(fx: &Effects) -> Vec<(String, String)> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::SaveFile { name, bytes, .. } => Some((name.clone(), String::from_utf8(bytes.clone()).unwrap())),
            _ => None,
        })
        .collect()
}

fn sent_bodies(fx: &Effects) -> Vec<String> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::Send { json } => serde_json::from_str::<Value>(json).ok(),
            _ => None,
        })
        .filter(|f| f["t"] == "message.send")
        .map(|f| f["d"]["body"].as_str().unwrap_or_default().to_string())
        .collect()
}

// ---- export ----

#[test]
fn export_writes_the_whole_local_history_as_text_and_jsonl() {
    let mut c = seated();
    c.handle_frame(&ev(1, "join", "robin", "joined the server"), NOW);
    c.handle_frame(&ev(2, "message", "robin", "first line\nsecond line"), NOW);
    c.handle_frame(&ev(3, "action", "waseem", "waves"), NOW);
    // ciphertext this device has no key for: exported, and honest about it
    c.handle_frame(
        &json!({"t":"event","d":{"seq":4,"id":"e_4","channel":"c_1","kind":"message","actor":"robin",
            "ts":"2027-01-15T12:01:00Z","body":"QUJD","enc":true,"epoch":1}})
        .to_string(),
        NOW,
    );

    let fx = c.submit("/export");
    let out = files(&fx);
    assert_eq!(out.len(), 2, "both formats when none is named");
    let (txt_name, txt) = &out[0];
    assert!(txt_name.starts_with("gliph-general-") && txt_name.ends_with(".txt"), "{txt_name}");
    assert!(txt.contains("[2027-01-15 12:00] <robin> first line\n    second line"), "{txt}");
    assert!(txt.contains("* waseem waves"));
    assert!(txt.contains("-!- join robin: joined the server"));
    assert!(txt.contains("<robin> [cannot decrypt"));

    let (jsonl_name, jsonl) = &out[1];
    assert!(jsonl_name.ends_with(".jsonl"));
    let rows: Vec<Value> = jsonl.lines().map(|l| serde_json::from_str(l).expect("every line is JSON")).collect();
    assert_eq!(rows.len(), 4);
    assert_eq!(rows[1]["text"], "first line\nsecond line");
    assert_eq!(rows[3]["text"], Value::Null, "unreadable is null, not empty");

    assert_eq!(files(&c.submit("/export jsonl")).len(), 1);
    assert!(files(&c.submit("/export pdf")).is_empty());
}

// ---- themes ----

fn theme_fx(fx: &Effects) -> Option<(String, String, Value)> {
    fx.iter().find_map(|e| match e {
        Effect::Theme { op, name, theme } => {
            Some((op.clone(), name.clone(), serde_json::from_str(theme).unwrap_or(Value::Null)))
        }
        _ => None,
    })
}

fn last_notice(c: &Client) -> String {
    let snap: Value = serde_json::from_str(&gliph_core::api::snapshot_json(c)).unwrap();
    snap["notices"].as_array().and_then(|n| n.last()).map(|n| n["text"].as_str().unwrap().to_string()).unwrap_or_default()
}

#[test]
fn a_theme_file_installs_and_a_broken_one_says_which_line() {
    let mut c = seated();
    let file = "/* harbour, for late nights\n   name: Harbour\n   base: midnight */\n:root {\n  --bg: #0B1D2A;\n  --accent: #f75;\n}\n";
    let (op, name, t) = theme_fx(&c.submit(&format!("/theme add\n{file}"))).expect("a Theme effect");
    assert_eq!((op.as_str(), name.as_str()), ("add", "harbour"));
    assert_eq!(t["base"], "midnight");
    assert_eq!(t["tokens"]["bg"], "#0b1d2a");
    assert_eq!(t["tokens"]["accent"], "#ff7755", "short hex is expanded");

    for (bad, says) in [
        ("/* name: x */\n--bgg: #000000;", "line 2: --bgg is not a theme token"),
        ("/* name: x */\n\n--bg: blue;", "line 3: --bg needs a colour"),
        ("--bg: #000;", "needs a name"),
        ("/* name: x */\nbackground: #000;", "line 2: expected --token"),
        ("/* name: midnight */\n--bg: #000;", "built-in"),
        ("/* name: x */", "no tokens"),
    ] {
        let fx = c.submit(&format!("/theme add\n{bad}"));
        assert!(theme_fx(&fx).is_none(), "{bad:?} installed");
        let n = last_notice(&c);
        assert!(n.contains(says), "{bad:?} said {n:?}");
    }

    // no body: the core asks the host for an editor that submits back here
    let fx = c.submit("/theme add");
    assert!(fx.iter().any(|e| matches!(e, Effect::Compose { submit, .. } if submit == "/theme add")));
    assert_eq!(theme_fx(&c.submit("/theme harbour")).unwrap().0, "use");
}

// ---- user scripts ----

fn notices(c: &Client) -> Vec<String> {
    let snap: Value = serde_json::from_str(&gliph_core::api::snapshot_json(c)).unwrap();
    snap["notices"].as_array().unwrap().iter().map(|n| n["text"].as_str().unwrap().to_string()).collect()
}

fn join(seq: i64, nick: &str) -> String {
    ev(seq, "join", nick, "joined")
}

#[test]
fn scripts_fire_on_join_highlight_and_pattern() {
    let mut c = seated();
    let src = "let pattern = \"*deploy*\";\n\
        fn on_join(nick, chan) { say(\"welcome \" + nick + \" to \" + chan) }\n\
        fn on_message(nick, text, chan) { notice(nick + \" deploys\") }\n\
        fn on_highlight(nick, text, chan) { notice(\"hl from \" + nick) }";
    c.submit(&format!("/script add greeter\n{src}"));
    assert!(notices(&c).last().unwrap().contains("installed"), "{:?}", notices(&c));

    let fx = c.handle_frame(&join(1, "robin"), NOW);
    assert_eq!(sent_bodies(&fx), vec!["welcome robin to #general"]);

    c.handle_frame(&ev(2, "message", "robin", "lunch?"), NOW);
    assert!(!notices(&c).iter().any(|n| n.contains("deploys")), "the pattern did not match");
    c.handle_frame(&ev(3, "message", "robin", "Deploy is green"), NOW);
    assert!(notices(&c).iter().any(|n| n == "[greeter] robin deploys"), "{:?}", notices(&c));
    c.handle_frame(&ev(4, "message", "robin", "we ship today"), NOW);
    assert!(notices(&c).iter().any(|n| n == "[greeter] hl from robin"));

    // my own lines never fire my scripts
    let fx = c.handle_frame(&join(5, "waseem"), NOW);
    assert!(sent_bodies(&fx).is_empty());
}

#[test]
fn a_runaway_script_is_stopped_and_reported() {
    let mut c = seated();
    c.submit("/script add spin\nfn on_join(nick, chan) { say(\"half\"); loop { } }");
    let fx = c.handle_frame(&join(1, "robin"), NOW);
    assert!(sent_bodies(&fx).is_empty(), "a killed run sends nothing");
    assert!(notices(&c).iter().any(|n| n.contains("script spin was stopped")), "{:?}", notices(&c));
    // it stays off, and the stop survives a relaunch
    assert!(sent_bodies(&c.handle_frame(&join(2, "ana"), NOW)).is_empty());
    c.submit("/script list");
    assert!(notices(&c).iter().any(|n| n.starts_with("spin · off")), "{:?}", notices(&c));
}

#[test]
fn scripts_have_no_way_out_of_the_sandbox() {
    let mut c = seated();
    for (src, why) in [
        ("import \"fs\" as fs; fn on_join(n, c) {}", "no modules"),
        ("fn on_join(n, c) { eval(\"1\") }", "no eval"),
        ("let x = 1;", "needs a hook"),
    ] {
        c.submit(&format!("/script add bad\n{src}"));
        assert!(notices(&c).last().unwrap().contains("not installed"), "{why}: {:?}", notices(&c));
    }
    // a script that tries anything else simply has no function to call
    c.submit("/script add probe\nfn on_join(n, c) { open_file(\"/etc/passwd\") }");
    c.handle_frame(&join(1, "robin"), NOW);
    assert!(notices(&c).last().unwrap().contains("open_file"), "{:?}", notices(&c));
}

#[test]
fn scripts_are_shared_as_text_and_survive_a_relaunch() {
    let mut c = seated();
    let src = "fn on_join(nick, chan) { say(\"hi \" + nick) }";
    let fx = c.submit(&format!("/script add hello\n{src}"));
    let cache = fx
        .iter()
        .find_map(|e| match e {
            Effect::Persist { key, value, .. } if key.starts_with("gliph.cache") => Some(value.clone()),
            _ => None,
        })
        .expect("the install is persisted");

    let out = files(&c.submit("/script export hello"));
    assert_eq!(out, vec![("hello.rhai".to_string(), src.to_string())]);

    // no body opens the editor on the current source
    let fx = c.submit("/script edit hello");
    assert!(fx.iter().any(|e| matches!(e, Effect::Compose { text, submit, .. } if text == src && submit == "/script add hello")));

    let mut again = Client::new();
    again.restore("", &cache, "");
    let mut c2 = seated_from(again);
    assert_eq!(sent_bodies(&c2.handle_frame(&join(1, "robin"), NOW)), vec!["hi robin"]);
}

#[test]
fn wildcards_match_like_mirc() {
    use gliph_core::client::script::wild;
    assert!(wild("*deploy*", "the DEPLOY is done"));
    assert!(wild("h?llo", "hello"));
    assert!(!wild("h?llo", "hllo"));
    assert!(wild("*", ""));
    assert!(!wild("deploy", "deploy now"));
}
