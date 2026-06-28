import { makeMsg } from "../util/dom.js";

const wallInstances = new Map(); // camId -> Hls (wall view only)

export function getWallInstances() { return wallInstances; }

export function attachHls(url, v) {
  if (v.canPlayType("application/vnd.apple.mpegurl")) {
    v.src = url;
    return null;
  }
  if (window.Hls && Hls.isSupported()) {
    const h = new Hls({ maxBufferLength: 10 });
    h.loadSource(url);
    h.attachMedia(v);
    return h;
  }
  v.replaceWith(makeMsg("HLS not supported in this browser"));
  return null;
}

export function captureFrame(hlsUrl) {
  return new Promise((resolve, reject) => {
    const video = document.createElement("video");
    video.muted = true;
    video.playsInline = true;
    video.crossOrigin = "anonymous";

    let hls = null;
    let settled = false;
    const timeout = setTimeout(() => finish(reject, new Error("timeout")), 15000);

    const finish = (fn, arg) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      try { if (hls) hls.destroy(); } catch {}
      video.removeAttribute("src");
      video.load();
      fn(arg);
    };

    video.addEventListener("playing", () => {
      try {
        const w = video.videoWidth || 640;
        const h = video.videoHeight || 360;
        const canvas = document.createElement("canvas");
        canvas.width = w;
        canvas.height = h;
        canvas.getContext("2d").drawImage(video, 0, 0, w, h);
        finish(resolve, canvas.toDataURL("image/jpeg", 0.7));
      } catch (e) {
        finish(reject, e);
      }
    }, { once: true });

    video.addEventListener("error", () => finish(reject, new Error("video error")), { once: true });

    if (video.canPlayType("application/vnd.apple.mpegurl")) {
      video.src = hlsUrl;
    } else if (window.Hls && Hls.isSupported()) {
      hls = new Hls();
      hls.loadSource(hlsUrl);
      hls.attachMedia(video);
    } else {
      finish(reject, new Error("HLS not supported"));
      return;
    }

    video.play().catch((e) => finish(reject, e));
  });
}
