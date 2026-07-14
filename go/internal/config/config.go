// Package config loads and caches the eneverre INI configuration, mirroring
// the lookup behavior of the original app/config.py.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

// Search paths, in priority order. The first existing file wins. These match
// the tuples in app/config.py.
var (
	configPaths = []string{"/etc/eneverre/eneverre.ini", "./data/eneverre.ini"}
	camerasDirs = []string{"/etc/eneverre/cameras.d", "./data/cameras.d"}
	dbPaths     = []string{"/var/run/eneverre/eneverre.db", "./data/eneverre.db"}
)

// Section is a flat key/value view of one INI section. Keys are lowercased on
// load, matching configparser's optionxform (so `home_Y` becomes `home_y`).
// A nil Section is valid and always returns defaults.
type Section map[string]string

// Get returns the value for key, or def if missing or empty.
func (s Section) Get(key, def string) string {
	if s == nil {
		return def
	}
	if v, ok := s[strings.ToLower(key)]; ok && v != "" {
		return v
	}
	return def
}

// GetInt parses key as an int, falling back to def on missing/invalid.
func (s Section) GetInt(key string, def int) int {
	v := s.Get(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// GetBool mirrors configparser.getboolean: 1/yes/true/on and 0/no/false/off
// (case-insensitive). Anything else falls back to def.
func (s Section) GetBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s.Get(key, ""))) {
	case "":
		return def
	case "1", "yes", "true", "on":
		return true
	case "0", "no", "false", "off":
		return false
	default:
		return def
	}
}

// Config holds the resolved file locations and parsed sections.
type Config struct {
	ConfigFile string
	// FileLoaded reports whether ConfigFile actually existed and was read. When
	// false the config file was absent and every setting falls back to its
	// built-in default (the file is optional, like the DB and cameras dir).
	FileLoaded bool
	CamerasDir string
	DBFile     string

	Server  Section
	Media   Section // nil if there is no [media] section (embedded NVR engine)
	Events  Section // nil if there is no [events] section
	Auth    Section // nil if there is no [auth] section
	Updates Section // nil if there is no [updates] section
}

// LoadOptions are the path overrides a caller can pass to Load. Precedence:
// explicit option > matching ENEVERRE_* env var > built-in default search
// paths. Empty fields fall through to the next layer.
type LoadOptions struct {
	ConfigFile string
	CamerasDir string
	DBPath     string
}

// Load reads the config file and resolves the cameras dir and DB path. The
// optional opts struct lets a CLI flag beat the ENEVERRE_* env vars; with
// opts zero-valued the behavior is the same as before (env vars, then
// built-in search paths).
func Load(opts LoadOptions) (*Config, error) {
	// The config file is optional only when its path is left to the built-in
	// search: a missing default file is not fatal — every setting falls back to
	// its default. But an explicit path (--config flag or ENEVERRE_CONFIG_PATH)
	// that does not exist is a mistake, not a "use defaults" signal, so it fails
	// loudly (see the load step below). When no path is given, default to the
	// last candidate (./data/eneverre.ini), picking an existing one if present.
	cfgFile := opts.ConfigFile
	if cfgFile == "" {
		cfgFile = os.Getenv("ENEVERRE_CONFIG_PATH")
	}
	explicit := cfgFile != ""
	if cfgFile == "" {
		cfgFile = configPaths[len(configPaths)-1]
		for _, p := range configPaths {
			if _, err := os.Stat(p); err == nil {
				cfgFile = p
				break
			}
		}
	}

	// The cameras dir need not exist: cameras live in the DB now, and this
	// directory is only the source of the one-time INI seed (see
	// camera.SeedFromINI). A missing cameras.d simply means "no seed", not a
	// fatal error — so, like the DB path, default to the last candidate and let
	// the seed find no files when it isn't there.
	camDir := opts.CamerasDir
	if camDir == "" {
		camDir = os.Getenv("ENEVERRE_CAMERAS_DIR")
	}
	if camDir == "" {
		camDir = camerasDirs[len(camerasDirs)-1]
		for _, p := range camerasDirs {
			if _, err := os.Stat(p); err == nil {
				camDir = p
				break
			}
		}
	}

	// The DB file need not exist yet; default to the last candidate so the
	// store package can create it.
	dbFile := opts.DBPath
	if dbFile == "" {
		dbFile = os.Getenv("ENEVERRE_DB_PATH")
	}
	if dbFile == "" {
		dbFile = dbPaths[len(dbPaths)-1]
		for _, p := range dbPaths {
			if _, err := os.Stat(p); err == nil {
				dbFile = p
				break
			}
		}
	}

	// Insensitive lowercases section and key names, matching configparser. When
	// a default-searched file is absent we start from an empty document so every
	// key resolves to its default. A file that exists but fails to parse is
	// always fatal (a genuine typo is not silently ignored), and an explicitly
	// requested file that is missing is fatal too (the operator pointed at it
	// on purpose).
	loadOpts := ini.LoadOptions{Insensitive: true}
	fileLoaded := false
	f := ini.Empty(loadOpts)
	if _, err := os.Stat(cfgFile); err == nil {
		if f, err = ini.LoadSources(loadOpts, cfgFile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", cfgFile, err)
		}
		fileLoaded = true
	} else if explicit {
		return nil, fmt.Errorf("config file not found: %s: %w", cfgFile, err)
	}

	c := &Config{ConfigFile: cfgFile, FileLoaded: fileLoaded, CamerasDir: camDir, DBFile: dbFile}
	c.Server = sectionMap(f, "server")
	if f.HasSection("media") {
		c.Media = sectionMap(f, "media")
	}
	if f.HasSection("events") {
		c.Events = sectionMap(f, "events")
	}
	if f.HasSection("auth") {
		c.Auth = sectionMap(f, "auth")
	}
	if f.HasSection("updates") {
		c.Updates = sectionMap(f, "updates")
	}
	return c, nil
}

