package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/schedule"
)

// schedIDPattern constrains a schedule id to the same short, URL-safe token as a
// camera id (it appears in the API path and is referenced by cameras.schedule_id).
var schedIDPattern = camIDPattern

// scheduleReq is the JSON body of POST /api/schedules and PUT /api/schedule/{id}.
// Days maps a weekday key ("mon".."sun") to its armed windows, each "HH:MM-HH:MM".
type scheduleReq struct {
	ID   string              `json:"id"`
	Name string              `json:"name"`
	Days map[string][]string `json:"days"`
}

// --- scheduler ------------------------------------------------------------

// startScheduler reconciles every camera's pipeline against its recording
// schedule once, then re-evaluates on each minute boundary. Started from
// SetMediaEngine (the engine must be attached to pause/resume a camera). It runs
// for the process lifetime, like the token/event cleaners.
func (a *App) startScheduler() {
	a.safeReevaluate()
	for {
		now := time.Now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		<-time.After(time.Until(next))
		a.safeReevaluate()
	}
}

// safeReevaluate runs one reevaluation, recovering from any panic so a single
// bad tick can't kill the scheduler goroutine (which would silently stop the
// per-minute ticks until the next camera/schedule mutation). Only the
// long-lived goroutine uses this; the HTTP handlers call reevaluateSchedules
// directly so a panic there surfaces as a 500 via net/http's per-request
// recovery instead of being swallowed.
func (a *App) safeReevaluate() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: reevaluate panicked, skipping this tick", "panic", r)
		}
	}()
	a.reevaluateSchedules()
}

// reevaluateSchedules recomputes the off-hours state of every camera and applies
// the resulting pause. Cheap enough to call on every minute tick and after any
// camera/schedule mutation (few cameras, one indexed schedule query). No-op when
// the engine or schedule store is absent.
func (a *App) reevaluateSchedules() {
	if a.engine == nil || a.schedStore == nil {
		return
	}
	list, err := a.schedStore.List()
	if err != nil {
		slog.Warn("scheduler: list schedules failed", "err", err)
		return
	}
	byID := make(map[string]schedule.Schedule, len(list))
	for _, s := range list {
		byID[s.ID] = s
	}
	now := time.Now()
	for _, cam := range a.listCameras() {
		a.evaluateCamera(cam, byID, now)
	}
}

// evaluateCamera updates one camera's off-hours state (a.schedOff) and, when it
// changed, re-applies the effective engine pause. A camera with no schedule (or
// one whose schedule was deleted) is always armed, so it never gets paused by
// the scheduler.
func (a *App) evaluateCamera(cam camera.Camera, byID map[string]schedule.Schedule, now time.Time) {
	armed := true
	if cam.ScheduleID != "" {
		if s, ok := byID[cam.ScheduleID]; ok {
			armed = s.Active(now)
		}
	}
	off := !armed
	a.privacyMu.Lock()
	prev := a.schedOff[cam.ID]
	if off {
		a.schedOff[cam.ID] = true
	} else {
		delete(a.schedOff, cam.ID)
	}
	a.privacyMu.Unlock()
	if off != prev {
		a.applyPause(cam.ID)
		slog.Info("recording schedule transition", "camera", cam.ID, "schedule", cam.ScheduleID, "recording", armed)
	}
}

// applyPause re-applies the effective engine pause for a camera: privacy OR
// off-hours. Taken under the per-camera privacy op lock so it serializes with a
// concurrent manual privacy toggle (handlePrivacy), and reads both flags under
// privacyMu so it always applies the freshest combination.
func (a *App) applyPause(id string) {
	if a.engine == nil {
		return
	}
	mu := a.privacyOp(id)
	mu.Lock()
	defer mu.Unlock()
	a.privacyMu.RLock()
	manual := a.privacy[id]
	off := a.schedOff[id]
	a.privacyMu.RUnlock()
	a.engine.SetPrivacy(id, manual || off)
}

// scheduleExists reports whether a schedule id is usable as a camera's
// schedule_id: an empty id (record 24/7) is always valid; otherwise the id must
// resolve to a stored schedule.
func (a *App) scheduleExists(id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	if a.schedStore == nil {
		return false, nil
	}
	_, ok, err := a.schedStore.Get(id)
	return ok, err
}

// --- CRUD endpoints -------------------------------------------------------

