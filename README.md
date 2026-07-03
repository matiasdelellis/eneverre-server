# Eneverre

A small, vendor-agnostic API for self-hosted NVRs. It does not stream and it
does not record — it sits **in front** of whatever you already use
([MediaMTX], [go2rtc], [lightNVR]…) and exposes a clean, uniform interface
to the [Android app][android], the [Android TV app][tv], and any other
client that wants to talk HTTP.

[MediaMTX]: https://github.com/bluenviron/mediamtx
[go2rtc]: https://github.com/AlexxIT/go2rtc
[lightNVR]: https://github.com/opensensor/lightNVR
[android]: https://github.com/matiasdelellis/eneverre-android
[tv]: https://github.com/matiasdelellis/eneverre-tv

## Why?

Every NVR project I tried is glued to a single brand or stack. Eneverre is
the opposite: pick the streamer you like, point Eneverre at it, and the
clients just work everywhere.

In one box you get:

- **One camera list, every client.** Same view on Android, Android TV, the
  web UI, and any third-party tool that speaks the API.
- **Login, sessions, and a token model** that work on phones *and* on
  headless devices — the TV app pairs with a short code, no on-screen
  keyboard required.
- **Optional MediaMTX integration** that hands out public RTSP / HLS /
  WebRTC URLs with **rotating random credentials**, so you can put the
  cameras on the internet without giving out a static secret.
