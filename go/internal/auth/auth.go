// Package auth provides Werkzeug-format password hashing and the Bearer
// authentication used by the API.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// CurrentUser is the authenticated principal for a request.
type CurrentUser struct {
	Username string
	Role     string
	// MustChangePassword mirrors users.must_change_password. requireUser
	// enforces it server-side (the web UI already gates on the login flag); a
	// seeded admin or an admin-forced reset can only change the password until
	// the flag clears.
	MustChangePassword bool
	// Service marks a scoped machine token (service_tokens table). Service
	// principals are rejected by requireUser and only reach the read-only
	// endpoints that explicitly check one of their Scopes.
	Service bool
	// Scopes is the service token's read-only scope list; nil for humans.
	Scopes []string
}

// IsAdmin reports whether the user has the admin role.
func (u CurrentUser) IsAdmin() bool { return u.Role == "admin" }

// IsService reports whether the principal is a scoped machine token.
func (u CurrentUser) IsService() bool { return u.Service }

// HasScope reports whether a service principal carries scope. Human sessions
// are not scope-limited by this helper; handlers gate them by role instead.
func (u CurrentUser) HasScope(scope string) bool {
	for _, s := range u.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// dummyPasswordHash has the default algorithm and iteration count, so
// checking a password against it costs exactly what a real check does.
var dummyPasswordHash = "pbkdf2:sha256:600000$dummy$" + strings.Repeat("0", 64)

// BurnPasswordCheck runs one password verification against a dummy hash. Call
// it on the unknown-user path of any password check: returning early there is
// ~100ms faster than a wrong password for a real user, which enumerates
// usernames by timing.
func BurnPasswordCheck(password string) {
	CheckPasswordHash(dummyPasswordHash, password)
}

// VerifyBearer authenticates an "Authorization: Bearer <token>" header against
// the tokens table, rejecting expired tokens.
func VerifyBearer(db *sql.DB, r *http.Request) *CurrentUser {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil
	}
	return VerifyToken(db, strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
}

// VerifyToken authenticates a raw access token against the tokens table,
// rejecting empty or expired tokens. It backs both VerifyBearer (header) and the
// query-param path used by browser WebSockets, which cannot set an
// Authorization header on the upgrade request.
func VerifyToken(db *sql.DB, token string) *CurrentUser {
	if token == "" {
		return nil
	}
	// One JOINed query instead of tokens-then-users: this runs on every Bearer
	// request, so the second round-trip was pure hot-path overhead. A token
	// whose user was deleted matches no row, same as before. A non-empty
	// tokens.role overrides the user's current role (device-login sessions are
	// pinned to "user" even when an admin approves them); NULL keeps the
	// historical behavior.
	var username, role string
	var expires sql.NullInt64
	var mustChange bool
	if err := db.QueryRow(
		"SELECT t.username, t.expires_at, COALESCE(NULLIF(t.role, ''), u.role), u.must_change_password "+
			"FROM tokens t JOIN users u ON u.username = t.username WHERE t.token = ?", token,
	).Scan(&username, &expires, &role, &mustChange); err != nil {
		return nil
	}
	if expires.Valid && expires.Int64 != 0 && expires.Int64 < time.Now().Unix() {
		return nil
	}
	return &CurrentUser{Username: username, Role: role, MustChangePassword: mustChange}
}

// BearerToken extracts the raw token from the Authorization header, or "".
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// TokenURLSafe mirrors secrets.token_urlsafe(n): n random bytes, base64url
// without padding.
func TokenURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHex mirrors secrets.token_hex(n): n random bytes, hex-encoded.
func TokenHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
