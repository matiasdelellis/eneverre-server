package server

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"eneverre/internal/auth"
)

// maxPasswordLen bounds every password handed to PBKDF2 (login verify and the
// set/change-password endpoints). A multi-MB password would make each hash
// catastrophically CPU-heavy (600k iterations), so we reject it before hashing.
// Enforcing the same limit on the write paths means an oversized password can
// never be stored, so login never has to reconcile one. Real passwords are far
// shorter, so legitimate users are unaffected.
const maxPasswordLen = 1024

// passwordTooLong reports whether pw exceeds maxPasswordLen and, if so, writes a
// 422. Rejecting on length alone leaks nothing about a username, so it is safe
// on the unauthenticated login path.
func passwordTooLong(w http.ResponseWriter, pw string) bool {
	if len(pw) > maxPasswordLen {
		httpError(w, http.StatusUnprocessableEntity, "Password too long")
		return true
	}
	return false
}

// Token lifetimes are configurable via the [auth] section
// (access_token_ttl_hours / refresh_token_ttl_days) and resolved into
// a.accessTTL / a.refreshTTL (seconds) at startup; see server.New.

// --- browser login / logout ----------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// DeviceName is an optional human label for the session (e.g. "Chrome on
	// laptop"). When set it is stored on the token so the session manager can
	// distinguish logins, matching the device-login flow. Empty -> NULL.
	DeviceName string `json:"device_name"`
}

