//! Sandboxed user script hooks (F13), mIRC's `on TEXT` for a chat with E2EE.
//!
//! A script is Rhai source with up to three entry points:
//!
//! ```text
//! let pattern = "*deploy*";                 // on_message only fires on a match
//! fn on_message(nick, text, chan) { notice(nick + " is deploying") }
//! fn on_highlight(nick, text, chan) { say("back in 5, " + nick) }
//! fn on_join(nick, chan) { say("welcome " + nick) }
//! ```
//!
//! It runs in the core, so web, desktop and iOS run the same interpreter on the
//! same rules, on the plaintext this device already decrypted. The sandbox is
//! what Rhai is with nothing added: no file, network, clock or module access.
//! The scoped API is `say(text)` (one line into the channel that fired it),
//! `notice(text)` and `print(text)` (a local line only you see). Every run has
//! an operation budget, so a runaway script is stopped deterministically, the
//! same way on every platform, and switched off until its owner turns it back
//! on. Scripts are text, so sharing one is `/script export` and a paste.

use std::cell::RefCell;
use std::rc::Rc;

use rhai::{CallFnOptions, Dynamic, Engine, EvalAltResult, Scope, AST};
use serde::{Deserialize, Serialize};
use serde_json::json;

use super::{parse_ms, Client, ANNOUNCE_MAX_AGE_MS};
use crate::effect::Effect;
use crate::wire::{Ev, Notice};

/// Rhai operations per run. Enough for string work on a line; a `loop {}`
/// burns through it in about a millisecond.
pub const SCRIPT_OPS: u64 = 50_000;
/// Lines a single run may say. A greeting is one line; a script that wants
/// more is a bot, and bots are plugins.
const SAYS_PER_RUN: usize = 1;
const NOTICES_PER_RUN: usize = 5;

const TEMPLATE: &str = "// fires on lines that match this pattern (* and ? wildcards)\nlet pattern = \"*deploy*\";\n\nfn on_message(nick, text, chan) {\n    notice(nick + \" mentioned a deploy in \" + chan);\n}\n\nfn on_highlight(nick, text, chan) {\n    // say() posts one line into chan, as you\n}\n\nfn on_join(nick, chan) {\n}\n";

enum Out {
    Say(String),
    Notice(String),
}

/// What survives a relaunch: the source, and whether it is switched on.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SavedScript {
    pub name: String,
    pub src: String,
    #[serde(default)]
    pub on: bool,
}

struct Script {
    saved: SavedScript,
    ast: AST,
    pattern: String,
}

pub struct Scripts {
    list: Vec<Script>,
    engine: Engine,
    out: Rc<RefCell<Vec<Out>>>,
}

impl Default for Scripts {
    fn default() -> Self {
        let out: Rc<RefCell<Vec<Out>>> = Rc::default();
        // Not Engine::new(): the full standard library is most of a megabyte
        // of wasm. Arithmetic, logic and strings are what a chat hook uses.
        let mut e = Engine::new_raw();
        {
            use rhai::packages::{ArithmeticPackage, BasicStringPackage, LogicPackage, MoreStringPackage, Package};
            e.register_global_module(ArithmeticPackage::new().as_shared_module());
            e.register_global_module(LogicPackage::new().as_shared_module());
            e.register_global_module(BasicStringPackage::new().as_shared_module());
            e.register_global_module(MoreStringPackage::new().as_shared_module());
        }
        e.set_max_operations(SCRIPT_OPS);
        e.set_max_call_levels(32);
        e.set_max_expr_depths(64, 32);
        e.set_max_string_size(8_000);
        e.set_max_array_size(1_000);
        e.disable_symbol("eval");
        let o = out.clone();
        e.register_fn("say", move |s: &str| o.borrow_mut().push(Out::Say(s.to_string())));
        let o = out.clone();
        e.register_fn("notice", move |s: &str| o.borrow_mut().push(Out::Notice(s.to_string())));
        let o = out.clone();
        e.on_print(move |s| o.borrow_mut().push(Out::Notice(s.to_string())));
        e.on_debug(|_, _, _| {});
        Scripts { list: Vec::new(), engine: e, out }
    }
}

impl Scripts {
    pub fn is_empty(&self) -> bool {
        self.list.is_empty()
    }

    pub fn saved(&self) -> Vec<SavedScript> {
        self.list.iter().map(|s| s.saved.clone()).collect()
    }

    /// Compile and check a script. Top-level code runs once here, under the
    /// same budget, to read `pattern`; anything it says is thrown away.
    fn compile(&self, saved: SavedScript) -> Result<Script, String> {
        let ast = self.engine.compile(&saved.src).map_err(|e| e.to_string())?;
        let hooks: Vec<String> = ast.iter_functions().map(|f| f.name.to_string()).collect();
        if !hooks.iter().any(|h| matches!(h.as_str(), "on_message" | "on_highlight" | "on_join")) {
            return Err("define fn on_message(nick, text, chan), on_highlight(nick, text, chan) or on_join(nick, chan)".into());
        }
        let mut scope = Scope::new();
        let ran = self.engine.run_ast_with_scope(&mut scope, &ast);
        self.out.borrow_mut().clear();
        ran.map_err(|e| e.to_string())?;
        let pattern = scope
            .get_value::<rhai::ImmutableString>("pattern")
            .or_else(|| scope.get_value::<rhai::ImmutableString>("PATTERN"))
            .map(|s| s.to_string())
            .unwrap_or_else(|| "*".into());
        Ok(Script { saved, ast, pattern })
    }

