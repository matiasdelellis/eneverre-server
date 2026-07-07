import { $, $$, escapeHtml, makeMsg } from "../util/dom.js";
import { getState, setWallFilter, setWallFilterBeforeCam, on } from "../state.js";
import { fetchCameras } from "../api.js";
import { loadSidebar, updateSidebarActive } from "./sidebar.js";
import { attachHls, getWallInstances } from "./hls.js";
import { attachMse } from "./mse.js";
import { updatePtzModal } from "./ptz.js";

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
  if (!video.videoWidth || !video.videoHeight) return;
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
}

function toggleCamFilterFromTile(camId) {
  const { wallFilter } = getState();
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
    <video autoplay playsinline muted></video>
    <div class="wall-overlay">
      <div class="wall-bottom">
        <div class="wall-name">${escapeHtml(cam.name || cam.id)}</div>
        <div class="wall-actions">
          <button data-act="snap" title="Snapshot" aria-label="Snapshot">📷</button>
          <button data-act="mute" title="Unmute" aria-label="Toggle audio">🔇</button>
          <button data-act="fs" title="Fullscreen" aria-label="Fullscreen">⛶</button>
        </div>
      </div>
    </div>
  `;
  const v = tile.querySelector("video");
  tile.addEventListener("click", (e) => {
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
  const wallInstances = getWallInstances();
  const h = wallInstances.get(cam.id);
  if (h) { try { h.destroy(); } catch {} wallInstances.delete(cam.id); }
  revokeTileBlob(tile);
  if (v && v.tagName === "VIDEO") {
    v.removeAttribute("src");
    v.load();
  } else {
    const firstEl = tile.firstElementChild;
    const fresh = document.createElement("video");
    fresh.autoplay = true; fresh.playsInline = true; fresh.muted = true;
    tile.insertBefore(fresh, firstEl);
    if (firstEl) firstEl.remove();
  }
  const video = tile.querySelector("video");
  if (mode === "live") {
    if (cam.live_mse) {
      const m = attachMse(cam, video);
      if (m) wallInstances.set(cam.id, m);
    } else if (cam.hls) {
      const nh = attachHls(cam.hls, video);
      if (nh) wallInstances.set(cam.id, nh);
    } else if (video) {
      video.replaceWith(makeMsg("No live stream"));
    }
  }
}

export function destroyWall() {
  const wallInstances = getWallInstances();
  for (const [id, h] of wallInstances) {
    try { h.destroy(); } catch {}
    const tile = $(`#wall .wall-tile[data-id="${CSS.escape(id)}"]`);
    const v = tile && tile.querySelector("video");
    if (v) { v.pause(); v.removeAttribute("src"); v.load(); }
    if (tile) revokeTileBlob(tile);
  }
  for (const tile of $$("#wall .wall-tile")) revokeTileBlob(tile);
  wallInstances.clear();
}

export function pauseWall() {
  const wallInstances = getWallInstances();
  for (const h of wallInstances.values()) {
    try { h.stopLoad(); } catch {}
  }
  $$("#wall video").forEach((v) => v.pause());
}

export function resumeWall() {
  $$("#wall video").forEach((v) => v.play().catch(() => {}));
}

export function wallSize() {
  return getWallInstances().size;
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
    if (hasRecording(cam)) {
      setTilePlaybackLoading(tile);
      camsWithData.push(cam);
    } else {
      setTileMode(tile, cam, "live");
    }
  }
  if (camsWithData.length) {
    if (myLoadGen !== getLoadGen()) return; // a newer loadWall superseded us
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
    }
    updateSidebarActive();
  }
  updatePtzModal();
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
}