type loginResponse struct {
	Token            string  `json:"token"`
	ExpiresAt        int64   `json:"expires_at"`
	RefreshToken     string  `json:"refresh_token"`
	RefreshExpiresAt int64   `json:"refresh_expires_at"`
	Username         string  `json:"username"`
	FirstName        *string `json:"first_name"`
	LastName         *string `json:"last_name"`
	Role             string  `json:"role"`
	IsAdmin          bool    `json:"is_admin"`
	// MustChangePassword tells the client to force the user through the
	// change-password flow before using the app (set for the seeded admin and
	// whenever an admin flags an account). Cleared by a successful self change.
	MustChangePassword bool `json:"must_change_password"`
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if passwordTooLong(w, req.Password) {
		return
	}
	// Throttle check before the DB hit and (crucially) before any PBKDF2 pass.
	if blocked, wait := a.authThrottle.blocked(remoteIP(r), req.Username); blocked {
		throttleExceeded(w, wait)
		return
	}
	var stored, role string
	var firstName, lastName sql.NullString
	var mustChange bool
	err := a.db.QueryRow(
		"SELECT password, role, first_name, last_name, must_change_password FROM users WHERE username = ?",
		req.Username,
	).Scan(&stored, &role, &firstName, &lastName, &mustChange)
	if err != nil {
		// Unknown user: hash a dummy so a missing account costs the same as a
		// present one. Doing this ONLY here (not also after a real hash on the
		// wrong-password path) keeps both branches at exactly one PBKDF2 pass —
		// otherwise "valid user, wrong password" would run two hashes and take
		// ~2x as long, leaking which usernames exist.
		auth.CheckPasswordHash("pbkdf2:sha256:600000$dummy$"+strings.Repeat("0", 64), req.Password)
		a.authThrottle.fail(remoteIP(r), req.Username)
		a.logAuthFailure(r, req.Username, "invalid_credentials")
		httpError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	if !auth.CheckPasswordHash(stored, req.Password) {
		a.authThrottle.fail(remoteIP(r), req.Username)
		a.logAuthFailure(r, req.Username, "invalid_credentials")
		httpError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	a.authThrottle.success(req.Username)
	token := auth.TokenURLSafe(32)
	refresh := auth.TokenURLSafe(32)
	now := time.Now().Unix()
	expiresAt := now + a.accessTTL
	refreshExpiresAt := now + a.refreshTTL
	deviceName := cleanDeviceName(req.DeviceName)
	// No client-supplied label: derive one from the User-Agent so browser
	// logins ("Chrome · Linux") are distinguishable in the session list
	// instead of showing blank. Best-effort and spoofable — not a security id.
	if !deviceName.Valid {
		deviceName = cleanDeviceName(deviceNameFromUA(r.UserAgent()))
	}
	a.cleanupExpiredTokens()
	if _, err := a.db.Exec(
		"INSERT INTO tokens (token, username, expires_at, created_at, device_name, refresh_token, refresh_expires_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		token, req.Username, expiresAt, now, deviceName, refresh, refreshExpiresAt,
	); err != nil {
		httpError(w, http.StatusInternalServerError, "Could not create session")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		Token:              token,
		ExpiresAt:          expiresAt,
		RefreshToken:       refresh,
		RefreshExpiresAt:   refreshExpiresAt,
		Username:           req.Username,
		FirstName:          nullStrPtr(firstName),
		LastName:           nullStrPtr(lastName),
		Role:               role,
		IsAdmin:            role == "admin",
		MustChangePassword: mustChange,
	})
}

// cleanupExpiredTokens deletes dead sessions: renewable ones past their
// refresh-window + grace, and non-renewable (device-flow or legacy) ones past
// their access expiry + grace. The grace keeps tokens visible in the sessions
// list for a while after they expire. Called on every login and on a
// background ticker when [auth] cleanup_interval_minutes > 0.
func (a *App) cleanupExpiredTokens() {
	cut := time.Now().Unix() - a.cleanupGrace
	_, _ = a.db.Exec(
		"DELETE FROM tokens WHERE (refresh_token IS NOT NULL AND refresh_expires_at < ?) "+
			"OR (refresh_token IS NULL AND expires_at < ?)",
		cut, cut,
	)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// handleRefresh exchanges a valid refresh token for a fresh access token,
// rotating both secrets in place on the same row (so the session list grows
// per login, never per refresh) and sliding both expiries forward. Device-flow
// sessions have a NULL refresh_token and so can never refresh — they re-pair.
func (a *App) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		httpError(w, http.StatusUnauthorized, "Invalid refresh token")
		return
	}
	now := time.Now().Unix()
	newToken := auth.TokenURLSafe(32)
	newRefresh := auth.TokenURLSafe(32)
	expiresAt := now + a.accessTTL
	newRefreshExpiresAt := now + a.refreshTTL
	// Rotate in a single conditional UPDATE rather than SELECT-then-UPDATE: the
	// old refresh token is consumed atomically, so two concurrent requests with
	// the same token can't both mint a new pair — the first flips refresh_token,
	// the second's WHERE no longer matches (RowsAffected 0). (NULL/0
	// refresh_expires_at means "never expires", matching the old semantics.)
	res, err := a.db.Exec(
		"UPDATE tokens SET token = ?, expires_at = ?, refresh_token = ?, refresh_expires_at = ? "+
			"WHERE refresh_token = ? AND (refresh_expires_at IS NULL OR refresh_expires_at = 0 OR refresh_expires_at > ?)",
		newToken, expiresAt, newRefresh, newRefreshExpiresAt, req.RefreshToken, now,
	)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not refresh session")
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Nothing rotated: the token is unknown, expired, or already consumed (a
		// reused/stale token or a lost race). Never mint a new pair; opportunistically
		// prune the row if it was simply expired.
		_, _ = a.db.Exec(
			"DELETE FROM tokens WHERE refresh_token = ? AND refresh_expires_at > 0 AND refresh_expires_at < ?",
			req.RefreshToken, now,
		)
		httpError(w, http.StatusUnauthorized, "Invalid refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":              newToken,
		"expires_at":         expiresAt,
		"refresh_token":      newRefresh,
		"refresh_expires_at": newRefreshExpiresAt,
	})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	if token := auth.BearerToken(r); token != "" {
		_, _ = a.db.Exec("DELETE FROM tokens WHERE token = ?", token)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Logged out"})
}

// --- device login flow ----------------------------------------------------

const deviceNameMax = 120

func cleanDeviceName(raw string) sql.NullString {
	s := strings.TrimSpace(raw)
	if s == "" {
		return sql.NullString{}
	}
	if len(s) > deviceNameMax {
		s = s[:deviceNameMax]
	}
	return sql.NullString{String: s, Valid: true}
}

func (a *App) cleanupExpiredDevices() {
	_, _ = a.db.Exec("DELETE FROM device_login WHERE expires_at < ?", time.Now().Unix())
}

func (a *App) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	a.cleanupExpiredDevices()
	deviceCode := auth.TokenURLSafe(16)
	// 4 bytes = 8 hex chars = 32 bits of entropy. The verify path is
	// authenticated and codes expire in 300s, but 3 bytes (24 bits) was thin for
	// a user-facing pairing code; 4 keeps it short to compare while removing any
	// realistic guessing margin.
	userCode := strings.ToUpper(auth.TokenHex(4))
	expiresAt := time.Now().Unix() + 300
	name := cleanDeviceName(r.URL.Query().Get("device_name"))
	if _, err := a.db.Exec(
		"INSERT INTO device_login (device_code, user_code, status, username, expires_at, device_name) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		deviceCode, userCode, "pending", nil, expiresAt, name,
	); err != nil {
		httpError(w, http.StatusInternalServerError, "Could not create device code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": deviceCode,
		"user_code":   userCode,
		"expires_in":  300,
		"device_name": nullStrPtr(name),
	})
}

