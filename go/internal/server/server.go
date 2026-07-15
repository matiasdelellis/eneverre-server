// Package server wires the HTTP routes and holds the application state,
// porting app/main.py and the app/routers package onto net/http.
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"eneverre/internal/auth"
	"eneverre/internal/backchannel"
	"eneverre/internal/camera"
	"eneverre/internal/config"
	"eneverre/internal/media"
	"eneverre/internal/metrics"
	"eneverre/internal/streamauth"
	"eneverre/internal/thingino"
	"eneverre/internal/updates"
)

// App carries the shared state for every request handler.
type App struct {
	cfg   *config.Config
	db    *sql.DB
	creds *streamauth.Store
	// camStore is the DB-backed source of truth for cameras. Reads for request
	// handling go through the in-memory `cameras` snapshot (rebuilt on every
	// create/delete); camStore is written on create/delete and read at startup.
	camStore *camera.Store
	// camerasMu guards the cameras slice, which is now mutable: the create/delete
	// endpoints add and remove entries at runtime. Every handler reads it through
	// getCamera/listCameras under this lock.
	camerasMu sync.RWMutex
	cameras   []camera.Camera
	// engine is the embedded media engine (recording, RTSP relay, live MSE,
	// playback). Always attached in normal operation (main builds and starts
	// it unconditionally); the [media] section only tunes it. May be nil in
	// tests that construct App directly without SetMediaEngine.
	engine    *media.Engine
	staticDir string
	assets    map[string]staticAsset // precomputed embedded UI (etag + gzip), nil if none
	// staticCacheControl is the Cache-Control header value sent for embedded
	// or on-disk static assets. Default "no-cache" (browser may store but
	// must revalidate via ETag). Use "no-store" from --no-cache to force
	// the browser to re-download on every page load (useful right after
	// a deploy, or as a permanent toggle for very low-traffic installs).
	staticCacheControl string
	// accessTTL / refreshTTL are the token lifetimes in seconds, resolved once
	// at startup from the [auth] section. accessTTL is the Bearer (access)
	// token life (login + device); refreshTTL is the refresh-token life that a
	// password-login session slides forward on each refresh.
	accessTTL  int64
	refreshTTL int64
	// secLog records authentication failures for fail2ban / CrowdSec. Never
	// nil (newSecLogger falls back to main-log-only when no file is set).
	secLog *secLogger

	// cleanupGrace is the number of seconds a token stays visible in the
	// sessions list after it expires. The background cleaner deletes tokens
	// only when they have been expired for longer than this window. This lets
	// the frontend display expired sessions (in a separate "expired" list)
	// instead of having them disappear the moment they lapse.
	cleanupGrace int64

	// privacy tracks the live privacy (lens blackout) state per camera id. It is
	// seeded once at startup from each camera's slow heartbeat and thereafter
	// updated whenever the privacy endpoint toggles it. Guarded by privacyMu
	// since handlers run concurrently and /api/cameras reads it.
	privacyMu sync.RWMutex
	privacy   map[string]bool

	// updates holds the per-track auto-update stores (keys: "tv", "phone").
	// Empty when the [updates] section is not configured; in that case the
	// /api/app/* endpoints answer 503.
	updates map[string]*updates.Store

	// talk tracks the live two-way-audio backchannel session per camera id.
	// A camera is present in the map (possibly with a nil placeholder during
	// RTSP setup) while a client is talking, so a second client is rejected.
	// Guarded by talkMu since talk handlers run concurrently.
	talkMu sync.Mutex
	talk   map[string]*backchannel.Session

	// talkCodecs holds the push-to-talk codecs each camera accepts, discovered by
	// probing its backchannel SDP once at startup (see seedTalkCodecs). Guarded by
	// talkCodecsMu since the background probe writes it while /api/cameras reads.
	talkCodecsMu sync.RWMutex
	talkCodecs   map[string][]string

	// metrics exposes Prometheus and JSON instrumentation for the entire service.
	// Nil when not configured (tests).
	metrics *metrics.Store
}

