import { $, $$, makeMsg } from "../util/dom.js";
import { token } from "../api.js";
import { icon } from "../ui/icons.js";
import { Timeline } from "../../timeline.js";
import { t } from "../i18n.js";

export const PB_DEFAULT_INTERVAL = 6 * 60 * 60 * 1000; // timeline window: 6 hours
const PB_START_OFFSET_MS = 5 * 60 * 1000;    // cursor lands 5 min in the past
let pbSpeed = 1;                             // current VOD playback speed
const PB_FRAME_STEP_SEC = 1 / 15;            // ,/. step size (recordings are ~15fps substreams)

let pbTimeline = null;
let pbCams = [];
let preservedPbState = null;

let pbBuildGen = 0;
let pbLoadGen = 0;     // initial-playback render; bumped when leaving playback
let pbSelectGen = 0;   // timeline clicks; bumped when leaving playback or on a newer click
export function getTimeline() { return pbTimeline; }
export function getPbCams() { return pbCams; }
export function getLoadGen() { return pbLoadGen; }
export function setPreservedPbState(s) { preservedPbState = s; }

function findRecordAt(records, msec) {
  for (const r of records) {
    if (r.timestampMsec <= msec && msec < r.timestampMsec + r.durationMsec) return r;
  }
  return null;
}

// Read the timeline colors from the document's CSS custom properties.
// Called both on initial build and on theme change (the canvas paints
// colors directly via the canvas 2D context, so it can't pick up a
// stylesheet-only theme switch). The hardcoded fallbacks (no-data bar,
// time-pill text) are kept verbatim from the original builder.
function readTimelineColors() {
  const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return {
    colorBackground: css("--panel-2") || "#222c38",
    colorDigits: css("--muted") || "#8b98a5",
    colorRectBackground: css("--accent") || "#3b82f6",
    colorRectNoData: "#2c3846",
    colorRectMajor1: css("--danger") || "#ef4444",
    colorRectMajor2: css("--accent") || "#3b82f6",
    colorTimeBackground: css("--accent") || "#3b82f6",
    colorTimeText: "#ffffff",
    colorTimelineSelected: css("--accent") || "#3b82f6",
    colorMajor1Selected: css("--danger") || "#ef4444",
    colorMajor2Selected: css("--accent") || "#3b82f6",
  };
}

// Theme change listener: re-read the CSS custom properties and ask the
// timeline to redraw itself. The canvas is the only non-CSS surface in
// the app that hardcodes theme-dependent colors, so it owns its own
// refresh. Other consumers (CSS-driven controls) don't need this.
document.addEventListener("eneverre:themechange", () => {
  if (!pbTimeline) return;
  Object.assign(pbTimeline.options, readTimelineColors());
  pbTimeline.draw();
});

async function fetchRecordings(camId, rangeMs = 24 * 3600 * 1000) {
  const end = new Date();
  const start = new Date(end.getTime() - rangeMs);
  const params = new URLSearchParams({
    start: start.toISOString(),
    end: end.toISOString(),
  });
  try {
    const r = await fetch(
      `/api/camera/${encodeURIComponent(camId)}/recordings/list?${params}`,
      { headers: { Authorization: `Bearer ${token()}` } },
    );
    if (!r.ok) return [];
    const segs = await r.json();
    return segs.map((s) => ({
      timestampMsec: new Date(s.start).getTime(),
      durationMsec: Math.round(s.duration * 1000),
      object: s,
    }));
  } catch {
    return [];
  }
}

