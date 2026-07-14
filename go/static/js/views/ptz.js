import { $ } from "../util/dom.js";
import { get, set } from "../util/storage.js";
import { getState, setLastPtzCam, setCamerasCache, on } from "../state.js";
import { api, fetchCameras } from "../api.js";
import { alertModal } from "../ui/dialog.js";

const STEP = 50;
const PTZ_MODAL_POS_KEY = "eneverre.ptzModalPos";

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
  const kind = cam.capabilities?.ptz ? "PTZ" : "Control";
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
  btn.textContent = on ? "🔒" : "🔓";
  btn.title = on ? "Privacy on — click to resume recording" : "Enable privacy";
  btn.setAttribute("aria-label", btn.title);
}

async function togglePrivacy(cam) {
  const next = !(cam.privacy === true);
  try {
    await api(`/api/camera/${encodeURIComponent(cam.id)}/privacy?enable=${next}`, { method: "POST" });
  } catch (e) {
    alertModal(`Privacy failed: ${e.message}`, { title: "Privacy error" });
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
    <div class="ptz-actions"><button data-go="home">Home</button></div>`;
  wrap.addEventListener("click", async (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    const base = `/api/camera/${encodeURIComponent(cam.id)}/ptz`;
    let path;
    if (btn.dataset.dx !== undefined) {
      path = `${base}/move?x=${Number(btn.dataset.dx)}&y=${Number(btn.dataset.dy)}`;
    } else if (btn.dataset.go === "home") {
      path = `${base}/home`;
    } else return;
    try {
      await api(path, { method: "POST" });
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
  document.addEventListener("keydown", async (e) => {
    const { viewMode, wallFilter, lastPtzCam } = getState();
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;

    // Playback mode: + / - zoom timeline
    if (viewMode === "playback") {
      const { getTimeline } = await import("./playback.js");
      const tl = getTimeline();
      if (!tl) return;
      if (e.key === "+") { tl.increaseInterval(); e.preventDefault(); }
      else if (e.key === "-") { tl.decreaseInterval(); e.preventDefault(); }
      return;
    }

    // Live mode + single cam + PTZ capable: arrow keys move PTZ
    if (viewMode === "live" && wallFilter.type === "cam" && lastPtzCam?.capabilities?.ptz) {
      const map = {
        ArrowUp:    { x: 0, y: -50 },
        ArrowDown:  { x: 0, y: 50 },
        ArrowLeft:  { x: -50, y: 0 },
        ArrowRight: { x: 50, y: 0 },
      };
      const dir = map[e.key];
      if (dir) {
        e.preventDefault();
        try {
          await api(`/api/camera/${encodeURIComponent(lastPtzCam.id)}/ptz/move?x=${dir.x}&y=${dir.y}`, { method: "POST" });
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
