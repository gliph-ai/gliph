/** room0 — the 3D multiplayer office, opened by /room (alias /3d) in a channel.
 *
 *  The Three.js game lives in three/game3d.js (imperative, one instance per
 *  open); this module is the vanilla-DOM shell around it: the overlay, the HUD
 *  (room label, chat, roster, emotes), the whiteboards, and the voice-note
 *  wall. It lives outside #app on purpose — paint() rebuilds #app's innerHTML
 *  on every event, and a WebGL canvas that is torn down per keystroke is not a
 *  game. Presence rides room.* frames relayed to the channel's members; the
 *  server stamps identity, clients drop their own echo.
 *
 *  Three.js and everything here arrive via dynamic import from client.ts, so
 *  none of it is in the first paint — the Monaco deal (codepad.ts).
 */

import { ROOMS } from "./engine.js";
import { createRoom0Game, EMOTES } from "./three/game3d.js";
import { LOOKS, parseLook, lookToString, hashString } from "./three/avatar3d.js";
import { startVoiceRecording, MAX_NOTE_MS } from "./voiceNotes.js";
import { setIceServers } from "./three/voice3d.js";
import { esc } from "../escape.ts";
import type { RoomCtx } from "../client.ts";
import type { Frame } from "../protocol";
import "./room.css";

type VoiceNote = {
  id: string;
  room: string;
  duration_ms: number;
  created_at: string;
  author_id: string;
  author_name: string;
  audio_url: string;
};

type Lane = { id: string; title: string; notes: { id: string; title: string; body: string; color: number }[] };

// ---- per-viewer prefs (character, sound, played notes) ----

const LS = "gliph.room0";
type Prefs = { character: string | null; ambientMuted: boolean; playedVnotes: string[]; pos: { channel: string; room: string; x: number; z: number } | null };
const loadPrefs = (): Prefs => {
  try {
    const p = JSON.parse(localStorage.getItem(LS) ?? "{}");
    return { character: p.character ?? null, ambientMuted: !!p.ambientMuted, playedVnotes: p.playedVnotes ?? [], pos: p.pos ?? null };
  } catch {
    return { character: null, ambientMuted: false, playedVnotes: [], pos: null };
  }
};
const savePrefs = (p: Prefs) => {
  try { localStorage.setItem(LS, JSON.stringify(p)); } catch { /* quota: keep playing */ }
};

// ---- little formatters (ported from the voice wall) ----

