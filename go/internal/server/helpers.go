package server

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// cors handles CORS and preflight OPTIONS. `allowed` is the Origin allowlist
// ([server] cors_origins):
//
//   - empty (default): any Origin may read responses, but WITHOUT credentials.
//     A cross-origin front-end that authenticates with a Bearer header keeps
//     working, while a hostile page can no longer ride the browser's cached
//     HTTP Basic credentials: without Access-Control-Allow-Credentials the
//     browser won't let it read a credentialed response (and a credentialed
//     preflight fails).
//   - listed Origins: reflected with credentials; others get no CORS headers.
//   - a "*" entry: any Origin, with credentials — the old permissive default,
//     now an explicit opt-in.
//
// A request with no Origin (same-origin, curl, the native apps) is unaffected.
func cors(h http.Handler, allowed []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow, creds := "", false
		switch {
		case len(allowed) == 0:
			if origin != "" {
				allow = origin
			} else {
				allow = "*"
			}
		case origin != "" && originAllowed(origin, allowed):
			allow, creds = origin, true
		}
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
			if creds {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if allow != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				// Echo the requested headers: a "*" wildcard never covers
				// Authorization, which is the one header a Bearer client needs.
				hdrs := r.Header.Get("Access-Control-Request-Headers")
				if hdrs == "" {
					hdrs = "Authorization, Content-Type"
				}
				w.Header().Set("Access-Control-Allow-Headers", hdrs)
				w.Header().Add("Vary", "Access-Control-Request-Headers")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin is in the allowlist. A literal "*" entry
// matches any Origin (an explicit opt-in to the permissive behavior).
func originAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

// queryFloat reads a float query param, falling back to def when missing or
// invalid. NaN and ±Inf count as invalid: ParseFloat accepts "NaN"/"Inf", and
// NaN slips through every clamp (all comparisons are false), so it would
// reach the camera firmware as x=NaN.
func queryFloat(r *http.Request, key string, def float64) float64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return def
	}
	return n
}

// clearWriteDeadline lifts the server's global WriteTimeout (30s) for one
// response whose body legitimately takes longer to send — a clip export, an
// APK download, the live MSE feed. Every other handler keeps the 30s guard.
func clearWriteDeadline(w http.ResponseWriter, what string) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug(what+": could not clear write deadline", "err", err)
	}
}

// maxJSONBodyBytes caps request bodies decoded by decodeJSON. Without it a
// client can POST an arbitrarily large body (fully buffered before any
// validation) and exhaust memory; the unauthenticated login path is the most
// exposed. http.MaxBytesReader aborts the read as soon as the limit is crossed.
const maxJSONBodyBytes = 1 << 20 // 1 MiB

// decodeJSON reads and validates a JSON request body into dst. On failure it
// writes a 422 and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		httpError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return false
	}
	return true
}
