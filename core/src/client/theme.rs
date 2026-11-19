//! User-installable themes (F12): a theme file is a block of the same tokens
//! `web/src/theme.css` defines, plus a name. The core checks it once, here, so
//! a file that installs on the desktop installs on the phone and a broken one
//! fails with the same message on both. Applying it is the host's: the pixels
//! are the one thing the core never touches.
//!
//! ```text
//! /* name: harbour
//!    base: midnight */
//! --bg: #0b1d2a;
//! --accent: #ff7a59;
//! ```
//!
//! `base` (creme or midnight, default creme) fills every token the file leaves
//! out. A `:root { … }` wrapper is accepted, so a block copied out of a
//! stylesheet works as it is.

use std::collections::BTreeMap;

use serde::Serialize;

use super::Client;
use crate::effect::Effect;

/// Colour tokens a theme may set: every colour in theme.css that both clients
/// draw with. Shadows are left out because the phone has nothing to put them on.
pub const TOKENS: &[&str] = &[
    "chrome", "chrome-text", "chrome-dim", "bg", "surface", "surface-2", "rail", "text", "text-2",
    "muted", "border", "border-2", "accent", "link", "join", "action", "mode", "agent", "gold",
    "azure", "teal", "olive", "magenta", "hover", "r0-from", "r0-to", "code-bg", "code-surface",
    "code-surface-2", "code-text",
];

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct Theme {
    pub name: String,
    pub base: String,
    /// token (no leading `--`) -> `#rrggbb`
    pub tokens: BTreeMap<String, String>,
}

/// What `/theme add` opens the editor with: every token, so a person edits
/// colours rather than looking up names.
const TEMPLATE: &str = "/* name: my-theme\n   base: midnight */\n--bg: #12172a;\n--surface: #171d33;\n--surface-2: #212843;\n--rail: #101528;\n--chrome: #232b4a;\n--chrome-text: #f0ece0;\n--chrome-dim: #99a1c2;\n--text: #e2ddcd;\n--text-2: #c9c4b2;\n--muted: #6a7396;\n--border: #2e3654;\n--border-2: #364060;\n--accent: #e8517c;\n--link: #7d92f0;\n--join: #93c99a;\n--action: #c084f0;\n--mode: #7d92f0;\n--agent: #eba98a;\n--gold: #f2c14e;\n--azure: #9fc0ff;\n--teal: #5fc9c9;\n--olive: #b9c85a;\n--magenta: #e08ad6;\n";

/// `#abc` or `#aabbcc`, any case, back as `#aabbcc`.
fn hex(v: &str) -> Option<String> {
    let h = v.strip_prefix('#')?;
    if !h.chars().all(|c| c.is_ascii_hexdigit()) {
        return None;
    }
    match h.len() {
        6 => Some(format!("#{}", h.to_ascii_lowercase())),
        3 => Some(format!("#{}", h.chars().flat_map(|c| [c, c]).collect::<String>().to_ascii_lowercase())),
        _ => None,
    }
}

/// Split a file into its code lines (comments blanked, line numbers kept) and
/// the lines found inside comments, where `name:` and `base:` usually live.
fn split_comments(text: &str) -> (Vec<String>, Vec<(usize, String)>) {
    let (mut code, mut notes) = (vec![String::new()], vec![(1, String::new())]);
    let mut chars = text.chars().peekable();
    let (mut block, mut line_c) = (false, false);
    while let Some(c) = chars.next() {
        if c == '\n' {
            line_c = false;
            code.push(String::new());
            notes.push((code.len(), String::new()));
            continue;
        }
        if !block && !line_c && c == '/' && matches!(chars.peek(), Some('*') | Some('/')) {
            block = chars.next() == Some('*');
            line_c = !block;
            continue;
        }
        if block && c == '*' && chars.peek() == Some(&'/') {
            chars.next();
            block = false;
            continue;
        }
        if block || line_c {
            notes.last_mut().unwrap().1.push(c);
        } else {
            code.last_mut().unwrap().push(c);
        }
    }
    (code, notes)
}

