import { $, $$, escapeHtml, makeMsg } from "../util/dom.js";
import { getState, setWallFilter, setWallFilterBeforeCam, on, emit } from "../state.js";
import { fetchCameras, token } from "../api.js";
import { loadSidebar, updateSidebarActive, publishLiveThumb } from "./sidebar.js";
import { attachMse, captureVideoFrame } from "./mse.js";
import { hidePtzModal } from "./ptz.js";
import { toast } from "../ui/toast.js";
import { setCamStatus } from "../ui/cam-status.js";
import { icon } from "../ui/icons.js";
import { loadJson, USER_KEY } from "../util/storage.js";

// Longest clip the save button will request. Guards against a forgotten
// "recording" marker producing a multi-hour download.
const MAX_CLIP_SECONDS = 5 * 60;

// Reflect a video's muted state on its mute button (glyph + tooltip).
function setMuteButton(btn, muted) {
  if (!btn) return;
  btn.innerHTML = icon(muted ? "volume-x" : "volume-2");
  btn.title = muted ? "Unmute" : "Mute";
}

// Mute every wall tile except `keep`, updating each mute button so the
// wall never has more than one camera playing audio at once.
function muteAllTiles(keep) {
  for (const t of $$("#wall .wall-tile")) {
    if (t === keep) continue;
    const video = t.querySelector("video");
    if (video && !video.muted) {
      video.muted = true;
      setMuteButton(t.querySelector('button[data-act="mute"]'), true);
    }
  }
}

// mm:ss for the live clip counter.
function fmtElapsed(sec) {
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  return `${m}:${String(s).padStart(2, "0")}`;
}

// camId -> wall live instance (the MSE handle from attachMse, wrapped so its
// .destroy() also stops the thumbnail grabber). destroyWall() calls .destroy()
// on every entry.
const wallInstances = new Map();

// The single camera currently playing audio (only one at a time). Remembered
// across wall re-renders — selecting a camera or location rebuilds the tiles,
// and the previously unmuted camera keeps its audio if it's still shown.
let audioCamId = null;

// While a tile plays live, grab a frame every THUMB_GRAB_MS and push it to the
// sidebar thumbnail — reusing the video the browser is already decoding rather
// than opening a second stream.
const THUMB_GRAB_MS = 15000;

// withThumbGrab wraps a live handle so the periodic thumbnail grabber shares its
// lifecycle: the interval is cleared when the tile is torn down or replaced.
function withThumbGrab(cam, video, handle) {
  const grab = () => {
    if (video.paused) return; // paused/off-screen: keep the last pushed frame
    publishLiveThumb(cam.id, captureVideoFrame(video, { maxWidth: 480 }));
  };
  const kick = setTimeout(grab, 4000); // first frame shortly after the stream starts
  const iv = setInterval(grab, THUMB_GRAB_MS);
  return {
    destroy() {
      clearTimeout(kick);
      clearInterval(iv);
      try { handle.destroy(); } catch {}
    },
    stopLoad() { try { handle.stopLoad?.(); } catch {} },
  };
}

export function getWallInstances() { return wallInstances; }

function isWallLike() {
  const v = getState().viewMode;
  return v === "live" || v === "playback";
}

function filterWallCams(cams, filter) {
  if (filter.type === "cam") return cams.filter((c) => c.id === filter.value);
  if (filter.type === "loc") return cams.filter((c) => (c.location || "") === filter.value);
  return cams;
}

export function gridLayout(count) {
  const wall = document.getElementById("wall");
  const W = wall ? wall.clientWidth : window.innerWidth;
  const H = wall ? wall.clientHeight : window.innerHeight;
  if (W <= 0 || H <= 0) return { cols: 1, tilePct: 100 };
  let best = { cols: 1, rows: count, tileW: 0 };
  for (let rows = 1; rows <= count; rows++) {
    const cols = Math.ceil(count / rows);
    const maxByCols = W / cols;
    const maxByRows = (H / rows) * 16 / 9;
    const tileW = Math.min(maxByCols, maxByRows);
    if (tileW > best.tileW) best = { cols, rows, tileW };
  }
  return { cols: best.cols, tilePct: (best.tileW / W) * 100 };
}

