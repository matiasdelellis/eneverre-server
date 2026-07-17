# Eneverre

Eneverre is a lightweight, vendor-agnostic Network Video Recorder (NVR) tailored
for self-hosted IP cameras.

Unlike traditional, heavy enterprise solutions, Eneverre operates as a single
static Go binary that records, relays, and serves camera streams in-process,
exposing a clean, uniform REST API to official apps and third-party clients
alike.

## ✨ Features

Most NVRs are tightly coupled to a single brand or proprietary ecosystem.
Eneverre takes the opposite approach: the same binary, the same configuration,
and the same seamless experience—everywhere.

- 📦 **All-in-One Binary:** Recording, live streaming, and playback run
  out-of-the-box via an [embedded media engine](doc/MEDIA.md). No external 
  streaming servers to install or manage.
- 📱 **Universal Synchronization:** A unified camera list across all clients.
  Android, Android TV, and the web UI all share the exact same state.
- 🔒 **Smooth Authentication:** Session and token management designed for both
  mobile devices and headless screens. Easily pair the Android TV app using a
  short pairing code—no keyboard required.
- 🕶️ **Strict Privacy Mode:** Stop recording and transmission instantly on
  demand. On Thingino cameras, it physically triggers the firmware lens blackout
  and moves the PTZ to a privacy position.
- 📡 **Native Thingino Support:** Enjoy advanced hardware integrations like
  real-time PTZ controls and live thumbnail generation.
- 🔔 **Motion Event Ingestion:** A simple, generic webhook endpoint lets any
  ONVIF or motion detection source push alerts directly to your application
  timeline.
- 🚀 **OTA Update Server for clients:** Publish an APK once, and Eneverre will
- automatically handle updates for all your connected Android devices.

## 🚀 Quick Start

Pre-built binaries for Linux / macOS / Windows are on the
[Releases page][releases].

### 💻 Automated Installation

On Linux and macOS the install script fetches the right one, verifies its
checksum, installs it and (with `--install-service`) registers a `systemd` unit
so it keeps running across reboots:

```bash
curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh | sudo bash -s -- --install-service
```

On Windows, run PowerShell as an Administrator:

```powershell
iex (irm 'https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.ps1')
# add `-InstallService` to register + start the native Windows service
.\install.ps1 -InstallService
```

### 🛠️ Building from Source
Or build it yourself with a recent Go toolchain (1.22+):

```bash
git clone https://github.com/matiasdelellis/eneverre-server
cd eneverre-server
go -C go build -o ./eneverre .
./eneverre
```

### 🪛 Releases

Just download a release tarball, extract it, and run the binary directly. No
installer and no service installed.

```bash
curl -fsSLO "https://github.com/matiasdelellis/eneverre-server/releases/latest/download/eneverre-linux-amd64.tar.gz"
tar -xzf eneverre-linux-amd64.tar.gz
cd eneverre-*/
./eneverre
```

### ✅ First run

Just open <http://localhost:8080/> in a browser. On its very first boot,
Eneverre automatically generates a random initial password for the default
`admin` account, which will be printed directly to the system logs. If you
prefer to set your own credentials from the start, you can fully customize or
override them using environment variables before running the application.

Now you need to add your cameras. From the user menu, navigate to `Manage
cameras` -> `+ Add camera` and follow the step-by-step wizard.

## Companion apps 📱

- 🌐 **Built-in Web UI:** Accessible out-of-the-box directly at /. It covers the
  same functional ground for a quick view or configuration right from your web
  browser, with no setup required.
- 📱 **Eneverre Android (Phones):** Live streams, recording browser, event
  timeline, PTZ controls, privacy toggle, and Picture-in-Picture support..
- 📺 **Eneverre TV (Android TV]** — Optimized full-screen layout designed for TV
  remotes with simple code-pairing authentication.

🛠️ Building your own client? The entire wire-protocol contract is thoroughly
documented in our OpenAPI specification: [`doc/openapi.yaml`](doc/openapi.yaml).

## Screenshots

This is the Android client. 😍

| Login | Cameras | Picture-in-picture | PTZ | Privacy | Playback |
| -- | -- | -- | -- | -- | -- |
| ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/eneverre-login.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/cameras-list.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/pip-camera.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/ptz-camera.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/privacy.png) | ![](https://raw.githubusercontent.com/matiasdelellis/eneverre-docs/refs/heads/main/images/android/playback.png) |

## Documentation

- 📖 [`doc/example/README.md`](doc/example/README.md) — Comprehensive
  configuration reference ([server], [media], etc.), systemd recipes, and
  Caddyfile examples.
- 🎥 [`doc/MEDIA.md`](doc/MEDIA.md) — Details on the embedded media engine:
  codecs, recording structure, MSE live streams, and RTSP relaying.
- 📖 [`doc/openapi.yaml`](doc/openapi.yaml) — the wire-protocol contract for
  client authors.
- 🔬 [`go/README.md`](go/README.md) — Go internals: layout and operational
  notes.

## License

MIT — see [`LICENSE`](LICENSE).