async function fetchEvents(camId, rangeMs = 24 * 3600 * 1000) {
  const end = new Date();
  const start = new Date(end.getTime() - rangeMs);
  const params = new URLSearchParams({
    since: start.toISOString(),
    until: end.toISOString(),
    limit: "1000",
  });
  try {
    const r = await fetch(
      `/api/camera/${encodeURIComponent(camId)}/events?${params}`,
      { headers: { Authorization: `Bearer ${token()}` } },
    );
    if (!r.ok) return [];
    const data = await r.json();
    return (data.events || [])
      .map((ev) => {
        const startMs = new Date(ev.start_ts).getTime();
        const endMs = new Date(ev.end_ts).getTime();
        return {
          timestampMsec: startMs,
          durationMsec: Math.max(1000, endMs - startMs),
          object: ev,
        };
      })
      // Newest first, so getNextRecord/getPrevRecord (which assume a
      // descending list) walk events the same way j/l walk recordings.
      .sort((a, b) => b.timestampMsec - a.timestampMsec);
  } catch {
    return [];
  }
}

export async function loadPlaybackBlob(camId, start, duration, opts = {}) {
  const startDate = start instanceof Date ? start : new Date(start);
  const params = new URLSearchParams({
    start: startDate.toISOString(),
    duration: String(duration),
  });
  // fill_gaps defaults to true on the server; opt out with opts.fillGaps=false
  // to get the legacy gapless (truncate-at-gap) avc1 output.
  if (opts.fillGaps === false) params.set("fill_gaps", "false");
  const r = await fetch(
    `/api/camera/${encodeURIComponent(camId)}/recordings/get?${params}`,
    { headers: { Authorization: `Bearer ${token()}` }, signal: opts.signal },
  );
  if (!r.ok) {
    let detail = `HTTP ${r.status}`;
    try { detail = (await r.json()).detail || detail; } catch {}
    throw new Error(detail);
  }
  return URL.createObjectURL(await r.blob());
}

export function setTilePlaybackLoading(tile) {
  hideTileBuffering(tile);
  let video = tile.querySelector("video");
  if (video) {
    video.removeAttribute("src");
    video.load();
  } else {
    const firstEl = tile.firstElementChild;
    video = document.createElement("video");
    video.autoplay = true; video.playsInline = true; video.muted = true;
    tile.insertBefore(video, firstEl);
    if (firstEl) firstEl.remove();
  }
  tile.dataset.poster = video.poster || "/img/camera-banner.png";
  const placeholder = makeMsg(t("wall.loading_playback"));
  placeholder.classList.add("wall-loading");
  video.replaceWith(placeholder);
  tile._loadingPlaceholder = placeholder;
  tile.dataset.mode = "playback-loading";
}

export function captureTimelineState() {
  if (!pbTimeline) return;
  preservedPbState = {
    selectedMsec: pbTimeline.getCurrent(),
    intervalMsec: pbTimeline.getInterval(),
  };
}