function snapshotTile(tile, video, cam) {
  if (!video.videoWidth || !video.videoHeight) {
    toast("Snapshot unavailable: no video frame yet", { type: "error" });
    return;
  }
  const canvas = document.createElement("canvas");
  canvas.width = video.videoWidth;
  canvas.height = video.videoHeight;
  const ctx = canvas.getContext("2d");
  ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
  const url = canvas.toDataURL("image/jpeg", 0.92);
  const a = document.createElement("a");
  const d = new Date();
  const pad = (n) => String(n).padStart(2, "0");
  const stamp = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}_${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
  a.href = url;
  a.download = `eneverre_${cam.id}_${stamp}.jpg`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  toast("Snapshot saved", { type: "success" });
}

async function tryLoadThumbnail(tile, camId) {
  if (!camId) return;
  try {
    const t = token();
    if (!t) return;
    const resp = await fetch(`/api/camera/${encodeURIComponent(camId)}/thumbnail`, {
      headers: { Authorization: `Bearer ${t}` },
    });
    if (!resp.ok) return;
    const blob = await resp.blob();
    const url = URL.createObjectURL(blob);
    if (tile._posterBlob) URL.revokeObjectURL(tile._posterBlob);
    tile._posterBlob = url;
    tile.dataset.poster = url;
    const video = tile.querySelector("video");
    if (video) video.poster = url;
  } catch {}
}

function toggleCamFilterFromTile(camId) {
  const { wallFilter, wallFilterBeforeCam } = getState();
  if (wallFilter.type === "cam" && wallFilter.value === camId) {
    setWallFilter(wallFilterBeforeCam || { type: "all" });
    setWallFilterBeforeCam(null);
  } else {
    setWallFilterBeforeCam(wallFilter);
    setWallFilter({ type: "cam", value: camId });
  }
}

function renderWallTile(cam) {
  const tile = document.createElement("div");
  tile.className = "wall-tile";
  tile.dataset.id = cam.id;
  tile.dataset.mode = "live";
  tile.innerHTML = `
    <video autoplay playsinline muted poster="/img/camera-banner.png"></video>
    <span class="cam-status-dot connecting" data-cam="${escapeHtml(cam.id)}" title="Connecting…" aria-label="Connecting…"></span>
    <div class="wall-overlay">
      <div class="wall-bottom">
        <div class="wall-name">${escapeHtml(cam.name || cam.id)}</div>
        <div class="wall-actions">
          <button data-act="clip" title="Download clip" aria-label="Download clip">${icon("save")}</button>
          <button data-act="snap" title="Snapshot" aria-label="Snapshot">${icon("camera")}</button>
          <button data-act="mute" title="Unmute" aria-label="Toggle audio">${icon("volume-x")}</button>
          <button data-act="fs" title="Fullscreen" aria-label="Fullscreen">${icon("maximize")}</button>
        </div>
      </div>
    </div>
  `;
  const v = tile.querySelector("video");
  tile.addEventListener("click", async (e) => {
    const btn = e.target.closest("button[data-act]");
    if (btn) {
      e.stopPropagation();
      if (btn.dataset.act === "mute") {
        const unmuting = v.muted;
        // Only one camera plays audio at a time: mute every other tile
        // before unmuting this one.
        if (unmuting) muteAllTiles(tile);
        v.muted = !unmuting;
        audioCamId = unmuting ? cam.id : null;
        setMuteButton(btn, v.muted);
      } else if (btn.dataset.act === "fs") {
        if (document.fullscreenElement === tile) document.exitFullscreen();
        else tile.requestFullscreen?.();
      } else if (btn.dataset.act === "snap") {
        snapshotTile(tile, v, cam);
      } else if (btn.dataset.act === "clip") {
        if (tile._clipStart) {
          // Second click: mark the end using the same clock as the start
          // (wall-clock in live, timeline position in playback) so the
          // duration is measured on a single time base.
          let endMsec;
          if (tile.dataset.mode === "live") {
            endMsec = Date.now();
          } else {
            const { getTimeline } = await import("./playback.js");
            const tl = getTimeline();
            endMsec = tl ? tl.getCurrent() : Date.now();
          }
          resetClipButton(tile, btn);
          let durationSec = (endMsec - tile._clipStartAt) / 1000;
          tile._clipStart = null;
          if (!(durationSec > 0)) {
            // End is at or before the start (double-click, scrub back):
            // nothing to download, just reset the button.
            return;
          }
          if (durationSec > MAX_CLIP_SECONDS) {
            durationSec = MAX_CLIP_SECONDS;
            toast(`Clip capped at ${MAX_CLIP_SECONDS / 60} min`, { type: "info" });
          }
          btn.disabled = true;
          btn.innerHTML = icon("loader");
          try {
            const { downloadClip } = await import("./playback.js");
            await downloadClip(cam.id, tile._clipStartAt, durationSec);
          } catch (err) {
            console.warn("clip download failed", err);
            btn.innerHTML = icon("x-circle");
            toast(`Clip download failed: ${err.message}`, { type: "error" });
            setTimeout(() => { btn.disabled = false; btn.innerHTML = icon("save"); }, 2000);
            return;
          }
          btn.disabled = false;
          btn.innerHTML = icon("save");
          toast(`Clip saved (${fmtElapsed(durationSec)})`, { type: "success" });
        } else {
          // First click: mark start time
          const mode = tile.dataset.mode;
          let startMsec;
          if (mode === "live") {
            startMsec = Date.now();
          } else {
            const { getTimeline } = await import("./playback.js");
            const tl = getTimeline();
            startMsec = tl ? tl.getCurrent() : Date.now();
          }
          tile._clipStart = true;
          tile._clipStartAt = startMsec;
          btn.classList.add("clipping");
          btn.title = "Click again to end clip";
          btn.innerHTML = icon("circle-dot");
          // Live clips are wall-clock bound, so tick a visible counter.
          // Playback clips advance with the timeline, so leave the marker
          // static (the elapsed time isn't real seconds).
          if (mode === "live") {
            tile._clipTimer = setInterval(() => {
              const sec = (Date.now() - tile._clipStartAt) / 1000;
              btn.innerHTML = `${icon("circle-dot")} ${fmtElapsed(Math.min(sec, MAX_CLIP_SECONDS))}`;
            }, 500);
          }
        }
      }
      return;
    }
    if (document.fullscreenElement === tile) document.exitFullscreen();
    toggleCamFilterFromTile(cam.id);
  });
  return tile;
}

