# Eneverre API — Go port

A Go rewrite of the Eneverre NVR API (originally Python/FastAPI). It is
drop-in compatible with the existing `data/eneverre.ini`, `data/cameras.d/`
and `data/eneverre.db` — including the existing Werkzeug password hashes
(both `scrypt` and `pbkdf2`).

## Why Go

- Single static binary, no virtualenv / Python runtime on the target host.
- Pure-Go SQLite (`modernc.org/sqlite`) — no CGO, cross-compiles cleanly.
- Good fit for the proxy/gateway workload (PTZ, thumbnails, playback streaming).
- The embedded media engine (`[media]`) adds in-process recording, RTSP
  relay and browser live without dragging in ffmpeg or a sidecar streamer —
  the same Go binary does it all.

## Build & run

```bash
cd go
go build -o eneverre .
# run from the project root so ./data/* resolves like the Python app
cd ..
./go/eneverre
```

Config resolution matches the Python app (`app/config.py`):

| What        | Search order                                      | Env override            |
|-------------|---------------------------------------------------|-------------------------|
| Config file | `/etc/eneverre/eneverre.ini`, `./data/eneverre.ini` | `ENEVERRE_CONFIG_PATH`  |
| Cameras dir | `/etc/eneverre/cameras.d`, `./data/cameras.d`     | `ENEVERRE_CAMERAS_DIR`  |
| Database    | `/var/run/eneverre/eneverre.db`, `./data/eneverre.db` | `ENEVERRE_DB_PATH`  |
| Static UI   | `./app/static`, `../app/static`, then embedded    | `ENEVERRE_STATIC_DIR`   |
| Log level   | `[server] log_level` (default `info`)             | `ENEVERRE_LOG_LEVEL`    |

### Logging & debugging

Structured logs via `slog` (text handler on stderr). Level is `debug` /
`info` / `warn` / `error`, set by `ENEVERRE_LOG_LEVEL` (precedence) or
`[server] log_level`.

- **Access log** — one INFO line per request: `method`, `path`, `status`,
  `dur_ms`, `ip` (honors `X-Forwarded-For`/`X-Real-IP` behind Caddy). At
  `debug` it adds `query` and response `bytes`.
- **MediaMTX authorizations** (`POST /api/auth`) — every probe is logged with
  `user`, `action`, `path`, `protocol`, `ip`, `authorized` (never the
  password). Denials log at **WARN** (always visible); grants at **DEBUG**.
  So by default you see failures with full context, and
  `ENEVERRE_LOG_LEVEL=debug` traces every successful authorization too —
  useful when debugging why MediaMTX accepts/rejects a stream.

```
level=DEBUG msg="mediamtx auth" user=mtxuser action=read path=calle protocol=rtsp ip=190.1.2.3 authorized=true
level=WARN  msg="mediamtx auth denied" user=mtxuser action=read path=interior protocol=webrtc ip=190.1.2.3 authorized=false
level=INFO  msg=request method=POST path=/api/auth status=200 dur_ms=0 ip=127.0.0.1
```

### Credential rotation (MediaMTX mode AND embedded engine)

The same rotating credential pair guards the embedded RTSP relay and the
external-MediaMTX auth probe, so live streams are valid across rotations:

```ini
[media]
; ...or [mediamtx] when you proxy through an external MediaMTX
rtsp_address = :8554
rotate_hours = 24           ; 0 or negative disables rotation
```

Set the interval with `rotate_hours` (default `24`; `0` or negative
disables). On rotation the previous pair stays valid for one interval (a
grace window) so a client that already holds an old RTSP/HLS URL is not
dropped the instant credentials rotate — it picks up the new URL on its
next `/api/cameras` call. The current pair is persisted in the single-row
`mediamtx_credentials` table of the SQLite DB so a restart keeps the last
credentials; the live pair is cached in memory, so the per-request path
never queries the DB. The embedded RTSP relay validates against both the
current and the grace pair (via `mediamtx.Store.Pairs`), so a stream started
just before rotation is not dropped the moment the pair rolls.

