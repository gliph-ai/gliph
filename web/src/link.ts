/** The device approval page (`/link.html`).
 *
 *  A native app opens this in the user's real browser — the one place they are
 *  already signed in, and the one place all the password / 2FA / SSO logic
 *  lives. The app itself never sees a password. See server/devicelink.go for
 *  the flow and what each parameter defends against.
 *
 *  This page's only job is to show a human what is being asked and get an
 *  honest yes or no. Everything else — PKCE, the loopback restriction, the
 *  single-use code — is enforced by the server, because a check that lives only
 *  in a page anybody can skip is not a check.
 */

import "./theme.css";
import { deliverable } from "./redirect";
import { esc } from "./escape.ts";
import { session } from "./session.ts";

const LS = { theme: "gliph.theme" };

document.documentElement.dataset.theme = localStorage.getItem(LS.theme) ?? "creme";

const app = document.getElementById("app")!;

const q = new URLSearchParams(location.search);
const params = {
  deviceName: q.get("device_name") ?? "",
  platform: q.get("platform") ?? "",
  redirectURI: q.get("redirect_uri") ?? "",
  state: q.get("state") ?? "",
  codeChallenge: q.get("code_challenge") ?? "",
  method: q.get("code_challenge_method") ?? "",
};

const { token, server: saved } = session();
const server = saved || location.origin;

const PLATFORM_LABEL: Record<string, string> = {
  desktop: "desktop app",
  ios: "iPhone or iPad",
  android: "Android phone",
  substrate: "Substrate",
};

// Substrate (substrate.gliph.sh) is the one web client allowed here: its
// callback is fixed on the server (-substrate-callback), which refuses any
// other redirect for this platform. The approve path hands the code only to the
// redirect the server just validated and echoed back.
const isSubstrate = params.platform === "substrate";
let approvedRedirect = "";

let busy = false;
let error = "";

function panel(inner: string): string {
  return `<div class="auth"><div class="panel">
    <h1 class="serif">gliph</h1>${inner}
  </div></div>`;
}

function render(): void {
  // Refuse before showing anything if the request is malformed. The server
  // would refuse too, but there is no reason to ask a human to approve
  // something we can already see is wrong.
  if (!params.redirectURI || !params.codeChallenge || params.method !== "S256") {
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- static copy
    app.innerHTML = panel(`
      <div class="sub">this link request is incomplete</div>
      <p class="note">Start it again from the app — it is missing the parameters
      that make the exchange safe.</p>`);
    return;
  }

  if (!token) {
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- static copy
    app.innerHTML = panel(`
      <div class="sub">sign in first</div>
      <p class="note">Approving a device means proving you are you. Open gliph,
      sign in, then run the link again from the app.</p>
      <button data-act="signin">open gliph</button>`);
    wire();
    return;
  }

  if (isSubstrate) {
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- host and error are esc()'d
    app.innerHTML = panel(`
      <div class="sub">sign in to Substrate</div>
      <p class="note">Substrate wants to sign you in with your gliph account on
        <strong>${esc(new URL(server).host)}</strong>. It gets your nick and email,
        not your messages or your keys.</p>
      ${error ? `<div class="err">${esc(error)}</div>` : ""}
      <div class="row">
        <button data-act="approve" ${busy ? "disabled" : ""}>${busy ? "signing in…" : "continue"}</button>
        <button data-act="deny" class="ghost">not me</button>
      </div>`);
    wire();
    return;
  }

  const what = PLATFORM_LABEL[params.platform] ?? "device";
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- device name, host and error are esc()'d
  app.innerHTML = panel(`
    <div class="sub">link a device</div>
    <p class="note">A ${esc(what)} calling itself
      <strong>${esc(params.deviceName || what)}</strong> wants to sign in to your
      gliph account on <strong>${esc(new URL(server).host)}</strong>.</p>
    <p class="note">Approve this only if you just started it yourself. It will
      get its own session, which you can sign out from your account without
      touching your other devices.</p>
    ${error ? `<div class="err">${esc(error)}</div>` : ""}
    <div class="row">
      <button data-act="approve" ${busy ? "disabled" : ""}>${busy ? "linking…" : "approve"}</button>
      <button data-act="deny" class="ghost">not me</button>
    </div>`);
  wire();
}

function wire(): void {
  app.querySelector('[data-act="signin"]')?.addEventListener("click", () => {
    location.href = "/";
  });
  app.querySelector('[data-act="deny"]')?.addEventListener("click", () => {
    // Tell the waiting app, rather than leaving it to time out wondering.
    hand({ error: "denied" });
  });
  app.querySelector('[data-act="approve"]')?.addEventListener("click", approve);
}

async function approve(): Promise<void> {
  if (busy) return;
  busy = true;
  error = "";
  render();
  try {
    const res = await fetch(server.replace(/\/$/, "") + "/api/auth/device-link/authorize", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
      body: JSON.stringify({
        device_name: params.deviceName,
        platform: params.platform,
        redirect_uri: params.redirectURI,
        code_challenge: params.codeChallenge,
        code_challenge_method: params.method,
      }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body?.error ?? "could not approve");
    approvedRedirect = body.redirect_uri ?? "";
    hand({ code: body.code });
  } catch (err) {
    busy = false;
    error = err instanceof Error ? err.message : "could not approve";
    render();
  }
}

/** Hand the result back to the waiting app over its loopback callback. */
function hand(result: { code?: string; error?: string }): void {
  if (isSubstrate) {
    // Only a code the server approved goes off this machine, and only to the
    // redirect the server validated. "not me" goes nowhere.
    if (!result.code || !approvedRedirect || approvedRedirect !== params.redirectURI) {
      location.href = "/";
      return;
    }
    const u = new URL(approvedRedirect);
    u.searchParams.set("code", result.code);
    if (params.state) u.searchParams.set("state", params.state);
    location.href = u.toString();
    return;
  }
  const url = deliverable(params.redirectURI);
  if (!url) {
    busy = false;
    error = "this link asks to send the result somewhere off this machine — refusing";
    render();
    return;
  }
  if (result.code) url.searchParams.set("code", result.code);
  if (result.error) url.searchParams.set("error", result.error);
  if (params.state) url.searchParams.set("state", params.state);
  location.href = url.toString();
}

render();
