// Package auth provides Werkzeug-format password hashing and the Basic/Bearer
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
}

// IsAdmin reports whether the user has the admin role.
func (u CurrentUser) IsAdmin() bool { return u.Role == "admin" }

// VerifyBasic authenticates HTTP Basic credentials against the users table.
func VerifyBasic(db *sql.DB, r *http.Request) *CurrentUser {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil
	}
	var stored, role string
	if err := db.QueryRow(
		"SELECT password, role FROM users WHERE username = ?", username,
	).Scan(&stored, &role); err != nil {
		return nil
	}
	if !CheckPasswordHash(stored, password) {
		return nil
	}
	return &CurrentUser{Username: username, Role: role}
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
	// whose user was deleted matches no row, same as before.
	var username, role string
	var expires sql.NullInt64
	if err := db.QueryRow(
		"SELECT t.username, t.expires_at, u.role FROM tokens t "+
			"JOIN users u ON u.username = t.username WHERE t.token = ?", token,
	).Scan(&username, &expires, &role); err != nil {
		return nil
	}
	if expires.Valid && expires.Int64 != 0 && expires.Int64 < time.Now().Unix() {
		return nil
	}
	return &CurrentUser{Username: username, Role: role}
}

// Current returns the user from Basic or Bearer credentials, preferring Basic.
func Current(db *sql.DB, r *http.Request) *CurrentUser {
	if u := VerifyBasic(db, r); u != nil {
		return u
	}
	return VerifyBearer(db, r)
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
