package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/events"
	"eneverre/internal/streamauth"
)

// deleteTestApp stores one camera ("cam1") with a motion event, plus an admin
// and a regular user.
func deleteTestApp(t *testing.T) (*App, int64) {
	t.Helper()
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")
	insertUser(t, a.db, "bob", "bobpw", "user")
	creds, err := streamauth.NewStore(a.db)
	if err != nil {
		t.Fatal(err)
	}
	a.creds = creds
	a.camStore = camera.NewStore(a.db)
	a.privacy = map[string]bool{}
	a.schedOff = map[string]bool{}
	a.talkCodecs = map[string][]string{}
	a.ptzPos = map[string]ptzPos{}
	a.heartbeats = map[string]heartbeatInfo{}
	spec := camera.Spec{ID: "cam1", Name: "Cam", Source: "rtsp://x/y", Enabled: true}
	spec.ApplyPTZDefaults()
	if _, err := a.camStore.Create(spec, 1); err != nil {
		t.Fatal(err)
	}
	a.cameras = []camera.Camera{spec.Camera()}
	ev, err := events.RecordMotion(a.db, "cam1", time.Now().Unix(), 5, 5, nil, "motion", "test")
	if err != nil {
		t.Fatal(err)
	}
	return a, ev.ID
}

func TestDeleteEventIsAdminOnly(t *testing.T) {
	a, id := deleteTestApp(t)
	path := fmt.Sprintf("/api/camera/cam1/events/%d", id)

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, http.MethodDelete, path, "bob", "bobpw", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin delete = %d, want 403", w.Code)
	}
	if _, found, _ := events.Get(a.db, "cam1", id); !found {
		t.Fatal("event was deleted by a non-admin")
	}

	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, http.MethodDelete, path, "admin", "adminpw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("admin delete = %d, want 200", w.Code)
	}
}

// Deleting a camera deletes its motion events (and, with an engine, its
// recordings), so a camera later created with the same id starts clean.
func TestDeleteCameraRemovesEvents(t *testing.T) {
	a, _ := deleteTestApp(t)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, http.MethodDelete, "/api/camera/cam1", "admin", "adminpw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("delete camera = %d: %s", w.Code, w.Body.String())
	}
	if list, total, _ := events.List(a.db, "cam1", nil, nil, 10, 0); total != 0 || len(list) != 0 {
		t.Errorf("cam1 still has %d events after deletion", total)
	}
}
