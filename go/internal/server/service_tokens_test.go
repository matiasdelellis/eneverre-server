package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eneverre/internal/auth"
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

func TestServiceTokenInheritsRole(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", auth.RoleAdmin)
	insertUser(t, a.db, "monitor", "pw", auth.RoleMonitor)
	insertUser(t, a.db, "viewer", "pw", auth.RoleViewer)

	monitorRaw, monitorID := createServiceToken(t, a,
		`{"name":"zabbix","username":"monitor","expires_in_days":30}`)
	viewerRaw, _ := createServiceToken(t, a,
		`{"name":"wall","username":"viewer","expires_in_days":30}`)

	// monitor sees /api/status but not cameras, events or user management.
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/status", monitorRaw, ""); w.Code != http.StatusOK {
		t.Errorf("status with monitor token = %d %s", w.Code, w.Body.String())
	}
	for _, target := range []string{"/api/cameras", "/api/camera/x/events", "/api/users"} {
		if w := serviceTokenRequest(t, a, http.MethodGet, target, monitorRaw, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s with monitor token = %d, want 403", target, w.Code)
		}
	}

	// viewer can list cameras and read events, but not status (monitor-only).
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/cameras", viewerRaw, ""); w.Code != http.StatusOK {
		t.Errorf("cameras with viewer token = %d %s", w.Code, w.Body.String())
	}
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/status", viewerRaw, ""); w.Code != http.StatusForbidden {
		t.Errorf("status with viewer token = %d, want 403", w.Code)
	}

	// The raw token is not stored: only its hash is.
	var stored string
	if err := a.db.QueryRow("SELECT token_hash FROM service_tokens WHERE id = ?", monitorID).Scan(&stored); err != nil {
		t.Fatalf("read token hash: %v", err)
	}
	if stored == monitorRaw || strings.Contains(stored, monitorRaw) {
		t.Error("raw service token was stored instead of its hash")
	}

	// The admin list exposes metadata but never the secret.
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodGet, "/api/service-tokens", "admin", "pw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), monitorRaw) || strings.Contains(w.Body.String(), viewerRaw) {
		t.Error("list response leaked a raw service token")
	}
	if !strings.Contains(w.Body.String(), `"username":"monitor"`) ||
		!strings.Contains(w.Body.String(), `"role":"monitor"`) {
		t.Errorf("list should show the linked user/role: %s", w.Body.String())
	}

	// Revocation takes effect immediately.
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodDelete, "/api/service-token/"+jsonNumber(monitorID), "admin", "pw", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/status", monitorRaw, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d, want 401", w.Code)
	}
}

func TestServiceTokenManagementIsHumanAdminOnly(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", auth.RoleAdmin)
	insertUser(t, a.db, "bob", "bobpw", auth.RoleUser)
	insertUser(t, a.db, "monitor", "pw", auth.RoleMonitor)

	// Non-admin human cannot create.
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodPost, "/api/service-tokens", "bob", "bobpw",
		`{"name":"nope","username":"bob"}`))
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin create = %d, want 403", w.Code)
	}

	// Even an admin-linked machine token cannot manage accounts/tokens.
	adminTokenRaw, _ := createServiceToken(t, a, `{"name":"automation","username":"admin"}`)
	if w := serviceTokenRequest(t, a, http.MethodGet, "/api/users", adminTokenRaw, ""); w.Code != http.StatusForbidden {
		t.Errorf("admin token users = %d, want 403", w.Code)
	}
	if w := serviceTokenRequest(t, a, http.MethodPost, "/api/service-tokens", adminTokenRaw,
		`{"name":"nested","username":"monitor"}`); w.Code != http.StatusForbidden {
		t.Errorf("admin token create token = %d, want 403", w.Code)
	}
}

func TestServiceTokenRejectsUnknownUserAndBadExpiry(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", auth.RoleAdmin)

	for _, body := range []string{
		`{"name":"x","username":"ghost"}`,
		`{"name":"x","username":"admin","expires_in_days":-1}`,
		`{"name":"","username":"admin"}`,
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
