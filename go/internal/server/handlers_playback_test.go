package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/media"
	"eneverre/internal/media/index"
)

// playbackTestApp builds an App wired to a real (temp-dir) media engine in
// recording mode and one camera with the playback capability, so the
// recordings handlers pass playbackGate without any live RTSP source.
func playbackTestApp(t *testing.T) *App {
	t.Helper()
	a := withUsersApp(t)
	insertUser(t, a.db, "alice", "alicepw", "user")
	dir := t.TempDir()
	eng, err := media.New(media.Options{
		RecordEnabled: true,
		IndexPath:     filepath.Join(dir, "index.db"),
		CacheDir:      dir,
		Retain:        7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(eng.Close)
	a.engine = eng
	a.cameras = []camera.Camera{{
		ID:           "cam",
		Name:         "Cam",
		Capabilities: camera.Capabilities{Playback: true},
	}}
	a.privacy = map[string]bool{}
	return a
}

func insertSegment(t *testing.T, a *App, start time.Time, durationSec float64) {
	t.Helper()
	err := a.engine.Index().Insert(index.Segment{
		Fpath:    start.Format("20060102T150405.000000") + ".mp4",
		Path:     "cam",
		Start:    start,
		Duration: durationSec,
	})
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
}

func doPlayback(t *testing.T, a *App, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, http.MethodGet, target, "alice", "alicepw", ""))
	return w
}

func TestHandlePlaybackListBounds(t *testing.T) {
	a := playbackTestApp(t)
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	insertSegment(t, a, base, 60)
	insertSegment(t, a, base.Add(time.Minute), 60)

	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"missing bounds", "/api/camera/cam/recordings/list", http.StatusUnprocessableEntity},
		{"bad timestamp", "/api/camera/cam/recordings/list?start=notatime&end=2026-05-04T11:00:00Z", http.StatusBadRequest},
		{"end before start", "/api/camera/cam/recordings/list?start=2026-05-04T11:00:00Z&end=2026-05-04T10:00:00Z", http.StatusUnprocessableEntity},
		{"span over cap", "/api/camera/cam/recordings/list?start=2026-05-01T00:00:00Z&end=2026-05-10T00:00:00Z", http.StatusUnprocessableEntity},
		{"huge span", "/api/camera/cam/recordings/list?start=2020-01-01T00:00:00Z&end=2026-12-31T00:00:00Z", http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doPlayback(t, a, c.target)
			if w.Code != c.want {
				t.Fatalf("list = %d, want %d (body: %s)", w.Code, c.want, w.Body.String())
			}
		})
	}

	t.Run("valid window returns the segments", func(t *testing.T) {
		w := doPlayback(t, a, "/api/camera/cam/recordings/list?start=2026-05-04T09:59:00Z&end=2026-05-04T10:02:00Z")
		if w.Code != http.StatusOK {
			t.Fatalf("list = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var out []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 2 {
			t.Errorf("segments = %d, want 2", len(out))
		}
	})
}

func TestHandlePlaybackGapsBounds(t *testing.T) {
	a := playbackTestApp(t)
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	insertSegment(t, a, base, 60)
	// A gap: the next segment starts 5s after the previous one ends.
	insertSegment(t, a, base.Add(65*time.Second), 60)

	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"missing bounds", "/api/camera/cam/recordings/gaps", http.StatusUnprocessableEntity},
		{"span over cap", "/api/camera/cam/recordings/gaps?start=2026-05-01T00:00:00Z&end=2026-05-10T00:00:00Z", http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doPlayback(t, a, c.target)
			if w.Code != c.want {
				t.Fatalf("gaps = %d, want %d (body: %s)", w.Code, c.want, w.Body.String())
			}
		})
	}

	t.Run("valid window reports the gap", func(t *testing.T) {
		w := doPlayback(t, a, "/api/camera/cam/recordings/gaps?start=2026-05-04T09:00:00Z&end=2026-05-04T11:00:00Z")
		if w.Code != http.StatusOK {
			t.Fatalf("gaps = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var out []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 1 {
			t.Errorf("gaps = %d, want 1", len(out))
		}
	})
}

func TestHandlePlaybackHLSPlaylistBounds(t *testing.T) {
	a := playbackTestApp(t)
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	insertSegment(t, a, base, 60)

	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"missing bounds", "/api/camera/cam/recordings/hls/playlist.m3u8", http.StatusUnprocessableEntity},
		{"span over cap", "/api/camera/cam/recordings/hls/playlist.m3u8?start=2026-05-01T00:00:00Z&end=2026-05-10T00:00:00Z", http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doPlayback(t, a, c.target)
			if w.Code != c.want {
				t.Fatalf("playlist = %d, want %d (body: %s)", w.Code, c.want, w.Body.String())
			}
		})
	}

	t.Run("valid window serves a playlist", func(t *testing.T) {
		w := doPlayback(t, a, "/api/camera/cam/recordings/hls/playlist.m3u8?start=2026-05-04T09:59:00Z&end=2026-05-04T10:02:00Z")
		if w.Code != http.StatusOK {
			t.Fatalf("playlist = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		if got := w.Body.String(); got == "" {
			t.Error("playlist body is empty")
		}
	})
}
