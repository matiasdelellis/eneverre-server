package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"eneverre/internal/auth"
)

func validRole(role string) bool { return role == "admin" || role == "user" }

// maxUsernameLen bounds a username. Usernames are looked up verbatim and shown
// in the session/user lists, so we keep them short, non-empty and free of
// surrounding or embedded whitespace (which would make lookups ambiguous).
const maxUsernameLen = 64

// cleanUsername trims a username and reports whether it is acceptable. It
// rejects empty, over-long, and whitespace-containing values. The returned
// string is the trimmed form to store.
func cleanUsername(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxUsernameLen {
		return "", false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", false
	}
	return s, true
}

// --- admin: list / create -------------------------------------------------

func (a *App) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	rows, err := a.db.Query("SELECT username, role, first_name, last_name, must_change_password FROM users")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Query failed")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var username, role string
		var firstName, lastName sql.NullString
		var mustChange bool
		if err := rows.Scan(&username, &role, &firstName, &lastName, &mustChange); err != nil {
			httpError(w, http.StatusInternalServerError, "Scan failed")
			return
		}
		out = append(out, map[string]any{
			"username":             username,
			"role":                 role,
			"first_name":           nullStrPtr(firstName),
			"last_name":            nullStrPtr(lastName),
			"must_change_password": mustChange,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Username  string  `json:"username"`
	Password  string  `json:"password"`
	Role      string  `json:"role"`
	FirstName *string `json:"first_name"`
	LastName  *string `json:"last_name"`
	// MustChangePassword, when true, forces the new user through the
	// change-password flow on their first login. Optional (defaults to false).
	MustChangePassword bool `json:"must_change_password"`
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	username, ok := cleanUsername(req.Username)
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "username is required (max 64 chars, no whitespace)")
		return
	}
	// A password is mandatory (an empty one would be a usable blank-password
	// account) and bounded — the same limit the login/change paths enforce, so a
	// create can never store a password that login would then refuse to hash.
	if req.Password == "" {
		httpError(w, http.StatusUnprocessableEntity, "password is required")
		return
	}
	if passwordTooLong(w, req.Password) {
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	if !validRole(req.Role) {
		httpError(w, http.StatusUnprocessableEntity, "role must be 'admin' or 'user'")
		return
	}
	_, err := a.db.Exec(
		"INSERT INTO users (username, password, role, first_name, last_name, must_change_password) VALUES (?, ?, ?, ?, ?, ?)",
		username, auth.GeneratePasswordHash(req.Password), req.Role, req.FirstName, req.LastName, req.MustChangePassword,
	)
	if err != nil {
		// UNIQUE/PRIMARY KEY violation on username.
		httpError(w, http.StatusBadRequest, "User exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message":              "User created",
		"username":             username,
		"role":                 req.Role,
		"first_name":           req.FirstName,
		"last_name":            req.LastName,
		"must_change_password": req.MustChangePassword,
	})
}

// --- self: password / name / sessions ------------------------------------

type selfUpdatePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (a *App) handleChangeMyPassword(w http.ResponseWriter, r *http.Request) {
	me := a.requireUser(w, r)
	if me == nil {
		return
	}
	var req selfUpdatePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NewPassword == "" {
		httpError(w, http.StatusUnprocessableEntity, "new_password is required")
		return
	}
	if passwordTooLong(w, req.NewPassword) {
		return
	}
	var stored string
	err := a.db.QueryRow("SELECT password FROM users WHERE username = ?", me.Username).Scan(&stored)
	if err != nil || !auth.CheckPasswordHash(stored, req.CurrentPassword) {
		httpError(w, http.StatusBadRequest, "Current password is incorrect")
		return
	}
	// Changing your own password satisfies any pending force-change flag.
	if _, err := a.db.Exec("UPDATE users SET password = ?, must_change_password = 0 WHERE username = ?",
		auth.GeneratePasswordHash(req.NewPassword), me.Username); err != nil {
		httpError(w, http.StatusInternalServerError, "Could not update password")
		return
	}
	// A password change must not leave old sessions valid — otherwise a token
	// stolen before the reset survives it. Revoke every other session for this
	// user but keep the caller's current token so they aren't logged out mid-change.
	current := auth.BearerToken(r)
	_, _ = a.db.Exec("DELETE FROM tokens WHERE username = ? AND token != ?", me.Username, current)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Password updated"})
}

type updateNameRequest struct {
	FirstName *string `json:"first_name"`
	LastName  *string `json:"last_name"`
}

func (a *App) handleChangeMyName(w http.ResponseWriter, r *http.Request) {
	me := a.requireUser(w, r)
	if me == nil {
		return
	}
	var req updateNameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := a.db.Exec("UPDATE users SET first_name = ?, last_name = ? WHERE username = ?",
		req.FirstName, req.LastName, me.Username); err != nil {
		httpError(w, http.StatusInternalServerError, "Could not update name")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":    "Name updated",
		"first_name": req.FirstName,
		"last_name":  req.LastName,
	})
}

