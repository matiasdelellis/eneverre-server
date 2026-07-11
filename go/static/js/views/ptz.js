import { $ } from "../util/dom.js";
import { get, set } from "../util/storage.js";
import { getState, setLastPtzCam, setCamerasCache, on } from "../state.js";
import { api, fetchCameras } from "../api.js";
import { alertModal } from "../ui/dialog.js";
import { createTalkClient } from "../util/talk-client.js";

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
  return lastPtzCam?.capabilities?.ptz === true || lastPtzCam?.capabilities?.talk === true || lastPtzCam?.capabilities?.privacy === true;
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
  if (!cam || !cam.capabilities || !(cam.capabilities.ptz || cam.capabilities.talk || cam.capabilities.privacy)) {
    hidePtzModal();
    syncPtzFab();
    return;
  }
  showPtzModal(cam);
  syncPtzFab();
}

function buildPtzPanel(cam) {
  const wrap = document.createElement("div");
  const hasPtz = cam.capabilities?.ptz === true;
  const hasTalk = cam.capabilities?.talk === true;
  const hasPrivacy = cam.capabilities?.privacy === true;
  let html = `<h3>${hasPtz ? "PTZ" : "Control"}</h3>`;
  if (hasPtz) {
    html += `
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
    </div>`;
  }
  // Privacy stops recording + transmission for any capable camera; Home is
  // PTZ-only. Render the actions row whenever either is available.
  if (hasPtz || hasPrivacy) {
    html += `<div class="ptz-actions">`;
    if (hasPtz) html += `<button data-go="home">Home</button>`;
    if (hasPrivacy) html += `<button data-go="privacy">${privacyLabel(cam.privacy === true)}</button>`;
    html += `</div>`;
  }
  if (hasTalk) {
    html += `
    <div class="ptz-talk">
      <button type="button" class="talk-btn" data-talk>🎤 Hold to talk</button>
    </div>`;
  }
  wrap.innerHTML = html;
  if (hasTalk) wireTalkButton(wrap.querySelector("[data-talk]"), cam);
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
        // Privacy pauses/resumes the media pipeline: invalidate the cached
        // camera list (its live URLs change) and re-render the live wall so
        // the tile flips to/from the privacy placeholder immediately.
        setCamerasCache(null);
        const { viewMode } = getState();
        if (viewMode === "live") {
          import("./wall.js").then(({ loadWall }) => loadWall("live")).catch(() => {});
        }
      }
    } catch (e) {
      alertModal(`PTZ failed: ${e.message}`, { title: "PTZ error" });
    }
  });
  return wrap;
}

// wireTalkButton turns the button into a press-and-hold push-to-talk control:
// the mic streams while the pointer is held down and stops on release or a
// server-side close. It uses pointer capture so the release is delivered even if
// the pointer drifts off the button — no document-level listener to leak across
// modal re-renders.
function wireTalkButton(btn, cam) {
  if (!btn) return;
  const idle = "🎤 Hold to talk";
  const connecting = "⏳ Connecting…";
  const live = "🔴 Talking — go ahead";
  const setState = (cls, text) => {
    btn.classList.remove("connecting", "talking");
    if (cls) btn.classList.add(cls);
    btn.textContent = text;
  };
  const reset = () => setState(null, idle);
  const client = createTalkClient(cam.id, {
    onReady: () => { if (client.isActive()) setState("talking", live); },
    onEnd: reset,
  });
  const begin = (e) => {
    e.preventDefault();
    if (client.isActive()) return;
    try { btn.setPointerCapture(e.pointerId); } catch {}
    setState("connecting", connecting);
    client.start().catch((err) => {
      reset();
      alertModal(`Microphone/connection error: ${err.message || err}`, { title: "Talk error" });
    });
  };
  const end = (e) => {
    e.preventDefault();
    try { if (e.pointerId != null) btn.releasePointerCapture(e.pointerId); } catch {}
    client.stop();
  };
  btn.addEventListener("pointerdown", begin);
  btn.addEventListener("pointerup", end);
  btn.addEventListener("pointercancel", end);
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
