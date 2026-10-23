# Eneverre API

The Eneverre NVR API: a single static Go binary. It reads the existing
`data/eneverre.ini`, `data/cameras.d/` and `data/eneverre.db` — including
password hashes in the Werkzeug format (both `scrypt` and `pbkdf2`).

## Why Go

- Single static binary — no interpreter or virtualenv on the target host.
- Pure-Go SQLite (`modernc.org/sqlite`) — no CGO, cross-compiles cleanly.
- Good fit for the proxy/gateway workload (PTZ, thumbnails, playback streaming).
- The embedded media engine (`[media]`) adds in-process recording, RTSP
  relay and browser live without dragging in ffmpeg or a sidecar streamer —
  the same Go binary does it all.

## Build & run

```bash
cd go
go build -o eneverre .
# run from the project root so ./data/* resolves as expected
cd ..
./go/eneverre
```

Run `./go/eneverre --help` for the full flag list (path flags override their
`ENEVERRE_*` env vars, which override the defaults). Two operational flags worth
calling out:

- **`--reindex`** — rebuild the recording index from the segments on disk before
  serving. Use it once to recover from a lost or corrupt `index.db`; it keeps
  existing rows and rebuilds only what is missing (see
  [`doc/MEDIA.md`](../doc/MEDIA.md) for the recovery model, including the
  automatic on-startup recovery that runs without this flag).
- **`--no-cache`** — serve the bundled UI assets with `Cache-Control: no-store`
  so every load re-downloads them; handy while editing the embedded UI.

Config resolution:

| What        | Search order                                          | Env override           |
|-------------|-------------------------------------------------------|------------------------|
| Config file | `/etc/eneverre/eneverre.ini`, `./data/eneverre.ini`   | `ENEVERRE_CONFIG_PATH` |
| Cameras dir | `/etc/eneverre/cameras.d`, `./data/cameras.d`         | `ENEVERRE_CAMERAS_DIR` |
| Database    | `/var/run/eneverre/eneverre.db`, `./data/eneverre.db` | `ENEVERRE_DB_PATH`     |
| Static UI   | embedded in the binary                                | `ENEVERRE_STATIC_DIR`  |
| Log level   | `[server] log_level` (default `info`)                 | `ENEVERRE_LOG_LEVEL`   |

`--data-dir` / `ENEVERRE_DATA_DIR` short-circuits the first three rows: each
one defaults to `<dir>/eneverre.ini`, `<dir>/cameras.d`, `<dir>/eneverre.db`
instead of running the `./data`-anchored search. A per-path override still
wins over it.

The config file is optional: when the search finds none, Eneverre starts on
built-in defaults (`Config.FileLoaded` is false). An **explicit** path
(`--config` / `ENEVERRE_CONFIG_PATH`) that doesn't exist is fatal, and a file
that exists but fails to parse is fatal too. The full key-by-key reference is
[`doc/example/README.md`](../doc/example/README.md).

> **Cameras are DB-backed.** The `cameras.d/*.ini` files are only an *initial
> seed*, imported into the database once on first start (when no cameras exist
> yet). After that, add and remove cameras from the web UI (**user menu →
> Manage cameras**, admin only) or the API (`POST /api/cameras`,
> `DELETE /api/camera/{id}`); changes take effect immediately, no restart.
> Editing an INI file after the first run has no effect.

### Logging & debugging

Structured logs via `slog` (text handler on stderr). Level is `debug` /
`info` / `warn` / `error`, set by `ENEVERRE_LOG_LEVEL` (precedence) or
`[server] log_level`.

- **Access log** — one INFO line per request: `method`, `path`, `status`,
  `dur_ms`, `ip` (honors `X-Forwarded-For`/`X-Real-IP` behind Caddy). At
  `debug` it adds `query` and response `bytes`.
- **Engine diagnostics** — the media engine logs its own state changes
  (camera connect/disconnect/reconnect, segment rotation, retention
  pass, relay auth attempts). Watch the media/recorder/media prefix
  with `ENEVERRE_LOG_LEVEL=debug` to trace.