// Token-lifetime defaults, used when nothing (flag/env/[auth]) sets them.
// Access in hours, refresh in days, each in its natural human scale.
const (
	DefaultAccessTTLHours = 24 // 1 day
	DefaultRefreshTTLDays = 90
)

// New builds an App and resolves the static UI directory. staticFS is the
// embedded web UI used as a fallback when no on-disk dir is present; pass nil
// to disable the fallback. staticCacheControl is the Cache-Control value
// served for static assets; pass "" to use the default ("no-cache").
// accessTTL/refreshTTL are the token lifetimes in seconds (already resolved by
// the caller with flag/env/config precedence); pass <= 0 to fall back to the
// built-in defaults. updateStores are the per-track auto-update stores; pass
// nil when the feature is not configured.
func New(cfg *config.Config, db *sql.DB, creds *streamauth.Store, camStore *camera.Store, cameras []camera.Camera, staticFS fs.FS, staticCacheControl string, accessTTL, refreshTTL int64, updateStores map[string]*updates.Store) *App {
	if staticCacheControl == "" {
		staticCacheControl = "no-cache"
	}
	if accessTTL <= 0 {
		accessTTL = int64(DefaultAccessTTLHours) * 3600
	}
	if refreshTTL <= 0 {
		refreshTTL = int64(DefaultRefreshTTLDays) * 86400
	}
	a := &App{
		cfg:                cfg,
		db:                 db,
		creds:              creds,
		camStore:           camStore,
		cameras:            cameras,
		staticDir:          resolveStaticDir(),
		staticCacheControl: staticCacheControl,
		accessTTL:          accessTTL,
		refreshTTL:         refreshTTL,
		cleanupGrace:       int64(cfg.AuthCleanupGraceHours()) * 3600,
		secLog:             newSecLogger(cfg.AuthSecurityLog()),
		privacy:            make(map[string]bool),
		updates:            updateStores,
		talk:               make(map[string]*backchannel.Session),
		talkCodecs:         make(map[string][]string),
	}
	// Precompute the embedded UI (ETag + gzip) so repeat loads revalidate
	// cheaply instead of re-downloading. Only used when no on-disk dir wins.
	if staticFS != nil {
		a.assets = buildStaticAssets(staticFS)
	}
	// Seed live privacy state in the background — the slow heartbeat must not
	// delay serving, and any camera that's unreachable just stays at false.
	go a.seedPrivacy()
	// Discover per-camera talk codecs in the background, for the same reason: the
	// RTSP probe must not delay serving, and unreachable cameras just report no
	// codecs (clients then assume G.711).
	go a.seedTalkCodecs()
	// Start the periodic token-cleanup ticker (0 or negative interval means
	// the background loop is disabled; cleanup still runs on login).
	if min := cfg.AuthCleanupIntervalMinutes(); min > 0 {
		go a.startTokenCleaner(time.Duration(min) * time.Minute)
	}
	return a
}

// getCamera returns a copy of the camera with the given id (and true), or a
// zero Camera and false. Taken under the read lock so it is safe against a
// concurrent create/delete. Callers get a value copy, so mutating it never
// races with another reader.
func (a *App) getCamera(id string) (camera.Camera, bool) {
	a.camerasMu.RLock()
	defer a.camerasMu.RUnlock()
	for i := range a.cameras {
		if a.cameras[i].ID == id {
			return a.cameras[i], true
		}
	}
	return camera.Camera{}, false
}

// listCameras returns a snapshot copy of the current camera set under the read
// lock, so iterating it never races with a create/delete.
func (a *App) listCameras() []camera.Camera {
	a.camerasMu.RLock()
	defer a.camerasMu.RUnlock()
	out := make([]camera.Camera, len(a.cameras))
	copy(out, a.cameras)
	return out
}

