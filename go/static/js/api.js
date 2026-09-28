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
 * Camera object as returned by GET /api/cameras. The embedded engine is the
 * only streaming mode and is always on for cameras with a `source` URL:
 *  - rtsp:     RTSP relay URL with rotating credentials (withheld while the
 *             camera is in privacy or off-hours).
 *  - live_mse: same-origin MSE fMP4 path (withheld under the same conditions,
 *             or when the camera opted out). The web UI plays live from this URL.
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

// Default per-request timeout. The server's Read timeout is 5m for regular
// endpoints — waiting that long with a disabled submit button is worse than
// failing fast; callers with legitimately slow work (RTSP probe, clip
// download) pass a larger timeoutMs.
const DEFAULT_TIMEOUT_MS = 15_000;

// joinSignals merges an optional caller AbortSignal with a timeout signal.
// AbortSignal.any is widely available in 2026; the fallback composes the two
// manually for older engines (the timeout always fires, the caller signal
// may already be aborted).
function joinSignals(callerSignal, timeoutMs) {
  const timeout = timeoutMs > 0 ? AbortSignal.timeout(timeoutMs) : null;
  if (!timeout) return callerSignal || null;
  if (!callerSignal) return timeout;
  if (typeof AbortSignal.any === "function") return AbortSignal.any([callerSignal, timeout]);
  const ctrl = new AbortController();
  const onAbort = () => ctrl.abort(callerSignal.reason);
  if (callerSignal.aborted) onAbort();
  else callerSignal.addEventListener("abort", onAbort, { once: true });
  timeout.addEventListener("abort", () => ctrl.abort(timeout.reason), { once: true });
  return ctrl.signal;
}

// isTimeoutError distinguishes our AbortSignal.timeout from a caller-initiated
// abort (e.g. a superseded request): a timeout carries a TimeoutError name.
function isTimeoutError(err) {
  return err && (err.name === "TimeoutError" ||
    (err.name === "AbortError" && err.message && err.message.includes("timeout")));
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
      // A network blip at the moment of expiry must not kill a valid session:
      // retry once after a short pause before giving up. Only a definitive
      // non-ok response (or a second network failure) ends the attempt.
      for (let attempt = 0; attempt < 2; attempt++) {
        try {
          const r = await fetch("/api/auth/refresh", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ refresh_token: rt }),
            signal: joinSignals(null, DEFAULT_TIMEOUT_MS),
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
          if (attempt === 0) {
            await new Promise((resolve) => setTimeout(resolve, 1000));
            continue;
          }
          return false;
        }
      }
      return false;
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
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const withAuth = () => {
    const headers = { ...(opts.headers || {}) };
    const t = token();
    if (t) headers["Authorization"] = `Bearer ${t}`;
    const { timeoutMs: _ignored, headers: _h, ...rest } = opts;
    return { ...rest, headers, signal: joinSignals(opts.signal, timeoutMs) };
  };
  let r;
  try {
    r = await fetch(path, withAuth());
  } catch (err) {
    if (isTimeoutError(err)) throw new Error("Request timed out");
    throw err;
  }
  // The auth endpoints answer 401 as part of their normal contract (wrong
  // password, consumed refresh token) — recovering or logging out on those
  // would loop.
  if (r.status === 401 && !path.startsWith("/api/auth/")) {
    if (await refreshSession()) {
      try {
        r = await fetch(path, withAuth());
      } catch (err) {
        if (isTimeoutError(err)) throw new Error("Request timed out");
        throw err;
      }
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
// Camera-list cache: shared by the wall, sidebar, PTZ and the admin list.
// - TTL: privacy/schedule_off/connection can change from another client or
//   tab; without a TTL the first fetch of a session stuck forever. 30s is
//   short enough that a returning tab sees fresh state on the next load and
//   long enough that filter-click storms don't refetch.
// - In-flight dedup: loadWall and updatePtzModal both call fetchCameras on
//   every filter change; with an empty cache they used to race two GETs.
const CAMERAS_TTL_MS = 30_000;
let camerasFetchedAt = 0;
let camerasInflight = null;
// camerasGen is bumped by invalidateCameras. A request started before an
// invalidation carries the pre-mutation list: it must neither be joined by a
// later caller nor be cached as fresh when it lands.
let camerasGen = 0;
let camerasInflightGen = -1;

export async function fetchCameras(opts = {}) {
  const s = getState();
  const fresh = s.camerasCache && (Date.now() - camerasFetchedAt) < CAMERAS_TTL_MS;
  if (fresh && opts.force !== true) return s.camerasCache;
  if (!camerasInflight || camerasInflightGen !== camerasGen) {
    const gen = camerasGen;
    const req = api("/api/cameras")
      .then((cams) => {
        if (gen === camerasGen) {
          s.camerasCache = cams;
          setCamerasCache(cams);
          camerasFetchedAt = Date.now();
        }
        return cams;
      })
      .finally(() => { if (camerasInflight === req) camerasInflight = null; });
    camerasInflight = req;
    camerasInflightGen = gen;
  }
  return camerasInflight;
}

/**
 * Drop the cached camera list so the next fetchCameras() (sidebar, wall) hits
 * the server. Call after a create/delete so the change shows up everywhere.
 */
export function invalidateCameras() {
  camerasGen++;
  setCamerasCache(null);
  camerasFetchedAt = 0;
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

// --- recording schedules --------------------------------------------------

/** List every recording schedule (named program). */
export async function fetchSchedules() {
  return api("/api/schedules");
}

/**
 * Create a recording schedule. body = { id, name, days }, where days maps a
 * weekday key ("mon".."sun") to ["HH:MM-HH:MM", ...] armed windows.
 */
export async function createSchedule(body) {
  return api("/api/schedules", { method: "POST", body: JSON.stringify(body) });
}

/** Update a schedule. body = { name, days } (id is fixed by the URL). */
export async function updateSchedule(id, body) {
  return api(`/api/schedule/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(body) });
}

/** Delete a schedule by id. Fails (409) when a camera still references it. */
export async function deleteSchedule(id) {
  return api(`/api/schedule/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/**
 * Probe an RTSP source before saving. Always resolves (never throws on an
 * unreachable camera): { ok: true, codecs, width, height } or { ok: false, error }.
 */
export async function probeCamera(source, transport) {
  return api("/api/cameras/probe", {
    method: "POST",
    body: JSON.stringify({ source, transport }),
    timeoutMs: 60_000, // RTSP DESCRIBE against an unreachable camera can hang
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
    timeoutMs: 30_000,
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

/**
 * Admin-only: the cached live-settings snapshot of one camera (day/night
 * mode, illuminators, motion, audio). 404 while no snapshot has been fetched
 * yet or the camera lacks the settings capability.
 */
export async function fetchCameraSettings(camId) {
  return api(`/api/camera/${camId}/settings`);
}

/**
 * Admin-only: adjust a camera's live settings (motion detection, mic,
 * speaker, day/night auto) — advertised by capabilities.settings, not
 * camera-specific. Body is a partial object — only the keys present are
 * changed. Throws on non-2xx, including 502 when the camera rejected the
 * change.
 */
export async function setCameraSettings(camId, body) {
  return api(`/api/camera/${camId}/settings`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}
