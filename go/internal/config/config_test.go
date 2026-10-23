package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadINI(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "eneverre.ini")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{ConfigFile: p, DataDir: dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// 0 is the documented way to disable the background token cleanup; it used
// to be rejected as invalid and silently replaced by the 60-minute default.
func TestAuthCleanupIntervalZeroDisables(t *testing.T) {
	t.Setenv("ENEVERRE_TOKEN_CLEANUP_INTERVAL", "")
	if got := loadINI(t, "[auth]\ncleanup_interval_minutes = 0\n").AuthCleanupIntervalMinutes(); got != 0 {
		t.Errorf("cleanup_interval_minutes = 0 -> %d, want 0 (disabled)", got)
	}
	if got := loadINI(t, "[auth]\ncleanup_interval_minutes = 15\n").AuthCleanupIntervalMinutes(); got != 15 {
		t.Errorf("cleanup_interval_minutes = 15 -> %d", got)
	}
	if got := loadINI(t, "[auth]\ncleanup_interval_minutes = soon\n").AuthCleanupIntervalMinutes(); got != 60 {
		t.Errorf("invalid value -> %d, want the 60 default", got)
	}
}