// addCamera appends a camera to the in-memory set under the write lock. Called
// by the create endpoint after the DB row and engine pipeline are in place.
func (a *App) addCamera(cam camera.Camera) {
	a.camerasMu.Lock()
	defer a.camerasMu.Unlock()
	a.cameras = append(a.cameras, cam)
}

// updateCamera replaces the in-memory entry for cam.ID in place (preserving its
// position) under the write lock. Returns true when the camera was present.
func (a *App) updateCamera(cam camera.Camera) bool {
	a.camerasMu.Lock()
	defer a.camerasMu.Unlock()
	for i := range a.cameras {
		if a.cameras[i].ID == cam.ID {
			a.cameras[i] = cam
			return true
		}
	}
	return false
}

// removeCamera drops the camera with the given id from the in-memory set under
// the write lock. Returns true when it was present.
func (a *App) removeCamera(id string) bool {
	a.camerasMu.Lock()
	defer a.camerasMu.Unlock()
	for i := range a.cameras {
		if a.cameras[i].ID == id {
			a.cameras = append(a.cameras[:i], a.cameras[i+1:]...)
			return true
		}
	}
	return false
}

// SetMetrics attaches the metrics store. Called from main so the /api/metrics
// and /api/metrics/json endpoints serve instrumentation data.
func (a *App) SetMetrics(m *metrics.Store) {
	a.metrics = m
}

// Cameras returns a snapshot copy of the current camera set. Safe for
// concurrent access; used by the metrics collector on each scrape so counts
// track runtime create/delete instead of a stale startup snapshot.
func (a *App) Cameras() []camera.Camera {
	return a.listCameras()
}

// PrivacyState returns the current privacy state for a camera. Safe for
// concurrent access; used by the metrics collector on each scrape.
func (a *App) PrivacyState(id string) bool {
	a.privacyMu.RLock()
	defer a.privacyMu.RUnlock()
	return a.privacy[id]
}

// SetMediaEngine attaches the embedded media engine. Called from main after the
// engine is started, so the playback/live handlers serve from it. A nil
// engine means the [media] section is not configured; the playback endpoints
// answer 404 in that case.
func (a *App) SetMediaEngine(e *media.Engine) {
	a.engine = e
	// Re-apply any privacy state already seeded (thingino cameras that booted in
	// privacy) so a boot-time privacy state also pauses recording + transmission,
	// regardless of whether seedPrivacy ran before or after the engine attached.
	// SetPrivacy is idempotent, so overlapping with seedPrivacy is harmless.
	a.privacyMu.RLock()
	on := make([]string, 0, len(a.privacy))
	for id, p := range a.privacy {
		if p {
			on = append(on, id)
		}
	}
	a.privacyMu.RUnlock()
	for _, id := range on {
		e.SetPrivacy(id, true)
	}
}

// startTokenCleaner runs cleanupExpiredTokens on a ticker. The ticker is
// stopped when the App's lifecycle ends (the goroutine exits when the program
// does). This keeps the tokens table lean between logins, so a rarely-used
// installation doesn't accumulate dead rows for days.
func (a *App) startTokenCleaner(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		a.cleanupExpiredTokens()
	}
}

// seedTalkCodecs probes each backchannel-capable camera once to discover which
// push-to-talk codecs it accepts, so /api/cameras can tell clients whether AAC
// is available instead of leaving them to guess. Cameras are probed
// concurrently; failures (unreachable / auth) are logged and leave the list
// empty, which clients treat as G.711-only.
func (a *App) seedTalkCodecs() {
	for _, c := range a.listCameras() {
		a.seedTalkCodecsFor(c)
	}
}