function revokeTileBlob(tile) {
  if (tile && tile._blobUrl) {
    try { URL.revokeObjectURL(tile._blobUrl); } catch {}
    tile._blobUrl = null;
  }
  if (tile && tile._playbackReq) {
    tile._playbackReq.cancelled = true;
    tile._playbackReq = null;
  }
}

export function setTileMode(tile, cam, mode, _opts = {}) {
  const v = tile.querySelector("video");
  tile.dataset.mode = mode;
  const h = wallInstances.get(cam.id);
  if (h) { try { h.destroy(); } catch {} wallInstances.delete(cam.id); }
  revokeTileBlob(tile);
  revokeTilePoster(tile);
  resetClipButton(tile);
  if (v && v.tagName === "VIDEO") {
    v.removeAttribute("src");
    v.load();
  } else {
    const firstEl = tile.firstElementChild;
    const fresh = document.createElement("video");
    fresh.autoplay = true; fresh.playsInline = true; fresh.muted = true;
    fresh.poster = tile.dataset.poster || "/img/camera-banner.png";
    tile.insertBefore(fresh, firstEl);
    if (firstEl) firstEl.remove();
  }
  const video = tile.querySelector("video");
  if (mode === "live") {
    if (cam.privacy === true) {
      // Camera is in privacy: the engine has stopped recording and streaming,
      // so there is no live feed to attach. Show a placeholder instead of
      // hammering a dead MSE endpoint.
      setCamStatus(cam.id, "offline");
      if (video) {
        const p = makeMsg("");
        p.innerHTML = `${icon("lock")} Privacy — not recording`;
        video.replaceWith(p);
      }
    } else if (cam.live_mse) {
      const m = attachMse(cam, video);
      if (m) wallInstances.set(cam.id, withThumbGrab(cam, video, m));
    } else if (video) {
      setCamStatus(cam.id, "offline");
      video.replaceWith(makeMsg("No live stream"));
    }
  }
}

// Stops any running live clip counter and restores the save button to its
// idle look. `btn` is optional; when omitted it is looked up in the tile.
function resetClipButton(tile, btn) {
  if (tile._clipTimer) { clearInterval(tile._clipTimer); tile._clipTimer = null; }
  tile._clipStart = null;
  const b = btn || tile.querySelector('.wall-actions button[data-act="clip"]');
  if (b) {
    b.classList.remove("clipping");
    b.title = "Download clip";
    // Don't clobber the in-flight loader glyph when the button is disabled
    // (a click that already started a download and is still mid-fetch).
    if (!b.disabled) b.innerHTML = icon("save");
  }
}

