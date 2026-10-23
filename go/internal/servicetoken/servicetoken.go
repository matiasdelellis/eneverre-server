// Package servicetoken manages scoped, read-only bearer tokens for machine
// integrations (Zabbix, dashboards, scripts). Only a SHA-256 hash of each
// token is stored; the raw value is returned once at creation.
package servicetoken

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Prefix marks a service token so it is never mistaken for a user session.
const Prefix = "envr_svc_"

// Read-only scopes. A service token can only reach the endpoints that check
// one of these; it is rejected everywhere else (users, cameras administration,
// PTZ, privacy, talk, device login, uploads, ...).
const (
	ScopeStatus     = "status:read"
	ScopeCameras    = "cameras:read"
	ScopeEvents     = "events:read"
	ScopeRecordings = "recordings:read"
	ScopeMetrics    = "metrics:read"
)

var allScopes = []string{
	ScopeStatus,
	ScopeCameras,
	ScopeEvents,
	ScopeRecordings,
	ScopeMetrics,
}

// AllScopes returns the supported scopes.
func AllScopes() []string {
	return append([]string(nil), allScopes...)
}

// ValidScope reports whether scope is one of the supported read-only scopes.
func ValidScope(scope string) bool {
	for _, s := range allScopes {
		if s == scope {
			return true
		}
	}
	return false
}

var (
	// ErrInvalid is returned for a missing, malformed, expired or revoked
	// token. Callers must not distinguish these cases to the client.
	ErrInvalid = errors.New("invalid service token")
	// ErrNotFound is returned when revoking an unknown/already-revoked token.
	ErrNotFound = errors.New("service token not found")
)

// Token is the public metadata of a service token. The raw secret is never
// part of this struct.
type Token struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	CreatedAt  int64    `json:"created_at"`
	ExpiresAt  int64    `json:"expires_at"`
	LastUsedAt int64    `json:"last_used_at,omitempty"`
	RevokedAt  int64    `json:"revoked_at,omitempty"`
}

// HasScope reports whether the token carries scope.
func (t Token) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
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

// Create mints a token. The raw value is returned once and never stored; only
// its SHA-256 hash is persisted. scopes must be non-empty and supported.
func (s *Store) Create(name string, scopes []string, expiresAt int64) (string, Token, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Token{}, errors.New("name is required")
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
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
		Name:      name,
		Prefix:    display,
		Scopes:    scopes,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	res, err := s.db.Exec(
		"INSERT INTO service_tokens (name, prefix, token_hash, scopes, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		t.Name, t.Prefix, hash(raw), strings.Join(t.Scopes, " "), t.CreatedAt, t.ExpiresAt,
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
		"SELECT id, name, prefix, scopes, created_at, expires_at, last_used_at, revoked_at " +
			"FROM service_tokens ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var scopes string
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &scopes, &t.CreatedAt, &t.ExpiresAt,
			&t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		t.Scopes = strings.Fields(scopes)
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
// revocation and expiry, and refreshes last_used_at at most once a minute.
func (s *Store) Verify(raw string) (*Token, error) {
	if !strings.HasPrefix(raw, Prefix) {
		return nil, ErrInvalid
	}
	var t Token
	var scopes string
	err := s.db.QueryRow(
		"SELECT id, name, prefix, scopes, created_at, expires_at, last_used_at, revoked_at "+
			"FROM service_tokens WHERE token_hash = ?", hash(raw),
	).Scan(&t.ID, &t.Name, &t.Prefix, &scopes, &t.CreatedAt, &t.ExpiresAt,
		&t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	t.Scopes = strings.Fields(scopes)
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

func normalizeScopes(scopes []string) ([]string, error) {
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if !ValidScope(s) {
			return nil, fmt.Errorf("unknown scope %q", s)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one scope is required")
	}
	return out, nil
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
