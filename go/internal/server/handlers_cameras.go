package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/media"
)

// camIDPattern constrains a camera id to a short, filesystem- and URL-safe
// token. The id is used as a recording subdirectory (the `%path` segment) and
// throughout the API paths, so path separators, dots, and whitespace are
// rejected; the first character must be alphanumeric.
var camIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// createCameraReq is the JSON body of POST /api/cameras. The three-state
// booleans (record/mse/relay/privacy/playback) and the numeric fields are
// pointers so an omitted key falls back to the same defaults the INI loader
// applies, rather than to Go's zero value.
type createCameraReq struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Comment     string `json:"comment"`
	Location    string `json:"location"`
	Source      string `json:"source"`
	Backchannel string `json:"backchannel"`
	SnapshotURL string `json:"snapshot_url"`
	Transport   string `json:"transport"`

	Record   *bool `json:"record"`
	MSE      *bool `json:"mse"`
	Relay    *bool `json:"relay"`
	Privacy  *bool `json:"privacy"`
	Playback *bool `json:"playback"`
	Width    *int  `json:"width"`
	Height   *int  `json:"height"`

	ThinginoURL    string   `json:"thingino_url"`
	ThinginoAPIKey string   `json:"thingino_api_key"`
	PTZ            *bool    `json:"ptz"`
	HomeX          *float64 `json:"home_x"`
	HomeY          *float64 `json:"home_y"`
	PrivacyX       *float64 `json:"privacy_x"`
	PrivacyY       *float64 `json:"privacy_y"`
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func floatOr(p *float64, def float64) float64 {
	if p != nil {
		return *p
	}
	return def
}

// spec validates the request and converts it to a camera.Spec. It returns a
// non-empty message describing the first validation failure (for a 422), in
// which case the returned Spec is unusable.
func (req createCameraReq) spec() (camera.Spec, string) {
	id := strings.TrimSpace(req.ID)
	if !camIDPattern.MatchString(id) {
		return camera.Spec{}, "id must be 1–64 chars of letters, digits, '-' or '_', starting with a letter or digit"
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return camera.Spec{}, "source (the camera's RTSP URL) is required"
	}
	transport := strings.ToLower(strings.TrimSpace(req.Transport))
	switch transport {
	case "", "auto", "tcp", "udp":
	default:
		return camera.Spec{}, "transport must be one of auto, tcp, udp"
	}
	return camera.Spec{
		ID:             id,
		Name:           strings.TrimSpace(req.Name),
		Comment:        strings.TrimSpace(req.Comment),
		Location:       strings.TrimSpace(req.Location),
		Source:         source,
		Backchannel:    strings.TrimSpace(req.Backchannel),
		SnapshotURL:    strings.TrimSpace(req.SnapshotURL),
		Transport:      transport,
		Record:         boolOr(req.Record, true),
		MSE:            boolOr(req.MSE, true),
		Relay:          boolOr(req.Relay, true),
		Privacy:        boolOr(req.Privacy, true),
		Playback:       boolOr(req.Playback, false),
		Width:          intOr(req.Width, 16),
		Height:         intOr(req.Height, 9),
		ThinginoURL:    strings.TrimSpace(req.ThinginoURL),
		ThinginoAPIKey: strings.TrimSpace(req.ThinginoAPIKey),
		PTZ:            boolOr(req.PTZ, false),
		HomeX:          floatOr(req.HomeX, -1),
		HomeY:          floatOr(req.HomeY, -1),
		PrivacyX:       floatOr(req.PrivacyX, -1),
		PrivacyY:       floatOr(req.PrivacyY, -1),
	}, ""
}

// handleCreateCamera creates a camera (admin only): it validates the body,
// persists the row (the DB is the source of truth), brings the camera online in
// the media engine without a restart, adds it to the in-memory set, and kicks
// off the background privacy/talk probes. It returns the new camera in the same
// public shape as GET /api/cameras.
func (a *App) handleCreateCamera(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req createCameraReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	s, msg := req.spec()
	if msg != "" {
		httpError(w, http.StatusUnprocessableEntity, msg)
		return
	}

	cam, err := a.camStore.Create(s, time.Now().Unix())
	if errors.Is(err, camera.ErrExists) {
		httpError(w, http.StatusConflict, "a camera with id '"+s.ID+"' already exists")
		return
	}
	if err != nil {
		slog.Error("create camera failed", "id", s.ID, "err", err)
		httpError(w, http.StatusInternalServerError, "could not save camera")
		return
	}

	// Bring it online without a restart, then make it visible to the API.
	if a.engine != nil {
		a.engine.AddCamera(cam)
	}
	a.addCamera(cam)
	a.seedPrivacyFor(cam)
	a.seedTalkCodecsFor(cam)

	slog.Info("camera created", "id", cam.ID, "name", cam.Name)
	writeJSON(w, http.StatusCreated, a.publicCamera(cam, r.Host))
}

