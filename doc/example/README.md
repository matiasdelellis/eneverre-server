# Configuration

Everything Eneverre needs is in one file: **`eneverre.ini`**. It is resolved
from `/etc/eneverre/eneverre.ini` first, then `./data/eneverre.ini` (override
with `--config` / `ENEVERRE_CONFIG_PATH`, or relocate the whole bundle with
`--data-dir`). Section and key names are case-insensitive, and every key has a
built-in default.

**The file is optional.** When neither `/etc/eneverre/eneverre.ini` nor
`./data/eneverre.ini` exists, Eneverre starts with every setting at its
built-in default (the same as an empty file). It only fails to start if the
file *you pointed it at* is missing — i.e. an explicit `ENEVERRE_CONFIG_PATH`
or `--config` path that does not exist — or if a file that *does* exist fails
to parse. So a minimal install needs no `eneverre.ini` at all, but a typo in an
explicit path is caught instead of silently falling back to defaults. When it
starts without one, Eneverre logs the exact path it searched and where to copy
the example from, so making a setting permanent is a single `cp` away:

```
level=INFO msg="no config file found, using defaults" searched=data/eneverre.ini
level=INFO msg="to make configuration changes permanent, copy the example config to the searched path" example=/opt/eneverre/doc/example/eneverre.ini target=data/eneverre.ini
```

