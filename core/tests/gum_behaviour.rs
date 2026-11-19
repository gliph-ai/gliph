//! The blob surface's rules, asserted without a DOM.
//!
//! These used to live in main.ts as keydown handlers and were reachable only by
//! driving a browser, which is why none of them were checked. They are the
//! framework's promise to every provider: a blob declares actions and never
//! defines a key, so what enter does has to mean the same thing everywhere.

use gliph_core::effect::Effect;
use gliph_core::{Client, Effects};
use serde_json::{json, Value};

const NOW: i64 = 1_800_000_000_000;

fn sent(fx: &Effects) -> Vec<Value> {
    fx.iter()
        .filter_map(|e| match e {
            Effect::Send { json } => serde_json::from_str(json).ok(),
            _ => None,
        })
        .collect()
}

fn asked(fx: &Effects) -> Option<(&'static str, String)> {
    fx.iter().find_map(|e| match e {
        Effect::AskConfirm { message } => Some(("confirm", message.clone())),
        Effect::AskPrompt { message } => Some(("prompt", message.clone())),
        _ => None,
    })
}

fn surface(c: &Client) -> Value {
    serde_json::from_str(&gliph_core::api::surface_json(c)).unwrap()
}

/// Signed in, in a channel that has one app, with that app open and rendered.
fn open_todos() -> Client {
    let mut c = Client::new();
    c.set_session("https://gliph.test", "tok", r#"{"id":"u_1","nick":"waseem","role":"admin"}"#);
    c.connected("test", NOW);
    c.handle_frame(
        &json!({"t":"state","d":{"channels":[{
            "id":"c_1","name":"general","modes":"nt","role":"op","seq":0,"members":[],
            "blobs":[{"id":"todos","title":"todos","mode":"shared","source":"builtin"},
                     {"id":"devlog","title":"devlog","mode":"shared","source":"deploybot"}]
        }]}}).to_string(),
        NOW,
    );
    c.set_active("c_1");
    c.open_blob("todos");
    c.handle_frame(
        &json!({"t":"blob.render","d":{"channel":"c_1","blob":"todos","doc":{
            "title":"todos",
            "blocks":[
                {"t":"text","body":"three left","style":"info"},
                {"t":"list","id":"open","items":[
                    {"id":"t1","label":"ship the core","state":"open",
                     "actions":[{"id":"done","label":"mark done"},
                                {"id":"drop","label":"delete","confirm":"delete this todo?"}]},
                    {"id":"t2","label":"write the tests","state":"done",
                     "actions":[{"id":"reopen","label":"reopen"}]},
                    {"id":"t3","label":"no actions here"}
                ]},
                {"t":"input","id":"add","action":"add","placeholder":"a new todo"},
                {"t":"buttons","actions":[{"id":"clear","label":"clear done",
                                           "prompt":"type CLEAR to confirm"}]}
            ]}}}).to_string(),
        NOW,
    );
    c
}

// ---- the tree ----

#[test]
fn the_surface_resolves_selection_numbering_and_submenus() {
    let c = open_todos();
    let s = surface(&c);
    assert_eq!(s["blob"], "todos");
    assert_eq!(s["title"], "todos");
    assert_eq!(s["loaded"], true);

    let nodes = s["nodes"].as_array().unwrap();
    assert_eq!(nodes[0]["kind"], "Text");
    assert_eq!(nodes[0]["style"], "info");

    // Three rows, numbered 1..3, the first selected, and only the ones with
    // actions marked as having a submenu.
    let rows: Vec<&Value> = nodes.iter().filter(|n| n["kind"] == "Row").collect();
    assert_eq!(rows.len(), 3);
    assert_eq!(rows[0]["number"], 1);
    assert_eq!(rows[0]["selected"], true);
    assert_eq!(rows[0]["has_actions"], true);
    assert_eq!(rows[2]["has_actions"], false, "an item with no actions offers no menu");
    assert_eq!(rows[1]["state"], "done");

    // The switcher's contents come with the surface, so no frontend goes
    // looking for them.
    let blobs = s["blobs"].as_array().unwrap();
    assert_eq!(blobs.len(), 2);
    assert_eq!(blobs[1]["source"], "deploybot");
}

#[test]
fn the_focus_order_is_the_visual_order_across_blocks() {
    let mut c = open_todos();
    // Three list items, then the input, then the button: index 3 is the input.
    c.gum_jump(4);
    let s = surface(&c);
    let input = s["nodes"].as_array().unwrap().iter().find(|n| n["kind"] == "Input").unwrap().clone();
    assert_eq!(input["index"], 3);
    assert_eq!(input["selected"], true);
}

// ---- moving ----

#[test]
fn the_selection_wraps_in_both_directions() {
    let mut c = open_todos();
    let selected = |c: &Client| -> i64 {
        let s = surface(c);
        s["nodes"]
            .as_array()
            .unwrap()
            .iter()
            .flat_map(|n| {
                if n["kind"] == "Buttons" {
                    n["buttons"].as_array().unwrap().clone()
                } else {
                    vec![n.clone()]
                }
            })
            .find(|n| n["selected"] == true)
            .map(|n| n["index"].as_i64().unwrap())
            .unwrap()
    };
    assert_eq!(selected(&c), 0);
    c.gum_move(-1);
    assert_eq!(selected(&c), 4, "up from the top lands on the last focusable");
    c.gum_move(1);
    assert_eq!(selected(&c), 0, "and down from the last comes back round");
}

#[test]
fn a_number_out_of_range_is_ignored_not_clamped() {
    let mut c = open_todos();
    c.gum_jump(9); // only five focusables
    let s = surface(&c);
    let rows: Vec<&Value> = s["nodes"].as_array().unwrap().iter().filter(|n| n["kind"] == "Row").collect();
    assert_eq!(rows[0]["selected"], true, "jumping to a row that is not there means staying put");
}

// ---- activating ----

#[test]
fn one_action_fires_and_several_open_a_menu() {
    let mut c = open_todos();

    // t2 has exactly one action: enter fires it, no menu.
    c.gum_jump(2);
    let fx = c.gum_activate();
    let f = sent(&fx);
    assert_eq!(f[0]["t"], "blob.event");
    assert_eq!(f[0]["d"]["action"], "reopen");
    assert_eq!(f[0]["d"]["item"], "t2");
    assert_eq!(f[0]["d"]["blob"], "todos");
    assert!(surface(&c)["menu"].is_null());

    // t1 has two: enter opens the menu, and the menu takes the selection.
    c.gum_jump(1);
    let fx = c.gum_activate();
    assert!(sent(&fx).is_empty(), "opening a menu is not an action");
    let s = surface(&c);
    let menu = s["menu"].as_array().unwrap();
    assert_eq!(menu.len(), 2);
    assert_eq!(menu[0]["selected"], true);
    let rows: Vec<&Value> = s["nodes"].as_array().unwrap().iter().filter(|n| n["kind"] == "Row").collect();
    assert!(
        rows.iter().all(|r| r["selected"] == false),
        "while a menu is open it owns the selection, not the list behind it"
    );
}

#[test]
fn an_open_menu_owns_the_keyboard() {
    let mut c = open_todos();
    c.gum_activate(); // opens t1's menu
    c.gum_move(1);
    assert_eq!(surface(&c)["menu"][1]["selected"], true);

    // And enter on the second entry fires THAT action — which needs a confirm.
    let fx = c.gum_activate();
    assert_eq!(asked(&fx), Some(("confirm", "delete this todo?".into())));
    assert!(sent(&fx).is_empty(), "nothing fires until the question is answered");
}

#[test]
fn an_item_with_no_actions_does_nothing() {
    let mut c = open_todos();
    c.gum_jump(3);
    let fx = c.gum_activate();
    assert!(sent(&fx).is_empty());
    assert!(surface(&c)["menu"].is_null());
}

// ---- confirm and prompt ----

#[test]
fn cancelling_a_confirm_never_fires_the_action() {
    let mut c = open_todos();
    c.gum_activate();
    c.gum_move(1);
    c.gum_activate(); // asks
    let fx = c.gum_answer(false, "");
    assert!(sent(&fx).is_empty(), "a cancelled delete is not a delete");
}

#[test]
fn confirming_fires_it() {
    let mut c = open_todos();
    c.gum_activate();
    c.gum_move(1);
    c.gum_activate();
    let fx = c.gum_answer(true, "");
    let f = sent(&fx);
    assert_eq!(f[0]["d"]["action"], "drop");
    assert_eq!(f[0]["d"]["item"], "t1");
}

#[test]
fn a_prompt_carries_its_answer_through() {
    let mut c = open_todos();
    c.gum_jump(5); // the button
    let fx = c.gum_activate();
    assert_eq!(asked(&fx), Some(("prompt", "type CLEAR to confirm".into())));
    let fx = c.gum_answer(true, "CLEAR");
    let f = sent(&fx);
    assert_eq!(f[0]["d"]["action"], "clear");
    assert_eq!(f[0]["d"]["value"], "CLEAR");
    assert_eq!(f[0]["d"]["item"], "", "a button belongs to no item");
}

// ---- inputs ----

#[test]
fn an_input_submits_its_own_draft_and_then_empties() {
    let mut c = open_todos();
    c.gum_set_draft("add", "  buy milk  ");
    c.gum_jump(4);
    let fx = c.gum_activate();
    let f = sent(&fx);
    assert_eq!(f[0]["d"]["action"], "add");
    assert_eq!(f[0]["d"]["value"], "buy milk", "trimmed");

    let s = surface(&c);
    let input = s["nodes"].as_array().unwrap().iter().find(|n| n["kind"] == "Input").unwrap().clone();
    assert_eq!(input["value"], "", "a submitted draft is spent");
}

#[test]
fn an_empty_input_submits_nothing() {
    let mut c = open_todos();
    c.gum_jump(4);
    c.gum_set_draft("add", "   ");
    let fx = c.gum_activate();
    assert!(sent(&fx).is_empty());
}

#[test]
fn the_selection_stays_on_the_input_after_a_submit() {
    let mut c = open_todos();
    c.gum_set_draft("add", "buy milk");
    c.gum_jump(4); // the input
    c.gum_activate();

    // The provider answers with a longer list, so the input has moved down.
    c.handle_frame(
        &json!({"t":"blob.render","d":{"channel":"c_1","blob":"todos","doc":{
            "title":"todos","blocks":[
                {"t":"list","id":"open","items":[
                    {"id":"t1","label":"ship the core","actions":[{"id":"done","label":"done"}]},
                    {"id":"t2","label":"write the tests","actions":[{"id":"done","label":"done"}]},
                    {"id":"t3","label":"no actions here"},
                    {"id":"t4","label":"buy milk","actions":[{"id":"done","label":"done"}]}
                ]},
                {"t":"input","id":"add","action":"add","placeholder":"a new todo"}
            ]}}}).to_string(),
        NOW,
    );

    let s = surface(&c);
    let input = s["nodes"].as_array().unwrap().iter().find(|n| n["kind"] == "Input").unwrap().clone();
    assert_eq!(input["index"], 4, "the input moved down as the list grew");
    assert_eq!(
        input["selected"], true,
        "you typed in a box and pressed enter, so you are still in that box — \
         otherwise the next thing you type opens the new row's menu"
    );
}

// ---- the switcher ----

#[test]
fn the_switcher_jump_counts_chat_as_one() {
    let mut c = open_todos();
    // The channel has two apps, so the switcher reads: chat · todos · devlog.
    c.blob_switch(3);
    assert_eq!(surface(&c)["blob"], "devlog");
    c.blob_switch(2);
    assert_eq!(surface(&c)["blob"], "todos");
    c.blob_switch(1);
    assert_eq!(surface(&c)["blob"], "", "one is always chat");
}

#[test]
fn a_switcher_jump_past_the_end_does_nothing() {
    let mut c = open_todos();
    c.blob_switch(9);
    assert_eq!(surface(&c)["blob"], "todos", "staying put beats guessing");
}

#[test]
fn a_command_can_ask_this_client_to_open_a_blob() {
    let mut c = open_todos();
    c.open_blob(""); // back to chat
    let fx = c.handle_frame(
        &json!({"t":"blob.focus","d":{"channel":"c_1","blob":"devlog"}}).to_string(),
        NOW,
    );
    assert_eq!(surface(&c)["blob"], "devlog");
    // And it subscribes, or the surface would sit on "loading…" forever.
    assert_eq!(sent(&fx)[0]["t"], "blob.open");
}

#[test]
fn a_focus_for_an_app_this_channel_does_not_have_is_ignored() {
    let mut c = open_todos();
    c.handle_frame(
        &json!({"t":"blob.focus","d":{"channel":"c_1","blob":"servers"}}).to_string(),
        NOW,
    );
    assert_eq!(surface(&c)["blob"], "todos", "the switcher is the whole menu");
}

// ---- escape ----

#[test]
fn escape_closes_the_menu_then_clears_the_draft_then_leaves() {
    let mut c = open_todos();
    c.gum_set_draft("add", "half typed");
    c.gum_activate(); // menu open on t1

    c.gum_escape();
    assert!(surface(&c)["menu"].is_null(), "first escape closes the menu");
    assert_eq!(surface(&c)["blob"], "todos", "and does not leave");

    c.gum_escape();
    let s = surface(&c);
    let input = s["nodes"].as_array().unwrap().iter().find(|n| n["kind"] == "Input").unwrap().clone();
    assert_eq!(input["value"], "", "second escape clears the draft");
    assert_eq!(s["blob"], "todos", "and still does not leave");

    c.gum_escape();
    assert_eq!(surface(&c)["blob"], "", "only the third leaves");
}

// ---- opening and leaving ----

#[test]
fn opening_a_blob_asks_the_provider_to_render_it() {
    let mut c = open_todos();
    let fx = c.open_blob("devlog");
    let f = sent(&fx);
    assert_eq!(f[0]["t"], "blob.open");
    assert_eq!(f[0]["d"]["blob"], "devlog");
    // Nothing has rendered yet, and the surface says so rather than showing the
    // last app's document under this app's name.
    let s = surface(&c);
    assert_eq!(s["loaded"], false);
    assert!(s["nodes"].as_array().unwrap().is_empty());
}

#[test]
fn changing_channel_leaves_the_blob_behind() {
    let mut c = open_todos();
    c.handle_frame(
        &json!({"t":"state","d":{"channels":[
            {"id":"c_1","name":"general","modes":"nt","role":"op","seq":0,"members":[],"blobs":[]},
            {"id":"c_2","name":"other","modes":"nt","role":"op","seq":0,"members":[],"blobs":[]}]}})
        .to_string(),
        NOW,
    );
    c.set_active("c_2");
    assert_eq!(surface(&c)["blob"], "", "an app belongs to the room it is attached to");
}

#[test]
fn a_fresh_document_drops_a_menu_standing_over_the_old_one() {
    let mut c = open_todos();
    c.gum_activate(); // menu open on t1
    assert!(!surface(&c)["menu"].is_null());

    c.handle_frame(
        &json!({"t":"blob.render","d":{"channel":"c_1","blob":"todos","doc":{
            "title":"todos","blocks":[{"t":"text","body":"all done"}]}}}).to_string(),
        NOW,
    );
    assert!(
        surface(&c)["menu"].is_null(),
        "the item the menu belonged to may not exist any more"
    );
}

#[test]
fn a_provider_can_pop_its_own_menu() {
    let mut c = open_todos();
    c.handle_frame(
        &json!({"t":"blob.menu","d":{"item":"t1","actions":[
            {"id":"assign","label":"assign to…"},{"id":"snooze","label":"snooze"}]}}).to_string(),
        NOW,
    );
    let menu = surface(&c)["menu"].as_array().unwrap().clone();
    assert_eq!(menu.len(), 2);
    let fx = c.gum_tap_menu("snooze");
    assert_eq!(sent(&fx)[0]["d"]["action"], "snooze");
    assert_eq!(sent(&fx)[0]["d"]["item"], "t1");
}

#[test]
fn a_tap_selects_and_activates_in_one_move() {
    let mut c = open_todos();
    let fx = c.gum_tap(1); // t2, one action
    assert_eq!(sent(&fx)[0]["d"]["action"], "reopen");
}

#[test]
fn tapping_the_list_behind_a_menu_dismisses_it_first() {
    let mut c = open_todos();
    c.gum_activate(); // menu on t1
    let fx = c.gum_tap(2); // t3 has no actions
    assert!(surface(&c)["menu"].is_null(), "clicking away from a menu closes it");
    assert!(sent(&fx).is_empty());
}
