import { $ } from "../util/dom.js";
import { get, set } from "../util/storage.js";
import { getState, setLastPtzCam, setCamerasCache, on } from "../state.js";
import { api, fetchCameras } from "../api.js";
import { alertModal } from "../ui/dialog.js";
import { icon } from "../ui/icons.js";
import { t } from "../i18n.js";

// Per-click pan/tilt step, in degrees. The old STEP=50 was firmware-native
// steps (which translated to ~8° pan / ~5° tilt on the default gimbal); the
// new value is the same physical move expressed in the public unit. 10° is
// big enough to feel responsive, small enough to land on a target without
// over-shooting. The server clamps the resulting relative move to the
// camera's full range, so a single tap can never command more than a half
// revolution in either axis.
const STEP_DEG = 10;
const PTZ_MODAL_POS_KEY = "eneverre.ptzModalPos";

// videoClickToPanTilt maps a point in viewport coordinates (a double-click on
// a live video tile) to the relative pan/tilt in degrees that would bring
// that point to the center of the frame. Uses the camera's lens FOV
// (cam.ptz.fov_h/fov_v — the same public metadata the server exposes so a
// client never needs the firmware's steps/calibration) and accounts for the
// video element's `object-fit: contain` letterboxing so the mapping is exact
// regardless of the tile's aspect ratio. Returns null when the camera has no
// PTZ FOV metadata, the video has no known dimensions yet, or the point
// landed in a letterbox bar rather than the actual frame.
export function videoClickToPanTilt(video, cam, clientX, clientY) {
  const fov = cam?.ptz;
  if (!fov || !(fov.fov_h > 0) || !(fov.fov_v > 0)) return null;
  const vw = video.videoWidth, vh = video.videoHeight;
  const rect = video.getBoundingClientRect();
  if (!vw || !vh || !rect.width || !rect.height) return null;
  const scale = Math.min(rect.width / vw, rect.height / vh);
  const dispW = vw * scale, dispH = vh * scale;
  const offX = (rect.width - dispW) / 2, offY = (rect.height - dispH) / 2;
  const px = clientX - rect.left - offX;
  const py = clientY - rect.top - offY;
  if (px < 0 || py < 0 || px > dispW || py > dispH) return null;
  const round2 = (n) => Math.round(n * 100) / 100;
  return {
    pan: round2(((px - dispW / 2) / (dispW / 2)) * (fov.fov_h / 2)),
    tilt: round2(((py - dispH / 2) / (dispH / 2)) * (fov.fov_v / 2)),
  };
}

// centerOnVideoPoint issues the relative move computed by
// videoClickToPanTilt (a no-op when that returns null). The server clamps
// the result to the camera's mechanical range, same as every other PTZ move.
export async function centerOnVideoPoint(cam, video, clientX, clientY) {
  const delta = videoClickToPanTilt(video, cam, clientX, clientY);
  if (!delta) return;
  await api(`/api/camera/${encodeURIComponent(cam.id)}/ptz/move?pan=${delta.pan}&tilt=${delta.tilt}`, { method: "POST" });
}

let ptzModalDrag = null;

function loadPtzModalPos() {
  try {
    const raw = get(PTZ_MODAL_POS_KEY);
    if (!raw) return null;
    const pos = JSON.parse(raw);
    if (typeof pos.left === "number" && typeof pos.top === "number") return pos;
  } catch {}
  return null;
}

function savePtzModalPos(left, top) {
  try { set(PTZ_MODAL_POS_KEY, JSON.stringify({ left, top })); } catch {}
}

function isMobileViewport() {
  return window.matchMedia && window.matchMedia("(max-width: 900px)").matches;
}

function applyPtzModalPos() {
  const modal = $("#ptz-modal");
  if (!modal) return;
  // On mobile the modal is a bottom sheet anchored to the viewport —
  // any persisted left/top from a previous desktop session would push
  // it off-screen. Clear the inline styles and let the mobile media
  // query take over.
  if (isMobileViewport()) {
    modal.style.left = "";
    modal.style.top = "";
    return;
  }
  let pos = loadPtzModalPos();
  if (!pos) {
    const rect = modal.getBoundingClientRect();
    pos = {
      left: Math.max(8, window.innerWidth - rect.width - 16),
      top: Math.max(8, 80),
    };
  }
  const rect = modal.getBoundingClientRect();
  pos.left = Math.min(Math.max(8, pos.left), window.innerWidth - rect.width - 8);
  pos.top = Math.min(Math.max(8, pos.top), window.innerHeight - rect.height - 8);
  modal.style.left = pos.left + "px";
  modal.style.top = pos.top + "px";
}

