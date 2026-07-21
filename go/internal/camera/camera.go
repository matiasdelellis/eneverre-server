// Package camera defines the Camera model and loads the per-camera INI files.
package camera

import (
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"

	"eneverre/internal/config"
	"eneverre/internal/streamauth"
)

// Capabilities flags what a camera supports.
type Capabilities struct {
	// Privacy reports whether the privacy toggle is offered for this camera.
	// It defaults to true (every camera can be put in privacy) and is turned
	// off per camera with `privacy = false` in the INI — marking an "always-on"
	// camera the operator never wants paused. Privacy stops recording and
	// transmission (live MSE + RTSP relay) for any camera; on thingino cameras
	// it additionally drives the firmware lens blackout + PTZ privacy position.
	Privacy   bool `json:"privacy"`
	Thumbnail bool `json:"thumbnail"`
	Playback  bool `json:"playback"`
	PTZ       bool `json:"ptz"`
	// Talk is true when the camera INI defines a `backchannel` URL, enabling the
	// two-way-audio (ONVIF Profile T) push-to-talk endpoint.
	Talk bool `json:"talk"`
	// TalkCodecs lists the push-to-talk codecs the camera accepts, discovered by
	// probing its backchannel SDP at startup: "aac" (MPEG4-GENERIC, wideband) and
	// "g711" (PCMA/PCMU, telephony). Empty when Talk is false or the probe has not
	// completed / the camera was unreachable — in which case clients should assume
	// G.711. Omitted from JSON when empty.
	TalkCodecs []string `json:"talk_codecs,omitempty"`
}

// PTZMetadata is the public PTZ capability block on a Camera, present only
// when capabilities.ptz is true. The server is the only thing that knows the
// hardware details (motor steps, gear ratios, mechanical limits); clients see
// only the lens FOV and the angular range so a pixel drag in the UI can be
// translated to a relative move in degrees without any per-camera constants.
type PTZMetadata struct {
	// FOVH is the horizontal field of view of the lens, in degrees. Drives
	// the pixel→degree mapping for drag gestures in the UI.
	FOVH float64 `json:"fov_h"`
	// FOVV is the vertical field of view, in degrees. Derived from FOVH and
	// the sensor aspect ratio at the time the spec is loaded (it is not
	// separately configurable in the INI; lenses don't usually have an
	// independent vertical FOV).
	FOVV float64 `json:"fov_v"`
	// PanRange is the total pan range of the gimbal, in degrees (e.g. 360 for
	// a continuous-rotation mount).
	PanRange float64 `json:"pan_range"`
	// TiltRange is the total tilt range of the gimbal, in degrees.
	TiltRange float64 `json:"tilt_range"`
}

