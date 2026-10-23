package servicetoken

import (
	"path/filepath"
	"strings"
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
	if err := store.Init(db); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	return NewStore(db)
}

func TestCreateVerifyListRevoke(t *testing.T) {
	s := newTestStore(t)
	raw, created, err := s.Create("zabbix", []string{ScopeStatus}, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(raw, Prefix) {
		t.Errorf("raw token %q does not carry the service prefix", raw)
	}
	if created.ID == 0 || created.Name != "zabbix" || !created.HasScope(ScopeStatus) {
		t.Errorf("created token = %+v", created)
	}

	got, err := s.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.ID != created.ID || !got.HasScope(ScopeStatus) {
		t.Errorf("verified token = %+v", got)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID || list[0].Scopes[0] != ScopeStatus {
		t.Errorf("List = %+v", list)
	}

	if err := s.Revoke(created.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.Verify(raw); err != ErrInvalid {
		t.Errorf("revoked token Verify error = %v, want ErrInvalid", err)
	}
	if err := s.Revoke(created.ID); err != ErrNotFound {
		t.Errorf("second Revoke error = %v, want ErrNotFound", err)
	}
}

func TestVerifyRejectsWrongPrefixUnknownAndExpired(t *testing.T) {
	s := newTestStore(t)
	raw, _, err := s.Create("expired", []string{ScopeStatus}, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify("envr_svc_deadbeef"); err != ErrInvalid {
		t.Errorf("unknown token = %v, want ErrInvalid", err)
	}
	if _, err := s.Verify("not-a-service-token"); err != ErrInvalid {
		t.Errorf("wrong prefix = %v, want ErrInvalid", err)
	}

	// Create with the smallest valid future expiry, then wait it out. The
	// store's check is second-granular, so a one-second expiry is enough.
	rawShort, _, err := s.Create("short", []string{ScopeStatus}, time.Now().Add(time.Second).Unix())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.Verify(rawShort); err != ErrInvalid {
		t.Errorf("expired token = %v, want ErrInvalid", err)
	}
	// The first token is still valid.
	if _, err := s.Verify(raw); err != nil {
		t.Errorf("long token unexpectedly invalid: %v", err)
	}
}

func TestCreateValidatesNameScopesExpiry(t *testing.T) {
	s := newTestStore(t)
	future := time.Now().Add(time.Hour).Unix()
	if _, _, err := s.Create("", []string{ScopeStatus}, future); err == nil {
		t.Error("empty name accepted")
	}
	if _, _, err := s.Create("x", nil, future); err == nil {
		t.Error("empty scopes accepted")
	}
	if _, _, err := s.Create("x", []string{"admin:write"}, future); err == nil {
		t.Error("unknown scope accepted")
	}
	if _, _, err := s.Create("x", []string{ScopeStatus}, time.Now().Add(-time.Minute).Unix()); err == nil {
		t.Error("past expiry accepted")
	}
}

func TestCreateNormalizesScopes(t *testing.T) {
	s := newTestStore(t)
	_, tok, err := s.Create("multi", []string{ScopeMetrics, " " + ScopeStatus + " ", ScopeMetrics}, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Scopes) != 2 || tok.Scopes[0] != ScopeMetrics || tok.Scopes[1] != ScopeStatus {
		t.Errorf("scopes = %v, want [%s %s]", tok.Scopes, ScopeMetrics, ScopeStatus)
	}
}

func TestParseID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"42", 42, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"x", 0, false},
		{"", 0, false},
	} {
		got, ok := ParseID(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseID(%q) = %d,%v; want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
