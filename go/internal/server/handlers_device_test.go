package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// deviceFlowApp builds an App with one user and runs GET /api/auth/device,
// returning the app and the issued codes.
func deviceFlowApp(t *testing.T) (*App, string, string) {
	t.Helper()
	a := withUsersApp(t)
	a.accessTTL = 3600
	a.refreshTTL = 30 * 24 * 3600
	insertUser(t, a.db, "alice", "pw", "admin")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/device", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("create device = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return a, out.DeviceCode, out.UserCode
}

func deviceStatus(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var out struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out.Status, out.Token
}

func TestDeviceFlowApprovesOnce(t *testing.T) {
	a, deviceCode, userCode := deviceFlowApp(t)

	verify := func() string {
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, adminRequest(t, a, "POST", "/api/auth/device/verify", "alice", "pw", `{"user_code":"`+userCode+`"}`))
		st, _ := deviceStatus(t, w)
		return st
	}
	if st := verify(); st != "approved" {
		t.Fatalf("first verify = %q, want approved", st)
	}
	if st := verify(); st != "expired" {
		t.Fatalf("second verify = %q, want expired (already approved)", st)
	}

	// Many concurrent polls on the approved code: exactly one gets a token.
	var mu sync.Mutex
	tokens := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/device/"+deviceCode, nil))
			if _, tok := deviceStatus(t, w); tok != "" {
				mu.Lock()
				tokens++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if tokens != 1 {
		t.Errorf("concurrent polls minted %d tokens, want exactly 1", tokens)
	}
	var n int
	_ = a.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE username = 'alice' AND token NOT LIKE 'test-%'").Scan(&n)
	if n != 1 {
		t.Errorf("tokens table has %d rows for alice, want 1", n)
	}
}

func TestDeviceSessionIsRenewableAndScopedToUser(t *testing.T) {
	a, deviceCode, userCode := deviceFlowApp(t)

	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, adminRequest(t, a, "POST", "/api/auth/device/verify", "alice", "pw", `{"user_code":"`+userCode+`"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/device/"+deviceCode, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("poll = %d: %s", w.Code, w.Body.String())
	}
	var approved struct {
		Status           string `json:"status"`
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		RefreshExpiresAt int64  `json:"refresh_expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &approved); err != nil {
		t.Fatalf("decode approved: %v", err)
	}
	if approved.Status != "approved" || approved.Token == "" {
		t.Fatalf("approved = %+v", approved)
	}
	// The refresh token is additive: old TVs ignore it and keep re-pairing;
	// new ones can renew without a new pairing code.
	if approved.RefreshToken == "" || approved.RefreshExpiresAt <= time.Now().Unix() {
		t.Fatalf("device refresh token missing/expired: %+v", approved)
	}

	// The device token is pinned to the ordinary "user" role even though the
	// approving account is an admin.
	usersReq := httptest.NewRequest("GET", "/api/users", nil)
	usersReq.Header.Set("Authorization", "Bearer "+approved.Token)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, usersReq)
	if w.Code != http.StatusForbidden {
		t.Errorf("device token reached /api/users: status %d, want 403", w.Code)
	}

	camsReq := httptest.NewRequest("GET", "/api/cameras", nil)
	camsReq.Header.Set("Authorization", "Bearer "+approved.Token)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, camsReq)
	if w.Code != http.StatusOK {
		t.Errorf("device token cannot list cameras: status %d: %s", w.Code, w.Body.String())
	}

	// The refresh token renews the same device session.
	body := `{"refresh_token":"` + approved.RefreshToken + `"}`
	refreshReq := httptest.NewRequest("POST", "/api/auth/refresh", strings.NewReader(body))
	refreshReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, refreshReq)
	if w.Code != http.StatusOK {
		t.Fatalf("device refresh = %d: %s", w.Code, w.Body.String())
	}
	var renewed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &renewed); err != nil || renewed.Token == "" {
		t.Fatalf("decode refresh: %v (%s)", err, w.Body.String())
	}

	// ... and the renewed token keeps the restricted role.
	usersReq = httptest.NewRequest("GET", "/api/users", nil)
	usersReq.Header.Set("Authorization", "Bearer "+renewed.Token)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, usersReq)
	if w.Code != http.StatusForbidden {
		t.Errorf("renewed device token reached /api/users: status %d, want 403", w.Code)
	}
}

func TestDeviceCreateIsRateLimited(t *testing.T) {
	a := withUsersApp(t)
	a.deviceCreateLimit = newWindowLimiter(deviceCreatesPerWindow, 5*time.Minute)
	code := func(remote string) int {
		r := httptest.NewRequest("GET", "/api/auth/device", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < deviceCreatesPerWindow; i++ {
		if c := code("198.51.100.1:1000"); c != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 under the cap", i, c)
		}
	}
	if c := code("198.51.100.1:1000"); c != http.StatusTooManyRequests {
		t.Fatalf("over the cap = %d, want 429", c)
	}
	if c := code("198.51.100.2:1000"); c != http.StatusOK {
		t.Fatalf("another client = %d, want 200", c)
	}
}