The web UI is embedded into the binary (`go:embed`) from `go/static/`, so the
single file runs standalone. Edit the UI there and rebuild. For live edits
without a rebuild, point `ENEVERRE_STATIC_DIR` at a directory on disk — it
takes precedence over the embedded copy (and is served uncached so changes
show up on refresh).

Embedded assets are served with a content-hash `ETag` and `Cache-Control:
no-cache`, so repeat loads revalidate with `If-None-Match` and get a `304`
instead of re-downloading (~550 KB of JS/CSS). Text assets are also served
gzip-compressed when the client accepts it (e.g. `hls.min.js` 414 KB → ~125
KB). The ETag is content-based, so a redeploy with changed assets invalidates
the cache automatically.

Admin seeding: when the users table is empty, an `admin` user is created with
a random password logged once at `WARN` (`ENEVERRE_ADMIN_USER` /
`ENEVERRE_ADMIN_PASS` override the username / password when set). No credential
is read from a config file — user management lives entirely in the DB. The
listen address comes from
`[server] host`/`port` (the Python `__main__` hardcoded `0.0.0.0:8080`; this
port honors the config, defaulting to the same values). The server runs with
explicit HTTP timeouts (`ReadHeaderTimeout` 5s, `ReadTimeout` 15s,
`WriteTimeout` 30s, `IdleTimeout` 60s) so a slow/idle client cannot hold a
connection open indefinitely; `WriteTimeout` is generous because the thumbnail
and playback handlers proxy upstream responses. SIGINT/SIGTERM trigger a
graceful `srv.Shutdown` (10s) followed by the embedded engine's
`Close()` — which finalizes and indexes every in-progress fMP4 segment so a
clean stop doesn't drop the recording since the last segment rotation.

```bash
go test ./...   # password-hash compatibility + server tests
go vet ./...
```

## Layout

```
main.go                       server bootstrap (was app/main.py)
internal/config               INI loading + path resolution (app/config.py)
internal/store                SQLite open + schema/migrations + admin seed (app/db.py, app/db_init.py)
internal/auth                 Werkzeug-compatible hashing + Basic/Bearer auth (app/auth.py)
internal/camera               Camera model + INI loader (app/models/camera.py, services/camera_service.py)
internal/mediamtx             credential store + stream URL builders (services/mediamtx_service.py)
internal/thingino             PTZ move + JPEG snapshot HTTP calls (services/thingino_service.py)
internal/events               event model + record/list/get/delete (models/event.py, services/events_service.py)
internal/updates              Android auto-update sidecar store
internal/backchannel          ONVIF Profile T backchannel + G.711/RTP (push-to-talk)
internal/media/               embedded media engine (active when [media] is configured)
  engine.go                   orchestrator: recorder + RTSP relay + live MSE + retention per camera
  recorder/                   per-camera gortsplib client, fMP4 segments, media watchdog
  recstore/                   record_path template -> on-disk path; common root for retention
  index/                      SQLite segment index (range, timeline, gaps, batched delete)
  liverelay/                  raw RTP passthrough served over RTSP on [media] rtsp_address
  live/                       chunked-HTTP fMP4 broadcaster (MSE feed for browsers)
  mtxi/                       MediaMTX-compatible mtxi box writer (gapless concat on playback)
  playback/                   VOD muxer: /get with gap fill + HLS VOD playlist
  retention/                  periodic cleaner (batched delete + dir prune)
internal/server               HTTP routes + handlers (app/routers/*)
  server.go                   App + mux + handler registry + deprecatedAlias
  handlers_auth.go            login/logout/refresh, device login, MediaMTX auth probe
  handlers_events.go          webhook + list/delete events
  handlers_live.go            live/info + live/stream (embedded engine, MSE fMP4)
  handlers_playback.go        recordings list/get/timeline/gaps + HLS VOD
  handlers_users.go           self + admin user CRUD, sessions
  handlers_updates.go         Android auto-update publish + download
```

## Endpoint parity

