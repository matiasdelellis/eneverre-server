package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// cors handles CORS and preflight OPTIONS. `allowed` is the Origin allowlist:
// when empty it preserves the default permissive behavior (reflect any
// Origin with credentials); when non-empty
// only those Origins get CORS headers, so a hostile page can't ride a browser
// session against the API. A request with no Origin (same-origin, curl, the
// native apps) is unaffected either way.
func cors(h http.Handler, allowed []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow := ""
		switch {
		case len(allowed) == 0:
			// No allowlist: reflect the Origin, or "*" when absent (unchanged).
			if origin != "" {
				allow = origin
			} else {
				allow = "*"
			}
		case origin != "" && originAllowed(origin, allowed):
			allow = origin
		}
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if allow != "" {
				w.Header().Set("Access-Control-Allow-Methods", "*")
				w.Header().Set("Access-Control-Allow-Headers", "*")
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

// queryFloat reads a float query param, falling back to def when missing/invalid.
func queryFloat(r *http.Request, key string, def float64) float64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
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
