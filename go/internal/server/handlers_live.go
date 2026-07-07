package server

import (
	"net/http"

	"eneverre/internal/camera"
)

// handleLiveInfo reports whether the embedded engine has a live source for the
// camera and, if so, the MSE mime type the browser needs to build a
// MediaSource. Returns 404 when the engine is not active.
func (a *App) handleLiveInfo(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}
	lb := a.engine.Broadcaster(cam.ID)
	if lb == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	lb.HandleInfo(w, r)
}

// handleLiveStream streams the camera's live fMP4 (init + parts) as chunked
// HTTP for a browser MediaSource. Returns 404 when the engine is not active.
func (a *App) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}
	lb := a.engine.Broadcaster(cam.ID)
	if lb == nil {
		httpError(w, http.StatusServiceUnavailable, "no live source")
		return
	}
	lb.HandleStream(w, r)
}
