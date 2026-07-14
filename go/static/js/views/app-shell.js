import { $, $$ } from "../util/dom.js";
import {
  get, set, remove, sessionGet, sessionSet, sessionRemove,
  loadJson, saveJson,
  TOKEN_KEY, USER_KEY, VIEW_KEY, USERCODE_KEY, USERCODE_NAME_KEY,
} from "../util/storage.js";
import { api, token, setOnUnauthorized } from "../api.js";
import {
  getState, setViewMode as setViewModeState, setWallFilter, setWallFilterBeforeCam,
  setSidebarCollapsed, resetOnLogout,
} from "../state.js";
import { closeUserMenu, refreshUserMenu } from "../ui/user-menu.js";
import { showLogin } from "./login.js";

const PB_DEFAULT_INTERVAL = 6 * 60 * 60 * 1000; // timeline window: 6 hours

export function setViewMode(mode) {
  setViewModeState(mode);
  sessionSet(VIEW_KEY, mode);
  $("#wall").hidden = mode !== "live" && mode !== "playback";
  $("#playback-bar").hidden = mode !== "playback";
  $("#view-live").classList.toggle("active", mode === "live");
  $("#view-playback").classList.toggle("active", mode === "playback");
  if (mode === "live" || mode === "playback") {
    // Lazy-load: avoids the wall/playback <-> app-shell cycle.
    Promise.all([import("./wall.js"), import("./ptz.js")]).then(([wall, ptz]) => {
      wall.loadWall(mode);
      ptz.updatePtzModal();
    });
  }
}

export async function logout(silent = false) {
  const t = token();
  const [
    { destroyWall },
    { teardownPlaybackTimeline },
    { hidePtzModal },
    { exitUsersView },
    { exitCamerasView },
    { hideDeviceAuth },
    { stopSidebarThumbRefresh },
  ] = await Promise.all([
    import("./wall.js"),
    import("./playback.js"),
    import("./ptz.js"),
    import("./users.js"),
    import("./cameras.js"),
    import("./device-auth.js"),
    import("./sidebar.js"),
  ]);
  destroyWall();
  teardownPlaybackTimeline();
  hidePtzModal();
  exitUsersView();
  exitCamerasView();
  hideDeviceAuth();
  $("#wall").innerHTML = "";
  $("#viewer-side-scroll").innerHTML = "";
  stopSidebarThumbRefresh();
  // Drop cached sidebar thumbnails so the next login captures them
  // fresh under the new session. `thumb_*` keys are the only ones
  // written by captureFrame().
  for (let i = localStorage.length - 1; i >= 0; i--) {
    const k = localStorage.key(i);
    if (k && k.startsWith("thumb_")) remove(k);
  }
  resetOnLogout();
  sessionRemove(VIEW_KEY);
  remove(TOKEN_KEY);
  remove(USER_KEY);
  if (t && !silent) {
    fetch("/api/auth/logout", {
      method: "POST",
      headers: { Authorization: `Bearer ${t}` },
    }).catch(() => {});
  }
  showLogin();
}

function parseUsercode() {
  const params = new URLSearchParams(window.location.search);
  const raw = (params.get("usercode") || "").trim().toUpperCase();
  if (!/^[A-F0-9]{6}$/.test(raw)) return null;
  return raw;
}

function parseDeviceNameFromUrl() {
  const params = new URLSearchParams(window.location.search);
  const raw = (params.get("device_name") || "").trim();
  if (!raw) return null;
  return raw.length > 120 ? raw.slice(0, 120) : raw;
}

function parseUrlState() {
  const params = new URLSearchParams(window.location.search);
  const cam = (params.get("cam") || "").trim();
  const tRaw = (params.get("t") || "").trim();
  const viewRaw = (params.get("view") || "").trim().toLowerCase();
  const view = (viewRaw === "live" || viewRaw === "playback") ? viewRaw : "live";
  if (!cam || !tRaw) return null;
  let t = parseInt(tRaw, 10);
  if (!Number.isFinite(t) || t <= 0) return null;
  if (t > 1e12) t = Math.floor(t / 1000);
  return { view, cam, t };
}

async function applyUrlState(state) {
  // Lazy import to avoid cycle (playback imports from app-shell for setViewMode).
  const { setPreservedPbState } = await import("./playback.js");
  setPreservedPbState({
    selectedMsec: state.t * 1000,
    intervalMsec: PB_DEFAULT_INTERVAL,
  });
  setWallFilter({ type: "cam", value: state.cam });
  setViewMode(state.view);
  const u = new URL(window.location.href);
  u.searchParams.set("view", state.view);
  u.searchParams.set("cam", state.cam);
  u.searchParams.set("t", String(state.t));
  history.replaceState(null, "", u.toString());
}

export async function showApp() {
  $("#login").hidden = true;
  $("#app").hidden = false;
  const u = loadJson(USER_KEY);
  const menu = $("#user-menu");
  menu.hidden = !u;
  refreshUserMenu(u);
  // finishBoot is in login.js but works without import cycle; re-import.
  const { finishBoot } = await import("./login.js");
  finishBoot();
  const pending = sessionGet(USERCODE_KEY);
  const pendingName = sessionGet(USERCODE_NAME_KEY);
  const fromUrl = parseUsercode();
  const { showDeviceAuth } = await import("./device-auth.js");
  if (fromUrl) {
    const fromUrlName = parseDeviceNameFromUrl();
    sessionSet(USERCODE_KEY, fromUrl);
    if (fromUrlName) sessionSet(USERCODE_NAME_KEY, fromUrlName);
    else sessionRemove(USERCODE_NAME_KEY);
    showDeviceAuth(fromUrl, fromUrlName);
    return;
  }
  if (pending) {
    showDeviceAuth(pending, pendingName);
    return;
  }
  const urlState = parseUrlState();
  if (urlState) {
    applyUrlState(urlState);
  } else {
    const saved = sessionGet(VIEW_KEY);
    setViewMode(saved === "playback" ? "playback" : "live");
  }
}

