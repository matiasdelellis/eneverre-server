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

## Project documentation
- [`README.md`](README.md) — user-facing intro, quick start, install recipe.
- [`doc/example/README.md`](doc/example/README.md) — every INI key of
  `eneverre.ini` and `cameras.d/*.ini`, the `systemd` install steps,
  and the hardening notes.
- [`doc/MEDIA.md`](doc/MEDIA.md) — the embedded media engine: recording,
  RTSP relay, browser (MSE) live, playback, codecs and configuration. Read
  this before touching anything under `internal/media/` or
  `handlers_live.go` / `handlers_playback.go`.
- [`doc/PLANS/GAPFILL-DYNAMIC.md`](doc/PLANS/GAPFILL-DYNAMIC.md) — the
  design for a date/time-stamped gap-fill caption (currently a static
  message).
- [`doc/PLANS/TALK-AUDIO-QUALITY.md`](doc/PLANS/TALK-AUDIO-QUALITY.md) —
  talk audio-quality work: what's done (G.711 latency cap, client-side AAC
  silence warm-up + no server settle, AAC drop-oldest) and what's deferred
  (stateful resampling, AAC `a=fmtp` parsing, `AudioWorklet` capture).
- [`doc/PLANS/TALK-BACKCHANNEL-LOCAL-GAPS.md`](doc/PLANS/TALK-BACKCHANNEL-LOCAL-GAPS.md) —
  additive plan to close the known backchannel gaps (RTCP SR, AAC `a=fmtp`
  parsing, Digest `qop=auth`, typed codec selection) in the existing
  hand-rolled code. ~1-2 days, low risk, no API change.
- [`doc/PLANS/TALK-BACKCHANNEL-GORTSPLIB-CLIENT.md`](doc/PLANS/TALK-BACKCHANNEL-GORTSPLIB-CLIENT.md) —
  alternative: replace the hand-rolled RTSP client in
  `internal/backchannel` with `gortsplib.Client` (already a dependency via
  the recorder). ~4-6.5 days, deletes ~540 lines, medium risk. Same end state
  on the wire.
- [`doc/PLANS/WEBRTC.md`](doc/PLANS/WEBRTC.md) — evaluation of browser
  live (and the browser leg of talk) over WebRTC: why there is no WebRTC
  today (an omission, not a rejection — three commits on 2026-07-07),
  what it would buy (200-500ms, audio on every camera since G711 is a
  native WebRTC codec, the `<video>` element survives), the AAC-vs-G711
  audio trade-off, and ~2.5-3 weeks of work. **Evaluated and declined**
  (2026-08-15): the Android/TV clients already have sub-second live over
  the RTSP relay, and the web UI is for consultation, where ~1-2s is
  adequate — so the latency premise doesn't hold. Kept as the decision
  record, with measurements (Firefox offers baseline-only H264 over
  WebRTC; the fleet encodes Main) and the conditions that would reopen
  it.
- [`doc/PLANS/MOQ.md`](doc/PLANS/MOQ.md) — decision record on
  Media-over-QUIC for browser live: where the current ~1-2s latency
  actually comes from, what MoQ would buy (sub-300ms, G711 audio in the
  browser, per-track priorities), what it would cost (a hand-written
  WebCodecs player, TLS/UDP where we terminate no TLS today), and the
  triggers to revisit. **Not adopted**, and its premise was superseded
  along with WebRTC's — browser live latency is not a product goal.
- [`doc/UPDATES.md`](doc/UPDATES.md) — the auto-update protocol for the
  Android clients.
- [`doc/TALK.md`](doc/TALK.md) — the two-way-audio (push-to-talk) WebSocket
  protocol and the Android (Kotlin/OkHttp) client implementation guide. Read
  this before touching `handlers_talk.go` or `internal/backchannel`.
- [`doc/RELEASES.md`](doc/RELEASES.md) — release process, supported
  platforms, and how to verify a download.
- [`doc/openapi.yaml`](doc/openapi.yaml) — machine-readable API spec;
  update it when routes change (there is no auto-generation).
- [`go/README.md`](go/README.md) — Go internals, layout, and endpoint notes.

