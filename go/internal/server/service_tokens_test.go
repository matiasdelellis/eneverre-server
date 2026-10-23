package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serviceTokenRequest sends a request authenticated with a raw service token.
func serviceTokenRequest(t *testing.T, a *App, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func createServiceToken(t *testing.T, a *App, body string) (raw string, id int64) {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodPost, "/api/service-tokens", "admin", "pw", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create service token = %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if resp.ID == 0 || !strings.HasPrefix(resp.Token, "envr_svc_") {
		t.Fatalf("create response = %s", w.Body.String())
	}
	return resp.Token, resp.ID
}

func TestServiceTokenLifecycleAndScope(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")
	raw, id := createServiceToken(t, a, `{"name":"zabbix","scopes":["status:read"],"expires_in_days":30}`)

	// The raw token is not stored: only its hash is.
	var stored string
	if err := a.db.QueryRow("SELECT token_hash FROM service_tokens WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatalf("read token hash: %v", err)
	}
	if stored == raw || strings.Contains(stored, raw) {
		t.Error("raw service token was stored instead of its hash")
	}

	// status:read reaches /api/status.
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/status", raw, ""); w.Code != http.StatusOK {
		t.Errorf("status with status:read = %d %s", w.Code, w.Body.String())
	}
	// It does not reach a different read surface or a human endpoint.
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/cameras", raw, ""); w.Code != http.StatusForbidden {
		t.Errorf("cameras with status:read = %d, want 403", w.Code)
	}
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/users", raw, ""); w.Code != http.StatusForbidden {
		t.Errorf("users with service token = %d, want 403", w.Code)
	}

	// A camera-read token reaches /api/cameras but not /api/status.
	camRaw, _ := createServiceToken(t, a, `{"name":"dashboard","scopes":["cameras:read"],"expires_in_days":30}`)
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/cameras", camRaw, ""); w.Code != http.StatusOK {
		t.Errorf("cameras with cameras:read = %d %s", w.Code, w.Body.String())
	}
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/status", camRaw, ""); w.Code != http.StatusForbidden {
		t.Errorf("status with cameras:read = %d, want 403", w.Code)
	}

	// The admin list exposes metadata but never the secret.
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/service-tokens", "admin", "pw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), raw) || strings.Contains(w.Body.String(), camRaw) {
		t.Error("list response leaked a raw service token")
	}

	// Revocation takes effect immediately.
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodDelete, "/api/service-token/"+jsonNumber(id), "admin", "pw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/status", raw, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d, want 401", w.Code)
	}
}

func TestServiceTokenManagementIsAdminOnly(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")
	insertUser(t, a.db, "bob", "bobpw", "user")

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodPost, "/api/service-tokens", "bob", "bobpw",
		`{"name":"nope","scopes":["status:read"]}`))
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin create = %d, want 403", w.Code)
	}
}

func TestServiceTokenRejectsUnknownScopeAndBadExpiry(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")

	for _, body := range []string{
		`{"name":"x","scopes":["admin:write"]}`,
		`{"name":"x","scopes":["status:read"],"expires_in_days":-1}`,
		`{"name":"","scopes":["status:read"]}`,
	} {
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodPost, "/api/service-tokens", "admin", "pw", body))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s = %d, want 422 (%s)", body, w.Code, w.Body.String())
		}
	}
}

// jsonNumber avoids importing strconv just for the path id.
func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