// seedTalkCodecsFor probes one camera's backchannel codecs in the background
// (no-op for cameras without talk). Called at startup by seedTalkCodecs and
// again when a camera is created, so a newly added talk-capable camera advertises
// its codecs without a restart.
func (a *App) seedTalkCodecsFor(c camera.Camera) {
	if !c.Capabilities.Talk || c.Backchannel == "" {
		return
	}
	go func() {
		codecs, err := backchannel.ProbeCodecs(c.Backchannel)
		if err != nil {
			slog.Warn("talk codec probe failed", "camera", c.ID, "err", err)
			return
		}
		a.talkCodecsMu.Lock()
		a.talkCodecs[c.ID] = codecs
		a.talkCodecsMu.Unlock()
		slog.Info("talk codecs discovered", "camera", c.ID, "codecs", codecs)
	}()
}

// seedPrivacy queries each privacy-capable camera's slow heartbeat once to
// initialize the in-memory privacy state. Cameras are polled concurrently;
// failures (unreachable / bad token) are logged and leave the state false.
func (a *App) seedPrivacy() {
	for _, c := range a.listCameras() {
		a.seedPrivacyFor(c)
	}
}

// seedPrivacyFor queries one thingino camera's privacy heartbeat in the
// background (no-op for non-thingino or privacy-disabled cameras). Called at
// startup by seedPrivacy and again when a camera is created.
func (a *App) seedPrivacyFor(c camera.Camera) {
	if !c.Capabilities.Privacy || c.ThinginoURL == "" || c.ThinginoAPIKey == "" {
		return
	}
	go func() {
		hb, err := thingino.State(c.ThinginoURL, c.ThinginoAPIKey)
		if err != nil {
			slog.Warn("privacy seed failed", "camera", c.ID, "err", err)
			return
		}
		a.privacyMu.Lock()
		a.privacy[c.ID] = hb.PrivacyEnabled
		a.privacyMu.Unlock()
		// A camera that booted in privacy must also be paused (stop
		// recording + transmission), not just reflected in the state map.
		if hb.PrivacyEnabled && a.engine != nil {
			a.engine.SetPrivacy(c.ID, true)
		}
	}()
}

