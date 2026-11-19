/** Link unfurling and inline cards (spec F10).
 *
 *  THE RULE THIS FILE EXISTS TO ENFORCE: opening a channel must not phone home.
 *
 *  A URL in a message is somebody else's URL. Loading it — an <img>, a font, a
 *  video poster, a favicon — hands that person your IP address, your
 *  User-Agent, and the exact moment you read their line. In email this is the
 *  tracking pixel; in a chat channel it is worse, because a message is
 *  broadcast and one paste can instrument everyone in the room at once.
 *
 *  So nothing remote loads on render. Ever. A media URL becomes a bounded card
 *  naming its host, and the bytes are fetched only when the reader asks (or has
 *  said "always", which is their call to make). When they do ask, we fetch and
 *  hand the DOM a `blob:` URL — so the request happens once, on purpose, from
 *  the code that decided to make it, rather than implicitly from markup.
 *
 *  The CSP in index.html is the backstop: `img-src 'self' data: blob:` means a
 *  vector this file forgets about is refused by the browser anyway. Defence in
 *  depth, because the cost of being wrong here is a de-anonymised reader.
 *
 *  And there is NO server involvement in any of it. gliph never fetches a URL on
 *  a user's behalf — a server-side unfurler would see every link every user
 *  reads, which is precisely the log this product exists not to keep.
 */

import { esc } from "./escape.ts";
// ---- preference ----

export type Unfurl = "ask" | "always" | "never";
const LS_UNFURL = "gliph.unfurl";

export const unfurlMode = (): Unfurl => (localStorage.getItem(LS_UNFURL) as Unfurl) ?? "ask";
export const setUnfurlMode = (m: Unfurl) => localStorage.setItem(LS_UNFURL, m);

// ---- classification ----

