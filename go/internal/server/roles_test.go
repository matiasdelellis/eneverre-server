package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"eneverre/internal/auth"
)

func TestRoleChangeKeepsDeviceSessionNonAdmin(t *testing.T) {
	a := withUsersApp(t)
	insertUser(t, a.db, "admin", "adminpw", auth.RoleAdmin)
	insertUser(t, a.db, "bob", "pw", auth.RoleUser)
	if _, err := a.db.Exec(
		"INSERT INTO tokens (token, username, expires_at, role) VALUES ('tv-session', 'bob', ?, 'user')",
		time.Now().Add(time.Hour).Unix(),
	); err != nil {
		t.Fatal(err)
	}

	update := func(role string) {
		t.Helper()
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, adminRequest(t, a, http.MethodPut, "/api/users/bob/role", "admin", "pw",
			`{"role":"`+role+`"}`))
		if w.Code != http.StatusOK {
			t.Fatalf("update role %s = %d %s", role, w.Code, w.Body.String())
		}
		var got string
		if err := a.db.QueryRow("SELECT role FROM tokens WHERE token = 'tv-session'").Scan(&got); err != nil {
			t.Fatal(err)
		}
		want := role
		if want == auth.RoleAdmin {
			want = auth.RoleUser // TV sessions are never admin
		}
		if got != want {
			t.Errorf("device session role after %s = %q, want %q", role, got, want)
		}
	}

	update(auth.RoleViewer)
	update(auth.RoleMonitor)
	update(auth.RoleAdmin)
}