function revokeTilePoster(tile) {
  if (tile._posterBlob) { URL.revokeObjectURL(tile._posterBlob); tile._posterBlob = null; }
}

export function destroyWall() {
  for (const [id, h] of wallInstances) {
    try { h.destroy(); } catch {}
    const tile = $(`#wall .wall-tile[data-id="${CSS.escape(id)}"]`);
    const v = tile && tile.querySelector("video");
    if (v) { v.pause(); v.removeAttribute("src"); v.load(); }
    if (tile) { revokeTileBlob(tile); revokeTilePoster(tile); }
  }
  for (const tile of $$("#wall .wall-tile")) { revokeTileBlob(tile); revokeTilePoster(tile); resetClipButton(tile); }
  wallInstances.clear();
}

export function pauseWall() {
  for (const h of wallInstances.values()) {
    try { h.stopLoad(); } catch {}
  }
  $$("#wall video").forEach((v) => v.pause());
}

export function resumeWall() {
  $$("#wall video").forEach((v) => v.play().catch(() => {}));
}

export function wallSize() {
  return wallInstances.size;
}

async function applyPlayback(filtered) {
  // claimPlaybackLoad bumps pbLoadGen first so the gen check below is
  // meaningful: a teardown-only bump (the previous design) invalidated
  // the caller itself because teardown ran between the capture and the
  // comparison, so applyPlayback always returned early after the first
  // call and the wall was never populated.
  const { claimPlaybackLoad, teardownPlaybackTimeline, buildPlaybackTimeline, setTilePlaybackLoading, startVodPlayback, getLoadGen } = await import("./playback.js");
  const myLoadGen = claimPlaybackLoad();
  teardownPlaybackTimeline();
  const tl = await buildPlaybackTimeline(filtered);
  if (tl === null) return;
  if (myLoadGen !== getLoadGen()) return; // a newer loadWall superseded us
  const { initMsec, hasRecording } = tl;
  // The timeline canvas now has its real height, so the playback-bar
  // has settled and the content-area has shrunk to its final size.
  // Recompute the grid here so tiles fill the available space.
  const { cols, tilePct } = gridLayout(filtered.length);
  const wall = document.getElementById("wall");
  wall.style.setProperty("--cols", cols);
  wall.style.setProperty("--tile-w", `${tilePct}%`);
  wall.innerHTML = "";
  const camsWithData = [];
  for (const cam of filtered) {
    const tile = renderWallTile(cam);
    wall.appendChild(tile);
    tryLoadThumbnail(tile, cam.id);
    if (hasRecording(cam)) {
      setTilePlaybackLoading(tile);
      camsWithData.push(cam);
    } else {
      setTileMode(tile, cam, "live");
    }
  }
  if (camsWithData.length) {
    if (myLoadGen !== getLoadGen()) return;
    startVodPlayback(camsWithData, initMsec);
  }
}

// Full-area empty state shown when no cameras exist at all (a fresh install
// or after the last camera is removed). Admins get a call-to-action that
// jumps straight into the add-camera wizard; everyone else gets a hint to
// ask an admin, since they can't add cameras themselves.
function renderWallEmpty(wall) {
  const isAdmin = !!loadJson(USER_KEY)?.is_admin;
  const empty = document.createElement("div");
  empty.className = "wall-empty wall-status";
  empty.innerHTML = `
    <div class="wall-empty-icon" aria-hidden="true">${icon("camera")}</div>
    <h2 class="wall-empty-title">No cameras yet</h2>
    <p class="wall-empty-text">${
      isAdmin
        ? "Add your first camera to start watching live video and browsing recordings."
        : "No cameras have been set up yet. Ask an administrator to add one."
    }</p>
    ${isAdmin ? '<button class="primary wall-empty-cta" id="wall-add-camera">+ Add camera</button>' : ""}`;
  wall.replaceChildren(empty);
  if (isAdmin) {
    $("#wall-add-camera").addEventListener("click", async () => {
      const { enterCamerasView } = await import("./cameras.js");
      enterCamerasView();
    });
  }
}

