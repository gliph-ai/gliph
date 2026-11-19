// room0 voice notes — the MediaRecorder side (docs/Room0-Voice.md). Records a
// short mic clip and hands back {blob, durationMs, filename} for the API
// upload. Chromium/Firefox record webm/opus; Safari records mp4 (AAC) — the
// server accepts both and every browser can play both back.

const MIME_CANDIDATES = [
  'audio/webm;codecs=opus',
  'audio/webm',
  'audio/mp4',
];

export const MAX_NOTE_MS = 60_000; // client cap — the API allows up to 120s

function pickMime() {
  if (typeof MediaRecorder === 'undefined') return null;
  return MIME_CANDIDATES.find((m) => MediaRecorder.isTypeSupported(m)) || '';
}

// Start recording. Resolves the controller once the mic is live; the returned
// stop() resolves {blob, durationMs, filename} (or null if cancelled/empty).
export async function startVoiceRecording() {
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { echoCancellation: true, noiseSuppression: true },
  });
  const mimeType = pickMime();
  const recorder = new MediaRecorder(stream, mimeType ? { mimeType } : undefined);
  const chunks = [];
  const startedAt = performance.now();
  let settled = false;

  recorder.ondataavailable = (e) => { if (e.data?.size) chunks.push(e.data); };
  recorder.start(250); // chunked so a long note never sits in one giant buffer

  const finish = () => {
    for (const t of stream.getTracks()) t.stop();
  };

  return {
    startedAt,

    stop() {
      return new Promise((resolve) => {
        if (settled) { resolve(null); return; }
        settled = true;
        recorder.onstop = () => {
          finish();
          if (!chunks.length) { resolve(null); return; }
          const type = (recorder.mimeType || mimeType || 'audio/webm').split(';')[0];
          const blob = new Blob(chunks, { type });
          const ext = type === 'audio/mp4' ? 'm4a' : type.split('/')[1] || 'webm';
          resolve({
            blob,
            durationMs: Math.round(performance.now() - startedAt),
            filename: `voice-note.${ext}`,
          });
        };
        try { recorder.stop(); } catch { finish(); resolve(null); }
      });
    },

    cancel() {
      if (settled) return;
      settled = true;
      recorder.onstop = null;
      try { recorder.stop(); } catch { /* already stopped */ }
      finish();
    },
  };
}
