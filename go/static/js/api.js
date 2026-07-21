import { TOKEN_KEY, REFRESH_KEY, get, set } from "./util/storage.js";
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
 * Public PTZ metadata block on a Camera, present only when
 * `capabilities.ptz` is true. Exposes the lens FOV and the angular range so
 * the UI can translate a pixel drag into a relative move in degrees; the
 * steps↔degrees calibration is server-side and never reaches the wire.
 * @typedef {object} CameraPTZ
 * @property {number} fov_h    horizontal FOV, degrees
 * @property {number} fov_v    vertical FOV, degrees (derived from fov_h and aspect)
 * @property {number} pan_range  total pan range, degrees
 * @property {number} tilt_range total tilt range, degrees
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
 * @property {CameraPTZ} [ptz]
 * @property {string} [rtsp]
 * @property {string} [live_mse]
 * @property {number} [width]
 * @property {number} [height]
 * @property {boolean} privacy
 */

let onUnauthorized = () => { location.reload(); };

export function setOnUnauthorized(fn) {
  onUnauthorized = fn;
}

export function token() {
  return localStorage.getItem(TOKEN_KEY);
}

// Single-flight guard: concurrent 401s (a wall of tiles expiring together)
// must produce ONE refresh call — the server rotates the refresh token
// atomically, so a second concurrent attempt with the same token would fail
// and log the session out.
let refreshing = null;

/**
 * Exchanges the stored refresh token for a fresh access/refresh pair.
 * Resolves true when the session was renewed (tokens already stored).
 * Safe to call concurrently from any number of failed requests.
 */
export function refreshSession() {
  const rt = get(REFRESH_KEY);
  if (!rt) return Promise.resolve(false);
  if (!refreshing) {
    refreshing = (async () => {
      try {
        const r = await fetch("/api/auth/refresh", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ refresh_token: rt }),
        });
        if (!r.ok) {
          // Another tab may have rotated the pair first; if the stored token
          // changed under us, its refresh succeeded and we can ride it.
          return get(REFRESH_KEY) !== rt;
        }
        const data = await r.json();
        set(TOKEN_KEY, data.token);
        set(REFRESH_KEY, data.refresh_token);
        return true;
      } catch {
        return false;
      }
    })();
    refreshing.finally(() => { refreshing = null; });
  }
  return refreshing;
}

/**
 * fetch + Bearer + expired-session recovery, returning the raw Response. On a
 * 401 it refreshes the session once and retries the request with the new
 * token; when that fails it fires onUnauthorized (logout) and returns the 401
 * for the caller to treat as fatal. Use this instead of bare fetch for any
 * authenticated request that needs the Response itself (streams, blobs);
 * api() below wraps it for JSON endpoints.
 */
export async function apiFetch(path, opts = {}) {
  const withAuth = () => {
    const headers = { ...(opts.headers || {}) };
    const t = token();
    if (t) headers["Authorization"] = `Bearer ${t}`;
    return { ...opts, headers };
  };
  let r = await fetch(path, withAuth());
  // The auth endpoints answer 401 as part of their normal contract (wrong
  // password, consumed refresh token) — recovering or logging out on those
  // would loop.
  if (r.status === 401 && !path.startsWith("/api/auth/")) {
    if (await refreshSession()) {
      r = await fetch(path, withAuth());
    }
    if (r.status === 401) onUnauthorized();
  }
  return r;
}

export async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  if (opts.body && !(opts.body instanceof FormData) && !headers["Content-Type"]) {
    headers["Content-Type"] = "application/json";
  }
  const r = await apiFetch(path, { ...opts, headers });
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

/** Update an existing camera. body is the same shape as createCamera. */
export async function updateCamera(id, body) {
  return api(`/api/camera/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(body) });
}

/**
 * Fetch a camera's full stored config (admin only), including source and
 * credentials, to prefill the edit form. Distinct from fetchCameras(), whose
 * public model omits those fields.
 */
export async function getCameraConfig(id) {
  return api(`/api/camera/${encodeURIComponent(id)}/config`);
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

/**
 * Test a Thingino camera's URL + API key before saving. Always resolves:
 * { ok: true, ptz: false } | { ok: true, ptz: true, pan_steps, pan_degrees,
 * tilt_steps, tilt_degrees, home_x, home_y, privacy_x, privacy_y } |
 * { ok: false, error }.
 */
export async function probeThingino(thingino_url, thingino_api_key) {
  return api("/api/cameras/probe-thingino", {
    method: "POST",
    body: JSON.stringify({ thingino_url, thingino_api_key }),
  });
}

/**
 * Admin-only operational snapshot: service/version/uptime, per-camera
 * connectivity/recording/privacy, aggregate totals, and (when recording)
 * storage headroom with the low-disk alert. Powers the server-status screen
 * and the low-disk banner. Throws on non-2xx (including 403 for non-admins).
 */
export async function fetchStatus() {
  return api("/api/status");
}