export function showPtzModal(cam) {
  const modal = $("#ptz-modal");
  if (!modal) return;
  const kind = cam.capabilities?.ptz ? t("ptz.title") : t("ptz.control");
  $("#ptz-modal-title").textContent = `${kind} — ${cam.name || cam.id}`;
  const body = $("#ptz-modal-body");
  body.innerHTML = "";
  body.appendChild(buildPtzPanel(cam));
  modal.hidden = false;
  modal.classList.add("draggable");
  requestAnimationFrame(applyPtzModalPos);
  $("#ptz-fab")?.setAttribute("hidden", "");
}

export function hidePtzModal() {
  const modal = $("#ptz-modal");
  if (modal) modal.hidden = true;
  ptzModalDrag = null;
  setPtzFab(ptzFabVisible());
}

function ptzFabVisible() {
  const { viewMode, wallFilter, lastPtzCam } = getState();
  if (viewMode !== "live" || wallFilter.type !== "cam") return false;
  if (!$("#ptz-modal")?.hidden) return false;
  // Privacy and push-to-talk now live in the topbar (syncPrivacyButton /
  // syncTalk), so the PTZ modal — and thus this FAB — is driven by the
  // PTZ capability alone.
  return lastPtzCam?.capabilities?.ptz === true;
}

function setPtzFab(visible) {
  const fab = $("#ptz-fab");
  if (!fab) return;
  if (visible) fab.removeAttribute("hidden");
  else fab.setAttribute("hidden", "");
}

function syncPtzFab() {
  setPtzFab(ptzFabVisible());
}

export async function updatePtzModal() {
  const { viewMode, wallFilter } = getState();
  if (viewMode !== "live" || wallFilter.type !== "cam") {
    setLastPtzCam(null);
    hidePtzModal();
    syncPtzFab();
    syncPrivacyButton();
    return;
  }
  let cams;
  try {
    cams = await fetchCameras();
  } catch {
    setLastPtzCam(null);
    hidePtzModal();
    syncPtzFab();
    syncPrivacyButton();
    return;
  }
  const cam = cams.find((c) => c.id === wallFilter.value) || null;
  setLastPtzCam(cam);
  syncPrivacyButton();
  if (!cam || !cam.capabilities || !cam.capabilities.ptz) {
    hidePtzModal();
    syncPtzFab();
    return;
  }
  // Selecting a PTZ camera no longer pops the modal open; it only refreshes an
  // already-open one (so it doesn't show a stale camera). Opening is an
  // explicit FAB click.
  if (!$("#ptz-modal")?.hidden) showPtzModal(cam);
  syncPtzFab();
}

// --- Privacy control (topbar) ---
// Privacy is per-camera but lives in the topbar: it shows only for the
// single selected camera in live view when that camera advertises the
// capability, and reflects/toggles its current state.
function syncPrivacyButton() {
  const btn = $("#privacy-toggle");
  if (!btn) return;
  const { viewMode, wallFilter, lastPtzCam } = getState();
  const cam = lastPtzCam;
  const show = viewMode === "live"
    && wallFilter.type === "cam"
    && cam?.capabilities?.privacy === true;
  if (!show) {
    btn.hidden = true;
    return;
  }
  btn.hidden = false;
  const on = cam.privacy === true;
  btn.classList.toggle("active", on);
  btn.setAttribute("aria-pressed", on ? "true" : "false");
  // Action button: the icon suggests what a click will DO, not the current
  // state. Privacy off → clicking enables it → show the crossed camera;
  // privacy on → clicking restores the feed → show the live camera.
  btn.innerHTML = on ? icon("camera") : icon("camera-off");
  btn.title = on ? t("privacy.on") : t("privacy.enable");
  btn.setAttribute("aria-label", btn.title);
}

