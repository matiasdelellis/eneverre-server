package mediamtx

import (
	"database/sql"
	"path/filepath"
	"testing"

	"eneverre/internal/store"
)

// testDB opens a temporary SQLite database with the full schema (including the
// mediamtx_credentials table) so the Store can read/write its row.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Init(db); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func dbCreds(t *testing.T, db *sql.DB) Creds {
	t.Helper()
	var c Creds
	if err := db.QueryRow("SELECT username, password FROM mediamtx_credentials WHERE id = 1").
		Scan(&c.Username, &c.Password); err != nil {
		t.Fatalf("read persisted creds: %v", err)
	}
	return c
}

func TestStorePersistsAndValidates(t *testing.T) {
	db := testDB(t)
	s, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cur := s.Current()
	if cur.Username == "" || cur.Password == "" {
		t.Fatal("generated empty credentials")
	}
	if got := dbCreds(t, db); got != cur {
		t.Fatalf("persisted creds %+v != current %+v", got, cur)
	}
	if !s.Validate(cur.Username, cur.Password) {
		t.Fatal("current credentials rejected")
	}
	if s.Validate(cur.Username, "wrong") {
		t.Fatal("wrong password accepted")
	}
	if s.Validate("", "") {
		t.Fatal("empty credentials accepted")
	}
}

func TestStoreReloadsExisting(t *testing.T) {
	db := testDB(t)
	s1, _ := NewStore(db)
	want := s1.Current()
	s2, err := NewStore(db)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s2.Current() != want {
		t.Fatal("reloaded credentials differ from persisted ones")
	}
}

func TestRotateGraceWindow(t *testing.T) {
	db := testDB(t)
	s, _ := NewStore(db)
	gen0 := s.Current()

	gen1, err := s.Rotate()
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if gen1 == gen0 {
		t.Fatal("rotation produced identical credentials")
	}
	if s.Current() != gen1 {
		t.Fatal("Current did not advance after rotation")
	}
	// New credentials valid, and the immediately previous pair still valid.
	if !s.Validate(gen1.Username, gen1.Password) {
		t.Fatal("new credentials rejected after rotation")
	}
	if !s.Validate(gen0.Username, gen0.Password) {
		t.Fatal("grace-window (previous) credentials rejected")
	}

	// After a second rotation, gen0 is two generations old and must be invalid.
	gen2, _ := s.Rotate()
	if s.Validate(gen0.Username, gen0.Password) {
		t.Fatal("credentials two generations old still accepted")
	}
	if !s.Validate(gen1.Username, gen1.Password) {
		t.Fatal("previous generation should still be in the grace window")
	}
	if !s.Validate(gen2.Username, gen2.Password) {
		t.Fatal("current generation rejected")
	}

	// The persisted row must hold the current credentials.
	if got := dbCreds(t, db); got != gen2 {
		t.Fatalf("persisted creds %+v != current %+v", got, gen2)
	}
}
