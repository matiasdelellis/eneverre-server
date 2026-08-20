package playback

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eneverre/internal/media/index"
)

func hlsTestHandler(t *testing.T) *Handler {
	t.Helper()
	idx, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("index.Open: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return &Handler{Index: idx}
}

func hlsInsert(t *testing.T, h *Handler, start time.Time) {
	t.Helper()
	err := h.Index.Insert(index.Segment{
		Fpath:    start.Format("20060102T150405.000000") + ".mp4",
		Path:     "cam",
		Start:    start,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func hlsPlaylistReq(from, to string) *http.Request {
	target := "/hls/playlist.m3u8?path=cam"
	if from != "" {
		target += "&from=" + url.QueryEscape(from)
	}
	if to != "" {
		target += "&to=" + url.QueryEscape(to)
	}
	return httptest.NewRequest(http.MethodGet, target, nil)
}

// TestHandleHLSPlaylistBounds pins the direct handler's defense in depth: even
// when called without the server's boundRange gate, an unbounded playlist is
// refused instead of building one line per segment ever recorded.
func TestHandleHLSPlaylistBounds(t *testing.T) {
	h := hlsTestHandler(t)
	base := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	hlsInsert(t, h, base)

	cases := []struct {
		name string
		from string
		to   string
		want int
	}{
		{"missing from/to", "", "", http.StatusBadRequest},
		{"missing to", "2026-05-04T09:00:00Z", "", http.StatusBadRequest},
		{"malformed from", "notatime", "2026-05-04T10:00:00Z", http.StatusBadRequest},
		{"to before from", "2026-05-04T11:00:00Z", "2026-05-04T10:00:00Z", http.StatusBadRequest},
		{"span over cap", "2026-05-01T00:00:00Z", "2026-05-10T00:00:00Z", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.HandleHLSPlaylist(w, hlsPlaylistReq(c.from, c.to))
			if w.Code != c.want {
				t.Fatalf("playlist = %d, want %d (body: %s)", w.Code, c.want, w.Body.String())
			}
		})
	}

	t.Run("valid window builds the playlist", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.HandleHLSPlaylist(w, hlsPlaylistReq("2026-05-04T09:59:00Z", "2026-05-04T10:02:00Z"))
		if w.Code != http.StatusOK {
			t.Fatalf("playlist = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "#EXTINF") || !strings.Contains(body, "segment.m4s?path=cam") {
			t.Errorf("playlist missing segment lines:\n%s", body)
		}
	})
}
