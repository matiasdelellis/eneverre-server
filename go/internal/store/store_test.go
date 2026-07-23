package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// columnExists reports whether table has a column named col (via PRAGMA).
func columnExists(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == col {
			return true
		}
	}
	return false
}

// tableExists reports whether a table with the given name is present.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return got == name
}

// TestMigrationAddsScheduleID proves the idempotent ALTER backfills schedule_id
// onto a cameras table that pre-dates the column (an upgraded install), and that
// running Init again is a harmless no-op (the "duplicate column" path).
func TestMigrationAddsScheduleID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Simulate a pre-migration install: a cameras table WITHOUT schedule_id.
	if _, err := db.Exec(`CREATE TABLE cameras (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		record INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatalf("seed old table: %v", err)
	}

	if err := Init(db); err != nil {
		t.Fatalf("Init (first): %v", err)
	}
	if !columnExists(t, db, "cameras", "schedule_id") {
		t.Fatal("schedule_id not added by migration")
	}
	if !tableExists(t, db, "schedules") {
		t.Fatal("schedules table not created")
	}
	// Running Init again must not error (ALTER hits duplicate-column, swallowed).
	if err := Init(db); err != nil {
		t.Fatalf("Init (second, idempotent): %v", err)
	}
}

// TestFreshInstallHasScheduleID confirms a brand-new DB gets schedule_id from
// the CREATE TABLE (and the redundant ALTER is swallowed without error).
func TestFreshInstallHasScheduleID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Init(db); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !columnExists(t, db, "cameras", "schedule_id") {
		t.Fatal("fresh install missing schedule_id")
	}
}
