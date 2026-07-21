# Releases

## Cutting a release

Tag the commit you want to ship with a `v`-prefixed tag and push it:

```bash
git tag v0.2.0
git push origin v0.2.0
```

The [`.github/workflows/release.yml`](../.github/workflows/release.yml) job
runs the test suite and a per-platform build matrix in parallel, then
publishes a GitHub release tagged `v0.2.0` with one tarball per supported
platform and a sha256 sum for each.

The `version` string baked into the binary is the tag itself (via
`git describe --tags --always --dirty` in the `Makefile`). A pre-release
like `v0.2.0-rc1` works the same way; the workflow does not special-case
them — GitHub's release UI lets you mark it as a pre-release if you want
it to stay out of the "Latest" badge.

To re-publish the artifacts of an existing tag (e.g. after a CI failure),
open the **Actions** tab, select **release**, and click **Run workflow**.
The optional `tag` input defaults to the current ref; set it explicitly to
re-run a specific tag.

## Supported platforms

Tarballs are produced for:

| OS      | Architectures              |
| ------- | -------------------------- |
| linux   | amd64, arm64, arm          |
| darwin  | amd64, arm64               |
| windows | amd64, arm64               |

Each tarball is named `eneverre-<version>-<goos>-<goarch>.tar.gz`, with
`<version>` coming from `git describe`. A matching `<name>.tar.gz.sha256`
is uploaded next to it.

## What's in a tarball

| Path                  | Notes                                                                             |
| --------------------- | --------------------------------------------------------------------------------- |
| `eneverre`            | Static binary (CGO disabled, `-trimpath`, stripped with `-ldflags="-s -w"`).      |
| `README.md`           | Top-level README.                                                                 |
| `GO.md`               | `go/README.md` — Go-specific layout, config keys, operational notes.              |
| `openapi.yaml`        | Machine-readable API spec for clients.                                            |
| `doc/example/`        | Sample `eneverre.ini`, camera INI files, `Caddyfile`, and `eneverre.service` unit. |

## Verifying a download

```bash
tar -xzf eneverre-0.2.0-linux-amd64.tar.gz
cd eneverre-0.2.0-linux-amd64
sha256sum -c eneverre-0.2.0-linux-amd64.tar.gz.sha256  # downloaded separately
./eneverre --version                                      # eneverre-api 0.2.0
```

For a deployable install, follow the systemd steps in
[`doc/example/README.md`](example/README.md), or use the install script
below.

## Installing with `install.sh`

[`scripts/install.sh`](../scripts/install.sh) automates the download →
verify → install flow above for Linux and macOS. **Windows** has its own
[`scripts/install.ps1`](../scripts/install.ps1) (download → verify → install
plus an optional native Windows service) — see
[`doc/WINDOWS.md`](WINDOWS.md). The rest of this section is the Linux/macOS
script. It fetches the correct tarball for the host's OS/arch from
the GitHub Releases of `matiasdelellis/eneverre-server`, checks its
SHA-256, and installs the binary — replacing an existing one **atomically**
(staged next to the target, then `rename(2)`d over it), so an update is
safe even while the service is running.

It needs only `bash`, `curl` and `tar`; `sha256sum` (or `shasum` on
macOS) is used for the default verification.

### Common invocations

```bash
# Install the latest release to /usr/local/bin
curl -fsSL https://raw.githubusercontent.com/matiasdelellis/eneverre-server/main/scripts/install.sh | sudo bash

# Same, but from a checked-out copy
sudo bash scripts/install.sh

# Update to the latest release (just run it again)
sudo bash install.sh

# Install a specific version (a bare number is accepted: 1.0.0 -> v1.0.0)
sudo bash install.sh --version v1.0.0

# Install and wire up the systemd service in one step (Linux only)
sudo bash install.sh --install-service

# See what would happen without writing anything
sudo bash install.sh --dry-run

# Remove the binary, unit, and stop the service (keeps config + state)
sudo bash install.sh --uninstall
```

### Flags

| Flag | Effect |
| ---- | ------ |
| `--version <tag>`   | Install a specific release instead of the latest. A leading `v` is added if omitted. |
| `--list`            | Print the last few release tags and exit. |
| `--target-dir <dir>`| Install the binary somewhere other than `/usr/local/bin`. |
| `--install-service` | Also install `/etc/systemd/system/eneverre.service`, then `enable` + `start` it. Linux only, needs root. Preserves an existing unit unless `--force`. |
| `--force`           | Overwrite an existing systemd unit when `--install-service` is given. |
| `--no-verify`       | Skip the SHA-256 check (not recommended). |
| `--dry-run`         | Show every step without writing anything. Works with `--uninstall` too. |
| `--uninstall`       | Stop + disable the service, remove the unit and the binary. |
| `-y`, `--yes`       | Skip the uninstall confirmation prompt (for non-interactive runs). |
| `-h`, `--help`      | Show usage. |

### Notes

* **Permissions.** The script writes to `/usr/local/bin` and (with
  `--install-service`) `/etc/systemd/system` and `/etc/eneverre`, so it
  normally needs `sudo`. It fails fast with an actionable message if it
  can't write.
* **Config seeding.** On first `--install-service`, the script creates
  `/etc/eneverre/` with the example `eneverre.ini` and an empty
  `cameras.d/`. Neither is required for the service to start — the unit runs
  `eneverre` with no explicit `--config`, so a missing file just means
  built-in defaults and a missing `cameras.d/` just means no seed — but the
  script seeds them so you have a place to configure. Add your cameras under
  `/etc/eneverre/cameras.d/` and `systemctl restart eneverre`.
* **Admin password.** On a first install the service creates an `admin`
  user with a random password and logs it once; the script prints it
  after starting the service (or read it later with
  `journalctl -u eneverre | grep 'generated password'`). Log in at the web
  UI — the admin is required to set a new password on that first login before
  the app opens. To pick the bootstrap password yourself, set
  `ENEVERRE_ADMIN_PASS` before the first start (e.g. via
  `systemctl edit eneverre`); that admin is still prompted to change it.
* **Existing config is never overwritten.** Re-running `--install-service`
  keeps your `eneverre.ini` and `cameras.d/` as they are, and `--uninstall`
  leaves `/etc/eneverre/` (config) and `/var/lib/eneverre/` (state) in
  place — remove them by hand for a fully clean uninstall.
* **Updating a running service.** Re-running the script replaces the
  binary atomically but does **not** restart the service unless you pass
  `--install-service`; it prints a reminder to
  `sudo systemctl restart eneverre`.
* **Piping into `sudo bash`** trusts the script served over TLS by
  GitHub. If you prefer, download it first, read it, then run it.

## Local reproduction

The same pipeline is driven by the `Makefile`. The release job is
`make release`; the per-target helper is `make dist-tar dist-checksums
GOOS=<os> GOARCH=<arch> VERSION=<tag>`:

```bash
go -C go test ./...
make dist-tar dist-checksums GOOS=linux GOARCH=amd64 VERSION=v0.2.0
ls -lh dist/  # eneverre-v0.2.0-linux-amd64.tar.gz + .sha256
```
