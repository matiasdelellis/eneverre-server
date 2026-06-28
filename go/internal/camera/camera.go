// Package camera defines the Camera model and loads the per-camera INI files,
// porting app/models/camera.py and app/services/camera_service.py.
package camera

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"

	"eneverre/internal/config"
	"eneverre/internal/mediamtx"
)

// Capabilities flags what a camera supports.
type Capabilities struct {
	Privacy   bool `json:"privacy"`
	Thumbnail bool `json:"thumbnail"`
	Playback  bool `json:"playback"`
	PTZ       bool `json:"ptz"`
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

	RTSP   string `json:"rtsp"`
	WebRTC string `json:"webrtc"`
	HLS    string `json:"hls"`
	Width  int    `json:"width"`
	Height int    `json:"height"`

	// Private — never serialized in API responses.
	ThinginoURL    string `json:"-"`
	ThinginoAPIKey string `json:"-"`

	HomeX    float64 `json:"home_x"`
	HomeY    float64 `json:"home_y"`
	PrivacyX float64 `json:"privacy_x"`
	PrivacyY float64 `json:"privacy_y"`

	Privacy bool `json:"privacy"`

	// Compatibility aliases for older clients.
	Live          string `json:"live"`
	PlaybackAlias bool   `json:"playback"`
	PTZAlias      bool   `json:"ptz"`
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
// Stream URLs are stored as their raw INI values; when MediaMTX integration is
// enabled the URLs are rebuilt per request from the current credentials via
// WithMediaMTXURLs, so credential rotation takes effect without a restart.
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

	rtsp := cam.Key("live").String()
	webrtc := cam.Key("webrtc").String()
	hls := cam.Key("hls").String()

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
		},
		RTSP:           rtsp,
		WebRTC:         webrtc,
		HLS:            hls,
		Width:          cam.Key("width").MustInt(16),
		Height:         cam.Key("height").MustInt(9),
		ThinginoURL:    thingino["thingino_url"],
		ThinginoAPIKey: thingino["thingino_api_key"],
		HomeX:          toFloat(thingino["home_x"], -1),
		HomeY:          toFloat(thingino["home_y"], -1),
		PrivacyX:       toFloat(thingino["privacy_x"], -1),
		PrivacyY:       toFloat(thingino["privacy_y"], -1),
		Privacy:        false,
		Live:           rtsp,
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

// WithMediaMTXURLs returns a copy with the rtsp/webrtc/hls/live URLs rebuilt
// from the given credentials. When MediaMTX integration is disabled it returns
// the camera unchanged (the raw INI URLs). Called per request so rotated
// credentials are reflected immediately.
func (c Camera) WithMediaMTXURLs(cfg *config.Config, creds mediamtx.Creds) Camera {
	if cfg.MediaMTX == nil {
		return c
	}
	server := cfg.MediaMTX.Get("server", "localhost")
	c.RTSP = creds.RtspURL(server, cfg.MediaMTX.Get("rtsp_port", "8554"), c.ID)
	c.WebRTC = creds.WebrtcURL(server, cfg.MediaMTX.Get("webrtc_path", ""), c.ID)
	c.HLS = creds.HlsURL(server, cfg.MediaMTX.Get("hls_path", "/hls/"), c.ID)
	c.Live = c.RTSP
	return c
}