fn header(line: &str) -> Option<(String, String)> {
    let (k, v) = line.trim().trim_start_matches('*').split_once(':')?;
    let k = k.trim().to_lowercase();
    (k == "name" || k == "base").then(|| (k, v.trim().trim_end_matches(';').trim().to_string()))
}

/// Check a theme file. The error names the line, because "invalid theme" on a
/// forty-line paste is no help to anyone.
pub fn parse(text: &str) -> Result<Theme, String> {
    let mut name = String::new();
    let mut base = String::from("creme");
    let mut tokens = BTreeMap::new();
    let (code, notes) = split_comments(text);
    let mut heads: Vec<(usize, String, String)> =
        notes.iter().filter_map(|(n, l)| header(l).map(|(k, v)| (*n, k, v))).collect();
    for (i, raw) in code.iter().enumerate() {
        let n = i + 1;
        let line = raw.trim();
        if line.is_empty() || line == "}" || line.ends_with('{') {
            continue;
        }
        if let Some(tok) = line.strip_prefix("--") {
            let (k, v) = tok.split_once(':').ok_or(format!("line {n}: expected --token: #hex;"))?;
            let (k, v) = (k.trim(), v.trim().trim_end_matches(';').trim());
            if !TOKENS.contains(&k) {
                return Err(format!("line {n}: --{k} is not a theme token"));
            }
            let c = hex(v).ok_or(format!("line {n}: --{k} needs a colour like #1a2138, not {v:?}"))?;
            tokens.insert(k.to_string(), c);
            continue;
        }
        match header(line) {
            Some((k, v)) => heads.push((n, k, v)),
            None => return Err(format!("line {n}: expected --token: #hex; or name: / base:")),
        }
    }
    for (n, k, v) in heads {
        if k == "name" {
            let ok = !v.is_empty()
                && v.len() <= 32
                && v.chars().all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_');
            if !ok {
                return Err(format!("line {n}: a name is letters, digits, - and _, up to 32"));
            }
            name = v.to_lowercase();
        } else if v == "creme" || v == "midnight" {
            base = v;
        } else {
            return Err(format!("line {n}: base is creme or midnight, not {v:?}"));
        }
    }
    if name.is_empty() {
        return Err("the theme needs a name: line, e.g. /* name: harbour */".into());
    }
    if name == "creme" || name == "midnight" {
        return Err(format!("{name} is a built-in theme; pick another name"));
    }
    if tokens.is_empty() {
        return Err("no tokens found: a theme is lines like --bg: #12172a;".into());
    }
    Ok(Theme { name, base, tokens })
}

impl Client {
    /// `/theme add [file]`, `/theme <name>`, `/theme remove <name>`, `/theme`.
    /// Only `add` needs the core: it is where a file is checked. The rest are
    /// forwarded, because the installed list lives with the host's theme pref.
    pub(crate) fn theme_command(&mut self, rest: &str, fx: &mut Vec<Effect>) {
        let (verb, arg) = match rest.find(char::is_whitespace) {
            Some(i) => (&rest[..i], rest[i..].trim()),
            None => (rest, ""),
        };
        let theme = |op: &str, name: &str, json: String| Effect::Theme { op: op.into(), name: name.into(), theme: json };
        match verb.to_lowercase().as_str() {
            "add" if arg.is_empty() => fx.push(Effect::Compose {
                title: "paste or write a theme".into(),
                lang: "css".into(),
                text: TEMPLATE.into(),
                submit: "/theme add".into(),
            }),
            "add" => match parse(arg) {
                Ok(t) => {
                    let json = serde_json::to_string(&t).unwrap_or_default();
                    fx.push(theme("add", &t.name, json));
                }
                Err(e) => self.notice(&format!("theme not installed: {e}"), "error"),
            },
            "remove" if !arg.is_empty() => fx.push(theme("remove", &arg.to_lowercase(), String::new())),
            "" | "list" => fx.push(theme("list", "", String::new())),
            "remove" => self.notice("usage: /theme remove <name>", "error"),
            name => fx.push(theme("use", &name.to_lowercase(), String::new())),
        }
    }
}
