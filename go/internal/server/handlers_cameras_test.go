package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/streamauth"
)

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

	t.Run("enabled defaults to true and honors explicit false", func(t *testing.T) {
		// Omitted → in service (the default), matching the INI loader.
		req := createCameraReq{Name: "Cam", Source: "rtsp://x/y"}
		s, msg := req.spec()
		if msg != "" {
			t.Fatalf("unexpected error: %s", msg)
		}
		if !s.Enabled {
			t.Error("enabled default = false; want true")
		}
		// Explicitly disabled via the request body.
		no := false
		req = createCameraReq{Name: "Cam", Source: "rtsp://x/y", Enabled: &no}
		s, _ = req.spec()
		if s.Enabled {
			t.Error("explicit enabled=false not honored")
		}
	})
}

// TestPublicCameraDisabled pins the API view of a disabled camera: stream URLs
// cleared and every runtime capability stripped (nothing to pause, move, talk
// to, configure or snapshot), while Playback survives — the whole point of
// disabling over deleting is that stored footage stays browsable.
func TestPublicCameraDisabled(t *testing.T) {
	a := withUsersApp(t)
	creds, err := streamauth.NewStore(a.db)
	if err != nil {
		t.Fatalf("streamauth.NewStore: %v", err)
	}
	a.creds = creds

	base := camera.Camera{
		ID:          "cam1",
		Name:        "Cam",
		Source:      "rtsp://x/y",
		MSE:         true,
		Relay:       true,
		Record:      false,
		Privacy:     false,
		ScheduleOff: false,
		Capabilities: camera.Capabilities{
			Playback:   true,
			Privacy:    true,
			Thumbnail:  true,
			PTZ:        true,
			Talk:       true,
			Settings:   true,
			TalkCodecs: []string{"aac"},
		},
	}
	base.PTZ = &camera.PTZMetadata{FOVH: 113, FOVV: 63.6, PanRange: 360, TiltRange: 180}

	// Playback depends on the engine actually having recordings for the camera
	// (publicCamera gates it on HasRecordings), which this nil-engine test can't
	// satisfy — so capture the enabled camera's value and require the disabled
	// branch to preserve it (it must never strip Playback itself).
	var enabledPlayback bool

	t.Run("enabled camera keeps streams and capabilities", func(t *testing.T) {
		c := base
		c.Enabled = true
		out := a.publicCamera(c, "host")
		if out.LiveMSE == "" || out.RTSP == "" {
			t.Errorf("enabled camera lost its stream URLs: live=%q rtsp=%q", out.LiveMSE, out.RTSP)
		}
		if !out.Capabilities.Privacy || !out.Capabilities.Talk || out.PTZ == nil {
			t.Errorf("enabled camera lost runtime capabilities: %+v", out.Capabilities)
		}
		enabledPlayback = out.Capabilities.Playback
	})

	t.Run("disabled camera is stripped to playback-only", func(t *testing.T) {
		c := base
		c.Enabled = false
		out := a.publicCamera(c, "host")
		if out.LiveMSE != "" || out.RTSP != "" {
			t.Errorf("disabled camera kept stream URLs: live=%q rtsp=%q", out.LiveMSE, out.RTSP)
		}
		if out.Privacy || out.ScheduleOff {
			t.Error("disabled camera should report privacy/schedule_off false")
		}
		caps := out.Capabilities
		if caps.Privacy || caps.Thumbnail || caps.PTZ || caps.Talk || caps.Settings || caps.TalkCodecs != nil {
			t.Errorf("disabled camera kept runtime capabilities: %+v", caps)
		}
		if out.PTZ != nil {
			t.Error("disabled camera kept its PTZ block")
		}
		if caps.Playback != enabledPlayback {
			t.Errorf("disabled camera lost Playback (%v vs enabled %v) — stored footage must stay advertised", caps.Playback, enabledPlayback)
		}
	})
}

