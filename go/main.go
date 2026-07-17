// Command eneverre is the Eneverre NVR API. It is a manufacturer-agnostic
// gateway: it serves a uniform camera list, mediates the device-login flow,
// proxies PTZ/thumbnail/playback to upstreams, and serves the static web UI.
// See AGENTS.md for the architecture overview.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/config"
	"eneverre/internal/media"
	"eneverre/internal/metrics"
	"eneverre/internal/server"
	"eneverre/internal/store"
	"eneverre/internal/streamauth"
	"eneverre/internal/updates"
)

// version is set at build time via -ldflags "-X main.version=...". The
// Makefile injects the value of $(VERSION) (git describe, or the
// fallback 0.1.0-dev).
var version = "0.1.0-dev"

// logWriter is where the logger sends its output. It defaults to stderr and is
// overridden once at startup by resolveLogWriter — on Windows, a service
// launched by the Service Control Manager has no console, so logs (including
// the one-time first-run admin password) are redirected to a file there.
var logWriter io.Writer = os.Stderr

// setupLogging installs a leveled slog text handler as the default logger.
// Recognized levels: debug, info (default), warn, error.
func setupLogging(level string) {
	lvl := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: lvl})))
}

// fatal logs an error and exits non-zero.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// resolveLogLevel picks the log level with the precedence CLI flag > env
// var > config file. Empty args are skipped.
func resolveLogLevel(cliFlag string) string {
	if l := strings.TrimSpace(cliFlag); l != "" {
		return l
	}
	if l := os.Getenv("ENEVERRE_LOG_LEVEL"); l != "" {
		return l
	}
	return ""
}

// resolveIntOption picks a positive int with precedence CLI flag (when > 0) >
// env var (when set to a positive int) > the already-resolved config/default
// value passed as cfgOrDefault. Used for the token-lifetime options.
func resolveIntOption(cliVal int, envKey string, cfgOrDefault int) int {
	if cliVal > 0 {
		return cliVal
	}
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return cfgOrDefault
}

