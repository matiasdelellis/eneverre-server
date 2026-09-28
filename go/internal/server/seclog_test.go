package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestSecLoggerEventFormat(t *testing.T) {
	var buf bytes.Buffer
	s := &secLogger{w: &buf}
	s.event("203.0.113.5", "authentication_failure", "admin", "/api/login", "invalid_credentials")

	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("event line must end with a newline: %q", line)
	}
	for _, want := range []string{
		"eneverre authentication_failure",
		"ip=203.0.113.5",
		`user="admin"`,
		`path="/api/login"`,
		"reason=invalid_credentials",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("event line missing %q\ngot: %s", want, line)
		}
	}
}

func TestSecLoggerNilWriterNoPanic(t *testing.T) {
	// A disabled logger (no file) must not panic and must write nothing.
	s := &secLogger{}
	s.event("203.0.113.5", "authentication_failure", "admin", "/api/login", "invalid_credentials")
}

func TestQuoteFieldEscapesInjection(t *testing.T) {
	// A crafted username must not inject a newline that forges a second
	// log line, nor unescaped quotes that break field parsing.
	got := quoteField("evil\nip=1.2.3.4 reason=faked\"end")
	if strings.Contains(got, "\n") {
		t.Errorf("newline not escaped: %q", got)
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("field not wrapped in quotes: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("newline should be rendered as \\n: %q", got)
	}
	if !strings.Contains(got, `\"end`) {
		t.Errorf("embedded quote should be escaped: %q", got)
	}
}

// Every client-controlled field must stay one space-free token: fail2ban's
// greedy failregex would otherwise ban an IP smuggled inside the username or
// the (percent-decoded) path.
func TestSecLoggerNoForgedFailure(t *testing.T) {
	var buf bytes.Buffer
	s := &secLogger{w: &buf}
	forged := "x eneverre authentication_failure ip=10.0.0.1 y"
	s.event("203.0.113.5", "authentication_failure", forged, "/api/camera/a\n"+forged+"/thumbnail", "basic_auth_failed")

	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("event produced more than one line: %q", line)
	}
	if n := strings.Count(line, " eneverre authentication_failure ip="); n != 1 {
		t.Fatalf("forged failure marker survived (%d occurrences): %s", n, line)
	}
	if strings.Contains(line, "ip=10.0.0.1") && !strings.Contains(line, `\x20ip=10.0.0.1`) {
		t.Errorf("smuggled ip= token is not escaped: %s", line)
	}
}

func TestSecLogIPRejectsNonAddress(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	if got := secLogIP("203.0.113.5", r); got != "203.0.113.5" {
		t.Errorf("valid ip rewritten: %q", got)
	}
	if got := secLogIP("1.2.3.4 user=x", r); got != "192.0.2.7" {
		t.Errorf("non-address forwarded value = %q, want the socket peer", got)
	}
}