func resolveStaticDir() string {
	if d := os.Getenv("ENEVERRE_STATIC_DIR"); d != "" {
		return d
	}
	for _, d := range []string{"./app/static", "../app/static"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return ""
}

// Handler returns the fully wired HTTP handler (routes + CORS).
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// health
	mux.HandleFunc("GET /api/health", a.handleHealth)

	// metrics (Prometheus + JSON). Open to a local scraper (Prometheus on the
	// same host, hitting us directly) but authenticated from anywhere else, so
	// the endpoint is not exposed publicly through the reverse proxy. Registered
	// only when a metrics store has been wired (never in tests).
	if a.metrics != nil {
		mux.Handle("GET /api/metrics", a.gateMetrics(a.metrics.PrometheusHandler()))
		mux.Handle("GET /api/metrics/json", a.gateMetrics(a.metrics.JSONHandler()))
	}

	// browser sessions
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleLogout)
	mux.HandleFunc("POST /api/auth/refresh", a.handleRefresh)

	// cameras and camera operations
	mux.HandleFunc("GET /api/cameras", a.handleCameras)
	// Camera CRUD (admin only). Create/probe live under the plural collection;
	// delete uses the singular per-camera prefix, consistent with the other
	// per-camera operations below.
	mux.HandleFunc("POST /api/cameras", a.handleCreateCamera)
	mux.HandleFunc("POST /api/cameras/probe", a.handleProbeCamera)
	mux.HandleFunc("GET /api/camera/{cam_id}/config", a.handleGetCameraConfig)
	mux.HandleFunc("PUT /api/camera/{cam_id}", a.handleUpdateCamera)
	mux.HandleFunc("DELETE /api/camera/{cam_id}", a.handleDeleteCamera)
	mux.HandleFunc("POST /api/camera/{cam_id}/ptz/move", a.handlePTZMove)
	mux.HandleFunc("POST /api/camera/{cam_id}/ptz/home", a.handlePTZHome)
	mux.HandleFunc("POST /api/camera/{cam_id}/ptz/recalibrate", a.handlePTZRecalibrate)
	mux.HandleFunc("POST /api/camera/{cam_id}/privacy", a.handlePrivacy)
	mux.HandleFunc("GET /api/camera/{cam_id}/thumbnail", a.handleThumbnail)
	mux.HandleFunc("GET /api/camera/{cam_id}/talk", a.handleTalk)
	// recordings (embedded engine). All under the /recordings/ prefix,
	// consistent with the /api/recordings/paths collection.
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/list", a.handlePlaybackList)
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/get", a.handlePlaybackGet)
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/timeline", a.handlePlaybackTimeline)
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/gaps", a.handlePlaybackGaps)
	// collection: camera ids that have recordings (for recordings-only clients)
	mux.HandleFunc("GET /api/recordings/paths", a.handleRecordingPaths)
	// HLS VOD. Playlist init/segment URIs are relative, so they resolve under
	// this same /recordings/hls/ prefix. Gaps between segments are emitted as
	// EXT-X-DISCONTINUITY in the playlist; the player (hls.js, VLC, ExoPlayer,
	// AVPlayer) handles them per the HLS spec.
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/hls/playlist.m3u8", a.handlePlaybackHLSPlaylist)
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/hls/init.mp4", a.handlePlaybackHLSInit)
	mux.HandleFunc("GET /api/camera/{cam_id}/recordings/hls/segment.m4s", a.handlePlaybackHLSSegment)

	// DEPRECATED — legacy /playback/{list,get} aliases, kept ONLY as a
	// compatibility shim during the transition while clients (Android, TV, web)
	// migrate to /recordings/*. These are the only two endpoints that existed
	// under the old /playback/ prefix; they dispatch to the same handlers and
	// tag every response with a `Deprecation` header. Remove once no client hits
	// them. The new endpoints (timeline, gaps, HLS VOD) never had a /playback/
	// form and are not aliased.
	mux.HandleFunc("GET /api/camera/{cam_id}/playback/list", deprecatedAlias("/api/camera/{cam_id}/recordings/list", a.handlePlaybackList))
	mux.HandleFunc("GET /api/camera/{cam_id}/playback/get", deprecatedAlias("/api/camera/{cam_id}/recordings/get", a.handlePlaybackGet))

	// embedded live (MSE fMP4). The engine is always running, so these are
	// always routed; for a camera whose `mse` feature is off there is no
	// broadcaster, so `info` reports {"available": false} and `stream` returns
	// 503, and the client falls back to its hls/webrtc URL from /api/cameras.
	mux.HandleFunc("GET /api/camera/{cam_id}/live/info", a.handleLiveInfo)
	mux.HandleFunc("GET /api/camera/{cam_id}/live/stream", a.handleLiveStream)

	// device login flow
	mux.HandleFunc("GET /api/auth/device", a.handleCreateDevice)
	mux.HandleFunc("GET /api/auth/device/{device_code}", a.handleCheckDevice)
	mux.HandleFunc("POST /api/auth/device/verify", a.handleVerifyDevice)

	// events (all under the singular /api/camera/{cam_id}/ prefix, consistent
	// with the other per-camera endpoints)
	mux.HandleFunc("POST /api/camera/{cam_id}/events", a.handleWebhookEvent)
	mux.HandleFunc("GET /api/camera/{cam_id}/events", a.handleListEvents)
	mux.HandleFunc("DELETE /api/camera/{cam_id}/events/{event_id}", a.handleDeleteEvent)
	// DEPRECATED — legacy plural read alias. Reading events used to live at
	// /api/cameras/{cam_id}/events (plural); kept as a compatibility shim (tagged
	// with a `Deprecation` header) while clients migrate to the singular
	// /api/camera/ prefix above. Remove once no client uses it.
	mux.HandleFunc("GET /api/cameras/{cam_id}/events", deprecatedAlias("/api/camera/{cam_id}/events", a.handleListEvents))

	// auto-update (Android TV + phone). Each track is independent: a publish
	// on one does not touch the other. Endpoints answer 503 when the [updates]
	// section is not configured.
	for _, track := range updateTracks {
		mux.HandleFunc("GET /api/app/"+track+"/update", a.handleAppUpdate(track))
		mux.HandleFunc("POST /api/admin/app/updates/"+track, a.handleAppUpdatesPublish(track))
		mux.HandleFunc("GET /api/app/updates/"+track+"/{filename}", a.handleAppUpdateFile(track))
	}

	// users
	mux.HandleFunc("GET /api/users", a.handleListUsers)
	mux.HandleFunc("POST /api/users", a.handleCreateUser)
	mux.HandleFunc("PUT /api/users/me/password", a.handleChangeMyPassword)
	mux.HandleFunc("PUT /api/users/me/name", a.handleChangeMyName)
	mux.HandleFunc("GET /api/users/me/sessions", a.handleListMySessions)
	mux.HandleFunc("DELETE /api/users/me/sessions/{session_id}", a.handleRevokeMySession)
	mux.HandleFunc("PUT /api/users/{username}/role", a.handleUpdateRole)
	mux.HandleFunc("PUT /api/users/{username}/password", a.handleChangePassword)
	mux.HandleFunc("PUT /api/users/{username}/name", a.handleChangeName)
	mux.HandleFunc("DELETE /api/users/{username}", a.handleDeleteUser)

	// static UI (catch-all, lowest precedence). Prefer an on-disk dir — so UI
	// edits show up without a rebuild — and fall back to the embedded copy.
	switch {
	case a.staticDir != "":
		// On-disk dir wins for live UI edits (dev); no caching so changes show
		// up on refresh.
		slog.Info("serving UI from disk", "dir", a.staticDir)
		mux.Handle("/", http.FileServer(http.Dir(a.staticDir)))
	case a.assets != nil:
		slog.Info("serving UI from embedded assets", "files", len(a.assets))
		mux.HandleFunc("/", a.serveStatic)
	default:
		slog.Warn("static dir not found; UI not served")
	}

	// accessLog is outermost so every request (including CORS preflight) is
	// logged; cors handles OPTIONS before the mux.
	return accessLog(cors(mux))
}