```
level=INFO msg="media/recorder[calle]: source connected" format=H264
level=INFO msg="request" method=GET path=/api/cameras status=200 dur_ms=1 ip=127.0.0.1
level=WARN msg="media/recorder[jardin]: camera codec not supported (recording/live disabled for it)" err="... stream offers: MJPEG"
```

### RTSP path tokens (embedded relay)

The embedded RTSP relay no longer uses a global username/password. Each
`GET /api/cameras` call mints a short-lived token per camera:

```
rtsp://<host>:<port>/<camera-id>/<exp>.<sig>
```

`sig` is HMAC-SHA256 over camera id + `rtsp` + expiry, using the server-only
`media_auth_key` (the same key as the HTTP media grants). Tokens last 6 h,
are scoped to one camera and are validated on DESCRIBE/SETUP only — the RTP
path stays a passthrough. The relay never logs the token. Plain RTSP over TCP
is deliberate: VLC does not support RTSPS, and GStreamer's RTSPS/SRTP MIKEY is
not negotiated by gortsplib. For remote clients use a VPN (WireGuard/Tailscale)
or a TLS tunnel (stunnel); the player still sees a local `rtsp://` URL, so no
certificate is configured in VLC/GStreamer/ExoPlayer.

The web UI is embedded into the binary (`go:embed`) from `go/static/`, so the
single file runs standalone. Edit the UI there and rebuild. For live edits
without a rebuild, point `ENEVERRE_STATIC_DIR` at a directory on disk
(`ENEVERRE_STATIC_DIR=go/static ./eneverre`) — it takes precedence over the
embedded copy and is served uncached, so changes show up on refresh. That env
var is the only override: the server never picks up a static dir from the
working directory, so a released binary always serves what was embedded in it.

Embedded assets are served with a content-hash `ETag` and `Cache-Control:
no-cache`, so repeat loads revalidate with `If-None-Match` and get a `304`
instead of re-downloading (~550 KB of JS/CSS). Text assets are also served
gzip-compressed when the client accepts it (e.g. `hls.min.js` 414 KB → ~125
KB, loaded on demand by the playback view). The ETag is content-based, so a redeploy with changed assets invalidates
the cache automatically.

Admin seeding: when the users table is empty, an `admin` user is created with
a random password logged once at `WARN` (`ENEVERRE_ADMIN_USER` /
`ENEVERRE_ADMIN_PASS` override the username / password when set). No credential
is read from a config file — user management lives entirely in the DB. The
seeded admin is flagged `must_change_password`: the server blocks every
protected route with `403 {"code":"password_change_required"}` until the
password is changed (only the change, logout and session-revoke endpoints are
reachable). Admins can require the same change when
creating a user or resetting a password; a self password change clears it. The
listen address comes from
`[server] host`/`port`, defaulting to `0.0.0.0:8080`. The server runs with
explicit HTTP timeouts (`ReadHeaderTimeout` 5s, `ReadTimeout` from
`[server] read_timeout` / `ENEVERRE_READ_TIMEOUT`, default 5m, `WriteTimeout`
30s, `IdleTimeout` 60s) so a slow/idle client cannot hold a connection open
indefinitely; `ReadTimeout` is generous because publishing a ~200 MiB APK over
a slow link legitimately takes minutes, and `WriteTimeout` because the
thumbnail and playback handlers proxy upstream responses (handlers that stream
for longer — live MSE, clip download, APK download — lift it per-request via
`clearWriteDeadline`). SIGINT/SIGTERM trigger a
graceful `srv.Shutdown` (10s) followed by the embedded engine's
`Close()` — which finalizes and indexes every in-progress fMP4 segment so a
clean stop doesn't drop the recording since the last segment rotation.

```bash
go test ./...   # password-hash + server tests
go vet ./...
```

## Layout

