package media

import (
	"context"
	"testing"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/config"
)

// Duration-valued [media] keys accept anything time.ParseDuration takes plus a
// trailing "d" for days (e.g. "10d" = 240h). Invalid or empty values fall back
// to the option's default; an empty days component ("d") is also invalid.
// "retain" defaults to 7d; the literal "0" keeps footage forever.
func TestOptionsDurationKeys(t *testing.T) {
	cases := []struct {
		name string
		key  string
		raw  string
		def  time.Duration
		want time.Duration
	}{
		{"absent retain defaults to 7d", "retain", "", 0, 7 * 24 * time.Hour},
		{"retain zero keeps forever", "retain", "0", 0, 0},
		{"days retain", "retain", "10d", 0, 10 * 24 * time.Hour},
		{"hours retain", "retain", "240h", 0, 240 * time.Hour},
		{"invalid retain falls back to 7d", "retain", "ten days", 0, 7 * 24 * time.Hour},
		{"empty days retain", "retain", "d", 0, 7 * 24 * time.Hour},
		{"default segment", "segment_duration", "", 60 * time.Second, 60 * time.Second},
		{"minutes segment", "segment_duration", "5m", 60 * time.Second, 5 * time.Minute},
		{"invalid segment", "segment_duration", "30", 60 * time.Second, 60 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sec := config.Section{}
			if c.raw != "" {
				sec[c.key] = c.raw
			}
			var got time.Duration
			switch c.key {
			case "retain":
				got = OptionsFromSection(sec).Retain
			case "segment_duration":
				got = OptionsFromSection(sec).SegmentDuration
			}
			if got != c.want {
				t.Fatalf("%s = %s, want %s", c.key, got, c.want)
			}
		})
	}
	if got := DefaultOptions().Retain; got != 7*24*time.Hour {
		t.Fatalf("DefaultOptions Retain = %s, want 7d", got)
	}
}

// max_part_size is parsed from [media] with a K/M/G suffix, falling back to the
// 50 MiB default when absent or invalid.
func TestOptionsMaxPartSize(t *testing.T) {
	const def = uint64(50 * 1024 * 1024)
	cases := []struct {
		name string
		sec  config.Section
		want uint64
	}{
		{"absent", config.Section{}, def},
		{"megabytes", config.Section{"max_part_size": "10M"}, 10 * 1024 * 1024},
		{"gigabytes", config.Section{"max_part_size": "1G"}, 1024 * 1024 * 1024},
		{"plain bytes", config.Section{"max_part_size": "1048576"}, 1024 * 1024},
		{"invalid falls back", config.Section{"max_part_size": "notasize"}, def},
		{"zero falls back", config.Section{"max_part_size": "0"}, def},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OptionsFromSection(c.sec).MaxPartSize; got != c.want {
				t.Fatalf("MaxPartSize = %d, want %d", got, c.want)
			}
		})
	}

	if got := DefaultOptions().MaxPartSize; got != def {
		t.Fatalf("DefaultOptions MaxPartSize = %d, want %d", got, def)
	}
}

// TestCamCtrlPauseResume exercises the retry-loop pause control the privacy
// endpoint drives: waitWhilePaused blocks while paused and unblocks on resume,
// and setPaused reports whether the state actually changed.
func TestCamCtrlPauseResume(t *testing.T) {
	c := &camCtrl{resumeCh: make(chan struct{})}

	if c.isPaused() {
		t.Fatal("new ctrl should not be paused")
	}
	// waitWhilePaused returns immediately when not paused.
	done := make(chan struct{})
	go func() { c.waitWhilePaused(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused blocked while not paused")
	}

	if !c.setPaused(true) {
		t.Fatal("setPaused(true) should report a change")
	}
	if c.setPaused(true) {
		t.Fatal("setPaused(true) again should report no change")
	}
	if !c.isPaused() {
		t.Fatal("ctrl should be paused")
	}

	// waitWhilePaused now blocks until resumed.
	blocked := make(chan struct{})
	go func() { c.waitWhilePaused(context.Background()); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("waitWhilePaused returned while still paused")
	case <-time.After(50 * time.Millisecond):
	}

	if !c.setPaused(false) {
		t.Fatal("setPaused(false) should report a change")
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused did not wake on resume")
	}
}

// TestCamCtrlWaitCancels ensures a paused waiter unblocks on context cancel so
// engine shutdown never hangs on a camera parked in privacy.
func TestCamCtrlWaitCancels(t *testing.T) {
	c := &camCtrl{resumeCh: make(chan struct{})}
	c.setPaused(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.waitWhilePaused(ctx); close(done) }()

	select {
	case <-done:
		t.Fatal("waitWhilePaused returned before cancel")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused did not return on context cancel")
	}
}

// TestAddCameraGating checks AddCamera's engage decision without spawning a
// recorder goroutine: a camera with no Source, or with every sink resolving
// off, is not engaged. (The happy path connects to real RTSP and is covered by
// integration use, not this unit test.)
func TestAddCameraGating(t *testing.T) {
	// MSE on, relay+record off: New binds no listener and opens no index.
	e, err := New(Options{MSEEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	// No Source -> never engaged, even with sinks on.
	noSource := camera.Camera{ID: "a", Source: "", MSE: true, Relay: true, Record: true}
	if _, ok := e.AddCamera(noSource); ok {
		t.Error("AddCamera with empty Source engaged; want false")
	}
	// Source present but all per-camera sinks off -> not engaged.
	allOff := camera.Camera{ID: "b", Source: "rtsp://x/b", MSE: false, Relay: false, Record: false}
	if _, ok := e.AddCamera(allOff); ok {
		t.Error("AddCamera with all sinks off engaged; want false")
	}
	// Removing a camera that was never added is a no-op.
	if e.RemoveCamera("ghost") {
		t.Error("RemoveCamera(ghost) = true; want false")
	}
}
