# Eneverre

A small, vendor-agnostic NVR for self-hosted cameras. A single static Go
binary that records, relays and serves camera streams **in-process**,
and exposes a clean, uniform REST API to the [Android app][android], the
[Android TV app][tv], and any other client that wants to talk HTTP.

[android]: https://github.com/matiasdelellis/eneverre-android
[tv]: https://github.com/matiasdelellis/eneverre-tv

> **Why one binary?** Eneverre was originally a thin configuration broker
> in front of an external [MediaMTX] / go2rtc / lightNVR. That extra hop
> turned out to be a lot of moving parts (a separate process, a reverse
> proxy, an auth probe, a control API, a per-camera recorder, a retention
> job…) for what is, at the end of the day, "record the camera, serve the
> clip". The current version runs all of that in-process via the
> [embedded media engine](doc/MEDIA.md). The historical MediaMTX mode was
> removed when the engine proved equivalent for H264 (+AAC/G711) cameras;
> see [`doc/MEDIA.md`](doc/MEDIA.md#why-the-embedded-engine) for the
> short version.

[MediaMTX]: https://github.com/bluenviron/mediamtx

## Why?

Every NVR project I tried is glued to a single brand or stack. Eneverre is
the opposite: the same binary, the same config, the same API — on Android,
Android TV, the web UI, and any third-party tool that speaks the API.

In one box you get:

- **One camera list, every client.** Same view on Android, Android TV, the
  web UI, and any third-party tool that speaks the API.
- **Login, sessions, and a token model** that work on phones *and* on
  headless devices — the TV app pairs with a short code, no on-screen
  keyboard required.
- **Recording + live + playback in one binary.** The embedded media engine
  (gortsplib + mediamux + a per-camera recorder + a retention cleaner)
  drops the need for an external streamer. Web live is over MediaSource
  (~1-2s), Android live is over the embedded RTSP relay, and VOD is served
  from an in-process segment index.
- **Privacy mode** for any camera — stops recording and transmission (live +
  relay) on demand; on [Thingino](https://thingino.com/) cameras it also drives
  the firmware lens blackout and PTZ privacy position.
- **PTZ control** and **live thumbnails** for
  [Thingino](https://thingino.com/) cameras.
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

A fixed camera is the bare minimum — an id, a name, and a `source` URL:

```ini
[camera]
id       = front_door
name     = Front door
location = Entrance
source   = rtsp://user:pass@192.168.1.91:554/stream
```

Privacy works on any camera out of the box (set `privacy = false` in `[camera]`
to make it always-on). Add a `[thingino]` section and you also unlock PTZ, live
thumbnails and the firmware lens blackout, plus optional home/privacy presets
the camera will snap to automatically when privacy toggles:

```ini
[camera]
id        = yard
name      = Backyard
location  = Garden
source    = rtsp://user:pass@192.168.1.92:554/stream
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

Every key (and the optional `[auth]`, `[media]`, `[events]`,
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
DB (and the rotating stream-auth credentials) under `/var/lib/eneverre/`,
and is hardened out of the box.

## Embedded media engine

Eneverre ships a built-in media engine — it runs for every camera with
a `source` URL, with **no external streamer to install or supervise**.

The engine has two modes:

- **No `[media]`** — live-only mode. The live MSE feed
  (`/api/camera/<id>/live/stream`) and the RTSP relay (`:8554`) are
  up; no disk write, no index, `/recordings/*` answer 404. The wall
  works end-to-end; retention is handled elsewhere.
- **`[media]` set** — full mode. The same live feed + relay, plus
  per-camera recording into `[media].record_dir` indexed in
  `[media].index_path`, with `[media].retain` enforcing age-based
  cleanup. Per-camera opt-out via `record = false` in the camera INI.

```ini
[media]
record_dir   = /var/lib/eneverre/recordings
retain       = 240h        ; 0 = keep forever
rtsp_address = :8554       ; RTSP relay for apps
;rtsp_host   = nvr.example.com
```

Each camera records/relays from its `source` RTSP URL. The web
UI plays live over MediaSource (`/api/camera/<id>/live/stream`, ~1-2s
latency); Android plays it over the RTSP relay; playback (VOD) is served
straight from the on-disk segment index. H264 (+AAC/G711) only. Full
reference: [`doc/MEDIA.md`](doc/MEDIA.md).

The historical alternative was an external [MediaMTX] process with
Eneverre as a thin auth/config broker; it was removed when the embedded
engine proved equivalent for H264 cameras. See
[`doc/MEDIA.md`](doc/MEDIA.md#why-the-embedded-engine) for the
historical note.

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
- [`doc/MEDIA.md`](doc/MEDIA.md) — the embedded media engine: recording,
  RTSP relay, browser (MSE) live, playback, codecs and configuration.
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