// Camera is the public-facing camera model. The Thingino credential fields are
// tagged json:"-" so marshaling a Camera never leaks them.
type Camera struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Comment  string `json:"comment"`
	Location string `json:"location"`

	Capabilities Capabilities `json:"capabilities"`

	// PTZ is the public PTZ metadata block. Present only when
	// capabilities.ptz is true; omitted otherwise. Exposes only the lens FOV
	// and the angular range — the steps/degrees calibration that maps the
	// API's pan/tilt (in degrees) to the firmware's x/y (in steps) is server-
	// side and never reaches the wire (see PanSteps et al. below).
	PTZ *PTZMetadata `json:"ptz,omitempty"`

	RTSP string `json:"rtsp"`
	// LiveMSE is the same-origin path a browser streams live fMP4 from (fed by
	// the embedded engine's MSE broadcaster). Set only in embedded-engine mode;
	// omitted otherwise. The web UI plays the live stream from this URL.
	LiveMSE string `json:"live_mse,omitempty"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`

	// Private — never serialized in API responses.
	ThinginoURL    string `json:"-"`
	ThinginoAPIKey string `json:"-"`
	// Backchannel is the direct RTSP URL (with credentials) used for two-way
	// audio. It must point at the camera itself, so it is stored raw and never
	// rewritten by URL helpers. Tagged json:"-" so the credentials never leak in
	// API responses.
	Backchannel string `json:"-"`

	// Source is the direct RTSP URL (with credentials) the embedded media engine
	// records and relays from. It must point at the camera itself. Read from
	// the INI `source` key — the same URL is also the public stream returned
	// by /api/cameras when the engine is not active. Tagged json:"-" so
	// credentials never leak in responses.
	Source string `json:"-"`

	// SnapshotURL is an HTTP(S) endpoint on the camera that returns a still JPEG
	// (many non-Thingino cameras expose one, e.g. an ONVIF/CGI snapshot path). When
	// set, /api/camera/{id}/thumbnail proxies it — giving non-Thingino cameras a
	// server snapshot without any decode/transcode. May carry credentials, so it
	// is json:"-" (never leaked in the public model). Thingino cameras use their
	// firmware API instead and ignore this.
	SnapshotURL string `json:"-"`

	// Transport overrides the embedded engine's RTSP source transport for this
	// camera: "tcp" | "udp" | "auto". Read from the INI `transport` key; empty
	// means use the global [media] transport. Useful to force TCP on a lossy
	// camera while leaving the rest on the default.
	Transport string `json:"-"`

	// Record controls whether the embedded engine writes this camera's stream
	// to disk. Read from the INI `record` key; defaults to true (cameras with
	// a Source are recorded). When false the camera still gets the live MSE
	// feed and the RTSP relay — only the on-disk segment writer is skipped
	// (so /recordings/* for this camera answer 404). Useful for privacy-
	// sensitive cameras or for cameras you only want to watch live.
	Record bool `json:"-"`

	// MSE controls whether this camera gets a live MSE broadcaster (fMP4
	// browser feed). Read from the INI `mse` key; defaults to true. Also
	// gated by the global [media] mse toggle. When false no live_mse URL is
	// returned in the API response and no broadcaster is started.
	MSE bool `json:"-"`

	// Relay controls whether this camera gets an RTSP relay entry. Read from
	// the INI `relay` key; defaults to true. Also gated by the global [media]
	// relay toggle. When false no rtsp URL is returned in the API response
	// and no relay entry is registered.
	Relay bool `json:"-"`

	// PTZ calibration (pan/tilt motor steps and the angular range they cover).
	// Server-side only: kept off the wire so a future ONVIF or generic
	// continuous-rotation PTZ camera can plug in without the API ever
	// promising a step-based contract to clients. The PTZ handler uses these
	// to convert a relative pan/tilt (degrees) from the API into firmware
	// x/y (steps) for json-motor.cgi?d=g.
	PanSteps    int `json:"-"`
	PanDegrees  int `json:"-"`
	TiltSteps   int `json:"-"`
	TiltDegrees int `json:"-"`

	// HomeX / HomeY are the configured absolute "home" position in
	// degrees (pan, tilt), sent to json-motor.cgi?d=x when /ptz/home is
	// called or when privacy is disabled. -1 means "unset" — the
	// corresponding absolute move is skipped. The server converts to
	// firmware x/y at move time using the camera's calibration. Server-side
	// only: a client never needs the raw coordinate, only the /ptz/home
	// action, so it stays off the wire (json:"-").
	HomeX float64 `json:"-"`
	HomeY float64 `json:"-"`
	// PrivacyX / PrivacyY are the configured absolute "privacy" position
	// in degrees (pan, tilt), sent to json-motor.cgi?d=x when privacy is
	// enabled. Same -1 sentinel and off-the-wire reasoning as HomeX/HomeY.
	PrivacyX float64 `json:"-"`
	PrivacyY float64 `json:"-"`

	Privacy bool `json:"privacy"`
}

