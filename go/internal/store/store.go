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
		refresh_expires_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tokens_username ON tokens(username)`,
	// handleRefresh looks sessions up by refresh_token on every renewal; without
	// this index that is a full-table scan.
	`CREATE INDEX IF NOT EXISTS idx_tokens_refresh ON tokens(refresh_token)`,
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
	// Single-row table holding the current stream-auth credential pair. The
	// CHECK pins it to one row; the streamauth.Store keeps the live pair in
	// memory and only reads this at startup / writes it on rotation.
	`CREATE TABLE IF NOT EXISTS streamauth_credentials (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		username TEXT NOT NULL,
		password TEXT NOT NULL,
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
		playback INTEGER NOT NULL DEFAULT 0,
		width INTEGER NOT NULL DEFAULT 16,
		height INTEGER NOT NULL DEFAULT 9,
		thingino_url TEXT NOT NULL DEFAULT '',
		thingino_api_key TEXT NOT NULL DEFAULT '',
		ptz INTEGER NOT NULL DEFAULT 0,
		home_x REAL NOT NULL DEFAULT -1,
		home_y REAL NOT NULL DEFAULT -1,
		privacy_x REAL NOT NULL DEFAULT -1,
		privacy_y REAL NOT NULL DEFAULT -1,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0
	)`,
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
	return db, nil
}

// Init applies the schema, idempotent column migrations, and seeds a default
// admin if the users table is empty.
func Init(db *sql.DB) error {
	if err := migrateColumns(db); err != nil {
		return err
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := migrateStreamAuthTable(db); err != nil {
		return err
	}
	return seedAdmin(db)
}

// migrateStreamAuthTable copies any pre-existing `mediamtx_credentials` row
// (left over from installations that predate the MediaMTX → embedded-engine
// migration) into the new `streamauth_credentials` table and drops the old
// one. New installs see neither table and the migration is a no-op.
func migrateStreamAuthTable(db *sql.DB) error {
	old, err := tableExists(db, "mediamtx_credentials")
	if err != nil || !old {
		return err
	}
	has, err := tableExists(db, "streamauth_credentials")
	if err != nil {
		return err
	}
	if !has {
		// New table hasn't been created yet (the schema loop ran before us only
		// because Go maps are unordered; in practice the schema runs first, but
		// guard against the rare case where it didn't). Move atomically: rename
		// is a metadata-only operation in SQLite.
		if _, err := db.Exec("ALTER TABLE mediamtx_credentials RENAME TO streamauth_credentials"); err != nil {
			return err
		}
		slog.Info("migrated mediamtx_credentials -> streamauth_credentials (rename)")
		return nil
	}
	// Both tables exist (someone ran a pre-migration version after the rename
	// already landed in the schema). Carry the row over and drop the old one.
	if _, err := db.Exec(
		`INSERT INTO streamauth_credentials (id, username, password, rotated_at)
		 SELECT id, username, password, rotated_at FROM mediamtx_credentials
		 WHERE id = 1
		 ON CONFLICT(id) DO UPDATE SET
		     username = excluded.username,
		     password = excluded.password,
		     rotated_at = excluded.rotated_at`,
	); err != nil {
		return err
	}
	if _, err := db.Exec("DROP TABLE mediamtx_credentials"); err != nil {
		return err
	}
	slog.Info("migrated mediamtx_credentials -> streamauth_credentials (copy+drop)")
	return nil
}

// migrateColumns adds columns that older databases may lack. New installs get
// them from the schema directly; this only fills gaps.
func migrateColumns(db *sql.DB) error {
	migrations := []struct{ table, column, typ string }{
		{"users", "first_name", "TEXT"},
		{"users", "last_name", "TEXT"},
		// Forces the user to set a new password on next login (the UI gates on
		// it; cleared by the self change-password endpoint). Defaults to 0 so
		// existing accounts are unaffected.
		{"users", "must_change_password", "INTEGER NOT NULL DEFAULT 0"},
		{"tokens", "device_name", "TEXT"},
		{"device_login", "device_name", "TEXT"},
		// Refresh-token columns: a renewable session (password login) carries a
		// refresh secret + its own expiry; device-flow tokens leave them NULL.
		{"tokens", "refresh_token", "TEXT"},
		{"tokens", "refresh_expires_at", "INTEGER"},
		// Generic per-camera still-JPEG URL, proxied by the thumbnail route so
		// non-Thingino cameras get a snapshot. Older DBs predate the column.
		{"cameras", "snapshot_url", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, m := range migrations {
		exists, err := tableExists(db, m.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		has, err := columnExists(db, m.table, m.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		slog.Info("migrating table: adding column", "table", m.table, "column", m.column)
		if _, err := db.Exec("ALTER TABLE " + m.table + " ADD COLUMN " + m.column + " " + m.typ); err != nil {
			return err
		}
	}
	return nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var one int
	err := db.QueryRow(
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, typ   string
			notnull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
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
