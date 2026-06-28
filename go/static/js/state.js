import { get, set, remove, WALL_FILTER_KEY, SIDEBAR_KEY } from "./util/storage.js";

const state = {
  viewMode: "live",
  sidebarCollapsed: get(SIDEBAR_KEY) === "1",
  wallFilter: loadWallFilter(),
  wallFilterBeforeCam: null,
  camerasCache: null,
  lastPtzCam: null,
};

function loadWallFilter() {
  const raw = get(WALL_FILTER_KEY);
  if (!raw || raw === "all") return { type: "all" };
  if (raw.startsWith("cam:")) return { type: "cam", value: raw.slice(4) };
  if (raw.startsWith("loc:")) return { type: "loc", value: raw.slice(4) };
  return { type: "loc", value: raw };
}

function saveWallFilter() {
  if (state.wallFilter.type === "all") remove(WALL_FILTER_KEY);
  else set(WALL_FILTER_KEY, `${state.wallFilter.type}:${state.wallFilter.value}`);
}

const listeners = new Map();

export function on(event, fn) {
  if (!listeners.has(event)) listeners.set(event, new Set());
  listeners.get(event).add(fn);
  return () => listeners.get(event)?.delete(fn);
}

export function emit(event, payload) {
  const set = listeners.get(event);
  if (!set) return;
  for (const fn of set) {
    try { fn(payload); } catch (e) { console.error(`state listener for ${event} threw:`, e); }
  }
}

export function getState() { return state; }

export function setViewMode(mode) {
  if (state.viewMode === mode) return;
  state.viewMode = mode;
  emit("viewMode", mode);
}

export function setSidebarCollapsed(v) {
  if (state.sidebarCollapsed === v) return;
  state.sidebarCollapsed = v;
  set(SIDEBAR_KEY, v ? "1" : "0");
  emit("sidebarCollapsed", v);
}

export function setWallFilter(next) {
  state.wallFilter = next;
  saveWallFilter();
  emit("wallFilter", next);
}

export function setWallFilterBeforeCam(v) {
  state.wallFilterBeforeCam = v;
}

export function setCamerasCache(cams) {
  state.camerasCache = cams;
}

export function setLastPtzCam(cam) {
  state.lastPtzCam = cam;
}

export function resetOnLogout() {
  state.wallFilter = { type: "all" };
  state.wallFilterBeforeCam = null;
  state.viewMode = "live";
  state.camerasCache = null;
  state.lastPtzCam = null;
  remove(WALL_FILTER_KEY);
}
