// Live playback via MediaSource Extensions, fed by the embedded engine's
// chunked-HTTP fMP4 stream (/api/camera/{id}/live/stream). Used instead of
// hls.js when a camera exposes `live_mse` (embedded-engine mode). Latency is
// ~1-2s. Returns a handle with .destroy() so wall teardown treats it like an
// Hls instance.
//
// Reconnects automatically: a flaky/far camera drops the source periodically,
// which resets the broadcaster and ends this HTTP stream. Instead of going
// black, we rebuild the pipeline and retry until .destroy() is called.
import { makeMsg } from "../util/dom.js";
import { token } from "../api.js";

const TARGET = 1.2;          // seconds of latency to hold at the live edge
const RECONNECT_MS = 1500;   // wait before retrying after the source drops

function authHeaders() {
  const t = token();
  return t ? { Authorization: `Bearer ${t}` } : {};
}

// attachMse wires `video` to the camera's live MSE stream, reconnecting on
// drops. Returns a handle { destroy() } (or null if MSE is unsupported).
export function attachMse(cam, video) {
  if (typeof MediaSource === "undefined") {
    video.replaceWith(makeMsg("Live requires MediaSource support"));
    return null;
  }

  const infoUrl = `/api/camera/${encodeURIComponent(cam.id)}/live/info`;
  const streamUrl = `/api/camera/${encodeURIComponent(cam.id)}/live/stream`;

  let destroyed = false;
  let abort = null;
  let timer = null;      // latency-control interval for the current connection
  let retry = null;      // pending reconnect timeout
  let objectUrl = null;

  const clearConn = () => {
    if (abort) { try { abort.abort(); } catch {} abort = null; }
    if (timer) { clearInterval(timer); timer = null; }
    if (objectUrl) { try { URL.revokeObjectURL(objectUrl); } catch {} objectUrl = null; }
  };

  const destroy = () => {
    destroyed = true;
    if (retry) { clearTimeout(retry); retry = null; }
    clearConn();
    try { video.pause(); video.playbackRate = 1.0; video.removeAttribute("src"); video.load(); } catch {}
  };

  const scheduleReconnect = () => {
    if (destroyed) return;
    clearConn();
    retry = setTimeout(() => { retry = null; connect(); }, RECONNECT_MS);
  };

  async function connect() {
    if (destroyed) return;
    abort = new AbortController();
    const signal = abort.signal;

    let info;
    try {
      const r = await fetch(infoUrl, { headers: authHeaders(), signal });
      info = await r.json();
    } catch {
      scheduleReconnect(); // engine/API momentarily unreachable — keep trying
      return;
    }
    if (destroyed) return;
    if (!info.available) { scheduleReconnect(); return; } // camera reconnecting
    if (!MediaSource.isTypeSupported(info.mime)) {
      video.replaceWith(makeMsg("Live codec unsupported"));
      return; // permanent: don't retry
    }

    const ms = new MediaSource();
    objectUrl = URL.createObjectURL(ms);
    video.src = objectUrl;
    try {
      await new Promise((res, rej) => {
        ms.addEventListener("sourceopen", res, { once: true });
        signal.addEventListener("abort", rej, { once: true });
      });
    } catch { return; } // aborted (destroy/reconnect)
    if (destroyed) return;

    let sb;
    try { sb = ms.addSourceBuffer(info.mime); }
    catch { scheduleReconnect(); return; }

    const queue = [];
    let started = false;
    const pump = () => {
      if (!sb || sb.updating || !queue.length) return;
      try { sb.appendBuffer(queue.shift()); } catch {}
    };
    sb.addEventListener("updateend", () => {
      if (!sb.updating && video.buffered.length) {
        const s = video.buffered.start(0);
        const e = video.buffered.end(video.buffered.length - 1);
        if (e - s > 30) { try { sb.remove(s, e - 15); return; } catch {} }
      }
      if (!started && video.buffered.length) {
        const e = video.buffered.end(video.buffered.length - 1);
        if (e - video.buffered.start(0) >= 0.6) {
          started = true;
          video.currentTime = Math.max(video.buffered.start(0), e - TARGET);
          video.play().catch(() => {});
        }
      }
      pump();
    });

    video.addEventListener("waiting", () => {
      if (!video.buffered.length) return;
      for (let i = 0; i < video.buffered.length; i++) {
        if (video.buffered.start(i) > video.currentTime && video.buffered.start(i) - video.currentTime < 1) {
          video.currentTime = video.buffered.start(i) + 0.01;
          break;
        }
      }
    });

    timer = setInterval(() => {
      if (!started || !video.buffered.length) return;
      const behind = video.buffered.end(video.buffered.length - 1) - video.currentTime;
      if (behind > 5) { video.currentTime = video.buffered.end(video.buffered.length - 1) - TARGET; video.playbackRate = 1.0; }
      else if (behind > TARGET + 0.8) video.playbackRate = 1.08;
      else video.playbackRate = 1.0;
    }, 1000);

    try {
      const resp = await fetch(streamUrl, { headers: authHeaders(), signal });
      if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);
      const reader = resp.body.getReader();
      while (true) {
        const { done, value } = await reader.read();
        if (done || destroyed) break;
        queue.push(value);
        pump();
      }
    } catch {
      // fall through to reconnect
    }
    // stream ended (source dropped / broadcaster reset) — reconnect unless torn down
    if (!destroyed && !signal.aborted) scheduleReconnect();
  }

  connect();
  return { destroy };
}

// captureVideoFrame grabs the currently-displayed frame from an already-playing
// <video> element and returns it as a JPEG data URL, or null if the element has
// no decoded frame yet (not enough data, zero dimensions, or a tainted canvas).
// The wall calls this on its live tiles to refresh sidebar thumbnails without
// opening a second stream — the browser is already decoding the frame.
// `maxWidth` (0 = native) downscales the frame so thumbnails stay small.
export function captureVideoFrame(video, { maxWidth = 0, quality = 0.7 } = {}) {
  if (!video || video.readyState < 2 || !video.videoWidth || !video.videoHeight) {
    return null;
  }
  try {
    let w = video.videoWidth;
    let h = video.videoHeight;
    if (maxWidth > 0 && w > maxWidth) {
      h = Math.round((h * maxWidth) / w);
      w = maxWidth;
    }
    const canvas = document.createElement("canvas");
    canvas.width = w;
    canvas.height = h;
    canvas.getContext("2d").drawImage(video, 0, 0, w, h);
    return canvas.toDataURL("image/jpeg", quality);
  } catch {
    return null;
  }
}
