/** Undo, recorded at the dispatcher.
 *
 *  Every slash command in gliph goes through one function — `Client.runCommand`
 *  — whether it came from typing `/ban`, from the command palette, or from a
 *  button somebody adds next year. So that is where undo is recorded, and the
 *  reason matters: the alternative is remembering to record at each call site,
 *  which works until the day it doesn't and one path quietly stops being
 *  undoable. One choke point, no per-caller code, no way to forget.
 *
 *  Undoing runs the inverse through the SAME dispatcher with recording off. Not
 *  doing that is the classic bug: the undo records itself, and ⌘Z starts
 *  flip-flopping between two states forever.
 *
 *  ONLY COMMANDS WITH AN HONEST INVERSE ARE OFFERED. `/kick` has none — the
 *  person is gone, and `/invite`-ing them back is a different act that does not
 *  unsay the kick line already in the channel. Pretending otherwise would be
 *  worse than not offering undo at all, so a kick gets the toast (so you can see
 *  what you just did) and no undo button.
 */

export type Cmd = { name: string; args: string };

export type Entry = {
  /** what the user did, in their words */
  label: string;
  /** the command as issued — this is what redo re-runs */
  did: Cmd;
  /** the command that puts it back, or null when nothing honestly does */
  inverse: Cmd | null;
};

const MAX = 50;
const TOAST_MS = 7000;

const history: Entry[] = [];
const redo: Entry[] = [];

let toast: Entry | null = null;
let toastTimer: ReturnType<typeof setTimeout> | null = null;
let onChange: (() => void) | null = null;

export const currentToast = (): Entry | null => toast;
export const onUndoChange = (fn: () => void) => { onChange = fn; };

export const canUndo = (): boolean => history.length > 0;
export const canRedo = (): boolean => redo.length > 0;

export function dismissToast(): void {
  if (toastTimer) { clearTimeout(toastTimer); toastTimer = null; }
  toast = null;
  onChange?.();
}

/** What undoes a command, given the state it is about to change.
 *
 *  `before` is read at dispatch time because some inverses need it — putting a
 *  topic back means knowing what it was, and after the command lands that is
 *  gone. */
export function inverseOf(
  name: string,
  args: string,
  before: { topic?: string; modes?: string },
): Entry | null {
  const nick = args.trim().split(/\s+/)[0] ?? "";
  const did: Cmd = { name, args };
  const e = (label: string, inverse: Cmd | null): Entry => ({ label, did, inverse });

  switch (name) {
    case "op": return nick ? e(`made ${nick} an op`, { name: "deop", args: nick }) : null;
    case "deop": return nick ? e(`removed ${nick}'s op`, { name: "op", args: nick }) : null;
    case "voice": return nick ? e(`voiced ${nick}`, { name: "devoice", args: nick }) : null;
    case "devoice": return nick ? e(`devoiced ${nick}`, { name: "voice", args: nick }) : null;
    case "ban": return nick ? e(`banned ${nick}`, { name: "unban", args: nick }) : null;
    case "unban": return nick ? e(`unbanned ${nick}`, { name: "ban", args: nick }) : null;

    case "topic":
      // Restoring a topic needs the old one, and an empty previous topic is a
      // real value — `/topic` with no argument clears it.
      return e("changed the topic", { name: "topic", args: before.topic ?? "" });

    case "mode": {
      // +m becomes -m and back. Only the simple single-sign form: a mixed
      // "+mi-t" has no obvious single inverse, and guessing one is how undo
      // does something the user did not ask for.
      const flags = args.trim();
      if (!/^[+-][a-z]+$/.test(flags)) return null;
      const flip = (flags[0] === "+" ? "-" : "+") + flags.slice(1);
      return e(`set mode ${flags}`, { name: "mode", args: flip });
    }

    // Destructive and irreversible. Shown, so you can see what you did, but
    // never offered as undoable.
    case "kick": return e(`kicked ${nick}`, null);

    default: return null;
  }
}

/** Record a dispatched command. Called by the dispatcher, not by callers. */
export function record(entry: Entry): void {
  if (entry.inverse) {
    history.push(entry);
    if (history.length > MAX) history.shift();
    // A new action abandons the redo branch — the future you could have gone
    // back to is not the future you are in now.
    redo.length = 0;
  }
  toast = entry;
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(dismissToast, TOAST_MS);
  onChange?.();
}

/** Pop the last undoable action and return the command that reverses it. The
 *  caller runs it through the dispatcher with recording OFF. */
export function takeUndo(): (Cmd & { label: string }) | null {
  const entry = history.pop();
  if (!entry?.inverse) return null;
  redo.push(entry);
  dismissToast();
  return { ...entry.inverse, label: entry.label };
}

/** The mirror: re-run what was undone. The entry carries the original command,
 *  so redo is just replaying it — no deriving an inverse of an inverse, which
 *  is where this would otherwise quietly go wrong (redoing a topic change would
 *  need the new topic, and only the original command still knows it). */
export function takeRedo(): (Cmd & { label: string }) | null {
  const entry = redo.pop();
  if (!entry) return null;
  history.push(entry);
  dismissToast();
  return { ...entry.did, label: entry.label };
}

/** Clear everything. A channel switch does not invalidate history (the commands
 *  name their target), but signing out does. */
export function reset(): void {
  history.length = 0;
  redo.length = 0;
  dismissToast();
}
