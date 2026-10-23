package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/config"
	"eneverre/internal/mediaauth"
	"eneverre/internal/streamauth"
)

func mediaAuthTestApp(t *testing.T) *App {
	t.Helper()
	a := withUsersApp(t)
	a.cfg = &config.Config{}
	ma, err := mediaauth.NewStore(a.db)
	if err != nil {
		t.Fatalf("mediaauth.NewStore: %v", err)
	}
	a.mediaAuth = ma
	creds, err := streamauth.NewStore(a.db)
	if err != nil {
		t.Fatalf("streamauth.NewStore: %v", err)
	}
	a.creds = creds
	a.privacy = map[string]bool{}
	a.schedOff = map[string]bool{}
	a.talkCodecs = map[string][]string{}
	insertUser(t, a.db, "alice", "pw", "user")
	return a
}

func requireMedia(t *testing.T, a *App, rawURL, camID, scope string) (bool, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, rawURL, nil)
	w := httptest.NewRecorder()
	return a.requireMediaAccess(w, r, camID, scope), w
}

func TestRequireMediaAccessSignedGrants(t *testing.T) {
	a := mediaAuthTestApp(t)
	exp, sig := a.mediaAuth.Sign("front", mediaauth.ScopeLive, time.Hour)
	base := "/api/camera/front/live/stream?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + sig

	if ok, _ := requireMedia(t, a, base, "front", mediaauth.ScopeLive); !ok {
		t.Fatal("valid live grant was rejected")
	}
	if ok, w := requireMedia(t, a, base, "garden", mediaauth.ScopeLive); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("grant for front accepted for garden: ok=%v status=%d", ok, w.Code)
	}
	if ok, w := requireMedia(t, a, base, "front", mediaauth.ScopeThumb); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("live grant accepted for thumb: ok=%v status=%d", ok, w.Code)
	}
	expiredExp := time.Now().Add(-time.Second).Unix()
	expiredSig := a.mediaAuth.SignAt("front", mediaauth.ScopeLive, expiredExp)
	if ok, w := requireMedia(t, a, "/api/camera/front/live/stream?exp="+strconv.FormatInt(expiredExp, 10)+"&sig="+expiredSig, "front", mediaauth.ScopeLive); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("expired grant accepted: ok=%v status=%d", ok, w.Code)
	}
	if ok, w := requireMedia(t, a, "/api/camera/front/live/stream?exp=1", "front", mediaauth.ScopeLive); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("malformed grant accepted: ok=%v status=%d", ok, w.Code)
	}
}

func TestRequireMediaAccessFallsBackToSessionAndLegacy(t *testing.T) {
	a := mediaAuthTestApp(t)

	r := httptest.NewRequest(http.MethodGet, "/api/camera/front/live/stream", nil)
	r.SetBasicAuth("alice", "pw")
	w := httptest.NewRecorder()
	if !a.requireMediaAccess(w, r, "front", mediaauth.ScopeLive) {
		t.Fatalf("valid session was rejected: %d", w.Code)
	}

	if ok, w := requireMedia(t, a, "/api/camera/front/live/stream", "front", mediaauth.ScopeLive); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous request accepted: ok=%v status=%d", ok, w.Code)
	}

	creds := a.creds.Current()
	legacy := "/api/camera/front/live/stream?user=" + url.QueryEscape(creds.Username) + "&pass=" + url.QueryEscape(creds.Password)
	if ok, w := requireMedia(t, a, legacy, "front", mediaauth.ScopeLive); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("legacy pair accepted without opt-in: ok=%v status=%d", ok, w.Code)
	}

	a.cfg.Zaguan = config.Section{"url": "http://hub.local", "legacy_media_auth": "true"}
	if ok, _ := requireMedia(t, a, legacy, "front", mediaauth.ScopeLive); !ok {
		t.Error("legacy pair rejected with explicit opt-in")
	}
}

func TestZaguanCamerasPublishesSignedGrants(t *testing.T) {
	a := mediaAuthTestApp(t)
	a.cameras = []camera.Camera{{
		ID: "front", Name: "Front", Enabled: true,
		MSE: true, Relay: true, Record: true,
		Capabilities: camera.Capabilities{Thumbnail: true, Playback: true},
	}}

	got := a.ZaguanCameras("https://cams.example.com")
	if len(got) != 1 {
		t.Fatalf("cameras = %d", len(got))
	}
	c := got[0]
	if c.MSEURL == "" || c.SnapshotURL == "" {
		t.Fatalf("missing media links: %+v", c)
	}
	for _, raw := range []string{c.MSEURL, c.SnapshotURL} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		q := u.Query()
		if q.Get("user") != "" || q.Get("pass") != "" {
			t.Errorf("link still carries the relay password: %s", raw)
		}
		exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
		if err != nil || q.Get("sig") == "" {
			t.Errorf("link is not a signed grant: %s", raw)
		}
		if exp <= time.Now().Unix() {
			t.Errorf("grant already expired: %s", raw)
		}
	}

	// A paused camera publishes no live or still URL; the grant would work
	// against the handler otherwise and bypass privacy/schedule.
	a.privacy["front"] = true
	c = a.ZaguanCameras("https://cams.example.com")[0]
	if c.MSEURL != "" || c.SnapshotURL != "" || c.RTSP != "" {
		t.Errorf("paused camera leaked media links: %+v", c)
	}

	// Without a signer and without the legacy opt-in, omit the links instead
	// of falling back to a reusable password.
	a.mediaAuth = nil
	a.privacy["front"] = false
	c = a.ZaguanCameras("https://cams.example.com")[0]
	if c.MSEURL != "" || c.SnapshotURL != "" {
		t.Errorf("no signer still published a media link: %+v", c)
	}

	a.cfg.Zaguan = config.Section{"url": "http://hub.local", "legacy_media_auth": "true"}
	c = a.ZaguanCameras("https://cams.example.com")[0]
	u, _ := url.Parse(c.MSEURL)
	if u.Query().Get("user") == "" || u.Query().Get("pass") == "" {
		t.Errorf("legacy opt-in did not publish the pair: %s", c.MSEURL)
	}
}
