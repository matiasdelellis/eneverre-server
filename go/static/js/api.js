import { TOKEN_KEY } from "./util/storage.js";
import { getState, setCamerasCache } from "./state.js";

/**
 * @typedef {object} CameraCapabilities
 * @property {boolean} privacy
 * @property {boolean} thumbnail
 * @property {boolean} playback
 * @property {boolean} ptz
 * @property {boolean} talk
 * @property {string[]} [talk_codecs]
 */

/**
 * Camera object as returned by GET /api/cameras. Field set depends on the
 * active mode (embedded [media] vs external [mediamtx] vs none):
 *  - hls / webrtc: external MediaMTX HLS and WebRTC URLs (embedded: empty)
 *  - live_mse:     same-origin MSE fMP4 path (embedded: set; otherwise absent)
 *  - rtsp:         RTSP relay URL (embedded: relay; otherwise the INI live URL)
 *  - live:         back-compat alias for rtsp
 *  - source/backchannel/thingino_*: NOT exposed (stripped server-side).
 * @typedef {object} Camera
 * @property {string} id
 * @property {string} [name]
 * @property {string} [comment]
 * @property {string} [location]
 * @property {CameraCapabilities} capabilities
 * @property {string} [rtsp]
 * @property {string} [webrtc]
 * @property {string} [hls]
 * @property {string} [live_mse]
 * @property {number} [width]
 * @property {number} [height]
 * @property {number} [home_x]
 * @property {number} [home_y]
 * @property {number} [privacy_x]
 * @property {number} [privacy_y]
 * @property {boolean} privacy
 * @property {string} [live]
 * @property {boolean} [playback]
 * @property {boolean} [ptz]
 */

let onUnauthorized = () => { location.reload(); };

export function setOnUnauthorized(fn) {
  onUnauthorized = fn;
}

export function token() {
  return localStorage.getItem(TOKEN_KEY);
}

export async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  const t = token();
  if (t) headers["Authorization"] = `Bearer ${t}`;
  if (opts.body && !(opts.body instanceof FormData) && !headers["Content-Type"]) {
    headers["Content-Type"] = "application/json";
  }
  const r = await fetch(path, { ...opts, headers });
  if (r.status === 401) {
    onUnauthorized();
    throw new Error("Unauthorized");
  }
  if (!r.ok) {
    let msg;
    try { msg = (await r.json()).detail; } catch { msg = r.statusText; }
    throw new Error(msg || `HTTP ${r.status}`);
  }
  if (r.status === 204) return null;
  const ct = r.headers.get("content-type") || "";
  return ct.includes("application/json") ? r.json() : r.text();
}

/** @returns {Promise<Camera[]>} */
export async function fetchCameras() {
  const s = getState();
  if (!s.camerasCache) s.camerasCache = await api("/api/cameras");
  setCamerasCache(s.camerasCache);
  return s.camerasCache;
}