// TestPrivacyRejectsDisabledCamera pins that the interactive endpoints refuse a
// disabled camera with 409 (distinct from the 404 "not available" used when the
// capability is simply absent).
func TestPrivacyRejectsDisabledCamera(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")
	a.cameras = []camera.Camera{{
		ID:           "cam1",
		Name:         "Cam",
		Enabled:      false,
		Capabilities: camera.Capabilities{Privacy: true},
	}}

	req := adminRequest(t, http.MethodPost, "/api/camera/cam1/privacy?enable=true", "admin", "adminpw", "")
	req.SetPathValue("cam_id", "cam1")
	w := httptest.NewRecorder()
	a.handlePrivacy(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("privacy on disabled camera = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
}

// TestUpdateCameraReseedsRuntimeState pins the re-probe on edit: handleUpdateCamera
// wipes the runtime caches (dropCameraState) and must seed them again, or an
// in-service camera silently loses its heartbeat, talk codecs and PTZ position
// until the next restart. A disabled camera, by contrast, is never probed — and
// re-enabling one must pick up the camera's *real* current state, since whatever
// was cached before it went out of service is meaningless by then.
func TestUpdateCameraReseedsRuntimeState(t *testing.T) {
	// The fake firmware counts only heartbeat probes (the other seeds may hit it
	// too) and reports whatever privacy state the subtest asks for.
	var mu sync.Mutex
	hits := 0
	privacyOn := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "json-heartbeat-slow.cgi") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		hits++
		on := privacyOn
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"privacy_enabled":%t}`, on)
	}))
	defer srv.Close()
	probes := func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
	setPrivacy := func(on bool) {
		mu.Lock()
		privacyOn = on
		mu.Unlock()
	}

	// newApp stores one camera (in service unless `enabled` says otherwise) whose
	// thingino endpoint is the fake firmware, and mirrors it into the in-memory
	// set the handlers read.
	newApp := func(t *testing.T, enabled bool) *App {
		a := withUsersApp(t)
		insertUser(t, a.db, "admin", "adminpw", "admin")
		creds, err := streamauth.NewStore(a.db)
		if err != nil {
			t.Fatalf("streamauth.NewStore: %v", err)
		}
		a.creds = creds
		a.camStore = camera.NewStore(a.db)
		a.privacy = map[string]bool{}
		a.schedOff = map[string]bool{}
		a.talkCodecs = map[string][]string{}
		a.ptzPos = map[string]ptzPos{}
		a.heartbeats = map[string]heartbeatInfo{}
		spec := camera.Spec{
			ID: "cam1", Name: "Cam", Source: "rtsp://x/y", Enabled: enabled,
			Privacy: true, ThinginoURL: srv.URL, ThinginoAPIKey: "k",
		}
		spec.ApplyPTZDefaults()
		if _, err := a.camStore.Create(spec, 1); err != nil {
			t.Fatalf("camStore.Create: %v", err)
		}
		a.cameras = []camera.Camera{spec.Camera()}
		return a
	}

	update := func(t *testing.T, a *App, enabled bool) {
		t.Helper()
		body := fmt.Sprintf(`{"name":"Cam","source":"rtsp://x/y","privacy":true,`+
			`"thingino_url":%q,"thingino_api_key":"k","enabled":%t}`, srv.URL, enabled)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, adminRequest(t, http.MethodPut, "/api/camera/cam1", "admin", "adminpw", body))
		if w.Code != http.StatusOK {
			t.Fatalf("update = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
	}

	// The seeds run in background goroutines, so wait for the cache to fill
	// rather than racing it.
	waitFor := func(cond func() bool) bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	heartbeatCached := func(a *App) bool {
		a.heartbeatsMu.RLock()
		defer a.heartbeatsMu.RUnlock()
		_, ok := a.heartbeats["cam1"]
		return ok
	}

	t.Run("enabled camera is re-probed", func(t *testing.T) {
		setPrivacy(false)
		a := newApp(t, true)
		before := probes()
		update(t, a, true)
		if !waitFor(func() bool { return heartbeatCached(a) }) {
			t.Error("heartbeat cache empty after updating an enabled camera; the update must re-seed it")
		}
		if probes() == before {
			t.Error("the camera was never probed after the update")
		}
	})

	t.Run("disabled camera is not probed", func(t *testing.T) {
		setPrivacy(false)
		a := newApp(t, true)
		before := probes()
		update(t, a, false)
		// Nothing to wait for — give any (wrongly) spawned probe a moment to land.
		time.Sleep(300 * time.Millisecond)
		if probes() != before {
			t.Errorf("disabled camera was probed %d time(s); want 0", probes()-before)
		}
		if heartbeatCached(a) {
			t.Error("disabled camera populated the heartbeat cache")
		}
	})

	t.Run("re-enabling picks up the camera's real privacy state", func(t *testing.T) {
		// While the camera was out of service someone left the lens blacked out.
		// Re-enabling must read that back from the firmware, not assume the
		// dropped (false) state.
		setPrivacy(true)
		a := newApp(t, false)
		update(t, a, true)
		ok := waitFor(func() bool {
			a.privacyMu.RLock()
			defer a.privacyMu.RUnlock()
			return a.privacy["cam1"]
		})
		if !ok {
			t.Error("privacy state not refreshed from the camera after re-enabling")
		}
	})
}

// TestUpdateCameraKeepsManualPrivacy pins that editing a camera doesn't lift
// the operator's privacy: the update wipes the derived runtime caches, but
// manual privacy is intent, and dropping it would resume recording and
// transmission on a camera the operator had paused (a non-thingino camera has
// no heartbeat to restore it).
func TestUpdateCameraKeepsManualPrivacy(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")
	creds, err := streamauth.NewStore(a.db)
	if err != nil {
		t.Fatalf("streamauth.NewStore: %v", err)
	}
	a.creds = creds
	a.camStore = camera.NewStore(a.db)
	a.privacy = map[string]bool{"cam1": true}
	a.schedOff = map[string]bool{}
	a.talkCodecs = map[string][]string{}
	a.ptzPos = map[string]ptzPos{}
	a.heartbeats = map[string]heartbeatInfo{}
	spec := camera.Spec{ID: "cam1", Name: "Cam", Source: "rtsp://x/y", Enabled: true, Privacy: true}
	spec.ApplyPTZDefaults()
	if _, err := a.camStore.Create(spec, 1); err != nil {
		t.Fatalf("camStore.Create: %v", err)
	}
	a.cameras = []camera.Camera{spec.Camera()}

	body := `{"name":"Renamed","source":"rtsp://x/y","privacy":true,"enabled":true}`
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, http.MethodPut, "/api/camera/cam1", "admin", "adminpw", body))
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	a.privacyMu.RLock()
	on := a.privacy["cam1"]
	a.privacyMu.RUnlock()
	if !on {
		t.Error("manual privacy was cleared by a camera update")
	}
}

// A malformed camera URL is refused at the API with a message that names the
// field but never echoes the value — it carries the camera's credentials, and
// url.Parse's own error would quote it whole.
func TestCameraSpecRejectsBadURLs(t *testing.T) {
	const secret = "s3cr3t"
	for _, tc := range []struct {
		name  string
		req   createCameraReq
		field string
	}{
		{"space in source", createCameraReq{Name: "c", Source: "rtsp://u:" + secret + "@cam host/x"}, "source"},
		{"http source", createCameraReq{Name: "c", Source: "http://u:" + secret + "@cam/x"}, "source"},
		{"no host", createCameraReq{Name: "c", Source: "rtsp:///x"}, "source"},
		{"bad backchannel", createCameraReq{Name: "c", Source: "rtsp://cam/x", Backchannel: "rtsp://u:" + secret + "@\x00"}, "backchannel"},
		{"bad thingino", createCameraReq{Name: "c", Source: "rtsp://cam/x", ThinginoURL: "http://cam host/?token=" + secret}, "thingino_url"},
		{"ftp snapshot", createCameraReq{Name: "c", Source: "rtsp://cam/x", SnapshotURL: "ftp://cam/snap.jpg"}, "snapshot_url"},
	} {
		_, msg := tc.req.spec()
		if !strings.HasPrefix(msg, tc.field+" ") {
			t.Errorf("%s: msg = %q, want a %s error", tc.name, msg, tc.field)
		}
		if strings.Contains(msg, secret) {
			t.Errorf("%s: error message leaks the credential: %q", tc.name, msg)
		}
	}
	if _, msg := (createCameraReq{Name: "c", Source: "rtsps://u:p@cam:322/x", ThinginoURL: "https://cam"}).spec(); msg != "" {
		t.Errorf("valid URLs rejected: %s", msg)
	}
}