// Spec is the persistable configuration of a camera: the raw fields stored in
// the DB (and, before the DB migration, in the per-camera INI). It excludes
// everything derived or runtime — capabilities, live stream URLs, and the live
// privacy state — which Camera() computes. Both the INI seed importer
// (loadOne) and the DB store map into a Spec, so the derivation of the public
// Camera model lives in exactly one place.
// The json tags let an admin-only endpoint (GET /api/camera/{id}/config) return
// the full spec — including the source/backchannel/thingino credentials — so the
// edit form can prefill. This is deliberately distinct from the public Camera
// model, whose credential fields are json:"-" and never leave the server.
type Spec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Comment  string `json:"comment"`
	Location string `json:"location"`

	Source      string `json:"source"`
	Backchannel string `json:"backchannel"`
	Transport   string `json:"transport"`
	// SnapshotURL is the camera's own still-JPEG HTTP endpoint, proxied by the
	// thumbnail route for non-Thingino cameras. Returned by the admin config
	// endpoint (may carry credentials) so the edit form can prefill it.
	SnapshotURL string `json:"snapshot_url"`

	Record   bool `json:"record"`
	MSE      bool `json:"mse"`
	Relay    bool `json:"relay"`
	Privacy  bool `json:"privacy"` // whether the privacy toggle is offered (INI `privacy`, default true)
	Playback bool `json:"playback"`

	Width  int `json:"width"`
	Height int `json:"height"`

	ThinginoURL    string  `json:"thingino_url"`
	ThinginoAPIKey string  `json:"thingino_api_key"`
	PTZ            bool    `json:"ptz"`
	HomeX          float64 `json:"home_x"`
	HomeY          float64 `json:"home_y"`
	PrivacyX       float64 `json:"privacy_x"`
	PrivacyY       float64 `json:"privacy_y"`

	// PTZ calibration: total steps per axis and the angular range those steps
	// cover. Defaults match the typical thingino gimbal (2130/360 pan,
	// 1600/180 tilt) so existing installs work without any INI change; the
	// FOVH is the horizontal lens FOV in degrees (vertical is derived from the
	// aspect at conversion time). These never leave the server — the public
	// Camera model only exposes the angular range and the FOV (PTZMetadata).
	PanSteps    int     `json:"pan_steps"`
	PanDegrees  int     `json:"pan_degrees"`
	TiltSteps   int     `json:"tilt_steps"`
	TiltDegrees int     `json:"tilt_degrees"`
	FOVH        float64 `json:"fov_h"`
}

// Camera expands a Spec into the public Camera model, deriving capabilities
// exactly as the INI loader historically did: Thumbnail follows a thingino
// firmware endpoint (url + key) or a snapshot URL, Talk follows a backchannel
// URL, PTZ/Playback/Privacy come straight from the spec. The live-privacy state
// and engine stream URLs are left at their zero values; the server fills them
// per request.
func (s Spec) Camera() Camera {
	// Guarantee a complete PTZ calibration regardless of how this Spec was
	// built — callers that already went through ApplyPTZDefaults (loadSpec,
	// scanSpec, req.spec()) leave this a no-op.
	s.ApplyPTZDefaults()
	// The thumbnail handler's thingino path needs BOTH the base URL and the API
	// key; advertise the capability on the same condition so it never claims a
	// thumbnail the endpoint would answer 404 for.
	hasThinginoThumb := s.ThinginoURL != "" && s.ThinginoAPIKey != ""
	cam := Camera{
		ID:       s.ID,
		Name:     s.Name,
		Comment:  s.Comment,
		Location: s.Location,
		Capabilities: Capabilities{
			Privacy: s.Privacy,
			// A thumbnail is available from a Thingino firmware endpoint or any
			// camera's own snapshot URL (proxied without decode).
			Thumbnail: hasThinginoThumb || s.SnapshotURL != "",
			Playback:  s.Playback,
			PTZ:       s.PTZ,
			Talk:      s.Backchannel != "",
		},
		RTSP:           s.Source,
		Backchannel:    s.Backchannel,
		Source:         s.Source,
		SnapshotURL:    s.SnapshotURL,
		Transport:      s.Transport,
		Record:         s.Record,
		MSE:            s.MSE,
		Relay:          s.Relay,
		PanSteps:       s.PanSteps,
		PanDegrees:     s.PanDegrees,
		TiltSteps:      s.TiltSteps,
		TiltDegrees:    s.TiltDegrees,
		Width:          s.Width,
		Height:         s.Height,
		ThinginoURL:    s.ThinginoURL,
		ThinginoAPIKey: s.ThinginoAPIKey,
		HomeX:          s.HomeX,
		HomeY:          s.HomeY,
		PrivacyX:       s.PrivacyX,
		PrivacyY:       s.PrivacyY,
		Privacy:        false,
	}
	// Build the public PTZ block only when the camera actually has PTZ — the
	// capability flag is the public contract for "this metadata is present".
	// The block exposes only the lens FOV and the angular range; the
	// steps↔degrees mapping is server-side (see PTZMetadata).
	if s.PTZ {
		fovH := s.FOVH
		fovV := fovH
		if s.Width > 0 {
			fovV = fovH * float64(s.Height) / float64(s.Width)
		}
		cam.PTZ = &PTZMetadata{
			FOVH:      fovH,
			FOVV:      fovV,
			PanRange:  float64(s.PanDegrees),
			TiltRange: float64(s.TiltDegrees),
		}
	}
	return cam
}

