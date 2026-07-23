package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"eneverre/internal/camera"
	"eneverre/internal/schedule"
)

// withSchedulesApp builds an App with a real DB plus the camera and schedule
// stores wired, and a single admin user, so the schedule endpoints can be
// exercised end to end.
func withSchedulesApp(t *testing.T) *App {
	t.Helper()
	a := withUsersApp(t)
	a.schedStore = schedule.NewStore(a.db)
	a.camStore = camera.NewStore(a.db)
	a.schedOff = map[string]bool{}
	a.privacy = map[string]bool{}
	a.privacyOps = map[string]*sync.Mutex{}
	insertUser(t, a.db, "admin", "adminpw", "admin")
	insertUser(t, a.db, "bob", "bobpw", "user")
	return a
}

func TestScheduleCRUD(t *testing.T) {
	a := withSchedulesApp(t)

	// Create.
	body := `{"id":"business","name":"Business hours","days":{"mon":["14:00-18:00","08:00-12:00"]}}`
	w := httptest.NewRecorder()
	a.handleCreateSchedule(w, adminRequest(t, http.MethodPost, "/api/schedules", "admin", "adminpw", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", w.Code, w.Body.String())
	}
	var created schedule.Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Windows must come back normalized (sorted by start).
	if got := created.Days["mon"]; len(got) != 2 || got[0] != "08:00-12:00" || got[1] != "14:00-18:00" {
		t.Errorf("windows not normalized: %v", got)
	}

	// List sees it.
	w = httptest.NewRecorder()
	a.handleListSchedules(w, adminRequest(t, http.MethodGet, "/api/schedules", "admin", "adminpw", ""))
	var list []schedule.Schedule
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != "business" {
		t.Fatalf("list = %+v, want one 'business'", list)
	}

	// Get one by id.
	w = httptest.NewRecorder()
	r := adminRequest(t, http.MethodGet, "/api/schedule/business", "admin", "adminpw", "")
	r.SetPathValue("id", "business")
	a.handleGetSchedule(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get: status = %d, body = %s", w.Code, w.Body.String())
	}
	var got schedule.Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.ID != "business" {
		t.Fatalf("get body = %s (err %v)", w.Body.String(), err)
	}

	// Get a missing schedule is 404.
	w = httptest.NewRecorder()
	r = adminRequest(t, http.MethodGet, "/api/schedule/ghost", "admin", "adminpw", "")
	r.SetPathValue("id", "ghost")
	a.handleGetSchedule(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("get missing: status = %d, want 404", w.Code)
	}

	// Update.
	w = httptest.NewRecorder()
	r = adminRequest(t, http.MethodPut, "/api/schedule/business", "admin", "adminpw", `{"name":"New name","days":{"sat":["09:00-13:00"]}}`)
	r.SetPathValue("id", "business")
	a.handleUpdateSchedule(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d, body = %s", w.Code, w.Body.String())
	}

	// Update with a malformed window is 422 (validation runs before the store).
	w = httptest.NewRecorder()
	r = adminRequest(t, http.MethodPut, "/api/schedule/business", "admin", "adminpw", `{"name":"x","days":{"mon":["8-18"]}}`)
	r.SetPathValue("id", "business")
	a.handleUpdateSchedule(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("update bad window: status = %d, want 422", w.Code)
	}

	// Update a missing schedule is 404.
	w = httptest.NewRecorder()
	r = adminRequest(t, http.MethodPut, "/api/schedule/ghost", "admin", "adminpw", `{"name":"x","days":{}}`)
	r.SetPathValue("id", "ghost")
	a.handleUpdateSchedule(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("update missing: status = %d, want 404", w.Code)
	}
}

func TestScheduleValidation(t *testing.T) {
	a := withSchedulesApp(t)
	cases := []struct{ name, body string }{
		{"bad id", `{"id":"has space","days":{}}`},
		{"unknown day", `{"id":"s","days":{"funday":["08:00-18:00"]}}`},
		{"bad window", `{"id":"s","days":{"mon":["8-18"]}}`},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		a.handleCreateSchedule(w, adminRequest(t, http.MethodPost, "/api/schedules", "admin", "adminpw", c.body))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422 (body %s)", c.name, w.Code, w.Body.String())
		}
	}

	// Duplicate id is 409.
	dup := `{"id":"dup","days":{}}`
	w := httptest.NewRecorder()
	a.handleCreateSchedule(w, adminRequest(t, http.MethodPost, "/api/schedules", "admin", "adminpw", dup))
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: %d", w.Code)
	}
	w = httptest.NewRecorder()
	a.handleCreateSchedule(w, adminRequest(t, http.MethodPost, "/api/schedules", "admin", "adminpw", dup))
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate create: status = %d, want 409", w.Code)
	}
}

func TestScheduleDeleteInUse(t *testing.T) {
	a := withSchedulesApp(t)
	if _, err := a.schedStore.Create(schedule.Schedule{ID: "nightly", Name: "Nightly"}, 0); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	// A camera referencing the schedule blocks deletion.
	spec := camera.Spec{ID: "cam1", Source: "rtsp://x/y", ScheduleID: "nightly"}
	spec.ApplyPTZDefaults()
	if _, err := a.camStore.Create(spec, 0); err != nil {
		t.Fatalf("seed camera: %v", err)
	}

	w := httptest.NewRecorder()
	r := adminRequest(t, http.MethodDelete, "/api/schedule/nightly", "admin", "adminpw", "")
	r.SetPathValue("id", "nightly")
	a.handleDeleteSchedule(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete in use: status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}

	// Remove the reference, then deletion succeeds.
	if err := a.camStore.Delete("cam1"); err != nil {
		t.Fatalf("delete camera: %v", err)
	}
	w = httptest.NewRecorder()
	r = adminRequest(t, http.MethodDelete, "/api/schedule/nightly", "admin", "adminpw", "")
	r.SetPathValue("id", "nightly")
	a.handleDeleteSchedule(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("delete not in use: status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	// Deleting it again (now gone) is 404.
	w = httptest.NewRecorder()
	r = adminRequest(t, http.MethodDelete, "/api/schedule/nightly", "admin", "adminpw", "")
	r.SetPathValue("id", "nightly")
	a.handleDeleteSchedule(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete missing: status = %d, want 404", w.Code)
	}
}

func TestScheduleRequiresAdmin(t *testing.T) {
	a := withSchedulesApp(t)
	w := httptest.NewRecorder()
	a.handleCreateSchedule(w, adminRequest(t, http.MethodPost, "/api/schedules", "bob", "bobpw", `{"id":"s","days":{}}`))
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin create: status = %d, want 403", w.Code)
	}
}
