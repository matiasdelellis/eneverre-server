// Package mediaauth signs short-lived, camera-scoped media grants.
//
// A grant authorizes one HTTP media surface of one camera — live MSE, the
// thumbnail, or the HLS recordings — without carrying a reusable password.
// The token is a query pair:
//
//	exp=<unix-seconds>&sig=<hex HMAC-SHA256>
//
// where sig covers the camera id, the scope and exp. The HMAC key is a random
// server secret kept in the database and never exposed through the API. The
// same key signs the RTSP relay path token; there is no separate global RTSP
// username/password to reuse.
//
// Grants are meant for clients that cannot set an Authorization header
// (browser <img>, HLS players). First-party clients keep using their normal
// session Bearer token. The API has no endpoint that issues a grant: the
// server mints one internally when an integration needs it, and the RTSP path
// token below is the one the HTTP handlers always mint.
package mediaauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Media scopes. One grant is valid only for the scope it was signed for.
const (
	ScopeLive  = "live"  // live MSE info + stream
	ScopeThumb = "thumb" // still JPEG thumbnail
	ScopeHLS   = "hls"   // HLS VOD playlist + init + segments
	ScopeRTSP  = "rtsp"  // RTSP relay path credential
)

// maxTokenLifetime bounds how far in the future a grant may claim to be valid.
// The publisher signs short lifetimes; this only stops a bug (or a leaked
// signing key) from minting effectively permanent links.
const maxTokenLifetime = 7 * 24 * time.Hour

// Store holds the signing key and produces/validates grants.
type Store struct {
	db *sql.DB

	mu  sync.RWMutex
	key []byte
}

// NewStore loads the signing key from the database, creating a random one on
// first use. It also creates the key table when the caller has not run
// store.Init yet (tests and embedders), so opening the store is idempotent.
func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("mediaauth: nil database")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS media_auth_key (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		secret TEXT NOT NULL,
		rotated_at INTEGER NOT NULL
	)`); err != nil {
		return nil, fmt.Errorf("mediaauth: creating the key table: %w", err)
	}

	var stored string
	err := db.QueryRow("SELECT secret FROM media_auth_key WHERE id = 1").Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		key, err := newKey()
		if err != nil {
			return nil, err
		}
		if _, err := db.Exec(
			"INSERT INTO media_auth_key (id, secret, rotated_at) VALUES (1, ?, ?)",
			hex.EncodeToString(key), time.Now().Unix(),
		); err != nil {
			return nil, fmt.Errorf("mediaauth: storing the signing key: %w", err)
		}
		return &Store{db: db, key: key}, nil
	case err != nil:
		return nil, fmt.Errorf("mediaauth: reading the signing key: %w", err)
	}

	key, err := hex.DecodeString(strings.TrimSpace(stored))
	if err != nil || len(key) < 16 {
		return nil, errors.New("mediaauth: the stored signing key is invalid")
	}
	return &Store{db: db, key: key}, nil
}

func newKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("mediaauth: generating the signing key: %w", err)
	}
	return key, nil
}

// TTL returns the recommended lifetime of a grant for a media scope. The
// publisher refreshes the camera inventory every sync interval, so these are
// intentionally short: a leaked URL stops working on its own.
func TTL(scope string) time.Duration {
	if scope == ScopeHLS || scope == ScopeRTSP {
		// A VOD playlist / an RTSP viewing session is opened once and then
		// used for a while; allow a long session without forcing a re-fetch.
		return 6 * time.Hour
	}
	return time.Hour
}

// Sign returns the grant query values for cameraID and scope, valid for ttl.
func (s *Store) Sign(cameraID, scope string, ttl time.Duration) (exp int64, sig string) {
	if ttl <= 0 || ttl > maxTokenLifetime {
		ttl = TTL(scope)
	}
	exp = time.Now().Add(ttl).Unix()
	return exp, s.SignAt(cameraID, scope, exp)
}

// SignAt signs a grant with an explicit expiry. It exists for tests and for
// callers that want to refresh a link with the same expiry.
func (s *Store) SignAt(cameraID, scope string, exp int64) string {
	s.mu.RLock()
	key := append([]byte(nil), s.key...)
	s.mu.RUnlock()
	return sign(key, cameraID, scope, exp)
}

// SignToken returns a single path-safe token ("<exp>.<sig>") for the RTSP
// relay. The relay path is /<camera-id>/<exp>.<sig>; a path segment avoids the
// RTSP URL query string, which gortsplib and some clients handle poorly.
func (s *Store) SignToken(cameraID, scope string, ttl time.Duration) string {
	exp, sig := s.Sign(cameraID, scope, ttl)
	return strconv.FormatInt(exp, 10) + "." + sig
}

// ValidateToken validates a token produced by SignToken.
func (s *Store) ValidateToken(cameraID, scope, token string) bool {
	expPart, sig, ok := strings.Cut(token, ".")
	if !ok || expPart == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expPart, 10, 64)
	if err != nil {
		return false
	}
	return s.Validate(cameraID, scope, exp, sig)
}

func sign(key []byte, cameraID, scope string, exp int64) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(cameraID))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(scope))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Validate reports whether sig authorizes cameraID/scope until exp. It is
// constant-time on the signature and never panics on malformed input.
func (s *Store) Validate(cameraID, scope string, exp int64, sig string) bool {
	now := time.Now().Unix()
	if exp <= now || exp > now+int64(maxTokenLifetime.Seconds()) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	s.mu.RLock()
	key := append([]byte(nil), s.key...)
	s.mu.RUnlock()
	want, err := hex.DecodeString(sign(key, cameraID, scope, exp))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}