// deprecatedAlias wraps a handler so a legacy route alias flags every response
// as deprecated (RFC 8594-style), pointing at the successor path. Used for the
// compatibility shims kept during the API migration (renamed recordings/events
// endpoints); drop the wrapped routes once no client uses them.
func deprecatedAlias(successor string, fn http.HandlerFunc) http.HandlerFunc {
	warning := fmt.Sprintf(`299 - "Deprecated endpoint; use %s. This alias is a temporary compatibility shim."`, successor)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Warning", warning)
		fn(w, r)
	}
}

// --- response helpers ----------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// httpError mirrors FastAPI's HTTPException body shape: {"detail": "..."}.
func httpError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

// thinginoError turns a Thingino client error into a 502 with an unambiguous
// message. A 401/403 means the camera was reached but rejected our stored API
// token (commonly because it changed), which is distinct from a network
// failure — and must not surface as a 401 to the browser, which would treat it
// as the user's own session expiring.
func thinginoError(w http.ResponseWriter, err error) {
	var se *thingino.StatusError
	if errors.As(err, &se) {
		if se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden {
			httpError(w, http.StatusBadGateway, "Camera rejected the API token — it may have changed; update the camera's token")
			return
		}
		httpError(w, http.StatusBadGateway, fmt.Sprintf("Camera returned HTTP %d", se.Code))
		return
	}
	httpError(w, http.StatusBadGateway, "Camera unreachable: "+err.Error())
}

// --- auth gates ----------------------------------------------------------

