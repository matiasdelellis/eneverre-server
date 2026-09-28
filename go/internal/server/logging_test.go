package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
			t.Errorf("clientIP = %q, want the client hop", got)
		}
		// The explicit list replaces the loopback default.
		if got := tr.clientIP(req("127.0.0.1:1234", "203.0.113.5")); got != "127.0.0.1" {
			t.Errorf("clientIP = %q, want loopback (no longer trusted)", got)
		}
	})

	t.Run("client-supplied XFF prefix is ignored", func(t *testing.T) {
		// A proxy that appends to an incoming header (nginx
		// $proxy_add_x_forwarded_for) forwards whatever the client sent on the
		// left; the real client is the hop the trusted proxy appended.
		tr := newProxyTrust(nil)
		if got := tr.clientIP(req("127.0.0.1:1234", "10.9.9.9, 203.0.113.5")); got != "203.0.113.5" {
			t.Errorf("clientIP = %q, want the hop the proxy appended", got)
		}
		// Garbage in the header ends the walk at the last trusted hop.
		if got := tr.clientIP(req("127.0.0.1:1234", "203.0.113.5 user=x")); got != "127.0.0.1" {
			t.Errorf("clientIP = %q, want the proxy for an unparseable hop", got)
		}
	})

	t.Run("chained trusted proxies are skipped", func(t *testing.T) {
		tr := newProxyTrust([]string{"127.0.0.1", "192.168.1.0/24"})
		if got := tr.clientIP(req("127.0.0.1:1234", "6.6.6.6, 203.0.113.5, 192.168.1.10")); got != "203.0.113.5" {
			t.Errorf("clientIP = %q, want the first untrusted hop from the right", got)
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

// The access log must not carry credentials: the device code in the poll
// path, or the talk/webhook ?token= at DEBUG.
func TestAccessLogRedaction(t *testing.T) {
	if got := logPath("/api/auth/device/abc123"); got != "/api/auth/device/{device_code}" {
		t.Errorf("logPath(poll) = %q", got)
	}
	if got := logPath("/api/auth/device/verify"); got != "/api/auth/device/verify" {
		t.Errorf("logPath(verify) = %q", got)
	}
	if got := logPath("/api/cameras"); got != "/api/cameras" {
		t.Errorf("logPath(other) = %q", got)
	}
	q, _ := url.ParseQuery("token=s3cret&start=10&Refresh_Token=x")
	got := logQuery(q)
	if strings.Contains(got, "s3cret") || strings.Contains(got, "=x") {
		t.Errorf("logQuery leaks a credential: %q", got)
	}
	if !strings.Contains(got, "start=10") {
		t.Errorf("logQuery dropped a harmless param: %q", got)
	}
}
