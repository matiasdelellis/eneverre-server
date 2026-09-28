package server

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// failN records n failed attempts for ip+user through the public API.
func failN(t *testing.T, th *authThrottle, n int, ip, user func(i int) string) {
	t.Helper()
	for i := 0; i < n; i++ {
		release, _, ok := th.begin(ip(i), user(i))
		if !ok {
			t.Fatalf("attempt %d refused before reaching the cap", i)
		}
		release(true)
	}
}

func fixed(s string) func(int) string { return func(int) string { return s } }

func blockedFor(th *authThrottle, ip, user string) bool {
	release, _, ok := th.begin(ip, user)
	if ok {
		release(false)
	}
	return !ok
}

func TestAuthThrottleBlocksAfterUserCap(t *testing.T) {
	th := newAuthThrottle()
	failN(t, th, maxFailsPerUser, fixed("10.0.0.1"), fixed("alice"))
	release, wait, ok := th.begin("10.0.0.1", "alice")
	if ok || wait <= 0 {
		if ok {
			release(false)
		}
		t.Fatalf("expected block with positive wait, got ok=%v wait=%v", ok, wait)
	}
	// Another failing source is blocked for the same username too...
	failN(t, th, 1, fixed("10.0.0.2"), fixed("bob"))
	if !blockedFor(th, "10.0.0.2", "alice") {
		t.Fatal("per-user cap should block a source that is itself failing")
	}
	// ...but another username from that source is fine.
	if blockedFor(th, "10.0.0.2", "carol") {
		t.Fatal("unrelated ip+user should not be blocked")
	}
}

// The per-username cap must not let an attacker lock the real user out: a
// source with no failures of its own still gets through, and its success
// clears the username's slate.
func TestAuthThrottleUserCapSparesCleanSource(t *testing.T) {
	th := newAuthThrottle()
	failN(t, th, maxFailsPerUser, func(i int) string { return "203.0.113." + strconv.Itoa(i) }, fixed("admin"))
	release, _, ok := th.begin("198.51.100.20", "admin")
	if !ok {
		t.Fatal("a clean source was locked out by other sources' failures")
	}
	// While that attempt is in flight the clean source counts as busy, so a
	// parallel guess from it is held back.
	if !blockedFor(th, "198.51.100.20", "admin") {
		t.Fatal("a second concurrent attempt on a capped account should wait")
	}
	release(false) // correct password
	if blockedFor(th, "203.0.113.1", "admin") {
		t.Fatal("success should clear the per-user strikes")
	}
}

func TestAuthThrottleBlocksAfterIPCap(t *testing.T) {
	th := newAuthThrottle()
	failN(t, th, maxFailsPerIP, fixed("10.0.0.9"), func(i int) string { return "user" + strconv.Itoa(i) })
	if !blockedFor(th, "10.0.0.9", "someone-new") {
		t.Fatal("per-IP cap should block even for fresh usernames")
	}
	if blockedFor(th, "10.0.0.10", "someone-new") {
		t.Fatal("other IPs must not be affected")
	}
}

// Attempts in flight count against the cap: a burst of concurrent requests
// must not all pass the check before the first failure is recorded.
func TestAuthThrottleCountsInflight(t *testing.T) {
	th := newAuthThrottle()
	var releases []func(bool)
	for i := 0; i < maxFailsPerIP; i++ {
		release, _, ok := th.begin("10.0.0.5", "u"+strconv.Itoa(i))
		if !ok {
			t.Fatalf("attempt %d refused under the cap", i)
		}
		releases = append(releases, release)
	}
	if !blockedFor(th, "10.0.0.5", "another") {
		t.Fatal("concurrent attempts beyond the cap were admitted")
	}
	for _, rel := range releases {
		rel(false)
	}
	if blockedFor(th, "10.0.0.5", "another") {
		t.Fatal("released attempts must free their slots")
	}
}

func TestNilAuthThrottleIsNoop(t *testing.T) {
	var th *authThrottle
	release, _, ok := th.begin("1.2.3.4", "x")
	if !ok {
		t.Fatal("nil throttle must never block")
	}
	release(true)
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

// Behind the same-host proxy every request arrives from 127.0.0.1, so the
// per-IP cap must key on the forwarded client — otherwise one attacker's
// failures lock every user out.
func TestLoginThrottleKeysOnForwardedClient(t *testing.T) {
	a := withUsersApp(t)
	a.authThrottle = newAuthThrottle()
	insertUser(t, a.db, "alice", "correct-horse", "admin")

	login := func(client, user, pass string) int {
		r := httptest.NewRequest("POST", "/api/auth/login",
			strings.NewReader(`{"username":"`+user+`","password":"`+pass+`"}`))
		r.RemoteAddr = "127.0.0.1:40000" // the proxy
		r.Header.Set("X-Forwarded-For", client)
		w := httptest.NewRecorder()
		a.handleLogin(w, r)
		return w.Code
	}
	// Rotate usernames so only the per-IP cap is in play.
	for i := 0; i < maxFailsPerIP; i++ {
		login("203.0.113.66", "nobody"+strconv.Itoa(i), "x")
	}
	if code := login("203.0.113.66", "someone", "x"); code != 429 {
		t.Fatalf("attacker after %d failures = %d, want 429", maxFailsPerIP, code)
	}
	if code := login("198.51.100.20", "alice", "correct-horse"); code != 200 {
		t.Fatalf("another client behind the same proxy = %d, want 200 (not throttled)", code)
	}
}