All REST endpoints from `app/routers/` are ported and exercised:
health, login/logout/refresh, cameras, ptz (move/home/recalibrate), privacy
(lens blackout), thumbnail, playback (list + streaming get with redirect-follow
and `X-Next-Available`), MediaMTX auth probe, the device-login flow, events
(webhook + list + delete), and the full users CRUD (self + admin routes, with
`me` taking precedence over `{username}`). PTZ home/recalibrate and privacy are
Go-side additions beyond the original Python surface.

The embedded media engine (`[media]`) adds a separate surface of its own,
mounted under `/api/camera/{id}/`:

- `live/{info,stream}` — MSE fMP4 live feed (browser).
- `recordings/{list,get,timeline,gaps,hls/*}` — VOD from the in-process
  segment index. `list`/`get` are also the canonical name of the
  `playback/{list,get}` endpoints that the MediaMTX proxy used to expose.
- `GET /api/recordings/paths` — camera ids that have recordings.

The legacy `playback/{list,get}` and `cameras/{id}/events` (plural) paths
are kept as deprecated aliases (RFC 8594 `Deprecation: true` + `Warning`
header) so existing clients keep working while they migrate. New clients
should hit the canonical routes. Full endpoint list, payload shapes, client
integration notes and the codec/coverage-gap semantics are in
[`doc/MEDIA.md`](../doc/MEDIA.md).

Behavioral details preserved: Thingino credentials stripped from camera
responses, INI keys are case-insensitive (`home_Y` → `home_y`), webhook
accepts arbitrary bodies and records a `webhook:raw (...)` source on parse
failure, timestamps accept unix-or-RFC3339 and serialize as RFC3339 UTC,
unreachable upstreams surface as `502`.

`POST /api/auth/login` additionally accepts an optional `device_name` string in
the JSON body. When set it is recorded on the issued token (the same field the
device-login flow populates), so `GET /api/users/me/sessions` shows a label per
session; when omitted the field is NULL — older clients keep working unchanged.

**Access + refresh tokens.** Password login returns a short-lived `token`
(access) and a long-lived `refresh_token`, both stored on one `tokens` row.
Clients renew with `POST /api/auth/refresh` (`{"refresh_token": "..."}`), which
rotates both secrets and slides both expiries **in place on the same row** — so
the session count tracks logins, not refreshes. Device-login (TV) sessions get
only an access token (`refresh_token` NULL) and so cannot refresh: they re-pair
when the access token lapses. A session is shown as alive in
`/api/users/me/sessions` while its refresh token is valid (`renewable: true`).

Both lifetimes are configurable, with precedence **CLI flag > env > `[auth]`
section > default**:

```ini
[auth]
access_token_ttl_hours = 24   ; access (Bearer) token life — also the TV session life
refresh_token_ttl_days = 90   ; refresh-token life; slid forward on every refresh
```

| Setting    | Flag                        | Env var                           | `[auth]` key             | Default |
|------------|-----------------------------|-----------------------------------|--------------------------|---------|
| Access TTL | `--access-token-ttl-hours`  | `ENEVERRE_ACCESS_TOKEN_TTL_HOURS` | `access_token_ttl_hours` | 24h     |
| Refresh TTL| `--refresh-token-ttl-days`  | `ENEVERRE_REFRESH_TOKEN_TTL_DAYS` | `refresh_token_ttl_days` | 90d     |

Note: clients must implement the refresh loop; until they do, the access-token
TTL is effectively the session length, so set `access_token_ttl_hours`
accordingly (e.g. higher) during the rollout.

## Out of scope

The previous Python implementation has been removed; this Go service is the
whole API. A few peripheral pieces were intentionally left out:

- **ONVIF watcher** and the **CLI tools** (user/MediaMTX-config management) —
  out of scope by request. The motion-event ingestion still works: any
  ONVIF/motion source can POST to the events webhook (`POST
  /api/camera/{id}/events`), which needs no shared code.
- **Auto-generated OpenAPI/Swagger** — FastAPI served these from the running
  app; the Go service does not. A hand-maintained spec lives at
  [`doc/openapi.yaml`](../doc/openapi.yaml) instead — update it when routes
  change.
