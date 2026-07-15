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
import { setCamStatus } from "../ui/cam-status.js";
import { icon } from "../ui/icons.js";

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
  let reconnectCount = 0;
  const tile = video.closest(".wall-tile");
  let bufferingEl = null;

  const ensureOverlay = () => {
    if (!tile) return null;
    if (!bufferingEl) {
      bufferingEl = document.createElement("div");
      bufferingEl.className = "wall-buffering";
      bufferingEl.setAttribute("role", "status");
      bufferingEl.setAttribute("aria-live", "polite");
      bufferingEl.innerHTML = `<span class="wall-buffering-icon" aria-hidden="true">${icon("loader")}</span><span class="wall-buffering-text">Loading…</span>`;
      const overlay = tile.querySelector(".wall-overlay");
      if (overlay) tile.insertBefore(bufferingEl, overlay);
      else tile.appendChild(bufferingEl);
    }
    return bufferingEl;
  };
  const removeOverlay = () => {
    if (bufferingEl) { bufferingEl.remove(); bufferingEl = null; }
    reconnectCount = 0;
  };

  const clearConn = () => {
    if (abort) { try { abort.abort(); } catch {} abort = null; }
    if (timer) { clearInterval(timer); timer = null; }
    if (objectUrl) { try { URL.revokeObjectURL(objectUrl); } catch {} objectUrl = null; }
  };

  const destroy = () => {
    destroyed = true;
    if (retry) { clearTimeout(retry); retry = null; }
    clearConn();
    removeOverlay();
    try { video.pause(); video.playbackRate = 1.0; video.removeAttribute("src"); video.load(); } catch {}
  };

  // Reset the backoff and reconnect immediately (from the Retry button).
  const retryNow = () => {
    if (destroyed) return;
    if (retry) { clearTimeout(retry); retry = null; }
    reconnectCount = 0;
    connect();
  };

  const scheduleReconnect = () => {
    if (destroyed) return;
    clearConn();
    reconnectCount++;
    const el = ensureOverlay();
    if (reconnectCount > 3) {
      setCamStatus(cam.id, "offline");
      if (el) {
        el.classList.add("wall-connection-lost");
        el.querySelector(".wall-buffering-text").textContent = "Connection lost";
        ensureRetryButton(el);
      }
    } else {
      setCamStatus(cam.id, "connecting");
      if (el) {
        el.classList.remove("wall-connection-lost");
        el.querySelector(".wall-buffering-text").textContent = "Loading…";
      }
    }
    retry = setTimeout(() => { retry = null; connect(); }, RECONNECT_MS);
  };

  // Adds a Retry button to the connection-lost overlay (once).
  const ensureRetryButton = (el) => {
    if (el.querySelector(".wall-retry-btn")) return;
    const btn = document.createElement("button");
    btn.className = "wall-retry-btn";
    btn.type = "button";
    btn.textContent = "Retry";
    btn.addEventListener("click", (e) => { e.stopPropagation(); retryNow(); });
    el.appendChild(btn);
  };

  async function connect() {
    if (destroyed) return;
    removeOverlay();
    setCamStatus(cam.id, "connecting");
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
    if (!info.available) {
      // The camera's video codec can't be played in a browser (e.g. H265/HEVC):
      // recording and the RTSP relay still work, but MSE can't. This is
      // permanent for this camera — show a clear message and stop retrying
      // instead of spinning on "connecting" forever.
      if (info.reason === "unsupported_codec") {
        setCamStatus(cam.id, "offline");
        const codec = info.codec || "This codec";
        const msg = `${codec} can't play in the browser. Recording and the RTSP relay are active — open the RTSP stream in a compatible player (VLC, the mobile app).`;
        video.replaceWith(makeMsg(msg));
        return;
      }
      scheduleReconnect(); // camera reconnecting
      return;
    }
    if (!MediaSource.isTypeSupported(info.mime)) {
      // The stream is advertised with its real codec string, but THIS browser
      // can't decode it in MediaSource. For H265/HEVC that is browser/hardware
      // dependent (Safari yes; Firefox/Chrome only with a system HEVC decoder),
      // so give a clear message pointing at the RTSP relay rather than a generic
      // "unsupported". Permanent for this browser — don't retry.
      setCamStatus(cam.id, "offline");
      const isHevc = /hvc1|hev1/i.test(info.mime || "");
      const msg = isHevc
        ? "This camera is H265/HEVC and this browser can't decode it. Recording and the RTSP relay are active — open the RTSP stream in a compatible player (VLC, the mobile app), or try Safari / a browser with hardware HEVC."
        : "Live codec unsupported";
      video.replaceWith(makeMsg(msg));
      return;
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

    let waitingTimer = null;
    video.addEventListener("waiting", () => {
      if (!tile || tile.querySelector(".wall-buffering")) return;
      if (!video.buffered.length) {
        if (!waitingTimer) waitingTimer = setTimeout(() => {
          if (!video.paused) { const el = ensureOverlay(); if (el) el.classList.remove("wall-connection-lost"); }
        }, 2000);
        return;
      }
      for (let i = 0; i < video.buffered.length; i++) {
        if (video.buffered.start(i) > video.currentTime && video.buffered.start(i) - video.currentTime < 1) {
          video.currentTime = video.buffered.start(i) + 0.01;
          break;
        }
      }
    });
    video.addEventListener("playing", () => {
      if (waitingTimer) { clearTimeout(waitingTimer); waitingTimer = null; }
      removeOverlay();
      setCamStatus(cam.id, "online");
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
