package server

import "testing"

// TestCreateCameraReqSpec pins the create-request validation and defaulting:
// the id must be a safe token, the source is required, transport is constrained,
// and omitted flags fall back to the same defaults the INI loader applies.
func TestCreateCameraReqSpec(t *testing.T) {
	t.Run("rejects bad ids", func(t *testing.T) {
		bad := []string{"", "-lead", "has space", "path/traversal", "..", "a.b", "über"}
		for _, id := range bad {
			req := createCameraReq{ID: id, Source: "rtsp://x/y"}
			if _, msg := req.spec(); msg == "" {
				t.Errorf("id %q accepted; want rejected", id)
			}
		}
	})

	t.Run("accepts good ids", func(t *testing.T) {
		good := []string{"frente", "cam-01", "cam_02", "A1", "0"}
		for _, id := range good {
			req := createCameraReq{ID: id, Source: "rtsp://x/y"}
			if _, msg := req.spec(); msg != "" {
				t.Errorf("id %q rejected: %s", id, msg)
			}
		}
	})

	t.Run("source required", func(t *testing.T) {
		req := createCameraReq{ID: "cam", Source: "  "}
		if _, msg := req.spec(); msg == "" {
			t.Error("empty source accepted; want rejected")
		}
	})

	t.Run("transport constrained", func(t *testing.T) {
		req := createCameraReq{ID: "cam", Source: "rtsp://x/y", Transport: "quic"}
		if _, msg := req.spec(); msg == "" {
			t.Error("bad transport accepted; want rejected")
		}
		for _, tr := range []string{"", "auto", "tcp", "udp", "TCP"} {
			req := createCameraReq{ID: "cam", Source: "rtsp://x/y", Transport: tr}
			if _, msg := req.spec(); msg != "" {
				t.Errorf("transport %q rejected: %s", tr, msg)
			}
		}
	})

	t.Run("defaults applied when flags omitted", func(t *testing.T) {
		req := createCameraReq{ID: "cam", Source: "rtsp://x/y"}
		s, msg := req.spec()
		if msg != "" {
			t.Fatalf("unexpected validation error: %s", msg)
		}
		if !s.Record || !s.MSE || !s.Relay || !s.Privacy {
			t.Errorf("record/mse/relay/privacy defaults = %v/%v/%v/%v; want all true", s.Record, s.MSE, s.Relay, s.Privacy)
		}
		if s.Playback {
			t.Error("playback default = true; want false")
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
		req := createCameraReq{ID: "cam", Source: "rtsp://x/y", Record: &no, MSE: &no}
		s, _ := req.spec()
		if s.Record || s.MSE {
			t.Errorf("explicit false not honored: record=%v mse=%v", s.Record, s.MSE)
		}
		if !s.Relay {
			t.Error("relay should still default true when only record/mse set false")
		}
	})

	t.Run("trims whitespace", func(t *testing.T) {
		req := createCameraReq{ID: "  cam  ", Name: "  Front  ", Source: "  rtsp://x/y  "}
		s, msg := req.spec()
		if msg != "" {
			t.Fatalf("unexpected error: %s", msg)
		}
		if s.ID != "cam" || s.Name != "Front" || s.Source != "rtsp://x/y" {
			t.Errorf("not trimmed: %+v", s)
		}
	})
}
