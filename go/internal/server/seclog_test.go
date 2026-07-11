package server

import (
	"bytes"
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
		"path=/api/login",
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