func main() {
	opts, printHelp := parseFlags()
	if opts.showVersion {
		fmt.Println("eneverre-api", version)
		return
	}
	if opts.showHelp {
		printHelp()
		return
	}

	// Decide where logs go before the first line is written. On Windows under
	// the Service Control Manager this redirects to a file (no console);
	// everywhere else it stays on stderr.
	logWriter = resolveLogWriter()

	// Initial level from CLI > env, so even config-load errors are
	// leveled correctly. The config-file value (if any) is applied once
	// the config is loaded.
	if lvl := resolveLogLevel(opts.logLevel); lvl != "" {
		setupLogging(lvl)
	} else {
		setupLogging(os.Getenv("ENEVERRE_LOG_LEVEL"))
	}

	cfg, err := config.Load(config.LoadOptions{
		ConfigFile: opts.configFile,
		CamerasDir: opts.camerasDir,
		DBPath:     opts.dbPath,
		DataDir:    opts.dataDir,
	})
	if err != nil {
		fatal("config load failed", "err", err)
	}
	// Let [server] log_level set the level when the CLI flag and env var
	// didn't.
	if resolveLogLevel(opts.logLevel) == "" && os.Getenv("ENEVERRE_LOG_LEVEL") == "" {
		if lvl := cfg.Server.Get("log_level", ""); lvl != "" {
			setupLogging(lvl)
		}
	}
	if cfg.FileLoaded {
		slog.Info("config loaded", "file", cfg.ConfigFile)
	} else {
		slog.Info("no config file found, using defaults", "searched", cfg.ConfigFile)
	}

	db, err := store.Open(cfg.DBFile)
	if err != nil {
		fatal("open database failed", "path", cfg.DBFile, "err", err)
	}
	if err := store.Init(db); err != nil {
		fatal("init database failed", "err", err)
	}

	// Stream-auth credentials live in the DB (generated on a fresh table).
	// The live pair is cached in memory — the per-request path never hits the
	// DB. The same pair guards the embedded RTSP relay and is embedded in the
	// relay URL returned by /api/cameras.
	creds, err := streamauth.NewStore(db)
	if err != nil {
		fatal("stream-auth credentials failed", "err", err)
	}

	// Cameras are DB-backed. On a fresh install (or an upgrade from the old
	// file-based layout) import the per-camera INI files once as the initial
	// seed; after that the DB is authoritative and the INI files are ignored.
	if _, err := camera.SeedFromINI(db, cfg, time.Now().Unix()); err != nil {
		fatal("camera seed failed", "err", err)
	}
	camStore := camera.NewStore(db)
	cams, err := camStore.List()
	if err != nil {
		fatal("load cameras failed", "err", err)
	}

	// Strip the "static/" prefix so the embedded files are served from root.
	uiFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		fatal("embedded UI failed", "err", err)
	}

	// Token lifetimes, precedence CLI flag > ENEVERRE_* env > [auth] section >
	// built-in default. The [auth].GetInt term already folds in the default.
	accessHours := resolveIntOption(opts.accessTTLHours, "ENEVERRE_ACCESS_TOKEN_TTL_HOURS",
		cfg.Auth.GetInt("access_token_ttl_hours", server.DefaultAccessTTLHours))
	refreshDays := resolveIntOption(opts.refreshTTLDays, "ENEVERRE_REFRESH_TOKEN_TTL_DAYS",
		cfg.Auth.GetInt("refresh_token_ttl_days", server.DefaultRefreshTTLDays))

	// Auto-update stores: one per client track, sharing the configured
	// storage root. When the [updates] section is absent and no env var
	// overrides, UpdatesStorageDir returns "" and the stores are disabled
	// (their Enabled() reports false), so the /api/app/* endpoints answer
	// 503 instead of 404. This lets operators opt in without code changes.
	updatesRoot := cfg.UpdatesStorageDir()
	updateStores := map[string]*updates.Store{}
	for _, track := range []string{"tv", "phone"} {
		s := updates.NewStore(updatesRoot, track)
		if err := s.Ensure(); err != nil {
			fatal("updates directory init failed", "track", track, "dir", s.Dir(), "err", err)
		}
		updateStores[track] = s
	}
	if updatesRoot != "" {
		slog.Info("auto-update enabled", "storage_dir", updatesRoot, "public_base_url", cfg.UpdatesPublicBaseURL())
	} else {
		slog.Info("auto-update disabled (no [updates] storage_dir and no ENEVERRE_UPDATES_DIR)")
	}

	// Embedded media engine — always built and started. Live MSE + RTSP relay
	// are on by default for any camera with a `source` URL, because that's
	// the point of the app. The optional [media] section adds recording
	// (off by default; enable explicitly with `[media] record = true`) and
	// tunes the rest of the engine (paths, segment timing, retention, …).
	// Per-camera `record = false` / `live = false` INI keys opt a single
	// camera out of recording or out of the live pipeline respectively.
	var mopts media.Options
	if cfg.Media != nil {
		mopts = media.OptionsFromSection(cfg.Media)
	} else {
		mopts = media.DefaultOptions()
	}
	mopts.RelayCredsFn = creds.Pairs // rotation-aware relay auth (current + grace)
	var engine *media.Engine
	engine, err = media.New(mopts)
	if err != nil {
		fatal("media engine init failed", "err", err)
	}
	if opts.reindex {
		slog.Info("reindex requested: rebuilding recording index from disk before start")
		engine.ReindexAll(cams)
	}
	engine.Start(cams)

	app := server.New(cfg, db, creds, camStore, cams, uiFS, opts.staticCacheControl,
		int64(accessHours)*3600, int64(refreshDays)*86400, updateStores)
	app.SetMediaEngine(engine)
	app.SetVersion(version)

	// Metrics (Prometheus + JSON). On by default; set [server] metrics = false
	// to drop the endpoints entirely (no store wired, so the routes 404).
	// Closures bridge the App's mutable state into the collectors without
	// exposing App internals.
	if cfg.Server.GetBool("metrics", true) {
		m := metrics.New(db, version,
			func() []media.CameraStatus { return engine.Status() },
			func(id string) bool { return app.PrivacyState(id) },
			app.Cameras)
		app.SetMetrics(m)
		slog.Info("metrics enabled", "prometheus", "/api/metrics", "json", "/api/metrics/json")
	} else {
		slog.Info("metrics disabled", "reason", "[server] metrics = false")
	}

	// Auto-rotate the stream/relay credentials. The previous pair stays valid
	// for one interval (grace window) so active streams are not dropped at the
	// moment of rotation. The interval comes from [media].rotate_hours when
	// [media] is configured; otherwise we use the 24h default — the relay
	// runs even without [media], so a fresh pair is just as useful.
	rotateHours := 24
	if cfg.Media != nil {
		rotateHours = cfg.Media.GetInt("rotate_hours", 24)
	}
	if rotateHours > 0 {
		creds.StartRotation(time.Duration(rotateHours) * time.Hour)
		slog.Info("credential rotation enabled", "every_hours", rotateHours)
	} else {
		slog.Info("credential rotation disabled (rotate_hours <= 0)")
	}

	// Resolve host/port with precedence CLI flag > [server] section > default.
	host := opts.host
	if host == "" {
		host = cfg.Server.Get("host", "0.0.0.0")
	}
	port := opts.port
	if port == "" {
		port = cfg.Server.Get("port", "8080")
	}
	addr := host + ":" + port

	slog.Info("eneverre-api ready",
		"version", version,
		"cameras", len(cams),
		"addr", addr,
		"static_cache_control", opts.staticCacheControl,
		"access_ttl_h", accessHours,
		"refresh_ttl_d", refreshDays,
		"read_timeout", cfg.ServerReadTimeout(),
		"max_apk_size", config.HumanSize(cfg.UpdatesMaxAPKSize()))
	// Explicit timeouts instead of http.ListenAndServe's zero-value (unlimited)
	// defaults: a slow or idle client must not hold a connection/goroutine
	// open indefinitely (slowloris). WriteTimeout is generous because the
	// thumbnail and playback handlers proxy upstream responses. ReadTimeout
	// is the only knob that must be loose enough to accept a multi-hundred-MB
	// APK upload over a slow link; it is configurable via [server] read_timeout.
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ServerReadTimeout(),
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// cleanup finalizes and indexes each camera's in-progress fMP4 segment (via
	// engine.Close -> recorder.Close) and closes the DB. It runs on every clean
	// shutdown so a stop doesn't drop the recording since the last segment
	// rotation.
	cleanup := func() {
		engine.Close()
		db.Close()
	}

	// runServer serves until a platform shutdown trigger fires — SIGINT/SIGTERM
	// on Unix, and on Windows either Ctrl+C or the Service Control Manager's
	// Stop/Shutdown control — then drains the HTTP server and runs cleanup. It
	// is the single graceful-shutdown path for both the terminal and the
	// Windows service. A fatal serve error returns non-nil and exits non-zero.
	if err := runServer(srv, cleanup); err != nil {
		fatal("server stopped", "err", err)
	}
}

