package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"eneverre/internal/camera"
)

func (a *App) mediamtxBase() string {
	return "http://localhost:" + a.cfg.MediaMTX.Get("playback_port", "9996")
}

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

// normalizeISO coerces a client timestamp into the canonical millisecond-UTC
// form MediaMTX accepts (e.g. 2025-01-15T10:30:00.000+00:00).
func normalizeISO(ts string) (string, error) {
	t, err := parseISOTime(ts)
	if err != nil {
		return "", err
	}
	return t.Format(isoMillis) + "+00:00", nil
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// mediamtxGetJSON GETs a MediaMTX control path with shared Basic auth and
// returns the raw body and status. A non-nil error means a transport failure.
func (a *App) mediamtxGetJSON(path string, params url.Values) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, a.mediamtxBase()+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.URL.RawQuery = params.Encode()
	creds := a.creds.Current()
	req.SetBasicAuth(creds.Username, creds.Password)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func (a *App) handlePlaybackList(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil && a.cfg.MediaMTX == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil || !cam.Capabilities.Playback {
		httpError(w, http.StatusNotFound, "Not found")
		return
	}
	q := r.URL.Query()
	start, end := q.Get("start"), q.Get("end")
	if start == "" || end == "" {
		httpError(w, http.StatusUnprocessableEntity, "start and end are required")
		return
	}

	// Embedded engine: answer from the in-process segment index.
	if a.engine != nil {
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
		return
	}

	body, status, err := a.mediamtxGetJSON("/list", url.Values{
		"path": {cam.ID}, "start": {start}, "end": {end},
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, "MediaMTX unreachable: "+err.Error())
		return
	}
	if status >= 400 {
		httpError(w, http.StatusBadGateway, fmt.Sprintf("MediaMTX error %d", status))
		return
	}
	var segments []map[string]any
	_ = json.Unmarshal(body, &segments)
	out := make([]map[string]any, 0, len(segments))
	for _, s := range segments {
		out = append(out, map[string]any{"start": s["start"], "duration": s["duration"]})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecordingPaths lists the camera ids that have at least one recorded
// segment in the index (the embedded-engine equivalent of the NVR standalone's
// /api/paths). Useful for a client that only browses recordings. Returns a
// plain JSON array of strings ([] when empty). Embedded-engine only.
func (a *App) handleRecordingPaths(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
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
// last end, and segment count. Embedded-engine only (404 otherwise) — the
// external MediaMTX playback server does not expose this. Mirrors the NVR
// standalone's /api/timeline. start/end are null when there are no recordings.
func (a *App) handlePlaybackTimeline(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil || !cam.Capabilities.Playback {
		httpError(w, http.StatusNotFound, "Not found")
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
// Embedded-engine only. Mirrors the NVR standalone's /api/gaps.
func (a *App) handlePlaybackGaps(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil || !cam.Capabilities.Playback {
		httpError(w, http.StatusNotFound, "Not found")
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

// hlsGate enforces engine-active + user auth + playback capability for the HLS
// VOD endpoints, returning the camera or nil (after writing the error).
func (a *App) hlsGate(w http.ResponseWriter, r *http.Request) *camera.Camera {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if a.requireUser(w, r) == nil {
		return nil
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil || !cam.Capabilities.Playback {
		httpError(w, http.StatusNotFound, "Not found")
		return nil
	}
	return cam
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
// over [start,end]. Embedded-engine only. Gaps are collapsed into a continuous
// timeline; EXT-X-PROGRAM-DATE-TIME carries wall-clock for cursor mapping.
func (a *App) handlePlaybackHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	cam := a.hlsGate(w, r)
	if cam == nil {
		return
	}
	// eneverre uses start/end; the playback handler reads from/to.
	q := r.URL.Query()
	a.delegateHLS(w, r, cam.ID, map[string]string{"from": q.Get("start"), "to": q.Get("end")}, a.engine.Playback().HandleHLSPlaylist)
}

// handlePlaybackHLSInit serves the CMAF init segment referenced by the playlist.
func (a *App) handlePlaybackHLSInit(w http.ResponseWriter, r *http.Request) {
	cam := a.hlsGate(w, r)
	if cam == nil {
		return
	}
	a.delegateHLS(w, r, cam.ID, nil, a.engine.Playback().HandleHLSInit)
}

// handlePlaybackHLSSegment serves a CMAF media segment referenced by the playlist.
func (a *App) handlePlaybackHLSSegment(w http.ResponseWriter, r *http.Request) {
	cam := a.hlsGate(w, r)
	if cam == nil {
		return
	}
	a.delegateHLS(w, r, cam.ID, nil, a.engine.Playback().HandleHLSSegment)
}

func (a *App) handlePlaybackGet(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil && a.cfg.MediaMTX == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil || !cam.Capabilities.Playback {
		httpError(w, http.StatusNotFound, "Not found")
		return
	}
	q := r.URL.Query()
	start, duration := q.Get("start"), q.Get("duration")
	if start == "" || duration == "" {
		httpError(w, http.StatusUnprocessableEntity, "start and duration are required")
		return
	}

	// Embedded engine: mux the clip in-process from the segment index.
	if a.engine != nil {
		a.playbackGetEngine(w, r, cam, start, duration)
		return
	}

	startNorm, err := normalizeISO(start)
	if err != nil {
		httpError(w, http.StatusBadRequest, "Invalid start timestamp: '"+start+"'")
		return
	}

	params := url.Values{
		"path":     {cam.ID},
		"start":    {startNorm},
		"duration": {duration},
		"format":   {"mp4"},
	}

	resp, err := a.openUpstream(params)
	if err != nil {
		httpError(w, http.StatusBadGateway, "MediaMTX unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = "no recording segments found"
		}
		if next := a.nextAvailable(cam.ID, startNorm); next != "" {
			w.Header().Set("X-Next-Available", next)
		}
		httpError(w, http.StatusNotFound, detail)
		return
	}
	if resp.StatusCode >= 400 {
		httpError(w, http.StatusBadGateway, fmt.Sprintf("MediaMTX error %d", resp.StatusCode))
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "video/mp4"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

// playbackGetEngine serves a clip from the embedded engine's segment index.
// It pre-checks coverage so it can set X-Next-Available on a miss (matching the
// MediaMTX path), then delegates the actual fMP4 muxing to the playback handler.
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

// openUpstream opens MediaMTX /get as a stream, following its single redirect
// by hand so Basic auth is re-sent. The caller owns closing the body.
func (a *App) openUpstream(params url.Values) (*http.Response, error) {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{ResponseHeaderTimeout: 10 * time.Second},
	}
	req, err := http.NewRequest(http.MethodGet, a.mediamtxBase()+"/get/", nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = params.Encode()
	creds := a.creds.Current()
	req.SetBasicAuth(creds.Username, creds.Password)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		next, err := req.URL.Parse(loc)
		if err != nil {
			return nil, err
		}
		req2, err := http.NewRequest(http.MethodGet, next.String(), nil)
		if err != nil {
			return nil, err
		}
		req2.SetBasicAuth(creds.Username, creds.Password)
		resp, err = client.Do(req2)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// nextAvailable returns the ISO start of the first segment at or after start,
// within a 1h window, or "" if none. Errors degrade to "".
func (a *App) nextAvailable(camID, start string) string {
	startNorm, err := normalizeISO(start)
	if err != nil {
		return ""
	}
	startDt, err := parseISOTime(startNorm)
	if err != nil {
		return ""
	}
	endDt := startDt.Add(time.Hour)

	body, status, err := a.mediamtxGetJSON("/list", url.Values{
		"path":  {camID},
		"start": {startNorm},
		"end":   {endDt.Format(isoMillis) + "+00:00"},
	})
	if err != nil || status >= 400 {
		return ""
	}
	var segments []map[string]any
	if json.Unmarshal(body, &segments) != nil {
		return ""
	}

	type cand struct {
		t   time.Time
		raw string
	}
	var cands []cand
	for _, s := range segments {
		raw, _ := s["start"].(string)
		if raw == "" {
			continue
		}
		t, err := parseISOTime(raw)
		if err != nil {
			continue
		}
		cands = append(cands, cand{t, raw})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].t.Before(cands[j].t) })

	for _, c := range cands {
		if !c.t.Before(startDt) { // c.t >= startDt
			if c.t.Sub(startDt) < time.Second {
				return c.t.Add(50*time.Millisecond).Format(isoMillis) + "+00:00"
			}
			return c.raw
		}
	}
	return ""
}
