package camera

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
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

// TestLoadPrivacyCapability checks the privacy toggle is offered by default and
// turned off per camera with `privacy = false` (an always-on camera).
func TestLoadPrivacyCapability(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"default on", "[camera]\nid = c1\nsource = rtsp://x/c1\n", true},
		{"opt out", "[camera]\nid = c1\nsource = rtsp://x/c1\nprivacy = false\n", false},
		{"explicit on", "[camera]\nid = c1\nsource = rtsp://x/c1\nprivacy = true\n", true},
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
			if cam.Capabilities.Privacy != c.want {
				t.Errorf("Capabilities.Privacy = %v; want %v", cam.Capabilities.Privacy, c.want)
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

// TestLoadPTZCalibration pins the [thingino] calibration defaults and the
// JSON→Spec mapping. Defaults match the typical thingino gimbal so existing
// installs work without any INI change.
func TestLoadPTZCalibration(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantPanSteps int
		wantPanDeg   int
		wantTiltSt   int
		wantTiltDeg  int
		wantFOVH     float64
	}{
		{
			name:         "defaults when no [thingino] section",
			body:         "[camera]\nid = c1\nsource = rtsp://x/c1\n[thingino]\nptz = true\n",
			wantPanSteps: 2130, wantPanDeg: 360,
			wantTiltSt: 1600, wantTiltDeg: 180,
			wantFOVH: 113,
		},
		{
			name:         "overrides honored",
			body:         "[camera]\nid = c1\nsource = rtsp://x/c1\n[thingino]\nptz = true\npan_steps = 1000\npan_degrees = 270\ntilt_steps = 800\ntilt_degrees = 90\nfov_h = 90.5\n",
			wantPanSteps: 1000, wantPanDeg: 270,
			wantTiltSt: 800, wantTiltDeg: 90,
			wantFOVH: 90.5,
		},
		{
			name:         "no PTZ section at all still gives defaults for the public block",
			body:         "[camera]\nid = c1\nsource = rtsp://x/c1\n",
			wantPanSteps: 2130, wantPanDeg: 360,
			wantTiltSt: 1600, wantTiltDeg: 180,
			wantFOVH: 113,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cam.ini")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, ok := loadSpec(path)
			if !ok {
				t.Fatal("loadSpec returned ok=false")
			}
			if s.PanSteps != c.wantPanSteps || s.PanDegrees != c.wantPanDeg ||
				s.TiltSteps != c.wantTiltSt || s.TiltDegrees != c.wantTiltDeg {
				t.Errorf("calibration = pan:%d/%d tilt:%d/%d; want pan:%d/%d tilt:%d/%d",
					s.PanSteps, s.PanDegrees, s.TiltSteps, s.TiltDegrees,
					c.wantPanSteps, c.wantPanDeg, c.wantTiltSt, c.wantTiltDeg)
			}
			if s.FOVH != c.wantFOVH {
				t.Errorf("FOVH = %v; want %v", s.FOVH, c.wantFOVH)
			}
		})
	}
}

// TestSpecCameraPTZBlock checks the public ptz block is built only when
// capabilities.ptz is true, that the FOV is derived from aspect when not
// configured, and that the steps/degrees calibration stays server-side.
func TestSpecCameraPTZBlock(t *testing.T) {
	t.Run("present when PTZ, derived fov_v from aspect", func(t *testing.T) {
		s := Spec{
			ID: "c1", PTZ: true, Width: 1920, Height: 1080,
			PanSteps: 2130, PanDegrees: 360, TiltSteps: 1600, TiltDegrees: 180,
			FOVH: 113,
		}
		cam := s.Camera()
		if cam.PTZ == nil {
			t.Fatal("PTZ block missing on a PTZ camera")
		}
		// 113 * 1080/1920 ≈ 63.5625
		wantV := 113.0 * 1080.0 / 1920.0
		if got := cam.PTZ.FOVH; got != 113 {
			t.Errorf("FOVH = %v; want 113", got)
		}
		if got := cam.PTZ.FOVV; got != wantV {
			t.Errorf("FOVV = %v; want %v (derived from aspect)", got, wantV)
		}
		if cam.PTZ.PanRange != 360 {
			t.Errorf("PanRange = %v; want 360", cam.PTZ.PanRange)
		}
		if cam.PTZ.TiltRange != 180 {
			t.Errorf("TiltRange = %v; want 180", cam.PTZ.TiltRange)
		}
		// Server-side calibration copied but kept off the wire.
		if cam.PanSteps != 2130 || cam.PanDegrees != 360 {
			t.Errorf("internal calibration not copied: pan=%d/%d", cam.PanSteps, cam.PanDegrees)
		}
		// Public JSON must not contain steps or degrees calibration.
		out, _ := json.Marshal(cam)
		if strings.Contains(string(out), "pan_steps") || strings.Contains(string(out), "pan_degrees") {
			t.Errorf("public JSON leaked calibration: %s", out)
		}
	})

	t.Run("absent when not PTZ", func(t *testing.T) {
		s := Spec{ID: "c1", PTZ: false, Width: 16, Height: 9}
		cam := s.Camera()
		if cam.PTZ != nil {
			t.Errorf("PTZ block present on a non-PTZ camera: %+v", cam.PTZ)
		}
	})

	t.Run("zero width falls back to fov_v = fov_h", func(t *testing.T) {
		s := Spec{ID: "c1", PTZ: true, Width: 0, Height: 0, FOVH: 90}
		cam := s.Camera()
		if cam.PTZ == nil || cam.PTZ.FOVV != 90 {
			t.Errorf("FOVV with zero aspect = %v; want 90", cam.PTZ.FOVV)
		}
	})

	t.Run("missing fov_h uses default", func(t *testing.T) {
		s := Spec{ID: "c1", PTZ: true, Width: 16, Height: 9, FOVH: 0}
		cam := s.Camera()
		if cam.PTZ == nil || cam.PTZ.FOVH != DefaultFOVH {
			t.Errorf("FOVH = %v; want default %v", cam.PTZ.FOVH, DefaultFOVH)
		}
	})
}

