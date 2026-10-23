# Changelog

All notable changes to Eneverre are documented here. Versions follow
[Semantic Versioning](https://semver.org/), and releases are cut by tagging
(see [`doc/RELEASES.md`](doc/RELEASES.md)).

## 1.1.0 — 2026-10-23

This one is mostly about **credentials**. Every reusable password in a URL is
gone, machine access is a revocable token tied to a user, and roles plus a
per-camera ACL decide what each principal can see. Recording, live, playback
and two-way audio got their own hardening along the way. The Android phone and
TV clients have already been updated for the new contract.

### 🚨 Breaking changes

If you wrote a client, a script or a dashboard against 1.0.0, read this first.

- **HTTP Basic is gone.** Every route takes a Bearer credential: a session
  access token or a service token (`envr_svc_…`).
- **The RTSP relay URL changed.** It is now
  `rtsp://host:port/<camera-id>/<exp>.<sig>` — a 6-hour token scoped to one
  camera. There is no global relay username/password anymore; re-read
  `GET /api/cameras` for a fresh URL.
- **No more credentials in query strings.** The talk WebSocket no longer
  accepts `?token=`, the motion webhook no longer accepts `?token=`, and HTTP
  media no longer accepts the relay `user`/`pass` pair.
- **Pairing codes are 8 hex characters.** The 6-character compatibility form
  was removed.
- **Recordings and HLS require a bounded window.** `recordings/list`,
  `recordings/gaps` and the HLS playlist need `start` and `end`, and reject
  spans over 7 days (`422`).
- **TV/headless sessions changed.** The device flow now issues a refresh token
  and pins the session to the approving user's role, clamped to `user` when
  that user is an admin. Clients that ignore the new fields keep working.
- **`must_change_password` is enforced by the server.** Every protected route
  answers `403 {"code":"password_change_required"}` except the password
  change, logout and session revocation; clients should route that to the
  change screen.
- **Zabbix and other integrations use a service token**, not Basic auth.

### 🔐 Security and access control

- **Roles.** `admin` (everything), `user` (live, playback, PTZ, privacy,
  talk), `viewer` (live and events, never playback) and `monitor` (status and
  metrics only). The capability map is the single source of truth.
- **Per-camera ACL.** `acl_restricted` plus the `camera_acl` table narrow every
  role to a camera set; `0` still means all cameras. Out-of-scope camera
  endpoints answer `404` (not `403`), and `/api/cameras` plus `/api/status` are
  filtered. Admin bypasses the ACL.
- **Service tokens.** `POST /api/service-tokens` mints a token linked to a
  user; the raw `envr_svc_…` value is shown once and only its SHA-256 hash is
  stored. The token inherits the user's role and ACL and is invalidated
  immediately on revoke. Account, token and ACL management stays
  human-admin-only, and a token cannot mint more tokens.
- **Short-lived media grants.** Thumbnail, live MSE and HLS accept a camera-
  and surface-scoped `exp`/`sig` grant for clients that cannot set a header, so
  no reusable media password is needed. The same key signs the RTSP path
  token; the old `streamauth` credentials table is dropped on upgrade.
- **Refresh-token rotation with reuse detection.** A replayed refresh token
  within 30 s is treated as a lost response (401, session kept); later replay
  is treated as theft, deletes the session and emits `refresh_token_reuse` in
  the security log. Password and device sessions share the path.
- **Browser hardening.** CSP with the boot shims in `/boot.js`,
  `X-Frame-Options`/`frame-ancestors`, `Referrer-Policy`, a microphone-only
  `Permissions-Policy`, HSTS behind TLS and `no-store` on API responses. With
  no `cors_origins` configured, any origin may read responses but only
  explicitly listed origins get credentialed CORS.
- **Throttling behind a proxy.** Failed logins are keyed on the rightmost
  untrusted `X-Forwarded-For` hop (behind Caddy every request used to look
  like 127.0.0.1, so 20 bad passwords locked everyone out), attempts are
  reserved before the password hash so a burst cannot slip past the cap, and
  the per-user cap only blocks sources that are themselves failing.
- **Device flow.** Approval is claimed atomically (one token per pairing), code
  creation is rate-limited per IP and re-drawn on collision, and an account
  flagged for a password change cannot approve a device.
- **Security log hardening.** Every client-supplied field is quoted and
  escaped and `ip=` must parse as an address, so a crafted username or a
  `%0A` in the path cannot forge a line for fail2ban.
- **Smaller fixes.** Camera URLs are validated without echoing the credential;
  the snapshot proxy refuses active image types and stops following redirects;
  the last admin cannot be deleted or demoted; event deletion is admin-only;
  the database file is chmod 0600 and the example unit sets `UMask=0077`.

### ✨ Added

- **Disable a camera without deleting it.** Setting `enabled: false` stops the
  recorder, live MSE, relay and the interactive endpoints (PTZ, talk,
  settings, privacy, thumbnail, webhook), while the footage already on disk
  stays indexed and playable. Re-enabling re-probes the camera, so a lens left
  blacked out by privacy is picked up again.
- **Live camera settings (Thingino).** `GET/PUT /api/camera/{id}/settings`
  adjusts motion detection, mic, speaker and day/night auto, and the web UI
  gets a settings dialog for it.
- **Two-way audio grew up.** The ONVIF backchannel is discovered from the
  camera source (the probe reports codecs, so the config key is optional),
  Opus is passed through untouched, and the hand-rolled RTSP client was
  replaced by `gortsplib`: auth, TCP interleaved framing, keepalives, RTCP
  sender reports and AAC `a=fmtp` packetization come from the library, while
  the local SDP layer still picks the backchannel track and payload type the
  cameras actually send. The WebSocket read is capped at 1 MiB.
- **Retention removes orphan segments.** A sweep at startup and every 6 hours
  deletes segment-shaped files the index does not know about (failed inserts,
  crash leftovers, a lost `index.db`), which used to fill the recording volume
  silently.
- **Playback UI.** Previous/next recording, zoom controls on mobile, "Share
  this moment", a speed selector, and a correct return to live. The cursor
  re-anchors when the speed changes instead of re-timing what was already
  played.
- **UI and accessibility.** Shared thumbnail cache between the sidebar and the
  wall, a global Escape for custom modals, keyboard/reader activation for
  tiles, focus traps, live language switching for dynamic views, and a
  role-aware UI that hides what the server would refuse.
- **Zabbix and Grafana examples**, driven by a `monitor` user and a linked
  service token.

### 🛠️ Fixed and hardened

- **Media engine.** The recorder closes its RTSP client on every failed
  connect (a bad password used to leak a goroutine and a socket per retry) and
  tears the relay/MSE state down on any exit; retention honours cancellation;
  the low-disk watcher re-arms the emergency purge while space stays low;
  segment-index inserts retry with bounded backoff; `RemoveCamera` waits for
  the camera loop to exit; a slow live viewer is disconnected instead of
  losing parts mid-GOP; every live write has its own deadline; a negative
  initial DTS is shifted instead of wrapping `tfdt`; corrupt segments are
  bounded; the gap-fill ffmpeg run has a 30 s timeout; the recorder waits for
  a keyframe after a write error.
- **Recording ownership.** Recovery and reindex only claim files the
  `record_path` format attributes to the camera, so a rebuild no longer
  re-assigns another camera's footage. Deleting a camera removes its events
  and, in the background, its segments and unindexed leftovers.
- **Privacy and schedules.** Heartbeat state and manual toggles are applied
  under the same lock, both ways, and no longer fight; edits, deletes and the
  scheduler tick are serialized; background seeds drop results for a camera
  edited or deleted meanwhile.
- **Playback API.** `start`/`end` are required and capped at 7 days, so a
  parameterless request can no longer materialize a camera's whole segment
  index. Clip export validates the duration (a `1000h` string used to bypass
  the 4 h cap). PTZ rejects NaN/Inf, and `home` is only advertised and executed
  when `home_x`/`home_y` are configured.
- **Frontend.** MSE recovers from `QuotaExceeded` and append errors; a stale
  playback start can no longer hijack the live tiles; hidden-tab handling acts
  on the current state; the camera-list cache cannot be repopulated by an
  in-flight request after invalidation; the upgrade prompt accepts `http(s)`
  downloads from another origin but still blocks script URLs.
- **Build and startup.** The `linux/arm` build is fixed, and a missing config
  file now logs where the example lives.

### 📚 Docs

- `openapi.yaml`, `MEDIA.md`, `TALK.md`, `UPDATES.md`, `security-logging.md`,
  the INI reference and the example `Caddyfile`/unit describe the new auth
  contract, the roles/ACL and the token model.
- Decision records for browser live over WebRTC, MoQ and UDP hole punching
  (all evaluated and not adopted for now).

## 1.0.0 — 2026-07-27

🎉 **The first release.** Eneverre is a self-hosted NVR that ships as a single
static binary: download it, run it, open <http://localhost:8080/>, and add a
camera. No streaming server to install, no database to provision, no
configuration file required.

Everything below is new — it's the first version — so instead of a flat "Added"
list, here's what you actually get.

### 📦 One binary, zero dependencies

- Recording, live streaming, playback, and the web UI all run in-process.
  There is **no MediaMTX or ffmpeg to install and babysit**.
- Static builds for Linux (amd64, arm64, arm), macOS (amd64, arm64) and
  Windows (amd64, arm64), each with a SHA-256 checksum.
- Install however you like: extract a tarball, run
  [`scripts/install.sh`](scripts/install.sh) (Linux/macOS, optional systemd
  unit), [`scripts/install.ps1`](scripts/install.ps1) (Windows, optional native
  service), or build from source with `make build`.
- `eneverre.ini` is **optional**. With no config at all, sensible defaults
  apply; `--data-dir` keeps config, cameras, and the database under one folder.

### 🎥 Recording and playback

- **Recording is on by default** — that is the whole point of an NVR.
- Continuous recording with a **7-day retention** default, plus **per-camera
  schedules** so a camera can record only when you want it to.
- **Low-disk safety:** free space is checked continuously and the oldest
  recordings are pruned before the disk fills up. `max_part_size` keeps
  segment buffers from eating RAM.
- **Crash recovery:** the recording index is rebuilt automatically on restart,
  and a lost or corrupt index can be regenerated with the reindex option.
- A scrubbable **timeline** that keeps updating while you watch, gap-aware clip
  downloads, and **Share this moment** links that deep-link straight to a
  timestamp.

### 📡 Live view and two-way audio

- Low-latency **live in the browser** over MSE (fMP4) — no plugins, no Flash
  ghosts.
- **RTSP relay** for VLC, ffmpeg, and anything else that speaks RTSP.
- **H264 and H265/HEVC** both recorded, relayed, and served. Browser live for
  H265 depends on the browser having an HEVC decoder, and the UI says so
  clearly instead of showing a black tile.
- Audio in **AAC** and **G711** (transcoded to LPCM for fMP4).
- **Push-to-talk** two-way audio, with a clean shutdown when you release the
  button.

### 🕶️ Privacy, PTZ, and Thingino

- **Strict privacy mode** from the top bar: recording and transmission stop
  instantly. On Thingino cameras it also triggers the firmware lens blackout
  and parks the PTZ in a privacy position.
- **PTZ in degrees** — an absolute, camera-independent API. Double-click the
  image to move the camera there.
- Thingino cameras get their parameters configured automatically where
  possible, plus live thumbnails.

### 📱 Clients and synchronization

- A **built-in web UI** covering the same ground as the apps: camera wall,
  playback, PTZ, privacy, schedules, account, and status.
- **Eneverre Android** (phones) and **Eneverre TV** (Android TV) share the exact
  same state as the web UI — one camera list everywhere.
- **Code pairing** for TVs and headless screens: no keyboard gymnastics.
- **OTA update server** for the Android clients: publish an APK once and
  connected devices update themselves.
- The whole wire protocol is documented in
  [`doc/openapi.yaml`](doc/openapi.yaml) if you'd rather write your own client.

### 🔒 Security and operations

- The first boot generates a random `admin` password, logs it once, and
  **forces a password change** at first login. Prefer your own?
  `ENEVERRE_ADMIN_USER` / `ENEVERRE_ADMIN_PASS`.
- Access/refresh tokens with configurable lifetimes, a visible session list
  (including recently expired ones), and background cleanup of stale tokens.
- **Security log** in a stable one-line format for fail2ban / CrowdSec — see
  [`doc/security-logging.md`](doc/security-logging.md).
- `trusted_proxies` so the logged client IP is the real one behind a reverse
  proxy, and a CORS allowlist for locking down the browser surface.
- **Metrics** at `/api/metrics` (Prometheus) and `/api/metrics/json`, plus a
  status page in the UI.
- **Motion event ingestion** via a generic webhook, so any ONVIF or motion
  source can push into your timeline.

### 🌍 Interface

- Full **internationalization**, currently English and Spanish, selectable
  right from the login screen.
- Light and dark themes, keyboard and URL navigation, a drag-and-drop
  Locations sidebar, and Picture-in-Picture on Android.

### Known limits

- Browser live and streaming playback for **H265** require an HEVC-capable
  browser; the fallback is RTSP. Transcoding is deliberately out of scope.
- Clip **gap-fill** (the black "NO RECORDING" filler) is H264-only — an H265
  clip spanning a gap is truncated at the gap instead. The footage itself is
  untouched.
- Cameras offering codecs other than H264/H265 (AV1, MJPEG, …) are detected and
  logged, but neither recorded nor relayed.

### Getting started

```bash
curl -fsSLO "https://github.com/matiasdelellis/eneverre-server/releases/latest/download/eneverre-linux-amd64.tar.gz"
tar -xzf eneverre-linux-amd64.tar.gz
cd eneverre-*/
./eneverre
```

Then open <http://localhost:8080/> and follow the log for the admin password.
Everything else — configuration reference, the media engine, the API — lives in
[`doc/README.md`](doc/README.md).