func (a *App) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="eneverre"`)
	httpError(w, http.StatusUnauthorized, "Unauthorized")
}

// requireUser enforces Basic-or-Bearer auth, writing 401 and returning nil on
// failure.
func (a *App) requireUser(w http.ResponseWriter, r *http.Request) *auth.CurrentUser {
	u := auth.Current(a.db, r)
	if u == nil {
		// A present-but-invalid HTTP Basic credential is a real brute-force
		// signal: the browser UI authenticates with Bearer tokens and never
		// sends Basic, so a wrong Basic password is a deliberate probe. Log
		// it for fail2ban / CrowdSec. Missing credentials and merely-expired
		// Bearer tokens are normal and intentionally not logged (they would
		// ban legitimate users whose sessions lapsed).
		if user, _, ok := r.BasicAuth(); ok {
			a.logAuthFailure(r, user, "basic_auth_failed")
		}
		a.unauthorized(w)
		return nil
	}
	return u
}

// gateMetrics wraps a metrics handler so it is reachable without credentials
// only from a genuinely local client, and requires normal auth otherwise. A
// local Prometheus scrapes us directly over loopback; anything arriving through
// the reverse proxy is treated as remote and must authenticate.
func (a *App) gateMetrics(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) && a.requireUser(w, r) == nil {
			return
		}
		h.ServeHTTP(w, r)
	})
}

// isLocalRequest reports whether the request came directly from a loopback
// peer with no reverse-proxy in front. It deliberately ignores
// X-Forwarded-For / X-Real-IP: those are client-supplied and trivially spoofed
// (a remote caller could send "X-Forwarded-For: 127.0.0.1"), so they must not
// influence an auth-bypass decision. The presence of either header means the
// request was forwarded — i.e. not a direct local scrape — so it is treated as
// remote regardless of the socket peer, even when the proxy runs on localhost.
func isLocalRequest(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireAdmin enforces auth plus the admin role.
func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) *auth.CurrentUser {
	u := a.requireUser(w, r)
	if u == nil {
		return nil
	}
	if !u.IsAdmin() {
		httpError(w, http.StatusForbidden, "Admin required")
		return nil
	}
	return u
}

// --- simple handlers -----------------------------------------------------

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "eneverre-api"})
}

func (a *App) handleCameras(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	// Rebuild the embedded engine's stream URLs from the current rotating
	// credentials so rotation is reflected immediately. Camera marshals without
	// the Thingino fields, so this is the public view. The global [media]
	// toggles come from the engine (falling back to defaults when the engine is
	// absent, e.g. in tests); camera.ResolveFeatures then decides, per camera,
	// which feeds to advertise — the same rule the engine uses to start them.
	def := media.DefaultOptions()
	gMSE, gRelay, gRecord := def.MSEEnabled, def.RelayEnabled, def.RecordEnabled
	if a.engine != nil {
		gMSE, gRelay, gRecord = a.engine.GlobalToggles()
	}
	creds := a.creds.Current()
	cams := a.listCameras()
	a.privacyMu.RLock()
	a.talkCodecsMu.RLock()
	out := make([]camera.Camera, len(cams))
	for i, c := range cams {
		f := c.ResolveFeatures(gMSE, gRelay, gRecord)
		out[i] = c.WithEngineURLs(a.cfg, creds, r.Host, f)
		out[i].Privacy = a.privacy[c.ID]
		if out[i].Privacy {
			// Paused for privacy: the engine serves no live feed, so don't
			// advertise stale live/relay URLs the client would fail to play.
			out[i].LiveMSE = ""
			out[i].RTSP = ""
		}
		out[i].Capabilities.TalkCodecs = a.talkCodecs[c.ID]
	}
	a.talkCodecsMu.RUnlock()
	a.privacyMu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handlePTZMove(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.PTZ || cam.ThinginoURL == "" || cam.ThinginoAPIKey == "" {
		httpError(w, http.StatusNotFound, "PTZ not available")
		return
	}
	x := queryFloat(r, "x", 0)
	y := queryFloat(r, "y", 0)
	body, err := thingino.Move(cam.ThinginoURL, cam.ThinginoAPIKey, x, y)
	if err != nil {
		thinginoError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handlePTZHome moves the camera to its configured home position (home_x/home_y).
func (a *App) handlePTZHome(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.PTZ || cam.ThinginoURL == "" || cam.ThinginoAPIKey == "" {
		httpError(w, http.StatusNotFound, "PTZ not available")
		return
	}
	body, err := thingino.MoveAbs(cam.ThinginoURL, cam.ThinginoAPIKey, cam.HomeX, cam.HomeY)
	if err != nil {
		thinginoError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handlePTZRecalibrate runs the motor recalibration routine.
func (a *App) handlePTZRecalibrate(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.PTZ || cam.ThinginoURL == "" || cam.ThinginoAPIKey == "" {
		httpError(w, http.StatusNotFound, "PTZ not available")
		return
	}
	body, err := thingino.Recalibrate(cam.ThinginoURL, cam.ThinginoAPIKey)
	if err != nil {
		thinginoError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handlePrivacy toggles a camera's privacy. Privacy stops recording and
// transmission (live MSE + RTSP relay) for any camera by pausing the media
// engine's pipeline for it; on thingino cameras it additionally drives the
// firmware lens blackout (prudynt) and moves a PTZ camera to/from its configured
// privacy position. Available for every camera whose `privacy` capability is on
// (INI `privacy != false`).
func (a *App) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || !cam.Capabilities.Privacy {
		httpError(w, http.StatusNotFound, "Privacy not available")
		return
	}
	enable, err := strconv.ParseBool(r.URL.Query().Get("enable"))
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "Missing or invalid 'enable' query param")
		return
	}

	// Firmware privacy (lens blackout + PTZ privacy position) is only possible
	// on thingino cameras. Non-thingino cameras rely solely on the engine pause
	// below to stop recording and transmission.
	hasThingino := cam.ThinginoURL != "" && cam.ThinginoAPIKey != ""
	var body []byte
	if hasThingino {
		// When enabling privacy, point the camera at its configured privacy
		// position first (both coords default to -1 when unset, so >= 0 means set).
		if enable && cam.PrivacyX >= 0 && cam.PrivacyY >= 0 {
			if _, err := thingino.MoveAbs(cam.ThinginoURL, cam.ThinginoAPIKey, cam.PrivacyX, cam.PrivacyY); err != nil {
				thinginoError(w, err)
				return
			}
		}
		body, err = thingino.SetPrivacy(cam.ThinginoURL, cam.ThinginoAPIKey, enable)
		if err != nil {
			thinginoError(w, err)
			return
		}
		// When disabling privacy, return the camera to its home position.
		if !enable && cam.HomeX >= 0 && cam.HomeY >= 0 {
			if _, err := thingino.MoveAbs(cam.ThinginoURL, cam.ThinginoAPIKey, cam.HomeX, cam.HomeY); err != nil {
				thinginoError(w, err)
				return
			}
		}
	}

	// Software privacy: pause (or resume) the engine's pipeline so the camera
	// stops (or resumes) recording and transmission. Applies to every camera.
	if a.engine != nil {
		a.engine.SetPrivacy(cam.ID, enable)
	}

	a.privacyMu.Lock()
	a.privacy[cam.ID] = enable
	a.privacyMu.Unlock()

	if body != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"privacy": enable})
}

func (a *App) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	cam, ok := a.getCamera(r.PathValue("cam_id"))
	if !ok || cam.ThinginoURL == "" || cam.ThinginoAPIKey == "" {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}
	content, err := thingino.Thumb(cam.ThinginoURL, cam.ThinginoAPIKey)
	if err != nil {
		thinginoError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
