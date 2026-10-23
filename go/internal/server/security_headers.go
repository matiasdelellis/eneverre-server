package server

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy is intentionally strict for scripts: the UI has no
// inline scripts (the early boot shims live in /boot.js), so any injected
// <script> is blocked. Styles keep 'unsafe-inline' because a few dynamic
// views use width/percentage style attributes; a future cleanup can tighten
// that to hashes. Media comes from the same origin (MSE/HLS fetch) or blob:
// objects built by the UI.
const contentSecurityPolicy = "default-src 'self'; " +
	"base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"media-src 'self' blob:; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"worker-src 'self' blob:"

// securityHeaders sets the browser-side hardening headers on every response.
// They are set by the Go binary (not only by the example Caddyfile) so a
// direct-LAN deployment gets the same protections, including clickjacking and
// XSS containment for the token-bearing UI.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=()")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// HSTS is only meaningful over TLS. Behind a reverse proxy r.TLS is nil
		// but the trusted edge sets X-Forwarded-Proto; a direct client that
		// spoofs it only gets an HSTS header on a plain-HTTP response, which
		// browsers ignore.
		if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// noStoreAPI keeps API responses out of browser and intermediary caches. The
// API returns session material, camera credentials and signed media grants;
// the only exception is the published build download, which is intentionally
// cacheable and public.
func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/app/updates/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
