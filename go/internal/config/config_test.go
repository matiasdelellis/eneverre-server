package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestZaguanConfig(t *testing.T) {
	t.Setenv("ENEVERRE_ZAGUAN_URL", "")
	t.Setenv("ENEVERRE_ZAGUAN_TOKEN", "")
	t.Setenv("ENEVERRE_ZAGUAN_TOKEN_FILE", "")
	t.Setenv("ENEVERRE_ZAGUAN_PUBLIC_URL", "")

	cfg := loadINI(t, `
[zaguan]
url = 192.168.1.20:8081
name = Casa
public_url = https://cams.example.com/
token = zsrc_kid_secret
sync_interval = 10s
`)
	if got := cfg.ZaguanURL(); got != "192.168.1.20:8081" {
		t.Errorf("ZaguanURL = %q", got)
	}
	if got := cfg.ZaguanName(); got != "Casa" {
		t.Errorf("ZaguanName = %q", got)
	}
	if got := cfg.ZaguanPublicURL(); got != "https://cams.example.com/" {
		t.Errorf("ZaguanPublicURL = %q", got)
	}
	if got := cfg.ZaguanSyncInterval(); got != 10*time.Second {
		t.Errorf("ZaguanSyncInterval = %s", got)
	}
	if cfg.ZaguanLegacyMediaAuth() {
		t.Error("legacy media auth must be off by default")
	}
	token, err := cfg.ZaguanToken()
	if err != nil {
		t.Fatalf("ZaguanToken: %v", err)
	}
	if token != "zsrc_kid_secret" {
		t.Errorf("ZaguanToken = %q", token)
	}
}

func TestZaguanTokenFileWinsAndEnvFallbacks(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "zaguan.token")
	if err := os.WriteFile(tokenFile, []byte("  zsrc_from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENEVERRE_ZAGUAN_URL", "")
	t.Setenv("ENEVERRE_ZAGUAN_PUBLIC_URL", "")
	t.Setenv("ENEVERRE_ZAGUAN_TOKEN", "zsrc_from_env")
	t.Setenv("ENEVERRE_ZAGUAN_TOKEN_FILE", "")

	cfg := loadINI(t, "[zaguan]\ntoken = zsrc_inline\ntoken_file = "+tokenFile+"\n")
	token, err := cfg.ZaguanToken()
	if err != nil {
		t.Fatalf("ZaguanToken: %v", err)
	}
	if token != "zsrc_from_file" {
		t.Errorf("token_file did not win: %q", token)
	}

	// Without a token file the inline token wins over the environment.
	cfg = loadINI(t, "[zaguan]\ntoken = zsrc_inline\n")
	if token, _ := cfg.ZaguanToken(); token != "zsrc_inline" {
		t.Errorf("inline token = %q", token)
	}

	// Neither configured: the environment is the fallback.
	cfg = loadINI(t, "[zaguan]\n")
	if token, _ := cfg.ZaguanToken(); token != "zsrc_from_env" {
		t.Errorf("env token = %q", token)
	}

	// An unreadable token file is an error, never a silent fallback.
	cfg = loadINI(t, "[zaguan]\ntoken = zsrc_inline\ntoken_file = "+filepath.Join(dir, "missing")+"\n")
	if _, err := cfg.ZaguanToken(); err == nil {
		t.Error("missing token_file was accepted")
	}
}
