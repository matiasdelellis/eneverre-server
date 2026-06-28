import { $ } from "../util/dom.js";
import { get, set } from "../util/storage.js";
import { getState, setLastPtzCam, on } from "../state.js";
import { api, fetchCameras } from "../api.js";
import { alertModal } from "../ui/dialog.js";

const STEP = 50;
const PTZ_MODAL_POS_KEY = "eneverre.ptzModalPos";

let ptzModalDrag = null;

function privacyLabel(on) {
  return on ? "Privacy: ON" : "Privacy";
}

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
  $("#ptz-modal-title").textContent = `PTZ — ${cam.name || cam.id}`;
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
    return;
  }
  let cams;
  try {
    cams = await fetchCameras();
  } catch {
    setLastPtzCam(null);
    hidePtzModal();
    syncPtzFab();
    return;
  }
  const cam = cams.find((c) => c.id === wallFilter.value) || null;
  setLastPtzCam(cam);
  if (!cam || !cam.capabilities || !cam.capabilities.ptz) {
    hidePtzModal();
    syncPtzFab();
    return;
  }
  showPtzModal(cam);
  syncPtzFab();
}

function buildPtzPanel(cam) {
  const wrap = document.createElement("div");
  wrap.innerHTML = `
    <h3>PTZ</h3>
    <div class="ptz-pad">
      <span class="empty"></span>
      <button data-dx="0" data-dy="-${STEP}" title="Up">↑</button>
      <span class="empty"></span>
      <button data-dx="-${STEP}" data-dy="0" title="Left">←</button>
      <button data-dx="0" data-dy="0" title="Center">•</button>
      <button data-dx="${STEP}" data-dy="0" title="Right">→</button>
      <span class="empty"></span>
      <button data-dx="0" data-dy="${STEP}" title="Down">↓</button>
      <span class="empty"></span>
    </div>
    <div class="ptz-actions">
      <button data-go="home">Home</button>
      <button data-go="privacy">${privacyLabel(cam.privacy === true)}</button>
    </div>
  `;
  wrap.addEventListener("click", async (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    const base = `/api/camera/${encodeURIComponent(cam.id)}/ptz`;
    let path;
    let nextPrivacy;
    if (btn.dataset.dx !== undefined) {
      path = `${base}/move?x=${Number(btn.dataset.dx)}&y=${Number(btn.dataset.dy)}`;
    } else if (btn.dataset.go === "home") {
      path = `${base}/home`;
    } else if (btn.dataset.go === "privacy") {
      nextPrivacy = !(cam.privacy === true);
      path = `/api/camera/${encodeURIComponent(cam.id)}/privacy?enable=${nextPrivacy}`;
    } else return;
    try {
      await api(path, { method: "POST" });
      if (nextPrivacy !== undefined) {
        cam.privacy = nextPrivacy;
        btn.textContent = privacyLabel(nextPrivacy);
      }
    } catch (e) {
      alertModal(`PTZ failed: ${e.message}`, { title: "PTZ error" });
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
  // Timeline zoom: "+" enlarges the visible window (more time, less
  // detail) and "-" shrinks it. Only fires when the timeline is active
  // and the user is not typing into a field.
  document.addEventListener("keydown", async (e) => {
    const { viewMode } = getState();
    if (viewMode !== "playback") return;
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
    const { getTimeline } = await import("./playback.js");
    const tl = getTimeline();
    if (!tl) return;
    if (e.key === "+") {
      tl.increaseInterval();
      e.preventDefault();
    } else if (e.key === "-") {
      tl.decreaseInterval();
      e.preventDefault();
    }
  });
}

export function initPtz() {
  $("#ptz-modal-close")?.addEventListener("click", hidePtzModal);
  $("#ptz-fab")?.addEventListener("click", () => updatePtzModal());
  initPtzDrag();
  initPtzKeyboard();
  // Re-render the PTZ panel when the wall filter changes (e.g. clicking
  // a different cam in the sidebar).
  on("wallFilter", () => updatePtzModal());

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
