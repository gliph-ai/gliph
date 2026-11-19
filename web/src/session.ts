/** Reading the session the core wrote.
 *
 *  The client core owns the session and persists it as one JSON blob under
 *  gliph.session — `{token, server}`, the shape of Session in core/src/client.rs.
 *  It is the only writer.
 *
 *  The two pages that are not the client — the admin console and the device-link
 *  approval page — each had their own idea of where to find it, and both had the
 *  same wrong one: a gliph.token and a gliph.server that nothing has ever
 *  written. The console read them and concluded you were signed out; link.ts
 *  read them and offered to send you to sign in when you already had. They were
 *  wrong in the same way because they were guesses made twice.
 *
 *  So: one reader, named after the thing it reads, next to nothing else. If the
 *  core changes where it keeps a session, this file is the search result.
 */

const KEY = "gliph.session";

export type Session = { token: string; server: string };

export function session(): Session {
  // A page load with no session, storage disabled, or a half-written blob are
  // all the same answer: not signed in. Never a thrown error out of a getter
  // that every page calls before it can paint anything.
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return { token: "", server: "" };
    const s = JSON.parse(raw) as Partial<Session>;
    return { token: String(s.token ?? ""), server: String(s.server ?? "") };
  } catch {
    return { token: "", server: "" };
  }
}

/** Signing out. The core clears its own copy when it is the one doing the
 *  signing out; this is for the pages that have no core loaded. */
export function clearSession(): void {
  try { localStorage.removeItem(KEY); } catch { /* storage disabled: nothing to clear */ }
}

/** gliph.sh shows its front page at / to a visitor with no session, and the
 *  app to everyone else (server/landing.go). The session lives in localStorage,
 *  which the server cannot see, so the client leaves this cookie as the hint.
 *  It holds no secret: forging it only gets you the sign-in form. */
export function markSignedIn(on: boolean): void {
  if (location.protocol !== "https:" && location.protocol !== "http:") return;
  const secure = location.protocol === "https:" ? "; Secure" : "";
  document.cookie = on
    ? `gliph_in=1; Path=/; Max-Age=31536000; SameSite=Lax${secure}`
    : `gliph_in=; Path=/; Max-Age=0; SameSite=Lax${secure}`;
}
