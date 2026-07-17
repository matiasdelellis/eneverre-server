package timeutil

import "testing"

func TestParseISO(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		wantUnix int64 // checked only when ok
	}{
		{"2026-07-17T10:30:00Z", true, 1784284200},
		{"2026-07-17T10:30:00.500Z", true, 1784284200},       // fractional seconds
		{"2026-07-17T10:30:00.123456789Z", true, 1784284200}, // nanoseconds
		{"2026-07-17T07:30:00-03:00", true, 1784284200},      // zoned -> same instant as 10:30Z
		{"2026-07-17T10:30:00", true, 1784284200},            // naive read as UTC
		{"2026-07-17T10:30:00.999999", true, 1784284200},     // naive + micros
		{"", false, 0},
		{"1784284200", false, 0}, // bare unix seconds are NOT accepted here
		{"2026-07-17", false, 0}, // date only
		{"garbage", false, 0},
	}
	for _, c := range cases {
		got, ok := ParseISO(c.in)
		if ok != c.ok {
			t.Errorf("ParseISO(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok {
			if got.Unix() != c.wantUnix {
				t.Errorf("ParseISO(%q).Unix() = %d, want %d", c.in, got.Unix(), c.wantUnix)
			}
			if got.Location() != nil && got.Location().String() != "UTC" {
				t.Errorf("ParseISO(%q) location = %s, want UTC", c.in, got.Location())
			}
		}
	}
}
