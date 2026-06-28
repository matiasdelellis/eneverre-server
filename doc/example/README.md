# Config

Eneverre reads one main config file plus one `.ini` per camera. Both are
resolved from `/etc/eneverre/...` first, then `./data/...` (override with
`ENEVERRE_CONFIG_PATH` / `ENEVERRE_CAMERAS_DIR`). Section and key names are
case-insensitive.

## eneverre.ini

The main file holds the listen address and the optional MediaMTX / auth /
events settings. See [`eneverre.ini`](eneverre.ini) in this folder for a fully
commented template.

```ini
[server]
host = 0.0.0.0
port = 8080
; log_level = info        ; debug | info (default) | warn | error
```

> **Admin user.** Eneverre does **not** read a username/password from this file.
> The first admin is seeded into `data/eneverre.db` from the environment
> variables `ENEVERRE_ADMIN_USER` / `ENEVERRE_ADMIN_PASS` (defaults
> `admin` / `eneverre`) the first time the users table is empty. **Change the
> default password before any non-local use.** Manage further users through the
> `/api/users` endpoints or the web UI.

Optional sections (all commented out by default):

```ini
[auth]
; Token lifetimes. Access is the Bearer-token life — also the lifetime of a
; TV (device-login) session, which cannot be refreshed. Refresh is the
; password-login renewal window, slid forward on every refresh.
access_token_ttl_hours = 24
refresh_token_ttl_days = 90

[mediamtx]
server = mediamtx.server.com
rtsp_port = 8554
hls_path = /hls/
webrtc_path = /whep/
playback_port = 9996
rotate_hours = 24            ; credential rotation interval; 0 disables

[events]
webhook_secret = changeme    ; required to accept POST /api/camera/{id}/events
```

When `[mediamtx]` is present the public `rtsp`/`hls`/`webrtc` URLs are generated
dynamically with rotating random credentials, so the `live`/`hls`/`webrtc` keys
in each camera file are ignored. Without it, those keys are served as-is and
securing them is up to your reverse proxy (Caddy, go2rtc, lightNVR, …) — see
the example [`Caddyfile`](Caddyfile). For the wire-level details — the
`POST /api/auth` protocol that MediaMTX calls to validate each request,
the rotation lifecycle, and the reverse-proxy caveats — see
[`doc/MEDIAMTX.md`](../MEDIAMTX.md).

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
live = rtsp://username:password@camera_url:port/path
playback = true
width = 1920
height = 1080

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

 * **id:** Camera id; must match the MediaMTX path when that integration is used.
 * **name / comment / location:** Friendly labels shown by the clients.
 * **live:** Public RTSP URL for playing the camera. Securing it is the
   responsibility of MediaMTX / go2rtc / lightNVR. With the MediaMTX
   integration enabled this key is **ignored** — the URL is generated
   dynamically with a rotating random username and password.
 * **hls / webrtc:** Optional public HLS / WebRTC URLs, likewise ignored when
   MediaMTX integration is enabled.
 * **playback:** Tells clients this camera has recordings available via
   MediaMTX. Ignored without the integration.
 * **width / height:** Pixel dimensions, used to give the playback boxes the
   right aspect ratio (default 16×9).

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
rotating MediaMTX credentials — in `/var/lib/eneverre/` (created automatically
via `StateDirectory=`).

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

The unit seeds the admin user from `ENEVERRE_ADMIN_USER` / `ENEVERRE_ADMIN_PASS`
on first start only — set `ENEVERRE_ADMIN_PASS` before enabling it, then change
the password from the UI (or `PUT /api/users/me/password`). Notes:

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
