# Getting Started

This page picks up right after the top-level [`README.md`](../README.md)
downloads-and-runs a release tarball. It covers the other ways to install
Eneverre, first-run options, and points at the complete documentation for
everything past a minimal install.

## Other ways to install

### Automated installation

On Linux and macOS the install script fetches the right binary for your
platform, verifies its checksum, installs it and (with `--install-service`)
registers a `systemd` unit so it keeps running across reboots:

```bash
curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh | sudo bash -s -- --install-service
```

On Windows, run PowerShell as an Administrator:

```powershell
iex (irm 'https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.ps1')
# add `-InstallService` to register + start the native Windows service
.\install.ps1 -InstallService
```

See [`WINDOWS.md`](WINDOWS.md) for Windows-service details, and
[`example/README.md`](example/README.md) for the systemd unit used on Linux.

### Building from source

Build it yourself with a recent Go toolchain (1.22+):

```bash
git clone https://github.com/matiasdelellis/eneverre-server
cd eneverre-server
go -C go build -o ./eneverre .
./eneverre
```

## First run

Just open <http://localhost:8080/> in a browser. On its very first boot,
Eneverre automatically generates a random initial password for the default
`admin` account, which is printed directly to the logs — the terminal in the
foreground, or `journalctl -u eneverre | grep 'generated password'` when
installed as a service. That first login forces a password change before the
app opens, so the bootstrap password never becomes the permanent one.

To skip that and set your own credentials from the start, export them before
the first run:

```bash
export ENEVERRE_ADMIN_USER=admin      # optional, defaults to "admin"
export ENEVERRE_ADMIN_PASS=changeme   # sets the initial password
./eneverre
```

A few other common startup options (`./eneverre --help` for the full list):

```bash
./eneverre --host 0.0.0.0 --port 8080   # listen address, default 0.0.0.0:8080
./eneverre --data-dir ./data-quincho    # keep config, cameras and DB under one folder
./eneverre --log-level debug            # debug | info (default) | warn | error
```

That covers a minimal, no-config install. Everything below goes further.

## Full documentation

- 📖 [`example/README.md`](example/README.md) — the complete configuration
  reference: `eneverre.ini` (`[server]`, `[auth]`, `[media]`, `[events]`,
  `[updates]`), per-camera `.ini` options, running as a systemd service, and a
  Caddyfile for TLS.
- 🎥 [`MEDIA.md`](MEDIA.md) — the embedded media engine: codecs, recording
  layout, MSE live streams, and RTSP relaying.
- 📖 [`openapi.yaml`](openapi.yaml) — the wire-protocol contract for client
  authors.
- 🔬 [`../go/README.md`](../go/README.md) — Go internals: layout and
  operational notes.
- 🔒 [`security-logging.md`](security-logging.md) — feeding auth failures to
  fail2ban / CrowdSec.
- 🎙️ [`TALK.md`](TALK.md) — the push-to-talk (two-way audio) client protocol.
- 🚀 [`UPDATES.md`](UPDATES.md) — the Android OTA auto-update protocol.
- 🪟 [`WINDOWS.md`](WINDOWS.md) — installing and running as a Windows service.