// UpdatesStorageDir resolves the directory where published APKs and
// manifests live. Precedence: [updates] storage_dir INI key >
// ENEVERRE_UPDATES_DIR env var. The feature is intentionally DISABLED unless
// one of those two is set explicitly: an empty result makes the stores report
// Enabled() == false and the /api/app/* endpoints answer 503 instead of 404.
// (The historical fallback that derived <cameras_dir_parent>/app-updates was
// removed because CamerasDir is always resolved at startup, which silently
// enabled the feature — and sprang up a writable directory — on every install
// even without any [updates] configuration.)
func (c *Config) UpdatesStorageDir() string {
	if v := strings.TrimSpace(c.UpdatesSection().Get("storage_dir", "")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("ENEVERRE_UPDATES_DIR")); v != "" {
		return v
	}
	return ""
}

// UpdatesPublicBaseURL resolves the base URL the manifest's `url` field is
// rooted at. Precedence: [updates] public_base_url > ENEVERRE_UPDATES_PUBLIC_BASE_URL
// > "" (the HTTP layer builds the URL from the request's Host when empty).
func (c *Config) UpdatesPublicBaseURL() string {
	if v := strings.TrimSpace(c.UpdatesSection().Get("public_base_url", "")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("ENEVERRE_UPDATES_PUBLIC_BASE_URL"))
}

// UpdatesPublishToken resolves the bearer token accepted by the publish
// endpoints. Precedence: [updates] publish_token >
// ENEVERRE_UPDATES_PUBLISH_TOKEN > "". When empty, the publish endpoints
// fall back to admin-user auth (Basic or Bearer issued by POST
// /api/auth/login) for backward compatibility. When set, only the token is
// accepted — user/password auth is rejected for these endpoints, so the
// token can be rotated without touching user accounts and revoked without
// affecting the rest of the API.
func (c *Config) UpdatesPublishToken() string {
	if v := strings.TrimSpace(c.UpdatesSection().Get("publish_token", "")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("ENEVERRE_UPDATES_PUBLISH_TOKEN"))
}

// UpdatesMaxAPKSize returns the maximum APK size the publish endpoint
// will accept, in bytes. The cap is enforced via http.MaxBytesReader, so
// a 413 is returned as soon as the body crosses the limit (no buffering
// of the over-limit bytes). Precedence: [updates] max_apk_size INI key
// (accepts a decimal byte count or a K/M/G suffix, base 1024) >
// ENEVERRE_UPDATES_MAX_APK_SIZE env var > 100 MiB default. The default
// is sized for current TV builds (~50-70 MiB universal); if a future
// build exceeds 100 MiB, raise the cap or split the publish into
// per-ABI POSTs.
func (c *Config) UpdatesMaxAPKSize() int64 {
	if v := strings.TrimSpace(c.UpdatesSection().Get("max_apk_size", "")); v != "" {
		if n, err := parseSize(v); err == nil && n > 0 {
			return n
		}
	}
	if env := strings.TrimSpace(os.Getenv("ENEVERRE_UPDATES_MAX_APK_SIZE")); env != "" {
		if n, err := parseSize(env); err == nil && n > 0 {
			return n
		}
	}
	return 100 * 1024 * 1024
}

