# AGENTS.md

## What this is
Eneverre is a manufacturer-agnostic NVR API. It serves a uniform camera list
to clients (Android, Android TV, Web), mediates a Bearer-token device-login
flow (used by TV/headless clients), offers a per-camera privacy toggle (stops
recording + transmission on any camera), and proxies PTZ
(move/home/recalibrate), the firmware lens blackout, and thumbnail requests to
[Thingino](https://thingino.com/) cameras.

For actual streaming, Eneverre runs an **embedded media engine** —
records, relays (RTSP) and broadcasts (live MSE) every camera
**in-process** in the same Go binary. No external streamer to install or
supervise. Single binary, single systemd unit. Codecs: H264 + AAC/G711.
See [`doc/MEDIA.md`](doc/MEDIA.md).

The historical alternative was an external [MediaMTX] process with
Eneverre as a thin auth/config broker; it was removed when the embedded
engine proved equivalent for H264 cameras. The short rationale is in
[`doc/MEDIA.md`](doc/MEDIA.md#why-the-embedded-engine); the on-disk
segment format is still MediaMTX-compatible (`mtxi` box, same fMP4
layout) so the recorder's output can still be inspected with the
MediaMTX tooling if needed.

Stack: **Go** (single static binary). HTTP via the stdlib `net/http`
`ServeMux` (method+pattern routing). SQLite via pure-Go `modernc.org/sqlite`
(no CGO). Passwords use Werkzeug-compatible hashing (`scrypt`/`pbkdf2`) so an
existing `data/eneverre.db` keeps working. INI parsing via `gopkg.in/ini.v1`.
The web UI is vanilla JS, embedded with `go:embed`. The embedded media engine
adds `gortsplib` (RTSP client) + `mediacommon` (fMP4) + `pion/*` (RTP/SDP).

The service was ported from an earlier Python/FastAPI implementation, which has
been removed. API behavior was verified against the Python version with a
differential test (identical HTTP status across all endpoints; identical
response bodies on every data-bearing route).

## Project documentation
- [`README.md`](README.md) — user-facing intro, quick start, install recipe.
- [`doc/example/README.md`](doc/example/README.md) — every INI key of
  `eneverre.ini` and `cameras.d/<id>.ini`, the `systemd` install steps,
  and the hardening notes.
- [`doc/MEDIA.md`](doc/MEDIA.md) — the embedded media engine: recording,
  RTSP relay, browser (MSE) live, playback, codecs and configuration. Read
  this before touching anything under `internal/media/` or
  `handlers_live.go` / `handlers_playback.go`.
- [`doc/PLANS/H265.md`](doc/PLANS/H265.md) — what's left to add H265/HEVC
  support to the embedded engine and where the browser-wall sits.
- [`doc/PLANS/GAPFILL-DYNAMIC.md`](doc/PLANS/GAPFILL-DYNAMIC.md) — the
  design for a date/time-stamped gap-fill caption (currently a static
  message).
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
  `streamauth.NewStore(db)` (reads/writes the credential row, so it runs
  after the schema exists), `camera.Load()`. The embedded engine is
  always built and started when any camera has a `source` URL — it runs
  in **live-only mode** without `[media]` (live MSE + RTSP relay, no
  recording) and in **full mode** with `[media]` (live + per-camera
  recording). `server.SetMediaEngine()` wires it into the handler set
  regardless of mode. `server.New()` is built next, then the server runs
  on an `http.Server` with explicit timeouts (ReadHeader 5s / Read 15s
  / Write 30s / Idle 60s) so a slow or idle client can't hold a goroutine
  open indefinitely. Credential rotation is started when the engine is
  running (always). SIGINT/SIGTERM trigger a graceful `srv.Shutdown`
  (10s) followed by `engine.Close()`, which finalizes each camera's
  in-progress fMP4 segment so a clean stop doesn't lose the tail of a
  recording (in live-only mode the segment has no disk backing — only
  the live relay/broadcaster state is torn down).
- `go/embed.go` — `//go:embed all:static` of the web UI.
- `go/static/` — the vanilla-JS frontend (no build step). `index.html`,
  `style.css`, `timeline.js`, vendored `hls.min.js`, and `app.js` — the entry
  module that imports and boots the ES modules under `go/static/js/`. Those are
  split into `js/api.js` (fetch wrapper + token), `js/state.js`, `js/util/*`
  (dom/format/storage helpers), `js/ui/*` (theme, password reveal, user menu,
  dialog) and `js/views/*` (login, app-shell, sidebar, wall, ptz, playback,
  hls, mse, users, device-auth). The browser resolves the imports directly.
  This is the canonical copy; edit here.
- `go/internal/config` — INI loading and path resolution. Searches
  `/etc/eneverre/...` then `./data/...`; env overrides
  `ENEVERRE_CONFIG_PATH` / `ENEVERRE_CAMERAS_DIR` / `ENEVERRE_DB_PATH`. Keys are
  read case-insensitively (configparser parity, e.g. `home_Y` → `home_y`).
  `Config` exposes optional section handles (`cfg.Media`, `cfg.Events`,
  `cfg.Auth`, `cfg.Updates`) — `nil` when the section is absent — so
  callers branch with a single nil check. `cfg.Media` specifically
  drives recording mode: when present, the engine records; when absent,
  the engine runs in live-only mode.
- `go/internal/store` — opens SQLite (WAL + busy_timeout), runs the schema
  (`users`, `device_login`, `tokens`, `events`, `streamauth_credentials`),
  idempotent column migrations,
  and seeds an admin when the users table is empty: username from
  `ENEVERRE_ADMIN_USER` (default `admin`), password from
  `ENEVERRE_ADMIN_PASS` or, when unset, a random one logged once at `WARN`.
  No credential is read from a config file. `Init` also runs the
  `mediamtx_credentials` → `streamauth_credentials` rename migration
  for upgrades from pre-rename installs.
- `go/internal/auth` — `CheckPasswordHash`/`GeneratePasswordHash` (Werkzeug
  format) plus Basic/Bearer verification and `CurrentUser`. Bearer reads the
  `tokens` table and rejects expired tokens.
- `go/internal/camera` — `Camera` model + INI loader. Credential fields
  (`thingino_url`/`thingino_api_key`/`backchannel`/`source`) are tagged
  `json:"-"`, so marshaling a `Camera` is the public view (no credential
  leak). `WithEngineURLs` rebuilds the per-request URLs for the embedded
  engine (sets `live_mse` to the same-origin MSE path, populates `rtsp`
  with the relay URL). The camera INI `source` key
  is the direct camera RTSP URL the embedded engine
  records/relays from; `transport` overrides the global `[media] transport`
  per camera; `record = false` opts the camera out of disk recording while
  keeping the live MSE feed and RTSP relay.
- `go/internal/streamauth` — credential `Store`: keeps the live pair in
  memory and persists it to the single-row `streamauth_credentials` table
  (`NewStore` reads it at startup or generates a fresh pair when the
  table is empty, and `Rotate` upserts it). Builds the authenticated RTSP
  relay URL and rotates credentials with a one-interval grace window
  (`Validate` accepts the current or previous pair so active streams
  aren't dropped at rotation). `Current`/`Validate` are in-memory, so the
  per-request path never touches the DB. `Pairs()` returns both the
  current and grace pair (when present) for the embedded RTSP relay to
  validate against, so a stream started before a rotation is not dropped
  the moment the pair rolls.
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
- `go/internal/media` — **embedded media engine** (active when `[media]` is
  configured). One binary, no external streamer. Subpackages:
  - `engine` — top-level orchestrator: owns the recorder, RTSP relay, live
    broadcaster and retention cleaner per camera; `OptionsFromSection` maps
    `[media]` INI keys to a struct; `Close` finalizes every in-progress
    fMP4 segment and shuts everything down on `SIGTERM`/`SIGINT`.
  - `recorder` — per-camera RTSP client (`gortsplib`) that demuxes H264/AAC,
    writes fragmented-MP4 segments on disk and indexes them in SQLite
    (with the `mtxi` box for gapless concatenation on playback). Includes a
    media watchdog (silent-but-alive detection + reconnect) and a
    graceful-segment-finalize on source loss / shutdown. Codecs: H264 + AAC
    / G711. H265/HEVC is not supported (see `doc/PLANS/H265.md`).
  - `recstore` — turns a `record_path` template (`%path`, strftime
    specifiers, `%f` for fractional seconds) into an on-disk path; given a
    list of files it computes the common root for retention pruning.
  - `index` — SQLite-backed segment index: insert / range / `Paths()` /
    `Timeline(start,end,count)` / `Gaps(start,end,minDuration)` / batched
    `Expired(cutoff,limit)` and `DeleteBatch(fpaths)` (one transaction, one
    fsync, instead of N round-trips).
  - `liverelay` — raw RTP passthrough of the recorder's RTP packets, served
    over RTSP on `[media] rtsp_address` (default `:8554`). Auth validates
    against the rotating credential pair (current + grace, via
    `streamauth.Store.Pairs`), so a stream started just before a rotation
    doesn't get dropped. No re-encode — same codec, sub-second latency.
  - `live` — chunked-HTTP fMP4 broadcaster: reads the recorder's RTP,
    remuxes to CMAF fMP4 on the fly, and serves
    `…/live/info` (codec string) and `…/live/stream` (init + parts) for
    browsers via MediaSource Extensions. Latency ~1-2s.
  - `mtxi` — MediaMTX-compatible `mtxi` fMP4 box writer (so the on-disk
    segments are byte-identical to what MediaMTX wrote, and the playback
    muxer can gaplessly concatenate them).
  - `playback` — VOD muxer: `HandleGet` (`/get`, with gap fill),
    `HandleHLSPlaylist` / `HandleHLSInit` / `HandleHLSSegment` (CMAF VOD
    playlist with `EXT-X-DISCONTINUITY` at coverage gaps), plus a gap-fill
    helper that generates a black "NO RECORDING" frame via ffmpeg, caches
    it to `<cache_dir>/gapfill/<WxH>-<msghash>.h264` and reuses it across
    restarts. Codec-agnostic (it just re-muxes the segment data).
  - `retention` — periodic cleaner: queries `Expired` in batches,
    `os.Remove`s the files with bounded parallel workers, calls
    `DeleteBatch` (one tx, one fsync), and prunes the now-empty parents of
    the just-deleted files (cheaper than walking the whole record tree on
    every pass).
- `go/internal/server` — `App` (holds cfg, db, cred store, cameras, the
  optional `*media.Engine` set via `SetMediaEngine`, static FS, per-track
  update stores) and all handlers, split across `server.go`, `helpers.go`,
  `handlers_auth.go`, `handlers_events.go`, `handlers_live.go` (embedded
  engine's `live/info` and `live/stream`),   `handlers_playback.go`
  (recordings list/get/timeline/gaps/HLS-VOD), `handlers_users.go`,
  `handlers_updates.go`. Routes under `/api/camera/{id}/recordings/*` are
  the canonical names; the legacy `/api/camera/{id}/playback/{list,get}`
  and `/api/cameras/{id}/events` are kept as `Deprecation: true` shims
  (see `deprecatedAlias` in `server.go`) and will be removed once no
  client uses them.

## Run / verify
- Build: `go -C go build -o ../eneverre .` → one static binary.
- Run: `./eneverre` (listens on `[server] host`/`port`, default `0.0.0.0:8080`).
- Test/vet: `go -C go test ./...`, `go -C go vet ./...`.
- Manual smoke (from the project root, after
  `ENEVERRE_ADMIN_PASS=devpass ./eneverre` is running — pinning a throwaway
  password so the authed call below is reproducible):
  - `curl localhost:8080/api/health`
  - `curl -u admin:devpass localhost:8080/api/cameras`
  - open `http://localhost:8080/` for the web UI.

## Logging
Structured `slog` (text on stderr). Level via `ENEVERRE_LOG_LEVEL` or
`[server] log_level` (debug/info/warn/error, default info). An access-log
middleware logs one line per request. Use `ENEVERRE_LOG_LEVEL=debug` to
see request query strings and the more verbose media-engine traces
(watchdog events, segment rotations, relay auth attempts).

## Behavioral quirks
- Cameras are loaded **once at startup** from `/etc/eneverre/cameras.d/*.ini`
  (with `./data/cameras.d/*.ini` as a dev fallback, overridable via
  `ENEVERRE_CAMERAS_DIR`). Edits require a restart. A file missing a `[camera]`
  section or `id` is skipped.
- **Single streaming mode (embedded engine).** When `[media]` is set,
  `GET /api/cameras` rewrites each camera's stream fields via
  `WithEngineURLs`: `live_mse` becomes the same-origin MSE path
  (`/api/camera/{id}/live/stream`), `rtsp` becomes the relay
  `rtsp://<user>:<pass>@<host>:<port>/<id>`. The camera's
  `source`/`thingino_*`/`backchannel` are tagged `json:"-"` and never
  appear in responses. Set `[media] rtsp_host` to pin a public host in
  reverse-proxied deployments; otherwise the relay host is taken from the
  request (`r.Host`). When `[media]` is absent the camera is returned
  unchanged from the INI (raw `source` value), and every
  recording endpoint answers 404.
- The rotating credential pair (random 8/8 alphanumeric) guards the
  embedded RTSP relay and is embedded in the relay URL on every
  `/api/cameras` call, so rotation takes effect without a restart. The
  previous pair stays valid for one interval as a grace window so live
  streams don't get dropped at rollover.
- Stream-auth credentials live in the `streamauth_credentials` table (one
  row). On first run a random pair is generated and rotated every
  `[media] rotate_hours` (default 24; `0` disables). Existing pre-rename
  installs (`mediamtx_credentials` table) are migrated on first run — see
  `store.migrateStreamAuthTable`.
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
- `cleanupExpiredTokens()` runs both on every login **and** on a background
  ticker (`[auth] cleanup_interval_minutes`, default 60 min) so the tokens
  table stays lean even on rarely-used installations. Deletes dead sessions:
  renewable rows past their refresh window, non-renewable/legacy rows past
  their access expiry. The deletion applies a grace window
  (`[auth] cleanup_grace_hours`, default 24h) so expired tokens remain visible
  in the sessions list long enough for a user to see them labelled "expired".
  Set the interval to 0 to keep only login-time cleanup. Set the grace to 0
  for the previous immediate-deletion behaviour.
- `GET /api/users/me/sessions` reports a renewable session as alive while its
  *refresh* token is valid (its `expires_at` in the response is the refresh
  expiry) and adds a `renewable` boolean; otherwise it uses the access expiry.
- The `[thingino]` section in a camera INI drives the Thingino capabilities:
  `ptz = true` marks the camera PTZ-capable, and a non-empty `thingino_api_key`
  enables the thumbnail capability plus the firmware lens blackout used by
  privacy. The credential fields (`thingino_url`/`thingino_api_key`) never
  appear in API responses.
- Privacy is a runtime pause available on **every** camera (`Capabilities.Privacy`
  from the `[camera] privacy` key, default true; `privacy = false` marks an
  always-on camera). Enabling it **stops recording and transmission**:
  `handlePrivacy` calls `Engine.SetPrivacy(id, on)`, which disconnects the
  recorder and parks its retry loop (a per-camera `camCtrl` in `engine.go`);
  `OnSourceLost` then tears down the live MSE broadcast + RTSP relay and the
  in-progress segment is finalized/indexed. State lives in `App.privacy` (a
  per-camera in-memory map), seeded once at startup from each thingino camera's
  slow heartbeat (`seedPrivacy`, concurrent, best-effort — unreachable cameras
  stay `false`; a camera that booted in privacy is re-paused via `SetMediaEngine`).
  On thingino cameras privacy additionally drives the firmware lens blackout and
  moves the PTZ to `privacy_x`/`privacy_y` on enable, back to `home_x`/`home_y`
  on disable (`home_x/y` and `privacy_x/y` default to `-1` → no auto-move).
  `GET /api/cameras` reflects the privacy state and withholds `live_mse`/`rtsp`
  while a camera is paused.
- In **embedded mode** (`[media]`) the recordings endpoints serve from the
  in-process segment index, and additional embedded-only endpoints are
  served: timeline, gaps, HLS VOD (`/recordings/hls/*`) and the live MSE
  feed (`/live/{info,stream}`). Without `[media]` every recording endpoint
  answers 404 (and the cameras' `source` is served raw from the INI).
  Full endpoint list, payload shapes and client integration notes are in
  [`doc/MEDIA.md`](doc/MEDIA.md).
- **Two-way audio (push-to-talk).** `GET /api/camera/{id}/talk` upgrades to a
  WebSocket that relays client mic audio to the camera's ONVIF backchannel
  (see `internal/backchannel`). It is enabled only when the camera INI defines a
  `backchannel` RTSP URL (→ `Capabilities.Talk`); that URL must reach the
  camera directly. Auth (validated **before**
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
  excluded) and apply `WithEngineURLs` (the live `engine` is set on `App`
  via `SetMediaEngine` when `[media]` is configured) so URLs reflect the
  embedded engine's stream fields and the rotating relay credentials.
- When wrapping an old route as a temporary alias, use `deprecatedAlias(successor, fn)`
  in `server.go` — it sets `Deprecation: true` + RFC 8594 `Warning` on every
  response so clients can detect the migration.

## Adding a new camera
1. Drop a new `<id>.ini` under `data/cameras.d/` (or `/etc/eneverre/cameras.d/`
   in production). Use `doc/example/cameras.d/camera01.ini` (PTZ Thingino) and
   `doc/example/cameras.d/camera02.ini` (fixed) as templates — every key is
   documented in `doc/example/README.md`. The file's `id` is the path the
   embedded engine records/relays under; the same id was the path the
   external MediaMTX used to publish each camera when that integration was
   the only mode (pre-rename historical note in `doc/MEDIA.md`).
2. Add a `[thingino]` section for PTZ / thumbnail credentials and the firmware
   lens blackout if the camera is a [Thingino](https://thingino.com/). A
   non-empty `thingino_api_key` enables the thumbnail capability and the
   blackout used by privacy; `ptz = true` enables the PTZ endpoints. Credential
   fields are tagged `json:"-"` and never appear in API responses. (Privacy
   itself works on any camera; use `[camera] privacy = false` to opt a camera
   out of being paused.)
3. For the embedded media engine (`[media]`), set `source` to the direct
   camera RTSP URL (it must point at the camera itself, since it carries
   credentials and is never exposed to clients). Use
   `transport = tcp|udp|auto` on a single camera to override the global
   `[media] transport` (e.g. force TCP on a lossy/distant camera). Use
   `record = false` to opt this camera out of disk recording while keeping
   the live MSE feed and RTSP relay (`/recordings/*` for it answer 404) —
   useful for privacy-sensitive cameras you only want to watch live.
4. Restart the API (cameras are loaded at startup).

## Frontend notes
- Single static page in `go/static/`, embedded in the binary. No build step.
- `ENEVERRE_STATIC_DIR` (or an on-disk `./app/static` / `../app/static`) takes
  precedence over the embedded copy — handy for live edits without rebuilding.
- The Bearer token lives in `localStorage`.
- **Live view** (`js/views/wall.js` + `js/views/hls.js` + `js/views/mse.js`):
  the wall uses `camera.live_mse` (the embedded engine's MSE feed at
  `/api/camera/{id}/live/stream`, ~1-2s latency) when the camera exposes
  it. With neither `[media]` nor another streamer in front, the wall
  falls back to `camera.hls` (played with hls.js).
- **HLS VOD playback** (`js/views/playback.js`): the timeline plays
  `/api/camera/{id}/recordings/hls/playlist.m3u8` via hls.js
  (CMAF; `EXT-X-DISCONTINUITY` at coverage gaps), one instance per camera
  tile. The cursor advances from wall-clock (1x) so it stays monotonic
  across gaps. Per-tile "No recording" overlays appear when the cursor
  sits inside a coverage gap; the tile's HLS instance is reinitialized at
  the cursor's current wall-clock when it exits the gap. Scrubbing resets
  everything. Auth: every playlist/init/segment request needs the Bearer
  token (use hls.js `xhrSetup`).
- The playback timeline (`timeline.js`) draws recordings as the background
  bar and motion events from `GET /api/camera/{id}/events` (singular
  prefix; the plural `/api/cameras/{id}/events` is a deprecated alias) as
  red Major1 markers (`fetchEvents` in `js/views/playback.js`). Clicking a
  marker seeks playback to the event's start. Both are fetched per camera
  for the last 24h when the timeline is built.
- **Deprecated endpoint aliases** (`server.go`'s `deprecatedAlias`): legacy
  routes `…/playback/{list,get}` and `/api/cameras/{id}/events` are kept
  as RFC 8594-deprecated shims with `Deprecation: true` and a `Warning`
  header pointing at the canonical path. New clients should hit the
  canonical routes (`/recordings/*` and the singular `/api/camera/`
  prefix); the aliases are removed once no client uses them.
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