export async function loadWall(mode = "live") {
  const wall = $("#wall");
  destroyWall();
  await loadSidebar();
  wall.innerHTML = "<p class='muted wall-status'>Loading…</p>";
  let cams;
  try {
    cams = await fetchCameras();
  } catch (e) {
    wall.innerHTML = `<p class="error wall-status">Failed to load cameras: ${escapeHtml(e.message)}</p>`;
    updateSidebarActive();
    return;
  }
  // Playback only makes sense when some camera advertises it (recording is
  // off by default, so a fresh install has none). Hide the whole Live/Playback
  // switch otherwise — a lone "Live" button toggles nothing — and fall back to
  // live if we were asked for playback with nothing to play back.
  const anyPlayback = cams.some((c) => c.capabilities?.playback);
  $(".view-toggle").hidden = !anyPlayback;
  if (mode === "playback" && !anyPlayback) {
    const { setViewMode } = await import("./app-shell.js");
    setViewMode("live"); // re-enters loadWall("live"); this invocation stops here
    return;
  }
  const filtered = filterWallCams(cams, getState().wallFilter);
  // With no cameras at all the camera list is dead weight, so collapse the
  // whole sidebar (and its mobile opener) and let the empty state own the
  // full width. Cleared again as soon as a camera exists.
  $("#app").classList.toggle("no-cameras", !cams.length);
  if (!cams.length) {
    renderWallEmpty(wall);
    updateSidebarActive();
    return;
  }
  if (!filtered.length) {
    const filter = getState().wallFilter;
    let msg;
    if (filter.type === "cam") msg = "Camera not found.";
    else if (filter.type === "loc") msg = `No cameras in <strong>${escapeHtml(filter.value)}</strong>.`;
    else msg = "No cameras match the current filter.";
    wall.innerHTML = `<p class='muted wall-status'>${msg} <button class="ghost" id="wall-clear">Show all</button></p>`;
    $("#wall-clear").addEventListener("click", () => setWallFilter({ type: "all" }));
    updateSidebarActive();
    return;
  }
  wall.innerHTML = "";
  const { cols, tilePct } = gridLayout(filtered.length);
  wall.style.setProperty("--cols", cols);
  wall.style.setProperty("--tile-w", `${tilePct}%`);
  if (mode === "playback") {
    wall.innerHTML = "<p class='muted wall-status'>Loading recordings…</p>";
    updateSidebarActive();
    await applyPlayback(filtered);
    updateSidebarActive();
  } else {
    const { teardownPlaybackTimeline } = await import("./playback.js");
    teardownPlaybackTimeline();
    for (const cam of filtered) {
      const tile = renderWallTile(cam);
      setTileMode(tile, cam, "live");
      // Keep audio on the camera that was playing before this re-render.
      if (cam.id === audioCamId) {
        const v = tile.querySelector("video");
        if (v) {
          v.muted = false;
          setMuteButton(tile.querySelector('button[data-act="mute"]'), false);
        }
      }
      wall.appendChild(tile);
      tryLoadThumbnail(tile, cam.id);
    }
    updateSidebarActive();
  }
  // Tiles were (re)created: let the topbar controls that inject per-tile
  // overlays or track the selected cam resync. PTZ and talk both subscribe.
  emit("wallRendered");
}

export function initWall() {
  // Reload the wall (or just refresh the sidebar highlight) when the
  // wall filter changes. In playback we need to preserve the cursor
  // state across the rebuild, so capture it before reloading.
  on("wallFilter", async () => {
    const { viewMode } = getState();
    if (viewMode === "live" || viewMode === "playback") {
      const { captureTimelineState } = await import("./playback.js");
      captureTimelineState();
      await loadWall(viewMode);
    } else {
      updateSidebarActive();
    }
  });

  // Escape walks the filter up one level: single camera → its location,
  // location → all cameras. Ignored while typing or when a blocking
  // overlay/dialog is open (help and dialogs own Escape themselves).
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
    if (document.querySelector(
      "#help-overlay:not([hidden]), #dlg-modal:not([hidden]), #user-edit-modal:not([hidden]), #users-view:not([hidden]), #cameras-view:not([hidden]), #cam-wizard-modal:not([hidden]), #device-auth-view:not([hidden])",
    )) return;
    const { wallFilter, camerasCache } = getState();
    if (wallFilter.type === "cam") {
      // In camera view an open PTZ panel takes priority: close it first,
      // and only step out to the location on a subsequent Escape.
      if (document.querySelector("#ptz-modal:not([hidden])")) {
        hidePtzModal();
        e.preventDefault();
        return;
      }
      const cam = (camerasCache || []).find((c) => c.id === wallFilter.value);
      const loc = cam && cam.location;
      setWallFilter(loc ? { type: "loc", value: loc } : { type: "all" });
      e.preventDefault();
    } else if (wallFilter.type === "loc") {
      setWallFilter({ type: "all" });
      e.preventDefault();
    }
  });
}
