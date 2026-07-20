package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"eneverre/internal/camera"
)

// ptzTestApp builds a minimal App wired to a real (temp-file) SQLite DB and
// a one-camera in-memory set, just enough to drive the PTZ handlers in
// isolation. The camera's ThinginoURL is patched to point at the supplied
// httptest server (use ptzTestServer for canned motor responses), so a
// successful move reaches the handler and the response is what's served.
func ptzTestApp(t *testing.T, thinginoSrv *httptest.Server, cam camera.Camera) *App {
	t.Helper()
	a := withUsersApp(t)
	insertUser(t, a.db, "alice", "alicepw", "user")
	if cam.ThinginoURL == "" {
		cam.ThinginoURL = thinginoSrv.URL
	}
	a.cameras = []camera.Camera{cam}
	a.privacy = map[string]bool{}
	a.ptzPos = map[string]ptzPos{}
	return a
}

// doPTZ dispatches a request through the App's real handler — the mux sets
// the {cam_id} path value the handlers read. Returning the recorder lets
// each test assert on the response.
func doPTZ(t *testing.T, a *App, method, target, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, method, target, user, pass, ""))
	return w
}

// ptzTestServer returns an httptest server that captures the last query
// string it saw and the (x, y) it parsed, and replies with the supplied
// response body. The test can then assert on the request and parse the
// body. Defaults to a json-motor.cgi?d=g-style echo.
func ptzTestServer(t *testing.T, response string) (*httptest.Server, *ptzServerCapture) {
	t.Helper()
	cap := &ptzServerCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.d = r.URL.Query().Get("d")
		cap.x = r.URL.Query().Get("x")
		cap.y = r.URL.Query().Get("y")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type ptzServerCapture struct {
	path, d, x, y string
}

// ptzCamera returns a PTZ-capable camera with the calibration defaults the
// test expects (2130/360 pan, 1600/180 tilt, 113° FOV). The caller can tweak
// the fields before passing the result to ptzTestApp.
func ptzCamera() camera.Camera {
	return camera.Camera{
		ID:             "cam1",
		Name:           "PTZ",
		Capabilities:   camera.Capabilities{PTZ: true, Privacy: true},
		ThinginoURL:    "", // filled in by ptzTestApp from the test server
		ThinginoAPIKey: "secret",
		PanSteps:       2130, PanDegrees: 360,
		TiltSteps: 1600, TiltDegrees: 180,
		Width: 16, Height: 9,
	}
}

// TestHandlePTZMoveDegrees pins the public pan/tilt contract: the request
// reaches json-motor.cgi?d=g with x/y in firmware steps, derived from the
// camera's calibration and the requested degrees, and the HTTP response is
// the camera's new position in degrees — not the vendor's raw JSON.
func TestHandlePTZMoveDegrees(t *testing.T) {
	// Echo back a body with xpos/ypos so the position cache gets refreshed
	// for the next /ptz/position test.
	srv, cap := ptzTestServer(t, `{"code":200,"result":"success","message":{"xpos":"200","ypos":"100","speed":"900","invert":"0"}}`)
	a := ptzTestApp(t, srv, ptzCamera())

	w := doPTZ(t, a, http.MethodPost, "/api/camera/cam1/ptz/move?pan=10&tilt=5", "alice", "alicepw")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if cap.d != "g" {
		t.Errorf("d = %q; want g (relative move)", cap.d)
	}
	// 10° pan on 2130/360 -> 2130*10/360 ≈ 59.17
	// 5° tilt on 1600/180 -> 1600*5/180 ≈ 44.44
	gotX, _ := strconv.ParseFloat(cap.x, 64)
	gotY, _ := strconv.ParseFloat(cap.y, 64)
	if gotX < 59.0 || gotX > 59.3 {
		t.Errorf("x = %q (parsed %v); want ≈59.17", cap.x, gotX)
	}
	if gotY < 44.3 || gotY > 44.6 {
		t.Errorf("y = %q (parsed %v); want ≈44.44", cap.y, gotY)
	}

	// The response must be {"pan","tilt"} in degrees, matching /ptz/position —
	// not the firmware's raw {code, result, message: {xpos, ypos, speed, invert}}.
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, leaked := range []string{"code", "result", "message", "xpos", "ypos", "speed", "invert"} {
		if _, present := got[leaked]; present {
			t.Errorf("response leaked vendor field %q: %s", leaked, w.Body.String())
		}
	}
	// 200 steps / 2130 = 0.0939 -> 33.80° pan; 100 steps / 1600 = 0.0625 -> 11.25° tilt
	pan, _ := got["pan"].(float64)
	tilt, _ := got["tilt"].(float64)
	if pan < 33.5 || pan > 34.0 {
		t.Errorf("pan = %v; want ≈33.80", pan)
	}
	if tilt < 11.0 || tilt > 11.5 {
		t.Errorf("tilt = %v; want ≈11.25", tilt)
	}
}

// TestHandlePTZMoveNoEcho: when the firmware doesn't echo a position (some
// modes/firmwares only do on certain d= values) and the cache was cold
// before this call, the move still succeeds (the firmware call worked) but
// the response has no position to report — {} rather than an error, since
// unlike GET /ptz/position there is no cold-cache failure to signal here.
func TestHandlePTZMoveNoEcho(t *testing.T) {
	srv, _ := ptzTestServer(t, `{"code":200,"result":"success"}`)
	a := ptzTestApp(t, srv, ptzCamera())

	w := doPTZ(t, a, http.MethodPost, "/api/camera/cam1/ptz/move?pan=10&tilt=5", "alice", "alicepw")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("body = %v; want {} (no position known)", got)
	}
}

