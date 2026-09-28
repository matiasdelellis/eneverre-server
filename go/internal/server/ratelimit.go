package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Failed-authentication throttle. Every password check costs a full PBKDF2
// pass (~100ms of CPU), so without a cap an attacker can both brute-force
// credentials and starve the recorder of CPU with concurrent bogus logins.
// Only FAILURES count: legitimate users never accumulate strikes, and a
// success clears their username's slate. Keys are the client IP and the
// attempted username, so rotating usernames still trips the per-IP cap and a
// distributed attack on one account still trips the per-username cap. The
// client IP is resolved like the access and security logs do
// (proxyTrust.clientIP): the socket peer, or — only behind a [server]
// trusted_proxies proxy — the hop that proxy forwarded. Keying on the socket
// peer alone made every client behind the same-host Caddy share 127.0.0.1,
// so 20 bad passwords from anywhere locked everyone out.
//
// Two refinements keep the caps from being turned against legitimate users:
//
//   - The per-username cap only blocks sources that are themselves failing
//     (a recent strike or an attempt in flight). Otherwise ten wrong guesses
//     at "admin" from anywhere would lock the real admin out for as long as
//     the attacker cared to keep it up; now a clean client still gets through
//     (one attempt at a time while the account is under attack) and each
//     attacking IP is cut to a single attempt.
//   - Checks reserve the attempt (begin/release) instead of reading counters
//     that are only bumped after the ~100ms PBKDF2 pass. Without that, a burst
//     of concurrent requests all passed the check before the first failure
//     landed, which is exactly the CPU starvation the cap exists to stop.
const (
	authStrikeWindow = 5 * time.Minute
	maxFailsPerIP    = 20
	maxFailsPerUser  = 10
	// inflightRetry is the Retry-After for a request refused only because
	// earlier attempts from the same source are still being verified.
	inflightRetry = time.Second
	// deviceCreatesPerWindow caps pairing codes one client may request per
	// 5 minutes (a code lives 300s; a TV needs one, a few retries at most).
	deviceCreatesPerWindow = 10
)

// authThrottle counts recent authentication failures per key ("ip:…" and
// "user:…") in fixed windows, plus the attempts currently being verified.
// Strike entries expire authStrikeWindow after their first strike; the map is
// swept lazily so it cannot grow without bound. In-flight entries are removed
// as soon as they drop to zero.
type authThrottle struct {
	mu       sync.Mutex
	strikes  map[string]*strikeEntry
	inflight map[string]int
}

type strikeEntry struct {
	count   int
	resetAt time.Time
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{strikes: make(map[string]*strikeEntry), inflight: make(map[string]int)}
}

// strikesLocked returns the live strike count for key and when its window
// resets, dropping an expired entry. Caller holds t.mu.
func (t *authThrottle) strikesLocked(key string, now time.Time) (int, time.Time) {
	e := t.strikes[key]
	if e == nil {
		return 0, time.Time{}
	}
	if now.After(e.resetAt) {
		delete(t.strikes, key)
		return 0, time.Time{}
	}
	return e.count, e.resetAt
}

// begin reserves one authentication attempt for ip+user. When the attempt is
// refused it returns ok=false and how long to wait (for Retry-After);
// otherwise the caller must call release exactly once with the outcome —
// failed=true records a strike against both keys, failed=false clears the
// username's strikes (the user proved they know the password; stale-client
// noise before that shouldn't linger). The IP's strikes are left alone on
// success, so a mixed attacker/legit-user source keeps its per-IP history.
// Nil-safe (tests build App without one): a nil throttle never refuses.
func (t *authThrottle) begin(ip, user string) (release func(failed bool), wait time.Duration, ok bool) {
	if t == nil {
		return func(bool) {}, 0, true
	}
	ipKey, userKey := "ip:"+ip, "user:"+user
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	ipStrikes, ipReset := t.strikesLocked(ipKey, now)
	userStrikes, userReset := t.strikesLocked(userKey, now)
	ipBusy := t.inflight[ipKey]

	// Per-IP cap: in-flight attempts count as pending strikes, so a burst
	// can't outrun the counter.
	if ipStrikes+ipBusy >= maxFailsPerIP {
		if ipStrikes >= maxFailsPerIP {
			return nil, ipReset.Sub(now), false
		}
		return nil, inflightRetry, false
	}
	// Per-username cap: only for a source that is itself failing or already
	// has an attempt in flight (see the comment at the top of this file).
	if userStrikes+t.inflight[userKey] >= maxFailsPerUser && (ipStrikes > 0 || ipBusy > 0) {
		if userStrikes >= maxFailsPerUser {
			return nil, userReset.Sub(now), false
		}
		return nil, inflightRetry, false
	}

	t.inflight[ipKey]++
	t.inflight[userKey]++
	var once sync.Once
	return func(failed bool) {
		once.Do(func() { t.finish(ipKey, userKey, failed) })
	}, 0, true
}

// finish ends an attempt reserved by begin.
func (t *authThrottle) finish(ipKey, userKey string, failed bool) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, key := range []string{ipKey, userKey} {
		if t.inflight[key]--; t.inflight[key] <= 0 {
			delete(t.inflight, key)
		}
	}
	if !failed {
		delete(t.strikes, userKey)
		return
	}
	if len(t.strikes) > 4096 { // sweep expired entries before growing further
		for k, e := range t.strikes {
			if now.After(e.resetAt) {
				delete(t.strikes, k)
			}
		}
	}
	for _, key := range []string{ipKey, userKey} {
		e := t.strikes[key]
		if e == nil || now.After(e.resetAt) {
			t.strikes[key] = &strikeEntry{count: 1, resetAt: now.Add(authStrikeWindow)}
			continue
		}
		e.count++
	}
}

// throttleExceeded writes the 429 for a blocked authentication attempt.
func throttleExceeded(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	httpError(w, http.StatusTooManyRequests, "Too many failed authentication attempts; try again later")
}

// windowLimiter allows up to max events per key in fixed windows. Used for
// unauthenticated endpoints that create server-side state per call (device
// pairing codes). Nil-safe: a nil limiter always allows (tests build App
// without one).
type windowLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	entries map[string]*strikeEntry
}

func newWindowLimiter(max int, window time.Duration) *windowLimiter {
	return &windowLimiter{max: max, window: window, entries: make(map[string]*strikeEntry)}
}

// allow records one event for key and reports whether it is within the cap,
// or how long until the window resets.
func (l *windowLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 4096 {
		for k, e := range l.entries {
			if now.After(e.resetAt) {
				delete(l.entries, k)
			}
		}
	}
	e := l.entries[key]
	if e == nil || now.After(e.resetAt) {
		l.entries[key] = &strikeEntry{count: 1, resetAt: now.Add(l.window)}
		return true, 0
	}
	if e.count >= l.max {
		return false, e.resetAt.Sub(now)
	}
	e.count++
	return true, 0
}
