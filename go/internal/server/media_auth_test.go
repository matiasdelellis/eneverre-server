package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"eneverre/internal/config"
	"eneverre/internal/mediaauth"
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

func TestRequireMediaAccessFallsBackToSession(t *testing.T) {
	a := mediaAuthTestApp(t)

	r := adminRequest(t, a, http.MethodGet, "/api/camera/front/live/stream", "alice", "pw", "")
	w := httptest.NewRecorder()
	if !a.requireMediaAccess(w, r, "front", mediaauth.ScopeLive) {
		t.Fatalf("valid session was rejected: %d", w.Code)
	}

	if ok, w := requireMedia(t, a, "/api/camera/front/live/stream", "front", mediaauth.ScopeLive); ok || w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous request accepted: ok=%v status=%d", ok, w.Code)
	}
}