// TestHandlePTZMoveClamps pins the server-side range clamp: a pan larger
// than half the mechanical range must land at the end of the axis, not at
// the firmware-native NaN or beyond the gimbal's reach.
func TestHandlePTZMoveClamps(t *testing.T) {
	srv, cap := ptzTestServer(t, `{"code":200}`)
	a := ptzTestApp(t, srv, ptzCamera())

	w := doPTZ(t, a, http.MethodPost, "/api/camera/cam1/ptz/move?pan=10000&tilt=0", "alice", "alicepw")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	gotX, _ := strconv.ParseFloat(cap.x, 64)
	if gotX != 2130.0/2 {
		t.Errorf("x = %q (parsed %v); want %v (clamp at half pan range)", cap.x, gotX, 2130.0/2)
	}
}

// TestHandlePTZPosition pins the position-read contract: a fresh app with no
// cache returns 503, a successful move refreshes the cache from the
// response, and the position endpoint renders the cached value in degrees
// using the camera's calibration.
func TestHandlePTZPosition(t *testing.T) {
	srv, _ := ptzTestServer(t, `{"code":200,"message":{"xpos":"200","ypos":"100"}}`)
	a := ptzTestApp(t, srv, ptzCamera())

	t.Run("cold cache returns 503", func(t *testing.T) {
		w := doPTZ(t, a, http.MethodGet, "/api/camera/cam1/ptz/position", "alice", "alicepw")
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
	})

	t.Run("move updates the cache and position returns degrees", func(t *testing.T) {
		mw := doPTZ(t, a, http.MethodPost, "/api/camera/cam1/ptz/move?pan=10&tilt=5", "alice", "alicepw")
		if mw.Code != http.StatusOK {
			t.Fatalf("move status = %d, want 200", mw.Code)
		}

		w := doPTZ(t, a, http.MethodGet, "/api/camera/cam1/ptz/position", "alice", "alicepw")
		if w.Code != http.StatusOK {
			t.Fatalf("position status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var got struct {
			Pan, Tilt float64
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// 200 steps / 2130 = 0.0939 -> 33.80° pan
		// 100 steps / 1600 = 0.0625 -> 11.25° tilt
		if got.Pan < 33.5 || got.Pan > 34.0 {
			t.Errorf("pan = %v; want ≈33.80", got.Pan)
		}
		if got.Tilt < 11.0 || got.Tilt > 11.5 {
			t.Errorf("tilt = %v; want ≈11.25", got.Tilt)
		}
	})
}

// TestHandlePTZPositionRequiresCapability: a camera without the PTZ
// capability (or thingino credentials) must not expose /ptz/position — the
// same gate /ptz/move uses, so a misconfigured camera can't leak motor
// status to a logged-in user.
func TestHandlePTZPositionRequiresCapability(t *testing.T) {
	srv, _ := ptzTestServer(t, `{"code":200}`)
	cam := ptzCamera()
	cam.Capabilities.PTZ = false
	a := ptzTestApp(t, srv, cam)

	w := doPTZ(t, a, http.MethodGet, "/api/camera/cam1/ptz/position", "alice", "alicepw")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandlePTZRecalibrateReportsPosition pins the recalibrate handler's
// contract: d=r itself doesn't echo a position (this fake firmware replies
// with none, like the real one), so the handler must read it back with a
// separate d=j query and report that — not an empty body — once recalibration
// succeeds.
func TestHandlePTZRecalibrateReportsPosition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		switch r.URL.Query().Get("d") {
		case "r":
			_, _ = w.Write([]byte(`{"code":200,"result":"success"}`))
		case "j":
			_, _ = w.Write([]byte(`{"code":200,"message":{"xpos":"1065","ypos":"800"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	a := ptzTestApp(t, srv, ptzCamera())

	w := doPTZ(t, a, http.MethodPost, "/api/camera/cam1/ptz/recalibrate", "alice", "alicepw")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]float64
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 1065 steps / 2130 * 360 = 180° pan; 800 steps / 1600 * 180 = 90° tilt.
	if got["pan"] != 180 || got["tilt"] != 90 {
		t.Errorf("pan/tilt = %v; want {pan:180, tilt:90}", got)
	}

	// The read-back position must also refresh the cache: /ptz/position
	// should now answer without needing another move.
	pw := doPTZ(t, a, http.MethodGet, "/api/camera/cam1/ptz/position", "alice", "alicepw")
	if pw.Code != http.StatusOK {
		t.Fatalf("position status = %d, want 200; body=%s", pw.Code, pw.Body.String())
	}
}

// TestHandlePTZHomeConvertsDegrees pins the home handler's contract: the
// stored HomeX/HomeY are in degrees (the public unit), the server converts
// to firmware x/y at call time using the camera's calibration, and the
// request reaches json-motor.cgi?d=x (absolute) with the converted
// coordinates. Same conversion path that the privacy move uses.
func TestHandlePTZHomeConvertsDegrees(t *testing.T) {
	srv, cap := ptzTestServer(t, `{"code":200,"message":{"xpos":"1065","ypos":"800"}}`)
	cam := ptzCamera()
	cam.HomeX = 180 // degrees — half revolution
	cam.HomeY = 90  // degrees — looking down
	a := ptzTestApp(t, srv, cam)

	w := doPTZ(t, a, http.MethodPost, "/api/camera/cam1/ptz/home", "alice", "alicepw")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if cap.d != "x" {
		t.Errorf("d = %q; want x (absolute move)", cap.d)
	}
	// 180° pan / 90° tilt on the default calibration land at 1065 / 800
	// firmware steps — the same physical position the old step-based
	// config would have produced with home_x=1065 / home_y=800.
	gotX, _ := strconv.ParseFloat(cap.x, 64)
	gotY, _ := strconv.ParseFloat(cap.y, 64)
	if gotX != 1065 {
		t.Errorf("x = %q (parsed %v); want 1065 (180° on default calibration)", cap.x, gotX)
	}
	if gotY != 800 {
		t.Errorf("y = %q (parsed %v); want 800 (90° on default calibration)", cap.y, gotY)
	}

	// Same public shape as /ptz/move: {"pan","tilt"} in degrees, not the
	// firmware's raw response.
	var got map[string]float64
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["pan"] != 180 || got["tilt"] != 90 {
		t.Errorf("pan/tilt = %v; want {pan:180, tilt:90}", got)
	}
}
