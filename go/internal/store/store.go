// Package store opens the SQLite database and bootstraps its schema, porting
// app/db.py and app/db_init.py. It uses the pure-Go modernc.org/sqlite driver
// (no CGO).
package store

import (
	"database/sql"
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
		role TEXT NOT NULL
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
	// Single-row table holding the current MediaMTX credential pair. The CHECK
	// pins it to one row; the mediamtx.Store keeps the live pair in memory and
	// only reads this at startup / writes it on rotation.
	`CREATE TABLE IF NOT EXISTS mediamtx_credentials (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		username TEXT NOT NULL,
		password TEXT NOT NULL,
		rotated_at INTEGER NOT NULL
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
	return seedAdmin(db)
}

// migrateColumns adds columns that older databases may lack. New installs get
// them from the schema directly; this only fills gaps.
func migrateColumns(db *sql.DB) error {
	migrations := []struct{ table, column, typ string }{
		{"users", "first_name", "TEXT"},
		{"users", "last_name", "TEXT"},
		{"tokens", "device_name", "TEXT"},
		{"device_login", "device_name", "TEXT"},
		// Refresh-token columns: a renewable session (password login) carries a
		// refresh secret + its own expiry; device-flow tokens leave them NULL.
		{"tokens", "refresh_token", "TEXT"},
		{"tokens", "refresh_expires_at", "INTEGER"},
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
	password := os.Getenv("ENEVERRE_ADMIN_PASS")
	if password == "" {
		password = "eneverre"
	}
	slog.Warn("no users found, creating default admin; set ENEVERRE_ADMIN_PASS before exposing this service",
		"user", username)
	_, err := db.Exec(
		"INSERT INTO users (username, password, role) VALUES (?, ?, ?)",
		username, auth.GeneratePasswordHash(password), "admin",
	)
	return err
}
