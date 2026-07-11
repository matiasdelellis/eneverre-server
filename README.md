# Eneverre

A small, vendor-agnostic NVR for self-hosted cameras. A single static Go
binary that records, relays and serves camera streams **in-process**, and
exposes a clean, uniform REST API to the [Android app][android], the
[Android TV app][tv], the built-in web UI, and any other client that speaks
HTTP.

[android]: https://github.com/matiasdelellis/eneverre-android
[tv]: https://github.com/matiasdelellis/eneverre-tv

## Why?

Every NVR I tried is glued to a single brand or stack. Eneverre is the
opposite: the same binary, the same config, the same API — everywhere.

- **One camera list, every client.** Android, Android TV, the web UI, and
  any third-party tool that speaks the API see the same thing.
- **Recording + live + playback in one binary,** via an
  [embedded media engine](doc/MEDIA.md) — no external streamer to install
  or supervise.
- **Login, sessions and a token model** that work on phones *and* headless
  devices — the TV app pairs with a short code, no keyboard needed.
- **Privacy mode** for any camera — stops recording and transmission on
  demand; on [Thingino] cameras it also drives the firmware lens blackout
  and PTZ privacy position.
- **PTZ control** and **live thumbnails** for [Thingino] cameras.
- **Motion event ingestion** via a simple webhook, surfaced as a timeline
  in the apps.
- **Auto-update server** for the Android clients — publish an APK once and
  every device picks it up.

> **ONVIF.** So far only [Thingino] is wired in by design, but the
> motion-event webhook is generic: any ONVIF / motion source can
> `POST /api/camera/{id}/events` and the timeline will show it.

[Thingino]: https://thingino.com/

## Quick start

Pre-built binaries for Linux / macOS / Windows are on the
[Releases page][releases]. On Linux and macOS the install script fetches the
right one, verifies its checksum, and installs it:

```bash
curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh | sudo bash
```

Or build it yourself with a recent Go toolchain (1.22+):

```bash
git clone https://github.com/matiasdelellis/eneverre-server
cd eneverre-server
go -C go build -o ./eneverre .
./eneverre
```

On first run, Eneverre creates an **`admin`** user with a random password
and prints it once to the log. Open <http://localhost:8080/>, log in, and
change it before exposing the port beyond `localhost`.

See [`doc/RELEASES.md`](doc/RELEASES.md) for install-script flags, the
`systemd` service, and how to verify a download.

[releases]: https://github.com/matiasdelellis/eneverre-server/releases

## Add your cameras

Cameras are one INI file each, under `data/cameras.d/`. The bare minimum is
an id, a name and a `source` URL:

```ini
[camera]
id       = front_door
name     = Front door
location = Entrance
source   = rtsp://user:pass@192.168.1.91:554/stream
```

Add a `[thingino]` section to unlock PTZ, live thumbnails and the firmware
lens blackout. Every key — plus the `[media]`, `[auth]`, `[events]` and
`[updates]` sections — is documented in
[`doc/example/README.md`](doc/example/README.md).

## Companion apps 📱

- **[Eneverre Android][android]** — phones and tablets. Live view,
  recordings, motion-event timeline, PTZ control, privacy.
- **[Eneverre TV][tv]** — Android TV. Keyboard-free device login and a
  full-screen playback experience.

The web UI at `/` covers the same ground for a quick look without a phone.

## Screenshots

This is the Android client. 😍

| Login | Cameras | Picture-in-picture | PTZ | Privacy | Playback |
| -- | -- | -- | -- | -- | -- |
| ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/eneverre-login.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/cameras-list.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/pip-camera.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/ptz-camera.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/privacy.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/playback.png) |

## Documentation

- [`doc/example/README.md`](doc/example/README.md) — full config reference
  and the `systemd` install recipe.
- [`doc/MEDIA.md`](doc/MEDIA.md) — the embedded media engine: recording,
  RTSP relay, browser (MSE) live, playback, codecs and configuration.
- [`doc/openapi.yaml`](doc/openapi.yaml) — machine-readable API description
  for client authors.
- [`doc/UPDATES.md`](doc/UPDATES.md) — auto-update protocol for the Android
  clients.
- [`doc/RELEASES.md`](doc/RELEASES.md) — release process, supported
  platforms, and how to verify a download.
- [`go/README.md`](go/README.md) — Go internals: layout and operational
  notes.

## License

MIT — see [`LICENSE`](LICENSE).