func toFloat(value string, def float64) float64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return def
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return def
	}
	return n
}

func toInt(value string, def int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return def
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return def
	}
	return n
}

// Default PTZ calibration values used when a camera declares PTZ but the
// operator hasn't customized the [thingino] section. The numbers match the
// typical thingino gimbal (2130/360 pan, 1600/180 tilt) so existing installs
// keep behaving as before; the 113° horizontal FOV is a sensible default for
// the wide-angle lenses these cameras ship with. They are also the DB
// column defaults (see store.go's schema), so a brand-new camera row
// that bypasses the INI loader still gets the same starting point.
const (
	DefaultPanSteps    = 2130
	DefaultPanDegrees  = 360
	DefaultTiltSteps   = 1600
	DefaultTiltDegrees = 180
	DefaultFOVH        = 113.0
)

// ApplyPTZDefaults fills every unset (zero or negative) PTZ calibration field
// with the default calibration. This is the one chokepoint for the defaulting
// rule: the INI loader, the DB row scan and the HTTP create/update path all
// call it, so a Spec from any source carries a complete calibration and no
// other layer needs its own fallback.
func (s *Spec) ApplyPTZDefaults() {
	if s.PanSteps <= 0 {
		s.PanSteps = DefaultPanSteps
	}
	if s.PanDegrees <= 0 {
		s.PanDegrees = DefaultPanDegrees
	}
	if s.TiltSteps <= 0 {
		s.TiltSteps = DefaultTiltSteps
	}
	if s.TiltDegrees <= 0 {
		s.TiltDegrees = DefaultTiltDegrees
	}
	if s.FOVH <= 0 {
		s.FOVH = DefaultFOVH
	}
}

// LoadSpecs reads every *.ini under the cameras dir (sorted, in filename order)
// and returns their raw configuration specs. It is the reader behind the
// one-time INI → DB seed (see SeedFromINI); it does not touch the DB itself.
func LoadSpecs(cfg *config.Config) []Spec {
	paths, _ := filepath.Glob(filepath.Join(cfg.CamerasDir, "*.ini"))
	sort.Strings(paths)

	specs := make([]Spec, 0, len(paths))
	for _, p := range paths {
		if s, ok := loadSpec(p); ok {
			specs = append(specs, s)
		}
	}
	return specs
}

// loadOne parses one camera INI into the public Camera model. Kept as a thin
// wrapper over loadSpec for tests and any caller that wants the derived model
// directly.
func loadOne(path string) (Camera, bool) {
	s, ok := loadSpec(path)
	if !ok {
		return Camera{}, false
	}
	return s.Camera(), true
}

