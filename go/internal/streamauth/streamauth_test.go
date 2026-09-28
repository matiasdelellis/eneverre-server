package streamauth

import "testing"

func TestRtspURLBracketsIPv6(t *testing.T) {
	c := Creds{Username: "u", Password: "p"}
	for host, want := range map[string]string{
		"192.168.1.5": "rtsp://u:p@192.168.1.5:8554/cam",
		"::1":         "rtsp://u:p@[::1]:8554/cam",
		"[fe80::1]":   "rtsp://u:p@[fe80::1]:8554/cam",
		"nvr.example": "rtsp://u:p@nvr.example:8554/cam",
	} {
		if got := c.RtspURL(host, "8554", "cam"); got != want {
			t.Errorf("RtspURL(%q) = %q, want %q", host, got, want)
		}
	}
}
