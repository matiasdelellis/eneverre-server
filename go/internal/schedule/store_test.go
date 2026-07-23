package schedule

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE schedules (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		rules TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestStoreCreateNormalizesAndGet(t *testing.T) {
	st := NewStore(openTestDB(t))
	s, err := st.Create(Schedule{ID: "a", Name: "A", Days: map[string][]string{"mon": {"14:00-18:00", "08:00-12:00"}}}, 100)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := s.Days["mon"]; len(got) != 2 || got[0] != "08:00-12:00" || got[1] != "14:00-18:00" {
		t.Errorf("windows not normalized on create: %v", got)
	}
	got, ok, err := st.Get("a")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Name != "A" || got.CreatedAt != 100 {
		t.Errorf("get = %+v", got)
	}
	if got.Days["mon"][0] != "08:00-12:00" {
		t.Errorf("get windows not normalized: %v", got.Days["mon"])
	}
}

// TestStoreNullRulesGuard covers the scanRow guard: a rules value of the JSON
// literal `null` must read back as an empty (non-nil) Days map.
func TestStoreNullRulesGuard(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO schedules (id, name, rules, created_at) VALUES ('n', 'N', 'null', 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, ok, err := NewStore(db).Get("n")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Days == nil {
		t.Error("Days is nil; the scanRow guard should leave an empty map")
	}
}