// ServerReadTimeout resolves the http.Server.ReadTimeout used for the
// listen socket. The body read is what trips the default (15s) for big
// publishes; the publish endpoints legitimately need a generous window
// (a 200 MB upload over a 5 Mbps link takes ~5 minutes). Precedence:
// [server] read_timeout > ENEVERRE_READ_TIMEOUT > 5m default. The format
// is what time.ParseDuration accepts: "5m", "30s", "1h", etc.
func (c *Config) ServerReadTimeout() time.Duration {
	if v := strings.TrimSpace(c.Server.Get("read_timeout", "")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if env := strings.TrimSpace(os.Getenv("ENEVERRE_READ_TIMEOUT")); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}

// parseSize accepts a decimal byte count with an optional K/M/G suffix
// (case-insensitive, base 1024). Empty or unparseable input returns 0 +
// error so the caller can fall through to the next precedence layer.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, s = 1024, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1024*1024, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1024*1024*1024, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// AuthCleanupIntervalMinutes returns how often the background token-cleanup
// goroutine runs, in minutes. 0 or negative means the background ticker is not
// started (cleanup still runs opportunistically on login). Default: 60 (1h).
// Precedence: [auth] cleanup_interval_minutes > ENEVERRE_TOKEN_CLEANUP_INTERVAL
// > 60.
func (c *Config) AuthCleanupIntervalMinutes() int {
	const def = 60
	if v := strings.TrimSpace(c.Auth.Get("cleanup_interval_minutes", "")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if env := strings.TrimSpace(os.Getenv("ENEVERRE_TOKEN_CLEANUP_INTERVAL")); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// AuthCleanupGraceHours returns how many hours a token remains in the database
// after it expires, so the frontend can still show it in the sessions list as
// "expired". Default: 24 (1 day). Precedence: [auth] cleanup_grace_hours >
// ENEVERRE_TOKEN_CLEANUP_GRACE_HOURS > 24.
func (c *Config) AuthCleanupGraceHours() int {
	const def = 24
	if v := strings.TrimSpace(c.Auth.Get("cleanup_grace_hours", "")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	if env := strings.TrimSpace(os.Getenv("ENEVERRE_TOKEN_CLEANUP_GRACE_HOURS")); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// AuthSecurityLog returns the path to the security event log — the file that
// records authentication failures in a stable, one-line-per-event format for
// intrusion-prevention tools (fail2ban, CrowdSec) to tail. Empty means no
// dedicated file is written (events still go to the main log at WARN).
// Precedence: [auth] security_log > ENEVERRE_SECURITY_LOG > "" (disabled).
func (c *Config) AuthSecurityLog() string {
	if v := strings.TrimSpace(c.Auth.Get("security_log", "")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("ENEVERRE_SECURITY_LOG"))
}

// UpdatesSection returns the [updates] section or an empty Section. Safe to
// call when the section is missing (returns a non-nil Section that yields
// defaults).
func (c *Config) UpdatesSection() Section {
	if c.Updates == nil {
		return Section{}
	}
	return c.Updates
}

// HumanSize formats a byte count using the largest K/M/G unit (base 1024)
// that divides it evenly, so the result is something the operator would
// type in the INI file. For values that don't divide cleanly (rare) it
// falls back to the raw byte count. The unit suffix matches parseSize:
// "K" = KiB, "M" = MiB, "G" = GiB.
func HumanSize(n int64) string {
	const (
		K = int64(1) << 10
		M = int64(1) << 20
		G = int64(1) << 30
	)
	switch {
	case n > 0 && n%G == 0:
		return fmt.Sprintf("%dG", n/G)
	case n > 0 && n%M == 0:
		return fmt.Sprintf("%dM", n/M)
	case n > 0 && n%K == 0:
		return fmt.Sprintf("%dK", n/K)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func sectionMap(f *ini.File, name string) Section {
	s := Section{}
	for _, k := range f.Section(name).Keys() {
		s[strings.ToLower(k.Name())] = k.Value()
	}
	return s
}