const IMAGE = /\.(png|jpe?g|gif|webp|avif|bmp|ico)(\?|#|$)/i;
// SVG is deliberately NOT an image here. It is a document that can carry script
// and remote references, and rendering one from a stranger is a different risk
// from rendering a PNG. It shows as a plain file card.
const VIDEO = /\.(mp4|webm|mov|m4v)(\?|#|$)/i;
const AUDIO = /\.(mp3|ogg|wav|m4a|flac)(\?|#|$)/i;
const PDF = /\.pdf(\?|#|$)/i;
const YOUTUBE = /(?:youtube\.com\/watch\?v=|youtu\.be\/|youtube\.com\/embed\/)([\w-]{11})/;

export type CardKind = "image" | "video" | "audio" | "pdf" | "youtube" | "link";

/** Does this filename name an image? Same list as the URL rule above, SVG
 *  included in the exclusion for the same reason. */
export const isImageName = (name: string) => IMAGE.test(name);

export type Card = { kind: CardKind; url: string; host: string; id?: string };

export function classify(url: string): Card | null {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  // http(s) only. A javascript:, data: or file: URL in a chat line has no
  // business becoming a card with a click target.
  if (u.protocol !== "http:" && u.protocol !== "https:") return null;

  const host = u.host;
  const yt = YOUTUBE.exec(url);
  if (yt) return { kind: "youtube", url, host, id: yt[1] };
  if (IMAGE.test(u.pathname)) return { kind: "image", url, host };
  if (VIDEO.test(u.pathname)) return { kind: "video", url, host };
  if (AUDIO.test(u.pathname)) return { kind: "audio", url, host };
  if (PDF.test(u.pathname)) return { kind: "pdf", url, host };
  return { kind: "link", url, host };
}

// ---- rendering ----

const LABEL: Record<CardKind, string> = {
  image: "image", video: "video", audio: "audio",
  pdf: "document", youtube: "youtube", link: "link",
};

/** One card, in its unloaded state. `data-load` is what the click handler in
 *  main.ts keys off; until that runs, this markup makes no network request of
 *  any kind. */
function placeholder(c: Card, i: number): string {
  const auto = unfurlMode() === "always";
  return `<div class="card media" data-card="${i}" data-kind="${c.kind}"
       data-url="${esc(c.url)}" ${auto ? "data-auto" : ""}>
    <div class="head">
      <span class="k">${LABEL[c.kind]}</span>
      <span class="host">${esc(c.host)}</span>
      <span class="spacer"></span>
      <button data-load="${i}">show</button>
    </div>
    <div class="slot"></div>
  </div>`;
}

/** The cards for one message body. Returns "" when unfurling is off or there is
 *  nothing to show — a plain link is already a link. */
export function renderCards(urls: string[]): string {
  if (unfurlMode() === "never" || !urls.length) return "";
  const cards = urls.map(classify).filter((c): c is Card => c !== null)
    // A bare link gets no card: it would be a box around text that is already
    // right there. Only media earns the space.
    .filter((c) => c.kind !== "link")
    .slice(0, 4); // one message does not get to fill the viewport
  return cards.map(placeholder).join("");
}

// ---- loading, on request ----

/** Blob URLs are revoked when their card leaves the DOM; until then this holds
 *  them so a re-render does not refetch what is already here. */
const loaded = new Map<string, string>();

/** The decoded <img> for each of those, kept alive across repaints. A repaint
 *  throws the whole stream DOM away; a brand new <img> pointing at the same
 *  blob still decodes asynchronously, and blanks for a frame or two on the way
 *  — which is the flicker. Handing the new slot back the SAME element it had
 *  before is what stops it: a node that is already decoded paints on the spot. */
const imgs = new Map<string, HTMLImageElement>();

/** Fetch the bytes and swap real media into the card. This is the ONLY place in
 *  the client that touches a URL somebody else chose, and it only runs because
 *  a reader asked it to. */
export async function loadCard(el: HTMLElement): Promise<void> {
  const url = el.dataset["url"];
  const kind = el.dataset["kind"] as CardKind | undefined;
  const slot = el.querySelector<HTMLElement>(".slot");
  if (!url || !kind || !slot || el.dataset["state"]) return;
  // A URL that already failed fails again. Repainting the stream re-runs this
  // for every auto card; refetching a dead link on each repaint swapped the
  // slot empty → failed asynchronously, and that height change is a visible
  // jump. Failing synchronously here keeps the layout settled within the same
  // paint. (A reload retries; a click on the link inside always works.)
  if (failed.has(url)) {
    el.dataset["state"] = "failed";
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- failedHTML() escapes the URL
    slot.innerHTML = failedHTML(url);
    return;
  }
  el.dataset["state"] = "loading";

  // YouTube never involves a fetch: it is an iframe, and clicking is consent.
  // -nocookie is the variant that does not set a tracking cookie before play.
  if (kind === "youtube") {
    const id = classify(url)?.id ?? "";
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- the video id is esc()'d
    slot.innerHTML = `<iframe class="yt" src="https://www.youtube-nocookie.com/embed/${esc(id)}?autoplay=1"
      allow="accelerometer; encrypted-media; picture-in-picture" allowfullscreen
      referrerpolicy="no-referrer" title="youtube"></iframe>`;
    el.dataset["state"] = "loaded";
    return;
  }

  try {
    // A file that never touched the server is already a blob in this tab —
    // fetching it back out only to make a second blob of it is a copy for
    // nothing.
    let objectURL = url.startsWith("blob:") ? url : loaded.get(url);
    if (!objectURL) {
      // no-referrer: the fetch itself must not tell the host which channel or
      // page the link came from.
      const res = await fetch(url, { referrerPolicy: "no-referrer", mode: "cors" });
      if (!res.ok) throw new Error(String(res.status));
      objectURL = URL.createObjectURL(await res.blob());
      loaded.set(url, objectURL);
    }
    if (kind === "image") slot.replaceChildren(image(url, objectURL));
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- mediaTag() escapes the blob URL and the link
    else slot.innerHTML = mediaTag(kind, objectURL, url);
    el.dataset["state"] = "loaded";
  } catch {
    // A failure here is ordinary — CORS, an offline host, a dead link. Say so
    // and leave the plain link, rather than pretending the card is empty.
    failed.add(url);
    el.dataset["state"] = "failed";
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- failedHTML() escapes the URL
    slot.innerHTML = failedHTML(url);
  }
}

/** URLs that failed to load this session. Remembered so a repaint neither
 *  refetches them nor resizes the card on the way to the same failure. */
const failed = new Set<string>();

function failedHTML(url: string): string {
  return `<div class="failed">could not load — <a href="${esc(url)}"
    target="_blank" rel="noreferrer noopener">open it directly</a></div>`;
}

/** The kept element for this URL, or a fresh one. The same file can be on
 *  screen twice; only one node can be in two places, so the second copy gets a
 *  clone — which decodes off the same blob and does not flicker either. */
function image(url: string, blob: string): HTMLImageElement {
  let img = imgs.get(url);
  if (!img) {
    img = new Image();
    img.alt = "";
    img.src = blob;
    imgs.set(url, img);
  }
  return img.isConnected ? (img.cloneNode(true) as HTMLImageElement) : img;
}

function mediaTag(kind: CardKind, blob: string, original: string): string {
  switch (kind) {
    case "image":
      return `<img src="${esc(blob)}" alt="" loading="lazy">`;
    case "video":
      return `<video src="${esc(blob)}" controls preload="metadata" playsinline></video>`;
    case "audio":
      return `<audio src="${esc(blob)}" controls preload="metadata"></audio>`;
    case "pdf":
      // A blob: PDF renders in the browser's own viewer, which is sandboxed and
      // — because the bytes are already local — makes no further request.
      return `<iframe class="pdf" src="${esc(blob)}#toolbar=0" title="document"></iframe>`;
    default:
      return `<a href="${esc(original)}" target="_blank" rel="noreferrer noopener">open</a>`;
  }
}

/** Full-size view of an image already fetched into a blob: URL — click the
 *  preview, get the whole thing. It hangs off <body> rather than the app root
 *  because a repaint replaces that root and would take the lightbox with it.
 *  Esc, the scrim and ✕ all close; clicking the image toggles 1:1 zoom. */
export function openLightbox(src: string, name: string, href: string): void {
  const box = document.createElement("div");
  box.className = "lightbox";
  box.setAttribute("data-overlay-open", "");
  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- name, src and href are esc()'d
  box.innerHTML =
    `<div class="lbar"><span class="lname">${esc(name)}</span>` +
    `<a href="${esc(src)}" target="_blank" rel="noopener">open</a>` +
    `<a href="${esc(href)}" download="${esc(name)}">download</a>` +
    `<button class="lclose">✕</button></div>` +
    `<div class="lwrap"><img src="${esc(src)}" alt="${esc(name)}"></div>`;

  const onKey = (e: KeyboardEvent) => {
    if (e.key !== "Escape") return;
    e.preventDefault();
    close();
  };
  const close = () => {
    box.remove();
    document.removeEventListener("keydown", onKey, true);
  };
  box.addEventListener("click", (e) => {
    const t = e.target;
    if (t instanceof HTMLImageElement) box.classList.toggle("zoom");
    else if (!(t instanceof HTMLAnchorElement)) close();
  });
  // Capture: the lightbox owns esc while it is up, ahead of the app's own
  // handlers, which see overlayOpen() and stand down anyway.
  document.addEventListener("keydown", onKey, true);
  document.body.appendChild(box);
}

/** Drop blob URLs whose cards are gone. Without this a long session leaks every
 *  image it ever showed. */
export function reapBlobs(root: ParentNode): void {
  const alive = new Set(
    [...root.querySelectorAll<HTMLElement>("[data-url]")].map((el) => el.dataset["url"]!),
  );
  // The lightbox hangs off <body>, outside the repainted root, and is showing
  // one of these blobs right now. Revoking it would blank the picture the
  // reader is looking at.
  const shown = new Set(
    [...document.querySelectorAll<HTMLImageElement>(".lightbox img")].map((i) => i.src),
  );
  for (const [url, objectURL] of loaded) {
    if (!alive.has(url) && !shown.has(objectURL)) {
      URL.revokeObjectURL(objectURL);
      loaded.delete(url);
      imgs.delete(url);
    }
  }
}