func (a *App) handleListMySessions(w http.ResponseWriter, r *http.Request) {
	me := a.requireUser(w, r)
	if me == nil {
		return
	}
	current := auth.BearerToken(r)
	rows, err := a.db.Query(
		"SELECT id, token, created_at, expires_at, device_name, refresh_token, refresh_expires_at FROM tokens "+
			"WHERE username = ? ORDER BY id DESC", me.Username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Query failed")
		return
	}
	defer rows.Close()

	now := time.Now().Unix()
	active := []map[string]any{}
	expired := []map[string]any{}
	for rows.Next() {
		var id int64
		var token string
		var createdAt, expiresAt, refreshExpiresAt sql.NullInt64
		var deviceName, refreshToken sql.NullString
		if err := rows.Scan(&id, &token, &createdAt, &expiresAt, &deviceName, &refreshToken, &refreshExpiresAt); err != nil {
			httpError(w, http.StatusInternalServerError, "Scan failed")
			return
		}
		sum := sha256.Sum256([]byte(token))
		fingerprint := hex.EncodeToString(sum[:])[:10]
		// A renewable session (password login) lives as long as its refresh
		// token is valid; the short-lived access token expiry would otherwise
		// make an active phone look expired between refreshes.
		exp := expiresAt.Int64
		renewable := refreshToken.Valid
		if renewable {
			exp = refreshExpiresAt.Int64
		}
		isExpired := exp > 0 && exp < now
		entry := map[string]any{
			"id":          id,
			"fingerprint": fingerprint,
			"created_at":  createdAt.Int64,
			"expires_at":  exp,
			"renewable":   renewable,
			"is_current":  current != "" && token == current,
			"device_name": nullStrPtr(deviceName),
		}
		if isExpired {
			expired = append(expired, entry)
		} else {
			active = append(active, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "expired": expired})
}

func (a *App) handleRevokeMySession(w http.ResponseWriter, r *http.Request) {
	me := a.requireUser(w, r)
	if me == nil {
		return
	}
	sessionID, err := strconv.ParseInt(r.PathValue("session_id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "Session not found")
		return
	}
	res, err := a.db.Exec("DELETE FROM tokens WHERE id = ? AND username = ?", sessionID, me.Username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not revoke session")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "Session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Session revoked"})
}

// --- admin: role / password / name / delete ------------------------------

// isLastAdmin reports whether username is an admin and the only one left, so
// demoting or deleting it would strand the system with no administrator
// (recoverable only by re-seeding an empty users table).
func (a *App) isLastAdmin(username string) bool {
	var role string
	if err := a.db.QueryRow("SELECT role FROM users WHERE username = ?", username).Scan(&role); err != nil {
		return false // not found / error: let the caller's normal path report it
	}
	if role != "admin" {
		return false
	}
	var admins int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM users WHERE role = 'admin'").Scan(&admins); err != nil {
		return false
	}
	return admins <= 1
}

type updateRoleRequest struct {
	Role string `json:"role"`
}

func (a *App) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req updateRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validRole(req.Role) {
		httpError(w, http.StatusUnprocessableEntity, "role must be 'admin' or 'user'")
		return
	}
	if req.Role != "admin" && a.isLastAdmin(r.PathValue("username")) {
		httpError(w, http.StatusBadRequest, "Cannot demote the last admin")
		return
	}
	res, err := a.db.Exec("UPDATE users SET role = ? WHERE username = ?", req.Role, r.PathValue("username"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not update role")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "User not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Role updated"})
}

type updatePasswordRequest struct {
	Password string `json:"password"`
	// MustChangePassword forces the target user to change this password on
	// their next login. Optional; omitted/false clears any pending flag.
	MustChangePassword bool `json:"must_change_password"`
}

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	var req updatePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		httpError(w, http.StatusUnprocessableEntity, "password is required")
		return
	}
	if passwordTooLong(w, req.Password) {
		return
	}
	username := r.PathValue("username")
	res, err := a.db.Exec("UPDATE users SET password = ?, must_change_password = ? WHERE username = ?",
		auth.GeneratePasswordHash(req.Password), req.MustChangePassword, username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not update password")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "User not found")
		return
	}
	// An admin-forced reset revokes all of the target user's sessions, so a
	// compromised token can't outlive the reset.
	_, _ = a.db.Exec("DELETE FROM tokens WHERE username = ?", username)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Password updated"})
}

func (a *App) handleChangeName(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	username := r.PathValue("username")
	var req updateNameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := a.db.Exec("UPDATE users SET first_name = ?, last_name = ? WHERE username = ?",
		req.FirstName, req.LastName, username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not update name")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if a.db.QueryRow("SELECT 1 FROM users WHERE username = ?", username).Scan(&one) == sql.ErrNoRows {
			httpError(w, http.StatusNotFound, "User not found")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":    "Name updated",
		"first_name": req.FirstName,
		"last_name":  req.LastName,
	})
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	username := r.PathValue("username")
	if a.isLastAdmin(username) {
		httpError(w, http.StatusBadRequest, "Cannot delete the last admin")
		return
	}
	res, err := a.db.Exec("DELETE FROM users WHERE username = ?", username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not delete user")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "User not found")
		return
	}
	// Best-effort cleanup of the deleted user's tokens.
	_, _ = a.db.Exec("DELETE FROM tokens WHERE username = ?", username)
	writeJSON(w, http.StatusOK, map[string]string{"message": "User deleted"})
}