export function isMobileViewport() {
  return window.matchMedia && window.matchMedia("(max-width: 900px)").matches;
}

function openSidebarDrawer() {
  $("#viewer-side").classList.add("collapsed");
  const scrim = $("#sidebar-scrim");
  if (scrim) scrim.classList.add("shown");
  scrim?.removeAttribute("hidden");
}

export function closeSidebarDrawer() {
  $("#viewer-side").classList.remove("collapsed");
  const scrim = $("#sidebar-scrim");
  if (scrim) scrim.classList.remove("shown");
  scrim?.setAttribute("hidden", "");
}

export function toggleSidebarDrawer(force) {
  if (force === true) { openSidebarDrawer(); return; }
  if (force === false) { closeSidebarDrawer(); return; }
  if ($("#viewer-side").classList.contains("collapsed")) closeSidebarDrawer();
  else openSidebarDrawer();
}

function initTopbar() {
  $("#audio-toggle").addEventListener("click", () => {
    const videos = $$("#wall video");
    if (!videos.length) return;
    const newMuted = !videos[0].muted;
    for (const v of videos) v.muted = newMuted;
    $("#audio-toggle").textContent = newMuted ? "🔇" : "🔊";
    $("#audio-toggle").title = newMuted ? "Unmute audio" : "Mute audio";
  });
  $("#view-live").addEventListener("click", () => setViewMode("live"));
  $("#view-playback").addEventListener("click", () => setViewMode("playback"));
  $("#viewer-toggle").addEventListener("click", () => {
    const next = !getState().sidebarCollapsed;
    setSidebarCollapsed(next);
    $("#viewer-side").classList.toggle("collapsed", next);
    $("#viewer-toggle").title = next ? "Expand sidebar" : "Collapse sidebar";
    document.documentElement.style.setProperty(
      "--sidebar-w",
      next ? "52px" : "280px",
    );
  });
  $("#sidebar-open")?.addEventListener("click", () => openSidebarDrawer());
  $("#sidebar-close")?.addEventListener("click", () => closeSidebarDrawer());
  $("#sidebar-scrim")?.addEventListener("click", () => closeSidebarDrawer());
  $("#logout").addEventListener("click", () => { closeUserMenu(); logout(false); });
}

function initDeepLinks() {
  window.addEventListener("popstate", () => {
    if ($("#app").hidden) return;
    const state = parseUrlState();
    if (!state) return;
    applyUrlState(state);
  });
}

function initResizeObserver() {
  if (typeof ResizeObserver === "undefined") return;
  const ca = document.getElementById("content-area");
  if (!ca) return;
  new ResizeObserver(async () => {
    if (!["live", "playback"].includes(getState().viewMode)) return;
    const wall = document.getElementById("wall");
    if (!wall) return;
    const tiles = wall.querySelectorAll(".wall-tile");
    if (!tiles.length) return;
    const { gridLayout } = await import("./wall.js");
    const { cols, tilePct } = gridLayout(tiles.length);
    wall.style.setProperty("--cols", cols);
    wall.style.setProperty("--tile-w", `${tilePct}%`);
  }).observe(ca);
}

function applyViewportLayout() {
  const mobile = isMobileViewport();
  if (mobile) {
    // On mobile the sidebar is a drawer, not a column. Always start
    // closed regardless of the desktop's `sidebarCollapsed` preference
    // (the localStorage value is for the desktop collapse state and
    // is intentionally not consulted here). The hamburger (#sidebar-open)
    // in the topbar opens the drawer.
    closeSidebarDrawer();
    document.documentElement.style.setProperty("--sidebar-w", "0px");
  } else {
    // Desktop: re-apply the persisted collapse state, both as a class
    // (drives width) and as a CSS variable (drives the PTZ FAB offset).
    const collapsed = getState().sidebarCollapsed;
    $("#viewer-side").classList.toggle("collapsed", collapsed);
    $("#viewer-toggle").title = collapsed ? "Expand sidebar" : "Collapse sidebar";
    document.documentElement.style.setProperty(
      "--sidebar-w",
      collapsed ? "52px" : "280px",
    );
  }
}

function initViewportListener() {
  if (!window.matchMedia) return;
  const mq = window.matchMedia("(max-width: 900px)");
  // Re-apply layout when the viewport crosses the breakpoint. The PTZ
  // module listens to the same query to clear its persisted
  // left/top inline styles when entering mobile (the bottom-sheet
  // layout ignores them).
  const onChange = () => applyViewportLayout();
  if (mq.addEventListener) mq.addEventListener("change", onChange);
  else if (mq.addListener) mq.addListener(onChange);
}

export function initAppShell() {
  applyViewportLayout();
  setOnUnauthorized(() => logout(true));
  initTopbar();
  initDeepLinks();
  initResizeObserver();
  initViewportListener();
}
