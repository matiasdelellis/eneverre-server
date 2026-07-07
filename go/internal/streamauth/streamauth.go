// Package streamauth holds the rotating username/password pair the embedded
// RTSP relay authenticates against (and that is embedded in the relay URLs
// returned by /api/cameras).
//
// The pair is generated on first start (random 8/8 alphanumeric) and rotated
// on a schedule. The previous pair stays valid for one rotation interval
// (a grace window) so a reader that already holds an old URL is not dropped
// the instant the pair rolls. Reads (Current/Validate) are concurrency-safe
// and served from memory; the DB is only touched at startup and on rotation.
package streamauth

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Creds is a username/password pair.
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

// RtspURL builds rtsp://user:pass@host:port/cam — the relay URL embedded in
// the camera response by /api/cameras.
func (c Creds) RtspURL(host, port, camID string) string {
	return fmt.Sprintf("rtsp://%s:%s@%s:%s/%s", c.Username, c.Password, host, port, camID)
}

// Store holds the current credentials and rotates them automatically. The
// immediately previous credentials stay valid for one rotation interval so a
// reader that already holds an old stream URL is not dropped the instant
// credentials rotate.
type Store struct {
	db *sql.DB

	mu   sync.RWMutex
	cur  Creds
	prev Creds // grace-window credentials; zero value never matches
}

// NewStore loads the credential pair from the `streamauth_credentials` table
// (which store.Init must have created), or generates and persists a fresh pair
// when the table is empty.
func NewStore(db *sql.DB) (*Store, error) {
	s := &Store{db: db}

	var c Creds
	err := db.QueryRow("SELECT username, password FROM streamauth_credentials WHERE id = 1").
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
	slog.Info("stream-auth credentials generated and stored in the database")
	return s, nil
}

// Current returns the credentials to embed in freshly built stream URLs.
func (s *Store) Current() Creds {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Pairs returns the currently-valid [username, password] pairs: the current
// pair, plus the grace-window (previous) pair when one exists. Used to authorize
// the embedded RTSP relay against rotating credentials without dropping readers
// mid-rotation.
func (s *Store) Pairs() [][2]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := [][2]string{{s.cur.Username, s.cur.Password}}
	if s.prev.Username != "" {
		out = append(out, [2]string{s.prev.Username, s.prev.Password})
	}
	return out
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
		`INSERT INTO streamauth_credentials (id, username, password, rotated_at)
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
				slog.Error("stream-auth credential rotation failed", "err", err)
				continue
			}
			slog.Info("stream-auth credentials rotated")
		}
	}()
}
