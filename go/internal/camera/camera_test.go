package camera

import (
	"os"
	"path/filepath"
	"testing"

	"eneverre/internal/config"
	"eneverre/internal/streamauth"
)

// TestResolveFeatures pins the "master switch + opt-out" gating rule: a sink is
// on only when the global [media] toggle AND the per-camera flag are both on.
// This is the single source of truth used by both the engine and the API.
func TestResolveFeatures(t *testing.T) {
	cases := []struct {
		name                        string
		gMSE, gRelay, gRec          bool // global [media] toggles
		cMSE, cRelay, cRec          bool // per-camera flags
		wantMSE, wantRelay, wantRec bool
	}{
		{"all on", true, true, true, true, true, true, true, true, true},
		{"all global off", false, false, false, true, true, true, false, false, false},
		{"all camera off", true, true, true, false, false, false, false, false, false},

		// opt-out: global on, camera opts a single sink out.
		{"mse opt-out", true, true, true, false, true, true, false, true, true},
		{"relay opt-out", true, true, true, true, false, true, true, false, true},
		{"record opt-out", true, true, true, true, true, false, true, true, false},

		// master switch: a per-camera flag can NOT force a sink on when the
		// global toggle is off (this is opt-out only, not a true override).
		{"record cannot force on", true, true, false, true, true, true, true, true, false},
		{"mse cannot force on", false, true, true, true, true, true, false, true, true},

		// record-only: MSE and relay off, recording on — must still resolve
		// Record=true so the engine engages the camera (the earlier bug skipped
		// it because engage required MSE||relay).
		{"record-only", true, true, true, false, false, true, false, false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cam := Camera{MSE: c.cMSE, Relay: c.cRelay, Record: c.cRec}
			got := cam.ResolveFeatures(c.gMSE, c.gRelay, c.gRec)
			if got.MSE != c.wantMSE || got.Relay != c.wantRelay || got.Record != c.wantRec {
				t.Errorf("ResolveFeatures(g=%v/%v/%v, c=%v/%v/%v) = %+v; want MSE=%v Relay=%v Record=%v",
					c.gMSE, c.gRelay, c.gRec, c.cMSE, c.cRelay, c.cRec,
					got, c.wantMSE, c.wantRelay, c.wantRec)
			}
		})
	}
}

// TestLoadOneFlags verifies the per-camera INI flags parse with the right
// defaults (all true when absent) and honor explicit false.
func TestLoadOneFlags(t *testing.T) {
	cases := []struct {
		name                        string
		body                        string
		wantMSE, wantRelay, wantRec bool
	}{
		{
			name:      "defaults when absent",
			body:      "[camera]\nid = c1\nsource = rtsp://x/c1\n",
			wantMSE:   true,
			wantRelay: true,
			wantRec:   true,
		},
		{
			name:      "all off",
			body:      "[camera]\nid = c1\nsource = rtsp://x/c1\nmse = false\nrelay = false\nrecord = false\n",
			wantMSE:   false,
			wantRelay: false,
			wantRec:   false,
		},
		{
			name:      "record-only",
			body:      "[camera]\nid = c1\nsource = rtsp://x/c1\nmse = false\nrelay = false\n",
			wantMSE:   false,
			wantRelay: false,
			wantRec:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cam.ini")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cam, ok := loadOne(path)
			if !ok {
				t.Fatal("loadOne returned ok=false")
			}
			if cam.MSE != c.wantMSE || cam.Relay != c.wantRelay || cam.Record != c.wantRec {
				t.Errorf("flags = MSE:%v Relay:%v Record:%v; want MSE:%v Relay:%v Record:%v",
					cam.MSE, cam.Relay, cam.Record, c.wantMSE, c.wantRelay, c.wantRec)
			}
		})
	}
}

// TestWithEngineURLs checks the API advertises a feed's URL only when its
// resolved feature is on, and builds the relay URL from rtsp_host / request host.
func TestWithEngineURLs(t *testing.T) {
	creds := streamauth.Creds{Username: "u", Password: "p"}
	cfg := &config.Config{Media: config.Section{"rtsp_host": "cam.lan", "rtsp_address": ":8554"}}

	t.Run("both on", func(t *testing.T) {
		c := Camera{ID: "c1"}.WithEngineURLs(cfg, creds, "10.0.0.1:8080", Features{MSE: true, Relay: true})
		if c.LiveMSE != "/api/camera/c1/live/stream" {
			t.Errorf("LiveMSE = %q; want the live path", c.LiveMSE)
		}
		if want := "rtsp://u:p@cam.lan:8554/c1"; c.RTSP != want {
			t.Errorf("RTSP = %q; want %q", c.RTSP, want)
		}
	})

	t.Run("both off clears URLs", func(t *testing.T) {
		c := Camera{ID: "c1", LiveMSE: "x", RTSP: "y"}.WithEngineURLs(cfg, creds, "10.0.0.1:8080", Features{})
		if c.LiveMSE != "" || c.RTSP != "" {
			t.Errorf("URLs not cleared: LiveMSE=%q RTSP=%q", c.LiveMSE, c.RTSP)
		}
	})

	t.Run("relay uses request host when rtsp_host unset", func(t *testing.T) {
		bare := &config.Config{Media: config.Section{}}
		c := Camera{ID: "c1"}.WithEngineURLs(bare, creds, "192.168.1.5:8080", Features{Relay: true})
		if want := "rtsp://u:p@192.168.1.5:8554/c1"; c.RTSP != want {
			t.Errorf("RTSP = %q; want %q", c.RTSP, want)
		}
	})

	t.Run("relay with no resolvable host clears RTSP", func(t *testing.T) {
		bare := &config.Config{Media: config.Section{}}
		c := Camera{ID: "c1", RTSP: "stale"}.WithEngineURLs(bare, creds, "", Features{Relay: true})
		if c.RTSP != "" {
			t.Errorf("RTSP = %q; want empty when no host resolvable", c.RTSP)
		}
	})
}
