/** Where a device-link result may be delivered.
 *
 *  This is the client half of the rule server/devicelink.go enforces before it
 *  will mint a code: a result may only ever reach something listening on this
 *  machine. It lives in its own module — rather than in link.ts, which is a page
 *  and paints on import — so the check suite can exercise the real function.
 *
 *  Mirroring the server is not redundant. The server sees the redirect_uri only
 *  on the approve path; "not me" answers the waiting app without asking the
 *  server anything, so on that path this is the only check there is. Without it
 *  a crafted /link.html?redirect_uri=https://evil.example is an open redirect on
 *  the server's own domain, sprung by the button a cautious user is most likely
 *  to press.
 */
/** Where a ?next= may send someone after they sign in.
 *
 *  The admin console has no login form of its own — it bounces to the client's
 *  and asks to be returned to. That return address arrives in the URL, which
 *  means anyone can write it, so it is a path on this origin or it is nothing.
 *  A scheme, a host, or the protocol-relative "//evil.example" that a browser
 *  reads as one, and this returns null.
 *
 *  Lives here next to deliverable() for the same reason it does: both are
 *  "where may this go", both are exercised by scripts/check-render.mjs, and a
 *  page that paints on import cannot be. */
export function nextPath(raw: string | null | undefined): string | null {
  if (!raw || raw[0] !== "/") return null;
  // "//host" and "/\host" are both read as protocol-relative — off this origin.
  if (raw[1] === "/" || raw[1] === "\\") return null;
  if (raw.includes("..")) return null;
  // A control character can smuggle one of the above past the checks above.
  if (/[\x00-\x1f\x7f]/.test(raw)) return null;
  return raw;
}

export function deliverable(raw: string): URL | null {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return null;
  }
  // Hostname, not the raw string: "127.0.0.1.evil.example" starts with the
  // loopback address and is not remotely it. No "localhost" either — it goes
  // through the host's name service, and what that resolves to is not this
  // page's to assume. Same reasoning as the server.
  const loopback = url.hostname === "127.0.0.1" || url.hostname === "[::1]" || url.hostname === "::1";
  if (url.protocol !== "http:" || !loopback) return null;
  if (url.pathname === "" || raw.includes("..")) return null;
  return url;
}
