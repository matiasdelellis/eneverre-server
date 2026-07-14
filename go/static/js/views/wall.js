import { $, $$, escapeHtml, makeMsg } from "../util/dom.js";
import { getState, setWallFilter, setWallFilterBeforeCam, on, emit } from "../state.js";
import { fetchCameras, token } from "../api.js";
import { loadSidebar, updateSidebarActive, publishLiveThumb } from "./sidebar.js";
import { attachMse, captureVideoFrame } from "./mse.js";
import { hidePtzModal } from "./ptz.js";
import { toast } from "../ui/toast.js";
import { setCamStatus } from "../ui/cam-status.js";

// Longest clip the 💾 button will request. Guards against a forgotten
// "recording" marker producing a multi-hour download.
const MAX_CLIP_SECONDS = 5 * 60;

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
          <button data-act="clip" title="Download clip" aria-label="Download clip">💾</button>
          <button data-act="snap" title="Snapshot" aria-label="Snapshot">📷</button>
          <button data-act="mute" title="Unmute" aria-label="Toggle audio">🔇</button>
          <button data-act="fs" title="Fullscreen" aria-label="Fullscreen">⛶</button>
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
        v.muted = !v.muted;
        btn.textContent = v.muted ? "🔇" : "🔊";
        btn.title = v.muted ? "Unmute" : "Mute";
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
          btn.textContent = "⏳";
          try {
            const { downloadClip } = await import("./playback.js");
            await downloadClip(cam.id, tile._clipStartAt, durationSec);
          } catch (err) {
            console.warn("clip download failed", err);
            btn.textContent = "❌";
            toast(`Clip download failed: ${err.message}`, { type: "error" });
            setTimeout(() => { btn.disabled = false; btn.textContent = "💾"; }, 2000);
            return;
          }
          btn.disabled = false;
          btn.textContent = "💾";
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
          btn.textContent = "🔴";
          // Live clips are wall-clock bound, so tick a visible counter.
          // Playback clips advance with the timeline, so leave the marker
          // static (the elapsed time isn't real seconds).
          if (mode === "live") {
            tile._clipTimer = setInterval(() => {
              const sec = (Date.now() - tile._clipStartAt) / 1000;
              btn.textContent = `🔴 ${fmtElapsed(Math.min(sec, MAX_CLIP_SECONDS))}`;
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
      if (video) video.replaceWith(makeMsg("🔒 Privacy — not recording"));
    } else if (cam.live_mse) {
      const m = attachMse(cam, video);
      if (m) wallInstances.set(cam.id, withThumbGrab(cam, video, m));
    } else if (video) {
      setCamStatus(cam.id, "offline");
      video.replaceWith(makeMsg("No live stream"));
    }
  }
}

// Stops any running live clip counter and restores the 💾 button to its
// idle look. `btn` is optional; when omitted it is looked up in the tile.
function resetClipButton(tile, btn) {
  if (tile._clipTimer) { clearInterval(tile._clipTimer); tile._clipTimer = null; }
  tile._clipStart = null;
  const b = btn || tile.querySelector('.wall-actions button[data-act="clip"]');
  if (b) {
    b.classList.remove("clipping");
    b.title = "Download clip";
    if (b.textContent !== "⏳") b.textContent = "💾";
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
  const filtered = filterWallCams(cams, getState().wallFilter);
  if (!cams.length) {
    wall.innerHTML = "<p class='muted wall-status'>No cameras configured.</p>";
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
      "#help-overlay:not([hidden]), #dlg-modal:not([hidden]), #user-edit-modal:not([hidden]), #users-view:not([hidden]), #device-auth-view:not([hidden])",
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
