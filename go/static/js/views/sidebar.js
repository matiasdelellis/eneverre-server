import { $, $$, escapeHtml } from "../util/dom.js";
import { setWallFilter, setWallFilterBeforeCam, getState, on } from "../state.js";
import { fetchCameras } from "../api.js";
import { captureFrame } from "./hls.js";
import { isMobileViewport, closeSidebarDrawer } from "./app-shell.js";

function maybeCloseDrawer() {
  // Only auto-close the camera list on mobile, where it overlays the
  // wall. On desktop the user controls the collapse state explicitly
  // via the sidebar toggle button and clicking a thumb shouldn't
  // override that.
  if (isMobileViewport()) closeSidebarDrawer();
}

const THUMB_CACHE = new Map(); // camId -> dataURL (in-memory, faster than localStorage)

const SIDEBAR_THUMB_REFRESH_MS = 10 * 60 * 1000;
let sidebarThumbTimer = null;
let sidebarThumbCams = [];

export function groupByLocation(cams) {
  const byLoc = new Map();
  for (const cam of cams) {
    const loc = cam.location || "Other";
    if (!byLoc.has(loc)) byLoc.set(loc, []);
    byLoc.get(loc).push(cam);
  }
  for (const list of byLoc.values()) {
    list.sort((a, b) => (a.name || a.id).localeCompare(b.name || b.id, undefined, { sensitivity: "base" }));
  }
  const keys = [...byLoc.keys()].sort((a, b) => a.localeCompare(b, undefined, { sensitivity: "base" }));
  return { keys, byLoc };
}

function isWallLike() {
  const v = getState().viewMode;
  return v === "live" || v === "playback";
}

function renderViewerThumb(cam) {
  const tile = document.createElement("div");
  tile.className = "viewer-thumb";
  tile.dataset.id = cam.id;
  tile.dataset.location = cam.location || "";
  tile.innerHTML = `
    <div class="thumb-preview">
      <img alt="" />
      <span class="thumb-loading">Loading…</span>
      <div class="thumb-caption">${escapeHtml(cam.name || cam.id)}</div>
    </div>
  `;
  tile.addEventListener("click", () => {
    onSidebarThumbClick(cam);
    maybeCloseDrawer();
  });
  loadViewerThumb(cam, tile);
  return tile;
}

export function updateSidebarActive() {
  const filter = getState().wallFilter;
  for (const t of $$("#viewer-side-scroll .viewer-thumb")) t.classList.remove("active");
  for (const h of $$("#viewer-side-scroll .viewer-location-header")) h.classList.remove("active");
  if (!isWallLike()) return;
  if (filter.type === "cam") {
    const t = $(`#viewer-side-scroll .viewer-thumb[data-id="${CSS.escape(filter.value)}"]`);
    if (t) {
      t.classList.add("active");
      const loc = t.dataset.location || "";
      if (loc) {
        const h = $(`#viewer-side-scroll .viewer-location-header[data-location="${CSS.escape(loc)}"]`);
        if (h) h.classList.add("active");
      }
    }
  } else if (filter.type === "loc") {
    const h = $(`#viewer-side-scroll .viewer-location-header[data-location="${CSS.escape(filter.value)}"]`);
    if (h) h.classList.add("active");
    for (const t of $$("#viewer-side-scroll .viewer-thumb")) {
      if ((t.dataset.location || "") === filter.value) t.classList.add("active");
    }
  }
}