func loadSpec(path string) (Spec, bool) {
	name := filepath.Base(path)
	f, err := ini.LoadSources(ini.LoadOptions{Insensitive: true}, path)
	if err != nil {
		slog.Warn("skipping camera ini", "file", name, "err", err)
		return Spec{}, false
	}
	if !f.HasSection("camera") {
		slog.Warn("skipping camera ini: missing [camera] section", "file", name)
		return Spec{}, false
	}
	cam := f.Section("camera")
	id := cam.Key("id").String()
	if id == "" {
		slog.Warn("skipping camera ini: missing id", "file", name)
		return Spec{}, false
	}

	thingino := map[string]string{}
	if f.HasSection("thingino") {
		for _, k := range f.Section("thingino").Keys() {
			thingino[strings.ToLower(k.Name())] = k.Value()
		}
	}

	source := strings.TrimSpace(cam.Key("source").String())
	backchannel := strings.TrimSpace(cam.Key("backchannel").String())
	snapshotURL := strings.TrimSpace(cam.Key("snapshot_url").String())
	transport := strings.ToLower(strings.TrimSpace(cam.Key("transport").String()))
	// Default to true: cameras with a Source are recorded. A per-camera
	// `record = false` opts out of disk writing (the live pipeline keeps
	// running). Recording itself still requires [media] record = true
	// globally — see the engine for the gate logic.
	record := cam.Key("record").MustBool(true)
	// Default to true: cameras with a Source get an MSE broadcaster.
	// Gated independently of relay — each can be toggled without affecting
	// the other. Both still require the global [media] mse/relay toggles.
	mse := cam.Key("mse").MustBool(true)
	relay := cam.Key("relay").MustBool(true)
	// Privacy control defaults to on: every camera offers the privacy toggle
	// (stop recording + transmission; plus firmware lens blackout on thingino).
	// `privacy = false` marks an always-on camera the operator never wants paused.
	privacyAllowed := cam.Key("privacy").MustBool(true)

	ptz := strings.ToLower(strings.TrimSpace(thingino["ptz"])) == "true"
	// Playback (the recordings switch in the UI + the /recordings/* endpoints)
	// defaults to the camera's `record` value: a recording camera exposes its
	// footage unless the operator explicitly sets `playback = false`.
	playback := cam.Key("playback").MustBool(record)

	s := Spec{
		ID:             id,
		Name:           cam.Key("name").String(),
		Comment:        cam.Key("comment").String(),
		Location:       cam.Key("location").String(),
		Source:         source,
		Backchannel:    backchannel,
		SnapshotURL:    snapshotURL,
		Transport:      transport,
		Record:         record,
		MSE:            mse,
		Relay:          relay,
		Privacy:        privacyAllowed,
		Playback:       playback,
		Width:          cam.Key("width").MustInt(16),
		Height:         cam.Key("height").MustInt(9),
		ThinginoURL:    thingino["thingino_url"],
		ThinginoAPIKey: thingino["thingino_api_key"],
		PTZ:            ptz,
		HomeX:          toFloat(thingino["home_x"], -1),
		HomeY:          toFloat(thingino["home_y"], -1),
		PrivacyX:       toFloat(thingino["privacy_x"], -1),
		PrivacyY:       toFloat(thingino["privacy_y"], -1),
		PanSteps:       toInt(thingino["pan_steps"], 0),
		PanDegrees:     toInt(thingino["pan_degrees"], 0),
		TiltSteps:      toInt(thingino["tilt_steps"], 0),
		TiltDegrees:    toInt(thingino["tilt_degrees"], 0),
		FOVH:           toFloat(thingino["fov_h"], 0),
	}
	s.ApplyPTZDefaults()
	return s, true
}

// Get returns a pointer to the camera with the given id, or nil.
func Get(cams []Camera, id string) *Camera {
	for i := range cams {
		if cams[i].ID == id {
			return &cams[i]
		}
	}
	return nil
}

// Features is the resolved on/off state of the engine's three per-camera sinks.
type Features struct {
	MSE    bool
	Relay  bool
	Record bool
}

// ResolveFeatures combines the engine's global [media] toggles with this
// camera's per-camera flags. Each sink is on only when both the global and the
// per-camera flag are on — the per-camera flag is opt-out only (see the
// "master switch + opt-out" model in doc/MEDIA.md). This is the single source
// of truth for the gating rule; both the engine (what it starts) and the API
// (what URLs it advertises) call it so the two can never disagree.
func (c Camera) ResolveFeatures(globalMSE, globalRelay, globalRecord bool) Features {
	return Features{
		MSE:    globalMSE && c.MSE,
		Relay:  globalRelay && c.Relay,
		Record: globalRecord && c.Record,
	}
}

