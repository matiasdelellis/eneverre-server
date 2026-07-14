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
 * active mode (embedded [media] vs none):
 *  - rtsp:     RTSP relay URL (embedded: relay with rotating credentials;
 *             otherwise: the INI `source` value as-is)
 *  - live_mse: same-origin MSE fMP4 path (embedded: set; otherwise absent).
 *              The web UI plays live from this URL.
 *  - source/backchannel/thingino_*: NOT exposed (stripped server-side).
 * @typedef {object} Camera
 * @property {string} id
 * @property {string} [name]
 * @property {string} [comment]
 * @property {string} [location]
 * @property {CameraCapabilities} capabilities
 * @property {string} [rtsp]
 * @property {string} [live_mse]
 * @property {number} [width]
 * @property {number} [height]
 * @property {number} [home_x]
 * @property {number} [home_y]
 * @property {number} [privacy_x]
 * @property {number} [privacy_y]
 * @property {boolean} privacy
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

/**
 * Drop the cached camera list so the next fetchCameras() (sidebar, wall) hits
 * the server. Call after a create/delete so the change shows up everywhere.
 */
export function invalidateCameras() {
  setCamerasCache(null);
}

/** Create a camera. body is the create request; returns the new Camera. */
export async function createCamera(body) {
  return api("/api/cameras", { method: "POST", body: JSON.stringify(body) });
}

/** Delete a camera by id. */
export async function deleteCamera(id) {
  return api(`/api/camera/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/**
 * Probe an RTSP source before saving. Always resolves (never throws on an
 * unreachable camera): { ok: true, codecs, width, height } or { ok: false, error }.
 */
export async function probeCamera(source, transport) {
  return api("/api/cameras/probe", {
    method: "POST",
    body: JSON.stringify({ source, transport }),
  });
}