export async function buildPlaybackTimeline(filtered) {
  const canvas = $("#pb-canvas");
  const myGen = ++pbBuildGen;
  pbTimeline = null;
  pbCams = filtered;

  const rowH = 24;
  const desiredH = Math.max(80, filtered.length * rowH + 50);
  canvas.style.height = desiredH + "px";

  const tl = new Timeline({
    timelines: filtered.length,
    timelineNames: filtered.map((c) => c.name || c.id),
    ...readTimelineColors(),
  });
  tl.intervalMsec = PB_DEFAULT_INTERVAL;
  tl.setCanvas(canvas);

  const [results, events] = await Promise.all([
    Promise.all(filtered.map((c) => fetchRecordings(c.id))),
    Promise.all(filtered.map((c) => fetchEvents(c.id))),
  ]);
  if (myGen !== pbBuildGen) return null;

  for (let i = 0; i < filtered.length; i++) {
    tl.setBackgroundRecords(i, results[i]);
    tl.setMajor1Records(i, events[i]);
  }

  tl.setTimeSelectedCallback((_tlIdx, msec, _record) => {
    for (const cam of pbCams) {
      const tile = $(`#wall .wall-tile[data-id="${CSS.escape(cam.id)}"]`);
      if (tile) setTilePlaybackLoading(tile);
    }
    startVodPlayback(pbCams, msec);
  });

  tl.setLiveCallback((_tlIdx, isLive) => {
    if (!isLive) return;
    killVods();
    // Lazy-load the wall module to break the wall <-> playback import
    // cycle. The wall module is always already loaded by the time the
    // live callback fires (this buildPlaybackTimeline() was called from
    // wall.js), so the import resolves synchronously in practice.
    import("./wall.js").then(({ setTileMode }) => {
      for (const cam of pbCams) {
        const tile = $(`#wall .wall-tile[data-id="${CSS.escape(cam.id)}"]`);
        if (tile) setTileMode(tile, cam, "live");
      }
    });
  });

  let initMsec;
  if (preservedPbState) {
    tl.intervalMsec = preservedPbState.intervalMsec;
    initMsec = preservedPbState.selectedMsec;
    preservedPbState = null;
  } else {
    const defaultMsec = Date.now() - PB_START_OFFSET_MS;
    const halfInterval = PB_DEFAULT_INTERVAL / 2;
    let latestMsec = 0;
    for (const evs of events) {
      for (const e of evs) {
        if (e.timestampMsec > latestMsec) latestMsec = e.timestampMsec;
      }
    }
    for (const recs of results) {
      for (const r of recs) {
        const end = r.timestampMsec + r.durationMsec;
        if (end > latestMsec) latestMsec = end;
      }
    }
    if (latestMsec > 0 && Math.abs(latestMsec - defaultMsec) > halfInterval) {
      initMsec = latestMsec;
    } else {
      initMsec = defaultMsec;
    }
  }
  tl.setCurrent(initMsec);

  if (myGen !== pbBuildGen) return null;
  pbTimeline = tl;

  resizePbCanvas();
  tl.draw();

  const hasRecording = (cam) => {
    const idx = filtered.indexOf(cam);
    if (idx < 0) return false;
    return findRecordAt(tl.getBackgroundRecords(idx), initMsec) !== null;
  };

  return { initMsec, hasRecording };
}

// Lazy accessor for the wall module — kept for callers that want a
// stable import surface, but the live callback above uses a direct
// dynamic import.
let _wallModule = null;
export async function ensureWall() {
  if (!_wallModule) {
    _wallModule = await import("./wall.js");
  }
  return _wallModule;
}

// claimPlaybackLoad bumps pbLoadGen and returns the new value. Called by the
// caller of buildPlaybackTimeline as the first step of a new playback load,
// so the bump can be used to detect a superseded in-flight load. teardown
// resets state but does NOT bump pbLoadGen anymore — the bump is the caller's
// signal that it owns the new generation, and self-bumping via teardown
// would always invalidate the caller itself.
export function claimPlaybackLoad() {
  return ++pbLoadGen;
}

export function teardownPlaybackTimeline() {
  if (pbTimeline) {
    preservedPbState = {
      intervalMsec: pbTimeline.getInterval(),
      selectedMsec: pbTimeline.getCurrent(),
    };
  }
  pbBuildGen++;
  pbSelectGen++;
  killVods();
  pbTimeline = null;
  pbCams = [];
  const canvas = $("#pb-canvas");
  if (canvas) canvas.style.height = "";
}

function resizePbCanvas() {
  const canvas = $("#pb-canvas");
  if (!canvas || !pbTimeline) return;
  void canvas.clientHeight;
  const dpr = window.devicePixelRatio || 1;
  canvas.width = canvas.clientWidth * dpr;
  canvas.height = canvas.clientHeight * dpr;
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  pbTimeline.draw();
}