// PTZDeltaToSteps converts a relative pan/tilt in degrees (the unit the public
// API uses) into firmware-native x/y in steps (the unit json-motor.cgi?d=g
// speaks). Each axis is clamped to the camera's full mechanical range so a
// single bad request can't command an unbounded rotation — a relative move
// of more than half the range in either direction lands at the end of the
// axis instead. Sign convention: pan > 0 = right, tilt > 0 = down.
func (c Camera) PTZDeltaToSteps(pan, tilt float64) (x, y float64) {
	x, y = c.PTZDegreesToSteps(pan, tilt)
	x = clampAbs(x, float64(c.PanSteps)/2)
	y = clampAbs(y, float64(c.TiltSteps)/2)
	return x, y
}

// clampAbs limits v to [-limit, limit]. A non-positive limit disables the
// clamp (an axis without a valid calibration already converts to 0 steps).
func clampAbs(v, limit float64) float64 {
	if limit <= 0 {
		return v
	}
	if v > limit {
		return limit
	}
	if v < -limit {
		return -limit
	}
	return v
}

// PTZStepsToDegrees converts a firmware x/y in steps into a pan/tilt in
// degrees, using the same calibration. Used to render the cached
// /ptz/position in the unit the public API speaks. The reported position is
// always within the mechanical range, so no clamping is applied.
func (c Camera) PTZStepsToDegrees(x, y float64) (pan, tilt float64) {
	if c.PanSteps > 0 {
		pan = x * float64(c.PanDegrees) / float64(c.PanSteps)
	}
	if c.TiltSteps > 0 {
		tilt = y * float64(c.TiltDegrees) / float64(c.TiltSteps)
	}
	return pan, tilt
}

// PTZDegreesToSteps converts an absolute pan/tilt in degrees to firmware
// x/y in steps, using the same calibration as the relative-move path. Used
// for the configured absolute positions (home, privacy): the server stores
// them in the public unit and converts at move time, so an operator can
// re-tune the calibration without rewriting the INI. No range clamp is
// applied: absolute moves target a specific position inside the
// mechanical range (or exactly on its boundary), and the firmware is the
// authority on its own end-stops.
func (c Camera) PTZDegreesToSteps(pan, tilt float64) (x, y float64) {
	if c.PanDegrees > 0 && c.PanSteps > 0 {
		x = pan * float64(c.PanSteps) / float64(c.PanDegrees)
	}
	if c.TiltDegrees > 0 && c.TiltSteps > 0 {
		y = tilt * float64(c.TiltSteps) / float64(c.TiltDegrees)
	}
	return x, y
}

// WithEngineURLs returns a copy with URLs rebuilt for the embedded media engine.
// A feed's URL is advertised only when its resolved feature is on (f, from
// ResolveFeatures), so the API never advertises a feed the engine did not start.
// The host is taken from the configured `[media] rtsp_host` when set
// (authoritative for public / reverse-proxied deployments), otherwise the host
// the client used to reach the API (reqHost) so RTSP works out of the box on a
// LAN. The raw camera source URL (which carries credentials) is never exposed.
func (c Camera) WithEngineURLs(cfg *config.Config, creds streamauth.Creds, reqHost string, f Features) Camera {
	if f.MSE {
		c.LiveMSE = "/api/camera/" + c.ID + "/live/stream"
	} else {
		c.LiveMSE = ""
	}
	if f.Relay {
		host := cfg.Media.Get("rtsp_host", "")
		if host == "" {
			host = hostOnly(reqHost)
		}
		if host != "" {
			port := portFromAddr(cfg.Media.Get("rtsp_address", ":8554"))
			c.RTSP = creds.RtspURL(host, port, c.ID)
		} else {
			c.RTSP = ""
		}
	} else {
		c.RTSP = ""
	}
	return c
}

// hostOnly strips the port from an HTTP Host header ("192.168.1.95:8081" ->
// "192.168.1.95", "[::1]:8081" -> "::1"), returning the input unchanged when it
// has no port.
func hostOnly(hostPort string) string {
	if hostPort == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}

// portFromAddr extracts the port from a listen address like ":8554" or
// "0.0.0.0:8554"; returns "8554" as a fallback.
func portFromAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return "8554"
}