    pub fn restore(&mut self, saved: Vec<SavedScript>) {
        self.list = saved.into_iter().filter_map(|s| self.compile(s).ok()).collect();
    }

    fn get(&self, name: &str) -> Option<&Script> {
        self.list.iter().find(|s| s.saved.name == name)
    }
}

/// mIRC wildcards, any case: `*` is any run, `?` one character.
pub fn wild(pattern: &str, text: &str) -> bool {
    let p: Vec<char> = pattern.to_lowercase().chars().collect();
    let t: Vec<char> = text.to_lowercase().chars().collect();
    let (mut pi, mut ti, mut star, mut mark) = (0, 0, None, 0);
    while ti < t.len() {
        if pi < p.len() && (p[pi] == '?' || p[pi] == t[ti]) {
            pi += 1;
            ti += 1;
        } else if pi < p.len() && p[pi] == '*' {
            star = Some(pi);
            mark = ti;
            pi += 1;
        } else if let Some(s) = star {
            pi = s + 1;
            mark += 1;
            ti = mark;
        } else {
            return false;
        }
    }
    p[pi..].iter().all(|&c| c == '*')
}

fn valid_name(n: &str) -> bool {
    !n.is_empty() && n.len() <= 32 && n.chars().all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
}

impl Client {
    /// Run every script that this arrival fires. Called from `announce`, which
    /// sees each event once, when it is first readable.
    pub(crate) fn run_scripts(&mut self, cid: &str, e: &Ev, fx: &mut Vec<Effect>) {
        if self.scripts.is_empty() || !self.live || is_local(e) {
            return;
        }
        if let Some(at) = parse_ms(&e.ts) {
            if self.now_ms > 0 && self.now_ms - at > ANNOUNCE_MAX_AGE_MS {
                return;
            }
        }
        if Some(&e.actor) == self.user.as_ref().map(|u| &u.nick) || self.ignores.contains(&e.actor) {
            return;
        }
        let chan = match self.channel(cid) {
            Some(c) if c.is_dm => c.name.clone(),
            Some(c) => format!("#{}", c.name),
            None => return,
        };
        let text = if e.enc { self.plain.get(&e.id).cloned().unwrap_or_default() } else { e.body.clone() };
        let text = crate::wire::split_quote(&text).1.to_string();
        let said = matches!(e.kind.as_str(), "message" | "action");
        let highlight = said && self.is_highlight(&text, &e.actor);

        let mut calls: Vec<(usize, &'static str, Vec<Dynamic>)> = Vec::new();
        for (i, s) in self.scripts.list.iter().enumerate().filter(|(_, s)| s.saved.on) {
            let has = |f: &str| s.ast.iter_functions().any(|x| x.name == f);
            let (nick, t, c) = (Dynamic::from(e.actor.clone()), Dynamic::from(text.clone()), Dynamic::from(chan.clone()));
            if said && has("on_message") && wild(&s.pattern, &text) {
                calls.push((i, "on_message", vec![nick.clone(), t.clone(), c.clone()]));
            }
            if highlight && has("on_highlight") {
                calls.push((i, "on_highlight", vec![nick.clone(), t.clone(), c.clone()]));
            }
            if e.kind == "join" && has("on_join") {
                calls.push((i, "on_join", vec![nick, c]));
            }
        }
        for (i, hook, args) in calls {
            self.run_one(i, hook, args, cid, fx);
        }
    }

    fn run_one(&mut self, i: usize, hook: &str, args: Vec<Dynamic>, cid: &str, fx: &mut Vec<Effect>) {
        let s = &self.scripts.list[i];
        if !s.saved.on {
            return; // stopped earlier in this same arrival
        }
        let name = s.saved.name.clone();
        let opts = CallFnOptions::new().eval_ast(false).rewind_scope(true);
        let res = self.scripts.engine.call_fn_with_options::<Dynamic>(opts, &mut Scope::new(), &s.ast, hook, args);
        let out: Vec<Out> = self.scripts.out.borrow_mut().drain(..).collect();
        if let Err(err) = res {
            let runaway = matches!(
                *err,
                EvalAltResult::ErrorTooManyOperations(_)
                    | EvalAltResult::ErrorStackOverflow(_)
                    | EvalAltResult::ErrorDataTooLarge(..)
            );
            if runaway {
                self.scripts.list[i].saved.on = false;
                self.script_notice("", &format!("script {name} was stopped: it ran past its limit in {hook}. /script on {name} restarts it"), "error");
                self.save_cache(fx);
                return; // a runaway's half-finished output is not sent
            }
            self.script_notice(cid, &format!("script {name}: {err}"), "error");
        }
        let (mut says, mut notes) = (0, 0);
        for o in out {
            match o {
                Out::Say(t) if says < SAYS_PER_RUN && !t.trim().is_empty() => {
                    says += 1;
                    self.script_say(cid, &t, fx);
                }
                Out::Notice(t) if notes < NOTICES_PER_RUN => {
                    notes += 1;
                    self.script_notice(cid, &format!("[{name}] {t}"), "info");
                }
                _ => {}
            }
        }
        fx.push(Effect::Changed);
    }

    fn script_notice(&mut self, cid: &str, text: &str, level: &str) {
        self.notices.push(Notice { channel: cid.into(), level: level.into(), text: text.into() });
    }

    /// One line into the channel that fired the script. `say` writes to the
    /// active channel and takes a pending reply with it, so both are set aside
    /// for the length of the call.
    ///
    /// ponytail: saying marks that channel read, the same as typing in it.
    fn script_say(&mut self, cid: &str, text: &str, fx: &mut Vec<Effect>) {
        let active = std::mem::replace(&mut self.active, cid.to_string());
        let reply = std::mem::take(&mut self.reply_to);
        fx.extend(self.say(text, "message", ""));
        self.active = active;
        self.reply_to = reply;
    }

    /// `/script [list|add|edit|remove|on|off|export] [name] [source]`.
    pub(crate) fn script_command(&mut self, rest: &str, fx: &mut Vec<Effect>) {
        let mut parts = rest.splitn(2, char::is_whitespace);
        let verb = parts.next().unwrap_or("").to_lowercase();
        let rest = parts.next().unwrap_or("").trim();
        let (name, body) = match rest.find(char::is_whitespace) {
            Some(i) => (rest[..i].to_lowercase(), rest[i..].trim()),
            None => (rest.to_lowercase(), ""),
        };
        let usage = "usage: /script add|edit|remove|on|off|export <name>, or /script list";
        if verb != "" && verb != "list" && !valid_name(&name) {
            self.notice(&format!("{usage} (a name is letters, digits, - and _)"), "error");
            return;
        }
        match verb.as_str() {
            "" | "list" => {
                if self.scripts.is_empty() {
                    self.notice("no scripts yet. /script add <name> writes one", "info");
                }
                let lines: Vec<String> = self
                    .scripts
                    .list
                    .iter()
                    .map(|s| {
                        let hooks: Vec<String> = s.ast.iter_functions().map(|f| f.name.to_string()).filter(|n| n.starts_with("on_")).collect();
                        format!("{} · {} · {} · pattern {}", s.saved.name, if s.saved.on { "on" } else { "off" }, hooks.join(", "), s.pattern)
                    })
                    .collect();
                for l in lines {
                    self.notice(&l, "info");
                }
            }
            "add" | "edit" if body.is_empty() => {
                let text = self.scripts.get(&name).map(|s| s.saved.src.clone()).unwrap_or_else(|| TEMPLATE.into());
                fx.push(Effect::Compose {
                    title: format!("script {name}"),
                    lang: "rust".into(),
                    text,
                    submit: format!("/script add {name}"),
                });
            }
            "add" | "edit" => match self.scripts.compile(SavedScript { name: name.clone(), src: body.to_string(), on: true }) {
                Ok(s) => {
                    let hooks: Vec<String> = s.ast.iter_functions().map(|f| f.name.to_string()).filter(|n| n.starts_with("on_")).collect();
                    self.notice(&format!("script {name} installed: {}", hooks.join(", ")), "info");
                    self.scripts.list.retain(|x| x.saved.name != name);
                    self.scripts.list.push(s);
                    self.save_cache(fx);
                }
                Err(e) => self.notice(&format!("script {name} not installed: {e}"), "error"),
            },
            "remove" | "on" | "off" | "export" => {
                let Some(i) = self.scripts.list.iter().position(|s| s.saved.name == name) else {
                    self.notice(&format!("no script called {name}"), "error");
                    return;
                };
                match verb.as_str() {
                    "remove" => {
                        self.scripts.list.remove(i);
                        self.notice(&format!("script {name} removed"), "info");
                    }
                    "export" => {
                        let src = self.scripts.list[i].saved.src.clone();
                        fx.push(Effect::SaveFile { name: format!("{name}.rhai"), mime: "text/plain".into(), bytes: src.into_bytes() });
                        return;
                    }
                    on => {
                        self.scripts.list[i].saved.on = on == "on";
                        self.notice(&format!("script {name} is {on}"), "info");
                    }
                }
                self.save_cache(fx);
            }
            _ => self.notice(usage, "error"),
        }
    }
}

fn is_local(e: &Ev) -> bool {
    e.meta.get("local") == Some(&json!(true)) || e.id.starts_with("pending-")
}
