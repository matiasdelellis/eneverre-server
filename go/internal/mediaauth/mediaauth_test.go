package mediaauth

import (
	"path/filepath"
	"testing"
	"time"

	"eneverre/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestSignAndValidate(t *testing.T) {
	s := newTestStore(t)
	exp, sig := s.Sign("front", ScopeLive, time.Hour)

	if !s.Validate("front", ScopeLive, exp, sig) {
		t.Fatal("fresh grant was rejected")
	}
	if s.Validate("garden", ScopeLive, exp, sig) {
		t.Error("grant validated for a different camera")
	}
	if s.Validate("front", ScopeHLS, exp, sig) {
		t.Error("grant validated for a different scope")
	}
	if s.Validate("front", ScopeLive, exp+1, sig) {
		t.Error("grant validated with a tampered expiry")
	}
	if s.Validate("front", ScopeLive, exp, sig[:len(sig)-1]+"0") {
		t.Error("grant validated with a tampered signature")
	}
	if s.Validate("front", ScopeLive, time.Now().Add(-time.Second).Unix(), s.SignAt("front", ScopeLive, time.Now().Add(-time.Second).Unix())) {
		t.Error("expired grant was accepted")
	}
	if s.Validate("front", ScopeLive, time.Now().Add(maxTokenLifetime+time.Hour).Unix(), "00") {
		t.Error("malformed signature was accepted")
	}
}

func TestStorePersistsKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	first, err := NewStore(db)
	if err != nil {
		t.Fatalf("first NewStore: %v", err)
	}
	exp, sig := first.Sign("cam", ScopeThumb, time.Hour)

	second, err := NewStore(db)
	if err != nil {
		t.Fatalf("second NewStore: %v", err)
	}
	if !second.Validate("cam", ScopeThumb, exp, sig) {
		t.Error("a grant did not survive reopening the store")
	}
}

func TestTTL(t *testing.T) {
	if got := TTL(ScopeHLS); got != 6*time.Hour {
		t.Errorf("HLS TTL = %s", got)
	}
	for _, scope := range []string{ScopeLive, ScopeThumb, "unknown"} {
		if got := TTL(scope); got != time.Hour {
			t.Errorf("TTL(%q) = %s", scope, got)
		}
	}
}