export function setupPlaybackBar() {
  const playBtn = $("#pb-play");
  const liveBtn = $("#pb-live");
  const canvas = $("#pb-canvas");
  const speedBtns = $$("#pb-speed .pb-speed-btn");

  // ----- Timeline click / scroll -----
  canvas.addEventListener("click", (e) => {
    if (!pbTimeline) return;
    pbTimeline.onSingleTapUp({ offsetX: e.offsetX, offsetY: e.offsetY });
  });
  canvas.addEventListener("wheel", (e) => {
    if (!pbTimeline) return;
    e.preventDefault();
    if (e.ctrlKey) {
      if (e.deltaY < 0) pbTimeline.increaseInterval();
      else pbTimeline.decreaseInterval();
    } else {
      pbTimeline.onScroll(e.deltaX || e.deltaY);
    }
  }, { passive: false });
  if (typeof ResizeObserver !== "undefined") {
    new ResizeObserver(() => {
      if (pbTimeline) resizePbCanvas();
    }).observe(canvas);
  }

  // ----- Hover tooltip on canvas (only on recorded segments) -----
  canvas.addEventListener("mousemove", (e) => {
    if (!pbTimeline || !pbCams.length) { pbTimeline?.clearHover(); return; }
    const msec = pbTimeline.msecFromPixel(e.offsetX);
    const OFFSET = 25;
    const numTL = pbTimeline.options.timelines;
    const rowH = (canvas.clientHeight - OFFSET * 2) / numTL;
    const idx = Math.floor((e.offsetY - OFFSET) / rowH);
    if (idx < 0 || idx >= numTL) { pbTimeline.clearHover(); pbTimeline.draw(); return; }
    const records = pbTimeline.getBackgroundRecords(idx);
    if (!records || !findRecordAt(records, msec)) { pbTimeline.clearHover(); pbTimeline.draw(); return; }
    pbTimeline.setHover(e.offsetX, msec);
    pbTimeline.draw();
  });
  canvas.addEventListener("mouseleave", () => { pbTimeline?.clearHover(); pbTimeline?.draw(); });

  // ----- Play / pause -----
  playBtn.addEventListener("click", () => {
    const videos = $$("#wall .wall-tile video");
    if (!videos.length) return;
    const paused = videos[0].paused;
    for (const v of videos) {
      if (paused) v.play().catch(() => {});
      else v.pause();
    }
    playBtn.innerHTML = icon(paused ? "pause" : "play");
    playBtn.setAttribute("aria-label", paused ? t("pb.pause") : t("pb.play"));
    setVodPaused(!paused);
  });

  // ----- Live button -----
  liveBtn.addEventListener("click", () => {
    if (!pbTimeline) return;
    pbTimeline.setCurrent(Date.now());
    pbTimeline.draw();
    playBtn.innerHTML = icon("play");
    playBtn.setAttribute("aria-label", t("pb.play"));
  });

  // ----- Speed control -----
  for (const btn of speedBtns) {
    btn.addEventListener("click", () => {
      pbSpeed = parseFloat(btn.dataset.speed);
      for (const b of speedBtns) {
        const active = b === btn;
        b.classList.toggle("active", active);
        b.setAttribute("aria-pressed", active ? "true" : "false");
      }
      // Apply to all existing VOD videos
      for (const h of vodInstances.values()) {
        const m = h && h.media;
        if (m) m.playbackRate = pbSpeed;
      }
    });
  }

  // ----- Jump to date/time -----
  const gotoBtn = $("#pb-goto");
  const gotoInput = $("#pb-goto-input");
  if (gotoBtn && gotoInput) {
    gotoBtn.addEventListener("click", () => {
      if (!pbTimeline) return;
      gotoInput.value = toLocalDatetimeValue(pbTimeline.getCurrent());
      // showPicker() where supported; otherwise focusing opens the native UI.
      if (typeof gotoInput.showPicker === "function") { try { gotoInput.showPicker(); return; } catch {} }
      gotoInput.focus();
    });
    gotoInput.addEventListener("change", () => {
      if (!pbTimeline || !gotoInput.value) return;
      const msec = new Date(gotoInput.value).getTime();
      if (Number.isNaN(msec)) return;
      const clamped = Math.min(msec, Date.now());
      pbTimeline.setCurrent(clamped);
      pbTimeline.draw();
      startVodPlayback(pbCams, clamped);
    });
  }
}