async function togglePrivacy(cam) {
  const next = !(cam.privacy === true);
  try {
    await api(`/api/camera/${encodeURIComponent(cam.id)}/privacy?enable=${next}`, { method: "POST" });
  } catch (e) {
    alertModal(t("privacy.failed", { msg: e.message }), { title: t("privacy.title") });
    return;
  }
  // Privacy pauses/resumes the media pipeline: invalidate the cached camera
  // list (its live URLs change), flip the topbar button and the sidebar
  // thumbnail, and re-render the live wall so the tile flips to/from the
  // privacy placeholder immediately.
  cam.privacy = next;
  setCamerasCache(null);
  syncPrivacyButton();
  import("./sidebar.js")
    .then(({ setSidebarPrivacy }) => setSidebarPrivacy(cam.id, next))
    .catch(() => {});
  const { viewMode } = getState();
  if (viewMode === "live") {
    import("./wall.js").then(({ loadWall }) => loadWall("live")).catch(() => {});
  }
}

// The PTZ modal is now PTZ-only (privacy and push-to-talk moved to the
// topbar), so it always renders the pad + Home for a PTZ-capable camera.
function buildPtzPanel(cam) {
  const wrap = document.createElement("div");
  wrap.className = "ptz-panel";
  // Circular joystick-style pad: four directional buttons hug the ring
  // (absolutely positioned at N/E/S/W) around a central Home button that
  // recenters the camera. The old separate x0/y0 "center" no-op move was
  // dropped — Home is the meaningful recenter action.
  wrap.innerHTML = `
    <div class="ptz-pad" role="group" aria-label="${t("ptz.title")}">
      <span class="ptz-ring" aria-hidden="true"></span>
      <button class="ptz-dir up"    data-dpan="0"  data-dtilt="-${STEP_DEG}" title="${t("ptz.up")}"    aria-label="${t("ptz.up")}">${icon("arrow-up")}</button>
      <button class="ptz-dir right" data-dpan="${STEP_DEG}" data-dtilt="0"  title="${t("ptz.right")}" aria-label="${t("ptz.right")}">${icon("arrow-right")}</button>
      <button class="ptz-dir down"  data-dpan="0"  data-dtilt="${STEP_DEG}"  title="${t("ptz.down")}"  aria-label="${t("ptz.down")}">${icon("arrow-down")}</button>
      <button class="ptz-dir left"  data-dpan="-${STEP_DEG}" data-dtilt="0" title="${t("ptz.left")}"  aria-label="${t("ptz.left")}">${icon("arrow-left")}</button>
      <button class="ptz-home" data-go="home" title="${t("ptz.home")}" aria-label="${t("ptz.home")}">${icon("home")}</button>
    </div>
    <p class="ptz-hint">${t("ptz.hint")}</p>`;
  wrap.addEventListener("click", async (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    const base = `/api/camera/${encodeURIComponent(cam.id)}/ptz`;
    let path;
    if (btn.dataset.dpan !== undefined) {
      // Public API: pan/tilt in degrees. The server converts to firmware
      // steps using the camera's calibration and clamps to the full range,
      // so a runaway request can't command an unbounded rotation.
      path = `${base}/move?pan=${Number(btn.dataset.dpan)}&tilt=${Number(btn.dataset.dtilt)}`;
    } else if (btn.dataset.go === "home") {
      path = `${base}/home`;
    } else return;
    try {
      await api(path, { method: "POST" });
    } catch (e) {
      alertModal(t("ptz.error", { msg: e.message }), { title: t("ptz.error_title") });
    }
  });
  return wrap;
}

function initPtzDrag() {
  const ptzHandle = $("#ptz-modal-handle");
  if (!ptzHandle) return;
  const startDrag = (e) => {
    const modal = $("#ptz-modal");
    if (!modal || modal.hidden) return;
    // Drag-to-move is desktop-only: the mobile layout anchors the
    // modal to the bottom of the viewport.
    if (isMobileViewport()) return;
    const point = e.touches ? e.touches[0] : e;
    const rect = modal.getBoundingClientRect();
    ptzModalDrag = {
      startX: point.clientX,
      startY: point.clientY,
      originLeft: rect.left,
      originTop: rect.top,
    };
    modal.classList.add("dragging");
    e.preventDefault();
  };
  const moveDrag = (e) => {
    if (!ptzModalDrag) return;
    const point = e.touches ? e.touches[0] : e;
    const modal = $("#ptz-modal");
    const left = ptzModalDrag.originLeft + (point.clientX - ptzModalDrag.startX);
    const top = ptzModalDrag.originTop + (point.clientY - ptzModalDrag.startY);
    const w = modal.offsetWidth;
    const h = modal.offsetHeight;
    const clampedLeft = Math.min(Math.max(0, left), window.innerWidth - w);
    const clampedTop = Math.min(Math.max(0, top), window.innerHeight - h);
    modal.style.left = clampedLeft + "px";
    modal.style.top = clampedTop + "px";
    if (!e.touches) e.preventDefault();
  };
  const endDrag = () => {
    if (!ptzModalDrag) return;
    const modal = $("#ptz-modal");
    if (modal) {
      modal.classList.remove("dragging");
      const left = parseInt(modal.style.left, 10);
      const top = parseInt(modal.style.top, 10);
      if (!Number.isNaN(left) && !Number.isNaN(top)) savePtzModalPos(left, top);
    }
    ptzModalDrag = null;
  };
  ptzHandle.addEventListener("mousedown", startDrag);
  document.addEventListener("mousemove", moveDrag);
  document.addEventListener("mouseup", endDrag);
  ptzHandle.addEventListener("touchstart", startDrag, { passive: false });
  document.addEventListener("touchmove", moveDrag, { passive: false });
  document.addEventListener("touchend", endDrag);
  document.addEventListener("touchcancel", endDrag);
  window.addEventListener("resize", () => {
    if (!$("#ptz-modal")?.hidden) applyPtzModalPos();
  });
}

