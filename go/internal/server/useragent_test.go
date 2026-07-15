package server

import "testing"

func TestDeviceNameFromUA(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		{
			name: "chrome on linux",
			ua:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			want: "Chrome · Linux",
		},
		{
			name: "edge on windows (embeds Chrome and Safari)",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
			want: "Edge · Windows",
		},
		{
			name: "safari on macos",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
			want: "Safari · macOS",
		},
		{
			name: "firefox on windows",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
			want: "Firefox · Windows",
		},
		{
			name: "chrome on android (embeds Linux)",
			ua:   "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			want: "Chrome · Android",
		},
		{
			name: "safari on ios (embeds Mac OS X)",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			want: "Safari · iOS",
		},
		{
			name: "chrome on chromeos (embeds Linux)",
			ua:   "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			want: "Chrome · ChromeOS",
		},
		{
			name: "empty user agent",
			ua:   "",
			want: "",
		},
		{
			name: "unrecognised",
			ua:   "curl/8.5.0",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceNameFromUA(tc.ua); got != tc.want {
				t.Errorf("deviceNameFromUA(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}
