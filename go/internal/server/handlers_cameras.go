package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"eneverre/internal/backchannel"
	"eneverre/internal/camera"
	"eneverre/internal/media"
	"eneverre/internal/thingino"
)

// idPattern constrains an id to a short, filesystem- and URL-safe token: it is
// used as a recording subdirectory (the `%path` segment) and throughout the API
// paths, so path separators, dots, and whitespace are rejected and the first
// character must be alphanumeric. Camera ids are no longer entered by hand —
// they are derived from the name (camera.Slugify, which produces a token in
// this shape) — but schedules still take an operator-supplied id, so the
// pattern lives on (see schedIDPattern).
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// createCameraReq is the JSON body of POST /api/cameras. The three-state
// booleans (record/mse/relay/privacy/playback) and the numeric fields are
// pointers so an omitted key falls back to the same defaults the INI loader
// applies, rather than to Go's zero value. There is no `id` field: the camera
// id is always derived from the name by the handler (it is the recording path
// and only ever appears in URLs thereafter).
type createCameraReq struct {
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

	// ScheduleID references a recording schedule by id; "" (or omitted) means
	// record 24/7. Validated against the schedule store by the handler.
	ScheduleID string `json:"schedule_id"`

	ThinginoURL    string   `json:"thingino_url"`
	ThinginoAPIKey string   `json:"thingino_api_key"`
	PTZ            *bool    `json:"ptz"`
	HomeX          *float64 `json:"home_x"`
	HomeY          *float64 `json:"home_y"`
	PrivacyX       *float64 `json:"privacy_x"`
	PrivacyY       *float64 `json:"privacy_y"`
	// PTZ calibration (pan/tilt motor steps and the angular range they cover,
	// plus the horizontal lens FOV). Pointers so an omitted key falls back to
	// the same defaults every Spec source applies (Spec.ApplyPTZDefaults in
	// the camera package). Omitted fields mean "use the default", not
	// "use zero", so a wizard that hides these fields still works.
	PanSteps    *int     `json:"pan_steps"`
	PanDegrees  *int     `json:"pan_degrees"`
	TiltSteps   *int     `json:"tilt_steps"`
	TiltDegrees *int     `json:"tilt_degrees"`
	FOVH        *float64 `json:"fov_h"`
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
	// The returned Spec carries no id: on create the handler derives it from the
	// name (camera.Slugify + uniqueness), and on update the handler sets it from
	// the URL path. The id is never part of the request body.
	//
	// The name is required on both create and update: it is the camera's
	// identity (the id is a slug of it on create) and there is no reason to strip
	// it when editing. Renaming is allowed — it just never changes the id.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return camera.Spec{}, "name is required"
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
	s := camera.Spec{
		Name:        name,
		Comment:     strings.TrimSpace(req.Comment),
		Location:    strings.TrimSpace(req.Location),
		Source:      source,
		Backchannel: strings.TrimSpace(req.Backchannel),
		SnapshotURL: strings.TrimSpace(req.SnapshotURL),
		Transport:   transport,
		Record:      boolOr(req.Record, true),
		MSE:         boolOr(req.MSE, true),
		Relay:       boolOr(req.Relay, true),
		Privacy:     boolOr(req.Privacy, true),
		// Playback defaults to the request's record value: a recording camera
		// exposes its footage unless the client explicitly disables playback.
		Playback:       boolOr(req.Playback, boolOr(req.Record, true)),
		Width:          intOr(req.Width, 16),
		Height:         intOr(req.Height, 9),
		ThinginoURL:    strings.TrimSpace(req.ThinginoURL),
		ThinginoAPIKey: strings.TrimSpace(req.ThinginoAPIKey),
		PTZ:            boolOr(req.PTZ, false),
		HomeX:          floatOr(req.HomeX, -1),
		HomeY:          floatOr(req.HomeY, -1),
		PrivacyX:       floatOr(req.PrivacyX, -1),
		PrivacyY:       floatOr(req.PrivacyY, -1),
		PanSteps:       intOr(req.PanSteps, 0),
		PanDegrees:     intOr(req.PanDegrees, 0),
		TiltSteps:      intOr(req.TiltSteps, 0),
		TiltDegrees:    intOr(req.TiltDegrees, 0),
		FOVH:           floatOr(req.FOVH, 0),
		ScheduleID:     strings.TrimSpace(req.ScheduleID),
	}
	s.ApplyPTZDefaults()
	return s, ""
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
	if !decodeJSON(w, r, &req) {
		return
	}
	s, msg := req.spec()
	if msg != "" {
		httpError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if ok, err := a.scheduleExists(s.ScheduleID); err != nil {
		slog.Error("schedule lookup failed", "id", s.ScheduleID, "err", err)
		httpError(w, http.StatusInternalServerError, "could not validate schedule")
		return
	} else if !ok {
		httpError(w, http.StatusUnprocessableEntity, "schedule_id references an unknown schedule")
		return
	}

	// Serialize the DB + engine + in-memory mutation against other camera
	// create/update/delete so they can't interleave.
	a.camMutateMu.Lock()
	defer a.camMutateMu.Unlock()

	// The id is always derived from the name: slug it, then disambiguate against
	// existing cameras. Done under the lock so the uniqueness probe (UniqueID)
	// and the insert are atomic with respect to other camera mutations.
	base := camera.Slugify(s.Name)
	if base == "" {
		// The name is present (spec() enforced that) but has nothing sluggable,
		// so no id can be derived from it.
		httpError(w, http.StatusUnprocessableEntity, "name must contain at least one letter or digit")
		return
	}
	id, err := a.camStore.UniqueID(base)
	if err != nil {
		slog.Error("derive camera id failed", "name", s.Name, "err", err)
		httpError(w, http.StatusInternalServerError, "could not assign a camera id")
		return
	}
	s.ID = id

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
	a.seedHeartbeatFor(cam)
	a.seedPTZPositionsFor(cam)
	a.seedTalkCodecsFor(cam)
	// Apply the recording schedule immediately: a camera created off-hours must
	// start paused, not wait for the next minute tick.
	a.reevaluateSchedules()

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

// dropCameraState clears every per-camera runtime cache (privacy, heartbeat,
// talk codecs, PTZ position). Called when a camera is updated (the config the
// caches were derived from may have changed) or deleted, so a new per-camera
// cache only needs a line here to be handled by both.
func (a *App) dropCameraState(id string) {
	a.privacyMu.Lock()
	delete(a.privacy, id)
	delete(a.schedOff, id)
	a.privacyMu.Unlock()
	a.talkCodecsMu.Lock()
	delete(a.talkCodecs, id)
	a.talkCodecsMu.Unlock()
	a.heartbeatsMu.Lock()
	delete(a.heartbeats, id)
	a.heartbeatsMu.Unlock()
	a.ptzPosMu.Lock()
	delete(a.ptzPos, id)
	a.ptzPosMu.Unlock()
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
	if !decodeJSON(w, r, &req) {
		return
	}
	s, msg := req.spec()
	if msg != "" {
		httpError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	s.ID = id // the id is fixed by the URL path; it is never in the body
	if ok, err := a.scheduleExists(s.ScheduleID); err != nil {
		slog.Error("schedule lookup failed", "id", s.ScheduleID, "err", err)
		httpError(w, http.StatusInternalServerError, "could not validate schedule")
		return
	} else if !ok {
		httpError(w, http.StatusUnprocessableEntity, "schedule_id references an unknown schedule")
		return
	}

	// Serialize the DB + engine reconfigure (RemoveCamera+AddCamera) + in-memory
	// update so a concurrent update/delete of the same camera can't interleave.
	a.camMutateMu.Lock()
	defer a.camMutateMu.Unlock()

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
	// changed, so the old privacy/talk-codec/position state no longer applies.
	a.dropCameraState(id)
	a.seedHeartbeatFor(cam)
	a.seedPTZPositionsFor(cam)
	a.seedTalkCodecsFor(cam)
	// The schedule assignment may have changed (and RemoveCamera+AddCamera reset
	// the engine's pause) — re-evaluate so the new schedule takes effect at once.
	a.reevaluateSchedules()

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
	// Serialize against other camera create/update/delete (see camMutateMu).
	a.camMutateMu.Lock()
	defer a.camMutateMu.Unlock()
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
	a.dropCameraState(id)

	slog.Info("camera deleted", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Camera deleted"})
}

// handleProbeCamera tests an RTSP source (admin only) for the wizard's "test
// connection" step. It always answers 200: {"ok": true, codecs, width, height,
// backchannel: {supported, codecs}} on success, or {"ok": false, "error":
// "..."} when the camera was unreachable or rejected the credentials — so the
// UI can show the reason inline rather than treating it as a request failure.
// backchannel reports whether the same URL doubles as the ONVIF two-way-audio
// backchannel (it does on thingino/prudynt) and which talk codecs it accepts,
// so the wizard can fill the backchannel field without the operator retyping
// the URL.
func (a *App) handleProbeCamera(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		Source    string `json:"source"`
		Transport string `json:"transport"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := media.ProbeSource(req.Source, req.Transport, 8*time.Second)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Backchannel probe: same handshake the server runs at startup, bounded by
	// the same 8s budget so a hung camera can't stall the wizard.
	bc := map[string]any{"supported": false}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if codecs, err := backchannel.ProbeCodecs(ctx, req.Source); err == nil && len(codecs) > 0 {
		bc = map[string]any{"supported": true, "codecs": codecs}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"codecs":      res.Codecs,
		"width":       res.Width,
		"height":      res.Height,
		"backchannel": bc,
	})
}

// handleProbeThingino tests a Thingino camera's URL + API key (admin only)
// for the wizard's Thingino step. It always answers 200: {"ok": false,
// "error": "..."} when the camera is unreachable or rejects the API key, or
// {"ok": true, "ptz": bool, ...} once reached. When the camera also reports a
// working motor, the PTZ calibration fields come along too — steps_pan/
// steps_tilt read straight off the firmware, pan assumed a full 360°
// (every Thingino gimbal rotates continuously) with tilt's degree range
// derived from pan's steps-per-degree ratio (the firmware doesn't report a
// tilt range directly), the firmware's own configured position as home, and
// privacy leveled to the same pan with tilt at 0°.
func (a *App) handleProbeThingino(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		ThinginoURL    string `json:"thingino_url"`
		ThinginoAPIKey string `json:"thingino_api_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	host := strings.TrimSpace(req.ThinginoURL)
	key := strings.TrimSpace(req.ThinginoAPIKey)
	if host == "" || key == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "URL and API key are required"})
		return
	}
	if _, err := thingino.State(host, key); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	res := map[string]any{"ok": true, "ptz": false}
	// A camera without a gimbal fails this call — that's "no PTZ to
	// calibrate", not a failure of the connection test above.
	if params, err := thingino.Params(host, key); err == nil && params.StepsPan > 0 && params.StepsTilt > 0 {
		const panDegrees = 360
		tiltDegrees := int(math.Round(float64(params.StepsTilt) * panDegrees / float64(params.StepsPan)))
		homeX := roundTenth(float64(params.Pos0X) * panDegrees / float64(params.StepsPan))
		homeY := roundTenth(float64(params.Pos0Y) * float64(tiltDegrees) / float64(params.StepsTilt))
		res["ptz"] = true
		res["pan_steps"] = params.StepsPan
		res["pan_degrees"] = panDegrees
		res["tilt_steps"] = params.StepsTilt
		res["tilt_degrees"] = tiltDegrees
		res["home_x"] = homeX
		res["home_y"] = homeY
		res["privacy_x"] = homeX
		res["privacy_y"] = 0.0
	}
	writeJSON(w, http.StatusOK, res)
}

func roundTenth(v float64) float64 {
	return math.Round(v*10) / 10
}

// handleGetCameraSettings serves the cached live-settings snapshot of one
// camera (admin-only): the read-only state the /api/status toggles used to
// show — day/night mode, illuminators, motion, audio — fetched in the
// background by the heartbeat seed/refresh loop. 404 while no snapshot has
// been fetched yet (unreachable camera at startup) or when the camera lacks
// the settings capability.
func (a *App) handleGetCameraSettings(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.Settings || cam.ThinginoURL == "" || cam.ThinginoAPIKey == "" {
		httpError(w, http.StatusNotFound, "camera settings not available")
		return
	}
	a.heartbeatsMu.RLock()
	hi, ok := a.heartbeats[cam.ID]
	a.heartbeatsMu.RUnlock()
	if !ok {
		httpError(w, http.StatusNotFound, "camera settings state not fetched yet")
		return
	}
	writeJSON(w, http.StatusOK, settingsSnapshot(hi))
}

// handleSetCameraSettings lets an admin adjust a camera's live settings —
// motion detection, mic input, speaker output, day/night auto or a fixed
// day/night mode, and the illuminators (IR cut, IR850, IR940, white light) —
// through the camera's own control channel. The API shape is
// backend-agnostic (Capabilities.Settings advertises it); today the only
// backend is thingino, mapped onto prudynt config fragments
// (json-prudynt.cgi, the same channel privacy uses) and allowlisted IMP
// commands (json-imp.cgi — the vocabulary was verified against the
// firmware's own web UI). Each field present in the body is applied in
// order; on the first failure the request answers 502 with the failing
// field, so a partial application is visible in the error. After a
// successful change the heartbeat cache is refreshed in the background, so
// /api/status reflects the new state within the heartbeat's ~1s call
// instead of the next 5-minute loop.
func (a *App) handleSetCameraSettings(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.Settings || cam.ThinginoURL == "" || cam.ThinginoAPIKey == "" {
		httpError(w, http.StatusNotFound, "camera settings not available")
		return
	}
	var req struct {
		MotionEnabled  *bool   `json:"motion_enabled"`
		MicEnabled     *bool   `json:"mic_enabled"`
		SpeakerEnabled *bool   `json:"speaker_enabled"`
		DaynightAuto   *bool   `json:"daynight_auto"`
		DaynightMode   *string `json:"daynight_mode"` // "day" | "night" | "manual"
		IRCut          *bool   `json:"ircut"`
		IR850          *bool   `json:"ir850"`
		IR940          *bool   `json:"ir940"`
		White          *bool   `json:"white"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DaynightMode != nil && *req.DaynightMode != "day" && *req.DaynightMode != "night" && *req.DaynightMode != "manual" {
		httpError(w, http.StatusBadRequest, `daynight_mode must be "day", "night" or "manual"`)
		return
	}

	type upd struct {
		label string
		run   func() error
	}
	var upds []upd
	prudynt := func(label string, fragment map[string]any) {
		upds = append(upds, upd{label, func() error {
			_, err := thingino.SetPrudynt(cam.ThinginoURL, cam.ThinginoAPIKey, fragment)
			return err
		}})
	}
	imp := func(label, cmd string, val any) {
		upds = append(upds, upd{label, func() error {
			_, err := thingino.Imp(cam.ThinginoURL, cam.ThinginoAPIKey, cmd, val)
			return err
		}})
	}
	boolInt := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}

	if req.MotionEnabled != nil {
		prudynt("motion", map[string]any{"motion": map[string]any{"enabled": *req.MotionEnabled}})
	}
	if req.MicEnabled != nil {
		prudynt("mic", map[string]any{"audio": map[string]any{"input_enabled": *req.MicEnabled}})
	}
	if req.SpeakerEnabled != nil {
		prudynt("speaker", map[string]any{"audio": map[string]any{"output_enabled": *req.SpeakerEnabled}})
	}
	if req.DaynightAuto != nil {
		imp("daynight_auto", "auto", boolInt(*req.DaynightAuto))
	}
	if req.DaynightMode != nil {
		imp("daynight_mode", "daynight", *req.DaynightMode)
	}
	if req.IRCut != nil {
		imp("ircut", "ircut", boolInt(*req.IRCut))
	}
	if req.IR850 != nil {
		imp("ir850", "ir850", boolInt(*req.IR850))
	}
	if req.IR940 != nil {
		imp("ir940", "ir940", boolInt(*req.IR940))
	}
	if req.White != nil {
		imp("white", "white", boolInt(*req.White))
	}
	if len(upds) == 0 {
		httpError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	for _, u := range upds {
		if err := u.run(); err != nil {
			slog.Warn("camera settings update failed", "camera", cam.ID, "field", u.label, "err", err)
			httpError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", u.label, err))
			return
		}
		// prudynt applies each config change by restarting the affected
		// worker; a multi-field request must give the previous one a moment
		// to land or a rapid second write can be lost (observed live: a
		// motion toggle-restore pair 300ms apart lost the restore). The
		// camera's own web UI sends one field per click, so this settle
		// delay mirrors that pacing.
		if len(upds) > 1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	// Refresh the cached heartbeat so the status view shows the change now.
	a.seedHeartbeatFor(cam)
	slog.Info("camera settings updated", "camera", cam.ID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "updated"})
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
	out.ScheduleOff = a.schedOff[c.ID]
	a.privacyMu.RUnlock()
	// A camera that is manually in privacy OR currently outside its recording
	// schedule has no live feed — the pipeline is paused — so clear the stale
	// stream URLs just as the privacy path does.
	if out.Privacy || out.ScheduleOff {
		out.LiveMSE = ""
		out.RTSP = ""
	}
	a.talkCodecsMu.RLock()
	out.Capabilities.TalkCodecs = a.talkCodecs[c.ID]
	// Talk capability is advertised when the camera defines a backchannel URL
	// OR the source probe found a backchannel on the source itself (see
	// backchannelURL in server.go) — the explicit config is an override, not a
	// requirement.
	out.Capabilities.Talk = out.Capabilities.Talk || a.talkCodecs[c.ID] != nil
	a.talkCodecsMu.RUnlock()
	// Playback is advertised only when the camera actually has recordings on
	// disk. The per-camera `playback` flag is an opt-out layered on top (an
	// admin can hide playback for a camera that does record) — it is not a
	// hint, so a camera with no segments never advertises playback. No engine
	// (or recording globally off) means no index, hence no playback.
	out.Capabilities.Playback = out.Capabilities.Playback && a.engine != nil && a.engine.HasRecordings(c.ID)
	return out
}
