// Package camera defines the Camera model and loads the per-camera INI files,
// porting app/models/camera.py and app/services/camera_service.py.
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

// Camera is the public-facing camera model. The Thingino credential fields are
// tagged json:"-" so marshaling a Camera is equivalent to the Python public()
// helper that strips them.
type Camera struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Comment  string `json:"comment"`
	Location string `json:"location"`

	Capabilities Capabilities `json:"capabilities"`

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

	HomeX    float64 `json:"home_x"`
	HomeY    float64 `json:"home_y"`
	PrivacyX float64 `json:"privacy_x"`
	PrivacyY float64 `json:"privacy_y"`

	Privacy bool `json:"privacy"`

	// Compatibility aliases for older clients.
	PlaybackAlias bool `json:"playback"`
	PTZAlias      bool `json:"ptz"`
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

// Load reads every *.ini under the cameras dir (sorted), in startup order.
// The camera's `source` URL is stored as the raw INI value. When the embedded
// media engine is enabled, /api/cameras rebuilds the public RTSP URL per
// request from the current rotating credentials via WithEngineURLs, so
// credential rotation takes effect without a restart.
func Load(cfg *config.Config) []Camera {
	paths, _ := filepath.Glob(filepath.Join(cfg.CamerasDir, "*.ini"))
	sort.Strings(paths)

	cams := make([]Camera, 0, len(paths))
	for _, p := range paths {
		if cam, ok := loadOne(p); ok {
			cams = append(cams, cam)
		}
	}
	slog.Info("loaded cameras", "count", len(cams), "dir", cfg.CamerasDir)
	return cams
}

func loadOne(path string) (Camera, bool) {
	name := filepath.Base(path)
	f, err := ini.LoadSources(ini.LoadOptions{Insensitive: true}, path)
	if err != nil {
		slog.Warn("skipping camera ini", "file", name, "err", err)
		return Camera{}, false
	}
	if !f.HasSection("camera") {
		slog.Warn("skipping camera ini: missing [camera] section", "file", name)
		return Camera{}, false
	}
	cam := f.Section("camera")
	id := cam.Key("id").String()
	if id == "" {
		slog.Warn("skipping camera ini: missing id", "file", name)
		return Camera{}, false
	}

	thingino := map[string]string{}
	if f.HasSection("thingino") {
		for _, k := range f.Section("thingino").Keys() {
			thingino[strings.ToLower(k.Name())] = k.Value()
		}
	}

	source := strings.TrimSpace(cam.Key("source").String())
	backchannel := strings.TrimSpace(cam.Key("backchannel").String())
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

	hasAPIKey := thingino["thingino_api_key"] != ""
	ptz := strings.ToLower(strings.TrimSpace(thingino["ptz"])) == "true"
	playback := cam.Key("playback").MustBool(false)

	return Camera{
		ID:       id,
		Name:     cam.Key("name").String(),
		Comment:  cam.Key("comment").String(),
		Location: cam.Key("location").String(),
		Capabilities: Capabilities{
			Privacy:   hasAPIKey,
			Thumbnail: hasAPIKey,
			Playback:  playback,
			PTZ:       ptz,
			Talk:      backchannel != "",
		},
		RTSP:           source,
		Backchannel:    backchannel,
		Source:         source,
		Transport:      transport,
		Record:         record,
		MSE:            mse,
		Relay:          relay,
		Width:          cam.Key("width").MustInt(16),
		Height:         cam.Key("height").MustInt(9),
		ThinginoURL:    thingino["thingino_url"],
		ThinginoAPIKey: thingino["thingino_api_key"],
		HomeX:          toFloat(thingino["home_x"], -1),
		HomeY:          toFloat(thingino["home_y"], -1),
		PrivacyX:       toFloat(thingino["privacy_x"], -1),
		PrivacyY:       toFloat(thingino["privacy_y"], -1),
		Privacy:        false,
		PlaybackAlias:  playback,
		PTZAlias:       ptz,
	}, true
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
