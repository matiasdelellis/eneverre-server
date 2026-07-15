package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"eneverre/internal/auth"
	"eneverre/internal/config"
	"eneverre/internal/store"
)

// withUsersApp builds an App backed by a fresh temp-file SQLite database with
// the real schema applied but no seeded users, so each test controls exactly
// which accounts (and passwords) exist. Handlers authenticate these tests via
// HTTP Basic (auth.VerifyBasic), which is why the inserted passwords are known.
func withUsersApp(t *testing.T) *App {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Init(db); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	// Drop the auto-seeded admin (random password) so tests start from a known,
	// empty user set.
	if _, err := db.Exec("DELETE FROM users"); err != nil {
		t.Fatalf("clear users: %v", err)
	}
	if _, err := db.Exec("DELETE FROM tokens"); err != nil {
		t.Fatalf("clear tokens: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &App{db: db, cfg: &config.Config{}}
}

func insertUser(t *testing.T, db *sql.DB, username, password, role string) {
	t.Helper()
	if _, err := db.Exec(
		"INSERT INTO users (username, password, role, must_change_password) VALUES (?, ?, ?, 0)",
		username, auth.GeneratePasswordHash(password), role,
	); err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
}

func adminRequest(t *testing.T, method, target, admin, pass, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.SetBasicAuth(admin, pass)
	return r
}

func TestCleanUsername(t *testing.T) {
	ok := []struct{ in, want string }{
		{"admin", "admin"},
		{"  spaced  ", "spaced"}, // surrounding whitespace trimmed
		{"user.name+tag@example", "user.name+tag@example"},
	}
	for _, tc := range ok {
		got, valid := cleanUsername(tc.in)
		if !valid || got != tc.want {
			t.Errorf("cleanUsername(%q) = (%q, %v); want (%q, true)", tc.in, got, valid, tc.want)
		}
	}
	bad := []string{"", "   ", "has space", "tab\tinside", "line\nbreak", strings.Repeat("x", maxUsernameLen+1)}
	for _, in := range bad {
		if got, valid := cleanUsername(in); valid {
			t.Errorf("cleanUsername(%q) = (%q, true); want rejected", in, got)
		}
	}
}

func TestIsLastAdmin(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "root", "pw", "admin")

	if !a.isLastAdmin("root") {
		t.Error("single admin should be the last admin")
	}
	if a.isLastAdmin("ghost") {
		t.Error("nonexistent user must not be reported as last admin")
	}

	insertUser(t, a.db, "bob", "pw", "user")
	if a.isLastAdmin("bob") {
		t.Error("a non-admin is never the last admin")
	}
	if !a.isLastAdmin("root") {
		t.Error("still the only admin with a plain user present")
	}

	insertUser(t, a.db, "root2", "pw", "admin")
	if a.isLastAdmin("root") {
		t.Error("with two admins, neither is the last admin")
	}
}

func TestCreateUserValidation(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty password rejected", `{"username":"newuser","password":""}`, http.StatusUnprocessableEntity},
		{"whitespace username rejected", `{"username":"bad name","password":"x"}`, http.StatusUnprocessableEntity},
		{"empty username rejected", `{"username":"   ","password":"x"}`, http.StatusUnprocessableEntity},
		{"valid create", `{"username":"newuser","password":"secret"}`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.handleCreateUser(w, adminRequest(t, http.MethodPost, "/api/users", "admin", "adminpw", tc.body))
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestLastAdminProtection(t *testing.T) {
	t.Run("cannot delete last admin", func(t *testing.T) {
		a := withUsersApp(t)
		insertUser(t, a.db, "admin", "adminpw", "admin")
		w := httptest.NewRecorder()
		r := adminRequest(t, http.MethodDelete, "/api/users/admin", "admin", "adminpw", "")
		r.SetPathValue("username", "admin")
		a.handleDeleteUser(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		var n int
		_ = a.db.QueryRow("SELECT COUNT(*) FROM users WHERE username='admin'").Scan(&n)
		if n != 1 {
			t.Error("last admin was deleted despite the guard")
		}
	})

	t.Run("cannot demote last admin", func(t *testing.T) {
		a := withUsersApp(t)
		insertUser(t, a.db, "admin", "adminpw", "admin")
		w := httptest.NewRecorder()
		r := adminRequest(t, http.MethodPut, "/api/users/admin/role", "admin", "adminpw", `{"role":"user"}`)
		r.SetPathValue("username", "admin")
		a.handleUpdateRole(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		var role string
		_ = a.db.QueryRow("SELECT role FROM users WHERE username='admin'").Scan(&role)
		if role != "admin" {
			t.Errorf("last admin demoted to %q despite the guard", role)
		}
	})

	t.Run("second admin can be deleted", func(t *testing.T) {
		a := withUsersApp(t)
		insertUser(t, a.db, "admin", "adminpw", "admin")
		insertUser(t, a.db, "admin2", "pw", "admin")
		w := httptest.NewRecorder()
		r := adminRequest(t, http.MethodDelete, "/api/users/admin2", "admin", "adminpw", "")
		r.SetPathValue("username", "admin2")
		a.handleDeleteUser(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
	})
}

func TestAdminPasswordChangeRevokesSessions(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", "admin")
	insertUser(t, a.db, "bob", "bobpw", "user")
	// Two live sessions for bob that must not survive the reset.
	for _, tok := range []string{"tok1", "tok2"} {
		if _, err := a.db.Exec(
			"INSERT INTO tokens (token, username, expires_at, created_at) VALUES (?, 'bob', 9999999999, 0)", tok,
		); err != nil {
			t.Fatalf("insert token: %v", err)
		}
	}
	w := httptest.NewRecorder()
	r := adminRequest(t, http.MethodPut, "/api/users/bob/password", "admin", "adminpw", `{"password":"newpw"}`)
	r.SetPathValue("username", "bob")
	a.handleChangePassword(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var n int
	_ = a.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE username='bob'").Scan(&n)
	if n != 0 {
		t.Errorf("bob still has %d sessions after admin password reset; want 0", n)
	}
}

func TestRefreshRotationIsSingleUse(t *testing.T) {
	a := withUsersApp(t)
	a.accessTTL = 3600
	a.refreshTTL = 86400
	insertUser(t, a.db, "bob", "bobpw", "user")
	if _, err := a.db.Exec(
		"INSERT INTO tokens (token, username, expires_at, created_at, refresh_token, refresh_expires_at) "+
			"VALUES ('acc', 'bob', 9999999999, 0, 'refresh-me', 9999999999)",
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	do := func() int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(`{"refresh_token":"refresh-me"}`))
		a.handleRefresh(w, r)
		return w.Code
	}

	if code := do(); code != http.StatusOK {
		t.Fatalf("first refresh = %d, want 200", code)
	}
	// The original refresh token has been consumed; presenting it again must fail
	// rather than mint a second pair.
	if code := do(); code != http.StatusUnauthorized {
		t.Errorf("reused refresh token = %d, want 401", code)
	}
	var newRefresh sql.NullString
	_ = a.db.QueryRow("SELECT refresh_token FROM tokens WHERE username='bob'").Scan(&newRefresh)
	if !newRefresh.Valid || newRefresh.String == "refresh-me" {
		t.Errorf("refresh token not rotated: %v", newRefresh)
	}
	// Exactly one session row survives (rotation is in place, not append).
	var rows int
	_ = a.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE username='bob'").Scan(&rows)
	if rows != 1 {
		t.Errorf("session rows = %d, want 1", rows)
	}
}