export async function loadSidebar() {
  const side = $("#viewer-side-scroll");
  if (side.dataset.loaded) return;
  side.innerHTML = "<p class='muted viewer-status'>Loading…</p>";
  let cams;
  try {
    cams = await fetchCameras();
  } catch (e) {
    side.innerHTML = `<p class="error viewer-status">Failed to load cameras: ${escapeHtml(e.message)}</p>`;
    return;
  }
  if (!cams.length) {
    side.innerHTML = "<p class='muted viewer-status'>No cameras configured.</p>";
    return;
  }
  side.innerHTML = "";
  const groups = groupByLocation(cams);
  for (const loc of groups.keys) {
    const header = document.createElement("div");
    header.className = "viewer-location-header";
    header.dataset.location = loc;
    header.title = `Filter wall to ${loc}`;
    header.innerHTML = `<span class="loc-icon" aria-hidden="true">▤</span><span class="loc-text">${escapeHtml(loc)}</span>`;
    header.addEventListener("click", () => {
      if (isWallLike()) {
        toggleLocFilter(loc);
        maybeCloseDrawer();
      }
    });
    side.appendChild(header);
    for (const cam of groups.byLoc.get(loc)) {
      side.appendChild(renderViewerThumb(cam));
    }
  }
  side.dataset.loaded = "1";
  updateSidebarActive();
  startSidebarThumbRefresh(cams);
}

function toggleCamFilter(camId) {
  const { wallFilter, wallFilterBeforeCam } = getState();
  if (wallFilter.type === "cam" && wallFilter.value === camId) {
    setWallFilter(wallFilterBeforeCam || { type: "all" });
    setWallFilterBeforeCam(null);
  } else {
    setWallFilterBeforeCam(wallFilter);
    setWallFilter({ type: "cam", value: camId });
  }
}

function toggleLocFilter(locName) {
  const { wallFilter } = getState();
  if (wallFilter.type === "loc" && wallFilter.value === locName) {
    setWallFilter({ type: "all" });
  } else {
    setWallFilterBeforeCam(null);
    setWallFilter({ type: "loc", value: locName });
  }
}

function onSidebarThumbClick(cam) {
  if (isWallLike()) toggleCamFilter(cam.id);
}

async function loadViewerThumb(cam, tile, { force = false } = {}) {
  const img = tile.querySelector("img");
  const loading = tile.querySelector(".thumb-loading");

  if (!force) {
    let dataUrl = THUMB_CACHE.get(cam.id);
    if (!dataUrl) {
      try {
        dataUrl = localStorage.getItem(`thumb_${cam.id}`) || null;
        if (dataUrl) THUMB_CACHE.set(cam.id, dataUrl);
      } catch {}
    }
    if (dataUrl) {
      img.src = dataUrl;
      loading.hidden = true;
      return;
    }
  } else {
    THUMB_CACHE.delete(cam.id);
  }

  if (!cam.hls) {
    loading.textContent = "No preview";
    return;
  }

  try {
    const captured = await captureFrame(cam.hls);
    img.src = captured;
    loading.hidden = true;
    THUMB_CACHE.set(cam.id, captured);
    try { localStorage.setItem(`thumb_${cam.id}`, captured); } catch {}
  } catch (e) {
    loading.textContent = "No preview";
    console.warn(`Snapshot failed for ${cam.id}:`, e);
  }
}

function startSidebarThumbRefresh(cams) {
  stopSidebarThumbRefresh();
  sidebarThumbCams = cams.slice();
  refreshSidebarThumbs();
  sidebarThumbTimer = setInterval(refreshSidebarThumbs, SIDEBAR_THUMB_REFRESH_MS);
}

export function stopSidebarThumbRefresh() {
  if (sidebarThumbTimer !== null) {
    clearInterval(sidebarThumbTimer);
    sidebarThumbTimer = null;
  }
  sidebarThumbCams = [];
  for (const k of [...THUMB_CACHE.keys()]) THUMB_CACHE.delete(k);
}

async function refreshSidebarThumbs() {
  for (const cam of sidebarThumbCams) {
    const tile = $(`#viewer-side-scroll .viewer-thumb[data-id="${CSS.escape(cam.id)}"]`);
    if (tile) await loadViewerThumb(cam, tile, { force: true });
  }
}

export function initSidebar() {
  // Re-render the sidebar's active highlight when the wall filter changes,
  // and rebuild it when leaving playback (the live wall may now show cams
  // that were filtered out). The wall filter listener in wall.js handles
  // loadWall(); we only need to keep the active class in sync.
  on("wallFilter", () => updateSidebarActive());
}
