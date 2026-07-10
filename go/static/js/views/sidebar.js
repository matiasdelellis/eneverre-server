import { $, $$, escapeHtml } from "../util/dom.js";
import { setWallFilter, setWallFilterBeforeCam, getState, on } from "../state.js";
import { fetchCameras, token } from "../api.js";
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
const THUMB_PERSIST_MS = 2 * 60 * 1000; // throttle localStorage writes for live frames
let sidebarThumbTimer = null;
let sidebarThumbCams = [];
const lastPersist = new Map(); // camId -> last localStorage write (live thumbs)

// publishLiveThumb is called by the wall while a camera plays live: it stores
// the freshly grabbed frame as that camera's thumbnail (in-memory always,
// localStorage throttled) and updates the visible sidebar tile. This is how
// non-Thingino cameras get a current thumbnail — from the video the wall is
// already decoding, with no extra stream or server round-trip.
export function publishLiveThumb(camId, dataUrl) {
  if (!dataUrl) return;
  THUMB_CACHE.set(camId, dataUrl);
  const now = Date.now();
  if (now - (lastPersist.get(camId) || 0) > THUMB_PERSIST_MS) {
    lastPersist.set(camId, now);
    try { localStorage.setItem(`thumb_${camId}`, dataUrl); } catch {}
  }
  const tile = $(`#viewer-side-scroll .viewer-thumb[data-id="${CSS.escape(camId)}"]`);
  if (tile) {
    const img = tile.querySelector("img");
    const loading = tile.querySelector(".thumb-loading");
    if (img) img.src = dataUrl;
    if (loading) loading.hidden = true;
  }
}

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
  // The banner is the static poster shown while the real thumbnail is
  // loading (or fails to load). loadViewerThumb() replaces it with the
  // camera's actual frame when available.
  tile.innerHTML = `
    <div class="thumb-preview">
      <img alt="" src="/img/camera-banner.png" />
      <span class="thumb-loading">Loading…</span>
      <span class="cam-status-dot connecting" data-cam="${escapeHtml(cam.id)}" title="Connecting…" aria-label="Connecting…"></span>
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

  // Thumbnail source, in priority order:
  //  1. Thingino endpoint (`/api/camera/{id}/thumbnail`) when the camera has
  //     a Thingino API key — fast, returns a server-rendered JPEG.
  //  2. Live frame pushed by the wall via publishLiveThumb while the camera
  //     plays (already handled through THUMB_CACHE above — nothing to pull
  //     here). Cameras that aren't currently playing simply keep the poster
  //     until they start.
  //  3. Keep the static `camera-banner.png` poster that renderViewerThumb
  //     set in the <img> — we just drop the "Loading…" overlay so it
  //     shows the banner alone, recognisable instead of empty.
  let dataUrl;
  if (cam.capabilities && cam.capabilities.thumbnail) {
    try { dataUrl = await fetchThumbnailDataUrl(cam.id); }
    catch (e) { console.warn(`Thumbnail endpoint failed for ${cam.id}:`, e); }
  }
  if (!dataUrl) {
    // No server-side thumbnail available — leave the camera-banner poster in
    // place and drop the "Loading…" overlay. If the camera is on the wall the
    // live frame will replace it within a few seconds via publishLiveThumb.
    loading.hidden = true;
    return;
  }
  img.src = dataUrl;
  loading.hidden = true;
  THUMB_CACHE.set(cam.id, dataUrl);
  try { localStorage.setItem(`thumb_${cam.id}`, dataUrl); } catch {}
}

// fetchThumbnailDataUrl pulls a fresh JPEG from the Thingino-backed thumbnail
// endpoint and returns it as a data URL (so the result is storable in
// localStorage the same way HLS captures are). Cache-busts the request and
// disables HTTP cache so refreshes always get a current frame.
async function fetchThumbnailDataUrl(camId) {
  const t = token();
  const headers = t ? { Authorization: `Bearer ${t}` } : {};
  const r = await fetch(`/api/camera/${encodeURIComponent(camId)}/thumbnail?_=${Date.now()}`, {
    headers,
    cache: "no-store",
  });
  if (!r.ok) return null;
  const blob = await r.blob();
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => resolve(fr.result);
    fr.onerror = () => reject(fr.error);
    fr.readAsDataURL(blob);
  });
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
  lastPersist.clear();
}

async function refreshSidebarThumbs() {
  for (const cam of sidebarThumbCams) {
    // Live cameras refresh themselves via publishLiveThumb from the wall while
    // playing; force-reloading them here would wipe the live frame back to the
    // poster. Only re-pull server-side (Thingino) thumbnails on the timer.
    if (!(cam.capabilities && cam.capabilities.thumbnail)) continue;
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