```
main.go                       flag parsing + server bootstrap
lifecycle.go                  graceful shutdown: drain in-flight requests (10s), then cleanup
embed.go                      go:embed of static/ (the web UI), with on-disk override
run_unix.go / run_windows.go  per-OS run loop: SIGINT/SIGTERM vs Service Control Manager,
                              and where log output goes (stderr vs ENEVERRE_LOG_FILE)
internal/config               INI loading + path resolution
internal/store                SQLite open + schema + admin seed
internal/auth                 Werkzeug-format hashing + Bearer auth
internal/servicetoken         machine tokens linked to a user (hashed at rest)
internal/camera               Camera model + DB store + one-time INI seed
internal/schedule             named recording programs (per-weekday armed windows) + DB store
internal/mediaauth            short-lived camera/surface-scoped HMAC media + RTSP grants
internal/thingino             PTZ move + JPEG snapshot HTTP calls
internal/events               event model + record/list/get/delete
internal/updates              Android auto-update store + per-track registry
internal/backchannel          ONVIF Profile T backchannel + G.711/RTP (push-to-talk)
internal/diskfree             shared statfs wrapper (Available/Total), unix + Windows
internal/timeutil             shared unix-or-RFC3339 timestamp parsing
internal/metrics              Prometheus + JSON instrumentation (/api/metrics{,/json})
internal/media/               embedded media engine (always built; [media] only tunes it)
  engine.go                   orchestrator: recorder + RTSP relay + live MSE + retention per camera
  probe.go                    one-shot RTSP probe (codec/resolution) behind POST /api/cameras/probe
  recorder/                   per-camera gortsplib client, fMP4 segments, media watchdog
  recstore/                   record_path template -> on-disk path; common root for retention
  index/                      SQLite segment index (range, timeline, gaps, batched delete)
  recovery/                   re-indexes segments left on disk by a hard crash (startup + --reindex)
  diskmonitor/                polls free space, fires OnLow/OnRecovered (low-disk emergency purge)
  liverelay/                  raw RTP passthrough served over RTSP on [media] rtsp_address
  live/                       chunked-HTTP fMP4 broadcaster (MSE feed for browsers)
  mtxi/                       MediaMTX-compatible mtxi box writer (gapless concat on playback)
  playback/                   VOD muxer: /get with gap fill + HLS VOD playlist
  retention/                  periodic cleaner (batched delete + dir prune) + PurgeToFree (low-disk)
internal/server               HTTP routes + handlers
  server.go                   App + mux + handler registry, GET /api/status, privacy, thumbnail
  handlers_auth.go            login/logout/refresh, device login
  handlers_cameras.go         camera list/CRUD/probe, PTZ move/home/recalibrate/position
  handlers_schedules.go       recording-program CRUD + per-camera assignment
  handlers_events.go          webhook + list/delete events
  handlers_live.go            live/info + live/stream (embedded engine, MSE fMP4)
  handlers_playback.go        recordings list/get/timeline/gaps + HLS VOD
  handlers_talk.go            push-to-talk WebSocket -> backchannel (see doc/TALK.md)
  handlers_users.go           self + admin user CRUD, sessions
  handlers_updates.go         Android auto-update publish + download
  logging.go                  access log + client-IP resolution (trusted_proxies)
  seclog.go                   auth-failure security log (fail2ban/CrowdSec format)
  ratelimit.go                failed-auth throttle, keyed per client IP and per username
  static.go                   embedded UI serving: ETag, gzip, Cache-Control
static/                       the web UI itself (js/views/*.js, one module per screen)
```

## Endpoints

The REST surface is implemented and exercised: health, `GET /api/status`
(admin-only operational snapshot: per-camera connected/recording/privacy,
totals, and — when recording is enabled — the recording volume's disk
headroom), login/logout/refresh, cameras (list + admin CRUD + probe), ptz
(move/home/recalibrate/position — pan/tilt in degrees, never firmware
steps), privacy (stops recording + transmission; lens blackout on Thingino),
thumbnail, the device-login flow, events (webhook + list + delete),
recording schedules (`/api/schedules` + `/api/schedule/{id}`, admin CRUD;
a camera references one by id and is armed 24/7 without one), and the full
users CRUD (self + admin routes, with `me` taking precedence over
`{username}`). An earlier external-MediaMTX proxy (`POST /api/auth` +
`playback/{list,get}` → MediaMTX control API) was removed when the embedded
engine replaced it.

The embedded media engine is now the only streaming mode (always built; the
optional `[media]` section only tunes it) and adds a separate surface of its
own, mounted under `/api/camera/{id}/`:

- `live/{info,stream}` — MSE fMP4 live feed (browser).
- `recordings/{list,get,timeline,gaps,hls/*}` — VOD from the in-process
  segment index.
