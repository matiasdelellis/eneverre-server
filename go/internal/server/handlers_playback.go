package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
	if a.cfg.MediaMTX == nil {
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

func (a *App) handlePlaybackGet(w http.ResponseWriter, r *http.Request) {
	if a.cfg.MediaMTX == nil {
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