const fmtDuration = (ms: number) => {
  const s = Math.max(1, Math.round(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60 ? ` ${s % 60}s` : ""}`;
};
const fmtAge = (iso: string) => {
  const mins = Math.floor((Date.now() - new Date(iso).getTime()) / 60000);
  if (mins < 1) return "now";
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
};

const NOTE_COLORS = ["#f7e463", "#f9a8c9", "#8fd0ff", "#9fe6a0", "#ffb36b", "#c9a8f9"];
/** A sticky's colour is an index off the wire, so it picks from the palette by
 *  position or it gets the first one — never `NOTE_COLORS[whatever]`, which for
 *  a string index hands back an Array method and prints it into the style. */
const noteColor = (i: unknown): string =>
  NOTE_COLORS[Number.isInteger(i) ? (i as number) : -1] ?? NOTE_COLORS[0]!;
const uid = () => Math.random().toString(36).slice(2, 10);
const EMBED_ORIGIN = "https://embed.diagrams.net";
const EMBED_URL = `${EMBED_ORIGIN}/?embed=1&proto=json&spin=1&ui=min&noSaveBtn=1`;

let current: { channel: string; close: () => void } | null = null;
export const roomOpen = () => current !== null;

export function openRoom(ctx: RoomCtx, channel: string) {
  if (!channel) return;
  if (current) {
    if (current.channel === channel) return;
    current.close();
  }
  const user = ctx.user();
  if (!user) return;
  const selfKey: string = user.id;
  const screenName: string = user.nick;
  const prefs = loadPrefs();
  const playedSet = new Set(prefs.playedVnotes);
  // TURN-capable ICE from the server — the same coturn-backed /api/ice list
  // the p2p transfers use. Voice keeps its public-STUN fallback if this fails.
  void ctx.ice().then((list) => { if (list) setIceServers(list); });
  const isCoarse = window.matchMedia?.("(pointer: coarse)")?.matches ?? false;

  // ---- HTTP (multipart + blobs, so not client.request) ----

  const apiFetch = async (path: string, opts: RequestInit = {}) => {
    const res = await fetch(ctx.api(path), {
      ...opts,
      headers: { Authorization: `Bearer ${ctx.token()}`, ...(opts.headers ?? {}) },
    });
    if (!res.ok) throw new Error(`room api ${res.status}`);
    return res;
  };
  const getBoard = (kind: string) =>
    apiFetch(`/api/room/board?channel=${encodeURIComponent(channel)}&kind=${kind}`).then((r) => r.json());
  const saveBoard = (kind: string, data: unknown) =>
    apiFetch(`/api/room/board?channel=${encodeURIComponent(channel)}&kind=${kind}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${ctx.token()}` },
      body: JSON.stringify({ data }),
    });

  // ---- the overlay ----

  const root = document.createElement("div");
  root.className = "overlay r0";
  root.setAttribute("data-overlay-open", "");
  root.innerHTML = `<div class="stage">
    <div class="canvasWrap" data-r="canvas"></div>
    <div class="loading" data-r="loading">Entering room0…</div>
    <div class="roomLabel" data-r="roomLabel" hidden></div>
    <button type="button" class="closeBtn" data-r="close" aria-label="Leave room0" title="leave room0 (Esc)">✕</button>
    <button type="button" class="muteBtn" data-r="mute" hidden></button>
    <button type="button" class="quietBtn" data-r="quiet" hidden></button>
    <button type="button" class="lookBtn" data-r="lookBtn" aria-label="Customize your character" title="customize character" hidden>👕</button>
    <div class="lookPanel" data-r="lookPanel" hidden></div>
    <button type="button" class="voiceBtn" data-r="voice" hidden>🎙️<span class="voiceCount" data-r="voiceCount" hidden></span></button>
    <button type="button" class="micBtn" data-r="mic" hidden></button>
    <div class="prompt" data-r="prompt" hidden></div>
    <div class="nearHint" data-r="nearHint" hidden></div>
    <div class="chatLog" data-r="chatLog"></div>
    <div class="roster" data-r="roster" hidden></div>
    <button type="button" class="rosterPill" data-r="rosterPill" hidden></button>
    <div class="boardModal" data-r="boardModal" hidden></div>
    <div class="tvModal" data-r="tvModal" hidden></div>
    <form class="chatForm" data-r="chatForm" hidden>
      <input class="chatInput" data-r="chatInput" maxlength="200" placeholder="Say something…" autocomplete="off">
      <span class="chatWhere" data-r="chatWhere"></span>
      <button type="button" class="chatClose" data-r="chatClose" aria-label="Close chat">×</button>
    </form>
    <div class="voiceWall" data-r="wall" hidden></div>
    <div class="recPill" data-r="rec" hidden></div>
    <div class="emoteBar" data-r="emoteBar" hidden></div>
    <div class="touchCluster" data-r="touch" hidden></div>
    <div class="hint" data-r="hint" hidden></div>
  </div>`;
  document.body.append(root);
  // Take the keyboard off the composer. The room refuses every key while an
  // input is focused (game3d's editable() guard), and /room0 is typed in the
  // composer, so without this the room opens deaf until you click something.
  root.tabIndex = -1;
  root.focus();
  const el = <T extends HTMLElement = HTMLElement>(name: string) =>
    root.querySelector<T>(`[data-r="${name}"]`)!;

  // ---- state the HUD renders ----

  let ready = false;
  let fatal: string | null = null;
  // Where you stood last time survives closing the room (per channel).
  const savedPos = prefs.pos && prefs.pos.channel === channel && (ROOMS as Record<string, unknown>)[prefs.pos.room] ? prefs.pos : null;
  let roomId = savedPos?.room ?? "office";
  let prompt: string | null = null;
  let nearPeer: string | null = null;
  let chatOpen = false;
  let lookOpen = false;
  let boardOpen: "tasks" | "drawing" | null = null;
  let watchTv = false;
  let rosterOpen = true;
  let selfAway = false;
  let peers: { id: string; name: string; room: string; away?: boolean }[] = [];
  let voiceState = { active: false, micMuted: false, connected: 0 };
  let notesByRoom: Record<string, VoiceNote[]> = {};
  let playing: { id: string; t: number } | null = null;
  let rec: { elapsed: number } | null = null;
  let recorder: { ctrl: { stop: () => Promise<{ blob: Blob; durationMs: number; filename: string } | null>; cancel: () => void }; room: string; timer: ReturnType<typeof setInterval> } | null = null;
  let wallAudio: HTMLAudioElement | null = null;
  const audioUrls = new Map<string, string>();
  let closed = false;

  const quiet = () => !!(ROOMS as Record<string, { quiet?: boolean; name?: string }>)[roomId]?.quiet;

  // ---- targeted renders ----

  const sync = () => {
    el("loading").hidden = ready || !!fatal;
    if (fatal) { el("loading").hidden = false; el("loading").textContent = fatal; }
    el("roomLabel").hidden = !ready;
    el("mute").hidden = !ready;
    el("mute").textContent = prefs.ambientMuted ? "🔇" : "🔊";
    el("mute").title = `${prefs.ambientMuted ? "unmute" : "mute"} ambience (M)`;
    el("lookBtn").hidden = !ready;
    el("lookPanel").hidden = !ready || !lookOpen;
    const v = el("voice");
    v.hidden = !ready || quiet();
    v.className = `voiceBtn ${voiceState.active ? "voiceBtnOn" : ""}`;
    v.title = voiceState.active ? `leave voice — ${roomName()} (V)` : `join voice — ${roomName()} (V)`;
    const vc = el("voiceCount");
    vc.hidden = !(voiceState.active && voiceState.connected > 0);
    vc.textContent = String(voiceState.connected + 1);
    const mic = el("mic");
    mic.hidden = !ready || !voiceState.active;
    mic.textContent = voiceState.micMuted ? "🤐" : "🎤";
    mic.title = voiceState.micMuted ? "unmute mic" : "mute mic";
    el("prompt").hidden = !(ready && prompt && !chatOpen && !isCoarse);
    el("prompt").textContent = prompt ?? "";
    el("nearHint").hidden = !(ready && nearPeer && !chatOpen);
    el("nearHint").textContent = nearPeer ? `Press T to chat with ${nearPeer}` : "";
    el("roster").hidden = !(ready && rosterOpen);
    el("rosterPill").hidden = !(ready && !rosterOpen);
    el("boardModal").hidden = !boardOpen;
    el("tvModal").hidden = !watchTv;
    el("chatForm").hidden = !(chatOpen && !watchTv);
    el("chatWhere").textContent = ctx.quiet() ? "quiet" : `#${channelName()} hears it too`;
    syncQuiet();
    el("rec").hidden = !rec;
    el("emoteBar").hidden = !(ready && !chatOpen);
    el("touch").hidden = !(ready && isCoarse && !chatOpen);
    el("hint").hidden = !ready;
    const fab = el("touch").querySelector<HTMLElement>("[data-act='interact']");
    if (fab) { fab.hidden = !prompt; fab.textContent = (prompt ?? "").replace(/^F — /, ""); }
    renderWall();
  };

  const roomName = () => (ROOMS as Record<string, { name?: string }>)[roomId]?.name ?? "";

  let chatSeq = 0;
  const appendChat = (m: {
    system?: boolean; self?: boolean; name?: string; text: string; chan?: boolean; faded?: boolean;
  }) => {
    const log = el("chatLog");
    const div = document.createElement("div");
    div.className = `chatLogEntry ${m.system ? "chatLogSystem" : ""} ${m.self ? "chatLogSelf" : ""}` +
      `${m.faded ? " chatLogWas" : ""}`;
    div.dataset["seq"] = String(++chatSeq);
    // A line typed in the channel window rather than said in here wears the
    // channel's name, so the two are never confused for each other.
    const chip = m.chan ? `<span class="chatLogChip">[#${esc(channelName())}]</span> ` : "";
    if (m.system) div.textContent = m.text;
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- name, text and channel name are esc()'d
    else div.innerHTML = `${chip}<span class="chatLogName">${esc(m.name ?? "")}</span> ${esc(m.text)}`;
    log.append(div);
    while (log.children.length > 8) log.firstElementChild?.remove();
  };

  const channelName = () => ctx.channelName(channel);

  // Quiet is the client's own switch, flipped through the same command anyone
  // can type. The button only shows what the core already decided.
  const syncQuiet = () => {
    const on = ctx.quiet();
    const b = el("quiet");
    b.hidden = !ready;
    b.className = `quietBtn ${on ? "quietBtnOn" : ""}`;
    b.textContent = on ? "quiet" : "loud";
    b.title = on
      ? "your lines stay out of #" + channelName() + " · click to speak up"
      : "#" + channelName() + " hears you · click to go quiet";
  };

  // What the log has already shown. The room reads the channel through the
  // core — decrypted, quiet lines included — rather than reading frames off
  // the socket, so there is exactly one path in and it is the same one the
  // stream uses.
  const shown = new Set<string>();
  const drawLog = (faded: boolean) => {
    for (const row of ctx.log(channel, 8)) {
      if (shown.has(row.id)) continue;
      shown.add(row.id);
      if (row.kind !== "message" && row.kind !== "action") continue;
      const said = String(row.meta?.["room"] ?? "");
      const mine = row.actor === screenName;
      const text = row.text ?? "";
      if (!text) continue;
      // Someone talking in this very room gets a bubble over their head; the
      // local sim already drew ours, and other rooms are log-only.
      if (!faded && !mine && said === roomId) game.sayAs(row.actor, text);
      if (mine && said) continue; // our own line: the sim logged it already
      appendChat({ name: row.actor, text, self: mine, chan: !said, faded });
    }
  };
  const systemLog = (text: string) => appendChat({ system: true, text });

  const renderRoster = () => {
    const members = ctx.members(channel);
    const rows: { id: string; name: string; status: string; sub?: string }[] = [{
      id: selfKey,
      name: `${screenName} (you)`,
      status: selfAway ? "away" : "online",
      sub: roomName(),
    }];
    const online = new Set([selfKey]);
    for (const p of [...peers].sort((a, b) => a.name.localeCompare(b.name))) {
      online.add(p.id);
      rows.push({
        id: p.id, name: p.name,
        status: p.away ? "away" : "online",
        sub: p.away ? "away" : (ROOMS as Record<string, { name?: string }>)[p.room]?.name,
      });
    }
    for (const m of members.filter((x) => !online.has(x.user_id)).sort((a, b) => a.nick.localeCompare(b.nick))) {
      rows.push({ id: m.user_id, name: m.nick, status: "offline", sub: "offline" });
    }
    const count = rows.filter((r) => r.status !== "offline").length;
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- names, ids and subs esc()'d, count is a number
    el("roster").innerHTML = `<div class="rosterHead">
        <span class="rosterTitle">Team</span>
        <span class="rosterCount">${count} online</span>
        <button type="button" class="rosterClose" data-act="roster-hide" aria-label="Hide team panel">×</button>
      </div>
      <div class="rosterList">${rows.map((r) => {
        const dot = `<span class="dot dot_${r.status}"></span>`;
        const body = `${dot}<span class="rosterName">${esc(r.name)}</span>${r.sub ? `<span class="rosterSub">${esc(r.sub)}</span>` : ""}`;
        return r.status !== "offline" && r.id !== selfKey
          ? `<button type="button" class="rosterRow rosterRowVisit" data-visit="${esc(r.id)}" title="Walk over to ${esc(r.name)}">${body}</button>`
          : `<div class="rosterRow">${body}</div>`;
      }).join("")}</div>
      <div class="rosterFoot">click a coworker to walk over</div>`;
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- count is a number
    el("rosterPill").innerHTML = `<span class="dot dot_online"></span> ${count} online`;
  };

  const renderWall = () => {
    const notes = notesByRoom[roomId] ?? [];
    const wall = el("wall");
    wall.hidden = !ready || notes.length === 0;
    if (wall.hidden) { wall.innerHTML = ""; return; }
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- note ids and names esc()'d, ages and durations are numbers
    wall.innerHTML = `<div class="voiceWallTitle">🎙️ voice wall</div>
      <div class="voiceWallList">${notes.map((n) => {
        const isPlaying = playing?.id === n.id;
        const fresh = !playedSet.has(n.id) && n.author_id !== selfKey;
        const pct = isPlaying ? `${Math.round((playing?.t ?? 0) * 100)}%` : "0%";
        return `<div class="voiceNote ${isPlaying ? "voiceNotePlaying" : ""}">
          <button type="button" class="voiceNotePlay" data-play="${esc(n.id)}" aria-label="${isPlaying ? "Stop" : "Play"} ${esc(n.author_name)}'s voice note">${isPlaying ? "⏸" : "▶"}</button>
          <div class="voiceNoteBody">
            <div class="voiceNoteMeta">
              <span class="voiceNoteName">${n.author_id === selfKey ? "you" : esc(n.author_name)}</span>
              <span class="voiceNoteAge">${fmtAge(n.created_at)} · ${fmtDuration(n.duration_ms)}</span>
            </div>
            <div class="voiceNoteTrack"><div class="voiceNoteProgress" style="--vn-progress:${pct}"></div></div>
          </div>
          ${fresh ? '<span class="voiceNoteFresh" aria-label="Unplayed"></span>' : ""}
          ${n.author_id === selfKey ? `<button type="button" class="voiceNoteDelete" data-del="${esc(n.id)}" aria-label="Delete your voice note">×</button>` : ""}
        </div>`;
      }).join("")}</div>`;
  };

  // ---- the game ----

  // game3d.js is untyped JS: TS infers its option types from `= null` defaults
  // and its return as nullable, neither of which is the real contract.
  const game: any = createRoom0Game({
    container: el("canvas"),
    roomId,
    screenName,
    selfKey,
    look: prefs.character,
    ambientMuted: prefs.ambientMuted,
    // Seed for the procedural worlds (sea/dunes/camp/space/terminal): FNV-1a
    // over the channel id, so every member generates the same world.
    worldSeed: [...channel].reduce((h, ch) => Math.imul(h ^ ch.charCodeAt(0), 16777619) >>> 0, 2166136261 >>> 0),
    spawnAt: savedPos ? { x: savedPos.x, z: savedPos.z } : null,
    classes: {
      overhead: "overhead", bubble: "bubble", nameTag: "nameTag", nameTagSelf: "nameTagSelf",
      nameTagAway: "nameTagAway", bubbleTyping: "bubbleTyping", emojiFloat: "emojiFloat", micBadge: "micBadge",
    },
    callbacks: {
      onReady: () => { ready = true; el("roomLabel").textContent = roomName(); sync(); renderRoster(); },
      onError: (msg: string) => { fatal = msg; sync(); },
      onRoomChange: (id: string) => {
        roomId = id;
        el("roomLabel").textContent = roomName();
        void fetchWall(id);
        sync(); renderRoster();
      },
      onPrompt: (p: string | null) => { prompt = p; sync(); },
      onNearPeer: (p: string | null) => { nearPeer = p; sync(); },
      onOpenChat: () => openChat(),
      onPeers: (p: typeof peers) => { peers = p; renderRoster(); },
      onSelfAway: (away: boolean) => { selfAway = away; renderRoster(); },
      onToggleRoster: () => { rosterOpen = !rosterOpen; sync(); },
      onSoundChange: (muted: boolean) => { prefs.ambientMuted = muted; savePrefs(prefs); sync(); },
      onVoice: (s: typeof voiceState) => { voiceState = s; sync(); },
      onRecordVoice: () => void toggleRecording(),
      onVoiceNote: (d: { op: string; note?: VoiceNote; id?: string; room?: string }) => {
        if (d.op === "new" && d.note) {
          const list = notesByRoom[d.note.room] ?? [];
          if (!list.some((n) => n.id === d.note!.id)) {
            notesByRoom[d.note.room] = [d.note, ...list].slice(0, 50);
          }
        } else if (d.op === "del" && d.id && d.room) {
          notesByRoom[d.room] = (notesByRoom[d.room] ?? []).filter((n) => n.id !== d.id);
        }
        renderWall();
      },
      onWatchTv: () => {
        watchTv = true;
        game.setChatOpen(true); // freeze game input while watching
        el("tvModal").innerHTML = `<div class="tvBox">
          <div class="tvHead">
            <span class="tvTitle">📺 STS-1 — the first Space Shuttle launch, April 12 1981</span>
            <button type="button" class="tvClose" data-act="tv-close" aria-label="Turn off TV">×</button>
          </div>
          <iframe class="tvFrame" src="https://www.youtube-nocookie.com/embed/pcJZrBDH40s?autoplay=1" title="STS-1: the first Space Shuttle launch" allow="autoplay; encrypted-media; fullscreen" allowfullscreen></iframe>
        </div>`;
        sync();
      },
      onOpenBoard: (kind: "tasks" | "drawing") => {
        boardOpen = kind;
        game.setChatOpen(true); // freeze game input while the board is open
        if (kind === "tasks") openTasksBoard(); else openDrawingBoard();
        sync();
      },
      onChatLog: (m: { self?: boolean; name?: string; text: string }) => appendChat(m),
    },
  } as never);

  // ---- presence over room.* frames ----

  const sendRoom = (type: string, payload: Record<string, unknown>) =>
    ctx.send({ t: "room.send", d: { channel, type, ...payload } });
  const join = () => ctx.send({ t: "room.join", d: { channel, ...game.getSelfState() } });
  join();
  game.setPresence({
    move: (p: object) => sendRoom("move", p as Record<string, unknown>),
    announce: (p: object) => sendRoom("announce", p as Record<string, unknown>),
    // Chat is not a relay any more: it is a channel message carrying the
    // sub-room it was said in. Same wire, same history, same search.
    chat: (p: object) => ctx.say(String((p as { text?: string }).text ?? ""), roomId),
    object: (p: object) => sendRoom("object", p as Record<string, unknown>),
    emote: (p: object) => sendRoom("emote", p as Record<string, unknown>),
    rtc: (p: object) => sendRoom("rtc", p as Record<string, unknown>),
  });
  // Walk in and the last few lines of the channel are already there, dimmed,
  // with a rule under them. You arrive in the conversation rather than in the
  // middle of it.
  drawLog(true);
  if (shown.size) systemLog("— you walked in —");
  ctx.onChange(() => {
    drawLog(false);
    syncQuiet();
  });

  ctx.setFrames((f: Frame) => {
    if (f.t === "room.event") {
      const d = f.d ?? {};
      if (d.channel !== channel) return;
      if (d.user_id === selfKey) return; // own echo; local sim already applied
      game.handlePresenceEvent(d);
    } else if (f.t === "hello") {
      // The socket redialled: the server forgot our join. Announce ourselves
      // again so peers get a fresh join (and re-announce back).
      join();
    }
  });

  // ---- chat ----

  function openChat() {
    chatOpen = true;
    game.setChatOpen(true);
    sync();
    el<HTMLInputElement>("chatInput").focus();
  }
  function closeChat() {
    chatOpen = false;
    game.setChatOpen(false);
    sync();
  }
  el("chatForm").addEventListener("submit", (e) => {
    e.preventDefault();
    const input = el<HTMLInputElement>("chatInput");
    if (input.value.trim()) game.sendChat(input.value);
    input.value = "";
    closeChat();
  });
  el("chatInput").addEventListener("keydown", (e) => {
    e.stopPropagation();
    if ((e as KeyboardEvent).key === "Escape") closeChat();
  });
  el("chatClose").addEventListener("click", closeChat);
  el("quiet").addEventListener("click", () => ctx.run("room0", "quiet"));

  // ---- voice-note wall: recording ----

  async function stopRecording(send: boolean) {
    const r = recorder;
    if (!r) return;
    recorder = null;
    clearInterval(r.timer);
    rec = null;
    sync();
    if (!send) { r.ctrl.cancel(); return; }
    const clip = await r.ctrl.stop();
    if (!clip || clip.durationMs < 1000) {
      if (clip) systemLog("voice note too short — hold it for at least a second");
      return;
    }
    try {
      const form = new FormData();
      form.set("channel", channel);
      form.set("room", r.room);
      form.set("duration_ms", String(Math.round(clip.durationMs)));
      form.set("file", clip.blob, clip.filename);
      const res = await apiFetch("/api/room/notes", { method: "POST", body: form });
      const note: VoiceNote | undefined = (await res.json())?.voice_note;
      if (note) {
        notesByRoom[note.room] = [note, ...(notesByRoom[note.room] ?? []).filter((n) => n.id !== note.id)].slice(0, 50);
        systemLog("voice note posted to the wall 🎙️");
        renderWall();
      }
    } catch {
      systemLog("voice note failed to upload");
    }
  }

  async function toggleRecording() {
    if (recorder) { void stopRecording(true); return; }
    let ctrl;
    try {
      ctrl = await startVoiceRecording();
    } catch {
      systemLog("voice notes need the microphone — permission was denied or no mic found");
      return;
    }
    const startedAt = performance.now();
    const timer = setInterval(() => {
      const elapsed = performance.now() - startedAt;
      if (elapsed >= MAX_NOTE_MS) void stopRecording(true);
      else {
        rec = { elapsed };
        // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- elapsed seconds, a number
        el("rec").innerHTML = `<span class="recDot"></span> recording… ${Math.floor(elapsed / 1000)}s
          <button type="button" class="recAction" data-act="rec-send">send ✓</button>
          <button type="button" class="recAction" data-act="rec-cancel">✕</button>`;
        el("rec").hidden = false;
      }
    }, 250);
    recorder = { ctrl, room: roomId, timer };
    rec = { elapsed: 0 };
    sync();
  }

  // ---- voice-note wall: playback (progress comes from duration_ms — webm
  // recordings report Infinity) ----

  function stopPlayback() {
    wallAudio?.pause();
    wallAudio = null;
    playing = null;
    renderWall();
  }

  async function playNote(note: VoiceNote) {
    if (wallAudio && playing?.id === note.id) { stopPlayback(); return; }
    stopPlayback();
    try {
      let url = audioUrls.get(note.id);
      if (!url) {
        const blob = await apiFetch(`/api/room/notes/${encodeURIComponent(note.id)}/audio`).then((r) => r.blob());
        url = URL.createObjectURL(blob);
        audioUrls.set(note.id, url);
      }
      const audio = new Audio(url);
      wallAudio = audio;
      playing = { id: note.id, t: 0 };
      renderWall();
      audio.ontimeupdate = () => {
        playing = { id: note.id, t: Math.min(1, (audio.currentTime * 1000) / (note.duration_ms || 1)) };
        const bar = el("wall").querySelector<HTMLElement>(".voiceNotePlaying .voiceNoteProgress");
        if (bar) bar.style.setProperty("--vn-progress", `${Math.round(playing.t * 100)}%`);
      };
      audio.onended = () => {
        if (!playedSet.has(note.id)) {
          playedSet.add(note.id);
          prefs.playedVnotes = [...prefs.playedVnotes.slice(-299), note.id];
          savePrefs(prefs);
        }
        if (wallAudio === audio) { wallAudio = null; playing = null; }
        renderWall();
      };
      await audio.play();
    } catch {
      stopPlayback();
      systemLog("could not play that voice note");
    }
  }

  async function deleteNote(note: VoiceNote) {
    if (playing?.id === note.id) stopPlayback();
    try {
      await apiFetch(`/api/room/notes/${encodeURIComponent(note.id)}`, { method: "DELETE" });
      notesByRoom[note.room] = (notesByRoom[note.room] ?? []).filter((n) => n.id !== note.id);
      renderWall();
    } catch {
      systemLog("could not delete the voice note");
    }
  }

  async function fetchWall(room: string) {
    try {
      const r = await apiFetch(`/api/room/notes?channel=${encodeURIComponent(channel)}&room=${encodeURIComponent(room)}`).then((x) => x.json());
      if (closed) return;
      notesByRoom[room] = r?.voice_notes ?? [];
      renderWall();
    } catch { /* the wall refetches on the next room change */ }
  }
  void fetchWall(roomId);

  el("wall").addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    const play = t.closest<HTMLElement>("[data-play]");
    const del = t.closest<HTMLElement>("[data-del]");
    const notes = notesByRoom[roomId] ?? [];
    if (play) { const n = notes.find((x) => x.id === play.dataset["play"]); if (n) void playNote(n); }
    if (del) { const n = notes.find((x) => x.id === del.dataset["del"]); if (n) void deleteNote(n); }
  });

  // ---- character customizer ----

  const renderLookPanel = () => {
    const cfg = parseLook(prefs.character, hashString(`self:${screenName}`)) as Record<string, number>;
    const hex = (c: number) => `#${c.toString(16).padStart(6, "0")}`;
    const swatchRow = (label: string, colors: number[], field: string) =>
      `<div class="lookRow"><span class="lookLabel">${label}</span><div class="lookSwatches">${colors.map((c, i) =>
        `<button type="button" style="--sw:${hex(c)}" class="lookSwatch ${cfg[field] === i ? "lookSwatchOn" : ""}" data-look="${field}:${i}" aria-label="${label} color ${i + 1}"></button>`).join("")}</div></div>`;
    const pillRow = (label: string, names: string[], field: string) =>
      `<div class="lookRow"><span class="lookLabel">${label}</span><div class="lookPills">${names.map((n, i) =>
        `<button type="button" class="lookPill ${cfg[field] === i ? "lookPillOn" : ""}" data-look="${field}:${i}">${esc(n)}</button>`).join("")}</div></div>`;
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- look names esc()'d, swatches and fields are constants
    el("lookPanel").innerHTML = `<div class="lookHead">
        <span class="lookTitle">Your character</span>
        <button type="button" class="lookClose" data-act="look-close" aria-label="Close customizer">×</button>
      </div>
      ${pillRow("Head", (LOOKS.heads as { name: string }[]).map((o) => o.name), "head")}
      ${pillRow("Outfit", LOOKS.torsos as string[], "torso")}
      ${swatchRow("Shirt", LOOKS.shirtColors as number[], "shirtColor")}
      ${pillRow("Fit", LOOKS.pants as string[], "pants")}
      ${swatchRow("Pants", LOOKS.pantsColors as number[], "pantsColor")}
      ${swatchRow("Shoes", LOOKS.shoeColors as number[], "shoeColor")}
      ${swatchRow("Hair", LOOKS.hairColors as number[], "hairColor")}
      ${swatchRow("Skin", LOOKS.skinColors as number[], "skinColor")}
      <div class="lookFooter">
        <button type="button" class="lookAction" data-act="look-random">🎲 Randomize</button>
        <button type="button" class="lookAction lookDone" data-act="look-close">Done</button>
      </div>`;
  };
  const applyLook = (patch: Record<string, number>) => {
    const cur = parseLook(prefs.character, hashString(`self:${screenName}`));
    const next = lookToString({ ...cur, ...patch });
    prefs.character = next;
    savePrefs(prefs);
    game.setLook(next);
    renderLookPanel();
  };
  el("lookPanel").addEventListener("click", (e) => {
    const t = (e.target as HTMLElement).closest<HTMLElement>("[data-look],[data-act]");
    if (!t) return;
    const look = t.dataset["look"];
    if (look) {
      const [field, i] = look.split(":");
      applyLook({ [field!]: Number(i) });
    } else if (t.dataset["act"] === "look-random") {
      const r = (n: number) => Math.floor(Math.random() * n);
      applyLook({
        head: r((LOOKS.heads as unknown[]).length), torso: r((LOOKS.torsos as unknown[]).length),
        shirtColor: r((LOOKS.shirtColors as unknown[]).length), pants: r((LOOKS.pants as unknown[]).length),
        pantsColor: r((LOOKS.pantsColors as unknown[]).length), shoeColor: r((LOOKS.shoeColors as unknown[]).length),
        hairColor: r((LOOKS.hairColors as unknown[]).length), skinColor: r((LOOKS.skinColors as unknown[]).length),
      });
    } else if (t.dataset["act"] === "look-close") {
      lookOpen = false;
      sync();
    }
  });

  // ---- whiteboards ----

  let boardCleanup: (() => void) | null = null;
  function closeBoard() {
    boardCleanup?.();
    boardCleanup = null;
    boardOpen = null;
    el("boardModal").innerHTML = "";
    game.setChatOpen(false);
    sync();
  }

  const boardShell = (title: string) => `<div class="board" data-r="board">
    <div class="boardHead">
      <span class="boardTitle">${title}</span>
      <span class="boardError" data-r="boardError" hidden></span>
      <button type="button" class="boardMaxBtn" data-act="board-max" aria-label="Maximize" title="maximize">⛶</button>
      <button type="button" class="boardClose" data-act="board-close" aria-label="Close the board">×</button>
    </div>
    <div data-r="boardBody" class="boardBody"></div>
  </div>`;
  const boardError = (msg: string | null) => {
    const e = el("boardError");
    e.hidden = !msg;
    e.textContent = msg ?? "";
  };
  el("boardModal").addEventListener("click", (e) => {
    const t = (e.target as HTMLElement).closest<HTMLElement>("[data-act]");
    if (!t) return;
    if (t.dataset["act"] === "board-close") closeBoard();
    if (t.dataset["act"] === "board-max") {
      const b = el("board");
      b.classList.toggle("boardMax");
      t.textContent = b.classList.contains("boardMax") ? "🗗" : "⛶";
    }
  });

  // The left office whiteboard: a Trello-style sticky board, debounced
  // last-writer-wins through the room boards API.
  function openTasksBoard() {
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- constant board title
    el("boardModal").innerHTML = boardShell("📌 This week");
    const body = el("boardBody");
    body.innerHTML = `<div class="boardLoading">Loading the board…</div>`;
    let lanes: Lane[] | null = null;
    let saveTimer: ReturnType<typeof setTimeout> | null = null;
    const defaultLanes = (): Lane[] => [
      { id: uid(), title: "To do", notes: [] },
      { id: uid(), title: "In progress", notes: [] },
      { id: uid(), title: "Done", notes: [] },
    ];
    const scheduleSave = () => {
      if (saveTimer) clearTimeout(saveTimer);
      saveTimer = setTimeout(() => {
        saveBoard("tasks", { lanes })
          .then(() => boardError(null))
          .catch(() => boardError("save failed — retrying on next change"));
      }, 800);
    };
    let drag: { noteId: string; laneId: string } | null = null;
    const render = () => {
      if (!lanes) return;
      // Every id here came off the wire: the board is one JSON blob any member
      // of the channel can PUT, and the server stores it without looking
      // inside. So the ids are escaped exactly like the titles, and the colour
      // is an index we pick, never a value we print.
      // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- lane and note ids, titles and bodies esc()'d, colours via noteColor()
      body.innerHTML = `<div class="lanes">${lanes.map((lane) => `<div class="lane" data-lane="${esc(lane.id)}">
          <div class="laneHead">
            <input class="laneTitle" data-lane-title="${esc(lane.id)}" value="${esc(lane.title)}" aria-label="Lane title">
            ${lane.notes.length === 0 ? `<button type="button" class="laneDelete" data-lane-del="${esc(lane.id)}" aria-label="Delete empty lane">×</button>` : ""}
          </div>
          ${lane.notes.map((note) => `<div class="note" style="--note-bg:${noteColor(note.color)}" draggable="true" data-note="${esc(note.id)}">
            <input class="noteTitle" data-note-title="${esc(note.id)}" placeholder="Title" value="${esc(note.title)}">
            <textarea class="noteBody" data-note-body="${esc(note.id)}" placeholder="Details…" rows="2">${esc(note.body)}</textarea>
            <div class="noteTools">
              ${NOTE_COLORS.map((c, i) => `<button type="button" style="--dot:${c}" class="noteDot ${note.color === i ? "noteDotOn" : ""}" data-note-color="${esc(note.id)}:${i}" aria-label="Sticky color ${i + 1}"></button>`).join("")}
              <button type="button" class="noteDelete" data-note-del="${esc(note.id)}" aria-label="Delete note">×</button>
            </div>
          </div>`).join("")}
          <button type="button" class="addNote" data-add-note="${esc(lane.id)}">+ Add a sticky</button>
        </div>`).join("")}
        <button type="button" class="addLane" data-act2="add-lane">+ Add lane</button>
      </div>`;
    };
    const laneOf = (noteId: string) => lanes?.find((l) => l.notes.some((n) => n.id === noteId));
    body.addEventListener("input", (e) => {
      const t = e.target as HTMLInputElement;
      if (t.dataset["laneTitle"]) { const l = lanes?.find((x) => x.id === t.dataset["laneTitle"]); if (l) { l.title = t.value; scheduleSave(); } }
      if (t.dataset["noteTitle"]) { const n = laneOf(t.dataset["noteTitle"]!)?.notes.find((x) => x.id === t.dataset["noteTitle"]); if (n) { n.title = t.value; scheduleSave(); } }
      if (t.dataset["noteBody"]) { const n = laneOf(t.dataset["noteBody"]!)?.notes.find((x) => x.id === t.dataset["noteBody"]); if (n) { n.body = t.value; scheduleSave(); } }
    });
    body.addEventListener("click", (e) => {
      const t = (e.target as HTMLElement).closest<HTMLElement>("[data-add-note],[data-lane-del],[data-note-del],[data-note-color],[data-act2]");
      if (!t || !lanes) return;
      if (t.dataset["addNote"]) {
        const l = lanes.find((x) => x.id === t.dataset["addNote"]);
        l?.notes.push({ id: uid(), title: "", body: "", color: (l.notes.length % NOTE_COLORS.length) });
      } else if (t.dataset["laneDel"]) {
        lanes = lanes.filter((l) => l.id !== t.dataset["laneDel"] || l.notes.length > 0);
      } else if (t.dataset["noteDel"]) {
        const l = laneOf(t.dataset["noteDel"]!);
        if (l) l.notes = l.notes.filter((n) => n.id !== t.dataset["noteDel"]);
      } else if (t.dataset["noteColor"]) {
        const [id, i] = t.dataset["noteColor"]!.split(":");
        const n = laneOf(id!)?.notes.find((x) => x.id === id);
        if (n) n.color = Number(i);
      } else if (t.dataset["act2"] === "add-lane") {
        lanes.push({ id: uid(), title: "New lane", notes: [] });
      } else return;
      scheduleSave();
      render();
    });
    body.addEventListener("dragstart", (e) => {
      const note = (e.target as HTMLElement).closest<HTMLElement>("[data-note]");
      if (note) drag = { noteId: note.dataset["note"]!, laneId: laneOf(note.dataset["note"]!)?.id ?? "" };
    });
    body.addEventListener("dragover", (e) => e.preventDefault());
    body.addEventListener("drop", (e) => {
      const laneEl = (e.target as HTMLElement).closest<HTMLElement>("[data-lane]");
      const d = drag;
      drag = null;
      if (!laneEl || !d || !lanes || laneEl.dataset["lane"] === d.laneId) return;
      const from = lanes.find((l) => l.id === d.laneId);
      const to = lanes.find((l) => l.id === laneEl.dataset["lane"]);
      const idx = from?.notes.findIndex((n) => n.id === d.noteId) ?? -1;
      if (from && to && idx >= 0) to.notes.push(from.notes.splice(idx, 1)[0]!);
      scheduleSave();
      render();
    });
    getBoard("tasks")
      .then((r) => {
        const stored = r?.board?.data?.lanes;
        lanes = Array.isArray(stored) && stored.length ? stored : defaultLanes();
        render();
      })
      .catch(() => { lanes = defaultLanes(); boardError("could not load the board — changes stay in this tab"); render(); });
    boardCleanup = () => { if (saveTimer) clearTimeout(saveTimer); };
  }

  // The right office whiteboard: embedded draw.io (diagrams.net). The diagram
  // XML persists per channel; autosave events debounce into PUTs.
  function openDrawingBoard() {
    // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- constant board title
    el("boardModal").innerHTML = boardShell("✏️ Sketch board");
    const body = el("boardBody");
    body.innerHTML = `<iframe class="drawFrame" src="${EMBED_URL}" title="draw.io sketch board"></iframe>`;
    boardError("Loading the sketch board…");
    const frame = body.querySelector("iframe")!;
    const xmlPromise = getBoard("drawing").then((r) => r?.board?.data?.xml ?? "").catch(() => "");
    let saveTimer: ReturnType<typeof setTimeout> | null = null;
    const post = (m: object) => frame.contentWindow?.postMessage(JSON.stringify(m), EMBED_ORIGIN);
    const save = (xml: unknown) => {
      if (typeof xml !== "string") return;
      if (saveTimer) clearTimeout(saveTimer);
      saveTimer = setTimeout(() => {
        saveBoard("drawing", { xml })
          .then(() => boardError(null))
          .catch(() => boardError("save failed — will retry on the next stroke"));
      }, 1200);
    };
    const onMsg = (e: MessageEvent) => {
      if (e.origin !== EMBED_ORIGIN || typeof e.data !== "string") return;
      let msg;
      try { msg = JSON.parse(e.data); } catch { return; }
      if (msg.event === "init") {
        void xmlPromise.then((xml) => { post({ action: "load", autosave: 1, xml: xml || "" }); boardError(null); });
      } else if (msg.event === "autosave") save(msg.xml);
      else if (msg.event === "save") { save(msg.xml); post({ action: "status", modified: false }); }
      else if (msg.event === "exit") closeBoard();
    };
    window.addEventListener("message", onMsg);
    boardCleanup = () => {
      window.removeEventListener("message", onMsg);
      if (saveTimer) clearTimeout(saveTimer);
    };
  }

  // ---- static HUD wiring ----

  el("mute").addEventListener("click", (e) => { game.toggleSound(); (e.currentTarget as HTMLElement).blur(); });
  el("lookBtn").addEventListener("click", (e) => { lookOpen = !lookOpen; if (lookOpen) renderLookPanel(); sync(); (e.currentTarget as HTMLElement).blur(); });
  el("voice").addEventListener("click", (e) => { game.toggleVoice(); (e.currentTarget as HTMLElement).blur(); });
  el("mic").addEventListener("click", (e) => { game.toggleMic(); (e.currentTarget as HTMLElement).blur(); });
  el("close").addEventListener("click", () => close());
  el("roster").addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    if (t.closest("[data-act='roster-hide']")) { rosterOpen = false; sync(); return; }
    const visit = t.closest<HTMLElement>("[data-visit]");
    if (visit) { game.visitPeer(visit.dataset["visit"]); visit.blur(); }
  });
  el("rosterPill").addEventListener("click", () => { rosterOpen = true; sync(); });
  el("tvModal").addEventListener("click", (e) => {
    if ((e.target as HTMLElement).closest("[data-act='tv-close']")) closeTv();
  });
  function closeTv() {
    watchTv = false;
    el("tvModal").innerHTML = ""; // removing the iframe stops playback
    game.setChatOpen(false);
    sync();
  }
  el("rec").addEventListener("click", (e) => {
    const t = (e.target as HTMLElement).closest<HTMLElement>("[data-act]");
    if (t?.dataset["act"] === "rec-send") void stopRecording(true);
    if (t?.dataset["act"] === "rec-cancel") void stopRecording(false);
  });

  // nosemgrep: javascript.browser.security.insecure-innerhtml, javascript.browser.security.insecure-document-method -- EMOTES is a constant table
  el("emoteBar").innerHTML = `<button type="button" class="emoteBtn" data-act="chat" title="chat (T)">💬</button>
    <button type="button" class="emoteBtn" data-act="record" title="voice note (R)">🎙️</button>
    ${(EMOTES as { kind: string; emoji: string }[]).map((e, i) =>
      `<button type="button" class="emoteBtn" data-emote="${e.kind}" title="${e.kind} (${i + 1})">${e.emoji}</button>`).join("")}`;
  el("emoteBar").addEventListener("click", (e) => {
    const t = (e.target as HTMLElement).closest<HTMLElement>("[data-act],[data-emote]");
    if (!t) return;
    if (t.dataset["act"] === "chat") openChat();
    else if (t.dataset["act"] === "record") void toggleRecording();
    else if (t.dataset["emote"]) game.emote(t.dataset["emote"]);
    t.blur();
  });

  el("touch").innerHTML = `<button type="button" class="fabBtn fabAction" data-act="interact" hidden></button>
    <div class="touchRow">
      <button type="button" class="fabBtn" data-act="rot-left" aria-label="Rotate camera left">⟲</button>
      <button type="button" class="fabBtn" data-act="jump" aria-label="Jump">⇧</button>
      <button type="button" class="fabBtn" data-act="rot-right" aria-label="Rotate camera right">⟳</button>
    </div>`;
  el("touch").addEventListener("click", (e) => {
    const t = (e.target as HTMLElement).closest<HTMLElement>("[data-act]");
    if (!t) return;
    if (t.dataset["act"] === "interact") game.interact();
    else if (t.dataset["act"] === "rot-left") game.rotateCamera(1);
    else if (t.dataset["act"] === "rot-right") game.rotateCamera(-1);
    else if (t.dataset["act"] === "jump") game.jump();
    t.blur();
  });
  el("hint").textContent = isCoarse
    ? "tap to walk · hold to steer · pinch to zoom · tap items to pick up"
    : "WASD or click to walk · Shift run · Space jump (chain hops to go faster) · F interact · T chat · 1–5 react · V voice · R voice note · P team · M sound · Q/E rotate";

  // Esc unwinds: customizer → board → TV → chat (its own handler) → the room.
  const onKey = (e: KeyboardEvent) => {
    if (e.key !== "Escape") return;
    if (lookOpen) { lookOpen = false; sync(); }
    else if (boardOpen) closeBoard();
    else if (watchTv) closeTv();
    else if (!chatOpen) close();
  };
  window.addEventListener("keydown", onKey);

  // ---- teardown ----

  function close() {
    if (closed) return;
    closed = true;
    window.removeEventListener("keydown", onKey);
    ctx.setFrames(null);
    ctx.send({ t: "room.leave", d: { channel } });
    boardCleanup?.();
    if (recorder) { clearInterval(recorder.timer); recorder.ctrl.cancel(); }
    wallAudio?.pause();
    for (const url of audioUrls.values()) URL.revokeObjectURL(url);
    audioUrls.clear();
    const s = game.getSelfState?.();
    if (s) { prefs.pos = { channel, room: s.room, x: s.x, z: s.y }; savePrefs(prefs); }
    game.dispose();
    root.remove();
    current = null;
  }

  current = { channel, close };
  sync();
  renderRoster();
}
