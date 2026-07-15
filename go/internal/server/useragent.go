package server

import "strings"

// deviceNameFromUA derives a human-friendly "Browser · OS" label from a
// User-Agent header, used as the session's device_name when the client did not
// supply one. It is a best-effort convenience label only: the User-Agent is
// client-controlled and trivially spoofable, so it must never back a security
// decision — the token fingerprint remains the stable session identifier.
//
// It recognises the common desktop/mobile browsers and OSes; anything else
// yields an empty string so the caller stores NULL (unchanged behaviour).
func deviceNameFromUA(ua string) string {
	browser := browserFromUA(ua)
	os := osFromUA(ua)
	switch {
	case browser != "" && os != "":
		return browser + " · " + os
	case browser != "":
		return browser
	case os != "":
		return os
	default:
		return ""
	}
}

// browserFromUA identifies the browser. Order matters: Edge, Opera and Samsung
// Internet all embed "Chrome" in their UA, and Chrome embeds "Safari", so the
// more specific tokens must be tested first.
func browserFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "Edge/"),
		strings.Contains(ua, "EdgA/"), strings.Contains(ua, "EdgiOS/"):
		return "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"):
		return "Opera"
	case strings.Contains(ua, "SamsungBrowser/"):
		return "Samsung Internet"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		return "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"):
		return "Chrome"
	case strings.Contains(ua, "Safari/"):
		return "Safari"
	default:
		return ""
	}
}

// osFromUA identifies the operating system. Order matters: Android and ChromeOS
// UAs both contain "Linux", and iOS/iPadOS UAs contain "Mac OS X" tokens, so the
// more specific platforms are tested before their generic bases.
func osFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "Windows NT"):
		return "Windows"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"),
		strings.Contains(ua, "iPod"):
		return "iOS"
	case strings.Contains(ua, "CrOS"):
		return "ChromeOS"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		return "macOS"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	default:
		return ""
	}
}
