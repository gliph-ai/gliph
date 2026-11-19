//! GUM: the blob surface, as a platform-neutral tree.
//!
//! A provider (a plugin, or a builtin) sends a document; the client renders it
//! and turns interaction back into actions. The client hardcodes no app UI —
//! exactly as it hardcodes no commands. Design: docs/plans/gliph-apps.md.
//!
//! What lives here is everything about that which is not pixels: the document
//! model, the flattened focus order, the selection, the open menu, the input
//! drafts, and the keyboard model. A frontend receives a [`Surface`] — a flat
//! list of [`Node`]s already carrying "this row is selected", "this one is
//! number 3", "this one has a submenu" — and draws it. It decides nothing.
//!
//! **The framework owns the keyboard so no blob ever defines its own keys:**
//!
//!   j/k or arrows  move the selection
//!   1..9           jump to a list item
//!   enter          open an item's action menu, fire a button, submit an input
//!   esc            close menu, then clear input, then leave the blob
//!
//! An open menu owns those keys: they walk the menu, not the list behind it.
//! That rule used to live in main.ts and applied to one client; it is here now
//! and applies to all of them.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

// ---- what a provider sends ----

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Action {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub label: String,
    #[serde(default)]
    pub min_role: Option<String>,
    /// Shown as a yes/no before the action fires. The core cannot draw a
    /// dialog, so it asks the host — see [`crate::effect::Effect::AskConfirm`].
    #[serde(default)]
    pub confirm: Option<String>,
    /// Asks for one line of text before the action fires.
    #[serde(default)]
    pub prompt: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Item {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub label: String,
    #[serde(default)]
    pub hint: Option<String>,
    /// ok | warn | error | done | open
    #[serde(default)]
    pub state: Option<String>,
    #[serde(default)]
    pub actions: Option<Vec<Action>>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Block {
    /// text | divider | list | table | input | buttons | bar
    #[serde(default)]
    pub t: String,
    #[serde(default)]
    pub id: Option<String>,
    #[serde(default)]
    pub body: Option<String>,
    /// plain | info | warn | error
    #[serde(default)]
    pub style: Option<String>,
    #[serde(default)]
    pub label: Option<String>,
    #[serde(default)]
    pub items: Option<Vec<Item>>,
    #[serde(default)]
    pub cols: Option<Vec<String>>,
    #[serde(default)]
    pub rows: Option<Vec<Vec<String>>>,
    #[serde(default)]
    pub placeholder: Option<String>,
    #[serde(default)]
    pub action: Option<String>,
    #[serde(default)]
    pub actions: Option<Vec<Action>>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Doc {
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub blocks: Vec<Block>,
}

/// One app attached to a channel, as the switcher lists it.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct BlobSpec {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub source: String,
}

// ---- what a frontend renders ----

/// A drawable node. Everything a UI needs to paint one row is on it, resolved:
/// no frontend recomputes selection, numbering, or whether a row has a submenu.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "kind")]
pub enum Node {
    Text { body: String, style: String },
    Divider { label: String },
    Row {
        /// Position in the focus order — what a tap reports back.
        index: usize,
        /// 1..9, or 0 for "no shortcut number".
        number: usize,
        label: String,
        hint: String,
        /// ok | warn | error | done | open | "" — a frontend maps it to a glyph.
        state: String,
        has_actions: bool,
        selected: bool,
    },
    Table { cols: Vec<String>, rows: Vec<Vec<String>> },
    Input {
        index: usize,
        label: String,
        placeholder: String,
        value: String,
        selected: bool,
    },
    Buttons { buttons: Vec<Button> },
    Bar { body: String },
}

#[derive(Debug, Clone, Serialize)]
pub struct Button {
    pub index: usize,
    pub label: String,
    pub selected: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct MenuEntry {
    pub id: String,
    pub label: String,
    pub selected: bool,
}

/// The whole blob surface, ready to draw.
#[derive(Debug, Clone, Serialize)]
pub struct Surface {
    /// The blob being shown; "" when the chat stream is.
    pub blob: String,
    pub title: String,
    /// False until the provider's first `blob.render` lands.
    pub loaded: bool,
    pub nodes: Vec<Node>,
    /// The action menu an item opened, if any. While this is Some it owns the
    /// keyboard.
    pub menu: Option<Vec<MenuEntry>>,
    /// The apps in this channel, for the switcher.
    pub blobs: Vec<BlobSpec>,
}

// ---- focus ----

#[derive(Debug, Clone)]
enum Focus {
    Item { item: Item },
    Button { action: Action },
    Input { block: String, action: String },
}

/// Whatever a confirm or prompt is standing in front of. Only one can exist:
/// it is a modal, and pretending otherwise would mean answering the wrong one.
#[derive(Debug, Clone)]
struct Pending {
    action: Action,
    item: Option<String>,
    /// The confirm has been answered yes and only the prompt is left.
    confirmed: bool,
}

#[derive(Default)]
pub struct Gum {
    /// Which blob is open ("" = the chat stream).
    pub active: String,
    /// channel:blob -> the last document the provider sent.
    docs: BTreeMap<String, Doc>,
    sel: usize,
    menu: Option<(String, Vec<Action>)>,
    msel: usize,
    /// Per input block, so two inputs cannot share one buffer — which is what
    /// a single draft field quietly did.
    drafts: BTreeMap<String, String>,
    pending: Option<Pending>,
    /// The input block a submit just came from. When the provider answers with
    /// a new document the selection goes back to it — you typed in a box and
    /// pressed enter, so you are still in that box. Without this the selection
    /// silently becomes whatever row the new item landed at, and the second
    /// thing you type opens that row's menu instead of adding anything.
    sticky_input: Option<String>,
}

/// What a keyboard-model call decided the client should do next.
pub enum Outcome {
    /// Nothing; the selection or a draft moved.
    Nothing,
    /// Send `blob.event` with these arguments.
    Fire { action: String, item: String, value: String },
    /// Ask the user to confirm, then call `answer`.
    Confirm(String),
    /// Ask the user for a line, then call `answer`.
    Prompt(String),
    /// Leave the blob and go back to the chat stream.
    Leave,
}

impl Gum {
    fn key(channel: &str, blob: &str) -> String {
        format!("{channel}:{blob}")
    }

    pub fn put_doc(&mut self, channel: &str, blob: &str, doc: Doc) {
        self.docs.insert(Self::key(channel, blob), doc);
        // A fresh document invalidates a menu that was standing over the old
        // one: the item it belonged to may not exist any more.
        self.menu = None;

        // Put the selection back on the input a submit came from, if it is
        // still there. Everything else about the document may have moved.
        if let Some(block) = self.sticky_input.take() {
            if self.active == blob {
                if let Some(i) = self.focus(channel).iter().position(
                    |f| matches!(f, Focus::Input { block: b, .. } if *b == block),
                ) {
                    self.sel = i;
                }
            }
        }
    }

    /// A provider popped a submenu for one item, unprompted.
    pub fn put_menu(&mut self, item: &str, actions: Vec<Action>) {
        self.menu = Some((item.to_string(), actions));
        self.msel = 0;
    }

    pub fn doc(&self, channel: &str) -> Option<&Doc> {
        if self.active.is_empty() {
            return None;
        }
        self.docs.get(&Self::key(channel, &self.active))
    }

    /// Open a blob, or "" to go back to the chat stream. Selection and drafts
    /// reset: they belonged to the surface being left.
    pub fn open(&mut self, blob: &str) {
        self.active = blob.to_string();
        self.sel = 0;
        self.msel = 0;
        self.menu = None;
        self.pending = None;
        self.drafts.clear();
        self.sticky_input = None;
    }

    fn focus(&self, channel: &str) -> Vec<Focus> {
        let mut out = Vec::new();
        let Some(doc) = self.doc(channel) else { return out };
        for b in &doc.blocks {
            match b.t.as_str() {
                "list" => {
                    for it in b.items.iter().flatten() {
                        out.push(Focus::Item { item: it.clone() });
                    }
                }
                "buttons" => {
                    for a in b.actions.iter().flatten() {
                        out.push(Focus::Button { action: a.clone() });
                    }
                }
                "input" => {
                    if let Some(action) = &b.action {
                        out.push(Focus::Input {
                            block: b.id.clone().unwrap_or_default(),
                            action: action.clone(),
                        });
                    }
                }
                _ => {}
            }
        }
        out
    }

    // ---- the keyboard model ----

    pub fn move_by(&mut self, channel: &str, delta: isize) {
        if let Some((_, actions)) = &self.menu {
            let n = actions.len();
            if n > 0 {
                self.msel = (self.msel as isize + delta).rem_euclid(n as isize) as usize;
            }
            return;
        }
        let n = self.focus(channel).len();
        self.sel = if n == 0 { 0 } else { (self.sel as isize + delta).rem_euclid(n as isize) as usize };
    }

    /// `1`..`9`. Out of range is ignored rather than clamped: jumping to the
    /// last row because you typed 8 in a list of three is not what you meant.
    pub fn jump(&mut self, channel: &str, n: usize) {
        if n == 0 {
            return;
        }
        let i = n - 1;
        if let Some((_, actions)) = &self.menu {
            if i < actions.len() {
                self.msel = i;
            }
            return;
        }
        if i < self.focus(channel).len() {
            self.sel = i;
        }
    }

    /// Tap: select and activate in one move, which is what a finger means.
    pub fn tap(&mut self, channel: &str, index: usize) -> Outcome {
        // Tapping the list behind an open menu dismisses it, as clicking away
        // from any menu does.
        self.menu = None;
        if index < self.focus(channel).len() {
            self.sel = index;
        }
        self.activate(channel)
    }

    pub fn tap_menu(&mut self, action_id: &str) -> Outcome {
        let Some((item, actions)) = self.menu.clone() else { return Outcome::Nothing };
        let Some(a) = actions.into_iter().find(|a| a.id == action_id) else {
            return Outcome::Nothing;
        };
        self.menu = None;
        self.fire(a, Some(item))
    }

    pub fn set_draft(&mut self, block: &str, text: &str) {
        self.drafts.insert(block.to_string(), text.to_string());
    }

    pub fn activate(&mut self, channel: &str) -> Outcome {
        // The menu, once open, is what enter acts on.
        if let Some((item, actions)) = self.menu.clone() {
            let Some(a) = actions.get(self.msel).cloned() else { return Outcome::Nothing };
            self.menu = None;
            return self.fire(a, Some(item));
        }
        let focus = self.focus(channel);
        let Some(f) = focus.get(self.sel).cloned() else { return Outcome::Nothing };
        match f {
            Focus::Button { action } => self.fire(action, None),
            Focus::Input { block, action } => {
                let text = self.drafts.get(&block).cloned().unwrap_or_default();
                let text = text.trim().to_string();
                if text.is_empty() {
                    return Outcome::Nothing;
                }
                self.drafts.remove(&block);
                self.sticky_input = Some(block);
                Outcome::Fire { action, item: String::new(), value: text }
            }
            // One action fires; several open the menu.
            Focus::Item { item } => {
                let acts = item.actions.clone().unwrap_or_default();
                match acts.len() {
                    0 => Outcome::Nothing,
                    1 => self.fire(acts[0].clone(), Some(item.id.clone())),
                    _ => {
                        self.menu = Some((item.id.clone(), acts));
                        self.msel = 0;
                        Outcome::Nothing
                    }
                }
            }
        }
    }

    /// esc: close the menu, then clear a draft, then leave the blob. Three
    /// meanings for one key, in the order the user most likely meant.
    pub fn escape(&mut self) -> Outcome {
        if self.pending.take().is_some() {
            return Outcome::Nothing;
        }
        if self.menu.take().is_some() {
            return Outcome::Nothing;
        }
        if self.drafts.values().any(|d| !d.is_empty()) {
            self.drafts.clear();
            return Outcome::Nothing;
        }
        self.sel = 0;
        Outcome::Leave
    }

    fn fire(&mut self, action: Action, item: Option<String>) -> Outcome {
        if let Some(msg) = action.confirm.clone() {
            self.pending = Some(Pending { action, item, confirmed: false });
            return Outcome::Confirm(msg);
        }
        if let Some(msg) = action.prompt.clone() {
            self.pending = Some(Pending { action, item, confirmed: true });
            return Outcome::Prompt(msg);
        }
        Outcome::Fire {
            action: action.id,
            item: item.unwrap_or_default(),
            value: String::new(),
        }
    }

    /// The host answered the confirm or prompt it was asked for. `ok` false is
    /// a cancel, and cancelling must never fire the action.
    pub fn answer(&mut self, ok: bool, value: &str) -> Outcome {
        let Some(p) = self.pending.take() else { return Outcome::Nothing };
        if !ok {
            return Outcome::Nothing;
        }
        // A confirm answered yes may still owe a prompt.
        if !p.confirmed {
            if let Some(msg) = p.action.prompt.clone() {
                self.pending = Some(Pending { confirmed: true, ..p });
                return Outcome::Prompt(msg);
            }
        }
        Outcome::Fire {
            action: p.action.id,
            item: p.item.unwrap_or_default(),
            value: value.to_string(),
        }
    }

    // ---- the drawable tree ----

    pub fn surface(&self, channel: &str, blobs: Vec<BlobSpec>) -> Surface {
        let doc = self.doc(channel);
        let mut nodes = Vec::new();
        let mut i = 0usize;

        if let Some(doc) = doc {
            for b in &doc.blocks {
                match b.t.as_str() {
                    "text" => nodes.push(Node::Text {
                        body: b.body.clone().unwrap_or_default(),
                        style: b.style.clone().unwrap_or_else(|| "plain".into()),
                    }),
                    "divider" => {
                        nodes.push(Node::Divider { label: b.label.clone().unwrap_or_default() })
                    }
                    "list" => {
                        for it in b.items.iter().flatten() {
                            let index = i;
                            i += 1;
                            nodes.push(Node::Row {
                                index,
                                // Only the first nine get a shortcut, because
                                // only the first nine have a key.
                                number: if index < 9 { index + 1 } else { 0 },
                                label: it.label.clone(),
                                hint: it.hint.clone().unwrap_or_default(),
                                state: it.state.clone().unwrap_or_default(),
                                has_actions: !it.actions.clone().unwrap_or_default().is_empty(),
                                selected: index == self.sel && self.menu.is_none(),
                            });
                        }
                    }
                    "table" => nodes.push(Node::Table {
                        cols: b.cols.clone().unwrap_or_default(),
                        rows: b.rows.clone().unwrap_or_default(),
                    }),
                    "input" => {
                        if b.action.is_some() {
                            let index = i;
                            i += 1;
                            let id = b.id.clone().unwrap_or_default();
                            nodes.push(Node::Input {
                                index,
                                label: b.label.clone().unwrap_or_default(),
                                placeholder: b.placeholder.clone().unwrap_or_default(),
                                value: self.drafts.get(&id).cloned().unwrap_or_default(),
                                selected: index == self.sel && self.menu.is_none(),
                            });
                        }
                    }
                    "buttons" => {
                        let mut buttons = Vec::new();
                        for a in b.actions.iter().flatten() {
                            let index = i;
                            i += 1;
                            buttons.push(Button {
                                index,
                                label: a.label.clone(),
                                selected: index == self.sel && self.menu.is_none(),
                            });
                        }
                        nodes.push(Node::Buttons { buttons });
                    }
                    "bar" => nodes.push(Node::Bar { body: b.body.clone().unwrap_or_default() }),
                    _ => {}
                }
            }
        }

        Surface {
            blob: self.active.clone(),
            title: doc.map(|d| d.title.clone()).unwrap_or_default(),
            loaded: doc.is_some(),
            nodes,
            menu: self.menu.as_ref().map(|(_, actions)| {
                actions
                    .iter()
                    .enumerate()
                    .map(|(n, a)| MenuEntry {
                        id: a.id.clone(),
                        label: a.label.clone(),
                        selected: n == self.msel,
                    })
                    .collect()
            }),
            blobs,
        }
    }

    /// The block id of the input the selection is on, if it is on one. A
    /// frontend needs this to know which field a keystroke belongs to.
    pub fn focused_input(&self, channel: &str) -> Option<String> {
        match self.focus(channel).get(self.sel) {
            Some(Focus::Input { block, .. }) => Some(block.clone()),
            _ => None,
        }
    }
}
