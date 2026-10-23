package server

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"eneverre/internal/media"
	"eneverre/internal/streamauth"
	"eneverre/internal/zaguan"
)

// ZaguanCameras returns the camera inventory published to a Zaguán hub. The
// source is one per server: each camera becomes a device with a `camera`
// entity.
//
// publicURL is [zaguan] public_url, the base the hub's clients use to reach
// this server. It is optional: without it the entity attributes carry no
// media links, and the RTSP relay URL is still built when [media] rtsp_host
// is set. The privacy/schedule pause, the engine status and the rotating
// relay credentials are applied here exactly as they are for /api/cameras, so
// the hub sees the same state the web UI does.
func (a *App) ZaguanCameras(publicURL string) []zaguan.Camera {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if u, err := url.Parse(base); err != nil || !u.IsAbs() || u.Host == "" {
		base = ""
	}

	// The relay URL needs a host the camera clients can reach. Prefer the
	// explicit [media] rtsp_host; otherwise use the host of public_url.
	host := a.cfg.Media.Get("rtsp_host", "")
	if host == "" {
		host = hostFromPublicURL(base)
	}

	// The static capabilities (a configured live feed, recording enabled) are
	// resolved from the same global/per-camera switches the engine uses, so
	// they do not flap with connectivity or privacy.
	def := media.DefaultOptions()
	gMSE, gRelay, gRecord := def.MSEEnabled, def.RelayEnabled, def.RecordEnabled
	engine := a.heartbeatEngine()
	status := map[string]media.CameraStatus{}
	if engine != nil {
		gMSE, gRelay, gRecord = engine.GlobalToggles()
		for _, s := range engine.Status() {
			status[s.ID] = s
		}
	}

	// The media endpoints need credentials the hub's web client can use
	// without an Eneverre login. The rotating relay pair is already handed to
	// clients in the RTSP URL, so carrying it as query parameters gives MSE,
	// HLS and thumbnails a single, rotation-aware auth story.
	var creds streamauth.Creds
	if a.creds != nil {
		creds = a.creds.Current()
	}
	mediaURL := func(path string, extra url.Values) string {
		if base == "" {
			return ""
		}
		q := url.Values{}
		if creds.Username != "" {
			q.Set("user", creds.Username)
			q.Set("pass", creds.Password)
		}
		for k, v := range extra {
			q[k] = v
		}
		if len(q) == 0 {
			return base + path
		}
		return base + path + "?" + q.Encode()
	}

	cams := a.Cameras()
	out := make([]zaguan.Camera, 0, len(cams))
	for _, c := range cams {
		view := a.publicCamera(c, host)
		st := status[c.ID]
		features := c.ResolveFeatures(gMSE, gRelay, gRecord)
		camPath := "/api/camera/" + url.PathEscape(view.ID)

		zc := zaguan.Camera{
			ID:          view.ID,
			Name:        view.Name,
			Comment:     view.Comment,
			Location:    view.Location,
			Enabled:     view.Enabled,
			Privacy:     view.Privacy,
			ScheduleOff: view.ScheduleOff,
			Connected:   st.Connected && !st.Paused && view.Enabled,
			Live:        features.MSE || features.Relay,
			Recording:   st.Recording,
			Record:      features.Record,
			Width:       view.Width,
			Height:      view.Height,
			RTSP:        view.RTSP,
			Snapshot:    view.Capabilities.Thumbnail,
			Playback:    view.Capabilities.Playback,
			Talk:        view.Capabilities.Talk,
			PTZ:         view.Capabilities.PTZ,
		}
		if features.MSE {
			zc.MSEURL = mediaURL(camPath+"/live/stream", nil)
			zc.MSEInfoURL = mediaURL(camPath+"/live/info", nil)
		}
		if view.Capabilities.Thumbnail {
			zc.SnapshotURL = mediaURL(camPath+"/thumbnail", nil)
		}
		if view.Capabilities.Playback && engine != nil {
			// HLS is VOD (the engine has no HLS live mode). Publish the last
			// hour of the camera's timeline so a web player can open a
			// ready-to-play playlist; a player that wants another window
			// calls the same endpoint with its own start/end.
			if idx := engine.Index(); idx != nil {
				if tl, err := idx.Timeline(view.ID); err == nil && tl.Count > 0 {
					start := tl.End.Add(-time.Hour)
					if start.Before(tl.Start) {
						start = tl.Start
					}
					q := url.Values{}
					q.Set("start", start.UTC().Format(time.RFC3339Nano))
					q.Set("end", tl.End.UTC().Format(time.RFC3339Nano))
					zc.HLSURL = mediaURL(camPath+"/recordings/hls/playlist.m3u8", q)
				}
			}
		}
		out = append(out, zc)
	}
	return out
}

// CaptureSnapshot fetches a fresh still from one camera. It is the action
// behind the hub's `snapshot` command: the Source API has no binary result,
// so the point of the command is to make the camera take a new image now; a
// camera card that wants the bytes asks this server for the thumbnail.
func (a *App) CaptureSnapshot(ctx context.Context, id string) error {
	cam, ok := a.getCamera(id)
	if !ok {
		return fmt.Errorf("camera %q not found", id)
	}
	if !cam.Enabled {
		return fmt.Errorf("camera %q is disabled", id)
	}
	_, _, err := a.cameraSnapshot(ctx, cam)
	return err
}

// hostFromPublicURL returns the host (with its port, if any) of an absolute
// base URL. An empty or relative value yields "".
func hostFromPublicURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