## Layout
All code lives under `go/` (module `eneverre`).
- `go/main.go` — bootstrap: `config.Load()`, `store.Open()`+`store.Init()`,
  `streamauth.NewStore(db)` (reads/writes the credential row, so it runs
  after the schema exists), `camera.Load()`. The embedded engine is
  always built and started when any camera has a `source` URL — live MSE,
  the RTSP relay and disk recording are all on by default. Set `[media]
  record = false` for **live-only mode** (per-camera `record = false` opts
  individual cameras out either way). `server.SetMediaEngine()` wires the engine into the
  handler set regardless of mode. `server.New()` is built next, then the server runs
  on an `http.Server` with explicit timeouts (ReadHeader 5s / Read from
  `[server] read_timeout`, default 5m / Write 30s / Idle 60s) so a slow or
  idle client can't hold a goroutine open indefinitely; the long-streaming
  handlers (live MSE, clip download, APK publish/download) lift the write
  deadline per-request via `clearWriteDeadline`. Credential rotation is started when the engine is
  running (always). SIGINT/SIGTERM trigger a graceful `srv.Shutdown`
  (10s) followed by `engine.Close()`, which finalizes each camera's
  in-progress fMP4 segment so a clean stop doesn't lose the tail of a
  recording (in live-only mode the segment has no disk backing — only
  the live relay/broadcaster state is torn down).
- `go/lifecycle.go` — the platform-agnostic `serveAndShutdown` (serve
  until `stop` fires, then `gracefulShutdown`) shared by the terminal
  shutdown paths. `gracefulShutdown` drains the HTTP server with a 10s
  timeout and runs the caller-supplied `cleanup` (engine + DB close).
- `go/run_unix.go` / `go/run_windows.go` — the platform split for
  `runServer` (signal-driven on Unix; SCM-`Execute` or `Ctrl+C` on
  Windows) and `resolveLogWriter` (stderr everywhere, plus
  `ENEVERRE_LOG_FILE` when running under the Windows Service Control
  Manager so the first-run admin password isn't discarded). On Windows
  the binary is service-aware: it reports `Running`/`StopPending` to
  the SCM and turns a `Stop`/shutdown control into the same graceful
  drain as `Ctrl+C`, so no NSSM/WinSW wrapper is needed.
- `go/embed.go` — `//go:embed all:static` of the web UI.
- `go/static/` — the vanilla-JS frontend (no build step). `index.html`,
  `style.css`, `timeline.js`, vendored `hls.min.js`, and `app.js` — the entry
  module that imports and boots the ES modules under `go/static/js/`. Those are
  split into `js/api.js` (fetch wrapper + token), `js/state.js`,
  `js/i18n.js` + `js/i18n/{en,es}.js` (string catalogs; the UI ships English
  and Spanish), `js/util/*` (dom, format, storage, focus-trap, talk-client),
  `js/ui/*` (theme, password reveal, user menu, dialog, toast, help,
  buffering, cam-status, **icons** — see below) and `js/views/*` — one module
  per screen: login, force-password, device-auth, app-shell, sidebar, wall,
  cameras, ptz, talk, playback, mse, schedules, status, users,
  upgrade-prompt. The browser resolves the imports directly (no build step).
  This is the canonical copy; edit here.
- `go/internal/config` — INI loading and path resolution. Searches
  `/etc/eneverre/...` then `./data/...`; env overrides
  `ENEVERRE_CONFIG_PATH` / `ENEVERRE_CAMERAS_DIR` / `ENEVERRE_DB_PATH`. Keys are
  read case-insensitively (configparser parity, e.g. `home_Y` → `home_y`). The
  main config file is **optional**: a missing default-searched `eneverre.ini`
  yields an empty document (every key falls back to its default), but an
  explicit path (`--config` / `ENEVERRE_CONFIG_PATH`) that doesn't exist is
  fatal, as is a present-but-unparseable file. `Config.FileLoaded` records
  whether a file was actually read (`main.go` logs "using defaults" when not).
  `Config` exposes optional section handles (`cfg.Media`, `cfg.Events`,
  `cfg.Auth`, `cfg.Updates`) — `nil` when the section is absent — so
  callers branch with a single nil check. `cfg.Media` being absent just
  means every `[media]` key falls back to its default, including
  `record = true` — so a missing section and a present-but-unconfigured
  one behave the same (recording on); go live-only only by setting
  `record = false` explicitly, section or no section. The per-platform search paths
  come from `config.go` (Unix) and `paths_windows.go` (Windows, which
  rewrites the slices in `init()` to `%ProgramData%\Eneverre\...`).
- `go/internal/store` — opens SQLite (WAL + busy_timeout), runs the schema
  (`users`, `device_login`, `tokens`, `events`, `streamauth_credentials`,
  `cameras`, `schedules`) plus a short list of idempotent `ALTER TABLE`
  migrations (`Init` swallows a "duplicate column" so re-running is safe — this
  is how `cameras.schedule_id` was backfilled onto upgraded installs),
  and seeds an admin when the users table is empty: username from
  `ENEVERRE_ADMIN_USER` (default `admin`), password from
  `ENEVERRE_ADMIN_PASS` or, when unset, a random one logged once at `WARN`.
  No credential is read from a config file. The seeded admin is inserted with
  `must_change_password = 1` so the web UI forces a new password on first login
  (the `users.must_change_password` column defaults to 0 for every other row).
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
  keeping the live MSE feed and RTSP relay. `schedule_id` references a
  recording schedule (see `internal/schedule`); empty = record 24/7. It is
  **not** read from the INI (the loader never sets it) — a seeded camera always
  starts 24/7 and a program is assigned later through the API/UI.
- `go/internal/schedule` — recording schedules (named programs): the `Schedule`
  model (`Days` maps a weekday key `sun..sat` to `"HH:MM-HH:MM"` armed windows),
  `Active(t)` (evaluated in local time; a window whose end ≤ start wraps past
  midnight), `Validate`/`Normalize`, and a DB-backed `Store` (CRUD over the
  `schedules` table, rules persisted as JSON). The server's scheduler
  (`server.startScheduler`) pauses a camera's pipeline outside its schedule's
  windows — see the privacy/scheduling quirk below.
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
  PTZ, `Thumb` for JPEG, `State` for the slow heartbeat). The heartbeat
  decodes the full read-only runtime state (day/night mode, illuminators,
  motion, mic/speaker, per-channel recording) with tolerant `Bool`/`Num`
  decoders (bool/number/string spellings across firmwares); the server caches
  it per camera (startup seed + 5-min `heartbeatLoop`) and serves it on
  `GET /api/camera/{id}/settings` — never fetched on a request path (~1s per
  call). Privacy seeds from the same fetch. The writable side is `SetPrudynt` (partial
  config fragment on `json-prudynt.cgi`) and `Imp` (allowlisted commands on
  `json-imp.cgi`), exposed to admins as `PUT /api/camera/{id}/settings` under
  the backend-agnostic `Capabilities.Settings` flag. Unreachable/non-2xx →
  caller maps to `502`. Future: newer thingino firmware (raptor) replaces
  this CGI surface with a REST agent on :8080 (GET `/api/v1/config`, POST
  `/api/v1/actions/{record,privacy,daynight,snapshot}`, PATCH
  `/api/v1/settings/{motion/enabled,audio/mic-enabled,audio/spk-enabled}`,
  GET `/api/v1/runtime/media`) — verified live (2026-08) that stable
  cameras only expose `POST /api/v1/config` (same fragment semantics), so
  the setters stay on the CGIs until the fleet migrates; the full route map
  is documented in `thingino.go`'s package comment.
