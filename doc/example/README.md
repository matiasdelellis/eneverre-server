# Config

Eneverre reads one main config file plus one `.ini` per camera. Both are
resolved from `/etc/eneverre/...` first, then `./data/...` (override with
`ENEVERRE_CONFIG_PATH` / `ENEVERRE_CAMERAS_DIR`). Section and key names are
case-insensitive.

**The main config file is optional.** When neither `/etc/eneverre/eneverre.ini`
nor `./data/eneverre.ini` exists, Eneverre starts with every setting at its
built-in default (the same as an empty file). It only fails to start if the
file *you pointed it at* is missing — i.e. an explicit `ENEVERRE_CONFIG_PATH`
or `--config` path that does not exist — or if a file that *does* exist fails
to parse. So a minimal install needs no `eneverre.ini` at all, but a typo in an
explicit path is caught instead of silently falling back to defaults.

## eneverre.ini

The main file holds the listen address and the optional embedded-media /
auth / events settings. See [`eneverre.ini`](eneverre.ini) in this folder
for a fully commented template.

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

`[server]` keys (all optional):

 * **host / port:** Listen address. Default `0.0.0.0:8080`.
 * **log_level:** `debug` | `info` (default) | `warn` | `error`. Overridden by
   `--log-level` / `ENEVERRE_LOG_LEVEL`.
 * **read_timeout:** Max time to read an HTTP request body (`time.ParseDuration`
   format). Default `5m` — enough to publish a ~200 MiB APK over a slow link.
   Precedence: this key > `ENEVERRE_READ_TIMEOUT` > `5m`.
 * **metrics:** Prometheus metrics at `/api/metrics` (+ `/api/metrics/json`). On
   by default; open to a loopback scraper and authenticated from anywhere else.
   Set `false` to drop the endpoints entirely. See [`doc/MEDIA.md`](../MEDIA.md#metrics).
 * **cors_origins:** Comma-separated browser CORS allowlist. Empty (default) is
   permissive — any Origin is reflected, which is safe with same-origin UI +
   Bearer-token auth. Set it to lock the browser surface to known front-ends.
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

Optional sections (all commented out by default). `[media]` is the embedded
media engine. The engine is **always** built for cameras with a `source`
URL, and the **live MSE feed, RTSP relay and disk recording are all on by
default** (no `[media]` section needed — that is the point of the app). With
recording on the engine also enforces `[media] retain` (default 7d). When no
`record_dir` is set the engine uses `/var/lib/eneverre/recordings` if that
directory exists and otherwise `<data_dir>/recordings`. Opt individual cameras
out with a per-camera `record = false`, or set `[media] record = false` for
**live-only mode** (live MSE + RTSP relay, no disk write, `/recordings/*`
answer 404), useful when you only want the wall to work and retention is
handled elsewhere:

```ini
[auth]
; Token lifetimes. Access is the Bearer-token life — also the lifetime of a
; TV (device-login) session, which cannot be refreshed. Refresh is the
; password-login renewal window, slid forward on every refresh.
access_token_ttl_hours = 24
refresh_token_ttl_days = 90
; Token-cleanup background interval (minutes). A ticker prunes expired token
; rows between user logins so the DB doesn't accumulate dead sessions on a
; rarely-used installation. 0 disables the background loop (login-time
; cleanup still runs). Precedence: this key > ENEVERRE_TOKEN_CLEANUP_INTERVAL
; > 60.
;cleanup_interval_minutes = 60
;
; Grace period in hours: a token stays visible in the sessions list for this
; long after it expires before the cleaner deletes it. 0 deletes expired
; tokens immediately (previous behaviour). Default 24.
;cleanup_grace_hours = 24
; Security event log: when set, authentication failures are also written here
; one line per event, for fail2ban/CrowdSec to tail. Empty (default) = no
; dedicated file (events still go to the main log at WARN). See
; doc/security-logging.md. Precedence: this key > ENEVERRE_SECURITY_LOG > "".
;security_log = /var/log/eneverre/security.log

[media]
; Embedded media engine — live MSE + RTSP relay + disk recording (all on by
; default). One binary, no external streamer. Every key is optional with
; sensible defaults; see [eneverre.ini](eneverre.ini) for the fully commented
; list.
;mse           = true         ; global toggle for the live MSE browser feed
;relay         = true         ; global toggle for the RTSP relay
;record        = true         ; global toggle for disk recording (off with false)
;record_dir    = /var/lib/eneverre/recordings
;index_path    = /var/lib/eneverre/recordings/index.db   ; segment index DB
;cache_dir     = /var/lib/eneverre/cache                 ; gap-fill frame cache
;segment_duration = 60s       ; min segment length
;part_duration    = 1s        ; fMP4 fragment length (crash recovery-point)
;max_part_size    = 50M       ; safety cap on a single fMP4 fragment (RAM valve)
;retain        = 7d           ; default 7d; set 0 to keep forever; accepts ParseDuration + "d" days
;min_free_bytes = 1G           ; pause + force-purge oldest below this; 0 disables; accepts K/M/G/T
;rtsp_address  = :8554
;transport     = auto         ; auto | tcp | udp
;gap_message   = NO RECORDING ; caption burned into gap-fill black frames
;rotate_hours  = 24           ; RTSP-relay credential rotation; 0 disables

[events]
webhook_secret = changeme    ; required to accept POST /api/camera/{id}/events
;pre_seconds  = 5            ; widen each event's range this many seconds before
;post_seconds = 5            ; ...and after the trigger (default 5 + 5)

[updates]
; Auto-update server for the Android clients. OFF unless storage_dir (or
; ENEVERRE_UPDATES_DIR) is set — otherwise /api/app/* return 503.
;storage_dir     = /var/lib/eneverre/app-updates
;public_base_url = https://updates.example.com   ; base URL in the APK manifest
;publish_token   = <32-byte-secret>              ; gate the publish endpoints
;max_build_size  = 100M                          ; hard cap on the upload body
```

Motion events are pruned on the **same** retention window as recordings
(`[media] retain`): with it set, a background sweep drops events older than
the window so the events table never outlives the footage its rows
reference. With `[media] retain` unset (the 7d default), the sweep still
runs against the default window. Set `retain = 0` to keep events forever.

See [`doc/MEDIA.md`](../MEDIA.md) for the full endpoint list, client
integration notes, and the codec/coverage-gap semantics.

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

## cameras.d/*.ini

Each camera is one file under `data/cameras.d/` (or `/etc/eneverre/cameras.d/`);
the filename is arbitrary. These files are only an initial seed — they are
imported into the database **once**, when the camera table is empty (a fresh
install). After that, cameras are managed entirely through the web UI / API and
the INI files are ignored. A file with no `[camera]` section or no `name` is
skipped. See [`cameras.d/camera01.ini`](cameras.d/camera01.ini) (PTZ Thingino
camera) and [`cameras.d/camera02.ini`](cameras.d/camera02.ini) (fixed camera, no
Thingino).

```ini
[camera]
name = Outside
comment = Thingino 360 Camera
location = Exterior
source = rtsp://username:password@camera_url:port/path
playback = true
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

### `[camera]` keys

 * **name:** The camera's display name, and its identity. The internal id —
   the path the embedded engine records/relays under, and the `{id}` in every
   API URL — is derived from it as a lowercased, accent-folded slug
   ("Outside" → "outside"); same-slug names are disambiguated with a numeric
   suffix. It is never set by hand and cannot change once assigned. `name` is
   required.
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
    a Source are recorded). Set to `false` to keep the live MSE feed and
    the RTSP relay working for this camera but skip writing to disk — the
    `/recordings/*` endpoints for it answer 404. Useful for privacy-
    sensitive cameras you only want to watch live.
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
 * **playback:** Per-camera opt-out of the recordings UI. The server exposes
   the Live/Playback switch for a camera **only when it actually has
   recordings on disk** — it is not a hint you set by hand. Recording a camera
   makes its playback appear automatically once the first segment is written;
   set `playback = false` to keep recording while hiding playback in the UI.
   It doesn't turn recording on (that's `record`, see above); a camera with no
   recordings never advertises playback.
 * **width / height:** Pixel dimensions, used to give the playback boxes the
   right aspect ratio (default 16×9).
 * **backchannel:** Optional direct RTSP URL (with credentials) to the camera's
   ONVIF Profile T two-way-audio backchannel. **Must point at the camera
   itself** so it is kept raw and never rewritten by URL helpers. Its
   presence enables the `talk` capability and the
   `GET /api/camera/{id}/talk` push-to-talk WebSocket. Never exposed in
   API responses.
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
    converts to firmware x/y at move time using the calibration above. Unset
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

## Running as a systemd service

[`eneverre.service`](eneverre.service) is a ready-to-use unit. It runs the
binary as an isolated transient user (`DynamicUser=yes`), reads config from
`/etc/eneverre/`, and keeps its state — a single SQLite DB, which also holds the
rotating stream-auth credentials — in `/var/lib/eneverre/` (created
automatically via `StateDirectory=`).

```bash
# Binary + config + cameras
sudo install -m0755 eneverre /usr/local/bin/eneverre
sudo install -d /etc/eneverre/cameras.d
sudo install -m0644 doc/example/eneverre.ini /etc/eneverre/eneverre.ini
sudo cp doc/example/cameras.d/*.ini /etc/eneverre/cameras.d/

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
 * **Config permissions.** `DynamicUser=yes` means the config must stay readable
   by the transient user (mode `0644`). If `eneverre.ini` holds secrets on a
   multi-user host, switch to a dedicated `eneverre` account instead — the unit
   file's header comment shows how.
 * **Override without editing the unit.** Use a drop-in:
   `sudo systemctl edit eneverre` (e.g. to change the port, log level, or admin
   env vars).
