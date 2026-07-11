# Config

Eneverre reads one main config file plus one `.ini` per camera. Both are
resolved from `/etc/eneverre/...` first, then `./data/...` (override with
`ENEVERRE_CONFIG_PATH` / `ENEVERRE_CAMERAS_DIR`). Section and key names are
case-insensitive.

## eneverre.ini

The main file holds the listen address and the optional embedded-media /
auth / events settings. See [`eneverre.ini`](eneverre.ini) in this folder
for a fully commented template.

```ini
[server]
host = 0.0.0.0
port = 8080
; log_level = info        ; debug | info (default) | warn | error
```

> **Admin user.** Eneverre does **not** read any username/password from this
> file — all user management lives in `data/eneverre.db`. The first time the
> users table is empty, an `admin` user is created with a **random password
> that is logged once** (`journalctl -u eneverre | grep 'generated password'`,
> or straight to the terminal when run in the foreground). **Log in and change
> it before any non-local use.** To choose the password yourself, set
> `ENEVERRE_ADMIN_PASS` (and optionally `ENEVERRE_ADMIN_USER`) before the first
> start. Manage further users through the `/api/users` endpoints or the web UI.

Optional sections (all commented out by default). `[media]` is the embedded
media engine. The engine is always built for cameras with a `source`
URL; with `[media]` it records per the per-camera `record` INI key
(default true) and enforces `[media].retain`. **Without `[media]`** the
engine runs in **live-only mode** (live MSE + RTSP relay, no disk write,
`/recordings/*` answer 404) — useful when you only want the wall to
work and retention is handled elsewhere:

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

[media]
; Embedded media engine — records each camera, relays it over RTSP and
; broadcasts it to browsers via MediaSource. One binary, no external streamer.
; Every key is optional with sensible defaults; see [eneverre.ini](eneverre.ini)
; for the full list (record_dir, record_path, segment_duration, retain,
; rtsp_address, rtsp_host, transport, gap_message, etc.).
;record_dir    = /var/lib/eneverre/recordings
;rtsp_address  = :8554
;retain        = 240h         ; 0 = keep forever
;transport     = auto         ; auto | tcp | udp
;rotate_hours  = 24           ; RTSP-relay credential rotation; 0 disables

[events]
webhook_secret = changeme    ; required to accept POST /api/camera/{id}/events
```

With `[media]`, every camera records/relays from its `source` RTSP URL
and the public `rtsp` URL is the embedded relay (rotating credentials
included) and `live_mse` is the same-origin browser feed. Without it,
the engine runs in live-only mode: `live_mse` and `rtsp` are still
populated (so the wall works), but `/recordings/*` answer 404. See
[`doc/MEDIA.md`](../MEDIA.md) for the full endpoint list, client
integration notes, and the codec/coverage-gap semantics.

## cameras.d/<id>.ini

Each camera is one file under `data/cameras.d/` (or `/etc/eneverre/cameras.d/`).
Cameras are loaded **once at startup**, so adding or editing one requires a
restart. A file with no `[camera]` section or no `id` is skipped. See
[`cameras.d/camera01.ini`](cameras.d/camera01.ini) (PTZ Thingino camera) and
[`cameras.d/camera02.ini`](cameras.d/camera02.ini) (fixed camera, no Thingino).

```ini
[camera]
id = camera01
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

; The [thingino] section is optional. Its presence (specifically a
; thingino_api_key) is what enables the thumbnail and privacy capabilities;
; ptz = true enables the PTZ endpoints. Omit the whole section for a plain
; fixed camera.
[thingino]
thingino_url = http://192.168.1.91
thingino_api_key = <api-key>
ptz = true
home_x = 1065
home_y = 800
privacy_x = 0
privacy_y = 1600
```

### `[camera]` keys

 * **id:** Camera id; the path the embedded engine records/relays under.
   One id, one camera.
 * **name / comment / location:** Friendly labels shown by the clients.
 * **source:** The camera's RTSP URL. With the embedded media engine
   (`[media]`) the engine records/relays from it; without the engine it
   is served as-is from `/api/cameras` as `rtsp`. Must point at the
   camera itself because the engine speaks RTSP directly to it (not at a
   streamer in front). Never exposed in API responses.
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
 * **transport:** Per-camera override of the global
   `[media] transport` for the source RTSP: `auto` (default), `tcp` (reliable,
   recommended for lossy/distant links), or `udp`. Useful to force TCP on a
   single camera without changing the global default.
 * **playback:** Tells clients this camera has recordings available. With
   `[media]` the engine serves them from its segment index; without it,
   playback endpoints answer 404.
 * **width / height:** Pixel dimensions, used to give the playback boxes the
   right aspect ratio (default 16×9).
 * **backchannel:** Optional direct RTSP URL (with credentials) to the camera's
   ONVIF Profile T two-way-audio backchannel. **Must point at the camera
   itself** so it is kept raw and never rewritten by URL helpers. Its
   presence enables the `talk` capability and the
   `GET /api/camera/{id}/talk` push-to-talk WebSocket. Never exposed in
   API responses.

### `[thingino]` keys (optional)

 * **thingino_url:** Base URL of the [Thingino](https://thingino.com/) camera.
 * **thingino_api_key:** API token. Its presence enables the thumbnail and
   privacy (lens blackout) capabilities. Never exposed in API responses.
 * **ptz:** `true` if the camera has PTZ support (currently Thingino only).
 * **home_x / home_y:** PTZ position the camera returns to on "home" / when
   privacy is disabled. Unset → `-1` (no auto-move).
 * **privacy_x / privacy_y:** PTZ position the camera moves to when privacy is
   enabled. Unset → `-1` (no auto-move).

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
password'`, then change it from the UI (or `PUT /api/users/me/password`). To
set a known password instead, add `ENEVERRE_ADMIN_PASS` (and optionally
`ENEVERRE_ADMIN_USER`) via a drop-in (`systemctl edit eneverre`) before the
first start. Notes:

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
