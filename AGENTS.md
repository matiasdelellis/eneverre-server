# AGENTS.md

## What this is
Eneverre is a manufacturer-agnostic NVR API. It is **not** a streamer — actual
RTSP/HLS/WebRTC is delegated to an external service (MediaMTX, go2rtc, or
lightNVR). The API exists to:
- serve a uniform camera list to clients (Android, Android TV, Web)
- mediate a Bearer-token device-login flow (used by TV/headless clients)
- proxy PTZ (move/home/recalibrate), privacy (lens blackout), and thumbnail
  requests to [Thingino](https://thingino.com/) cameras
- optionally hold MediaMTX credentials and proxy playback/recording calls

Stack: **Go** (single static binary). HTTP via the stdlib `net/http`
`ServeMux` (method+pattern routing). SQLite via pure-Go `modernc.org/sqlite`
(no CGO). Passwords use Werkzeug-compatible hashing (`scrypt`/`pbkdf2`) so an
existing `data/eneverre.db` keeps working. INI parsing via `gopkg.in/ini.v1`.
The web UI is vanilla JS, embedded with `go:embed`.

The service was ported from an earlier Python/FastAPI implementation, which has
been removed. API behavior was verified against the Python version with a
differential test (identical HTTP status across all endpoints; identical
response bodies on every data-bearing route).

## Project documentation
- [`README.md`](README.md) — user-facing intro, quick start, install recipe.
- [`doc/example/README.md`](doc/example/README.md) — every INI key of
  `eneverre.ini` and `cameras.d/<id>.ini`, the `systemd` install steps,
  and the hardening notes.
- [`doc/MEDIAMTX.md`](doc/MEDIAMTX.md) — the MediaMTX integration in
  depth: the `POST /api/auth` protocol, the credential-rotation
  lifecycle, and the reverse-proxy caveats. Read this before touching
  anything in `internal/mediamtx` or `handlers_auth.go:handleMediaMTXAuth`.
- [`doc/UPDATES.md`](doc/UPDATES.md) — the auto-update protocol for the
  Android clients.
- [`doc/TALK.md`](doc/TALK.md) — the two-way-audio (push-to-talk) WebSocket
  protocol and the Android (Kotlin/OkHttp) client implementation guide. Read
  this before touching `handlers_talk.go` or `internal/backchannel`.
- [`doc/RELEASES.md`](doc/RELEASES.md) — release process, supported
  platforms, and how to verify a download.
- [`doc/openapi.yaml`](doc/openapi.yaml) — machine-readable API spec;
  update it when routes change (there is no auto-generation).
- [`go/README.md`](go/README.md) — Go internals, layout, and the
  endpoint-parity notes with the old Python service.

## Layout
All code lives under `go/` (module `eneverre`).
- `go/main.go` — bootstrap: `config.Load()`, `store.Open()`+`store.Init()`,
  `mediamtx.NewStore(db)` (reads/writes the credential row, so it runs after the
  schema exists), `camera.Load()`, `server.New()`, then serves
  via an `http.Server` with explicit timeouts (ReadHeader 5s / Read 15s /
  Write 30s / Idle 60s) so a slow or idle client can't hold a goroutine open
  indefinitely. Starts MediaMTX credential rotation when enabled.
- `go/embed.go` — `//go:embed all:static` of the web UI.
- `go/static/` — the vanilla-JS frontend (no build step). `index.html`,
  `style.css`, `timeline.js`, vendored `hls.min.js`, and `app.js` — the entry
  module that imports and boots the ES modules under `go/static/js/`. Those are
  split into `js/api.js` (fetch wrapper + token), `js/state.js`, `js/util/*`
  (dom/format/storage helpers), `js/ui/*` (theme, password reveal, user menu,
  dialog) and `js/views/*` (login, app-shell, sidebar, wall, ptz, playback,
  hls, users, device-auth). The browser resolves the imports directly. This is
  the canonical copy; edit here.
- `go/internal/config` — INI loading and path resolution. Searches
  `/etc/eneverre/...` then `./data/...`; env overrides
  `ENEVERRE_CONFIG_PATH` / `ENEVERRE_CAMERAS_DIR` / `ENEVERRE_DB_PATH`. Keys are
  read case-insensitively (configparser parity, e.g. `home_Y` → `home_y`).
- `go/internal/store` — opens SQLite (WAL + busy_timeout), runs the schema
  (`users`, `device_login`, `tokens`, `events`, `mediamtx_credentials`),
  idempotent column migrations,
  and seeds an admin from `ENEVERRE_ADMIN_USER`/`ENEVERRE_ADMIN_PASS`
  (default `admin`/`eneverre`) when the users table is empty. **Change the
  default password before any non-local use.**
- `go/internal/auth` — `CheckPasswordHash`/`GeneratePasswordHash` (Werkzeug
  format) plus Basic/Bearer verification and `CurrentUser`. Bearer reads the
  `tokens` table and rejects expired tokens.
- `go/internal/camera` — `Camera` model + INI loader. Thingino credential
  fields are tagged `json:"-"`, so marshaling a `Camera` is the public view
  (no credential leak). `WithMediaMTXURLs` rebuilds rtsp/webrtc/hls/live from
  the current credentials **per request** (see rotation below).
- `go/internal/mediamtx` — credential `Store`: keeps the live pair in memory
  and persists it to the single-row `mediamtx_credentials` table (`NewStore`
  reads it at startup or generates a fresh pair when the table is empty, and
  `Rotate` upserts it). Builds the authenticated stream URLs and rotates
  credentials with a one-interval grace window (`Validate` accepts the current
  or previous pair so active streams aren't dropped at rotation).
  `Current`/`Validate` are in-memory, so the per-request path never touches the
  DB. The companion MediaMTX config (auth endpoint, listeners, recording
  defaults) is in `doc/example/mediamtx.yml`; the wire-level details of the
  `POST /api/auth` probe are in `doc/MEDIAMTX.md`.
- `go/internal/thingino` — direct HTTP calls to Thingino cameras (`Move` for
  PTZ, `Thumb` for JPEG). Unreachable/non-2xx → caller maps to `502`.
- `go/internal/backchannel` — two-way-audio (push-to-talk) to a camera's ONVIF
  Profile T backchannel, a library port of the standalone `web2rtsp` PoC.
  `Dial` opens the RTSP session (OPTIONS/DESCRIBE/SETUP/PLAY, Basic+Digest auth),
  `Session.FeedPCM` takes native-rate mono S16LE and does anti-alias LPF →
  linear resample to 8 kHz → G.711 (A-law/µ-law) → 160-sample RTP frames every
  20 ms → RTSP interleaved (`$`-framing, channel 0). Hand-implemented RTSP/G.711/
  RTP with the stdlib; only new external dep is `gorilla/websocket` (transport
  used by the handler). Trace via `ENEVERRE_LOG_LEVEL=debug`.
- `go/internal/events` — `Event` model (RFC3339-on-the-wire, unix-internally)
  plus record/list/get/delete. `RecordMotion` extends an overlapping row to
  the union of ranges.
- `go/internal/updates` — auto-update sidecar store (one directory per client
  track, `tv` and `phone`). Each track holds a `manifest.json` + the current
  APKs + an in-flight `pending.json`. Supports single- and multi-POST
  publishes (the publish handler can stream one APK per POST and finalize with
  `finalize=true`); at commit, APKs that aren't in the new release are
  deleted (rotation is bounded to the current release's APKs). The wire
  protocol lives in `doc/UPDATES.md`.
- `go/internal/server` — `App` (holds cfg, db, cred store, cameras, static FS,
  per-track update stores) and all handlers, split across `server.go`,
  `helpers.go`, `handlers_auth.go`, `handlers_events.go`,
  `handlers_playback.go`, `handlers_users.go`, `handlers_updates.go`.

## Run / verify
- Build: `go -C go build -o ../eneverre .` → one static binary.
- Run: `./eneverre` (listens on `[server] host`/`port`, default `0.0.0.0:8080`).
- Test/vet: `go -C go test ./...`, `go -C go vet ./...`.
- Manual smoke (from the project root, after `./eneverre` is running):
  - `curl localhost:8080/api/health`
  - `curl -u admin:eneverre localhost:8080/api/cameras`
  - open `http://localhost:8080/` for the web UI.

## Logging
Structured `slog` (text on stderr). Level via `ENEVERRE_LOG_LEVEL` or
`[server] log_level` (debug/info/warn/error, default info). An access-log
middleware logs one line per request; `POST /api/auth` logs each MediaMTX
authorization (user/action/path/protocol/ip/result, never the password) —
denials at WARN, grants at DEBUG. Use `ENEVERRE_LOG_LEVEL=debug` to trace
MediaMTX auth and see request query strings.

## Behavioral quirks
- Cameras are loaded **once at startup** from `/etc/eneverre/cameras.d/*.ini`
  (with `./data/cameras.d/*.ini` as a dev fallback, overridable via
  `ENEVERRE_CAMERAS_DIR`). Edits require a restart. A file missing a `[camera]`
  section or `id` is skipped.
- MediaMTX stream URLs are generated per request from the current credentials,
  so credential rotation needs no restart and no MediaMTX config change
  (MediaMTX delegates auth to `POST /api/auth`). The probe body, the grace
  window, and the reverse-proxy caveats are in `doc/MEDIAMTX.md`.
- MediaMTX credentials live in the `mediamtx_credentials` table (one row). On
  first run a random 8-char username/password is generated and rotated every
  `[mediamtx] rotate_hours` (default 24; `0` disables). The previous pair
  stays valid for one interval as a grace window so live streams don't get
  dropped at rollover — see `doc/MEDIAMTX.md` for the full lifecycle.
- The webhook (`POST /api/camera/{id}/events`) accepts any body shape; on a
  parse failure it still records a motion event and stashes the raw body in
  `source` as `webhook:raw (...)`. Requires `[events] webhook_secret` (via
  `X-Webhook-Secret` header or `token` query param), else `503`. The recorded
  range is widened to `[ts - pre_seconds, ts + duration + post_seconds]` (both
  default to `5`; `pre_seconds` / `post_seconds` are read from the `[events]`
  section).
- `POST /api/auth/login` accepts an optional `device_name` in the JSON body
  (alongside `username`/`password`). When present it is stored on the issued
  token (same column the device-login flow uses), so `GET
  /api/users/me/sessions` can label the session; omit it and the column is
  NULL (backward compatible). The expensive password hash runs once here.
- **Token model.** A `tokens` row is one session. Password login issues a
  short-lived **access** token plus a long-lived **refresh** token stored on
  the same row. Both lifetimes are resolved once at startup with precedence
  CLI flag > env > `[auth]` section > default: `--access-token-ttl-hours` /
  `ENEVERRE_ACCESS_TOKEN_TTL_HOURS` / `[auth] access_token_ttl_hours` (default
  24), and `--refresh-token-ttl-days` / `ENEVERRE_REFRESH_TOKEN_TTL_DAYS` /
  `[auth] refresh_token_ttl_days` (default 90). They land in
  `App.accessTTL`/`App.refreshTTL` (seconds). The
  refresh secret lives in the `refresh_token`/`refresh_expires_at` columns.
  `POST /api/auth/refresh`
  (body `{"refresh_token": "..."}`) validates the refresh secret, then rotates
  *both* secrets and slides both expiries **in place with an `UPDATE` on the
  same row** — the session list grows per login, never per refresh. Lookups
  never cross columns: `VerifyBearer` matches `WHERE token = ?`, refresh matches
  `WHERE refresh_token = ?`, so the two are not interchangeable.
- **Device (TV) sessions are deliberately non-renewable**: the device-login
  flow issues only an access token with `refresh_token` left NULL, so they
  cannot hit `/api/auth/refresh` and must re-pair when the access token lapses.
- `cleanupExpiredTokens()` (called opportunistically on login, like
  `cleanupExpiredDevices`) deletes dead sessions: renewable rows past their
  refresh window, non-renewable/legacy rows past their access expiry.
- `GET /api/users/me/sessions` reports a renewable session as alive while its
  *refresh* token is valid (its `expires_at` in the response is the refresh
  expiry) and adds a `renewable` boolean; otherwise it uses the access expiry.
- The `[thingino]` section in a camera INI drives the Thingino capabilities:
  `ptz = true` marks the camera PTZ-capable, and a non-empty `thingino_api_key`
  enables the privacy/thumbnail capabilities. The credential fields
  (`thingino_url`/`thingino_api_key`) never appear in API responses.
- Privacy (lens blackout) is stateful: `App.privacy` is a per-camera in-memory
  map, seeded once at startup from each privacy-capable camera's slow heartbeat
  (`seedPrivacy`, concurrent, best-effort — unreachable cameras stay `false`)
  and updated by `POST /api/camera/{id}/privacy`. Enabling privacy first moves
  the PTZ to `privacy_x`/`privacy_y` (when both ≥ 0); disabling returns it to
  `home_x`/`home_y`. `home_x/y` and `privacy_x/y` default to `-1` (unset → no
  auto-move). `GET /api/cameras` reflects the current privacy state per camera.
- `playback` talks to MediaMTX on `http://localhost:<playback_port>` — both
  must run on the same host. Unreachable upstreams surface as `502`.
- **Two-way audio (push-to-talk).** `GET /api/camera/{id}/talk` upgrades to a
  WebSocket that relays client mic audio to the camera's ONVIF backchannel
  (see `internal/backchannel`). It is enabled only when the camera INI defines a
  `backchannel` RTSP URL (→ `Capabilities.Talk`); that URL must reach the camera
  directly, since MediaMTX does not relay backchannel. Auth (validated **before**
  the upgrade, by `auth.VerifyToken`): the access token rides the
  `Sec-WebSocket-Protocol` carrier — the browser offers `["eneverre-talk",
  <token>]` and the server echoes only `eneverre-talk`, keeping the token out of
  the URL and reverse-proxy logs — with a `?token=` query param and a Bearer
  header as fallbacks. Sessions are one per camera — a second client gets `409`
  — tracked in `App.talk` (guarded by `talkMu`; a nil placeholder reserves the
  slot during the RTSP handshake). Wire protocol: client sends JSON
  `{"sampleRate": N}` then binary S16LE PCM; once the RTSP session is live the
  server sends one text `{"status":"ready"}` (so the UI switches
  connecting→talking) and thereafter pings every 25s (drops the session if no
  pong/audio within 60s, reclaiming the slot from dead clients). The browser
  client is `static/js/util/talk-client.js`, wired to a hold-to-talk button
  (pointer-capture, no leaked listeners) in the PTZ/control modal
  (`static/js/views/ptz.js`). Note: WebSocket over HTTP/3 fails behind Caddy —
  restrict it to `protocols h1 h2` (see the project memory note).

## Adding an API endpoint
- Register the route in `server.go`'s `Handler()` with a method+pattern
  (`mux.HandleFunc("GET /api/...", a.handleX)`); read wildcards with
  `r.PathValue("...")`.
- Gate auth at the top of the handler: `a.requireUser(w, r)` (Basic or Bearer)
  or `a.requireAdmin(w, r)`; both write the 401/403 and return nil on failure.
- Respond with `writeJSON(w, status, v)` and `httpError(w, status, detail)`
  (FastAPI-compatible `{"detail": "..."}` shape).
- For camera responses, marshal `camera.Camera` (credentials are already
  excluded) and apply `WithMediaMTXURLs` so URLs reflect current credentials.

## Adding a new camera
1. Drop a new `<id>.ini` under `data/cameras.d/` (or `/etc/eneverre/cameras.d/`
   in production). Use `doc/example/cameras.d/camera01.ini` (PTZ Thingino) and
   `doc/example/cameras.d/camera02.ini` (fixed) as templates — every key is
   documented in `doc/example/README.md`. The file's `id` must match the path
   the camera is published under in MediaMTX (see `doc/MEDIAMTX.md`).
2. Add a `[thingino]` section for PTZ / thumbnail / privacy credentials if the
   camera is a [Thingino](https://thingino.com/). A non-empty
   `thingino_api_key` enables thumbnail + privacy; `ptz = true` enables the
   PTZ endpoints. Credential fields are tagged `json:"-"` and never appear in
   API responses.
3. Restart the API (cameras are loaded at startup).

## Frontend notes
- Single static page in `go/static/`, embedded in the binary. No build step.
- `ENEVERRE_STATIC_DIR` (or an on-disk `./app/static` / `../app/static`) takes
  precedence over the embedded copy — handy for live edits without rebuilding.
- The HLS URL from `/api/cameras` already embeds MediaMTX credentials; hls.js
  re-sends them per segment. If MediaMTX is a different origin it must send
  permissive CORS headers. The Bearer token lives in `localStorage`.
- The playback timeline (`timeline.js`) draws MediaMTX recordings as the
  background bar and motion events from `GET /api/cameras/{id}/events` as red
  Major1 markers (`fetchEvents` in `js/views/playback.js`). Clicking a marker
  seeks playback to the event's start. Both are fetched per camera for the last
  24h when the timeline is built.
- **Auto-update prompt** (`js/views/upgrade-prompt.js`): on boot, the page
  sniffs `navigator.userAgent` for an Android device (TV vs phone/tablet).
  On Android it GETs `/api/app/{tv,phone}/update` (anonymous) and, on 200,
  shows a small dismissible bottom banner with a "Download" link to the
  APK. On 204 / 503 / network error the banner is suppressed. Dismissal
  is per-session (`sessionStorage["eneverre.upgradePrompt.dismissedVersionCode"]`)
  and version-keyed: a new release re-prompts. The check is non-blocking
  and runs in parallel with login. Detection is heuristic: Android TV
  signals include `Android TV`, `AFT*` (Fire TV), `GoogleTV`, `Chromecast`
  (with Android in the UA), `SmartTV`, `BRAVIA` and `; CrKey`; anything
  else matching `Android` is treated as a phone/tablet. iOS, desktop and
  non-Android TVs are not detected.
