import { $ } from "../util/dom.js";
import { getState, on } from "../state.js";
import { alertModal } from "../ui/dialog.js";
import { createTalkClient } from "../util/talk-client.js";
import { icon } from "../ui/icons.js";

// Push-to-talk, split into two controls and kept separate from PTZ:
//   1. A topbar button that ARMS the mic — it requests the microphone
//      permission (or reuses an existing grant) and holds the live stream.
//   2. Once armed, a talk button appears at the bottom-centre of the selected
//      camera; clicking it (or pressing Space) opens/closes the connection to
//      the camera's ONVIF backchannel, showing a spinner while connecting.
// Splitting them moves the slow mic-permission step out of the connection path,
// so pressing talk only pays the WebSocket + RTSP dial (the spinner window).
//
// Talk is per-camera: it applies only to the single talk-capable camera
// selected in live view. syncTalk() — subscribed to the wallFilter/viewMode
// selection signals and to wallRendered (tiles recreated) — keeps the topbar
// button and the tile overlay in step and disarms when the selection moves
// away. It reads the selected cam straight from the synchronous camerasCache,
// so it depends on neither PTZ nor a network fetch.

let currentCam = null;      // selected single camera that supports talk, or null
let armed = false;          // mic permission granted + stream held
let stream = null;          // the armed mic MediaStream (this module owns it)
let client = null;          // active talk client during a session
let sessionState = "idle";  // "idle" | "connecting" | "talking"

function topBtn() { return $("#talk-toggle"); }

// --- topbar button (arm / disarm) ----------------------------------------

function renderTopButton() {
  const btn = topBtn();
  if (!btn) return;
  btn.hidden = !currentCam;
  btn.classList.toggle("active", armed);
  // Same mic glyph in both states; the .active class fills the stroke so
  // the user gets the standard "armed" visual without a second icon.
  btn.innerHTML = icon("mic");
  btn.title = armed ? "Disable talk" : "Enable talk (allow microphone)";
  btn.setAttribute("aria-label", btn.title);
  btn.setAttribute("aria-pressed", armed ? "true" : "false");
}

async function arm() {
  if (armed) return;
  let s;
  try {
    // Resolves without a prompt when the mic is already granted ("o ya lo
    // tenía"); rejects on insecure origins or when the user denies access.
    s = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
    });
  } catch (err) {
    alertModal(`Microphone access failed: ${err.message || err}`, { title: "Talk" });
    return;
  }
  // The selection may have moved while the permission prompt was up.
  if (!currentCam) { try { s.getTracks().forEach((t) => t.stop()); } catch {} return; }
  stream = s;
  armed = true;
  sessionState = "idle";
  renderTopButton();
  syncTalkOverlay();
}

function disarm() {
  if (client) { client.userStopped = true; try { client.stop(); } catch {} client = null; }
  if (stream) { try { stream.getTracks().forEach((t) => t.stop()); } catch {} stream = null; }
  armed = false;
  sessionState = "idle";
  renderTopButton();
  removeOverlay();
}

// --- talk session (connect / disconnect) ---------------------------------

function toggleTalk() {
  if (!armed || !currentCam || !stream) return;
  if (client) { client.userStopped = true; client.stop(); return; } // stop → onEnd resets to idle
  sessionState = "connecting";
  applyOverlayState();
  let becameReady = false;
  const c = createTalkClient(currentCam.id, {
    stream,
    onReady: () => { becameReady = true; if (client === c) { sessionState = "talking"; applyOverlayState(); } },
    onEnd: () => {
      // A close before "ready" that we didn't ask for means the camera
      // backchannel dial failed — surface it instead of silently reverting.
      const failed = !becameReady && !c.userStopped;
      if (client === c) { client = null; sessionState = "idle"; applyOverlayState(); }
      if (failed) alertModal("Couldn't connect talk audio to the camera.", { title: "Talk" });
    },
  });
  c.userStopped = false;
  client = c;
  try {
    c.start();
  } catch (err) {
    // Synchronous failure (e.g. AudioContext blocked): onEnd never fires here.
    if (client === c) { client = null; sessionState = "idle"; applyOverlayState(); }
    alertModal(`Couldn't start talk audio: ${err.message || err}`, { title: "Talk" });
  }
}

// --- tile overlay (bottom-centre talk trigger) ---------------------------

function currentTile() {
  if (!currentCam) return null;
  return $(`#wall .wall-tile[data-id="${CSS.escape(currentCam.id)}"]`);
}

function removeOverlay() {
  for (const el of document.querySelectorAll(".wall-talk")) el.remove();
}

function applyOverlayState() {
  const btn = document.querySelector(".wall-talk");
  if (!btn) return;
  btn.classList.toggle("connecting", sessionState === "connecting");
  btn.classList.toggle("talking", sessionState === "talking");
  if (sessionState === "connecting") {
    // The spinner is drawn by CSS (.wall-talk.connecting::before); leave the
    // button empty so we don't stack a second, non-spinning loader glyph.
    btn.innerHTML = "";
    btn.title = "Connecting…";
  } else if (sessionState === "talking") {
    btn.innerHTML = icon("mic-off");
    btn.title = "Talking — click or Space to stop";
  } else {
    btn.innerHTML = icon("mic");
    btn.title = "Click or Space to talk";
  }
  btn.setAttribute("aria-label", btn.title);
}

// Ensure the overlay button exists in the selected tile (re-injecting after a
// wall re-render, which recreates tiles) and reflects the current session
// state; remove it when not armed or the tile is gone.
function syncTalkOverlay() {
  const tile = armed ? currentTile() : null;
  if (!tile) { removeOverlay(); return; }
  // Drop any stray overlay left on a different tile.
  for (const el of document.querySelectorAll(".wall-talk")) {
    if (el.parentElement !== tile) el.remove();
  }
  let btn = tile.querySelector(".wall-talk");
  if (!btn) {
    btn = document.createElement("button");
    btn.type = "button";
    btn.className = "wall-talk";
    btn.addEventListener("click", (e) => { e.stopPropagation(); toggleTalk(); });
    tile.appendChild(btn);
  }
  applyOverlayState();
}

// --- selection sync (subscribed to state signals) ------------------------

function syncTalk() {
  const { viewMode, wallFilter, camerasCache } = getState();
  let cam = null;
  if (viewMode === "live" && wallFilter.type === "cam") {
    cam = (camerasCache || []).find((c) => c.id === wallFilter.value) || null;
    if (cam?.capabilities?.talk !== true) cam = null;
  }

  const changed = (cam?.id || null) !== (currentCam?.id || null);
  currentCam = cam;
  // Leaving the armed camera (switched cams, or left single-cam/live) releases
  // the mic and any in-flight session.
  if (armed && changed) disarm();
  renderTopButton();
  syncTalkOverlay();
}

export function initTalk() {
  topBtn()?.addEventListener("click", () => { if (armed) disarm(); else arm(); });

  // Resync on selection changes and after the wall recreates its tiles (the
  // latter re-injects the per-tile overlay). No PTZ, no fetch.
  on("wallFilter", syncTalk);
  on("viewMode", syncTalk);
  on("wallRendered", syncTalk);

  // Space toggles the session while armed (walkie-talkie feel), unless the user
  // is typing. preventDefault stops the page scroll and any focused-button
  // double-activation.
  document.addEventListener("keydown", (e) => {
    if (e.code !== "Space" && e.key !== " ") return;
    if (!armed || !currentCam || !currentTile()) return;
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
    e.preventDefault();
    toggleTalk();
  });
}
