package server

import (
	"net/http"
)

// handleLiveInfo reports whether the embedded engine has a live source for the
// camera and, if so, the MSE mime type the browser needs to build a
// MediaSource. Reports {"available": false} when the camera has no live
// broadcaster (its `mse` feature is off). The engine == nil guard only trips
// in tests that construct App without a media engine.
func (a *App) handleLiveInfo(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok {
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
// HTTP for a browser MediaSource. Returns 503 when the camera has no live
// broadcaster (its `mse` feature is off). The engine == nil guard only trips
// in tests that construct App without a media engine.
func (a *App) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		httpError(w, http.StatusNotFound, "Not Found")
		return
	}
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}
	lb := a.engine.Broadcaster(cam.ID)
	if lb == nil {
		httpError(w, http.StatusServiceUnavailable, "no live source")
		return
	}
	// The live MSE feed is an indefinite chunked response; without this the
	// global WriteTimeout would cut it every 30s, forcing the client to
	// reconnect and re-buffer.
	clearWriteDeadline(w, "live")
	lb.HandleStream(w, r)
}
