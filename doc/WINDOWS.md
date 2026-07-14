# Windows install & service

Eneverre runs on Windows exactly like on Linux: it's a single static Go
binary (`eneverre.exe`) with the media engine, SQLite (pure-Go
`modernc.org/sqlite`) and the web UI all embedded. There is **no external
dependency to install** — no MediaMTX, no runtime, and ffmpeg is optional
(only used to pre-render the privacy black frame; without it a text
placeholder is used).

Two Windows-specific things to get right, both covered below:

1. The built-in default search paths are Unix-style (`/etc/eneverre`,
   `/var/lib/eneverre`, …), which don't exist on Windows. Point Eneverre at
   Windows paths with a handful of `ENEVERRE_*` environment variables.
2. Eneverre finalizes the in-progress recording segment on a clean stop
   (it shuts down on `Ctrl+C` / `SIGINT`). Run it under a service wrapper
   that **sends `Ctrl+C` on stop** — [NSSM](#option-a-nssm-recommended) does
   this by default — rather than one that hard-kills the process and drops
   the last segment.

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
├─ eneverre.ini          ← required; copy from doc\example\eneverre.ini
├─ cameras.d\            ← one .ini per camera (seeded into the DB on first run)
├─ eneverre.db           ← created automatically
└─ recordings\           ← created automatically when [media] record = true
```

Only `eneverre.ini` **must** exist before the first start — Eneverre exits
with `missing eneverre.ini` if it can't find one. The database file,
`cameras.d\` and the recordings/cache directories are created for you.

Copy the sample config and edit it:

```powershell
mkdir C:\ProgramData\Eneverre\cameras.d
copy doc\example\eneverre.ini      C:\ProgramData\Eneverre\eneverre.ini
copy doc\example\cameras.d\*.ini   C:\ProgramData\Eneverre\cameras.d\
```

> **Recording path.** If you enable recording (`[media] record = true`),
> set `record_dir` in `eneverre.ini` to a real Windows path — the built-in
> default `/var/lib/eneverre/recordings` would otherwise land at
> `C:\var\lib\eneverre\recordings` on the current drive. For example:
>
> ```ini
> [media]
> record     = true
> record_dir = C:\ProgramData\Eneverre\recordings
> ```

## 3. Tell Eneverre where things live

Because the default search paths are Unix-style, set these environment
variables so Eneverre finds the Windows layout regardless of the working
directory. They mirror the `Environment=` lines in the Linux `systemd`
unit:

| Variable               | Value                                        |
| ---------------------- | -------------------------------------------- |
| `ENEVERRE_CONFIG_PATH` | `C:\ProgramData\Eneverre\eneverre.ini`       |
| `ENEVERRE_CAMERAS_DIR` | `C:\ProgramData\Eneverre\cameras.d`          |
| `ENEVERRE_DB_PATH`     | `C:\ProgramData\Eneverre\eneverre.db`        |
| `ENEVERRE_LOG_LEVEL`   | `info` (or `debug`, `warn`, `error`)         |

The same values can also be passed as flags (`--config`, `--cameras-dir`,
`--db`, `--log-level`); the service setup below uses the environment
variables so one place holds the whole layout.

### First-run admin password

On the first start (empty users table) Eneverre creates an `admin` user
with a **random** password and logs it once. Read it from the service log
(see below), then log in at <http://localhost:8080/> and change it. To pick
the password yourself, set `ENEVERRE_ADMIN_USER` / `ENEVERRE_ADMIN_PASS`
before the first start; they're honored only while the users table is empty.

## 4. Run it as a Windows service

### Option A — NSSM (recommended)

[NSSM] wraps a console program as a proper Windows service and, crucially,
sends `Ctrl+C` on **Stop** — which triggers Eneverre's graceful shutdown so
the last recording segment is finalized instead of dropped.

Download `nssm.exe`, then from an **elevated** (Administrator) PowerShell or
`cmd`:

```powershell
# Install the service (opens the NSSM GUI, or use the flags below)
nssm install Eneverre "C:\Program Files\Eneverre\eneverre.exe"

# Working directory (used for any relative paths and for the SQLite WAL)
nssm set Eneverre AppDirectory "C:\ProgramData\Eneverre"

# Environment (one KEY=VALUE per line; AppEnvironmentExtra appends to the
# system environment)
nssm set Eneverre AppEnvironmentExtra ^
  ENEVERRE_CONFIG_PATH=C:\ProgramData\Eneverre\eneverre.ini ^
  ENEVERRE_CAMERAS_DIR=C:\ProgramData\Eneverre\cameras.d ^
  ENEVERRE_DB_PATH=C:\ProgramData\Eneverre\eneverre.db ^
  ENEVERRE_LOG_LEVEL=info

# Restart on failure, like systemd Restart=on-failure
nssm set Eneverre AppExit Default Restart
nssm set Eneverre AppRestartDelay 2000

# Capture the log (admin password + access log land here)
nssm set Eneverre AppStdout "C:\ProgramData\Eneverre\eneverre.log"
nssm set Eneverre AppStderr "C:\ProgramData\Eneverre\eneverre.log"

# Start it
nssm start Eneverre
```

NSSM's default shutdown method already tries `Ctrl+C` (`Console`) first, so
no extra configuration is needed for a clean stop. If you tighten it, keep
`Console` enabled and give it a few seconds:

```powershell
nssm set Eneverre AppStopMethodConsole 5000
```

Read the generated admin password:

```powershell
Select-String -Path C:\ProgramData\Eneverre\eneverre.log -Pattern "generated password"
```

Manage it like any service:

```powershell
nssm restart Eneverre
nssm stop Eneverre
nssm remove Eneverre confirm      # uninstall (leaves config + data in place)
```

[NSSM]: https://nssm.cc/

### Option B — WinSW

[WinSW] is an alternative wrapper driven by an XML file. Place
`eneverre-service.exe` (a renamed copy of `WinSW.exe`) next to a
`eneverre-service.xml`:

```xml
<service>
  <id>Eneverre</id>
  <name>Eneverre NVR API</name>
  <description>Vendor-agnostic NVR — records, relays and serves camera streams.</description>
  <executable>C:\Program Files\Eneverre\eneverre.exe</executable>
  <workingdirectory>C:\ProgramData\Eneverre</workingdirectory>
  <env name="ENEVERRE_CONFIG_PATH" value="C:\ProgramData\Eneverre\eneverre.ini"/>
  <env name="ENEVERRE_CAMERAS_DIR" value="C:\ProgramData\Eneverre\cameras.d"/>
  <env name="ENEVERRE_DB_PATH"     value="C:\ProgramData\Eneverre\eneverre.db"/>
  <env name="ENEVERRE_LOG_LEVEL"   value="info"/>
  <!-- Send Ctrl+C on stop so the in-progress recording segment is finalized -->
  <stopmode>ctrlc</stopmode>
  <onfailure action="restart" delay="2 sec"/>
  <log mode="roll-by-size"/>
</service>
```

```powershell
.\eneverre-service.exe install
.\eneverre-service.exe start
```

[WinSW]: https://github.com/winsw/winsw

### A note on `sc.exe` / `New-Service`

You *can* register the bare `eneverre.exe` with `sc.exe create` or
PowerShell's `New-Service`, and it will start. Avoid it for a recording
deployment: Windows stops such a service with `TerminateProcess` (a hard
kill), so Eneverre never runs its graceful shutdown and the last, still-open
recording segment is lost. Use NSSM or WinSW, which stop it with `Ctrl+C`.

## 5. Open the firewall

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
nssm stop Eneverre
nssm remove Eneverre confirm
# Optional, destroys config + database + recordings:
Remove-Item -Recurse -Force C:\ProgramData\Eneverre
Remove-Item -Recurse -Force "C:\Program Files\Eneverre"
```