// handleListSchedules returns every recording schedule (any logged-in user, so
// the UI can resolve a camera's program name).
func (a *App) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	if a.schedStore == nil {
		writeJSON(w, http.StatusOK, []schedule.Schedule{})
		return
	}
	list, err := a.schedStore.List()
	if err != nil {
		slog.Error("list schedules failed", "err", err)
		httpError(w, http.StatusInternalServerError, "could not read schedules")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleGetSchedule returns one schedule by id.
func (a *App) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	if a.schedStore == nil {
		httpError(w, http.StatusNotFound, "schedule not found")
		return
	}
	s, ok, err := a.schedStore.Get(r.PathValue("id"))
	if err != nil {
		slog.Error("get schedule failed", "id", r.PathValue("id"), "err", err)
		httpError(w, http.StatusInternalServerError, "could not read schedule")
		return
	}
	if !ok {
		httpError(w, http.StatusNotFound, "schedule not found")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// handleCreateSchedule creates a schedule (admin only).
func (a *App) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	if a.schedStore == nil {
		httpError(w, http.StatusServiceUnavailable, "schedules unavailable")
		return
	}
	var req scheduleReq
	if !decodeJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	if !schedIDPattern.MatchString(id) {
		httpError(w, http.StatusUnprocessableEntity, "id must be 1–64 chars of letters, digits, '-' or '_', starting with a letter or digit")
		return
	}
	if msg := schedule.Validate(req.Days); msg != "" {
		httpError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	s, err := a.schedStore.Create(schedule.Schedule{ID: id, Name: strings.TrimSpace(req.Name), Days: req.Days}, time.Now().Unix())
	if errors.Is(err, schedule.ErrExists) {
		httpError(w, http.StatusConflict, "a schedule with id '"+id+"' already exists")
		return
	}
	if err != nil {
		slog.Error("create schedule failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "could not save schedule")
		return
	}
	slog.Info("schedule created", "id", s.ID, "name", s.Name)
	writeJSON(w, http.StatusCreated, s)
}

// handleUpdateSchedule edits a schedule (admin only). The id is fixed by the URL.
// After a successful update the scheduler re-evaluates immediately so a changed
// window takes effect without waiting for the next minute tick.
func (a *App) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	if a.schedStore == nil {
		httpError(w, http.StatusServiceUnavailable, "schedules unavailable")
		return
	}
	id := r.PathValue("id")
	var req scheduleReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := schedule.Validate(req.Days); msg != "" {
		httpError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	switch err := a.schedStore.Update(schedule.Schedule{ID: id, Name: strings.TrimSpace(req.Name), Days: req.Days}); {
	case errors.Is(err, schedule.ErrNotFound):
		httpError(w, http.StatusNotFound, "schedule not found")
		return
	case err != nil:
		slog.Error("update schedule failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "could not update schedule")
		return
	}
	a.reevaluateSchedules()
	s, _, _ := a.schedStore.Get(id)
	slog.Info("schedule updated", "id", id, "name", s.Name)
	writeJSON(w, http.StatusOK, s)
}

// handleDeleteSchedule removes a schedule (admin only). It refuses (409) when any
// camera still references it, so a live camera is never stranded pointing at a
// missing program — the operator reassigns those cameras first.
func (a *App) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	if a.schedStore == nil {
		httpError(w, http.StatusServiceUnavailable, "schedules unavailable")
		return
	}
	id := r.PathValue("id")
	if users := a.camerasUsingSchedule(id); len(users) > 0 {
		httpError(w, http.StatusConflict, "schedule in use by "+strings.Join(users, ", ")+"; reassign those cameras first")
		return
	}
	switch err := a.schedStore.Delete(id); {
	case errors.Is(err, schedule.ErrNotFound):
		httpError(w, http.StatusNotFound, "schedule not found")
		return
	case err != nil:
		slog.Error("delete schedule failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "could not delete schedule")
		return
	}
	slog.Info("schedule deleted", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Schedule deleted"})
}

// camerasUsingSchedule returns the ids of cameras that reference the given
// schedule. Reads the store (source of truth) rather than the in-memory snapshot
// so it can't miss a just-created camera.
func (a *App) camerasUsingSchedule(id string) []string {
	specs, err := a.camStore.ListSpecs()
	if err != nil {
		slog.Warn("schedule usage check failed", "id", id, "err", err)
		return nil
	}
	var users []string
	for _, s := range specs {
		if s.ScheduleID == id {
			users = append(users, s.ID)
		}
	}
	return users
}
