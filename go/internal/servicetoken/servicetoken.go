// Package servicetoken manages machine bearer tokens. A token is linked to a
// user and inherits that user's role and camera ACL; it cannot have a broader
// permission set than the user. Only a SHA-256 hash of the token is stored,
// and the raw value is returned once at creation.
package servicetoken

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Prefix marks a service token so it is never mistaken for a user session.
const Prefix = "envr_svc_"

var (
	// ErrInvalid is returned for a missing, malformed, expired, revoked or
	// orphaned token. Callers must not distinguish these cases to the client.
	ErrInvalid = errors.New("invalid service token")
	// ErrNotFound is returned when revoking an unknown/already-revoked token.
	ErrNotFound = errors.New("service token not found")
	// ErrUnknownUser is returned when creating a token for a user that does
	// not exist.
	ErrUnknownUser = errors.New("unknown user")
)

// Token is the public metadata of a service token. The raw secret is never
// part of this struct.
type Token struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`

	// MustChangePassword comes from the linked user and is used to enforce the
	// same bootstrap-password gate as a human session. Not exposed in JSON.
	MustChangePassword bool `json:"-"`
}

// Active reports whether the token is currently usable.
func (t Token) Active(now int64) bool {
	return t.RevokedAt == 0 && t.ExpiresAt > now
}

// Store persists service tokens in SQLite.
type Store struct {
	db *sql.DB
}

// NewStore returns a store over an initialized database.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Create mints a token for username. The raw value is returned once and never
// stored; only its SHA-256 hash is persisted. The linked user must exist; its
// role and ACL are resolved on every request, so a role/ACL change takes
// effect immediately.
func (s *Store) Create(name, username string, expiresAt int64) (string, Token, error) {
	name = strings.TrimSpace(name)
	username = strings.TrimSpace(username)
	if name == "" {
		return "", Token{}, errors.New("name is required")
	}
	if username == "" {
		return "", Token{}, errors.New("username is required")
	}
	var role string
	var mustChange bool
	if err := s.db.QueryRow(
		"SELECT role, must_change_password FROM users WHERE username = ?", username,
	).Scan(&role, &mustChange); errors.Is(err, sql.ErrNoRows) {
		return "", Token{}, ErrUnknownUser
	} else if err != nil {
		return "", Token{}, err
	}
	now := time.Now().Unix()
	if expiresAt <= now {
		return "", Token{}, errors.New("expires_at must be in the future")
	}

	secret, err := randomHex(32)
	if err != nil {
		return "", Token{}, err
	}
	raw := Prefix + secret
	display := raw[:len(Prefix)+8]
	t := Token{
		Name:               name,
		Username:           username,
		Role:               role,
		Prefix:             display,
		CreatedAt:          now,
		ExpiresAt:          expiresAt,
		MustChangePassword: mustChange,
	}
	// scopes is a legacy column from the first service-token iteration. It is
	// kept in the schema so an upgraded database accepts the insert; it is no
	// longer read.
	res, err := s.db.Exec(
		"INSERT INTO service_tokens (name, username, prefix, token_hash, scopes, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, '', ?, ?)",
		t.Name, t.Username, t.Prefix, hash(raw), t.CreatedAt, t.ExpiresAt,
	)
	if err != nil {
		return "", Token{}, err
	}
	t.ID, err = res.LastInsertId()
	if err != nil {
		return "", Token{}, err
	}
	return raw, t, nil
}

// List returns every token, including revoked/expired ones (the admin list
// needs to show them). It never returns the raw secret.
func (s *Store) List() ([]Token, error) {
	rows, err := s.db.Query(
		"SELECT st.id, st.name, st.username, COALESCE(u.role, ''), st.prefix, " +
			"st.created_at, st.expires_at, st.last_used_at, st.revoked_at " +
			"FROM service_tokens st LEFT JOIN users u ON u.username = st.username " +
			"ORDER BY st.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Username, &t.Role, &t.Prefix,
			&t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke marks a token unusable. It returns ErrNotFound when the id does not
// exist or is already revoked.
func (s *Store) Revoke(id int64) error {
	res, err := s.db.Exec(
		"UPDATE service_tokens SET revoked_at = ? WHERE id = ? AND revoked_at = 0",
		time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Verify authenticates a raw bearer token. It checks the prefix, hash,
// revocation and expiry, and joins the linked user so the caller gets the
// current role and ACL. It refreshes last_used_at at most once a minute.
func (s *Store) Verify(raw string) (*Token, error) {
	if !strings.HasPrefix(raw, Prefix) {
		return nil, ErrInvalid
	}
	var t Token
	err := s.db.QueryRow(
		"SELECT st.id, st.name, st.username, u.role, st.prefix, st.created_at, "+
			"st.expires_at, st.last_used_at, st.revoked_at, u.must_change_password "+
			"FROM service_tokens st JOIN users u ON u.username = st.username "+
			"WHERE st.token_hash = ?", hash(raw),
	).Scan(&t.ID, &t.Name, &t.Username, &t.Role, &t.Prefix, &t.CreatedAt,
		&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt, &t.MustChangePassword)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if !t.Active(now) {
		return nil, ErrInvalid
	}
	if t.LastUsedAt == 0 || t.LastUsedAt < now-60 {
		if _, err := s.db.Exec(
			"UPDATE service_tokens SET last_used_at = ? WHERE id = ? AND revoked_at = 0",
			now, t.ID); err == nil {
			t.LastUsedAt = now
		}
	}
	return &t, nil
}

func hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ParseID parses a path id for the revoke endpoint.
func ParseID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
