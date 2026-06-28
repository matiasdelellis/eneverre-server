import { TOKEN_KEY } from "./util/storage.js";
import { getState, setCamerasCache } from "./state.js";

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

export async function fetchCameras() {
  const s = getState();
  if (!s.camerasCache) s.camerasCache = await api("/api/cameras");
  setCamerasCache(s.camerasCache);
  return s.camerasCache;
}
