package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/media"
)

func insertEvent(t *testing.T, a *App, camID string, endTS int64) {
	t.Helper()
	if _, err := a.db.Exec(
		"INSERT INTO events (camera_id, start_ts, end_ts, type, source, created_at) VALUES (?, ?, ?, 'motion', NULL, ?)",
		camID, endTS-1, endTS, endTS,
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func countEvents(t *testing.T, a *App) int {
	t.Helper()
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func TestPruneOldEvents(t *testing.T) {
	now := time.Now()

	t.Run("prunes on the recording-retention window", func(t *testing.T) {
		a := withUsersApp(t)
		// Event retention follows [media] retain, carried on the engine.
		eng, err := media.New(media.Options{Retain: 7 * 24 * time.Hour})
		if err != nil {
			t.Fatalf("media.New: %v", err)
		}
		t.Cleanup(eng.Close)
		a.engine = eng
		insertEvent(t, a, "cam", now.Add(-10*24*time.Hour).Unix()) // older than 7d -> pruned
		insertEvent(t, a, "cam", now.Add(-1*24*time.Hour).Unix())  // within 7d -> kept
		insertEvent(t, a, "cam", now.Unix())                       // now -> kept

		a.pruneOldEvents()

		if got := countEvents(t, a); got != 2 {
			t.Errorf("events after prune = %d, want 2", got)
		}
	})

	t.Run("no engine keeps everything", func(t *testing.T) {
		a := withUsersApp(t) // engine nil -> retention disabled
		insertEvent(t, a, "cam", now.Add(-100*24*time.Hour).Unix())
		a.pruneOldEvents()
		if got := countEvents(t, a); got != 1 {
			t.Errorf("events with no engine = %d, want 1", got)
		}
	})

	t.Run("zero retain keeps everything", func(t *testing.T) {
		a := withUsersApp(t)
		eng, err := media.New(media.Options{Retain: 0}) // keep forever
		if err != nil {
			t.Fatalf("media.New: %v", err)
		}
		t.Cleanup(eng.Close)
		a.engine = eng
		insertEvent(t, a, "cam", now.Add(-100*24*time.Hour).Unix())
		a.pruneOldEvents()
		if got := countEvents(t, a); got != 1 {
			t.Errorf("events with retain=0 = %d, want 1", got)
		}
	})
}

func TestHandleStatus(t *testing.T) {
	a := withUsersApp(t)
	a.version = "test-1.2.3"
	insertUser(t, a.db, "admin", "adminpw", "admin")
	insertUser(t, a.db, "bob", "bobpw", "user")
	a.cameras = []camera.Camera{{ID: "front", Name: "Front"}, {ID: "back", Name: "Back"}}
	a.privacy = map[string]bool{"back": true}

	t.Run("non-admin is forbidden", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.handleStatus(w, adminRequest(t, http.MethodGet, "/api/status", "bob", "bobpw", ""))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("admin gets a snapshot", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.handleStatus(w, adminRequest(t, http.MethodGet, "/api/status", "admin", "adminpw", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var resp struct {
			Service          string `json:"service"`
			Version          string `json:"version"`
			RecordingEnabled bool   `json:"recording_enabled"`
			Cameras          []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Privacy bool   `json:"privacy"`
			} `json:"cameras"`
			Totals struct {
				Cameras int `json:"cameras"`
				Privacy int `json:"privacy"`
			} `json:"totals"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Version != "test-1.2.3" {
			t.Errorf("version = %q, want test-1.2.3", resp.Version)
		}
		if resp.Totals.Cameras != 2 {
			t.Errorf("totals.cameras = %d, want 2", resp.Totals.Cameras)
		}
		if resp.Totals.Privacy != 1 {
			t.Errorf("totals.privacy = %d, want 1", resp.Totals.Privacy)
		}
		// engine is nil in this test, so recording is off and no storage block.
		if resp.RecordingEnabled {
			t.Error("recording_enabled = true with no engine; want false")
		}
	})
}
