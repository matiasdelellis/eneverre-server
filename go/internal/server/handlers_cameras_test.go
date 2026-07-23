package server

import "testing"

// TestCreateCameraReqSpec pins the create-request validation and defaulting:
// the source is required, transport is constrained, and omitted flags fall back
// to the same defaults the INI loader applies. The id is never part of the
// request — the Spec it returns carries an empty ID, which the handler fills by
// slugging the name.
func TestCreateCameraReqSpec(t *testing.T) {
	t.Run("spec carries no id (derived from the name by the handler)", func(t *testing.T) {
		req := createCameraReq{Name: "Front door", Source: "rtsp://x/y"}
		s, msg := req.spec()
		if msg != "" {
			t.Fatalf("valid request rejected: %s", msg)
		}
		if s.ID != "" {
			t.Errorf("spec() invented an id %q; derivation is the handler's job", s.ID)
		}
	})

	t.Run("name required", func(t *testing.T) {
		// The name is the camera's identity (and the id is slugged from it), so
		// it is required on both create and update.
		for _, name := range []string{"", "   "} {
			req := createCameraReq{Name: name, Source: "rtsp://x/y"}
			if _, msg := req.spec(); msg == "" {
				t.Errorf("empty name %q accepted; want rejected", name)
			}
		}
	})

	t.Run("source required", func(t *testing.T) {
		req := createCameraReq{Name: "Cam", Source: "  "}
		if _, msg := req.spec(); msg == "" {
			t.Error("empty source accepted; want rejected")
		}
	})

	t.Run("transport constrained", func(t *testing.T) {
		req := createCameraReq{Name: "Cam", Source: "rtsp://x/y", Transport: "quic"}
		if _, msg := req.spec(); msg == "" {
			t.Error("bad transport accepted; want rejected")
		}
		for _, tr := range []string{"", "auto", "tcp", "udp", "TCP"} {
			req := createCameraReq{Name: "Cam", Source: "rtsp://x/y", Transport: tr}
			if _, msg := req.spec(); msg != "" {
				t.Errorf("transport %q rejected: %s", tr, msg)
			}
		}
	})

	t.Run("defaults applied when flags omitted", func(t *testing.T) {
		req := createCameraReq{Name: "Cam", Source: "rtsp://x/y"}
		s, msg := req.spec()
		if msg != "" {
			t.Fatalf("unexpected validation error: %s", msg)
		}
		if !s.Record || !s.MSE || !s.Relay || !s.Privacy {
			t.Errorf("record/mse/relay/privacy defaults = %v/%v/%v/%v; want all true", s.Record, s.MSE, s.Relay, s.Privacy)
		}
		// Playback defaults to the record value, which defaults true.
		if !s.Playback {
			t.Error("playback default = false; want true (follows record)")
		}
		if s.Width != 16 || s.Height != 9 {
			t.Errorf("width/height defaults = %d/%d; want 16/9", s.Width, s.Height)
		}
		if s.HomeX != -1 || s.PrivacyY != -1 {
			t.Errorf("thingino coords default = %v/%v; want -1", s.HomeX, s.PrivacyY)
		}
	})

	t.Run("explicit false flags honored", func(t *testing.T) {
		no := false
		req := createCameraReq{Name: "Cam", Source: "rtsp://x/y", Record: &no, MSE: &no}
		s, _ := req.spec()
		if s.Record || s.MSE {
			t.Errorf("explicit false not honored: record=%v mse=%v", s.Record, s.MSE)
		}
		if !s.Relay {
			t.Error("relay should still default true when only record/mse set false")
		}
		// Playback was omitted, so it follows record — which is explicitly false here.
		if s.Playback {
			t.Error("playback should follow record=false when omitted")
		}
	})

	t.Run("explicit playback overrides the record-derived default", func(t *testing.T) {
		no, yes := false, true
		req := createCameraReq{Name: "Cam", Source: "rtsp://x/y", Record: &no, Playback: &yes}
		s, _ := req.spec()
		if s.Record {
			t.Error("record should be false")
		}
		if !s.Playback {
			t.Error("explicit playback=true not honored over record=false")
		}
	})

	t.Run("trims whitespace", func(t *testing.T) {
		req := createCameraReq{Name: "  Front  ", Source: "  rtsp://x/y  "}
		s, msg := req.spec()
		if msg != "" {
			t.Fatalf("unexpected error: %s", msg)
		}
		if s.Name != "Front" || s.Source != "rtsp://x/y" {
			t.Errorf("not trimmed: %+v", s)
		}
	})
}