// cliOptions holds the parsed CLI flags. A zero value means "not set";
// empty-string fields defer to the env var / config-file / built-in
// default chain.
type cliOptions struct {
	showHelp           bool
	showVersion        bool
	dataDir            string
	configFile         string
	camerasDir         string
	dbPath             string
	host               string
	port               string
	logLevel           string
	noCache            bool
	staticCacheControl string
	accessTTLHours     int
	refreshTTLDays     int
	reindex            bool
}

// parseFlags wires up the flag set, parses os.Args[1:], and returns the
// resolved options plus a function the caller can invoke to print the
// help text (used by --help / -h). --version is handled in main().
func parseFlags() (cliOptions, func()) {
	fs := flag.NewFlagSet("eneverre", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := func() {
		fmt.Fprintf(fs.Output(), "eneverre-api %s\n\n", version)
		fmt.Fprintf(fs.Output(), "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintf(fs.Output(), "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(fs.Output(), "\nPath-resolution precedence (highest first):\n")
		fmt.Fprintf(fs.Output(), "  CLI flag  >  ENEVERRE_* env var  >  built-in defaults\n")
		fmt.Fprintf(fs.Output(), "\nFile-path env vars (also overridable as flags):\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_DATA_DIR      -> --data-dir\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_CONFIG_PATH   -> --config, -c\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_CAMERAS_DIR   -> --cameras-dir\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_DB_PATH       -> --db\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_LOG_LEVEL     -> --log-level\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_STATIC_DIR    -> on-disk UI dir (no flag, env-only)\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_ACCESS_TOKEN_TTL_HOURS  -> --access-token-ttl-hours\n")
		fmt.Fprintf(fs.Output(), "  ENEVERRE_REFRESH_TOKEN_TTL_DAYS  -> --refresh-token-ttl-days\n")
	}
	fs.Usage = usage

	var opts cliOptions
	// Files
	fs.StringVar(&opts.dataDir, "data-dir", "", "config folder: config, cameras dir and DB default to <dir>/eneverre.ini, <dir>/cameras.d, <dir>/eneverre.db (e.g. --data-dir ./data-quincho)")
	fs.StringVar(&opts.configFile, "config", "", "path to eneverre.ini")
	fs.StringVar(&opts.configFile, "c", "", "alias for --config")
	fs.StringVar(&opts.camerasDir, "cameras-dir", "", "directory with camera .ini files")
	fs.StringVar(&opts.dbPath, "db", "", "path to SQLite database file")
	// Network
	fs.StringVar(&opts.host, "host", "", "listen host (default: [server] host or 0.0.0.0)")
	fs.StringVar(&opts.port, "port", "", "listen port (default: [server] port or 8080)")
	// Behavior
	fs.StringVar(&opts.logLevel, "log-level", "", "log level: debug, info, warn, error")
	fs.BoolVar(&opts.noCache, "no-cache", false, "send Cache-Control: no-store on static assets (forces a fresh download on every page load)")
	fs.BoolVar(&opts.reindex, "reindex", false, "rebuild the recording index from segments on disk before starting (recover from a lost or corrupt index)")
	// Token lifetimes (0 = unset -> env / [auth] / default)
	fs.IntVar(&opts.accessTTLHours, "access-token-ttl-hours", 0, "access token lifetime in hours (overrides env / [auth]; default 24)")
	fs.IntVar(&opts.refreshTTLDays, "refresh-token-ttl-days", 0, "refresh token lifetime in days (overrides env / [auth]; default 90)")
	// Info
	fs.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	fs.BoolVar(&opts.showVersion, "v", false, "alias for --version")
	fs.BoolVar(&opts.showHelp, "help", false, "print this help and exit")
	fs.BoolVar(&opts.showHelp, "h", false, "alias for --help")

	if err := fs.Parse(os.Args[1:]); err != nil {
		usage()
		os.Exit(2)
	}

	// Resolve --no-cache into a concrete Cache-Control value. Done here
	// (not inline in main) so the App constructor receives a single,
	// well-defined string and never has to know about the flag.
	if opts.noCache {
		opts.staticCacheControl = "no-store"
	} else {
		opts.staticCacheControl = "no-cache"
	}
	return opts, usage
}
