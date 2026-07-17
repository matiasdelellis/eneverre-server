package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Failed-authentication throttle. Every password check costs a full PBKDF2
// pass (~100ms of CPU), so without a cap an attacker can both brute-force
// credentials and starve the recorder of CPU with concurrent bogus logins.
// Only FAILURES count: legitimate users never accumulate strikes, and a
// success clears their username's slate. Keys are the socket peer IP (never
// X-Forwarded-For, which the client controls) and the attempted username, so
// rotating usernames still trips the per-IP cap and a distributed attack on
// one account still trips the per-username cap. Behind a reverse proxy every
// client shares the proxy's IP; the per-IP cap is deliberately generous so
// only an actual attack (not a fleet of wall clients with a stale password)
// reaches it — finer-grained banning stays with fail2ban via the security log.
const (
	authStrikeWindow = 5 * time.Minute
	maxFailsPerIP    = 20
	maxFailsPerUser  = 10
)

// authThrottle counts recent authentication failures per key ("ip:…" and
// "user:…") in fixed windows. Entries expire authStrikeWindow after their
// first strike; the map is swept lazily so it cannot grow without bound.
type authThrottle struct {
	mu      sync.Mutex
	strikes map[string]*strikeEntry
}

type strikeEntry struct {
	count   int
	resetAt time.Time
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{strikes: make(map[string]*strikeEntry)}
}

// blocked reports whether the ip or username has exceeded its failure cap,
// and how long until the oldest window expires (for Retry-After). Nil-safe
// (tests build App without one), like secLogger.event.
func (t *authThrottle) blocked(ip, user string) (bool, time.Duration) {
	if t == nil {
		return false, 0
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var wait time.Duration
	for key, max := range map[string]int{"ip:" + ip: maxFailsPerIP, "user:" + user: maxFailsPerUser} {
		e := t.strikes[key]
		if e == nil {
			continue
		}
		if now.After(e.resetAt) {
			delete(t.strikes, key)
			continue
		}
		if e.count >= max && e.resetAt.Sub(now) > wait {
			wait = e.resetAt.Sub(now)
		}
	}
	return wait > 0, wait
}

// fail records one authentication failure against both keys.
func (t *authThrottle) fail(ip, user string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.strikes) > 4096 { // sweep expired entries before growing further
		for k, e := range t.strikes {
			if now.After(e.resetAt) {
				delete(t.strikes, k)
			}
		}
	}
	for _, key := range []string{"ip:" + ip, "user:" + user} {
		e := t.strikes[key]
		if e == nil || now.After(e.resetAt) {
			t.strikes[key] = &strikeEntry{count: 1, resetAt: now.Add(authStrikeWindow)}
			continue
		}
		e.count++
	}
}

// success clears the username's strikes (the user proved they know the
// password; stale-client noise before that shouldn't linger). The IP counter
// is left alone: a mixed attacker/legit-user source keeps its per-IP history.
func (t *authThrottle) success(user string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.strikes, "user:"+user)
	t.mu.Unlock()
}

// remoteIP is the socket peer address of r, ignoring X-Forwarded-For /
// X-Real-IP (client-supplied, trivially spoofed — same rationale as
// isLocalRequest). This is the only IP the throttle keys on.
func remoteIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// throttleExceeded writes the 429 for a blocked authentication attempt.
func throttleExceeded(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	httpError(w, http.StatusTooManyRequests, "Too many failed authentication attempts; try again later")
}
