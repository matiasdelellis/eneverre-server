package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type refreshTestPair struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}

func loginForRefreshTest(t *testing.T, a *App) refreshTestPair {
	t.Helper()
	body := `{"username":"alice","password":"pw"}`
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body.String())
	}
	var out refreshTestPair
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return out
}

func refreshForTest(t *testing.T, a *App, refresh string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"refresh_token":"` + refresh + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestRefreshReuseDetection(t *testing.T) {
	a := withUsersApp(t)
	a.accessTTL = 3600
	a.refreshTTL = 86400
	insertUser(t, a.db, "alice", "pw", "user")

	first := loginForRefreshTest(t, a)

	// First rotation: R1 -> R2.
	w := refreshForTest(t, a, first.RefreshToken)
	if w.Code != http.StatusOK {
		t.Fatalf("first refresh = %d: %s", w.Code, w.Body.String())
	}
	var second refreshTestPair
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode first refresh: %v", err)
	}

	// An immediate replay of R1 is the retry path (lost response): refused,
	// but the freshly rotated R2 must stay usable.
	if w := refreshForTest(t, a, first.RefreshToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("immediate replay = %d, want 401", w.Code)
	}
	w = refreshForTest(t, a, second.RefreshToken)
	if w.Code != http.StatusOK {
		t.Fatalf("R2 after immediate replay = %d: %s", w.Code, w.Body.String())
	}
	var third refreshTestPair
	if err := json.Unmarshal(w.Body.Bytes(), &third); err != nil {
		t.Fatalf("decode second refresh: %v", err)
	}

	// A replay after the grace window is theft: revoke the session, so the
	// attacker's rotated token dies too.
	if _, err := a.db.Exec(
		"UPDATE tokens SET refresh_prev_used_at = ? WHERE refresh_prev = ?",
		1, second.RefreshToken,
	); err != nil {
		t.Fatalf("age the rotated token: %v", err)
	}
	if w := refreshForTest(t, a, second.RefreshToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("late replay = %d, want 401", w.Code)
	}
	if w := refreshForTest(t, a, third.RefreshToken); w.Code != http.StatusUnauthorized {
		t.Fatalf("session survived a late replay: %d", w.Code)
	}
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE username = 'alice'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tokens left after reuse revocation = %d", n)
	}
}
