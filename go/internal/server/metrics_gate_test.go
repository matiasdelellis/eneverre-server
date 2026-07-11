package server

import (
	"net/http"
	"testing"
)

func TestIsLocalRequest(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		want       bool
	}{
		{"loopback ipv4 direct", "127.0.0.1:54321", "", "", true},
		{"loopback ipv6 direct", "[::1]:54321", "", "", true},
		{"loopback range direct", "127.0.0.5:9", "", "", true},
		{"lan peer", "192.168.1.10:443", "", "", false},
		{"public peer", "203.0.113.7:443", "", "", false},
		// Spoof attempts: a forwarded header must never grant the local bypass,
		// even when the socket peer is loopback (proxy on the same host).
		{"loopback but forwarded (proxy on localhost)", "127.0.0.1:443", "203.0.113.7", "", false},
		{"loopback but spoofed xff", "127.0.0.1:443", "127.0.0.1", "", false},
		{"loopback but x-real-ip set", "127.0.0.1:443", "", "127.0.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/api/metrics", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}
			if got := isLocalRequest(r); got != tc.want {
				t.Errorf("isLocalRequest(%q, xff=%q, xrealip=%q) = %v, want %v",
					tc.remoteAddr, tc.xff, tc.xRealIP, got, tc.want)
			}
		})
	}
}
