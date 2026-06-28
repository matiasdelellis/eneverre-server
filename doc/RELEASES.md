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
| `doc/example/`        | Sample `eneverre.ini`, camera INI files, `mediamtx.yml`, `Caddyfile`, and `eneverre.service` unit. |

## Verifying a download

```bash
tar -xzf eneverre-0.2.0-linux-amd64.tar.gz
cd eneverre-0.2.0-linux-amd64
sha256sum -c eneverre-0.2.0-linux-amd64.tar.gz.sha256  # downloaded separately
./eneverre --version                                      # eneverre-api 0.2.0
```

For a deployable install, follow the systemd steps in
[`doc/example/README.md`](example/README.md).

## Local reproduction

The same pipeline is driven by the `Makefile`. The release job is
`make release`; the per-target helper is `make dist-tar dist-checksums
GOOS=<os> GOARCH=<arch> VERSION=<tag>`:

```bash
go -C go test ./...
make dist-tar dist-checksums GOOS=linux GOARCH=amd64 VERSION=v0.2.0
ls -lh dist/  # eneverre-v0.2.0-linux-amd64.tar.gz + .sha256
```