- **PTZ control**, **privacy mode (lens blackout)** and **live thumbnails**
  for [Thingino](https://thingino.com/) cameras.
- **Motion event ingestion** via a simple webhook, surfaced as a timeline in
  the apps.
- **Auto-update server** for the Android clients — publish a new APK once
  and every device picks it up on next boot.

> **ONVIF.** I would love to say this supports ONVIF — the whole reason
> this project exists is that I couldn't find an easy way to manage my
> ONVIF cameras. So far only [Thingino](https://thingino.com/) is wired in
> by design, but the motion-event webhook is generic: any ONVIF / motion
> source can `POST /api/camera/{id}/events` and the timeline will show it.

## Quick start

You need a recent Go toolchain (1.22 or newer). The output is a single
static binary — no runtime, no CGO, no `pip install`.

```bash
git clone https://github.com/matiasdelellis/eneverre-server
cd eneverre-server
go -C go build -o ./eneverre .
./eneverre
```

On first run, Eneverre creates an **`admin`** user with a random password
and prints it once to the log (here, straight to the terminal):

```
WARN ... no users found: created admin with a generated password user=admin password=Xk9...
```

Open <http://localhost:8080/>, log in as `admin` with that password, and
change it — definitely before opening the port to anything that isn't
`localhost`. (Set `ENEVERRE_ADMIN_PASS` before the first start to choose
the password yourself instead.)

Pre-built binaries for Linux / macOS / Windows (amd64, arm64, arm) are on
the [Releases page](https://github.com/matiasdelellis/eneverre-server/releases).
Each tarball ships with the example config and a `systemd` unit — see
[`doc/RELEASES.md`](doc/RELEASES.md) for the layout and how to verify a
download.

### Install script

On Linux and macOS, [`scripts/install.sh`](scripts/install.sh) downloads
the right release for your platform, verifies its SHA-256, and installs
the binary to `/usr/local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh | sudo bash
```

Re-run it any time to update to the latest release (it replaces the
binary atomically, so it is safe even while the service is running).
On Linux you can also set up the `systemd` service in one go:

```bash
sudo bash install.sh --install-service     # install + enable + start
sudo bash install.sh --uninstall           # stop, remove unit + binary
```

Every flag (`--version`, `--list`, `--target-dir`, `--dry-run`, …) is
documented in [`doc/RELEASES.md`](doc/RELEASES.md#installing-with-installsh),
or run `install.sh --help`.

## Add your cameras

Cameras are one INI file each, under `data/cameras.d/` (or
`/etc/eneverre/cameras.d/`). They are loaded at startup, so adding or
editing a camera needs a restart.

A fixed camera is the bare minimum — an id, a name, and a public
`live` URL:

```ini
[camera]
id       = front_door
name     = Front door
location = Entrance
live     = rtsp://user:pass@192.168.1.91:554/stream
```

Add a `[thingino]` section and you unlock PTZ, live thumbnails and privacy
(lens blackout), plus optional home/privacy presets the camera will snap
to automatically:

```ini
[camera]
id        = yard
name      = Backyard
location  = Garden
live      = rtsp://user:pass@192.168.1.92:554/stream
playback  = true
width     = 1920
height    = 1080

[thingino]
thingino_url     = http://192.168.1.92
thingino_api_key = your-thingino-token
ptz              = true
home_x           = 1065
home_y           = 800
privacy_x        = 0
privacy_y        = 1600
```

Every key (and the optional `[auth]`, `[mediamtx]`, `[events]`,
`[updates]` sections) is documented in
[`doc/example/README.md`](doc/example/README.md).

## Run as a service

If you installed from a release, `install.sh --install-service` already
installed, enabled and started the `systemd` unit for you (see
[Install script](#install-script) above). The rest of this section is the
manual recipe, handy when you built from source:

There is a ready-to-use `systemd` unit and a one-liner install recipe in
[`doc/example/README.md`](doc/example/README.md#running-as-a-systemd-service).
The short version:

```bash
sudo install -m0755 eneverre /usr/local/bin/eneverre
sudo install -d /etc/eneverre/cameras.d
sudo install -m0644 doc/example/eneverre.ini /etc/eneverre/eneverre.ini
sudo cp doc/example/cameras.d/*.ini /etc/eneverre/cameras.d/
sudo install -m0644 doc/example/eneverre.service /etc/systemd/system/eneverre.service
sudo systemctl enable --now eneverre
```

The unit runs the service as a transient isolated user, keeps the SQLite
DB (and the rotating MediaMTX credentials) under `/var/lib/eneverre/`,
and is hardened out of the box.

## MediaMTX integration (recommended)

Eneverre on its own only brokers *configuration* — to actually stream and
record, point it at [MediaMTX]:

```ini
[mediamtx]
server        = nvr.example.com
rtsp_port     = 8554
hls_path      = /hls/
webrtc_path   = /whep/
playback_port = 9996
rotate_hours  = 24
```

Restart, and the public `rtsp` / `hls` / `webrtc` URLs in
`GET /api/cameras` start coming back with a **random rotating
username/password** baked in. MediaMTX is told to authorize each request
against `POST /api/auth`, and the credentials rotate every
`rotate_hours` hours (with a one-interval grace window so active streams
are not dropped at the rollover).

The auth protocol, the request / response shapes, the rotation
lifecycle, and the reverse-proxy caveats are spelled out in
[`doc/MEDIAMTX.md`](doc/MEDIAMTX.md).

No MediaMTX? No problem — point each camera's `live` URL at whatever you
already use (go2rtc, lightNVR, or a plain reverse proxy in front of the
camera) and skip the `[mediamtx]` section entirely. The web UI and the
Android apps don't care which.

## Companion apps 📱

- **[Eneverre Android](https://github.com/matiasdelellis/eneverre-android)**
  — phones and tablets. Live view, recordings, motion-event timeline, PTZ
  control, privacy.
- **[Eneverre TV](https://github.com/matiasdelellis/eneverre-tv)**
  — Android TV. Live view with a device-login flow that needs no
  keyboard, plus a full-screen playback experience.

The web UI at `/` covers the same ground for a quick look without a
phone.

## Screenshots

This is the Android client. 😍

| Login | Cameras | Picture-in-picture | PTZ | Privacy | Playback |
| -- | -- | -- | -- | -- | -- |
| ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/eneverre-login.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/cameras-list.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/pip-camera.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/ptz-camera.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/privacy.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/playback.png) |

## Where to go next

- [`doc/example/README.md`](doc/example/README.md) — full config
  reference and the `systemd` install recipe.
- [`doc/MEDIAMTX.md`](doc/MEDIAMTX.md) — the MediaMTX integration in
  detail: the `POST /api/auth` protocol, credential rotation, and
  reverse-proxy caveats.
- [`doc/openapi.yaml`](doc/openapi.yaml) — machine-readable API
  description for client authors.
- [`doc/UPDATES.md`](doc/UPDATES.md) — auto-update protocol for the
  Android clients (how publishing and downloading actually work).
- [`doc/RELEASES.md`](doc/RELEASES.md) — release process, supported
  platforms, and how to verify a download.
- [`go/README.md`](go/README.md) — Go internals: layout, endpoint
  parity with the original Python service, and operational notes.

## License

MIT — see [`LICENSE`](LICENSE).
