import { $ } from "../util/dom.js";
import {
  get, set, remove, sessionGet, sessionSet, sessionRemove,
  loadJson, saveJson,
  TOKEN_KEY, REFRESH_KEY, USER_KEY, VIEW_KEY, USERCODE_KEY, USERCODE_NAME_KEY,
} from "../util/storage.js";
import { api, token, setOnUnauthorized } from "../api.js";
import { t } from "../i18n.js";
import {
  getState, setViewMode as setViewModeState, setWallFilter, setWallFilterBeforeCam,
  setSidebarCollapsed, setOverlay, resetOnLogout, on,
} from "../state.js";
import { closeUserMenu, refreshUserMenu } from "../ui/user-menu.js";
import { showLogin } from "./login.js";

const PB_DEFAULT_INTERVAL = 6 * 60 * 60 * 1000; // timeline window: 6 hours

// Label for the overlay "back" buttons (Users / Cameras). They reveal the
// main app in whatever mode it was left in, so the label names that mode —
// "Back to Live" or "Back to Playback". Set on each entry into an overlay.
export function backLabel() {
  const mode = getState().viewMode === "playback" ? "view.playback" : "view.live";
  return t("nav.back", { view: t(mode) });
}

export function setViewMode(mode) {
  setViewModeState(mode);
  sessionSet(VIEW_KEY, mode);
  $("#wall").hidden = mode !== "live" && mode !== "playback";
  $("#playback-bar").hidden = mode !== "playback";
  const liveTab = $("#view-live");
  const pbTab = $("#view-playback");
  liveTab.classList.toggle("active", mode === "live");
  pbTab.classList.toggle("active", mode === "playback");
  // Mirror the visual state to assistive tech: role="tab" needs aria-selected
  // to announce which view is active (the .active class alone is invisible to
  // a screen reader).
  liveTab.setAttribute("aria-selected", mode === "live" ? "true" : "false");
  pbTab.setAttribute("aria-selected", mode === "playback" ? "true" : "false");
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
    { exitStatusView, stopDiskAlertPolling },
    { hideDeviceAuth },
    { stopSidebarThumbRefresh },
  ] = await Promise.all([
    import("./wall.js"),
    import("./playback.js"),
    import("./ptz.js"),
    import("./users.js"),
    import("./cameras.js"),
    import("./status.js"),
    import("./device-auth.js"),
    import("./sidebar.js"),
  ]);
  destroyWall();
  teardownPlaybackTimeline();
  hidePtzModal();
  exitUsersView();
  exitCamerasView();
  exitStatusView();
  stopDiskAlertPolling();
  hideDeviceAuth();
  $("#wall").innerHTML = "";
  const sideScroll = $("#viewer-side-scroll");
  sideScroll.innerHTML = "";
  // Clear the "already populated" flag too, or loadSidebar() short-circuits
  // on the next login and the sidebar stays empty until a full page reload.
  delete sideScroll.dataset.loaded;
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
  remove(REFRESH_KEY);
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

// Guard flag: true while we are pushing state INTO the app from the URL
// (initial load / popstate). It suppresses syncUrl() so applying URL state
// doesn't turn around and write a fresh history entry (which would clobber
// the entry we're navigating to and break back/forward).
let applyingUrl = false;

// parseUrlState reads the navigable state from the query string:
//   ?view=live|playback            main view mode (default live)
//   ?view=account|users|cameras|status
//                                 a full-screen overlay panel (the
//                                 four admin/personal views that used
//                                 to be DOM-only — now they have a
//                                 URL so a refresh keeps the user
//                                 on the same panel)
//   ?cam=<id>                      single-camera filter (main view only)
//   ?loc=<name>                    location filter, cam wins (main view only)
//   ?t=<unix>                      playback cursor, seconds or ms (only from
//                                  a shared "this moment" link — normal
//                                  navigation never writes it)
// Returns null when the URL carries no navigable state (bare "/"), so the
// caller can fall back to the stored session state on first load.
function parseUrlState() {
  const params = new URLSearchParams(window.location.search);
  const viewRaw = (params.get("view") || "").trim().toLowerCase();
  let view;
  if (viewRaw === "playback") view = "playback";
  else if (viewRaw === "account" || viewRaw === "users" || viewRaw === "cameras" || viewRaw === "status") view = viewRaw;
  else view = "live";
  const cam = (params.get("cam") || "").trim();
  const loc = (params.get("loc") || "").trim();
  let filter = { type: "all" };
  if (cam) filter = { type: "cam", value: cam };
  else if (loc) filter = { type: "loc", value: loc };
  let t = null;
  const tRaw = (params.get("t") || "").trim();
  if (tRaw) {
    let n = parseInt(tRaw, 10);
    if (Number.isFinite(n) && n > 0) {
      if (n > 1e12) n = Math.floor(n / 1000);
      t = n;
    }
  }
  if (filter.type === "all" && view === "live" && t == null) return null;
  return { view, filter, t };
}

