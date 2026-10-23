package zaguan

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/getzaguan/zaguan-sourceapi"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildSnapshotMapsCameras(t *testing.T) {
	p := &Publisher{
		cfg: Config{
			Version: "1.2.3",
			Cameras: func() []Camera {
				return []Camera{
					{
						ID: "front-door", Name: "Front door", Comment: "Porch",
						Location: "Entrance", Enabled: true, Connected: true,
						Live: true, Recording: true, Record: true, Width: 1920, Height: 1080,
						RTSP:        "rtsp://u:p@nvr:8554/front-door",
						MSEURL:      "https://cams.example.com/api/camera/front-door/live/stream?user=u&pass=p",
						MSEInfoURL:  "https://cams.example.com/api/camera/front-door/live/info?user=u&pass=p",
						HLSURL:      "https://cams.example.com/api/camera/front-door/recordings/hls/playlist.m3u8?start=x&end=y&user=u&pass=p",
						SnapshotURL: "https://cams.example.com/api/camera/front-door/thumbnail?user=u&pass=p",
						Snapshot:    true, Playback: true, Talk: true, PTZ: true, PTZHome: true,
					},
					{
						ID: "garden", Name: "Garden", Location: "Entrance",
						Enabled: true, Privacy: true,
						Live: true, RTSP: "rtsp://u:p@nvr:8554/garden", Snapshot: true,
					},
					{
						ID: "attic", Name: "Attic", Enabled: false,
					},
				}
			},
		},
		log: testLogger(),
	}

	snap := p.buildSnapshot()
	if len(snap.Devices) != 3 || len(snap.Entities) != 3 {
		t.Fatalf("devices/entities = %d/%d, want 3/3", len(snap.Devices), len(snap.Entities))
	}
	if len(snap.Areas) != 1 {
		t.Fatalf("areas = %+v, want the two Entrance cameras to share one", snap.Areas)
	}
	if snap.Areas[0].ID != "eneverre:entrance" || snap.Areas[0].Name != "Entrance" {
		t.Errorf("area = %+v", snap.Areas[0])
	}

	byID := map[string]sourceapi.Entity{}
	for _, e := range snap.Entities {
		byID[e.UniqueID] = e
	}
	front := byID["eneverre:front-door/camera"]
	if front.Domain != "camera" {
		t.Errorf("domain = %q", front.Domain)
	}
	if front.State != "streaming" {
		t.Errorf("front state = %v, want streaming", front.State)
	}
	if front.Available == nil || !*front.Available {
		t.Errorf("front available = %v", front.Available)
	}
	wantCommands := []string{"snapshot", "ptz_move", "ptz_home", "ptz_recalibrate"}
	if len(front.Commands) != len(wantCommands) {
		t.Errorf("commands = %v, want %v", front.Commands, wantCommands)
	} else {
		for i := range wantCommands {
			if front.Commands[i] != wantCommands[i] {
				t.Errorf("commands = %v, want %v", front.Commands, wantCommands)
				break
			}
		}
	}
	wantCaps := []string{"live", "snapshot", "recording", "ptz"}
	if len(front.Capabilities) != len(wantCaps) {
		t.Errorf("capabilities = %v, want %v", front.Capabilities, wantCaps)
	} else {
		for i := range wantCaps {
			if front.Capabilities[i] != wantCaps[i] {
				t.Errorf("capabilities = %v, want %v", front.Capabilities, wantCaps)
				break
			}
		}
	}
	if front.Attributes["rtsp_url"] != "rtsp://u:p@nvr:8554/front-door" {
		t.Errorf("rtsp_url = %v", front.Attributes["rtsp_url"])
	}
	if front.Attributes["mse_url"] != "https://cams.example.com/api/camera/front-door/live/stream?user=u&pass=p" {
		t.Errorf("mse_url = %v", front.Attributes["mse_url"])
	}
	if front.Attributes["mse_info_url"] == nil {
		t.Error("mse_info_url was not published")
	}
	if front.Attributes["hls_url"] == nil {
		t.Error("hls_url was not published")
	}
	if front.Attributes["aspect_ratio"] != "1920:1080" {
		t.Errorf("aspect_ratio = %v", front.Attributes["aspect_ratio"])
	}

	garden := byID["eneverre:garden/camera"]
	if garden.State != "idle" {
		t.Errorf("privacy camera state = %v, want idle", garden.State)
	}
	if garden.Available == nil || *garden.Available {
		t.Errorf("privacy camera available = %v, want false", garden.Available)
	}
	if garden.Attributes["privacy"] != true {
		t.Errorf("privacy attribute = %v", garden.Attributes["privacy"])
	}

	attic := byID["eneverre:attic/camera"]
	if attic.Available == nil || *attic.Available {
		t.Errorf("disabled camera available = %v, want false", attic.Available)
	}
	if len(attic.Commands) != 0 {
		t.Errorf("a camera without snapshots offers commands: %v", attic.Commands)
	}
	var atticDevice sourceapi.Device
	for _, d := range snap.Devices {
		if d.UniqueID == "eneverre:attic" {
			atticDevice = d
		}
	}
	if !atticDevice.Disabled {
		t.Error("disabled camera was not marked disabled on the device")
	}
}