Cameras are **not** configured here: they live in the database and are managed
from the web UI / API. The `cameras.d/*.ini` files are only a one-time seed for
a fresh install — see [Preloading cameras](#preloading-cameras) at the end.

[`eneverre.ini`](eneverre.ini) in this folder is a fully commented template of
everything below.

| Section     | What it controls                                         | Needed? |
| ----------- | -------------------------------------------------------- | ------- |
| `[server]`  | Listen address, log level, metrics, CORS, proxies         | no      |
| `[auth]`    | Token lifetimes, session cleanup, security log            | no      |
| `[media]`   | The embedded media engine: live, relay, recording, retention | no   |
| `[events]`  | The motion webhook                                         | only to accept motion events |
| `[updates]` | The Android OTA update server                              | only to serve app updates |

---

## `[server]`

```ini
[server]
host = 0.0.0.0
port = 8080
; log_level = info        ; debug | info (default) | warn | error
; read_timeout = 5m       ; HTTP request-body read timeout (time.ParseDuration)
; metrics = true          ; expose /api/metrics + /api/metrics/json
; cors_origins =          ; comma-separated Origin allowlist; empty = permissive
; trusted_proxies =       ; peers whose X-Forwarded-For is honored; empty = loopback
```

 * **host / port:** Listen address. Default `0.0.0.0:8080`. Overridden by
   `--host` / `--port`.
 * **log_level:** `debug` | `info` (default) | `warn` | `error`. The access log
   is one INFO line per request; `warn` silences it. Overridden by
   `--log-level` / `ENEVERRE_LOG_LEVEL`.
 * **read_timeout:** Max time to read an HTTP request body (`time.ParseDuration`
   format). Default `5m` — enough to publish a ~200 MiB APK over a slow link.
   Precedence: this key > `ENEVERRE_READ_TIMEOUT` > `5m`.
 * **metrics:** Prometheus metrics at `/api/metrics` (+ `/api/metrics/json`). On
   by default; open to a loopback scraper and authenticated from anywhere else.
   Set `false` to drop the endpoints entirely. See [`doc/MEDIA.md`](../MEDIA.md#metrics).
 * **cors_origins:** Comma-separated browser CORS allowlist. Empty (default) is
   permissive — any Origin is reflected, which is safe with same-origin UI +
   Bearer-token auth. Set it to lock the browser surface to known front-ends; a
   single `*` entry keeps the permissive behavior explicitly.
 * **trusted_proxies:** Comma-separated IPs or CIDRs of reverse proxies whose
   `X-Forwarded-For` / `X-Real-IP` headers are honored when resolving the
   client IP for the access log and the [security log](../security-logging.md)
   (the IP fail2ban bans). Empty (default) trusts **loopback only**, which
   covers the same-host Caddy setup from this guide. A proxy on another host
   must be listed explicitly (e.g. `192.168.1.10` or `10.0.0.0/24`); use
   `none` when eneverre is exposed directly with no proxy at all. Peers not
   on the list get logged by their socket address, so a direct client cannot
   spoof the banned IP.

> **Admin user.** Eneverre does **not** read any username/password from this
> file — all user management lives in `data/eneverre.db`. The first time the
> users table is empty, an `admin` user is created with a **random password
> that is logged once** (`journalctl -u eneverre | grep 'generated password'`,
> or straight to the terminal when run in the foreground). To choose the
> password yourself, set `ENEVERRE_ADMIN_PASS` (and optionally
> `ENEVERRE_ADMIN_USER`) before the first start. Either way the seeded admin is
> flagged **must change password**: the web UI forces a new password on the
> first login before the app opens, so the bootstrap credential never becomes
> the permanent one. (The flag is UI-enforced; Basic-auth API calls such as
> `curl -u admin:...` are not blocked by it.) Manage further users through the
> `/api/users` endpoints or the web UI — where an admin can require the same
> forced change when creating a user or resetting a password.

## `[auth]`

Token lifetimes and session hygiene. Every key is optional.

```ini
[auth]
;access_token_ttl_hours   = 24   ; Bearer-token life (also the TV session life)
;refresh_token_ttl_days   = 90   ; password-login renewal window
;cleanup_interval_minutes = 60   ; background expired-token sweep; 0 disables
;cleanup_grace_hours      = 24   ; keep expired tokens visible this long
;security_log             =      ; dedicated auth-failure log for fail2ban
```

 * **access_token_ttl_hours:** Bearer-token lifetime. Default `24`. This is
   also the lifetime of a TV (device-login) session, which cannot be
   refreshed. Overridden by `--access-token-ttl-hours` /
   `ENEVERRE_ACCESS_TOKEN_TTL_HOURS`.
 * **refresh_token_ttl_days:** Renewal window for a password login, slid
   forward on every refresh. Default `90`. Overridden by
   `--refresh-token-ttl-days` / `ENEVERRE_REFRESH_TOKEN_TTL_DAYS`.
 * **cleanup_interval_minutes:** How often the background goroutine prunes
   expired token rows, so the DB doesn't accumulate dead sessions on a
   rarely-used installation. `0` (or negative) disables the ticker —
   login-time cleanup still runs. Precedence: this key >
   `ENEVERRE_TOKEN_CLEANUP_INTERVAL` > `60`.
 * **cleanup_grace_hours:** How long an expired token stays in the sessions
   list before the cleaner deletes it, so the frontend can show an "expired"
   section instead of having sessions vanish the moment they lapse. Default
   `24`; `0` deletes immediately. Precedence: this key >
   `ENEVERRE_TOKEN_CLEANUP_GRACE_HOURS` > `24`.
 * **security_log:** When set, authentication failures are also written here,
   one line per event, for fail2ban / CrowdSec to tail. Empty (default) means
   no dedicated file — the events still go to the main log at WARN. The path
   must be writable by the eneverre process and its directory is **not**
   created for you; rotate it with logrotate's `copytruncate`. Precedence:
   this key > `ENEVERRE_SECURITY_LOG` > `""`. See
   [`security-logging.md`](../security-logging.md).

## `[media]`

The embedded media engine. It is **always** built for cameras with a `source`
URL, and the **live MSE feed, RTSP relay and disk recording are all on by
default** — no `[media]` section needed, that is the point of the app. The
section only tunes the engine or opts out of one of its three switches.

With recording on, the engine also enforces `retain` (default `7d`). When no
`record_dir` is set it uses `/var/lib/eneverre/recordings` if that directory
exists (the systemd/FHS install) and otherwise `<data_dir>/recordings`, so a
self-contained `--data-dir` keeps recordings next to its config and DB.

Set `record = false` for **live-only mode** (live MSE + RTSP relay, no disk
write, `/recordings/*` answer 404) — useful when you only want the wall to work
and retention is handled elsewhere. To exclude just one camera, turn its own
`record` switch off from the web UI instead: the global and per-camera values
are ANDed, so a camera can opt out but never opt back in.

```ini
[media]
;mse              = true       ; global toggle for the live MSE browser feed
;relay            = true       ; global toggle for the RTSP relay
;record           = true       ; global toggle for disk recording
;record_dir       = /var/lib/eneverre/recordings
;record_path      = /var/lib/eneverre/recordings/%path/%Y-%m-%d/%H/%Y-%m-%d_%H-%M-%S-%f
;index_path       = /var/lib/eneverre/recordings/index.db  ; segment index DB
;cache_dir        = /var/lib/eneverre/cache                ; gap-fill frame cache
;segment_duration = 60s        ; min segment length
;part_duration    = 1s         ; fMP4 fragment length (crash recovery point)
;max_part_size    = 50M        ; safety cap on a single fMP4 fragment (RAM valve)
;retain           = 7d         ; 0 keeps forever; ParseDuration + "d" for days
;min_free_bytes   = 1G         ; force-purge oldest below this; 0 disables
;rtsp_address     = :8554      ; RTSP relay listen address
;rtsp_host        =            ; public host in the relay URL; empty = from request
;transport        = auto       ; auto | tcp | udp
;gap_message      = NO RECORDING  ; caption burned into gap-fill black frames
;rotate_hours     = 24         ; RTSP-relay credential rotation; 0 disables
```

**Switches** — three independent booleans, each `true` by default and each
overridable per camera:

 * **mse:** The live MSE (fMP4) browser feed. `false` drops the `live_mse` URL
   from `/api/cameras`.
 * **relay:** The RTSP relay entry. `false` drops the `rtsp` URL from
   `/api/cameras`.
 * **record:** Disk recording. `false` is live-only mode (see above).

**Paths:**

 * **record_dir:** Where segments are written. Default
   `/var/lib/eneverre/recordings` when it exists, else `<data_dir>/recordings`.
 * **record_path:** Segment path pattern; must contain `%path` and time
   specifiers including `%f`. Default
   `<record_dir>/%path/%Y-%m-%d/%H/%Y-%m-%d_%H-%M-%S-%f`, so a recording
   started at 10:30:45.123456 on 2025-01-15 for `camera01` lands at
   `<record_dir>/camera01/2025-01-15/10/2025-01-15_10-30-45-123456.mp4`. Each
   camera's day is its own subtree, which keeps the file count per directory
   small; the full timestamp keeps segments unique across day/hour boundaries.
   `retain` wipes old segments by index, not by path, so the layout is
   independent of the rotation policy.
 * **index_path:** The segment index DB. Default `<record_dir>/index.db`.
   Rebuild it with `--reindex` if it is lost or corrupt.
 * **cache_dir:** Generated assets — currently the black "NO RECORDING"
   gap-fill frames, one tiny `.h264` per resolution under `<cache_dir>/gapfill/`.
   Persisted so they survive restarts. Default `<record_dir>/../cache`; keep it
   under a writable path given the unit's `ProtectSystem=strict`.

**Segmenting and retention:**

 * **segment_duration:** Minimum segment length (`time.ParseDuration`).
   Default `60s`.
 * **part_duration:** fMP4 fragment length, i.e. the recovery-point objective —
   on a hard crash at most this much of the open segment is lost. Default `1s`.
 * **max_part_size:** Size cap that forces a fragment out early; a safety valve
   against RAM growth when a keyframe interval is huge. Suffixes `K`/`M`/`G`
   (base 1024). Default `50M`.
 * **retain:** Delete recordings older than this. Default `7d`; `0` keeps
   forever. Accepts any `time.ParseDuration` value plus a `d` suffix for days
   (`10d` = 240h). Motion events are pruned on this **same** window (see
   `[events]`).
 * **min_free_bytes:** Low-water mark on the recording volume. When free space
   drops below it a WARN is logged and an emergency purge force-removes the
   oldest segments — ignoring `retain` — until free space is back above twice
   the mark. **Recording is never paused:** the oldest footage is sacrificed to
   make room for the newest, so the disk never fills. `0` disables the check.
   Suffixes `K`/`M`/`G` (base 1024). Default `1G`. The current state is
   reported by `GET /api/status` under `storage.low_space` and
   `storage.low_space_since`.

**Relay and source:**

 * **rtsp_address:** Listen address of the RTSP relay that re-serves the live
   streams. Default `:8554`. It does **not** go through the reverse proxy —
   the port must be reachable by RTSP clients directly.
 * **rtsp_host:** Public hostname clients use to reach the relay, embedded in
   the `rtsp` URL of `/api/cameras`
   (`rtsp://<user>:<pass>@<rtsp_host>:<port>/<id>`). Empty (default) takes the
   host from the request, which works out of the box on a LAN; set it to pin a
   public host when the API and relay hostnames differ. The web UI uses
   `live_mse` regardless.
 * **transport:** RTSP source transport: `auto` (default — UDP with TCP
   fallback), `tcp` (reliable, cleanest on lossy or distant links), or `udp`.
   Overridable per camera.
 * **rotate_hours:** The relay URL embeds a random 8/8 username/password that
   rotates on this interval, with the previous pair staying valid for one
   interval as a grace window. Default `24`; `0` (or negative) disables
   rotation. The credentials live in the SQLite DB, not in this file.
 * **gap_message:** Caption burned into the gap-fill black frames served for
   uncovered ranges. UTF-8 is fine; changing it regenerates the cached asset.
   Default `NO RECORDING`.

See [`doc/MEDIA.md`](../MEDIA.md) for the full endpoint list, client
integration notes, and the codec/coverage-gap semantics.

## `[events]`

The motion webhook. Without a `webhook_secret`, `POST /api/camera/{id}/events`
returns 503 — this section is the only thing that turns the feature on.

```ini
[events]
webhook_secret = change-me   ; required; sent as X-Webhook-Secret or ?token=
;pre_seconds  = 5            ; widen each event this many seconds before...
;post_seconds = 5            ; ...and after the trigger
```

 * **webhook_secret:** Shared secret the camera (or motion detector) sends in
   the `X-Webhook-Secret` header or the `?token=` query param. Required.
 * **pre_seconds / post_seconds:** Each recorded event is widened to
   `[start - pre_seconds, end + post_seconds]`. The defaults (`5` + `5`) make a
   single event cover a 10-second window centered on the trigger, which is what
   the apps' playback timeline expects. Raise them for sources that fire short
   bursts, or for a bigger post-roll.

There is no separate event-retention knob: motion events are pruned on the
**same** window as recordings (`[media] retain`). A background sweep (hourly,
plus once at startup) drops events older than the window so the events table
never outlives the footage its rows reference. With `retain = 0` events are
kept forever.

## `[updates]`

The auto-update server for the Android clients. **Off by default:** with
neither `storage_dir` nor `ENEVERRE_UPDATES_DIR` set, all `/api/app/*`
endpoints return 503.

```ini
[updates]
;storage_dir     = /var/lib/eneverre/app-updates
;public_base_url = https://updates.example.com   ; base URL in the APK manifest
;publish_token   = <32-byte-secret>              ; gate the publish endpoints
;max_build_size  = 100M                          ; hard cap on the upload body
```

 * **storage_dir:** Root under which each client *track* gets its own
   subdirectory holding `manifest.json` plus the uploaded APKs. Track names are
   arbitrary, operator/CI-chosen identifiers (`tv`, `phone`, `tablet`, …) —
   the server keeps no fixed list, publishing to a new name is enough to start
   serving it. Setting this (or `ENEVERRE_UPDATES_DIR`) is what enables the
   feature. Precedence: this key > `ENEVERRE_UPDATES_DIR` > `""` (disabled).
 * **public_base_url:** Base URL each `builds[i].url` in the manifest is rooted
   at (`<public_base_url>/api/app/updates/<track>/<filename>`). **Recommended
   in production:** without it the server auto-detects scheme and host from the
   request, honoring `X-Forwarded-Proto` / `X-Forwarded-Host` — headers a
   TLS-terminating proxy does not send by default (Caddy included). Setting it
   works behind any proxy with zero extra config. Env:
   `ENEVERRE_UPDATES_PUBLIC_BASE_URL`.
 * **publish_token:** Optional publish-only bearer token. When set,
   `POST /api/admin/app/updates/{track}` requires
   `Authorization: Bearer <token>` and rejects user/password and session
   tokens — only the dedicated token works, so it can be rotated without
   touching user accounts. Leave empty to keep the default (publish requires an
   admin user). Generate with `openssl rand -hex 32`. Precedence: this key >
   `ENEVERRE_UPDATES_PUBLISH_TOKEN`.
 * **max_build_size:** Hard cap on the publish request body (multipart + APK),
   enforced via `http.MaxBytesReader` so a 413 comes back as soon as the body
   crosses the limit. Suffixes `K`/`M`/`G` (base 1024). Default `100M` — current
   TV universals are ~50-70 MiB. Precedence: this key >
   `ENEVERRE_UPDATES_MAX_BUILD_SIZE` > `100M`.

No release history is kept: at every commit the previous release's APKs are
deleted, so disk usage is bounded by the current release. See
[`UPDATES.md`](../UPDATES.md) for the wire protocol, the multi-POST
`finalize=false` flow, and the client-side rules.

---

## Command-line flags

Everything above can also be pointed at from the command line; run
`eneverre --help` for the authoritative list. Path flags override their
`ENEVERRE_*` env vars, which override the built-in defaults.

 * **`--data-dir <dir>`** — shortcut that roots config, cameras and DB at
   `<dir>/eneverre.ini`, `<dir>/cameras.d`, `<dir>/eneverre.db` (e.g.
   `--data-dir ./data-quincho` to run a second test environment).
 * **`--config, -c <path>`** / **`--cameras-dir <dir>`** / **`--db <path>`** —
   point at each file/dir individually (env: `ENEVERRE_CONFIG_PATH`,
   `ENEVERRE_CAMERAS_DIR`, `ENEVERRE_DB_PATH`).
 * **`--host` / `--port`** — override `[server] host`/`port`.
 * **`--log-level <level>`** — `debug` | `info` | `warn` | `error`
   (env: `ENEVERRE_LOG_LEVEL`).
 * **`--no-cache`** — send `Cache-Control: no-store` on static UI assets, forcing
   a fresh download every load (handy while editing the bundled UI).
 * **`--reindex`** — rebuild the recording index from the segments on disk
   before serving, then start normally. Use it once to recover from a lost or
   corrupt `index.db`; it keeps existing rows and rebuilds only what is missing.
   See [`doc/MEDIA.md`](../MEDIA.md) for the recovery model.
 * **`--access-token-ttl-hours`** / **`--refresh-token-ttl-days`** — override the
   `[auth]` token lifetimes.
 * **`--version, -v`** / **`--help, -h`** — print version / usage and exit.

## Environment variables

Two precedence rules are in play, depending on whether the setting has a CLI
flag of its own:

 * **Paths, log level and the two token lifetimes** — the settings with a flag —
   resolve **CLI flag > env var > INI key > built-in default**.
 * **Everything else** (`read_timeout`, the `[auth]` cleanup keys,
   `security_log`, all `[updates]` keys) resolves **INI key > env var >
   built-in default**: the config file wins, and the env var is the fallback
   for containerized installs that ship no `eneverre.ini`.

| Variable | Equivalent | Notes |
| -------- | ---------- | ----- |
| `ENEVERRE_DATA_DIR` | `--data-dir` | relocates the whole bundle |
| `ENEVERRE_CONFIG_PATH` | `--config, -c` | a missing explicit path is fatal |
| `ENEVERRE_CAMERAS_DIR` | `--cameras-dir` | seed directory; may be absent |
| `ENEVERRE_DB_PATH` | `--db` | created if it doesn't exist |
| `ENEVERRE_LOG_LEVEL` | `--log-level` | beats `[server] log_level` |
| `ENEVERRE_STATIC_DIR` | — | serve the web UI from this directory instead of the embedded copy — development only (env-only, no flag) |
| `ENEVERRE_LOG_FILE` | — | **Windows only**, and only under the Service Control Manager (no console): append logs to this file. Set by the installer; ignored elsewhere. See [`WINDOWS.md`](../WINDOWS.md) |
| `ENEVERRE_ADMIN_USER` / `ENEVERRE_ADMIN_PASS` | — | seed the first admin; only used while the users table is empty |
| `ENEVERRE_ACCESS_TOKEN_TTL_HOURS` | `--access-token-ttl-hours` | |
| `ENEVERRE_REFRESH_TOKEN_TTL_DAYS` | `--refresh-token-ttl-days` | |
| `ENEVERRE_READ_TIMEOUT` | `[server] read_timeout` | |
| `ENEVERRE_TOKEN_CLEANUP_INTERVAL` | `[auth] cleanup_interval_minutes` | |
| `ENEVERRE_TOKEN_CLEANUP_GRACE_HOURS` | `[auth] cleanup_grace_hours` | |
| `ENEVERRE_SECURITY_LOG` | `[auth] security_log` | |
| `ENEVERRE_UPDATES_DIR` | `[updates] storage_dir` | |
| `ENEVERRE_UPDATES_PUBLIC_BASE_URL` | `[updates] public_base_url` | |
| `ENEVERRE_UPDATES_PUBLISH_TOKEN` | `[updates] publish_token` | |
| `ENEVERRE_UPDATES_MAX_BUILD_SIZE` | `[updates] max_build_size` | |

## Running as a systemd service

[`eneverre.service`](eneverre.service) is a ready-to-use unit. It runs the
binary as an isolated transient user (`DynamicUser=yes`), reads config from
`/etc/eneverre/`, and keeps its state — a single SQLite DB, which also holds the
rotating stream-auth credentials — in `/var/lib/eneverre/` (created
automatically via `StateDirectory=`).

```bash
# Binary + config
sudo install -m0755 eneverre /usr/local/bin/eneverre
sudo install -d /etc/eneverre
sudo install -m0644 doc/example/eneverre.ini /etc/eneverre/eneverre.ini

# Unit file
sudo install -m0644 doc/example/eneverre.service /etc/systemd/system/eneverre.service
sudo systemctl daemon-reload
sudo systemctl enable --now eneverre

# Watch it
systemctl status eneverre
journalctl -u eneverre -f
```

On its first start the service creates the admin user with a random password
and logs it once — read it with `journalctl -u eneverre | grep 'generated
password'`. The seeded admin is flagged **must change password**, so the first
web login walks you through setting a new one before the app opens (the
bootstrap password is only ever meant to get you in once). To set a known
password instead, add `ENEVERRE_ADMIN_PASS` (and optionally
`ENEVERRE_ADMIN_USER`) via a drop-in (`systemctl edit eneverre`) before the
first start — that admin is still prompted to change it on first login. Notes:

 * **Listen port.** The default is `8080`. To bind a privileged port (< 1024)
   add `AmbientCapabilities=CAP_NET_BIND_SERVICE` and
   `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`; the example otherwise drops all
   capabilities. The common setup keeps Eneverre on `8080` behind the example
   [`Caddyfile`](Caddyfile) for TLS.
 * **Recording paths.** Recordings default to `/var/lib/eneverre/recordings`,
   which is under `StateDirectory=` and therefore writable despite
   `ProtectSystem=strict`. If you point `[media] record_dir` (or `cache_dir` /
   `index_path`) at a path **outside** `/var/lib/eneverre`, add it to
   `ReadWritePaths=` or the writes will fail.
 * **Config permissions.** `DynamicUser=yes` means the config must stay readable
   by the transient user (mode `0644`). If `eneverre.ini` holds secrets on a
   multi-user host, switch to a dedicated `eneverre` account instead — the unit
   file's header comment shows how.
 * **Override without editing the unit.** Use a drop-in:
   `sudo systemctl edit eneverre` (e.g. to change the port, log level, or admin
   env vars).

---

## Preloading cameras

Cameras live in `eneverre.db` and are managed from the web UI (or the
`/api/cameras` endpoints). You never *have* to write a camera file — add your
first camera from the UI and you are done.

The `cameras.d/*.ini` files exist for one thing: **preloading a fresh install**.
They are imported into the database **once**, when the camera table is empty,
and ignored on every start after that. Editing a file later changes nothing;
edit the camera in the UI instead. This is what makes an unattended
provisioning run (image an SD card, drop in the INIs, boot) come up with the
cameras already there.

Each camera is one file under `/etc/eneverre/cameras.d/` (or
`./data/cameras.d/`, or `--cameras-dir <dir>`); the filename is arbitrary. A
file with no `[camera]` section or no `name` is skipped with a warning. The
directory need not exist — no directory simply means no seed.

```bash
sudo install -d /etc/eneverre/cameras.d
sudo cp doc/example/cameras.d/*.ini /etc/eneverre/cameras.d/
# ...then start Eneverre for the first time
```

Two complete examples ship here:
[`cameras.d/camera01.ini`](cameras.d/camera01.ini) (a PTZ Thingino camera) and
[`cameras.d/camera02.ini`](cameras.d/camera02.ini) (a fixed camera, no
Thingino).

```ini
[camera]
name = Outside
comment = Thingino 360 Camera
location = Exterior
source = rtsp://username:password@camera_url:port/path
width = 1920
height = 1080
; Optional: direct RTSP URL to the camera for two-way audio (ONVIF Profile T).
; Must point at the camera itself. Its presence enables the push-to-talk
; endpoint.
backchannel = rtsp://username:password@192.168.1.91:554/ch0
; Optional: the camera's own still-JPEG endpoint. Its presence enables the
; thumbnail capability for a non-Thingino camera; the server proxies it (no
; decode). Never exposed to clients.
snapshot_url = http://username:password@192.168.1.91/snapshot.jpg

; The [thingino] section is optional. Its presence (specifically a
; thingino_api_key) is what enables the thumbnail capability and the firmware
; lens blackout used by privacy; ptz = true enables the PTZ endpoints. Omit the
; whole section for a plain fixed camera (privacy still works — it just stops
; recording + transmission without a firmware blackout).
[thingino]
thingino_url = http://192.168.1.91
thingino_api_key = <api-key>
ptz = true
; Home / privacy positions in DEGREES (pan, tilt). The server converts to
; firmware x/y at move time using the calibration below. -1 in either axis
; disables the auto-move for that axis.
home_x = 180
home_y = 90
privacy_x = 0
privacy_y = 180
; PTZ calibration: total steps per axis and the angular range they cover, plus
; the horizontal lens FOV. Defaults are the typical thingino values (2130/360
; pan, 1600/180 tilt, 113° FOV) — only uncomment to override for your hardware.
; pan_steps = 2130
; pan_degrees = 360
; tilt_steps = 1600
; tilt_degrees = 180
; fov_h = 113
```

Recording **schedules** have no INI representation — named programs live only
in the DB — so a seeded camera always starts recording 24/7. Assign a schedule
from the UI afterwards.

### `[camera]` keys

 * **name:** The camera's display name, and its identity. The internal id —
   the path the embedded engine records/relays under, and the `{id}` in every
   API URL — is derived from it as a lowercased, accent-folded slug
   ("Outside" → "outside"); same-slug names are disambiguated with a numeric
   suffix. It is never set by hand and cannot change once assigned. Any `id`
   key in the file is ignored. `name` is required.
 * **comment / location:** Friendly labels shown by the clients.
 * **source:** The camera's direct RTSP URL. The engine always connects to
   it and relays/records from it — `[media]` only decides whether
   *recording* happens, not whether the engine talks to the camera. This
   URL is never returned by `/api/cameras`; clients get the relay
   `rtsp://…:8554/{id}` instead. Must point at the camera itself, since
   the engine speaks RTSP to it directly (not to a streamer in front of it).
 * **mse:** Per-camera opt-out of the live MSE (fMP4) browser feed. Default
    true. Set to `false` to skip the MSE broadcaster for this camera — it
    will not appear with a `live_mse` URL in `/api/cameras`. The RTSP relay
    and recording are unaffected. Gated independently of `relay`.
 * **relay:** Per-camera opt-out of the RTSP relay entry. Default true. Set
    to `false` to skip the RTSP relay for this camera — it will not appear
    with an `rtsp` URL in `/api/cameras`. The MSE feed and recording are
    unaffected. Gated independently of `mse`.
 * **record:** Per-camera opt-out of recording. Default true (cameras with
    a source are recorded). Set to `false` to keep the live MSE feed and
    the RTSP relay working for this camera but skip writing to disk — the
    `/recordings/*` endpoints for it answer 404. Useful for privacy-
    sensitive cameras you only want to watch live. It can only turn recording
    off, never on: `[media] record = false` still wins globally.
 * **privacy:** Per-camera opt-out of the privacy toggle. Default true: every
    camera offers a runtime privacy switch (`POST /api/camera/{id}/privacy`)
    that stops recording **and** transmission (live MSE + RTSP relay) by pausing
    the engine's pipeline for it — and, on Thingino cameras, drives the firmware
    lens blackout + PTZ privacy position. Set to `false` to mark an always-on
    camera that must never be paused (no privacy button, `capabilities.privacy`
    is false, the endpoint answers 404).
 * **transport:** Per-camera override of the global
   `[media] transport` for the source RTSP: `auto` (default), `tcp` (reliable,
   recommended for lossy/distant links), or `udp`. Useful to force TCP on a
   single camera without changing the global default.
 * **playback:** Per-camera opt-out of the recordings UI. Defaults to this
   camera's `record` value. Note it is not a hint the server takes at face
   value: the Live/Playback switch is advertised only when the camera
   *actually has recordings on disk*, so recording a camera makes playback
   appear by itself once the first segment is written. Set `playback = false`
   to keep recording while hiding playback in the UI; it never turns recording
   on (that's `record`, above).
 * **width / height:** Pixel dimensions, used to give the playback boxes the
   right aspect ratio (default 16×9).
 * **backchannel:** Optional direct RTSP URL (with credentials) to the camera's
   ONVIF Profile T two-way-audio backchannel. **Must point at the camera
   itself** so it is kept raw and never rewritten by URL helpers. Its
   presence enables the `talk` capability and the
   `GET /api/camera/{id}/talk` push-to-talk WebSocket. Never exposed in
   API responses. See [`TALK.md`](../TALK.md).
 * **snapshot_url:** Optional HTTP(S) URL of the camera's own still-JPEG
   endpoint (many non-Thingino cameras expose one, e.g. an ONVIF/CGI snapshot
   path). Its presence enables the `thumbnail` capability and makes
   `GET /api/camera/{id}/thumbnail` proxy that image — no server-side decode or
   transcode. Thingino cameras use their firmware API instead and ignore this.
   May carry credentials, so it is never exposed in API responses.

### `[thingino]` keys (optional)

 * **thingino_url:** Base URL of the [Thingino](https://thingino.com/) camera.
 * **thingino_api_key:** API token. Its presence enables the thumbnail
    capability and the firmware lens blackout used by privacy (privacy itself is
    available on every camera). Never exposed in API responses.
 * **ptz:** `true` if the camera has PTZ support (currently Thingino only).
 * **home_x / home_y:** PTZ position the camera returns to on "home" /
    when privacy is disabled, in **degrees** (pan, tilt). The server
    converts to firmware x/y at move time using the calibration below. Unset
    → `-1` (no auto-move).
 * **privacy_x / privacy_y:** PTZ position the camera moves to when privacy
    is enabled, in **degrees** (pan, tilt). Same conversion as `home_x/y`.
    Unset → `-1` (no auto-move).
 * **pan_steps / pan_degrees:** PTZ calibration — total steps the gimbal
    reports for a full pan revolution, and the angular range those steps
    cover. Defaults to `2130` / `360` (typical thingino gimbal); only set
    these if your hardware reports a different value. The server uses them
    to convert a public `pan` (degrees) on `/ptz/move` to firmware `x`
    (steps) and to clamp runaway requests. Never exposed in API responses.
 * **tilt_steps / tilt_degrees:** Same for the tilt axis. Defaults to
    `1600` / `180`.
 * **fov_h:** Horizontal field of view of the lens, in degrees. The public
    `Camera` model exposes it under `ptz.fov_h` so a client can translate
    a pixel drag into a `pan` / `tilt` move without per-camera constants.
    The vertical FOV is derived from this and the aspect ratio at read
    time. Defaults to `113` (typical wide-angle lens on a 16:9 sensor).
