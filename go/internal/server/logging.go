package server

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// statusRecorder wraps a ResponseWriter to capture the status code and byte
// count for the access log, while transparently forwarding Flush so streaming
// responses (playback) still flush.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	bytes   int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.status = http.StatusOK
		r.written = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped ResponseWriter so http.ResponseController can reach
// the underlying connection — needed by streaming handlers that clear the
// server's WriteTimeout (e.g. the long-lived live MSE feed) via
// SetWriteDeadline. Without Unwrap the controller can't see past this recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack forwards to the underlying ResponseWriter so WebSocket upgrades (the
// push-to-talk endpoint) work through the access-log middleware. The connection
// is taken over by the caller, so the logged status stays at its default (the
// handler never calls WriteHeader on a hijacked response).
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// accessLog logs one line per request at INFO (method, path, status, duration,
// client IP). At DEBUG it also logs the query string and response size.
func accessLog(next http.Handler, trust *proxyTrust) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		attrs := []any{
			"method", r.Method,
			"path", logPath(r.URL.Path),
			"status", rec.status,
			"dur_ms", dur.Milliseconds(),
			"ip", trust.clientIP(r),
		}
		if slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			attrs = append(attrs, "query", logQuery(r.URL.Query()), "bytes", rec.bytes)
		}
		slog.Info("request", attrs...)
	})
}

// devicePollPrefix is the device-login poll route; the path segment after it
// is the device code, which works as a bearer credential until the pairing
// completes (whoever polls it first after approval gets the session token).
const devicePollPrefix = "/api/auth/device/"

// logPath returns the request path with credentials in it masked.
func logPath(p string) string {
	if code, ok := strings.CutPrefix(p, devicePollPrefix); ok && code != "" && code != "verify" {
		return devicePollPrefix + "{device_code}"
	}
	return p
}

// secretQueryParams are query parameters that carry credentials: the talk
// WebSocket's ?token= (an access token) and the webhook's ?token= (the shared
// secret) among them.
var secretQueryParams = map[string]bool{"token": true, "refresh_token": true, "password": true, "secret": true, "api_key": true}

// logQuery renders the query string for the DEBUG access log with credential
// values replaced.
func logQuery(q url.Values) string {
	for k := range q {
		if secretQueryParams[strings.ToLower(k)] {
			q[k] = []string{"REDACTED"}
		}
	}
	return q.Encode()
}

// proxyTrust resolves the client IP for logging: X-Forwarded-For / X-Real-IP
// are honored ONLY when the socket peer is a trusted proxy. The resolved IP
// feeds the security log that fail2ban bans on, so an untrusted peer must
// never control it — otherwise a direct client can spoof X-Forwarded-For to
// get an innocent address banned (or to evade a ban). Configured via
// [server] trusted_proxies; the default trusts loopback only (the documented
// same-host Caddy deployment).
type proxyTrust struct {
	nets []*net.IPNet
}

// newProxyTrust parses [server] trusted_proxies entries (IPs or CIDRs).
// nil/empty entries -> loopback default; a single "none" -> trust no one;
// invalid entries are logged and skipped.
func newProxyTrust(entries []string) *proxyTrust {
	t := &proxyTrust{}
	if len(entries) == 0 {
		entries = []string{"127.0.0.0/8", "::1/128"}
	}
	for _, e := range entries {
		if strings.EqualFold(e, "none") {
			continue
		}
		cidr := e
		if !strings.Contains(e, "/") {
			if strings.Contains(e, ":") {
				cidr = e + "/128"
			} else {
				cidr = e + "/32"
			}
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			slog.Warn("ignoring invalid [server] trusted_proxies entry", "entry", e, "err", err)
			continue
		}
		t.nets = append(t.nets, n)
	}
	return t
}

// trusts reports whether the socket peer of r is a trusted proxy. Nil-safe
// (tests build App without one): a nil resolver trusts the loopback default.
func (t *proxyTrust) trusts(r *http.Request) bool {
	return t.trustsIP(net.ParseIP(peerHost(r)))
}

// trustsIP reports whether ip belongs to a trusted proxy (nil ip: no).
func (t *proxyTrust) trustsIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if t == nil {
		return ip.IsLoopback()
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// peerHost is the socket peer address of r without the port.
func peerHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// clientIP returns the client IP for r. Only a trusted proxy's forwarding
// headers are honored, and X-Forwarded-For is read from the RIGHT: each
// proxy appends the peer it saw, so the entries a trusted proxy added are at
// the end while everything to their left may have been sent by the client
// itself (nginx's $proxy_add_x_forwarded_for passes a client's header
// along). The client is therefore the rightmost hop that is not itself a
// trusted proxy — taking the leftmost would let any client pick the address
// the throttle and fail2ban act on. An unparseable hop ends the walk: it was
// not written by a proxy we trust, so the last trusted hop is the answer.
func (t *proxyTrust) clientIP(r *http.Request) string {
	peer := peerHost(r)
	if !t.trusts(r) {
		return peer
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		last := peer
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			ip := net.ParseIP(hop)
			if ip == nil {
				return last
			}
			if !t.trustsIP(ip) {
				return hop
			}
			last = hop
		}
		return last // every hop is a trusted proxy: the innermost one
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xr) != nil {
		return xr
	}
	return peer
}