- `go/internal/backchannel` — two-way-audio (push-to-talk) to a camera's ONVIF
  Profile T backchannel, a library port of the standalone `web2rtsp` PoC.
  `Dial` opens the RTSP session (OPTIONS/DESCRIBE/SETUP/PLAY, Basic+Digest
  auth incl. `qop=auth` with nonce counter), parses the SDP into a per-PT
  codec table (multi-codec thingino tracks select the right payload type) and
  the AAC `a=fmtp` framing (fails closed on missing `config=`), then
  `Session.FeedPCM` takes native-rate mono S16LE and does anti-alias LPF →
  linear resample to 8 kHz → G.711 (A-law/µ-law) → 160-sample RTP frames every
  20 ms → RTSP interleaved (`$`-framing, channel 0), with a periodic RTCP
  Sender Report every 5 s on channel 1. AAC and Opus are **passthrough** (no
  server-side encode/decode, no cgo): `FeedAU` forwards client-encoded AAC-LC
  access units (RFC 3640 framing from the track's fmtp) and `FeedOpus`
  forwards raw 20 ms Opus packets (RFC 7587, one packet per RTP frame, +960
  per timestamp). Hand-implemented RTSP/G.711/RTP/RTCP with the stdlib; only
  new external dep is `gorilla/websocket` (transport used by the handler).
  Trace via `ENEVERRE_LOG_LEVEL=debug`.
- `go/internal/events` — `Event` model (RFC3339-on-the-wire, unix-internally)
  plus record/list/get/delete. `RecordMotion` extends an overlapping row to
  the union of ranges.
- `go/internal/updates` — auto-update sidecar store (one directory per client
  track). Track names are arbitrary operator/CI-chosen identifiers (`tv`,
  `phone`, `tablet`, …) — there is no fixed list; a `Registry` lazily creates
  and caches one `Store` per name so concurrent publishes to the same track
  serialize through one mutex. Each track holds a `manifest.json` + the current
  APKs + an in-flight `pending.json`. Supports single- and multi-POST
  publishes (the publish handler can stream one APK per POST and finalize with
  `finalize=true`); at commit, APKs that aren't in the new release are
  deleted (rotation is bounded to the current release's APKs). The wire
  protocol lives in `doc/UPDATES.md`.
- `go/internal/media` — **embedded media engine**. Always built and started;
  the optional `[media]` section only tunes it (a missing section behaves
  exactly like a present-but-empty one). One binary, no external streamer.
  Subpackages:
  - `engine` — top-level orchestrator: owns the recorder, RTSP relay, live
    broadcaster and retention cleaner per camera; `OptionsFromSection` maps
    `[media]` INI keys to a struct; `Close` finalizes every in-progress
    fMP4 segment and shuts everything down on `SIGTERM`/`SIGINT`. When the
    `record_dir` INI key is unset, `resolveRecordDir` prefers the per-platform
    default when it already exists (`paths_other.go` → FHS
    `/var/lib/eneverre/recordings`; `paths_windows.go` →
    `%ProgramData%\Eneverre\recordings`) and otherwise falls back to
    `<data_dir>/recordings` (`config.Config.DataDir`, i.e. the `--data-dir`
    bundle or `./data`).
  - `recorder` — per-camera RTSP client (`gortsplib`) that demuxes video
    (H264 or H265) + AAC/G711, writes fragmented-MP4 segments on disk and
    indexes them in SQLite (with the `mtxi` box for gapless concatenation on
    playback). Includes a media watchdog (silent-but-alive detection +
    reconnect) and a graceful-segment-finalize on source loss / shutdown.
    Codecs: H264/H265 video + AAC/G711 audio. H265 live/playback is advertised
    with its `hvc1` codec string and gated client-side by browser HEVC support
    (see `doc/MEDIA.md` → Codec support).
  - `recstore` — turns a `record_path` template (`%path`, strftime
    specifiers, `%f` for fractional seconds) into an on-disk path; given a
    list of files it computes the common root for retention pruning.
  - `index` — SQLite-backed segment index: insert / range / `Paths()` /
    `Timeline(start,end,count)` / `Gaps(start,end,minDuration)` / batched
    `Expired(cutoff,limit)` and `DeleteBatch(fpaths)` (one transaction, one
    fsync, instead of N round-trips) / `Oldest(limit)` for the low-disk
    emergency purge (force-remove oldest-first, ignoring `[media] retain`).
  - `recovery` — re-indexes segments that are on disk but missing from the
    index, the footage a hard crash (power loss, SIGKILL, panic) leaves
    behind: `Recover` runs per camera at startup and scans only the recent
    tail, `Reindex` rescans everything and is what `--reindex` drives. Both
    keep existing rows and add only what's missing.
  - `probe.go` — one-shot RTSP probe (codec, resolution) used by
    `POST /api/cameras/probe` so the UI can validate a `source` URL before
    the camera is saved.
  - `diskmonitor` — polls free space on the recording volume and fires
    `OnLow` / `OnRecovered` callbacks when the free-bytes figure crosses
    below `[media] min_free_bytes` (default 1 GiB; `0` disables). Hysteresis
    (enter below the threshold, exit at 2x) keeps the watcher from
    flapping. On `OnLow` the engine runs `retention.Cleaner.PurgeToFree`,
    force-removing the oldest segments (ignoring `[media] retain`) until
    free space is back above the high-water mark. Recording is never
    paused — the oldest footage is dropped to make room for the newest. The
    state is exposed on `GET /api/status` under `storage.low_space` and
    `storage.low_space_since`.
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
  - `retention` — segment cleaner. The periodic sweep (`clean`) queries
    `Expired` in batches, `os.Remove`s the files with bounded parallel
    workers, calls `DeleteBatch` (one tx, one fsync), and prunes the
    now-empty parents of the just-deleted files (cheaper than walking the
    whole record tree). `PurgeToFree` reuses the same per-batch machinery
    (`purgeBatch`) but queries `Oldest` and stops on a free-space target
    instead of an age cutoff — this is the engine's low-disk emergency valve.
- `go/internal/diskfree` — shared statfs wrapper (`Available(path)`,
  `Total(path)`) used by both `internal/server` (the `/api/status` snapshot)
  and the media engine's low-disk watcher. Build-tag-split into unix
  (`statfs`) and Windows (`GetDiskFreeSpaceEx`). Returns the caller-available
  figure, which is the honest "free space" for an unprivileged process
  watching recording headroom.
- `go/internal/timeutil` — the shared unix-or-RFC3339 timestamp parsing used
  by the recordings handlers and the events store, so the accepted formats
  can't drift between them (they used to keep separate layout lists).
- `go/internal/metrics` — Prometheus + JSON instrumentation (`Store`, wired
  into `App` via `SetMetrics`). Collectors are called once per scrape: Go
  runtime (stdlib collector), DB pool stats (`db.Stats()`), aggregate camera
  counts (total/connected/mse_active/recording/privacy — no per-camera `id`
  label, so the endpoint can't be used as a per-camera surveillance map), and
  `eneverre_build_info{version}`. Served on `GET /api/metrics` (Prometheus
  text) and `GET /api/metrics/json`; both routes are only registered when
  `SetMetrics` was called (`[server] metrics = false` skips it, so the routes
  404). See [`doc/MEDIA.md`](doc/MEDIA.md#metrics).
- `go/internal/server` — `App` (holds cfg, db, cred store, cameras, the
  optional `*media.Engine` set via `SetMediaEngine`, static FS, per-track
  update stores) and all handlers, split across `server.go` (also `GET
  /api/status`), `helpers.go`, `handlers_auth.go`, `handlers_cameras.go`
  (camera CRUD/probe, PTZ move/home/recalibrate/position), `handlers_events.go`,
  `handlers_live.go` (embedded engine's `live/info` and `live/stream`),
  `handlers_playback.go` (recordings list/get/timeline/gaps/HLS-VOD),
  `handlers_schedules.go` (recording-program CRUD + assignment),
  `handlers_talk.go` (push-to-talk WebSocket → `internal/backchannel`),
  `handlers_users.go`, `handlers_updates.go`. Routes under
  `/api/camera/{id}/recordings/*` are the canonical names. The
  non-handler files carry the cross-cutting middleware: `logging.go`
  (access log + client-IP resolution honoring `[server] trusted_proxies`),
  `seclog.go` (the auth-failure security log fail2ban tails —
  `doc/security-logging.md`), `ratelimit.go` (failed-auth throttle keyed per
  peer socket IP *and* per attempted username; only failures count) and
  `static.go` (embedded-UI serving: content-hash `ETag`, gzip,
  `Cache-Control`).

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
- Cameras are **DB-backed** — the `cameras` table is the source of truth. The
  per-camera `*.ini` files under `cameras_dir` are only an **initial seed**:
  `camera.SeedFromINI` imports them once when the table is empty (a fresh
  install, or an upgrade from the old file-based layout), then they are ignored.
  A file missing a `[camera]` section or `name` is skipped during the seed. The
  camera id is not read from the INI (any `id` key is ignored): it is derived
  from the name (`camera.Slugify` + a numeric suffix on collision), the same way
  the create API assigns one. Thereafter cameras are created and deleted through the admin API
  (`POST /api/cameras`, `DELETE /api/camera/{id}`) and the web wizard
  ("Manage cameras" in the user menu). Create/delete take effect **without a
  restart**: `engine.AddCamera`/`RemoveCamera` bring the camera's recorder,
  relay and MSE pipeline up or down live, and the server's in-memory camera set
  (guarded by `camerasMu`) is updated in step. `store.go` maps rows ↔
  `camera.Spec`; `Spec.Camera()` derives the public model (capabilities) exactly
  as the INI loader did, so both paths agree.
- **Single streaming mode (embedded engine).** `GET /api/cameras` always
  rewrites each camera's stream fields via `WithEngineURLs` — this does
  not depend on `[media]` being present. `live_mse` becomes the
  same-origin MSE path (`/api/camera/{id}/live/stream`) and `rtsp`
  becomes the relay `rtsp://<user>:<pass>@<host>:<port>/<id>`, each shown
  only when that feature resolves on (global toggle AND per-camera
  toggle — see `Camera.ResolveFeatures`). The camera's
  `source`/`thingino_*`/`backchannel` are tagged `json:"-"` and never
  appear in responses, `[media]` or not. Set `[media] rtsp_host` to pin a
  public host in reverse-proxied deployments; otherwise the relay host is
  taken from the request (`r.Host`). `[media]`'s only effect here is
  `record`: when it (or a per-camera override) resolves false — the
  default — every recording endpoint answers 404 for that camera; live
  MSE and the RTSP relay are unaffected.
- The rotating credential pair (random 8/8 alphanumeric) guards the
  embedded RTSP relay and is embedded in the relay URL on every
  `/api/cameras` call, so rotation takes effect without a restart. The
  previous pair stays valid for one interval as a grace window so live
  streams don't get dropped at rollover.
- Stream-auth credentials live in the `streamauth_credentials` table (one
  row). On first run a random pair is generated and rotated every
  `[media] rotate_hours` (default 24; `0` disables).
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
  NULL (backward compatible). The expensive password hash runs once here. The
  login response carries `must_change_password`; the web client gates the app
  on it and routes to a forced change-password screen until the user changes
  it. `PUT /api/users/me/password` clears the flag (`must_change_password = 0`
  in the same UPDATE); admins can set it when creating a user
  (`POST /api/users`) or resetting a password (`PUT /api/users/{u}/password`).
  The flag is UI-enforced only — a valid token still works for API calls.
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
  privacy; the same credentials enable `Capabilities.Settings` — the
  backend-agnostic flag advertising that admins can adjust the camera's live
  settings (motion, mic/speaker, day/night auto) via
  `PUT /api/camera/{id}/settings`. The credential fields
  (`thingino_url`/`thingino_api_key`) never appear in API responses.
- **Live camera settings (admin).** `PUT /api/camera/{id}/settings` adjusts a
  camera's motion detection, mic input, speaker output and day/night auto —
  admin-gated, advertised through the generic `Capabilities.Settings` flag so
  the API shape is not thingino-specific. Today the only backend is thingino:
  `thingino.SetPrudynt` posts a partial config fragment to
  `json-prudynt.cgi` (motion/audio — the same channel privacy uses) and
  `thingino.Imp` sends   allowlisted IMP commands to `json-imp.cgi` (day/night
  auto; the full command vocabulary is firmware-specific and partly
  board-dependent, so nothing is forwarded blindly). Fields apply in order
  and the first failure answers 502; on success the heartbeat cache refreshes
  so `GET /api/camera/{id}/settings` (admin, the same cached snapshot — 404
  until the first heartbeat lands) shows the change within ~1s instead of
  the next 5-min loop.
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
- **Recording schedules share the privacy pause.** A camera's `schedule_id`
  points at a named program (`internal/schedule`); `server.startScheduler` runs
  from `SetMediaEngine`, reconciles once, then re-evaluates on every minute
  boundary (and after any camera/schedule mutation). The **effective** engine
  pause for a camera is `App.privacy[id] || App.schedOff[id]` — manual privacy
  OR outside the schedule's armed windows — applied via the same
  `Engine.SetPrivacy`, under the per-camera privacy op lock so a manual toggle
  and the scheduler can't fight (`applyPause`/`reconcilePause`... see
  `handlers_schedules.go`). By design the scheduler drives **only the software
  pipeline pause**, never the thingino firmware blackout/PTZ moves (those stay
  on the manual toggle, to avoid daily mechanical churn). A camera with no
  schedule is always armed. `App.schedOff` is a per-camera in-memory map (like
  `App.privacy`); `/api/cameras` exposes `schedule_off` and withholds
  `live_mse`/`rtsp` while off-hours, and `/api/status` reports it per camera.
  Schedule CRUD is admin-gated (`/api/schedules`, `/api/schedule/{id}`); deleting
  a schedule still referenced by a camera is refused (409).
- The embedded engine (live MSE, RTSP relay, recording) is always active for
  any camera with a `source` URL — there is no mode where it's off.
  `[media]` only toggles **recording** (default off) and tunes its
  paths/timing/retention. With recording on for a camera, its recordings
  endpoints (list/get/timeline/gaps, HLS VOD under `/recordings/hls/*`)
  serve from the in-process segment index; with it off, those answer 404
  but `/live/{info,stream}` and the RTSP relay keep working regardless.
  The raw camera `source` is never returned by the API either way. Full
  endpoint list, payload shapes and client integration notes are in
  [`doc/MEDIA.md`](doc/MEDIA.md).
- **Two-way audio (push-to-talk).** `GET /api/camera/{id}/talk` upgrades to a
  WebSocket that relays client mic audio to the camera's ONVIF backchannel
  (see `internal/backchannel`). The backchannel URL is **optional**: when the
  camera defines a `backchannel` RTSP URL that URL is used; otherwise the
  camera's `source` URL itself is used, provided the startup/create-time probe
  (`seedTalkCodecsFor` → `backchannel.ProbeCodecs`) found a send-capable audio
  track on it — on thingino/prudynt the ONVIF backchannel lives on the same
  RTSP endpoint as the video, so the second URL is almost never needed.
  `Capabilities.Talk` is advertised when the config is explicit **or** the
  probe succeeded; the probe result also populates `Capabilities.TalkCodecs`
  (e.g. `["aac","opus","g711"]` on a thingino track0 — Opus is passthrough,
  see above). `App.backchannelURL(c)` resolves the effective URL
  (explicit config wins over probed source); the URL must reach the camera
  directly. Auth (validated **before**
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
  restrict it to `protocols h1 h2` (documented in `doc/example/Caddyfile` and
  `doc/TALK.md#deployment-gotcha-websocket-over-http3`).

## Adding an API endpoint
- Register the route in `server.go`'s `Handler()` with a method+pattern
  (`mux.HandleFunc("GET /api/...", a.handleX)`); read wildcards with
  `r.PathValue("...")`.
- Gate auth at the top of the handler: `a.requireUser(w, r)` (Basic or Bearer)
  or `a.requireAdmin(w, r)`; both write the 401/403 and return nil on failure.
- Respond with `writeJSON(w, status, v)` and `httpError(w, status, detail)`
  (the standard `{"detail": "..."}` error shape).
- For camera responses, marshal `camera.Camera` (credentials are already
  excluded) and apply `WithEngineURLs` (the live `engine` is set on `App`
  via `SetMediaEngine` when `[media]` is configured) so URLs reflect the
  embedded engine's stream fields and the rotating relay credentials.

## Adding a new camera
The easiest way is the web wizard: **user menu → Manage cameras → Add camera**
(admin only). It walks through basics, RTSP source (with a "test connection"
probe — which also discovers whether the same URL carries the ONVIF
two-way-audio backchannel and with which codecs, prefilling the backchannel
field under an "Advanced" collapsible), media options, and the optional
Thingino section, then creates the camera live via `POST /api/cameras` — no
restart. Delete is a button on the same screen
(`DELETE /api/camera/{id}`), which stops the pipeline and removes the row;
recorded footage on disk is left for retention to prune.

The INI files are now only the **initial seed** (imported once into the DB when
the `cameras` table is empty), so hand-editing them is a first-run/bootstrap
path, not the normal way to manage cameras. To seed via INI on a fresh install:

1. Drop a new `<id>.ini` under `data/cameras.d/` (or `/etc/eneverre/cameras.d/`
   in production) **before first start**. Use `doc/example/cameras.d/camera01.ini`
   (PTZ Thingino) and `doc/example/cameras.d/camera02.ini` (fixed) as templates —
   every key is documented in `doc/example/README.md`. The file's `id` is the
   path the embedded engine records/relays under; the same id was the path the
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
4. Start the API. The INI is imported into the `cameras` table on first run
   (when the table is empty); after that, manage cameras through the API/wizard
   — later INI edits are ignored.

## Frontend notes
- Single static page in `go/static/`, embedded in the binary. No build step.
- `ENEVERRE_STATIC_DIR=go/static` takes precedence over the embedded copy —
  handy for live edits without rebuilding. It is the only override; there is
  no cwd-relative autodetection, so a released binary always serves the assets
  embedded in it.
- The Bearer token lives in `localStorage`.
- **Forced password change** (`js/views/force-password.js`): when the stored
  user carries `must_change_password`, the app is gated behind a mandatory
  change-password screen (no cancel). `login.js` routes to it after a flagged
  login (prefilling the just-typed current password), and `app-shell.js`
  `showApp()` re-gates on reload while the flag is still set. A successful
  `PUT /api/users/me/password` clears the flag server-side and in the stored
  user, then `showApp()` proceeds.
- **Live view** (`js/views/wall.js` + `js/views/mse.js`): the wall plays
  `camera.live_mse` (the embedded engine's MSE feed at
  `/api/camera/{id}/live/stream`, ~1-2s latency). There is no live fallback
  anymore — the old `camera.hls` path went away with the external streamer,
  so a camera without `live_mse` (own `mse = false`, global `[media] mse =
  false`, or an unsupported codec) renders a "no live stream" placeholder,
  and a camera in privacy renders its own placeholder rather than hammering
  a dead endpoint. hls.js is still vendored, but only for VOD playback.
- **PTZ** (`js/views/ptz.js`): all pan/tilt is in degrees (`STEP_DEG = 10` per
  arrow tap), matching the server's degrees-only API — no firmware step
  values ever reach the client. Double-clicking a live wall tile for a
  PTZ-capable camera (`js/views/wall.js`) centers the view on the clicked
  point: `videoClickToPanTilt` maps the click's viewport coordinates to a
  relative pan/tilt using the camera's lens FOV (`cam.ptz.fov_h`/`fov_v`,
  server-supplied) and the `<video>`'s actual displayed rect (accounting for
  `object-fit: contain` letterboxing), then `centerOnVideoPoint` posts it to
  `/ptz/move`. A single click still zooms the wall filter to that camera; it's
  delayed 250ms on PTZ tiles so a following second click (the first half of a
  dblclick) can cancel it before the zoom fires. The zoom is one-way — only
  Escape walks the filter back out (camera → its location → all), so a click
  never has a history-dependent destination.
- **Admin status view** (`js/views/status.js`): a `GET /api/status` overlay
  (version, uptime, per-camera connected/recording/privacy, totals, and —
  when recording is enabled — storage headroom) plus a persistent low-disk
  banner that polls the same endpoint every 30s and shows/hides based on
  `storage.low_space` (the engine's disk monitor crossing `[media]
  min_free_bytes`). Camera-side live settings are deliberately NOT part of
  this snapshot — they live on the per-camera GET /api/camera/{id}/settings
  (see the camera-settings dialog note). Admin-only; the overlay itself
  auto-refreshes every 10s while open.
- **Camera settings dialog** (`js/views/camera-settings.js`): the topbar
  "sliders" button (next to talk/privacy) opens the live-settings dialog for
  the camera selected in live view — a day/night mode selector (Auto / Day /
  Night / Manual — manual disables auto while keeping the sensor's current
  mode) above sectioned toggles: Illumination (IR cut, IR850, IR940, white
  light — visible only while the selector sits on Manual, right under the
  selector since it is the mode that reveals it), then Audio (mic, speaker)
  and Motion, backed by `PUT /api/camera/{id}/settings`, with the
  state read from the per-camera `GET /api/camera/{id}/settings` (the cached
  heartbeat snapshot). Admin-gated (both endpoints are admin-only, so the
  button never shows for non-admins). The dialog repaints after every change
  with camera-confirmed values; the selector position is remembered
  client-side (`uiMode`) because the heartbeat can't tell manual apart from
  a fixed mode. It closes on backdrop click, the close button, or when the
  selection changes away from a settings-capable camera.
- **HLS VOD playback** (`js/views/playback.js`): the timeline plays
  `/api/camera/{id}/recordings/hls/playlist.m3u8` via hls.js
  (CMAF; `EXT-X-DISCONTINUITY` at coverage gaps), one instance per camera
  tile. The cursor advances from wall-clock — scaled by the selected
  playback speed (`vodCursorMsec` = anchor + elapsed × `pbSpeed`), so it
  tracks the frame on screen at 0.5x/1.5x/2x — and stays monotonic across
  gaps. A speed change re-anchors first (`reanchorVodCursor`) so the new
  rate applies from that moment instead of retroactively re-timing what
  was already played. Per-tile "No recording" overlays appear when the
  cursor sits inside a coverage gap; the tile's HLS instance is
  reinitialized at the cursor's current wall-clock when it exits the gap.
  Scrubbing resets everything. The play/pause control is painted only by
  `setPlayButtonState`, from `vodPaused` (never from a tile's
  `video.paused` — a tile paused inside a gap would misreport the state).
  Auth: every playlist/init/segment request needs the Bearer
  token (use hls.js `xhrSetup`).
- The playback timeline (`timeline.js`) draws recordings as the background
  bar and motion events from `GET /api/camera/{id}/events` as red Major1
  markers (`fetchEvents` in `js/views/playback.js`). Clicking a marker seeks
  playback to the event's start. Both are fetched per camera for the last
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
- **i18n** (`js/i18n.js` + `js/i18n/{en,es}.js`): flat `key -> string`
  dictionaries, one module per language, imported statically so everything
  resolves at load and `applyI18n()` can run synchronously at boot. Two ways
  to reach a string: `t("key", vars)` from JS (`{name}`-style holes filled
  from `vars`), or a `data-i18n*` attribute in `index.html` —
  `data-i18n` (textContent), `-html`, `-placeholder`, `-title`,
  `-aria-label`. A missing key falls back to English and then to the key
  itself, so a gap shows up as visible mojibake rather than a blank label.
  **Adding a string means adding the key to every `js/i18n/*.js`** — there is
  no build step to catch a missing one. `setLang()` persists the choice
  (`util/storage.js` `LANG_KEY`) and re-runs the static pass in place;
  dynamic views pick it up on their next render, so a view that caches
  rendered text must re-read `t()` rather than hold the string.
  `applyDocumentLang()` keeps `<html lang>` on the active catalog — at boot
  (`index.html` can only ship the static `en`, so an auto-detected or
  restored `es` would otherwise be announced as English) and on every
  switch.
- **Icons** (`js/ui/icons.js`): a small set of inline Lucide/Feather-style
  SVG icons (24×24, 2px stroke, `currentColor` for theming). `icon(name)`
  returns an SVG string — drop it into a template literal or assign with
  `el.innerHTML = icon("mic")` to swap a glyph. Static buttons in
  `index.html` carry a `data-icon="name"` attribute instead of inline SVG;
  `hydrateIcons()` (called first at boot in `app.js`) fills them from the
  same `PATHS` table, so path data lives in exactly one place. It *prepends*
  the SVG, so buttons with a text label (e.g. "← Back to cameras") keep
  their text. Dynamic state (play↔pause, audio on/off, theme sun/moon,
  privacy lock/lock-open, talk idle/armed, clip states) is swapped in JS
  with `el.innerHTML = icon(...)`. The loader glyph is the radial spokes
  icon, animated by `.wall-buffering-icon` for the live-tile spinner (the
  talk "connecting" spinner is a separate CSS ring, so that button is left
  empty). Adding a new icon: append an entry to the `PATHS` table in
  `icons.js`; the helper applies the standard stroke and `aria-hidden` so
  screen readers skip the icon (the surrounding button's `title` /
  `aria-label` carries the meaning).
