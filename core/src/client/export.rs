//! Local log export (F8): a channel or DM as plain text and as JSONL, from what
//! THIS device holds — the decrypted copy, not the server's ciphertext.
//!
//! It is a child of `client` so it can read the event map and the decrypted
//! bodies directly; nothing here is reachable from a host except through
//! `/export`, which comes back as `SaveFile` effects every host already writes.

use serde_json::json;

use super::{fmt_ms, Client};
use crate::effect::Effect;
use crate::wire::Ev;

/// What a line says when this device never had the key for it. The export is
/// a record of what you could read, so it says so rather than dropping the row.
const UNREADABLE: &str = "[cannot decrypt: no key on this device]";

impl Client {
    /// Rows worth exporting: everything held for the channel, oldest first,
    /// except the client's own weather lines and messages not yet sent.
    fn export_rows(&self, cid: &str) -> Vec<(&Ev, Option<String>)> {
        self.events
            .get(cid)
            .map(|evs| {
                evs.iter()
                    .filter(|e| e.meta.get("local") != Some(&json!(true)) && !e.id.starts_with("pending-"))
                    .map(|e| (e, if e.enc { self.plain.get(&e.id).cloned() } else { Some(e.body.clone()) }))
                    .collect()
            })
            .unwrap_or_default()
    }

    /// `/export [txt|jsonl]`: one file per format, both when none is named.
    pub(crate) fn export_command(&mut self, rest: &str, fx: &mut Vec<Effect>) {
        let cid = self.active.clone();
        let fmt = rest.trim().to_lowercase();
        if !matches!(fmt.as_str(), "" | "txt" | "text" | "jsonl" | "json") {
            self.notice("usage: /export [txt|jsonl]", "error");
            return;
        }
        let Some(ch) = self.channel(&cid) else {
            self.notice("open a channel or DM to export it", "error");
            return;
        };
        let label = if ch.is_dm { ch.name.clone() } else { format!("#{}", ch.name) };
        let rows = self.export_rows(&cid);
        if rows.is_empty() {
            self.notice(&format!("nothing on this device for {label} yet"), "info");
            return;
        }
        let day = fmt_ms(self.now_ms).get(..10).unwrap_or("").to_string();
        let stem = file_stem(&ch.name, &day);
        let n = rows.len();
        let mut made = Vec::new();
        if fmt != "jsonl" && fmt != "json" {
            let head = format!("# gliph log · {label} · {} · exported {day} · {n} lines\n", self.server_name);
            let body: String = rows.iter().map(|(e, t)| text_line(e, t.as_deref()) + "\n").collect();
            made.push(format!("{stem}.txt"));
            fx.push(Effect::SaveFile { name: format!("{stem}.txt"), mime: "text/plain".into(), bytes: format!("{head}{body}").into_bytes() });
        }
        if fmt != "txt" && fmt != "text" {
            let body: String = rows.iter().map(|(e, t)| jsonl_line(e, t.as_deref()) + "\n").collect();
            made.push(format!("{stem}.jsonl"));
            fx.push(Effect::SaveFile { name: format!("{stem}.jsonl"), mime: "application/x-ndjson".into(), bytes: body.into_bytes() });
        }
        self.notice(&format!("exported {n} lines of {label}: {}", made.join(", ")), "info");
    }
}

/// `gliph-general-2026-11-18`. Only characters every filesystem accepts.
fn file_stem(name: &str, day: &str) -> String {
    let safe: String = name
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || c == '-' || c == '_' { c } else { '-' })
        .collect();
    if day.is_empty() { format!("gliph-{safe}") } else { format!("gliph-{safe}-{day}") }
}

/// `2026-11-18T12:00:00Z` → `2026-11-18 12:00`. Whatever else the server sent
/// is printed as it came.
fn stamp(ts: &str) -> String {
    match (ts.get(..10), ts.get(11..16)) {
        (Some(d), Some(t)) => format!("{d} {t}"),
        _ => ts.to_string(),
    }
}

/// One line in the irssi shape: `[stamp] <nick> text`, `* nick does`, and
/// `-!-` for everything the server said rather than a person.
pub(crate) fn text_line(e: &Ev, text: Option<&str>) -> String {
    let said = text.unwrap_or(UNREADABLE).replace('\n', "\n    ");
    let at = stamp(&e.ts);
    match e.kind.as_str() {
        "message" | "agent" | "log" | "commit" => format!("[{at}] <{}> {said}", e.actor),
        "action" => format!("[{at}] * {} {said}", e.actor),
        k if said.is_empty() => format!("[{at}] -!- {k} {}", e.actor),
        k => format!("[{at}] -!- {k} {}: {said}", e.actor),
    }
}

/// One JSON object per event. `text` is null when this device could not read
/// it, so a script reading the file can tell "empty" from "unknown".
pub(crate) fn jsonl_line(e: &Ev, text: Option<&str>) -> String {
    let mut row = json!({
        "seq": e.seq, "id": e.id, "ts": e.ts, "kind": e.kind,
        "actor": e.actor, "text": text, "enc": e.enc,
    });
    if !e.meta.is_null() {
        row["meta"] = e.meta.clone();
    }
    row.to_string()
}
