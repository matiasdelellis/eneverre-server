# Windows install & service

Eneverre runs on Windows exactly like on Linux: it's a single static Go
binary (`eneverre.exe`) with the media engine, SQLite (pure-Go
`modernc.org/sqlite`) and the web UI all embedded. There is **no external
dependency to install** — no MediaMTX, no runtime, no service wrapper, and
ffmpeg is optional (only used to pre-render the privacy black frame; without
it a text placeholder is used).

`eneverre.exe` is **service-aware on Windows**: launched by the Service
Control Manager it reports its state to the SCM and turns a *Stop* / machine
shutdown into the same graceful shutdown as `Ctrl+C`, so the in-progress
recording segment is finalized instead of dropped. That means the built-in
`New-Service` / `sc.exe` tooling is all you need — no NSSM, no WinSW.

On Windows the default search paths already follow Windows conventions:
Eneverre looks for its config, cameras and database under
`%ProgramData%\Eneverre\` (e.g. `C:\ProgramData\Eneverre\`), falling back to a
portable `.\data\` next to the current directory. So if you use that default
layout you can run `eneverre.exe` with **no flags at all**. Point it elsewhere
with a handful of flags / environment variables (§3). The
[install script](#4-quick-install-with-installps1) sets everything up for you;
§2–§3 cover the layout if you do it by hand.

## 1. Get the binary

Download the `windows-amd64` (or `windows-arm64`) archive from the
[Releases page][releases], extract it, and copy `eneverre.exe` somewhere
stable, e.g. `C:\Program Files\Eneverre\eneverre.exe`.

Or build it yourself with a recent Go toolchain (1.22+) — on Windows or by
cross-compiling from Linux/macOS:

```powershell
# On Windows (from the repo root)
go -C go build -o ..\eneverre.exe .
```

```bash
# Cross-compile from Linux/macOS
GOOS=windows GOARCH=amd64 make build   # produces eneverre.exe
```

Verify it runs:

```powershell
C:\Program Files\Eneverre\eneverre.exe --version   # eneverre-api <version>
```

[releases]: https://github.com/matiasdelellis/eneverre-server/releases

## 2. Lay out config and data

Pick a data root — `C:\ProgramData\Eneverre` is the conventional place for
per-machine service data. Create it with the config file and an (empty)
cameras folder:

```
C:\ProgramData\Eneverre\
├─ eneverre.ini          ← copy from doc\example\eneverre.ini (see note below)
├─ cameras.d\            ← one .ini per camera (seeded into the DB on first run)
├─ eneverre.db           ← created automatically
├─ eneverre.log          ← service log (see §3)
└─ recordings\           ← created automatically (recording is on by default)
```

The config file itself is optional — with none present Eneverre runs on
built-in defaults. **But the service registration below passes an explicit
`--config C:\ProgramData\Eneverre\eneverre.ini`, and an explicit path that
doesn't exist is fatal** (`config file not found: ...`), so for the documented
service layout `eneverre.ini` must be in place before the first start. (A bare
`eneverre.exe` with no `--config` would instead fall back to defaults.) The
database file, `cameras.d\` and the recordings/cache directories are created
for you.

Copy the sample config and edit it:

```powershell
mkdir C:\ProgramData\Eneverre\cameras.d
copy doc\example\eneverre.ini      C:\ProgramData\Eneverre\eneverre.ini
copy doc\example\cameras.d\*.ini   C:\ProgramData\Eneverre\cameras.d\
```

> **Recording path.** Recording is on by default. When `record_dir` is unset,
> Eneverre uses `%ProgramData%\Eneverre\recordings` if that folder already
> exists, and otherwise falls back to `<DataDir>\recordings` — so a custom
> `-DataDir` install keeps its recordings alongside its config and DB. Set
> `record_dir` explicitly to put them elsewhere (e.g. on another drive), or
> `record = false` to run live-only:
>
> ```ini
> [media]
> record_dir = D:\CamData\recordings
> ```

## 3. Tell Eneverre where things live

With the default `%ProgramData%\Eneverre\` layout these are all optional. Set
them only to use a different location — as flags **or** the matching
`ENEVERRE_*` environment variables (flags win). The installer and the manual
service recipe below pass the paths explicitly so the service is independent
of the working directory and of a custom `-DataDir`:

| Flag / env var                                   | Value                                   |
| ------------------------------------------------ | --------------------------------------- |
| `--config` / `ENEVERRE_CONFIG_PATH`              | `C:\ProgramData\Eneverre\eneverre.ini`  |
| `--cameras-dir` / `ENEVERRE_CAMERAS_DIR`         | `C:\ProgramData\Eneverre\cameras.d`     |
| `--db` / `ENEVERRE_DB_PATH`                       | `C:\ProgramData\Eneverre\eneverre.db`   |
| `--log-level` / `ENEVERRE_LOG_LEVEL`             | `info` (or `debug`, `warn`, `error`)    |
| `ENEVERRE_LOG_FILE` *(env only)*                 | `C:\ProgramData\Eneverre\eneverre.log`  |

`ENEVERRE_LOG_FILE` matters for the service: a service started by the SCM has
no console, so without it the log — including the one-time first-run admin
password — would be discarded. When it's set, Eneverre appends its log there
while running as a service (running from a console, it still logs to the
terminal).

### First-run admin password

On the first start (empty users table) Eneverre creates an `admin` user with
a **random** password and logs it once. Read it from `eneverre.log`, then log
in at <http://localhost:8080/>: the admin is required to set a new password on
that first login before the app opens, so the logged password is used only
once. To pick the bootstrap password yourself, set `ENEVERRE_ADMIN_USER` /
`ENEVERRE_ADMIN_PASS` before the first start (the install script's
`-AdminPassword` does this); they're honored only while the users table is
empty, and that admin is still prompted to change it on first login.

## 4. Quick install with `install.ps1`

[`scripts\install.ps1`](../scripts/install.ps1) automates download → verify →
install and, with `-InstallService`, registers the native Windows service and
seeds the config. Run it from an **elevated** PowerShell:

```powershell
# Install the latest release + register and start the service
.\install.ps1 -InstallService

