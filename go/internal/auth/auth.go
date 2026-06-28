// Package auth provides Werkzeug-compatible password hashing and the
// Basic/Bearer authentication used by the API, porting app/auth.py.
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
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if token == "" {
		return nil
	}
	var username string
	var expires sql.NullInt64
	if err := db.QueryRow(
		"SELECT username, expires_at FROM tokens WHERE token = ?", token,
	).Scan(&username, &expires); err != nil {
		return nil
	}
	if expires.Valid && expires.Int64 != 0 && expires.Int64 < time.Now().Unix() {
		return nil
	}
	var role string
	if err := db.QueryRow(
		"SELECT role FROM users WHERE username = ?", username,
	).Scan(&role); err != nil {
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
