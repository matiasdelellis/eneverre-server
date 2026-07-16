package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"eneverre/internal/camera"
)

const isoMillis = "2006-01-02T15:04:05.000"

func parseISOTime(ts string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC(), nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid iso %q", ts)
}

func (a *App) handlePlaybackList(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	q := r.URL.Query()
	start, end := q.Get("start"), q.Get("end")
	if start == "" || end == "" {
		httpError(w, http.StatusUnprocessableEntity, "start and end are required")
		return
	}
	from, err1 := parseISOTime(start)
	to, err2 := parseISOTime(end)
	if err1 != nil || err2 != nil {
		httpError(w, http.StatusBadRequest, "invalid start/end timestamp")
		return
	}
	segs, err := a.engine.Index().Range(cam.ID, &from, &to)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(segs))
	for _, s := range segs {
		out = append(out, map[string]any{
			"start":    s.Start.UTC().Format(time.RFC3339Nano),
			"duration": s.Duration,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecordingPaths lists the camera ids that have at least one recorded
// segment in the index. Useful for a client that only browses recordings.
// Returns a plain JSON array of strings ([] when empty). Requires the embedded
// engine in recording mode.
func (a *App) handleRecordingPaths(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil || !a.engine.RecordingEnabled() {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	paths, err := a.engine.Index().Paths()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, http.StatusOK, paths)
}

// handlePlaybackTimeline reports the recorded extent of a camera: first start,
// last end, and segment count. Requires the embedded engine; start/end are null
// when there are no recordings.
func (a *App) handlePlaybackTimeline(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	tl, err := a.engine.Index().Timeline(cam.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"count": tl.Count, "start": nil, "end": nil}
	if tl.Count > 0 {
		resp["start"] = tl.Start.UTC().Format(time.RFC3339Nano)
		resp["end"] = tl.End.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePlaybackGaps reports coverage gaps (interruptions longer than 1s
// between consecutive segments) for a camera, optionally bounded by start/end.
// Requires the embedded engine.
func (a *App) handlePlaybackGaps(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	q := r.URL.Query()
	var from, to *time.Time
	if s := q.Get("start"); s != "" {
		t, err := parseISOTime(s)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid start timestamp")
			return
		}
		from = &t
	}
	if s := q.Get("end"); s != "" {
		t, err := parseISOTime(s)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid end timestamp")
			return
		}
		to = &t
	}
	gaps, err := a.engine.Index().Gaps(cam.ID, from, to, time.Second)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, map[string]any{
			"start":    g.Start.UTC().Format(time.RFC3339Nano),
			"end":      g.End.UTC().Format(time.RFC3339Nano),
			"duration": g.Duration,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// playbackGate enforces engine-active (in recording mode) + user auth + the
// per-camera playback capability shared by every recordings endpoint, returning
// the camera or nil (after writing the error). It is the single source of truth
// for that preamble so the individual handlers can't drift apart.
func (a *App) playbackGate(w http.ResponseWriter, r *http.Request) *camera.Camera {
	if a.engine == nil || !a.engine.RecordingEnabled() {
		httpError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if a.requireUser(w, r) == nil {
		return nil
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.Playback {
		httpError(w, http.StatusNotFound, "Not found")
		return nil
	}
	return &cam
}

// delegateHLS rewrites the request to carry path=<camID> and hands it to fn.
// The playlist's init/segment URIs are relative, so they resolve under this
// same /recordings/hls/ prefix and reach handlePlaybackHLSInit/Segment.
func (a *App) delegateHLS(w http.ResponseWriter, r *http.Request, camID string, extra map[string]string, fn http.HandlerFunc) {
	q := r.URL.Query()
	q.Set("path", camID)
	for k, v := range extra {
		if v != "" {
			q.Set(k, v)
		}
	}
	rq := r.Clone(r.Context())
	rq.URL.RawQuery = q.Encode()
	fn(w, rq)
}

// handlePlaybackHLSPlaylist serves an HLS VOD playlist (CMAF fMP4) for a camera
// over [start,end]. Requires the embedded engine. Gaps are collapsed into a
// continuous timeline; EXT-X-PROGRAM-DATE-TIME carries wall-clock for cursor
// mapping.
func (a *App) handlePlaybackHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	q := r.URL.Query()
	a.delegateHLS(w, r, cam.ID, map[string]string{"from": q.Get("start"), "to": q.Get("end")}, a.engine.Playback().HandleHLSPlaylist)
}

// handlePlaybackHLSInit serves the CMAF init segment referenced by the playlist.
func (a *App) handlePlaybackHLSInit(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	a.delegateHLS(w, r, cam.ID, nil, a.engine.Playback().HandleHLSInit)
}

// handlePlaybackHLSSegment serves a CMAF media segment referenced by the playlist.
func (a *App) handlePlaybackHLSSegment(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	a.delegateHLS(w, r, cam.ID, nil, a.engine.Playback().HandleHLSSegment)
}

func (a *App) handlePlaybackGet(w http.ResponseWriter, r *http.Request) {
	cam := a.playbackGate(w, r)
	if cam == nil {
		return
	}
	q := r.URL.Query()
	start, duration := q.Get("start"), q.Get("duration")
	if start == "" || duration == "" {
		httpError(w, http.StatusUnprocessableEntity, "start and duration are required")
		return
	}
	a.playbackGetEngine(w, r, cam, start, duration)
}

// playbackGetEngine serves a clip from the embedded engine's segment index.
// It pre-checks coverage so it can set X-Next-Available on a miss, then
// delegates the actual fMP4 muxing to the playback handler.
func (a *App) playbackGetEngine(w http.ResponseWriter, r *http.Request, cam *camera.Camera, start, duration string) {
	t, err := parseISOTime(start)
	if err != nil {
		httpError(w, http.StatusBadRequest, "Invalid start timestamp: '"+start+"'")
		return
	}
	dur := parseClipSeconds(duration)
	end := t.Add(dur)

	segs, err := a.engine.Index().Range(cam.ID, &t, &end)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(segs) == 0 {
		if next := a.nextAvailableIndex(cam.ID, t); next != "" {
			w.Header().Set("X-Next-Available", next)
		}
		httpError(w, http.StatusNotFound, "no recording segments found")
		return
	}

	// Delegate to the playback handler with the query shape it expects. It writes
	// the fMP4 clip (Content-Type video/mp4) straight to w.
	rq := r.Clone(r.Context())
	vals := url.Values{
		"path":     {cam.ID},
		"start":    {t.UTC().Format(time.RFC3339Nano)},
		"duration": {duration},
		"format":   {"fmp4"},
	}
	// Forward the gap-fill toggle (default on) so clients can opt out.
	if fg := r.URL.Query().Get("fill_gaps"); fg != "" {
		vals.Set("fill_gaps", fg)
	}
	rq.URL.RawQuery = vals.Encode()
	a.engine.Playback().HandleGet(w, rq)
}

// nextAvailableIndex returns the ISO start of the first segment at or after
// start within a 1h window, or "" if none. Used to hint clients where to seek.
func (a *App) nextAvailableIndex(camID string, start time.Time) string {
	end := start.Add(time.Hour)
	segs, err := a.engine.Index().Range(camID, &start, &end)
	if err != nil {
		return ""
	}
	for _, s := range segs {
		if !s.Start.Before(start) { // s.Start >= start
			return s.Start.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

// parseClipSeconds parses the client's duration param (seconds, e.g. "5" or
// "5.0"); anything unparseable falls back to 5s, matching PB_CLIP_SECONDS.
func parseClipSeconds(s string) time.Duration {
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return 5 * time.Second
}