- `GET /api/recordings/paths` — camera ids that have recordings.

`GET /api/camera/{id}/talk` is the push-to-talk WebSocket: it upgrades the
connection and pumps the client's audio into the camera's ONVIF Profile T
backchannel. Advertised when the camera defines a `backchannel` URL (an
advanced override) or the startup/create-time probe found the backchannel on
the camera's own `source` URL — the normal thingino case, no config needed.
Note it does not survive HTTP/3 behind Caddy — see
[`doc/TALK.md`](../doc/TALK.md).

`GET /api/metrics` (+ `/api/metrics/json`) exposes Prometheus instrumentation,
open to a local scraper over loopback and authenticated otherwise. Camera
metrics are aggregate counts with no per-camera `id` label. See
[`doc/MEDIA.md`](../doc/MEDIA.md#metrics).

Full endpoint list, payload shapes, client integration notes and the
codec/coverage-gap semantics are in
[`doc/MEDIA.md`](../doc/MEDIA.md).

Behavioral details worth noting: Thingino credentials stripped from camera
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
the session count tracks logins, not refreshes. The consumed token is kept in
`refresh_prev`: a replay within 30 s is a lost-response retry (401, session
kept); a later replay revokes the row as theft. Device-login (TV) sessions get
the same access + refresh pair, but the token carries the user's current role
(admin is clamped to ordinary `user`) and never grants administration; older
TVs that ignore the new refresh fields keep re-pairing, new ones renew like a
phone. A session is
shown as alive in `/api/users/me/sessions` while its refresh token is valid
(`renewable: true`). A user carrying `must_change_password` is confined
server-side to `PUT /api/users/me/password`, logout and session revoke/listing;
every other protected route answers `403 {"code":"password_change_required"}`.

**Roles and camera ACL.** `users.role` is `admin`, `user` (live, playback,
PTZ, privacy, talk), `viewer` (live and events, never playback) or `monitor`
(status and metrics only). `auth.CurrentUser.Can` maps roles to capabilities.
Every role is additionally narrowed by the per-user camera ACL
(`users.acl_restricted` + `camera_acl`; unrestricted by default, admin bypass),
managed through `GET/PUT /api/users/{username}/cameras`.

**Service tokens (machine integrations).** `POST /api/service-tokens` (human
admin) mints a token prefixed `envr_svc_` linked to an existing `username`; the
token inherits that user's role and camera ACL, and a role/ACL change applies
immediately. The raw token is returned once; only its SHA-256 hash is stored in
`service_tokens`. Each request refreshes `last_used_at` at most once a minute.
`GET /api/service-tokens` lists metadata, `DELETE /api/service-token/{id}`
revokes immediately. Account, token and ACL management stays human-admin-only;
a token cannot mint more tokens. HTTP Basic is no longer accepted anywhere —
Zabbix and other machine integrations use a service token (a `monitor` user
gives status/metrics but no camera list, live or playback).

HTTP media links for clients that cannot set an `Authorization` header (native
HLS players, `<img>`) use short-lived HMAC grants:
`?exp=<unix>&sig=<hex>`, scoped to one camera and one surface (`live`, `thumb`,
`hls`). The same key signs the RTSP path token
(`/<camera-id>/<exp>.<sig>`). The clip/list/timeline/gaps APIs accept a
principal whose role includes playback (admin/user), including a linked service
token; signed media grants are still rejected there. The relay pair is gone and is not accepted on HTTP media.

Both lifetimes are configurable, with precedence **CLI flag > env > `[auth]`
section > default**:

```ini
[auth]
access_token_ttl_hours = 24   ; access (Bearer) token life (also TV access)
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

This Go service is the whole API. A few peripheral pieces are intentionally
left out:

- **ONVIF watcher** and the **CLI tools** (user management) — out of
  scope by request. The motion-event ingestion still works: any
  ONVIF/motion source can POST to the events webhook (`POST
  /api/camera/{id}/events`), which needs no shared code.
- **Auto-generated OpenAPI/Swagger** — not served from the running app. A
  hand-maintained spec lives at [`doc/openapi.yaml`](../doc/openapi.yaml)
  instead — update it when routes change.
