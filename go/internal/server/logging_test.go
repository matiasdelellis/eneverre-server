package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyTrustClientIP(t *testing.T) {
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/api/cameras", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	t.Run("default trusts loopback proxy", func(t *testing.T) {
		tr := newProxyTrust(nil)
		if got := tr.clientIP(req("127.0.0.1:54321", "203.0.113.5")); got != "203.0.113.5" {
			t.Errorf("clientIP = %q, want the forwarded address", got)
		}
	})

	t.Run("default ignores XFF from a remote peer", func(t *testing.T) {
		tr := newProxyTrust(nil)
		// A direct client spoofing X-Forwarded-For must be logged by its
		// socket address — that IP feeds fail2ban.
		if got := tr.clientIP(req("198.51.100.7:40000", "10.0.0.1")); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the socket peer", got)
		}
	})

	t.Run("explicit CIDR trusts a remote proxy", func(t *testing.T) {
		tr := newProxyTrust([]string{"192.168.1.0/24"})
		if got := tr.clientIP(req("192.168.1.10:1234", "203.0.113.5, 192.168.1.10")); got != "203.0.113.5" {
			t.Errorf("clientIP = %q, want the first forwarded hop", got)
		}
		// The explicit list replaces the loopback default.
		if got := tr.clientIP(req("127.0.0.1:1234", "203.0.113.5")); got != "127.0.0.1" {
			t.Errorf("clientIP = %q, want loopback (no longer trusted)", got)
		}
	})

	t.Run("bare IP entry works", func(t *testing.T) {
		tr := newProxyTrust([]string{"10.0.0.2"})
		if got := tr.clientIP(req("10.0.0.2:9999", "203.0.113.9")); got != "203.0.113.9" {
			t.Errorf("clientIP = %q, want forwarded", got)
		}
	})

	t.Run("none trusts nobody", func(t *testing.T) {
		tr := newProxyTrust([]string{"none"})
		if got := tr.clientIP(req("127.0.0.1:1234", "203.0.113.5")); got != "127.0.0.1" {
			t.Errorf("clientIP = %q, want the socket peer", got)
		}
	})

	t.Run("nil resolver falls back to loopback default", func(t *testing.T) {
		var tr *proxyTrust
		if got := tr.clientIP(req("127.0.0.1:1234", "203.0.113.5")); got != "203.0.113.5" {
			t.Errorf("clientIP = %q, want forwarded (nil = loopback default)", got)
		}
		if got := tr.clientIP(req("198.51.100.7:1234", "203.0.113.5")); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the socket peer", got)
		}
	})
}

// deadlineRW is a ResponseWriter that records a SetWriteDeadline call, standing
// in for the real http.conn writer that supports write deadlines.
type deadlineRW struct {
	http.ResponseWriter
	setCalled bool
}

func (d *deadlineRW) SetWriteDeadline(time.Time) error { d.setCalled = true; return nil }

// TestStatusRecorderUnwrapForDeadline guards the fix for the live feed being cut
// every 30s by the server WriteTimeout: the access-log wrapper must expose
// Unwrap so http.ResponseController can reach the underlying writer and clear
// the deadline. If Unwrap is dropped, SetWriteDeadline silently stops reaching
// the connection and the 30s reconnect/rebuffer regression returns.
func TestStatusRecorderUnwrapForDeadline(t *testing.T) {
	base := &deadlineRW{ResponseWriter: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: base, status: http.StatusOK}

	if err := http.NewResponseController(rec).SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline through statusRecorder: %v", err)
	}
	if !base.setCalled {
		t.Error("SetWriteDeadline did not reach the underlying writer (statusRecorder.Unwrap missing?)")
	}
}