async function applyUrlState(state) {
  // Applying is guarded so the resulting wallFilter/viewMode/overlay events
  // don't push a new history entry — the URL already reflects this state
  // (it's either the address the user opened or the entry popstate restored).
  applyingUrl = true;
  try {
    if (state.t != null) {
      // Lazy import to avoid cycle (playback imports from app-shell for setViewMode).
      const { setPreservedPbState } = await import("./playback.js");
      setPreservedPbState({
        selectedMsec: state.t * 1000,
        intervalMsec: PB_DEFAULT_INTERVAL,
      });
    }
    setWallFilter(state.filter);
    if (state.view === "live" || state.view === "playback") {
      setViewMode(state.view);
      // Returning to the main view: drop any overlay (its `exit*` does the
      // real cleanup — see the overlay listener below).
      setOverlay(null);
    } else {
      // Open the named overlay panel. The listener in initDeepLinks calls
      // the matching enter* and, if another overlay was open, its exit* first.
      setOverlay(state.view);
    }
  } finally {
    applyingUrl = false;
  }
}

// currentUrlParams builds the URL that reflects the current navigation state
// (overlay panel, wall filter, view mode). When an overlay is open the only
// param emitted is `view=<overlay>` — the main-view filter/cursor are not
// carried through, so going back from an overlay lands on the main view with
// the same `view=live|playback` state the overlay was stacked on top of.
// `t` is never emitted: the playback cursor only enters the URL through the
// explicit "share this moment" button.
function currentUrlParams() {
  const { wallFilter, viewMode, overlay } = getState();
  const u = new URL(window.location.href);
  u.searchParams.delete("view");
  u.searchParams.delete("cam");
  u.searchParams.delete("loc");
  u.searchParams.delete("t");
  if (overlay) {
    u.searchParams.set("view", overlay);
  } else {
    if (viewMode === "playback") u.searchParams.set("view", "playback");
    if (wallFilter.type === "cam") u.searchParams.set("cam", wallFilter.value);
    else if (wallFilter.type === "loc") u.searchParams.set("loc", wallFilter.value);
  }
  return u;
}

// writeUrl reflects the current state into the address bar. `push` adds a
// history entry (ordinary navigation, so back/forward works); otherwise it
// replaces in place (boot normalization). No-op when the URL is unchanged.
function writeUrl(push) {
  const u = currentUrlParams();
  if (u.toString() === window.location.href) return;
  if (push) history.pushState(null, "", u.toString());
  else history.replaceState(null, "", u.toString());
}

// syncUrl is subscribed to wallFilter/viewMode/overlay changes: every
// camera/location selection, Live<->Playback switch, or overlay open/close
// becomes a bookmarkable, back/forward-able URL. Suppressed while applyingUrl
// is set (change came from the URL itself) so the round-trip doesn't push a
// duplicate history entry.
function syncUrl() {
  if (applyingUrl) return;
  writeUrl(true);
}

// Track the overlay that was open just before the most recent change so the
// overlay listener can call its `exit*` for full cleanup (focus-trap release,
// status-view timer, etc.) when the panel is replaced or closed. A bare local
// is enough: the listener always knows the previous value before the new one
// lands, and the URL is the only state that survives a reload — the listener
// starts with prev=null and the URL-driven applyUrlState opens the right
// panel on its own.
let prevOverlay = null;

function openOverlayPanel(name) {
  if (name === "account" || name === "users") {
    const mode = name === "account" ? "account" : "manage";
    return import("./users.js").then(({ enterUsersView }) => enterUsersView(mode));
  }
  if (name === "cameras") {
    return import("./cameras.js").then(({ enterCamerasView }) => enterCamerasView());
  }
  if (name === "status") {
    return import("./status.js").then(({ enterStatusView }) => enterStatusView());
  }
  return Promise.resolve();
}

function closeOverlayPanel(name) {
  if (name === "account" || name === "users") {
    return import("./users.js").then(({ exitUsersView }) => exitUsersView());
  }
  if (name === "cameras") {
    return import("./cameras.js").then(({ exitCamerasView }) => exitCamerasView());
  }
  if (name === "status") {
    return import("./status.js").then(({ exitStatusView }) => exitStatusView());
  }
  return Promise.resolve();
}

