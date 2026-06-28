// Package mediamtx holds the MediaMTX credentials and builds the authenticated
// stream URLs handed to clients. It is the Go port of
// app/services/mediamtx_service.py, extended with automatic credential
// rotation.
package mediamtx

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Creds is the username/password pair MediaMTX validates (via /api/auth) and
// that is embedded in the stream URLs returned by /api/cameras.
type Creds struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func gen(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		idx, _ := rand.Int(rand.Reader, max)
		b[i] = alphabet[idx.Int64()]
	}
	return string(b)
}

// normalize collapses a path prefix to "" or "/<trimmed>", matching the
// trailing-slash handling in the Python URL builders.
func normalize(path string) string {
	p := strings.Trim(path, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

// RtspURL builds rtsp://user:pass@server:port/cam.
func (c Creds) RtspURL(server, port, camID string) string {
	return fmt.Sprintf("rtsp://%s:%s@%s:%s/%s", c.Username, c.Password, server, port, camID)
}

// WebrtcURL builds https://user:pass@server<path>/cam.
func (c Creds) WebrtcURL(server, path, camID string) string {
	return fmt.Sprintf("https://%s:%s@%s%s/%s", c.Username, c.Password, server, normalize(path), camID)
}

// HlsURL builds https://user:pass@server<path>/cam/index.m3u8.
func (c Creds) HlsURL(server, path, camID string) string {
	return fmt.Sprintf("https://%s:%s@%s%s/%s/index.m3u8", c.Username, c.Password, server, normalize(path), camID)
}

// Store holds the current credentials and rotates them automatically. It keeps
// the immediately previous credentials valid for one rotation interval (a grace
// window) so a client that already holds an old stream URL is not dropped the
// instant credentials rotate. Reads (Current/Validate) are concurrency-safe and
// served from memory; the DB is only touched at startup and on rotation.
type Store struct {
	db *sql.DB

	mu   sync.RWMutex
	cur  Creds
	prev Creds // grace-window credentials; zero value never matches
}

// NewStore loads the credential pair from the `mediamtx_credentials` table
// (which store.Init must have created), or generates and persists a fresh pair
// when the table is empty.
func NewStore(db *sql.DB) (*Store, error) {
	s := &Store{db: db}

	var c Creds
	err := db.QueryRow("SELECT username, password FROM mediamtx_credentials WHERE id = 1").
		Scan(&c.Username, &c.Password)
	switch {
	case err == nil && c.Username != "" && c.Password != "":
		s.cur = c
		return s, nil
	case err != nil && err != sql.ErrNoRows:
		return nil, err
	}

	s.cur = Creds{Username: gen(8), Password: gen(8)}
	if err := s.persist(s.cur); err != nil {
		return nil, err
	}
	slog.Info("mediamtx credentials generated and stored in the database")
	return s, nil
}

// Current returns the credentials to embed in freshly built stream URLs.
func (s *Store) Current() Creds {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Validate reports whether user/pass match the current or grace-window
// credentials, in constant time.
func (s *Store) Validate(user, pass string) bool {
	s.mu.RLock()
	cur, prev := s.cur, s.prev
	s.mu.RUnlock()
	return credsMatch(cur, user, pass) || credsMatch(prev, user, pass)
}

func credsMatch(c Creds, user, pass string) bool {
	if c.Username == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Username), []byte(user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(c.Password), []byte(pass)) == 1
}

// Rotate generates a new credential pair, demotes the current pair to the
// grace window, and persists the new pair. The previous grace-window pair (two
// rotations old) is discarded.
func (s *Store) Rotate() (Creds, error) {
	next := Creds{Username: gen(8), Password: gen(8)}
	s.mu.Lock()
	s.prev = s.cur
	s.cur = next
	s.mu.Unlock()
	if err := s.persist(next); err != nil {
		return next, err
	}
	return next, nil
}

// persist upserts the credential pair into the single-row table. Called only on
// first run and on rotation, so the per-request path never hits the DB.
func (s *Store) persist(c Creds) error {
	_, err := s.db.Exec(
		`INSERT INTO mediamtx_credentials (id, username, password, rotated_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     username = excluded.username,
		     password = excluded.password,
		     rotated_at = excluded.rotated_at`,
		c.Username, c.Password, time.Now().Unix(),
	)
	return err
}

// StartRotation rotates credentials every interval in a background goroutine.
// A non-positive interval disables rotation. Errors are logged and rotation
// continues on the next tick.
func (s *Store) StartRotation(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if _, err := s.Rotate(); err != nil {
				slog.Error("mediamtx credential rotation failed", "err", err)
				continue
			}
			slog.Info("mediamtx credentials rotated")
		}
	}()
}