func (a *App) handleCheckDevice(w http.ResponseWriter, r *http.Request) {
	deviceCode := r.PathValue("device_code")
	var status string
	var username, deviceName sql.NullString
	var expiresAt int64
	err := a.db.QueryRow(
		"SELECT status, username, expires_at, device_name FROM device_login WHERE device_code = ?",
		deviceCode,
	).Scan(&status, &username, &expiresAt, &deviceName)
	if err == sql.ErrNoRows {
		httpError(w, http.StatusNotFound, "Invalid device")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Lookup failed")
		return
	}
	name := nullStrPtr(deviceName)
	now := time.Now().Unix()

	if status == "expired" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "device_name": name})
		return
	}
	if expiresAt < now {
		_, _ = a.db.Exec("UPDATE device_login SET status='expired' WHERE device_code = ?", deviceCode)
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "device_name": name})
		return
	}
	if status == "approved" {
		token := auth.TokenURLSafe(16)
		// Device (TV) sessions get an access token with a fixed life and no
		// refresh_token, so they cannot be extended — the device re-pairs.
		tokenExpiresAt := now + a.accessTTL
		_, _ = a.db.Exec(
			"INSERT INTO tokens (token, username, expires_at, created_at, device_name) VALUES (?, ?, ?, ?, ?)",
			token, username, tokenExpiresAt, now, deviceName,
		)
		_, _ = a.db.Exec("UPDATE device_login SET status='expired' WHERE device_code = ?", deviceCode)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "approved",
			"token":       token,
			"expires_at":  tokenExpiresAt,
			"device_name": name,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "device_name": name})
}

type verifyDeviceRequest struct {
	UserCode string `json:"user_code"`
}

func (a *App) handleVerifyDevice(w http.ResponseWriter, r *http.Request) {
	user := a.requireUser(w, r)
	if user == nil {
		return
	}
	var req verifyDeviceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var status string
	var username, deviceName sql.NullString
	var expiresAt int64
	err := a.db.QueryRow(
		"SELECT status, username, expires_at, device_name FROM device_login WHERE user_code = ?",
		req.UserCode,
	).Scan(&status, &username, &expiresAt, &deviceName)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, map[string]any{"status": "invalid", "device_name": nil})
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Lookup failed")
		return
	}
	name := nullStrPtr(deviceName)

	if status == "approved" || status == "expired" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "device_name": name})
		return
	}
	if expiresAt < time.Now().Unix() {
		_, _ = a.db.Exec("UPDATE device_login SET status='expired' WHERE user_code = ?", req.UserCode)
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "device_name": name})
		return
	}
	_, _ = a.db.Exec(
		"UPDATE device_login SET status='approved', username = ? WHERE user_code = ?",
		user.Username, req.UserCode,
	)
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "device_name": name})
}

func nullStrPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}