// handleGetCameraConfig returns the full stored config of one camera (admin
// only), INCLUDING the source/backchannel/thingino credentials, so the edit
// form can prefill them. This is intentionally separate from GET /api/cameras,
// whose public model strips those fields (json:"-") and must never leak them —
// here the caller is an authenticated admin editing a camera they configured.
func (a *App) handleGetCameraConfig(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	s, ok, err := a.camStore.GetSpec(r.PathValue("cam_id"))
	if err != nil {
		slog.Error("get camera config failed", "id", r.PathValue("cam_id"), "err", err)
		httpError(w, http.StatusInternalServerError, "could not read camera")
		return
	}
	if !ok {
		httpError(w, http.StatusNotFound, "camera not found")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// handleUpdateCamera edits an existing camera (admin only): it validates the
// body, overwrites the DB row, and reconfigures the media engine live by tearing
// down the old pipeline and bringing the new config up (RemoveCamera +
// AddCamera) — so source/transport/sink changes take effect without a restart.
// The id is fixed by the URL and cannot be changed (it is the recording path).
func (a *App) handleUpdateCamera(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	id := r.PathValue("cam_id")
	var req createCameraReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.ID = id // the id is set by the path; any body id is ignored
	s, msg := req.spec()
	if msg != "" {
		httpError(w, http.StatusUnprocessableEntity, msg)
		return
	}

	switch err := a.camStore.Update(s); {
	case errors.Is(err, camera.ErrNotFound):
		httpError(w, http.StatusNotFound, "camera not found")
		return
	case err != nil:
		slog.Error("update camera failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "could not update camera")
		return
	}

	cam := s.Camera()
	// Apply live: the engine has no in-place update, so tear the old pipeline
	// down (finalizing its segment) and start fresh with the new config.
	if a.engine != nil {
		a.engine.RemoveCamera(id)
		a.engine.AddCamera(cam)
	}
	a.updateCamera(cam)
	// Reset runtime state and re-probe: the thingino/backchannel config may have
	// changed, so the old privacy/talk-codec state no longer applies.
	a.privacyMu.Lock()
	delete(a.privacy, id)
	a.privacyMu.Unlock()
	a.talkCodecsMu.Lock()
	delete(a.talkCodecs, id)
	a.talkCodecsMu.Unlock()
	a.seedPrivacyFor(cam)
	a.seedTalkCodecsFor(cam)

	slog.Info("camera updated", "id", id, "name", cam.Name)
	writeJSON(w, http.StatusOK, a.publicCamera(cam, r.Host))
}

// handleDeleteCamera removes a camera (admin only): it deletes the DB row
// (source of truth) first, then detaches the camera from the media engine and
// the in-memory set and clears its runtime state. Recorded segments on disk are
// left for retention to prune.
func (a *App) handleDeleteCamera(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	id := r.PathValue("cam_id")
	switch err := a.camStore.Delete(id); {
	case errors.Is(err, camera.ErrNotFound):
		httpError(w, http.StatusNotFound, "camera not found")
		return
	case err != nil:
		slog.Error("delete camera failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "could not delete camera")
		return
	}

	if a.engine != nil {
		a.engine.RemoveCamera(id)
	}
	a.removeCamera(id)
	a.privacyMu.Lock()
	delete(a.privacy, id)
	a.privacyMu.Unlock()
	a.talkCodecsMu.Lock()
	delete(a.talkCodecs, id)
	a.talkCodecsMu.Unlock()

	slog.Info("camera deleted", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Camera deleted"})
}

// handleProbeCamera tests an RTSP source (admin only) for the wizard's "test
// connection" step. It always answers 200: {"ok": true, codecs, width, height}
// on success, or {"ok": false, "error": "..."} when the camera was unreachable
// or rejected the credentials — so the UI can show the reason inline rather
// than treating it as a request failure.
func (a *App) handleProbeCamera(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		Source    string `json:"source"`
		Transport string `json:"transport"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	res, err := media.ProbeSource(req.Source, req.Transport, 8*time.Second)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"codecs": res.Codecs,
		"width":  res.Width,
		"height": res.Height,
	})
}

// publicCamera renders one camera as the API view GET /api/cameras returns:
// engine stream URLs rebuilt from the current rotating credentials, the live
// privacy state applied (stale live/relay URLs cleared while paused), and the
// discovered talk codecs attached. reqHost is the request Host, used to build
// the relay URL when no [media] rtsp_host is configured.
func (a *App) publicCamera(c camera.Camera, reqHost string) camera.Camera {
	def := media.DefaultOptions()
	gMSE, gRelay, gRecord := def.MSEEnabled, def.RelayEnabled, def.RecordEnabled
	if a.engine != nil {
		gMSE, gRelay, gRecord = a.engine.GlobalToggles()
	}
	f := c.ResolveFeatures(gMSE, gRelay, gRecord)
	out := c.WithEngineURLs(a.cfg, a.creds.Current(), reqHost, f)
	a.privacyMu.RLock()
	out.Privacy = a.privacy[c.ID]
	a.privacyMu.RUnlock()
	if out.Privacy {
		out.LiveMSE = ""
		out.RTSP = ""
	}
	a.talkCodecsMu.RLock()
	out.Capabilities.TalkCodecs = a.talkCodecs[c.ID]
	a.talkCodecsMu.RUnlock()
	return out
}