function initPtzKeyboard() {
  document.addEventListener("keydown", async (e) => {
    const { viewMode, wallFilter, lastPtzCam } = getState();
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;

    // Playback mode: + / - zoom timeline. preventDefault synchronously (before
    // the dynamic import resolves) so the key never triggers a browser default.
    if (viewMode === "playback") {
      if (e.key !== "+" && e.key !== "-") return;
      e.preventDefault();
      const { getTimeline } = await import("./playback.js");
      const tl = getTimeline();
      if (!tl) return;
      if (e.key === "+") tl.increaseInterval();
      else tl.decreaseInterval();
      return;
    }

    // Live mode + single cam + PTZ capable: arrow keys move PTZ. Same public
    // unit (degrees) as the dpad, same per-press step. Holding the key fires
    // native key repeat on the browser side, so the user can scrub by holding
    // — the server's range clamp still bounds the cumulative travel to a
    // half revolution per direction.
    if (viewMode === "live" && wallFilter.type === "cam" && lastPtzCam?.capabilities?.ptz) {
      const map = {
        ArrowUp:    { pan: 0,  tilt: -STEP_DEG },
        ArrowDown:  { pan: 0,  tilt: STEP_DEG },
        ArrowLeft:  { pan: -STEP_DEG, tilt: 0 },
        ArrowRight: { pan: STEP_DEG,  tilt: 0 },
      };
      const dir = map[e.key];
      if (dir) {
        e.preventDefault();
        try {
          await api(`/api/camera/${encodeURIComponent(lastPtzCam.id)}/ptz/move?pan=${dir.pan}&tilt=${dir.tilt}`, { method: "POST" });
        } catch (err) {
          const { alertModal } = await import("../ui/dialog.js");
          alertModal(`PTZ failed: ${err.message}`, { title: "PTZ error" });
        }
      }
    }
  });
}

export function initPtz() {
  $("#ptz-modal-close")?.addEventListener("click", hidePtzModal);
  $("#ptz-fab")?.addEventListener("click", () => {
    const { lastPtzCam } = getState();
    if (lastPtzCam?.capabilities?.ptz === true) showPtzModal(lastPtzCam);
  });
  $("#privacy-toggle")?.addEventListener("click", () => {
    const { lastPtzCam } = getState();
    if (lastPtzCam?.capabilities?.privacy === true) togglePrivacy(lastPtzCam);
  });
  initPtzDrag();
  initPtzKeyboard();
  // Re-render the PTZ panel when the wall filter changes (e.g. clicking
  // a different cam in the sidebar) or after the wall re-renders its tiles.
  on("wallFilter", () => updatePtzModal());
  on("wallRendered", () => updatePtzModal());

  // When the viewport crosses the mobile breakpoint, drop any persisted
  // desktop position so the bottom-sheet layout can take over without
  // stale inline `left`/`top` values fighting the media query.
  if (window.matchMedia) {
    const mq = window.matchMedia("(max-width: 900px)");
    const onChange = (e) => {
      const modal = $("#ptz-modal");
      if (!modal) return;
      if (e.matches) {
        modal.style.left = "";
        modal.style.top = "";
      } else if (!modal.hidden) {
        applyPtzModalPos();
      }
    };
    if (mq.addEventListener) mq.addEventListener("change", onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }
}