// fakeHub is a minimal Source API hub: it stores the last snapshot, serves one
// queued command and records the answers.
type fakeHub struct {
	mu        sync.Mutex
	snapshots []sourceapi.Snapshot
	metas     []sourceapi.Meta
	results   []sourceapi.CommandResult
	commands  chan sourceapi.Command

	conflictOnce bool
	conflicted   bool
}

func newFakeHub(t *testing.T) (*fakeHub, *httptest.Server) {
	t.Helper()
	h := &fakeHub{commands: make(chan sourceapi.Command, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/source", func(w http.ResponseWriter, r *http.Request) {
		var meta sourceapi.Meta
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			t.Errorf("meta decode: %v", err)
		}
		h.mu.Lock()
		h.metas = append(h.metas, meta)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /api/v1/source/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var snap sourceapi.Snapshot
		if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
			t.Errorf("snapshot decode: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		if h.conflictOnce && !h.conflicted {
			h.conflicted = true
			h.mu.Unlock()
			http.Error(w, `{"code":"stale_snapshot"}`, http.StatusConflict)
			return
		}
		h.snapshots = append(h.snapshots, snap)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /api/v1/source/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"online":true,"lease_seconds":90}`))
	})
	mux.HandleFunc("GET /api/v1/source/commands", func(w http.ResponseWriter, r *http.Request) {
		select {
		case cmd := <-h.commands:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]sourceapi.Command{cmd})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST /api/v1/source/commands/{id}", func(w http.ResponseWriter, r *http.Request) {
		var result sourceapi.CommandResult
		if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
			t.Errorf("result decode: %v", err)
		}
		h.mu.Lock()
		h.results = append(h.results, result)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *fakeHub) snapshotCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.snapshots)
}

func (h *fakeHub) resultCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.results)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunPublishesAndAnswersSnapshot(t *testing.T) {
	hub, srv := newFakeHub(t)
	hub.commands <- sourceapi.Command{
		ID:             "cmd_1",
		EntityUniqueID: "eneverre:front-door/camera",
		Command:        "snapshot",
	}

	snapshotted := make(chan string, 1)
	p, err := New(Config{
		BaseURL:           srv.URL,
		Token:             "zsrc_kid_secret",
		Name:              "Test NVR",
		Version:           "9.9.9",
		Logger:            testLogger(),
		SyncInterval:      20 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		CommandWait:       50 * time.Millisecond,
		Cameras: func() []Camera {
			return []Camera{{
				ID: "front-door", Name: "Front door", Enabled: true,
				Connected: true, Snapshot: true,
			}}
		},
		Snapshot: func(ctx context.Context, id string) error {
			snapshotted <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := p.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	waitFor(t, "the first snapshot", func() bool { return hub.snapshotCount() > 0 })
	waitFor(t, "the command result", func() bool { return hub.resultCount() > 0 })

	select {
	case id := <-snapshotted:
		if id != "front-door" {
			t.Errorf("snapshot camera = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("the snapshot command did not reach the camera")
	}

	hub.mu.Lock()
	snap := hub.snapshots[0]
	meta := hub.metas
	var result sourceapi.CommandResult
	if len(hub.results) > 0 {
		result = hub.results[0]
	}
	hub.mu.Unlock()

	if snap.Version == 0 {
		t.Error("snapshot version was not set")
	}
	if len(snap.Devices) != 1 || len(snap.Entities) != 1 {
		t.Errorf("snapshot = %d devices, %d entities", len(snap.Devices), len(snap.Entities))
	}
	if len(meta) != 1 || meta[0].Name != "Test NVR" || meta[0].Kind != "eneverre" {
		t.Errorf("meta = %+v", meta)
	}
	if !result.OK || result.State != "streaming" {
		t.Errorf("command result = %+v", result)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// TestExecutePTZCommands checks that the source passes the three PTZ
// commands to the server callbacks and answers the hub.
func TestExecutePTZCommands(t *testing.T) {
	hub, srv := newFakeHub(t)
	moved := make(chan [2]float64, 1)
	home := make(chan string, 1)
	recalibrated := make(chan string, 1)
	p, err := New(Config{
		BaseURL: srv.URL,
		Token:   "zsrc_kid_secret",
		Logger:  testLogger(),
		Cameras: func() []Camera {
			return []Camera{{ID: "cam", Name: "Cam", Enabled: true, Connected: true, PTZ: true}}
		},
		PTZMove: func(_ context.Context, id string, pan, tilt float64) error {
			if id != "cam" {
				t.Errorf("PTZ camera = %q", id)
			}
			moved <- [2]float64{pan, tilt}
			return nil
		},
		PTZHome: func(_ context.Context, id string) error {
			home <- id
			return nil
		},
		PTZRecalibrate: func(_ context.Context, id string) error {
			recalibrated <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	p.execute(ctx, sourceapi.Command{ID: "move", EntityUniqueID: "eneverre:cam/camera", Command: "ptz_move", Args: map[string]any{"pan": float64(-10), "tilt": float64(5)}})
	select {
	case got := <-moved:
		if got != [2]float64{-10, 5} {
			t.Errorf("move = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ptz_move did not reach the server")
	}
	p.execute(ctx, sourceapi.Command{ID: "home", EntityUniqueID: "eneverre:cam/camera", Command: "ptz_home"})
	select {
	case got := <-home:
		if got != "cam" {
			t.Errorf("home camera = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ptz_home did not reach the server")
	}
	p.execute(ctx, sourceapi.Command{ID: "recalibrate", EntityUniqueID: "eneverre:cam/camera", Command: "ptz_recalibrate"})
	select {
	case got := <-recalibrated:
		if got != "cam" {
			t.Errorf("recalibrate camera = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ptz_recalibrate did not reach the server")
	}

	waitFor(t, "the three command results", func() bool { return hub.resultCount() == 3 })
	hub.mu.Lock()
	results := append([]sourceapi.CommandResult(nil), hub.results...)
	hub.mu.Unlock()
	for _, result := range results {
		if !result.OK || result.State != "streaming" {
			t.Errorf("result = %+v", result)
		}
	}
}

func TestSnapshotVersionAdvancesAfterConflict(t *testing.T) {
	hub, srv := newFakeHub(t)
	hub.conflictOnce = true

	p, err := New(Config{
		BaseURL:           srv.URL,
		Token:             "zsrc_kid_secret",
		Logger:            testLogger(),
		SyncInterval:      time.Hour,
		HeartbeatInterval: time.Hour,
		CommandWait:       50 * time.Millisecond,
		Cameras: func() []Camera {
			return []Camera{{ID: "cam", Name: "Cam", Enabled: true, Connected: true}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Run(ctx)
	}()

	waitFor(t, "the snapshot after the conflict", func() bool { return hub.snapshotCount() > 0 })
	hub.mu.Lock()
	snap := hub.snapshots[0]
	conflicted := hub.conflicted
	hub.mu.Unlock()
	if !conflicted {
		t.Fatal("the fake hub did not answer the first snapshot with 409")
	}
	if snap.Version == 0 {
		t.Error("snapshot version was not set")
	}

	cancel()
	<-done
}
