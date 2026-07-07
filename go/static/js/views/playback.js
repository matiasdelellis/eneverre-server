import { $, $$, makeMsg } from "../util/dom.js";
import { token } from "../api.js";

export const PB_CLIP_SECONDS = 5;            // playback window loaded per scrub
const PB_PRELOAD_MARGIN_SECONDS = 2.0;       // start fetching the next segment this many seconds before the current one ends
export const PB_DEFAULT_INTERVAL = 6 * 60 * 60 * 1000; // timeline window: 6 hours
const PB_START_OFFSET_MS = 5 * 60 * 1000;    // cursor lands 5 min in the past

let pbTimeline = null;
let pbCams = [];
let preservedPbState = null;

let pbBuildGen = 0;
let pbLoadGen = 0;     // initial-playback render; bumped when leaving playback
let pbSelectGen = 0;   // timeline clicks; bumped when leaving playback or on a newer click
let pbSessions = [];
let pbClock = null;    // master clock { startMsec, segmentIndex, segmentStartedAt, intervalMs, preloadTriggered, running, rafId }

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
    return (data.events || []).map((ev) => {
      const startMs = new Date(ev.start_ts).getTime();
      const endMs = new Date(ev.end_ts).getTime();
      return {
        timestampMsec: startMs,
        durationMsec: Math.max(1000, endMs - startMs),
        object: ev,
      };
    });
  } catch {
    return [];
  }
}

