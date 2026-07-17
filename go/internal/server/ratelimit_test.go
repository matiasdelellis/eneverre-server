package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthThrottleBlocksAfterUserCap(t *testing.T) {
	th := newAuthThrottle()
	for i := 0; i < maxFailsPerUser; i++ {
		if blocked, _ := th.blocked("10.0.0.1", "alice"); blocked {
			t.Fatalf("blocked after only %d failures", i)
		}
		th.fail("10.0.0.1", "alice")
	}
	if blocked, wait := th.blocked("10.0.0.1", "alice"); !blocked || wait <= 0 {
		t.Fatalf("expected block with positive wait, got blocked=%v wait=%v", blocked, wait)
	}
	// Same username from another IP is still blocked (per-user cap)...
	if blocked, _ := th.blocked("10.0.0.2", "alice"); !blocked {
		t.Fatal("per-user cap should block regardless of IP")
	}
	// ...but another username from the second IP is fine.
	if blocked, _ := th.blocked("10.0.0.2", "bob"); blocked {
		t.Fatal("unrelated ip+user should not be blocked")
	}
	// A success clears the username's slate.
	th.success("alice")
	if blocked, _ := th.blocked("10.0.0.2", "alice"); blocked {
		t.Fatal("success should clear the per-user strikes")
	}
}

func TestAuthThrottleBlocksAfterIPCap(t *testing.T) {
	th := newAuthThrottle()
	for i := 0; i < maxFailsPerIP; i++ {
		th.fail("10.0.0.9", "user"+string(rune('a'+i%26)))
	}
	if blocked, _ := th.blocked("10.0.0.9", "someone-new"); !blocked {
		t.Fatal("per-IP cap should block even for fresh usernames")
	}
	if blocked, _ := th.blocked("10.0.0.10", "someone-new"); blocked {
		t.Fatal("other IPs must not be affected")
	}
}

func TestNilAuthThrottleIsNoop(t *testing.T) {
	var th *authThrottle
	th.fail("1.2.3.4", "x")
	th.success("x")
	if blocked, _ := th.blocked("1.2.3.4", "x"); blocked {
		t.Fatal("nil throttle must never block")
	}
}

func TestLoginThrottled(t *testing.T) {
	a := withUsersApp(t)
	a.authThrottle = newAuthThrottle()
	insertUser(t, a.db, "alice", "correct-horse", "admin")

	body := `{"username":"alice","password":"wrong"}`
	var lastCode int
	// Each bad login burns one PBKDF2 pass, so this test costs ~1s of CPU —
	// acceptable for the coverage (the cap must kick in at exactly the limit).
	for i := 0; i < maxFailsPerUser; i++ {
		w := httptest.NewRecorder()
		a.handleLogin(w, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(body)))
		lastCode = w.Code
	}
	if lastCode != 401 {
		t.Fatalf("failed logins under the cap should be 401, got %d", lastCode)
	}
	w := httptest.NewRecorder()
	a.handleLogin(w, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(body)))
	if w.Code != 429 {
		t.Fatalf("expected 429 once throttled, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	// The correct password is throttled too (that's the point: no oracle), and
	// no PBKDF2 pass is spent on it.
	w = httptest.NewRecorder()
	a.handleLogin(w, httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"username":"alice","password":"correct-horse"}`)))
	if w.Code != 429 {
		t.Fatalf("expected 429 for correct password while throttled, got %d", w.Code)
	}
}

func TestBasicAuthThrottled(t *testing.T) {
	a := withUsersApp(t)
	a.authThrottle = newAuthThrottle()
	insertUser(t, a.db, "bob", "hunter2", "admin")

	for i := 0; i < maxFailsPerUser; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/users", nil)
		r.SetBasicAuth("bob", "nope")
		if a.requireUser(w, r) != nil {
			t.Fatal("bad password must not authenticate")
		}
		if w.Code != 401 {
			t.Fatalf("expected 401 under the cap, got %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/users", nil)
	r.SetBasicAuth("bob", "hunter2")
	if a.requireUser(w, r) != nil {
		t.Fatal("throttled request must not authenticate")
	}
	if w.Code != 429 {
		t.Fatalf("expected 429 once throttled, got %d", w.Code)
	}
	// Bearer traffic is unaffected by a Basic throttle on the same source.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/users", nil)
	if a.requireUser(w, r) != nil {
		t.Fatal("no credentials must not authenticate")
	}
	if w.Code != 401 {
		t.Fatalf("credential-less request should still get a plain 401, got %d", w.Code)
	}
}
