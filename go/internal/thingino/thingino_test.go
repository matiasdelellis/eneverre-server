package thingino

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseMotorPos pins the motor-response parser: the firmware wraps the
// position in a nested message block and serialises xpos/ypos as strings, so
// the parser has to unwrap both. The function returns ok=false for anything
// the caller can't use to refresh the position cache.
func TestParseMotorPos(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantX  float64
		wantY  float64
		wantOK bool
	}{
		{
			name:  "happy path",
			body:  `{"code":200,"result":"success","message":{"status":"0","xpos":"1089","ypos":"859","speed":"900","invert":"0"}}`,
			wantX: 1089, wantY: 859, wantOK: true,
		},
		{
			name:  "negative position",
			body:  `{"code":200,"message":{"xpos":"-100","ypos":"-200"}}`,
			wantX: -100, wantY: -200, wantOK: true,
		},
		{
			name:   "empty fields means no position",
			body:   `{"code":200,"message":{"xpos":"","ypos":""}}`,
			wantOK: false,
		},
		{
			name:   "missing message block",
			body:   `{"code":200,"result":"success"}`,
			wantOK: false,
		},
		{
			name:   "non-numeric xpos",
			body:   `{"code":200,"message":{"xpos":"abc","ypos":"100"}}`,
			wantOK: false,
		},
		{
			name:   "malformed json",
			body:   `not json`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x, y, ok := ParseMotorPos([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("ok = %v; want %v (x=%v y=%v)", ok, c.wantOK, x, y)
			}
			if c.wantOK && (x != c.wantX || y != c.wantY) {
				t.Errorf("position = %v, %v; want %v, %v", x, y, c.wantX, c.wantY)
			}
		})
	}
}

// TestPosition drives the (d=j) call against an httptest server and checks
// the request and the parsed response. The camera returns xpos/ypos as
// strings inside a nested message block, so the wire format and the parser
// need to agree end-to-end.
func TestPosition(t *testing.T) {
	var gotPath, gotMode, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMode = r.URL.Query().Get("d")
		gotToken = r.URL.Query().Get("token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":200,"result":"success","message":{"status":"0","xpos":"1500","ypos":"750","speed":"900","invert":"0"}}`))
	}))
	defer srv.Close()

	x, y, err := Position(srv.URL, "secret-token")
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if x != 1500 || y != 750 {
		t.Errorf("position = %v, %v; want 1500, 750", x, y)
	}
	if !strings.HasSuffix(gotPath, "/x/json-motor.cgi") {
		t.Errorf("path = %q; want /x/json-motor.cgi", gotPath)
	}
	if gotMode != "j" {
		t.Errorf("d = %q; want j", gotMode)
	}
	if gotToken != "secret-token" {
		t.Errorf("token = %q; want secret-token", gotToken)
	}
}

// TestPositionParseFailure makes sure a malformed body bubbles up as an
// error rather than a zero position, so a buggy camera never silently
// "resets" the position cache.
func TestPositionParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":200,"result":"success"}`))
	}))
	defer srv.Close()
	_, _, err := Position(srv.URL, "k")
	if err == nil {
		t.Error("expected error on unparseable position, got nil")
	}
}