// Formats an epoch-ms value as the local "YYYY-MM-DDTHH:MM:SS" string a
// datetime-local input expects (its value is always local, no timezone).
function toLocalDatetimeValue(msec) {
  const d = new Date(msec);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// ---------- HLS VOD playback (embedded engine) ----------
//
// Plays the recordings timeline via one hls.js VOD instance per camera
// tile, fed by /api/camera/{id}/recordings/hls/playlist.m3u8. hls.js
// handles seeking and buffering natively (best responsiveness). Coverage
// gaps between segments are signaled with EXT-X-DISCONTINUITY in the
// playlist; the player (hls.js, VLC, ExoPlayer, AVPlayer) resets
// decoder state and seeks to the next keyframe at the gap per the HLS
// spec.
//
// The timeline cursor is driven by wall-clock, NOT by the player's
// currentTime. This matters at gaps: the player seeks over the gap
// (its currentTime jumps), but the cursor keeps advancing at 1x so
// the wall-clock of the playback remains monotonic. The cursor equals
//
//     playbackStartMsec + (Date.now() - playbackStartTime)
//
// where playbackStartMsec is the wall-clock of the scrub position and
// playbackStartTime is when this playback session started.
//
// Per-tile "No recording" overlays are shown whenever the cursor's
// wall-clock falls inside a coverage gap. The tile's video is paused
// (the player has seek-past on the discontinuity, so the video is
// already at the next keyframe) and the overlay hides it; when the
// cursor exits the gap the tile's HLS instance is reinitialized at the
// cursor's current position so playback resumes in sync.
//
// Note: each hls.js runs its own clock, so the videos drift apart
// over time. We don't try to re-sync (any snap implies destroy + reload
// + re-buffer, which is visible as a glitch); the cursor is the source
// of truth and the videos are best-effort aligned. Scrubbing resets
// everything.

const PB_VOD_WINDOW_MS = 60 * 60 * 1000; // forward window loaded per scrub (1h)
let vodInstances = new Map();            // camId -> Hls
let vodCursorTimer = null;
let vodPlaybackStartTime = 0;           // Date.now() at the start of the current scrub
let vodPlaybackStartMsec = 0;           // wall-clock of the scrub position
let vodPaused = false;                  // wall-clock cursor respects user pause
let vodPausedAt = 0;                    // Date.now() when paused (for elapsed adjustment on resume)

function tileOf(camId) {
  return $(`#wall .wall-tile[data-id="${CSS.escape(camId)}"]`);
}

function vodPlaylistUrl(camId, startMsec) {
  const start = new Date(startMsec).toISOString();
  const end = new Date(Math.min(startMsec + PB_VOD_WINDOW_MS, Date.now())).toISOString();
  const p = new URLSearchParams({ start, end });
  return `/api/camera/${encodeURIComponent(camId)}/recordings/hls/playlist.m3u8?${p}`;
}

function showTileBuffering(tile) {
  if (!tile) return;
  let el = tile.querySelector(".wall-buffering");
  if (!el) {
    el = document.createElement("div");
    el.className = "wall-buffering";
    el.setAttribute("role", "status");
    el.setAttribute("aria-live", "polite");
    el.innerHTML = '<span class="wall-buffering-icon" aria-hidden="true">⟳</span><span class="wall-buffering-text">Loading…</span>';
    const overlay = tile.querySelector(".wall-overlay");
    if (overlay) tile.insertBefore(el, overlay);
    else tile.appendChild(el);
  }
  el.classList.remove("wall-connection-lost");
}

function hideTileBuffering(tile) {
  const el = tile && tile.querySelector(".wall-buffering");
  if (el) el.remove();
}

export function killVods() {
  if (vodCursorTimer) { clearInterval(vodCursorTimer); vodCursorTimer = null; }
  for (const h of vodInstances.values()) { try { h.destroy(); } catch {} }
  vodInstances.clear();
  vodPlaybackStartTime = 0;
  vodPlaybackStartMsec = 0;
  vodPaused = false;
  vodPausedAt = 0;
  for (const t of $$("#wall .wall-gap-overlay")) t.remove();
  for (const t of $$("#wall .wall-buffering")) t.remove();
}

// setVodPaused toggles the wall-clock cursor and every vod video
// together. Used by the playback bar's play/pause button so the cursor
// freezes at the current wall-clock and the videos stop, then resume
// together from the same point. The cursor's elapsed time at the moment
// of pause is preserved across the pause by shifting vodPlaybackStartTime
// forward on resume, so a long pause doesn't "skip" the wall-clock
// forward when playback continues.
export function setVodPaused(paused) {
  if (vodPaused === paused) return;
  vodPaused = paused;
  if (paused) {
    vodPausedAt = Date.now();
    for (const h of vodInstances.values()) {
      const m = h && h.media;
      if (m) { try { m.pause(); } catch {} }
    }
  } else {
    const dt = Date.now() - vodPausedAt;
    if (dt > 0) vodPlaybackStartTime += dt;
    for (const h of vodInstances.values()) {
      const m = h && h.media;
      if (m) { try { m.play().catch(() => {}); } catch {} }
    }
  }
}

// frameStep nudges every VOD video by deltaSec while paused and shifts the
// wall-clock cursor mapping by the same amount so the timeline (and the
// resume point) tracks the step. No-op when not paused or no videos exist.
export function frameStep(deltaSec) {
  if (!vodPaused || !vodInstances.size) return;
  for (const h of vodInstances.values()) {
    const m = h && h.media;
    if (m) { try { m.currentTime = Math.max(0, m.currentTime + deltaSec); } catch {} }
  }
  vodPlaybackStartMsec += deltaSec * 1000;
  if (pbTimeline) {
    pbTimeline.setCurrent(pbTimeline.getCurrent() + deltaSec * 1000);
    pbTimeline.draw();
  }
}

// showVodNoRecording replaces the video with the terminal "no
// recording" message (the playlist itself had no segments for the
// requested range, so hls.js never gets a source).
function showVodNoRecording(tile) {
  for (const o of tile.querySelectorAll(".wall-gap-overlay")) o.remove();
  const v = tile.querySelector("video");
  const msg = document.createElement("div");
  msg.className = "wall-no-recording wall-status";
  msg.innerHTML = `<div class='wall-no-recording-icon'>${icon("signal-off")}</div><div>${t("no_recording")}</div>`;
  if (v) v.replaceWith(msg); else tile.appendChild(msg);
  tile.dataset.mode = "playback-no-data";
}

// setTileGapState drives the per-tile gap state machine. Each tile is
// independent so a camera with recording continues playing while a
// camera without recording (cursor in a gap) is paused + overlaid.
//
//   playback → playback-gap  : cursor entered a gap → pause the video
//                               (the player has seeked past the gap and
//                               is showing the next segment; pause +
//                               overlay hides that and signals "no
//                               recording here").
//   playback-gap → playback    : cursor exited a gap → reinit the HLS
//                               instance at the cursor's current wall-
//                               clock so the tile resumes in sync,
//                               hide overlay.
function setTileGapState(tile, cam, inGap, cursorMsec) {
  const mode = tile.dataset.mode;
  if (inGap) {
    if (mode !== "playback-gap" && mode !== "playback-no-data") {
      tile.dataset.mode = "playback-gap";
      const v = tile.querySelector("video");
      if (v) { try { v.pause(); } catch {} }
      showTileGapOverlay(tile);
    }
  } else {
    if (mode === "playback-gap") {
      tile.dataset.mode = "playback";
      hideTileGapOverlay(tile);
      reinitTileVideo(tile, cam, cursorMsec);
    }
  }
}

function showTileGapOverlay(tile) {
  let overlay = tile.querySelector(".wall-gap-overlay");
  if (!overlay) {
    overlay = document.createElement("div");
    overlay.className = "wall-no-recording wall-gap-overlay wall-status";
    overlay.innerHTML = `<div class='wall-no-recording-icon'>${icon("signal-off")}</div><div>${t("no_recording")}</div>`;
    tile.appendChild(overlay);
  }
  overlay.hidden = false;
}

function hideTileGapOverlay(tile) {
  const overlay = tile.querySelector(".wall-gap-overlay");
  if (overlay) overlay.hidden = true;
}

// reinitTileVideo rebuilds the HLS instance for one tile so playback
// resumes at cursorMsec (the cursor's current wall-clock). The existing
// video element is reused; only the HLS controller is torn down and a
// fresh one is attached loading the playlist from cursorMsec.
function reinitTileVideo(tile, cam, cursorMsec) {
  hideTileBuffering(tile);
  const existing = vodInstances.get(cam.id);
  if (existing) { try { existing.destroy(); } catch {} }
  vodInstances.delete(cam.id);

  const video = tile.querySelector("video");
  if (!video) return;

  const hls = new Hls({
    maxBufferLength: 30,
    xhrSetup: (xhr) => { const t = token(); if (t) xhr.setRequestHeader("Authorization", `Bearer ${t}`); },
  });
  hls.on(Hls.Events.MANIFEST_PARSED, () => { video.play().catch(() => {}); });
  hls.on(Hls.Events.ERROR, (_e, data) => {
    if (data && data.fatal) {
      try { hls.destroy(); } catch {}
      vodInstances.delete(cam.id);
      showVodNoRecording(tile);
    }
  });
  hls.loadSource(vodPlaylistUrl(cam.id, cursorMsec));
  hls.attachMedia(video);
  video.playbackRate = pbSpeed;
  vodInstances.set(cam.id, hls);
}

// startVodPlayback (re)starts VOD playback for every camera tile at
// startMsec. Anchor the cursor to wall-clock first so the per-tile gap
// check (which keys off vodPlaybackStartTime) sees the correct value
// before the first HLS instance is created.
export function startVodPlayback(cams, startMsec) {
  killVods();
  if (!window.Hls || !Hls.isSupported()) {
    for (const cam of cams) {
      const tile = tileOf(cam.id);
      const ph = tile && tile._loadingPlaceholder;
      if (ph) { ph.textContent = t("wall.hls_unsupported"); tile._loadingPlaceholder = null; }
    }
    return;
  }

  vodPlaybackStartTime = Date.now();
  vodPlaybackStartMsec = startMsec;

  for (const cam of cams) {
    const tile = tileOf(cam.id);
    if (!tile) continue;

    const video = document.createElement("video");
    video.autoplay = true; video.playsInline = true; video.muted = true;
    video.playbackRate = pbSpeed;
    video.poster = tile.dataset.poster || "/img/camera-banner.png";
    video.addEventListener("waiting", () => showTileBuffering(tile));
    video.addEventListener("playing", () => hideTileBuffering(tile));
    const ph = tile._loadingPlaceholder;
    if (ph) { ph.replaceWith(video); tile._loadingPlaceholder = null; }
    else {
      const existing = tile.querySelector("video");
      if (existing) existing.replaceWith(video); else tile.appendChild(video);
    }
    tile.dataset.mode = "playback";

    const hls = new Hls({
      maxBufferLength: 30,
      xhrSetup: (xhr) => { const t = token(); if (t) xhr.setRequestHeader("Authorization", `Bearer ${t}`); },
    });
    hls.on(Hls.Events.MANIFEST_PARSED, () => { video.play().catch(() => {}); });
    hls.on(Hls.Events.ERROR, (_e, data) => {
      hideTileBuffering(tile);
      if (data && data.fatal) {
        try { hls.destroy(); } catch {}
        vodInstances.delete(cam.id);
        showVodNoRecording(tile);
      }
    });
    hls.loadSource(vodPlaylistUrl(cam.id, startMsec));
    hls.attachMedia(video);
    vodInstances.set(cam.id, hls);
  }
  vodPaused = false;
  const pbPlay = $("#pb-play");
  if (pbPlay) { pbPlay.innerHTML = icon("pause"); pbPlay.setAttribute("aria-label", t("pb.pause")); }
  startVodCursor();
}

// startVodCursor drives the timeline cursor from wall-clock (1x, monotonic)
// and drives the per-tile gap state machine: each tile whose wall-clock
// position falls inside a coverage gap is paused + overlaid, and is
// reinitialized at the cursor's current position when the cursor exits
// the gap. No re-sync: the videos drift slowly because each hls.js runs
// its own clock, and any snap would imply destroy+reload+re-buffer
// (visible glitch). The cursor is the source of truth; the videos
// are best-effort. Scrubbing resets everything.
function startVodCursor() {
  if (vodCursorTimer) clearInterval(vodCursorTimer);
  vodCursorTimer = setInterval(() => {
    if (!pbTimeline || pbTimeline.timerSelectedId) return;
    if (!vodPlaybackStartTime || vodPaused) return;

    const cursorMsec = vodPlaybackStartMsec + (Date.now() - vodPlaybackStartTime);
    pbTimeline.setCurrent(cursorMsec);
    pbTimeline.draw();

    for (let i = 0; i < pbCams.length; i++) {
      const cam = pbCams[i];
      const tile = tileOf(cam.id);
      if (!tile || tile.dataset.mode === "playback-no-data") continue;
      const records = pbTimeline.getBackgroundRecords(i);
      if (!records) continue;
      const inGap = findRecordAt(records, cursorMsec) === null;
      setTileGapState(tile, cam, inGap, cursorMsec);
    }
  }, 250);
}

// ---------- Keyboard shortcuts (j / k / l / Space) for recordings ----------

export function initPlaybackKeys() {
  document.addEventListener("keydown", (e) => {
    import("../state.js").then(({ getState }) => {
      if (getState().viewMode !== "playback") return;
      const t = e.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
      if (!pbTimeline) return;
      if (e.key === "j") {
        e.preventDefault();
        const records = pbTimeline.getBackgroundRecords(pbTimeline.timelineSelected);
        const prev = pbTimeline.getPrevRecord(pbTimeline.getCurrent(), records || []);
        if (prev) {
          pbTimeline.setCurrent(prev.timestampMsec);
          pbTimeline.draw();
          startVodPlayback(pbCams, prev.timestampMsec);
        }
      } else if (e.key === "k" || e.key === " ") {
        e.preventDefault();
        const btn = $("#pb-play");
        if (btn) btn.click();
      } else if (e.key === "l") {
        e.preventDefault();
        const records = pbTimeline.getBackgroundRecords(pbTimeline.timelineSelected);
        const next = pbTimeline.getNextRecord(pbTimeline.getCurrent(), records || []);
        if (next) {
          pbTimeline.setCurrent(next.timestampMsec);
          pbTimeline.draw();
          startVodPlayback(pbCams, next.timestampMsec);
        }
      } else if (e.key === "p" || e.key === "n") {
        // Jump between events (drawn as major markers on the timeline).
        e.preventDefault();
        const events = pbTimeline.getMajor1Records(pbTimeline.timelineSelected);
        const rec = e.key === "p"
          ? pbTimeline.getPrevRecord(pbTimeline.getCurrent(), events || [])
          : pbTimeline.getNextRecord(pbTimeline.getCurrent(), events || []);
        if (rec) {
          pbTimeline.setCurrent(rec.timestampMsec);
          pbTimeline.draw();
          startVodPlayback(pbCams, rec.timestampMsec);
        }
      } else if (e.key === "," || e.key === ".") {
        // Frame-step backward / forward. Only meaningful while paused.
        e.preventDefault();
        frameStep(e.key === "," ? -PB_FRAME_STEP_SEC : PB_FRAME_STEP_SEC);
      }
    });
  });
}

// ---------- Download clip ----------
//
// Downloads a clip from the camera's recordings using the /recordings/get
// endpoint. Triggers a browser download with a descriptive filename.
// `startMsec` is epoch-milliseconds; `durationSec` defaults to 10.

export async function downloadClip(camId, startMsec, durationSec = 10) {
  const blobUrl = await loadPlaybackBlob(camId, startMsec, durationSec);
  const a = document.createElement("a");
  const d = new Date(startMsec);
  const pad = (n) => String(n).padStart(2, "0");
  const stamp = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}_${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
  a.href = blobUrl;
  a.download = `eneverre_${camId}_${stamp}.mp4`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(blobUrl), 60000);
}
