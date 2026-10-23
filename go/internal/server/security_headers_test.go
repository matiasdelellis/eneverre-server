package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := securityHeaders(next)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, header := range []string{
		"Content-Security-Policy",
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Referrer-Policy",
		"Permissions-Policy",
	} {
		if w.Header().Get(header) == "" {
			t.Errorf("missing %s", header)
		}
	}
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS on a plain-HTTP request")
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("missing HSTS behind a TLS proxy")
	}
}

func TestNoStoreAPICoversSecretsButNotBuilds(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := noStoreAPI(next)

	for _, path := range []string{"/api/cameras", "/api/camera/front/config", "/api/auth/login"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q", path, got)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/app/updates/tv/app.apk", nil))
	if got := w.Header().Get("Cache-Control"); got != "" {
		t.Errorf("build download Cache-Control = %q, want empty", got)
	}
}
