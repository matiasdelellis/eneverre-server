package server

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", dur.Milliseconds(),
			"ip", clientIP(r),
		}
		if slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			attrs = append(attrs, "query", r.URL.RawQuery, "bytes", rec.bytes)
		}
		slog.Info("request", attrs...)
	})
}

// clientIP returns the real client IP, honoring X-Forwarded-For / X-Real-IP set
// by a fronting reverse proxy (Caddy), else the socket peer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return xr
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
