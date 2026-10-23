package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"eneverre/internal/auth"
	"eneverre/internal/camera"
)

func aclTestApp(t *testing.T) *App {
	t.Helper()
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", auth.RoleAdmin)
	insertUser(t, a.db, "viewer", "pw", auth.RoleViewer)
	insertUser(t, a.db, "monitor", "pw", auth.RoleMonitor)
	a.cameras = []camera.Camera{
		{ID: "front", Name: "Front", Enabled: true},
		{ID: "garden", Name: "Garden", Enabled: true},
	}
	return a
}

func putCameraACL(t *testing.T, a *App, username, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodPut, "/api/users/"+username+"/cameras", "admin", "pw", body))
	return w
}

func TestCameraACLFiltersListAndStatus(t *testing.T) {
	a := aclTestApp(t)
	w := putCameraACL(t, a, "viewer", `{"restricted":true,"camera_ids":["front"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put ACL = %d %s", w.Code, w.Body.String())
	}

	// Viewer's camera list is narrowed to front.
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/cameras", "viewer", "pw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("viewer cameras = %d %s", w.Code, w.Body.String())
	}
	var cams []camera.Camera
	if err := json.Unmarshal(w.Body.Bytes(), &cams); err != nil {
		t.Fatal(err)
	}
	if len(cams) != 1 || cams[0].ID != "front" {
		t.Errorf("viewer cameras = %+v, want only front", cams)
	}

	// Admin bypasses the ACL.
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/cameras", "admin", "pw", ""))
	if err := json.Unmarshal(w.Body.Bytes(), &cams); err != nil {
		t.Fatal(err)
	}
	if len(cams) != 2 {
		t.Errorf("admin cameras = %+v, want all", cams)
	}

	// Monitor's status snapshot carries only the allowed camera.
	w = putCameraACL(t, a, "monitor", `{"restricted":true,"camera_ids":["garden"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put monitor ACL = %d %s", w.Code, w.Body.String())
	}
	monitorToken, _ := createServiceToken(t, a, `{"name":"zbx","username":"monitor"}`)
	w = serviceTokenRequest(t, a, http.MethodGet, "/api/status", monitorToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("monitor status = %d %s", w.Code, w.Body.String())
	}
	var status struct {
		Cameras []struct {
			ID string `json:"id"`
		} `json:"cameras"`
		Totals struct {
			Cameras int `json:"cameras"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Cameras) != 1 || status.Cameras[0].ID != "garden" {
		t.Errorf("monitor status cameras = %+v, want only garden", status.Cameras)
	}
	if status.Totals.Cameras != 1 {
		t.Errorf("monitor status total = %d, want 1", status.Totals.Cameras)
	}
}

func TestCameraACLBlocksSingleCameraEndpoints(t *testing.T) {
	a := aclTestApp(t)
	if w := putCameraACL(t, a, "viewer", `{"restricted":true,"camera_ids":["front"]}`); w.Code != http.StatusOK {
		t.Fatalf("put ACL = %d %s", w.Code, w.Body.String())
	}

	// Allowed camera: auth/capability/ACL pass; the event list is empty.
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/camera/front/events", "viewer", "pw", ""))
	if w.Code != http.StatusOK {
		t.Errorf("front events = %d %s", w.Code, w.Body.String())
	}
	// Restricted camera: 404, not 403, so its existence is not revealed.
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/camera/garden/events", "viewer", "pw", ""))
	if w.Code != http.StatusNotFound {
		t.Errorf("garden events = %d, want 404", w.Code)
	}
}

func TestCameraACLValidationAndReset(t *testing.T) {
	a := aclTestApp(t)
	if w := putCameraACL(t, a, "viewer", `{"restricted":true,"camera_ids":["ghost"]}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown camera ACL = %d, want 422", w.Code)
	}
	if w := putCameraACL(t, a, "viewer", `{"restricted":true,"camera_ids":["front"]}`); w.Code != http.StatusOK {
		t.Fatalf("put ACL = %d", w.Code)
	}
	if w := putCameraACL(t, a, "viewer", `{"restricted":false}`); w.Code != http.StatusOK {
		t.Fatalf("reset ACL = %d", w.Code)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/users/viewer/cameras", "admin", "pw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("get ACL = %d", w.Code)
	}
	var view cameraACLView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Restricted || len(view.CameraIDs) != 0 {
		t.Errorf("reset ACL view = %+v", view)
	}
}