// TestPTZDeltaToSteps exercises the public→firmware conversion: it must scale
// linearly with the calibration, clamp runaway requests to half the range in
// each direction, and short-circuit on missing/invalid calibration so a
// misconfigured camera never produces a divide-by-zero or NaN.
func TestPTZDeltaToSteps(t *testing.T) {
	cam := Camera{PanSteps: 2130, PanDegrees: 360, TiltSteps: 1600, TiltDegrees: 180}

	t.Run("linear scaling", func(t *testing.T) {
		// 10° pan -> 2130 * 10 / 360 ≈ 59.17
		// 10° tilt -> 1600 * 10 / 180 ≈ 88.89
		x, y := cam.PTZDeltaToSteps(10, 10)
		if math.Abs(x-59.1667) > 0.01 {
			t.Errorf("x = %v; want ≈59.17", x)
		}
		if math.Abs(y-88.8889) > 0.01 {
			t.Errorf("y = %v; want ≈88.89", y)
		}
	})

	t.Run("clamps to half range", func(t *testing.T) {
		// Anything beyond ±pan_range/2 in degrees is clamped to half the steps.
		x, _ := cam.PTZDeltaToSteps(1000, 0)
		if x != 2130/2 {
			t.Errorf("oversize pan clamp = %v; want %v", x, 2130/2)
		}
		x, _ = cam.PTZDeltaToSteps(-1000, 0)
		if x != -2130/2 {
			t.Errorf("negative oversize pan clamp = %v; want %v", x, -2130/2)
		}
		_, y := cam.PTZDeltaToSteps(0, 1000)
		if y != 1600/2 {
			t.Errorf("oversize tilt clamp = %v; want %v", y, 1600/2)
		}
	})

	t.Run("zero calibration is a no-op", func(t *testing.T) {
		// A misconfigured camera (no calibration) must produce 0,0 instead of
		// dividing by zero and forwarding NaN to the firmware.
		broken := Camera{}
		x, y := broken.PTZDeltaToSteps(10, 10)
		if x != 0 || y != 0 {
			t.Errorf("zero calibration = x:%v y:%v; want 0,0", x, y)
		}
	})
}

// TestPTZStepsToDegrees checks the inverse conversion used by /ptz/position:
// firmware-native xpos/ypos land in the public pan/tilt unit through the same
// calibration, and a zero calibration short-circuits to zero (rather than
// producing NaN from a 0/0).
func TestPTZStepsToDegrees(t *testing.T) {
	cam := Camera{PanSteps: 2130, PanDegrees: 360, TiltSteps: 1600, TiltDegrees: 180}
	pan, tilt := cam.PTZStepsToDegrees(2130, 1600)
	if math.Abs(pan-360) > 0.01 {
		t.Errorf("pan = %v; want 360", pan)
	}
	if math.Abs(tilt-180) > 0.01 {
		t.Errorf("tilt = %v; want 180", tilt)
	}
	// Round-trip: 10° -> steps -> 10° (within float precision).
	x, y := cam.PTZDeltaToSteps(10, 10)
	pan, tilt = cam.PTZStepsToDegrees(x, y)
	if math.Abs(pan-10) > 0.001 || math.Abs(tilt-10) > 0.001 {
		t.Errorf("round-trip = %v, %v; want ≈10, 10", pan, tilt)
	}
}

// TestPTZDegreesToSteps pins the absolute-move conversion used by home and
// privacy: the same scaling as the relative path, no range clamp (the
// firmware is the authority on its end-stops), and a round-trip that lands
// within float precision.
func TestPTZDegreesToSteps(t *testing.T) {
	cam := Camera{PanSteps: 2130, PanDegrees: 360, TiltSteps: 1600, TiltDegrees: 180}

	t.Run("linear scaling", func(t *testing.T) {
		// 180° pan -> 2130*180/360 = 1065
		// 90° tilt -> 1600*90/180 = 800
		x, y := cam.PTZDegreesToSteps(180, 90)
		if math.Abs(x-1065) > 0.001 {
			t.Errorf("x = %v; want 1065", x)
		}
		if math.Abs(y-800) > 0.001 {
			t.Errorf("y = %v; want 800", y)
		}
	})

	t.Run("does not clamp", func(t *testing.T) {
		// 720° (two full revolutions) on a 360° gimbal: the relative path
		// would clamp to ±half range, but the absolute path must let the
		// firmware decide what to do.
		x, _ := cam.PTZDegreesToSteps(720, 0)
		if math.Abs(x-4260) > 0.001 {
			t.Errorf("x = %v; want 4260 (no clamp on absolute path)", x)
		}
	})

	t.Run("round-trip", func(t *testing.T) {
		// 137.5° pan / 42.25° tilt -> steps -> degrees, within float precision.
		x, y := cam.PTZDegreesToSteps(137.5, 42.25)
		pan, tilt := cam.PTZStepsToDegrees(x, y)
		if math.Abs(pan-137.5) > 0.001 || math.Abs(tilt-42.25) > 0.001 {
			t.Errorf("round-trip = %v, %v; want ≈137.5, 42.25", pan, tilt)
		}
	})

	t.Run("zero calibration is a no-op", func(t *testing.T) {
		broken := Camera{}
		x, y := broken.PTZDegreesToSteps(45, 30)
		if x != 0 || y != 0 {
			t.Errorf("zero calibration = x:%v y:%v; want 0,0", x, y)
		}
	})
}
