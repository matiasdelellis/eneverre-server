# Changelog

All notable changes to Eneverre are documented here. Versions follow
[Semantic Versioning](https://semver.org/), and releases are cut by tagging
(see [`doc/RELEASES.md`](doc/RELEASES.md)).

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
