package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eneverre/internal/backchannel"
)

// TestCloseAllTalkClearsPlaceholders checks that shutdown teardown skips the
// nil placeholders a reservation leaves during Dial and clears the map without
// panicking. (Sessions with a live backchannel need a real RTSP peer, covered
// by the idempotency test in the backchannel package.)
func TestCloseAllTalkClearsPlaceholders(t *testing.T) {
	a := &App{talk: map[string]*backchannel.Session{"cam1": nil, "cam2": nil}}
	a.CloseAllTalk()
	if len(a.talk) != 0 {
		t.Fatalf("talk map not cleared: %d entries left", len(a.talk))
	}
}

// The Basic fallback carries browser-cached credentials a foreign page can't
// see but can still trigger, so without a token the talk upgrade must be
// same-origin. Token-bearing and non-browser (no Origin) requests pass.
func TestTalkCheckOrigin(t *testing.T) {
	req := func(origin, proto string) *http.Request {
		r := httptest.NewRequest("GET", "http://nvr.example:8080/api/camera/c/talk", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if proto != "" {
			r.Header.Set("Sec-WebSocket-Protocol", proto)
		}
		return r
	}
	for _, tc := range []struct {
		name         string
		origin, prot string
		want         bool
	}{
		{"same origin, no token", "http://nvr.example:8080", "", true},
		{"foreign origin, no token", "https://evil.example", "", false},
		{"foreign origin with token", "https://evil.example", "eneverre-talk, tok123", true},
		{"no origin (native client)", "", "", true},
	} {
		if got := talkCheckOrigin(req(tc.origin, tc.prot)); got != tc.want {
			t.Errorf("%s: talkCheckOrigin = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTalkCloseReasonStripsCredentials(t *testing.T) {
	got := talkCloseReason(errors.New(`dial rtsp://admin:hunter2@10.0.0.5:554/ch0: connection refused`))
	if strings.Contains(got, "hunter2") || strings.Contains(got, "admin") {
		t.Errorf("close reason leaks credentials: %q", got)
	}
	if !strings.Contains(got, "rtsp://10.0.0.5:554/ch0") {
		t.Errorf("close reason lost the useful part: %q", got)
	}
	long := talkCloseReason(errors.New(strings.Repeat("x", 500)))
	if len(long) > 123 {
		t.Errorf("close reason is %d bytes, over the 123-byte frame limit", len(long))
	}
}
