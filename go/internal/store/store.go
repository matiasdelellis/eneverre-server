// Package store opens the SQLite database and bootstraps its schema. It uses
// the pure-Go modernc.org/sqlite driver (no CGO).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"eneverre/internal/auth"
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS users (
		username TEXT PRIMARY KEY,
		password TEXT NOT NULL,
		fullname TEXT,
		first_name TEXT,
		last_name TEXT,
		role TEXT NOT NULL,
		must_change_password INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS device_login (
		device_code TEXT PRIMARY KEY,
		user_code TEXT,
		status TEXT,
		username TEXT,
		expires_at INTEGER,
		device_name TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT NOT NULL UNIQUE,
		username TEXT,
		expires_at INTEGER,
		created_at INTEGER,
		device_name TEXT,
		refresh_token TEXT,
		refresh_expires_at INTEGER,
		role TEXT,
		refresh_prev TEXT,
		refresh_prev_used_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_device_login_user_code ON device_login(user_code)`,
	`CREATE INDEX IF NOT EXISTS idx_tokens_username ON tokens(username)`,
	// handleRefresh looks sessions up by refresh_token on every renewal; without
	// this index that is a full-table scan.
	`CREATE INDEX IF NOT EXISTS idx_tokens_refresh ON tokens(refresh_token)`,
	// Scoped read-only tokens for machine integrations (Zabbix, dashboards).
	// Only a SHA-256 hash of the token is stored; the raw value is shown once
	// by the create endpoint. scopes is a space-separated list.
	`CREATE TABLE IF NOT EXISTS service_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		prefix TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		scopes TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		revoked_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_service_tokens_revoked ON service_tokens(revoked_at)`,
	`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		camera_id TEXT NOT NULL,
		start_ts INTEGER NOT NULL,
		end_ts INTEGER NOT NULL,
		type TEXT NOT NULL DEFAULT 'motion',
		source TEXT,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_camera_start ON events(camera_id, start_ts)`,
	// The global RTSP credential pair was replaced by per-camera signed path
	// tokens. Drop the old table on upgrade so a dead reusable secret does not
	// stay behind in the database.
	`DROP TABLE IF EXISTS streamauth_credentials`,
	// Single-row HMAC key for the short-lived HTTP media grants and the RTSP
	// path tokens. It is the only relay/media signing secret; there is no
	// global RTSP username/password.
	`CREATE TABLE IF NOT EXISTS media_auth_key (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		secret TEXT NOT NULL,
		rotated_at INTEGER NOT NULL
	)`,
	// Cameras are DB-backed: this table is the source of truth. The per-camera
	// .ini files under [server] cameras_dir are only an initial seed, imported
	// once when this table is empty (see camera.SeedFromINI); after that they
	// are ignored and cameras are created/deleted through the API. Booleans are
	// stored as 0/1 with the same defaults the INI parser applies; the thingino
	// PTZ coordinates default to -1 ("unset"). sort_order preserves the display
	// order (the INI seed used the alphabetical filename order).
	`CREATE TABLE IF NOT EXISTS cameras (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		comment TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',
		backchannel TEXT NOT NULL DEFAULT '',
		snapshot_url TEXT NOT NULL DEFAULT '',
		transport TEXT NOT NULL DEFAULT '',
		record INTEGER NOT NULL DEFAULT 1,
		mse INTEGER NOT NULL DEFAULT 1,
		relay INTEGER NOT NULL DEFAULT 1,
		privacy INTEGER NOT NULL DEFAULT 1,
		playback INTEGER NOT NULL DEFAULT 1,
		width INTEGER NOT NULL DEFAULT 16,
		height INTEGER NOT NULL DEFAULT 9,
		thingino_url TEXT NOT NULL DEFAULT '',
		thingino_api_key TEXT NOT NULL DEFAULT '',
		ptz INTEGER NOT NULL DEFAULT 0,
		home_x REAL NOT NULL DEFAULT -1,
		home_y REAL NOT NULL DEFAULT -1,
		privacy_x REAL NOT NULL DEFAULT -1,
		privacy_y REAL NOT NULL DEFAULT -1,
		pan_steps INTEGER NOT NULL DEFAULT 2130,
		pan_degrees INTEGER NOT NULL DEFAULT 360,
		tilt_steps INTEGER NOT NULL DEFAULT 1600,
		tilt_degrees INTEGER NOT NULL DEFAULT 180,
		fov_h REAL NOT NULL DEFAULT 113.0,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0,
		schedule_id TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1
	)`,
	// Recording schedules (named programs). A camera references one by its
	// schedule_id; the server pauses the camera's pipeline outside the schedule's
	// armed windows (see internal/schedule). An empty schedule_id means "record
	// 24/7", the historical default. `rules` is the per-weekday windows as a JSON
	// object (see schedule.Schedule.Days).
	`CREATE TABLE IF NOT EXISTS schedules (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		rules TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL DEFAULT 0
	)`,
}

// migrations are idempotent ALTER statements applied after the base schema, for
// columns added to tables that pre-date them. Each runs on every boot; a
// "duplicate column name" error means the column is already present on this
// install and is treated as success. New installs get the column from the
// CREATE TABLE above and these become no-ops.
var migrations = []string{
	// schedule_id was added after the cameras table shipped; existing DBs need
	// it backfilled to the same default the CREATE TABLE uses.
	`ALTER TABLE cameras ADD COLUMN schedule_id TEXT NOT NULL DEFAULT ''`,
	// enabled was added after the cameras table shipped; existing DBs need
	// it backfilled to the same default the CREATE TABLE uses (enabled).
	`ALTER TABLE cameras ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1`,
	// role scopes a token independently of its user: device-login sessions are
	// deliberately issued as ordinary users even when an admin approves them.
	// NULL keeps the historical behavior (the user's current role).
	`ALTER TABLE tokens ADD COLUMN role TEXT`,
	// refresh_prev keeps the immediately consumed refresh token for one sliding
	// window so a replayed/stolen token can be detected (and the session
	// revoked) instead of silently minting another pair.
	`ALTER TABLE tokens ADD COLUMN refresh_prev TEXT`,
	`ALTER TABLE tokens ADD COLUMN refresh_prev_used_at INTEGER`,
}

// Open opens the SQLite database at path (creating its directory), enabling WAL
// and a busy timeout so readers and a writer can run concurrently.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	// The DB holds session tokens, camera credentials and the media/relay
	// signing material. sqlite creates it with the process umask, which is
	// commonly 0644; tighten the file itself so a multi-user host does not
	// expose it. (WAL/SHM files inherit the process umask; the example unit
	// sets UMask=0077 as well.)
	_ = os.Chmod(path, 0o600)
	return db, nil
}

// Init applies the schema and seeds a default admin if the users table is
// empty.
func Init(db *sql.DB) error {
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return err
		}
	}
	// Created after the migrations: on a pre-existing DB the refresh_prev
	// column only exists once the ALTER above has run.
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_tokens_refresh_prev ON tokens(refresh_prev)"); err != nil {
		return err
	}
	return seedAdmin(db)
}

// isDuplicateColumn reports whether err is SQLite's "duplicate column name"
// error, raised when an idempotent ADD COLUMN migration runs against a DB that
// already has the column. The pure-Go driver surfaces it in the message.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func seedAdmin(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		slog.Info("users already exist, skipping admin seed", "count", count)
		return nil
	}
	username := os.Getenv("ENEVERRE_ADMIN_USER")
	if username == "" {
		username = "admin"
	}
	// The password is never read from a config file: it comes from
	// ENEVERRE_ADMIN_PASS (for automation) or, when unset, a freshly
	// generated random secret. The generated one is logged once here — the
	// only place it is ever shown in the clear — so the operator can log in
	// and change it. It is not persisted anywhere but the hashed column.
	password := os.Getenv("ENEVERRE_ADMIN_PASS")
	generated := password == ""
	if generated {
		var err error
		if password, err = randomPassword(18); err != nil {
			return err
		}
	}
	// The initial admin is flagged to change its password on first login: the
	// seed credential is either a random secret shown once in the log or a
	// bootstrap value from ENEVERRE_ADMIN_PASS, neither of which should remain
	// the account's permanent password.
	if _, err := db.Exec(
		"INSERT INTO users (username, password, role, must_change_password) VALUES (?, ?, ?, 1)",
		username, auth.GeneratePasswordHash(password), "admin",
	); err != nil {
		return err
	}
	if generated {
		slog.Warn("no users found: created admin with a generated password - log in and change it now",
			"user", username, "password", password)
	} else {
		slog.Info("no users found: created admin from ENEVERRE_ADMIN_PASS", "user", username)
	}
	return nil
}

// randomPassword returns a URL-safe base64 string carrying nBytes of
// cryptographic randomness (nBytes=18 -> 24 characters). Used to seed the
// first admin when no password is supplied, so a fresh install never ships
// with a known default credential.
func randomPassword(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
