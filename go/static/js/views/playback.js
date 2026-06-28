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
      `/api/camera/${encodeURIComponent(camId)}/playback/list?${params}`,
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
      `/api/cameras/${encodeURIComponent(camId)}/events?${params}`,
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
  const params = new URLSearchParams({
    start: start.toISOString(),
    duration: String(duration),
  });
  const r = await fetch(
    `/api/camera/${encodeURIComponent(camId)}/playback/get?${params}`,
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

  tl.setTimeSelectedCallback(async (_tlIdx, msec, _record) => {
    const mySelGen = ++pbSelectGen;
    const startTime = new Date(msec);
    killAllSessions();
    for (const cam of pbCams) {
      const tile = $(`#wall .wall-tile[data-id="${CSS.escape(cam.id)}"]`);
      if (tile) setTilePlaybackLoading(tile);
    }
    const clips = await preloadPlaybackClips(pbCams, startTime, PB_CLIP_SECONDS);
    if (mySelGen !== pbSelectGen) {
      for (const { blobUrl } of clips) if (blobUrl) URL.revokeObjectURL(blobUrl);
      return;
    }
    bindClipsAndStart(pbCams, clips, msec);
  });

  tl.setLiveCallback((_tlIdx, isLive) => {
    if (!isLive) return;
    killAllSessions();
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

export function teardownPlaybackTimeline() {
  pbBuildGen++;
  pbLoadGen++;
  pbSelectGen++;
  killAllSessions();
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

// ---------- Gapless playback sessions ----------

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
        showNoRecording(s);
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
      showNoRecording(s);
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
