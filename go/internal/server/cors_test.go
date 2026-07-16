package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	call := func(allowed []string, method, origin string) *httptest.ResponseRecorder {
		h := cors(next, allowed)
		r := httptest.NewRequest(method, "/api/cameras", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	t.Run("empty allowlist reflects any origin", func(t *testing.T) {
		w := call(nil, http.MethodGet, "https://evil.example")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://evil.example" {
			t.Errorf("ACAO = %q, want the reflected origin", got)
		}
		if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Error("credentials header not set on permissive default")
		}
	})

	t.Run("empty allowlist, no origin -> wildcard", func(t *testing.T) {
		w := call(nil, http.MethodGet, "")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("ACAO = %q, want *", got)
		}
	})

	t.Run("allowlist permits a listed origin", func(t *testing.T) {
		w := call([]string{"https://app.example"}, http.MethodGet, "https://app.example")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("ACAO = %q, want the listed origin", got)
		}
	})

	t.Run("allowlist blocks an unlisted origin", func(t *testing.T) {
		w := call([]string{"https://app.example"}, http.MethodGet, "https://evil.example")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("ACAO = %q, want empty (blocked)", got)
		}
	})

	t.Run("star entry reflects any origin", func(t *testing.T) {
		w := call([]string{"*"}, http.MethodGet, "https://anything.example")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.example" {
			t.Errorf("ACAO = %q, want reflected", got)
		}
	})

	t.Run("preflight OPTIONS short-circuits with 200", func(t *testing.T) {
		w := call([]string{"https://app.example"}, http.MethodOptions, "https://app.example")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if w.Header().Get("Access-Control-Allow-Methods") == "" {
			t.Error("preflight for an allowed origin should set Allow-Methods")
		}
	})

	t.Run("preflight OPTIONS for blocked origin sets no CORS headers", func(t *testing.T) {
		w := call([]string{"https://app.example"}, http.MethodOptions, "https://evil.example")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("blocked origin must not receive Allow-Origin on preflight")
		}
	})
}