# Install a specific version (a bare number is accepted: 1.0.0 -> v1.0.0)
.\install.ps1 -InstallService -Version v1.0.0

# Set a known admin password instead of the generated one
.\install.ps1 -InstallService -AdminPassword 'your-secret'

# Update the binary only (re-run; restart the service to apply)
.\install.ps1

# See what would happen without writing anything
.\install.ps1 -InstallService -DryRun

# Remove the service and the binary (keeps config + data)
.\install.ps1 -Uninstall
```

| Flag                    | Effect |
| ----------------------- | ------ |
| `-Version <tag>`        | Install a specific release instead of the latest. |
| `-List`                 | Print the last few release tags and exit. |
| `-TargetDir <dir>`      | Install the binary elsewhere (default `C:\Program Files\Eneverre`). |
| `-DataDir <dir>`        | Config + database + logs root (default `C:\ProgramData\Eneverre`). |
| `-InstallService`       | Register the service via `New-Service`, seed config, start it. Needs elevation; preserves an existing service unless `-Force`. |
| `-AdminPassword <pw>`   | Provision a known admin password (sets `ENEVERRE_ADMIN_*` for the service). |
| `-Force`                | Recreate an existing service when `-InstallService` is given. |
| `-NoVerify`             | Skip the SHA-256 check (not recommended). |
| `-DryRun`               | Show every step without writing anything. |
| `-Uninstall`            | Stop + delete the service and remove the binary. |
| `-Yes`                  | Skip the uninstall confirmation prompt. |
| `-Help`                 | Show detailed help. |

Existing `eneverre.ini` and `cameras.d\` are **never overwritten**, and
`-Uninstall` leaves the whole data directory in place — delete it by hand for
a fully clean uninstall.

## 5. Manual service setup (`sc.exe` / `New-Service`)

If you'd rather not use the script, register the service yourself. Because
`eneverre.exe` is service-aware, the built-in tooling works directly. From an
**elevated** PowerShell:

```powershell
$bin  = 'C:\Program Files\Eneverre\eneverre.exe'
$data = 'C:\ProgramData\Eneverre'

# Absolute paths on the command line so the service starts regardless of its
# working directory.
$binPath = '"{0}" --config "{1}\eneverre.ini" --cameras-dir "{1}\cameras.d" --db "{1}\eneverre.db" --log-level info' -f $bin, $data

New-Service -Name Eneverre -BinaryPathName $binPath `
  -DisplayName 'Eneverre NVR API' -StartupType Automatic `
  -Description 'Vendor-agnostic NVR - records, relays and serves camera streams.'

# The log file is env-only: the SCM injects per-service vars from the
# service key's Environment value (REG_MULTI_SZ).
New-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Services\Eneverre' `
  -Name Environment -PropertyType MultiString `
  -Value @("ENEVERRE_LOG_FILE=$data\eneverre.log") -Force

# Restart on failure (optional), like systemd Restart=on-failure
sc.exe failure Eneverre reset= 86400 actions= restart/2000/restart/2000/restart/2000
sc.exe failureflag Eneverre 1

Start-Service Eneverre
Get-Service  Eneverre
```

Manage it like any service — a **Stop** finalizes the current recording
segment before exiting:

```powershell
Restart-Service Eneverre
Stop-Service    Eneverre
sc.exe delete   Eneverre     # uninstall (leaves config + data in place)
```

Read the generated admin password once the service has started:

```powershell
Select-String -Path C:\ProgramData\Eneverre\eneverre.log -Pattern 'generated password'
```

> **`cmd.exe` variant.** `sc.exe create` uses `key= value` pairs (note the
> space) and needs the inner quotes escaped with `\`:
>
> ```bat
> sc create Eneverre binPath= "\"C:\Program Files\Eneverre\eneverre.exe\" --config \"C:\ProgramData\Eneverre\eneverre.ini\" --cameras-dir \"C:\ProgramData\Eneverre\cameras.d\" --db \"C:\ProgramData\Eneverre\eneverre.db\" --log-level info" start= auto DisplayName= "Eneverre NVR API"
> ```

## 6. Open the firewall

The web UI / API listens on TCP **8080** by default and the RTSP relay on
TCP **8554** (used by the apps for live/playback; it does not go through a
reverse proxy). Allow them if clients are on other machines:

```powershell
New-NetFirewallRule -DisplayName "Eneverre API"  -Direction Inbound -Protocol TCP -LocalPort 8080 -Action Allow
New-NetFirewallRule -DisplayName "Eneverre RTSP" -Direction Inbound -Protocol TCP -LocalPort 8554 -Action Allow
```

Change the API port with `[server] port` in `eneverre.ini` (or `--port`).

## Uninstall

Stop and remove the service, then delete the folders by hand if you want a
clean slate (they are left in place so an accidental removal doesn't wipe
your recordings):

```powershell
.\install.ps1 -Uninstall          # or, by hand:
Stop-Service Eneverre; sc.exe delete Eneverre
Remove-Item -Recurse -Force "C:\Program Files\Eneverre"
# Optional, destroys config + database + recordings:
Remove-Item -Recurse -Force C:\ProgramData\Eneverre
```
