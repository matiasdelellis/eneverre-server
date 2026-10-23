package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eneverre/internal/auth"
)

func insertUserMustChange(t *testing.T, a *App, username, password, role string) {
	t.Helper()
	if _, err := a.db.Exec(
		"INSERT INTO users (username, password, role, must_change_password) VALUES (?, ?, ?, 1)",
		username, auth.GeneratePasswordHash(password), role,
	); err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
}

func TestMustChangePasswordIsEnforcedAtTheAPI(t *testing.T) {
	a := withUsersApp(t)
	insertUserMustChange(t, a, "bob", "oldpw", "user")

	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := adminRequest(t, a, method, path, "bob", "oldpw", body)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}

	// Everything except the change/logout/session endpoints is blocked, even
	// though the session token is valid.
	w := request(http.MethodGet, "/api/cameras", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("flagged user reached /api/cameras: %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"password_change_required"`) {
		t.Errorf("missing machine code: %s", w.Body.String())
	}

	// The change itself is allowed and clears the gate.
	w = request(http.MethodPut, "/api/users/me/password",
		`{"current_password":"oldpw","new_password":"newpw"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change password = %d: %s", w.Code, w.Body.String())
	}

	w = request(http.MethodGet, "/api/cameras", "")
	if w.Code != http.StatusOK {
		t.Fatalf("after change /api/cameras = %d: %s", w.Code, w.Body.String())
	}
}
