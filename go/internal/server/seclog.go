package server

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// secLogger writes security-relevant events (authentication failures) in a
// stable, greppable, one-line-per-event format meant to be tailed by an
// intrusion-prevention tool such as fail2ban or CrowdSec.
//
// Line format (fields are space-separated, values that may contain spaces are
// quoted):
//
//	2006-01-02T15:04:05Z07:00 eneverre <event> ip=<client-ip> user="<username>" path=<path> reason=<reason>
//
// Example:
//
//	2026-07-10T14:23:01-03:00 eneverre authentication_failure ip=203.0.113.5 user="admin" path=/api/login reason=invalid_credentials
//
// The leading RFC3339 timestamp and the `ip=<HOST>` token are what the
// fail2ban filter keys off. See doc/security-logging.md for ready-to-use
// fail2ban and CrowdSec configuration.
//
// Every event is always mirrored to the default slog logger at WARN so it is
// visible in the main log / journal even when no dedicated file is configured.
type secLogger struct {
	mu sync.Mutex
	w  io.Writer // append-only file, or nil when no path is configured / open failed
}

// newSecLogger opens the security log file at path in append mode. An empty
// path (feature disabled) or an open error yields a logger that only mirrors
// to slog — it never returns nil, so callers need no guard.
func newSecLogger(path string) *secLogger {
	if path == "" {
		return &secLogger{}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		slog.Error("could not open security log; falling back to main log only", "path", path, "err", err)
		return &secLogger{}
	}
	slog.Info("security log enabled", "path", path)
	return &secLogger{w: f}
}

// event records one security event. It is safe for concurrent use.
func (s *secLogger) event(ip, event, user, path, reason string) {
	slog.Warn("security", "event", event, "ip", ip, "user", user, "path", path, "reason", reason)
	if s == nil || s.w == nil {
		return
	}
	// Quote the username (attacker-controlled, may contain spaces/newlines);
	// %q also escapes control characters so a crafted username cannot forge
	// extra log lines. The other fields are server-controlled tokens.
	line := time.Now().Format(time.RFC3339) + " eneverre " + event +
		" ip=" + ip + " user=" + quoteField(user) + " path=" + path + " reason=" + reason + "\n"
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.w, line)
}

// quoteField renders v as a double-quoted, escaped token so untrusted input
// cannot inject newlines or break field parsing.
func quoteField(v string) string {
	// strconv.Quote via fmt would pull escaping semantics we want; do it
	// directly to keep the dependency surface tiny and the output predictable.
	const hex = "0123456789abcdef"
	buf := make([]byte, 0, len(v)+2)
	buf = append(buf, '"')
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"' || c == '\\':
			buf = append(buf, '\\', c)
		case c == '\n':
			buf = append(buf, '\\', 'n')
		case c == '\r':
			buf = append(buf, '\\', 'r')
		case c == '\t':
			buf = append(buf, '\\', 't')
		case c < 0x20 || c == 0x7f:
			buf = append(buf, '\\', 'x', hex[c>>4], hex[c&0xf])
		default:
			buf = append(buf, c)
		}
	}
	buf = append(buf, '"')
	return string(buf)
}

// logAuthFailure records a failed authentication attempt from r. user is the
// attempted username (may be empty when unknown); reason is a short machine
// token (invalid_credentials, basic_auth_failed, …).
func (a *App) logAuthFailure(r *http.Request, user, reason string) {
	a.secLog.event(a.proxyTrust.clientIP(r), "authentication_failure", user, r.URL.Path, reason)
}