function onOverlayChange(name) {
  // Close whatever was open (if any) with full cleanup, then open the new
  // panel — or just the close if the user is going back to the main view.
  // Both happen in the same tick: the `exit*` briefly unhides `#app`, but
  // the `enter*` (or nothing) immediately hides it again, so no repaint.
  const prev = prevOverlay;
  prevOverlay = name;
  const closePrev = prev != null ? closeOverlayPanel(prev) : Promise.resolve();
  const openNext = name != null ? openOverlayPanel(name) : Promise.resolve();
  return Promise.all([closePrev, openNext]);
}

export async function showApp() {
  $("#login").hidden = true;
  $("#app").hidden = false;
  const u = loadJson(USER_KEY);
  // A still-pending forced password change (e.g. the page was reloaded before
  // it completed) gates the app until it is done.
  if (u && u.must_change_password) {
    const { showForcePasswordChange } = await import("./force-password.js");
    showForcePasswordChange();
    return;
  }
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
  // Low-disk banner: admins get a background poll of /api/status that shows
  // a persistent banner while the recording volume is low. Lazy import keeps
  // status.js (which imports from this module) out of a static cycle.
  import("./status.js").then(({ startDiskAlertPolling }) => startDiskAlertPolling());
  const urlState = parseUrlState();
  if (urlState) {
    applyUrlState(urlState);
  } else {
    // No navigable URL state: restore the last view mode from the session and
    // keep the wall filter loaded from localStorage, then reflect that restored
    // state into the address bar (replaceState, so no spurious history entry).
    const saved = sessionGet(VIEW_KEY);
    applyingUrl = true;
    setViewMode(saved === "playback" ? "playback" : "live");
    applyingUrl = false;
    writeUrl(false);
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

// moveGlobalControlsTo re-parents the global topbar controls (theme
// toggle + signed-in user menu) into the active view's topbar. All
// three topbars (main app, Users, Cameras) share a single instance of
// these controls — the container is moved (not cloned) on view switch,
// so there is no ID duplication and the dropdown positions naturally
// under whichever trigger is clicked. The dropdown is closed first so a
// stale open popup doesn't get carried across views.
export function moveGlobalControlsTo(targetTopbar) {
  if (!targetTopbar) return;
  const controls = document.getElementById("global-topbar-controls");
  if (!controls) return;
  if (controls.parentElement === targetTopbar) return;
  // Close any open user menu before moving so its open state and
  // aria-expanded don't get carried to a topbar the user can't see.
  const list = document.getElementById("user-menu-list");
  if (list && !list.hidden) {
    list.hidden = true;
    const trigger = document.getElementById("user-menu-trigger");
    if (trigger) trigger.setAttribute("aria-expanded", "false");
  }
  targetTopbar.appendChild(controls);
}

// closeOverlayViews hides every full-screen overlay (the Users and Cameras
// panels and their modals). They all use `position: fixed; inset: 0` with the
// same z-index, so when two are open at once the later one in the DOM paints
// on top — and closing it reveals the other stacked underneath instead of the
// live app. The user menu lives in each overlay's topbar, so an admin can jump
// straight from one overlay to another without the first ever closing. Calling
// this when entering an overlay keeps exactly one open, so "Back to cameras"
// always lands on the live wall rather than the previously-open panel.
export function closeOverlayViews() {
  for (const id of ["status-view", "users-view", "user-edit-modal", "cameras-view", "cam-wizard-modal"]) {
    const el = document.getElementById(id);
    if (el) el.hidden = true;
  }
}

function initTopbar() {
  $("#view-live").addEventListener("click", () => setViewMode("live"));
  $("#view-playback").addEventListener("click", () => setViewMode("playback"));
  $("#viewer-toggle").addEventListener("click", () => {
    const next = !getState().sidebarCollapsed;
    setSidebarCollapsed(next);
    $("#viewer-side").classList.toggle("collapsed", next);
    $("#viewer-toggle").title = next ? t("sidebar.expand") : t("sidebar.collapse");
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
  // Reflect every camera/location selection, Live<->Playback switch, and
  // overlay open/close into the URL so any view can be bookmarked (and
  // reached with back/forward or by reloading the tab).
  on("wallFilter", syncUrl);
  on("viewMode", syncUrl);
  on("overlay", syncUrl);
  on("overlay", onOverlayChange);
  window.addEventListener("popstate", () => {
    // Skip on the login / forced-password screens — the URL is still theirs
    // to drive (usercode flow, etc.). On a successful popstate the user is in
    // the main app (logged in) and the URL state is the source of truth.
    if (!$("#login").hidden || !$("#force-pw").hidden || !$("#device-auth-view").hidden) return;
    // A bare "/" (no navigable params) means "all cameras, live".
    const state = parseUrlState() || { view: "live", filter: { type: "all" }, t: null };
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
    $("#viewer-toggle").title = collapsed ? t("sidebar.expand") : t("sidebar.collapse");
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