export async function loadPlaybackBlob(camId, start, duration, opts = {}) {
  // Accept either a Date or an epoch-ms number; the caller (startVodPlayback
  // → preloadPlaybackClips) passes the scrub position as a number, while
  // the in-loop swappers pass a Date. Normalize here so neither caller
  // crashes with "start.toISOString is not a function".
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
  revokeTileBlob(tile);
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
  const placeholder = makeMsg("Loading playback…");
  placeholder.classList.add("wall-loading");
  video.replaceWith(placeholder);
  tile._loadingPlaceholder = placeholder;
  tile.dataset.mode = "playback-loading";
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

export async function preloadPlaybackClips(cams, start, duration) {
  const results = await Promise.allSettled(
    cams.map((cam) => loadPlaybackBlob(cam.id, start, duration)),
  );
  return cams.map((cam, i) => ({
    cam,
    blobUrl: results[i].status === "fulfilled" ? results[i].value : null,
    error: results[i].status === "rejected" ? (results[i].reason && results[i].reason.message) || "fetch failed" : null,
  }));
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

  const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  const tl = new Timeline({
    timelines: filtered.length,
    timelineNames: filtered.map((c) => c.name || c.id),
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
    killAllSessions();
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
  pbBuildGen++;
  pbSelectGen++;
  killAllSessions();
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

  playBtn.addEventListener("click", () => {
    const videos = $$("#wall .wall-tile video");
    if (!videos.length) return;
    const paused = videos[0].paused;
    for (const v of videos) {
      if (paused) v.play().catch(() => {});
      else v.pause();
    }
    playBtn.textContent = paused ? "⏸" : "▶";
    playBtn.setAttribute("aria-label", paused ? "Pause" : "Play");
  });

  liveBtn.addEventListener("click", () => {
    if (!pbTimeline) return;
    pbTimeline.setCurrent(Date.now());
    pbTimeline.draw();
    playBtn.textContent = "▶";
    playBtn.setAttribute("aria-label", "Play");
  });
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

export function killVods() {
  if (vodCursorTimer) { clearInterval(vodCursorTimer); vodCursorTimer = null; }
  for (const h of vodInstances.values()) { try { h.destroy(); } catch {} }
  vodInstances.clear();
  vodPlaybackStartTime = 0;
  vodPlaybackStartMsec = 0;
  vodPaused = false;
  vodPausedAt = 0;
  for (const t of $$("#wall .wall-gap-overlay")) t.remove();
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

// showVodNoRecording replaces the video with the terminal "no
// recording" message (the playlist itself had no segments for the
// requested range, so hls.js never gets a source).
function showVodNoRecording(tile) {
  for (const o of tile.querySelectorAll(".wall-gap-overlay")) o.remove();
  const v = tile.querySelector("video");
  const msg = document.createElement("div");
  msg.className = "wall-no-recording wall-status";
  msg.innerHTML = "<div class='wall-no-recording-icon'>📡</div><div>No recording</div>";
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
    overlay.innerHTML = "<div class='wall-no-recording-icon'>📡</div><div>No recording</div>";
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
  vodInstances.set(cam.id, hls);
}

// startVodPlayback (re)starts VOD playback for every camera tile at
// startMsec. Anchor the cursor to wall-clock first so the per-tile gap
// check (which keys off vodPlaybackStartTime) sees the correct value
// before the first HLS instance is created.
export function startVodPlayback(cams, startMsec) {
  killAllSessions(); // stop any legacy blob player still running
  killVods();
  if (!window.Hls || !Hls.isSupported()) {
    for (const cam of cams) {
      const tile = tileOf(cam.id);
      const ph = tile && tile._loadingPlaceholder;
      if (ph) { ph.textContent = "HLS not supported in this browser"; tile._loadingPlaceholder = null; }
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

// ---------- Gapless playback sessions (legacy blob player) ----------

function startMasterClock(startMsec) {
  if (pbClock) stopMasterClock();
  pbClock = {
    startMsec,
    segmentIndex: 0,
    segmentStartedAt: Date.now(),
    intervalMs: PB_CLIP_SECONDS * 1000,
    preloadTriggered: false,
    running: true,
    rafId: 0,
  };
  pbClock.rafId = requestAnimationFrame(tickMasterClock);
}

function stopMasterClock() {
  if (!pbClock) return;
  pbClock.running = false;
  if (pbClock.rafId) cancelAnimationFrame(pbClock.rafId);
  pbClock = null;
}

function tickMasterClock() {
  if (!pbClock || !pbClock.running) return;
  let elapsed = Date.now() - pbClock.segmentStartedAt;
  const preloadAt = pbClock.intervalMs - PB_PRELOAD_MARGIN_SECONDS * 1000;
  if (elapsed >= preloadAt && elapsed < pbClock.intervalMs && !pbClock.preloadTriggered) {
    pbClock.preloadTriggered = true;
    triggerPreloadForAllCams();
  }
  while (elapsed >= pbClock.intervalMs) {
    advanceAllCams();
    elapsed = Date.now() - pbClock.segmentStartedAt;
  }

  // Per-tile gap state. The cursor's current wall-clock is the source of
  // truth; a tile whose cursor position is in a coverage gap pauses its
  // video and shows the overlay while the other tiles keep playing.
  // Without this check the master clock would happily swap clips and
  // play through the gap (the clip is filled with black "NO RECORDING"
  // frames) — we want the per-tile pause instead so each camera that
  // has recording in the window keeps advancing at 1x.
  const currentStartMsec = pbClock.startMsec + pbClock.segmentIndex * pbClock.intervalMs;
  for (const s of pbSessions) {
    if (s.killed) continue;
    const camIndex = pbCams.findIndex((c) => c.id === s.cam.id);
    if (camIndex < 0) continue;
    const records = pbTimeline && pbTimeline.getBackgroundRecords(camIndex);
    if (!records) continue;
    const inGap = findRecordAt(records, currentStartMsec) === null;
    setTileGapState(s.tile, inGap);
  }

  // Drive the timeline cursor from the master clock: the cursor's
  // wall-clock is startMsec + segmentIndex*clipDuration + elapsed,
  // so it advances at 1x monotonic regardless of the videos.
  if (pbTimeline) {
    const playhead = currentStartMsec + elapsed;
    if (!pbTimeline.timerSelectedId) {
      pbTimeline.setCurrent(playhead);
      pbTimeline.draw();
    }
  }

  if (pbClock.running) {
    pbClock.rafId = requestAnimationFrame(tickMasterClock);
  }
}

function triggerPreloadForAllCams() {
  if (!pbClock) return;
  const nextStart = pbClock.startMsec + (pbClock.segmentIndex + 1) * pbClock.intervalMs;
  const expectedSegmentIndex = pbClock.segmentIndex;
  for (const s of pbSessions) {
    if (s.killed || s.nextBlobUrl || s.inflightNext) continue;
    if (!pbTimeline) continue;
    const camIndex = pbCams.findIndex((c) => c.id === s.cam.id);
    if (camIndex < 0) continue;
    const records = pbTimeline.getBackgroundRecords(camIndex);
    if (!findRecordAt(records, nextStart)) continue;
    s.inflightNext = true;
    s.preloadController = new AbortController();
    const controller = s.preloadController;
    loadPlaybackBlob(s.cam.id, new Date(nextStart), PB_CLIP_SECONDS, { signal: controller.signal })
      .then((blobUrl) => {
        s.inflightNext = false;
        s.preloadController = null;
        if (s.killed) { URL.revokeObjectURL(blobUrl); return; }
        if (!pbClock || pbClock.segmentIndex !== expectedSegmentIndex) {
          URL.revokeObjectURL(blobUrl);
          return;
        }
        s.nextBlobUrl = blobUrl;
      })
      .catch((err) => {
        s.inflightNext = false;
        s.preloadController = null;
        if (err && err.name === "AbortError") return;
      });
  }
}

function advanceAllCams() {
  if (!pbClock) return;
  pbClock.segmentIndex++;
  pbClock.segmentStartedAt = Date.now();
  pbClock.preloadTriggered = false;
  for (const s of pbSessions) {
    if (!s.killed) swapCamToNext(s);
  }
}

function swapCamToNext(s) {
  if (s.killed || !pbClock) return;
  const nextStart = pbClock.startMsec + pbClock.segmentIndex * pbClock.intervalMs;
  const expectedSegmentIndex = pbClock.segmentIndex;
  if (s.preloadController) {
    s.preloadController.abort();
    s.preloadController = null;
  }
  if (s.nextBlobUrl) {
    const blob = s.nextBlobUrl;
    s.nextBlobUrl = null;
    swapPbVideo(s, blob);
    return;
  }
  if (pbTimeline) {
    const camIndex = pbCams.findIndex((c) => c.id === s.cam.id);
    if (camIndex >= 0) {
      const records = pbTimeline.getBackgroundRecords(camIndex);
      if (!findRecordAt(records, nextStart)) {
        // Next segment is in a gap: don't fetch a clip, don't replace
        // the video. The continuous gap check in tickMasterClock pauses
        // the current video and shows the overlay. The next clip will
        // be fetched when the cursor exits the gap (next advanceAllCams
        // sees the next start as having a recording).
        return;
      }
    }
  }
  loadPlaybackBlob(s.cam.id, new Date(nextStart), PB_CLIP_SECONDS)
    .then((blobUrl) => {
      if (s.killed) { URL.revokeObjectURL(blobUrl); return; }
      if (!pbClock || pbClock.segmentIndex !== expectedSegmentIndex) {
        URL.revokeObjectURL(blobUrl);
        return;
      }
      const msg = s.tile.querySelector(".wall-no-recording");
      if (msg) msg.remove();
      swapPbVideo(s, blobUrl);
    })
    .catch(() => {
      if (s.killed) return;
      if (!pbClock || pbClock.segmentIndex !== expectedSegmentIndex) return;
      // On fetch error (network/4xx/5xx) just leave the tile paused
      // with the overlay; the next advanceAllCams will retry.
    });
}

function killAllSessions() {
  stopMasterClock();
  for (const s of pbSessions) killSession(s);
  pbSessions = [];
}

function killSession(s) {
  s.killed = true;
  if (s.preloadController) {
    s.preloadController.abort();
    s.preloadController = null;
  }
  if (s.video && s.timeUpdateHandler) {
    s.video.removeEventListener("timeupdate", s.timeUpdateHandler);
  }
}

function startPbSession(tile, cam) {
  const video = tile.querySelector("video");
  if (!video) return;
  const s = {
    tile,
    cam,
    video,
    nextBlobUrl: null,
    inflightNext: false,
    preloadController: null,
    killed: false,
    lastDrawnPlayhead: 0,
  };
  s.timeUpdateHandler = () => onPbTimeUpdate(s);
  video.addEventListener("timeupdate", s.timeUpdateHandler);
  pbSessions.push(s);
}

function onPbTimeUpdate(s) {
  if (s.killed || !s.video) return;
  if (!pbTimeline) return;
  if (pbTimeline.timerSelectedId) return;
  const currentStartMsec = pbClock
    ? pbClock.startMsec + pbClock.segmentIndex * pbClock.intervalMs
    : 0;
  const playhead = currentStartMsec + s.video.currentTime * 1000;
  if (Math.abs(playhead - s.lastDrawnPlayhead) > 100) {
    pbTimeline.setCurrent(playhead);
    pbTimeline.draw();
    s.lastDrawnPlayhead = playhead;
  }
}

function swapPbVideo(s, blobUrl) {
  const oldVideo = s.video;
  if (oldVideo) {
    oldVideo.removeEventListener("timeupdate", s.timeUpdateHandler);
    oldVideo.pause();
  }
  if (s.tile._blobUrl && s.tile._blobUrl !== blobUrl) {
    URL.revokeObjectURL(s.tile._blobUrl);
  }
  s.tile._blobUrl = blobUrl;
  const fresh = document.createElement("video");
  fresh.autoplay = true; fresh.playsInline = true; fresh.muted = true;
  fresh.src = blobUrl;
  if (oldVideo) oldVideo.replaceWith(fresh);
  else s.tile.appendChild(fresh);
  s.video = fresh;
  fresh.addEventListener("timeupdate", s.timeUpdateHandler);
  fresh.play().catch((e) => {
    console.warn("swapPbVideo play() rejected:", e && e.message);
    const tile = s.tile;
    if (tile && !tile.querySelector(".wall-status")) {
      const msg = document.createElement("div");
      msg.className = "wall-status wall-no-recording";
      msg.textContent = `Autoplay blocked: ${e && e.message ? e.message : "play() rejected"}`;
      fresh.replaceWith(msg);
    }
  });
  s.lastDrawnPlayhead = 0;
}

function showNoRecording(s) {
  if (s.killed) return;
  if (s.video) {
    s.video.removeEventListener("timeupdate", s.timeUpdateHandler);
    s.video.pause();
  }
  if (s.tile._blobUrl) {
    URL.revokeObjectURL(s.tile._blobUrl);
    s.tile._blobUrl = null;
  }
  if (s.tile.querySelector(".wall-no-recording")) {
    s.video = null;
    s.tile.dataset.mode = "playback-no-data";
    return;
  }
  const msg = document.createElement("div");
  msg.className = "wall-no-recording wall-status";
  msg.innerHTML = "<div class='wall-no-recording-icon'>📡</div><div>No recording</div>";
  if (s.video) s.video.replaceWith(msg);
  else s.tile.appendChild(msg);
  s.video = null;
  s.tile.dataset.mode = "playback-no-data";
}

export function bindClipsAndStart(filtered, clips, startMsec) {
  for (const { cam, blobUrl, error } of clips) {
    const tile = $(`#wall .wall-tile[data-id="${CSS.escape(cam.id)}"]`);
    if (!tile) {
      if (blobUrl) URL.revokeObjectURL(blobUrl);
      continue;
    }
    const placeholder = tile._loadingPlaceholder;
    if (blobUrl) {
      const fresh = document.createElement("video");
      fresh.autoplay = true; fresh.playsInline = true; fresh.muted = true;
      fresh.src = blobUrl;
      tile._blobUrl = blobUrl;
      tile.dataset.mode = "playback";
      if (placeholder) {
        placeholder.replaceWith(fresh);
        tile._loadingPlaceholder = null;
      } else {
        tile.appendChild(fresh);
      }
      startPbSession(tile, cam);
    } else if (placeholder) {
      placeholder.textContent = `Playback failed: ${error}`;
      tile.dataset.mode = "playback-error";
      tile._loadingPlaceholder = null;
    }
  }
  for (const v of $$("#wall .wall-tile video")) {
    v.play().catch((e) => {
      console.warn("play() rejected for", v.src, e && e.message);
      const tile = v.closest(".wall-tile");
      if (tile && !tile.querySelector(".wall-status")) {
        const msg = document.createElement("div");
        msg.className = "wall-status wall-no-recording";
        msg.textContent = `Autoplay blocked: ${e && e.message ? e.message : "play() rejected"}`;
        v.replaceWith(msg);
      }
    });
  }
  startMasterClock(startMsec);
}
