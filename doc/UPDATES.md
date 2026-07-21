# Eneverre API — Auto-update protocol

This document is the contract any client app follows when it calls
`eneverre-api` to check for and download a new build. The server-side is
implemented in `go/internal/updates/` and
`go/internal/server/handlers_updates.go`. The `OpenAPI 3` description in
`doc/openapi.yaml` is the machine-readable version of the same contract.

Every client lives on its own **track** — an arbitrary identifier the
operator/CI chooses (e.g. `tv`, `phone`, `tablet`, `ios`, `web`) — so
different device classes or platforms can be released on independent
cadences and `versionCode` spaces. **The server has no fixed track list or
allowlist.** A track exists the moment something is published to it; there
is no config change, restart, or code change needed to add a new one.

## Endpoints

| Method | Path                                  | Auth   | Purpose                                |
| ------ | -------------------------------------- | ------ | --------------------------------------- |
| GET    | `/api/app/{track}/update`              | none   | Latest release for `{track}`            |
| POST   | `/api/admin/app/updates/{track}`       | admin  | Publish a new release to `{track}`      |
| GET    | `/api/app/updates/{track}/{filename}`  | none   | Download a build file from `{track}`    |

`{track}` may be any lowercase alphanumeric string (`-`/`_` allowed, up to
40 chars, e.g. `tv`, `phone`, `tablet`, `ios`, `web-beta`). Publishing to a
track name that has never been used before is enough to start serving it —
there is no registration step.

## Server configuration

The auto-update feature is **off by default**. It enables when the server
has a storage directory configured, via any of (precedence high → low):

1. `[updates] storage_dir` in `eneverre.ini`
2. `ENEVERRE_UPDATES_DIR` environment variable
3. `<cameras_dir>/../app-updates` (the directory next to `cameras.d`)

When none of those resolves, every endpoint above returns **`503 Service
Unavailable`** with body `{"detail": "Auto-update is not configured on this server"}`.
Clients should treat 503 the same as 204 (no update). This is the *only*
way the feature is gated — there is no separate on/off switch or allowlist
per track.

### Setting the public base URL (recommended)

**`[updates] public_base_url`** is the **recommended** way to tell the
server what absolute URL the `builds[i].url` field in the manifest should
use. Set it whenever the API is reachable from clients under a public
hostname (which is always the case in production — including behind a
reverse proxy like Caddy or Nginx).

```ini
[updates]
public_base_url = https://nvr.example.com
```

Or via the env var (handy for containerized deploys):

```bash
export ENEVERRE_UPDATES_PUBLIC_BASE_URL=https://nvr.example.com
```

Each `builds[i].url` becomes
`<public_base_url>/api/app/updates/<track>/<filename>`. **This is the
operator's authoritative source of truth** — the server does not need
to guess the scheme or host from the request, and it works identically
behind any reverse proxy, with or without `X-Forwarded-Proto` /
`X-Forwarded-Host`.

#### Auto-detection fallback (when `public_base_url` is empty)

If the operator does not set `public_base_url`, the server falls back to
building the URL from the incoming request:

1. Scheme: `X-Forwarded-Proto` header → `r.TLS != nil` → `http` (last resort).
2. Host: `X-Forwarded-Host` header → `r.Host`.

This works out of the box if the reverse proxy sends those headers.
**Caddy does not send them by default** — the operator must add to the
`reverse_proxy` block, e.g.:

```caddyfile
nvr.example.com {
    reverse_proxy localhost:8080 {
        header_up X-Forwarded-Proto https
        header_up X-Forwarded-Host {host}
    }
}
```

If neither `public_base_url` nor the proxy headers are set, the
manifest will contain `http://` URLs even when the site is HTTPS —
the downloads still work because Caddy redirects HTTP→HTTPS with one
hop, but it is wasted bandwidth. **Setting `public_base_url` is the
simplest fix and the recommended default.**

## On-disk layout

The storage directory contains one subdirectory per track, created on
first publish. Disk usage is **bounded by the current release's build
files** (a few hundred MB) — old ones are rotated out on every commit so
the directory does not grow without bound. No release history is kept:
the on-disk `manifest.json` is the only record.

```
${storage_dir}/
└── tv/
    ├── manifest.json                       # the current release
    ├── eneverre-tv-arm64-1.0.1.apk         # current
    ├── eneverre-tv-universal-1.0.1.apk     # current
    └── pending.json                        # only present while a multi-POST
                                             # publish is in progress
```

`manifest.json` has the following JSON shape (the `Manifest` type):

```json
{
  "versionName": "1.0.1",
  "versionCode": 10101,
  "mandatory": false,
  "releaseNotes": "Bug fixes and stability improvements.",
  "uploadedAt": "2026-06-28T15:00:00Z",
  "builds": [
    {"variant": "arm64-v8a", "filename": "eneverre-tv-arm64-1.0.1.apk", "size": 12345678, "sha256": "..."},
    {"variant": "universal", "filename": "eneverre-tv-universal-1.0.1.apk", "size": 15678901, "sha256": "..."}
  ]
}
```

### Rotation

Every time a release is committed, the server:

1. Replaces `manifest.json` with the new release.
2. Deletes every file in the track directory that is not a sidecar
   (`manifest.json`, `pending.json`, or an in-progress `.tmp` write) and
   is not in the new release's build list.

Result: the directory holds **only the current release's build files**.
No history is kept. The previous release's files are deleted and are
**not** addressable via the download endpoint (they return 404), so an
in-flight download of the previous release will fail. If you need true
in-flight support, use the multi-POST `finalize=false` flow so a single
release is built up across many requests before any old file is deleted.

Because builds can now have any extension (or none), the server can no
longer tell an "orphaned build" from "an unrelated stray file" by name —
anything in the track directory that isn't a sidecar and isn't referenced
by the current manifest gets cleaned up at commit time. Don't drop
unrelated files into a track's storage directory.

The server is the only writer. The on-disk manifest is the source of
truth — there is no database row mirroring it.

## `GET /api/app/{track}/update`

### Response — update available (multi-variant)

```
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: no-store
```

```json
{
  "versionName": "1.0.1",
  "versionCode": 10101,
  "mandatory": false,
  "releaseNotes": "Bug fixes and stability improvements.",
  "builds": [
    {
      "variant": "arm64-v8a",
      "filename": "eneverre-tv-arm64-1.0.1.apk",
      "size": 12345678,
      "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
      "contentType": "application/vnd.android.package-archive",
      "url": "https://updates.example.com/api/app/updates/tv/eneverre-tv-arm64-1.0.1.apk"
    },
    {
      "variant": "armeabi-v7a",
      "filename": "eneverre-tv-armv7-1.0.1.apk",
      "size": 10234567,
      "sha256": "...",
      "url": "https://updates.example.com/api/app/updates/tv/eneverre-tv-armv7-1.0.1.apk"
    },
    {
      "variant": "universal",
      "filename": "eneverre-tv-universal-1.0.1.apk",
      "size": 15678901,
      "sha256": "...",
      "url": "https://updates.example.com/api/app/updates/tv/eneverre-tv-universal-1.0.1.apk"
    }
  ]
}
```

#### Field-by-field

| Field                    | Type    | Required | Meaning                                                                |
| ------------------------ | ------- | -------- | ------------------------------------------------------------------------ |
| `versionCode`            | integer | yes      | Strictly increasing build number. Client upgrades iff `> its own version code` |
| `versionName`            | string  | yes      | Human-readable version (e.g. `1.0.1`).                                  |
| `mandatory`              | boolean | no       | If `true`, dialog has only *Update*; the app force-closes if the installer is dismissed. |
| `releaseNotes`           | string  | no       | Shown in the update dialog. Empty / omitted → no body.                |
| `builds`                 | array   | yes      | One entry per build variant in the release. Order = CI publish order.   |
| `builds[].variant`       | string  | yes      | Client-defined tag identifying the build (e.g. an Android ABI, or `ios`, `web`). Opaque to the server. |
| `builds[].filename`      | string  | yes      | Basename of the build file as stored on the server.                    |
| `builds[].size`          | integer | yes      | Size of the file in bytes.                                              |
| `builds[].sha256`        | string  | yes      | Lowercase hex SHA-256 of the file's bytes. Client must verify after download. |
| `builds[].contentType`   | string  | no       | The Content-Type the uploader declared for this file, if any. Omitted if unset. |
| `builds[].url`           | string  | yes      | Absolute URL the client GETs to download the file.                     |

#### Client selection algorithm

The client must pick exactly one build from `builds`, using whatever
variant-matching makes sense for its platform. For Android clients
matching against ABIs:

1. Walk `Build.SUPPORTED_ABIS` in order (the device's preferred ABIs).
2. For each, find a `builds[i].variant` matching it exactly. Take the first match.
3. If no device ABI matched, look for `variant == "universal"`.
4. If no universal either, take the first build in the array (last-resort
   fallback; only relevant if the operator published a single non-universal
   build by mistake).

Non-Android tracks are free to define their own variant-matching rule
(e.g. a single `variant == "ios"` or `variant == "web"` build per release).

#### Other semantics

* If `versionCode` is not greater than the client's own build number (or
  the manifest fails to parse), the client treats the response as "no
  update".
* If the version is one the user previously chose to *Skip*, the prompt is
  suppressed **unless** `mandatory == true`.
* If `mandatory == true`, the *Later* and *Skip* buttons are removed, the
  dialog is non-cancelable, and dismissing the installer causes the app to
  close (Android: `finish()`).

### Response — no update available

```
HTTP/1.1 204 No Content
```

Empty body. The client treats this as "up to date". A 204 is **not** the
response when the server is misconfigured — in that case the endpoint
returns 503. Clients should treat 204 as "definitely no update" and 503
as "skip this check, try again next launch" (server is having a bad day).

### Other status codes

| Status | Meaning | Client behavior |
| ------ | ------- | --------------- |
| 200    | Update available, parse the body. | Standard flow. |
| 204    | No update. | Skip. |
| 400    | Track name failed validation (see [Endpoints](#endpoints)). | Fix the track name in the client build; this is a client bug, not a transient condition. |
| 503    | Feature disabled on the server. | Skip silently (do not surface an error to the user). |
| Any other 4xx/5xx | Transient error. | Skip silently, treat as up-to-date, retry on next cold start. |

## `POST /api/admin/app/updates/{track}` (publish)

Multipart form body. The response is `200 OK` with `{"ok": true, ...}` on
success, or a `4xx/5xx` with `{"detail": "..."}` on failure. **Any non-2xx
fails the CI build.**

### Auth

The publish endpoint accepts **only** `Authorization: Bearer <token>` where
`<token>` matches the server's `[updates] publish_token` (or
`ENEVERRE_UPDATES_PUBLISH_TOKEN` env var). User/password and session Bearer
tokens are **not** accepted. If the token is not configured on the server,
the endpoint returns **`503 Service Unavailable`** — there is no admin
fallback, by design, so a misconfigured deploy never silently grants publish
access through user credentials. The same token authorizes publishing to
*any* track — there is no per-track credential.

Generate a publish token with `openssl rand -hex 32` (or any 32+ char
high-entropy secret) and put it in the server config as
`[updates] publish_token` or in the env as
`ENEVERRE_UPDATES_PUBLISH_TOKEN`. Never commit it to the repo. Rotating
the token revokes CI access immediately; it does not affect user logins.

### Size and timeout limits

* **Build size cap** (`[updates] max_build_size`, default `100M` — suffix
  syntax `K`/`M`/`G` accepted, base 1024; or a plain decimal byte count
  like `104857600`). Enforced by `http.MaxBytesReader`, so the server returns
  **`413 Request Entity Too Large`** as soon as the body crosses
  the limit and the client can abort the upload. Raise this for
  legitimately large builds.
* **Request read timeout** (`[server] read_timeout`, default `5m`).
  Bounded to defend against slow-trickle DoS, but generous enough for
  a 200 MiB upload over a 5 Mbps link. Raise for slower links.

Each build file is streamed end-to-end: `r.MultipartReader()` walks the
parts without buffering, the small form fields are read into memory (each
capped with `io.LimitReader`), and each `build_<variant>` part is piped
into a temp file as soon as it is encountered. Memory usage is O(1)
regardless of file size. Disk usage briefly doubles during publish
(temp + final) and the temp file is removed after the publish
completes (even on error).

### Body fields

Every artifact in the release must be sent as a form file with name
`build_<variant>`, where `<variant>` is a client-defined string
identifying the build — opaque to the server. For Android tracks this is
typically an ABI:

* `build_arm64-v8a` — most modern ARM devices
* `build_armeabi-v7a` — older 32-bit ARM
* `build_x86_64` — Chromebooks / modern emulators
* `build_x86` — older emulators
* `build_universal` — fat APK; fallback for clients that don't list a specific ABI

Non-Android tracks are free to use any variant name that fits their
platform (e.g. `build_ios`, `build_web`). A single release can carry any
combination of variants (at least one is required). The CI can either
send them all in **one** POST, or send **one POST per variant** (the
`finalize` field controls when the release becomes visible to clients —
see below). Each part's own `Content-Type` header, if sent, is recorded
and served back on download — the server does not assume a fixed archive
format or force an extension.

The other form fields apply to the whole release (not per build):

| Field         | Type    | Required | Notes |
| ------------- | ------- | -------- | ----- |
| `versionName` | string  | yes      | Human-readable, e.g. `1.0.1`. |
| `versionCode` | integer (form string) | yes | Non-negative integer. CI typically computes `MAJOR*10000 + MINOR*100 + PATCH`. |
| `releaseNotes`| string  | no       | Stored verbatim; surfaced to the user in the dialog. |
| `mandatory`   | boolean | no       | Defaults to `false`. Accepts `true`/`false`/`1`/`0`/`yes`/`no` (case-insensitive). |
| `finalize`    | boolean | no       | Whether the release becomes visible to clients after this POST. **Default `true`**: this POST commits the release. **`false`**: stage only — the release stays invisible until a later POST with `finalize=true` (or no `finalize` field) commits. See [Single POST vs multi-POST](#single-post-vs-multi-post--the-finalize-flag) below. |

Unknown form fields are ignored.

### Single POST vs multi-POST — the `finalize` flag

**TL;DR:**
- Default behavior: each POST is self-contained. You POST all your build files in one request and the release goes live immediately. This is the simple case.
- If you set `finalize=false` on a POST, you're saying "don't make the release live yet — I'll send more POSTs". You must send a final POST with `finalize=true` (or just omit the field) to actually publish.
- Within a single release, all POSTs must carry the same `versionCode`. Different `versionCode` = different release = previous in-progress one is discarded.

**Mental model — the in-progress release:**

The server keeps an "in-progress release" keyed by `versionCode` in
`pending.json` (invisible to clients). Each POST either:

| POST does | Visible to clients after the POST? |
|---|---|
| `finalize=true` (or omitted) | **Yes** — the in-progress release becomes the current release. |
| `finalize=false` | **No** — the in-progress release is updated but stays invisible. |

In both cases, the POST's `build_<variant>` files are merged into the
in-progress release. If the in-progress release has the same
`versionCode`, the new files are added to it (or replace existing builds
for the same variant). If the in-progress release has a different
`versionCode`, it is discarded and a new one is started with this POST's
metadata.

**Concrete examples:**

*Single POST (default, one curl, all builds at once):*

```bash
curl --max-time 1800 \
  -H "Authorization: Bearer $UPDATE_TOKEN" \
  -F build_arm64-v8a=@...apk \
  -F build_armeabi-v7a=@...apk \
  -F build_universal=@...apk \
  -F versionName=1.0.1 -F versionCode=10101 \
  -X POST .../api/admin/app/updates/tv
```

→ one POST, the release is live when the curl returns.

*Multi-POST (one build file per POST, useful for large builds on slow links):*

```bash
# First POST: arm64 — stage only, do not commit yet.
curl -F build_arm64-v8a=@...apk -F versionName=1.0.1 -F versionCode=10101 \
     -F finalize=false \
     -X POST .../api/admin/app/updates/tv

# Second POST: armv7 — stage only.
curl -F build_armeabi-v7a=@...apk -F versionName=1.0.1 -F versionCode=10101 \
     -F finalize=false \
     -X POST .../api/admin/app/updates/tv

# Third POST: universal — this one commits (finalize=true or omitted).
curl -F build_universal=@...apk -F versionName=1.0.1 -F versionCode=10101 \
     -X POST .../api/admin/app/updates/tv
```

→ between POST 1 and POST 3, `GET /api/app/tv/update` returns **204
No Content** (no release visible). After POST 3, the GET returns the
full manifest with all 3 variants.

**Publishing to a brand-new track (e.g. tablets) needs no server changes:**

```bash
curl -H "Authorization: Bearer $UPDATE_TOKEN" \
  -F build_universal=@eneverre-tablet-1.0.0.apk \
  -F versionName=1.0.0 -F versionCode=10000 \
  -X POST .../api/admin/app/updates/tablet
```

The `tablet` track did not exist before this call — the storage directory
and manifest are created on the fly, and `GET /api/app/tablet/update`
works immediately afterward.

**Rule of thumb for the CI script:**

```bash
for variant in arm64-v8a armeabi-v7a universal; do
  finalize=false
  [ "$variant" = "universal" ] && finalize=true   # last one commits
  curl -F "build_${variant}=@..." -F versionName=... -F versionCode=... \
       -F finalize=$finalize \
       -X POST .../api/admin/app/updates/tv
done
```

**What if I get it wrong?**

* I forgot `finalize=false` on the first POST (left it default = `true`):
  the first POST commits a release with just the first build. The
  second POST (with the same `versionCode`) starts a new in-progress
  release, commits it with just the second build. The manifest ends
  up with only the LAST published file. The other files are discarded
  (and their files deleted by rotation).
* I sent `finalize=true` on a middle POST: same effect — that POST
  commits whatever's in the in-progress release. Subsequent POSTs
  with the same `versionCode` start a new in-progress release.
* I sent `finalize=false` on the last POST: the release is staged
  but never visible. `GET` returns 204. Send a new POST with
  `finalize=true` to commit.

**Why `finalize=true` is the default:**

Most users publish a release in one POST (all variants in one request, or
just one file). With the default `finalize=true`, the simple case
"just works" with no extra form fields. Multi-POST is opt-in by
adding `finalize=false` to intermediate POSTs.

**For a GitHub Actions workflow**, store `$UPDATE_TOKEN` in a repo
secret named e.g. `UPDATE_PUBLISH_TOKEN` and pass it via
`env: UPDATE_TOKEN: ${{ secrets.UPDATE_PUBLISH_TOKEN }}`. The single
and multi-POST shapes are equivalent on the server side; pick the
multi-POST shape when the total release exceeds the per-request upload
budget.

### Status codes

The response body always carries a `state` field:

* `state: "committed"` — the file is in the current release; the response
  also includes the final `builds` count and the full `variants` list.
* `state: "pending"` — the file is staged in the in-progress release; the
  response includes the current `builds` count, the full `variants` list,
  and a `variants_appended` list of what THIS POST added. Subsequent
  POSTs with the same `versionCode` append more; one with
  `finalize=true` (or default) commits.

| Status | Meaning | CI behavior |
| ------ | ------- | ----------- |
| 200    | Publish succeeded (state=committed or state=pending). | Done. |
| 400    | Track name failed validation. | Fix the track name (lowercase alphanumeric, `-`/`_`, ≤40 chars). |
| 401    | Missing or invalid token. | Check `UPDATE_TOKEN` / header name. |
| 413    | Total body exceeds `[updates] max_build_size` (default 100 MiB). | Raise the cap on the server, or slim the builds (or split into per-variant POSTs). |
| 422    | Body present but malformed (missing `versionName` / `versionCode` / any `build_<variant>`). | Inspect the body. |
| 503    | Server has no `publish_token` configured. | Server-side misconfiguration; publish is disabled. |

## `GET /api/app/updates/{track}/{filename}` (download)

Anonymous. Returns the build file's bytes for any filename that appears
in the current `builds` list. The `Content-Type` sent is, in order: the
value declared at publish time (`builds[].contentType`), else a guess
from the file extension, else `application/octet-stream` — the server
never assumes a fixed archive format. A request for any other filename
returns **404**. The only addressable files are the builds of the current
release; a previous release's files are deleted at commit time, so an
in-flight download of the previous release fails after a republish.

## Caching

The check endpoint sends `Cache-Control: no-store`. Clients must not cache
the manifest response; the server considers each cold-start check
independent.

The download endpoint is a normal static file — clients should use the
`If-None-Match` / `If-Modified-Since` machinery of their HTTP stack as
usual, but the server does not set ETag or Last-Modified (the file is
expected to change rarely and clients always want the freshest bytes after
a manifest hit).

## Security

* **Manifest MITM**: prevented by HTTPS. Clients are expected to hard-code
  (or pin) the API host.
* **Tampered build**: client verifies `sha256` after download. The publish
  endpoint computes the hash server-side from the received bytes, so the
  client can trust the manifest only as much as it trusts the TLS
  connection.
* **Forged APK update** (Android tracks): the package manager refuses to
  install an APK whose signature differs from the installed app's. As long
  as the signing key in the CI is stable, a malicious APK that somehow
  bypassed the SHA-256 check still cannot replace the installed app.
* **No auth on the check**: by design. The manifest only reveals version
  metadata; the download endpoint serves only the file the operator chose
  to publish. To lock down the download, put the API behind auth and have
  the client send the Bearer token (the `requireUser` gate is not currently
  on the download — easy to add if needed).
* **No auth on the publish (recommended)**: it is gated by the configured
  `[updates] publish_token` (Bearer), or by `requireAdmin` (Basic or
  session Bearer) when the token is not configured. Prefer the token:
  it is dedicated, can be rotated without affecting user accounts, and a
  stolen session token cannot publish.
* **Track names**: validated against a fixed pattern (lowercase
  alphanumeric, `-`/`_`, ≤40 chars) purely to keep them safe as a
  subdirectory name — this is not an authorization boundary. Anyone with a
  valid `publish_token` can publish to any track name.

## Client checklist (what to implement)

* [ ] On cold start, in parallel with the auth flow, GET
      `/api/app/<my-track>/update`. The track is **baked into the client
      build** (e.g. TV builds use `tv`, phone builds use `phone`, a tablet
      build might use `tablet`). It is **not** autodetected.
* [ ] If response is 204, 503, or any non-2xx: do nothing, continue normal
      flow.
* [ ] If response is 200 and `versionCode` is newer than the client's own
      build number: show the update dialog. `mandatory=true` removes
      *Later* / *Skip*.
* [ ] Pick the right build from `builds` using the algorithm above
      (Android: walk `Build.SUPPORTED_ABIS`, fall back to `universal`,
      then the first element; other platforms: whatever variant-matching
      makes sense).
* [ ] On user accept: GET `build.url`. Save the bytes to a private cache
      dir. Compute SHA-256 and compare to `build.sha256`. On mismatch,
      abort (toast "integrity check failed") and behave as if *Later* was
      tapped.
* [ ] On match: install using the platform's normal mechanism (Android:
      fire `ACTION_INSTALL_PACKAGE` with a `FileProvider` URI).
* [ ] On *Skip*: persist `versionName` locally as the skipped version. A
      higher `versionName` later implicitly un-skips.
* [ ] **Never** re-GET the same `/api/app/<track>/update` more than once
      per cold start (server sets `Cache-Control: no-store` — every hit
      counts).

## Reverse-proxy / CDN notes

When the API is fronted by Caddy, Nginx or a CDN, those have their own
body read timeouts that can cut off a large upload *before* it reaches
the API. Caddy's `reverse_proxy` directive's default `read_timeout` is
**30 s**, which is too short for any non-trivial build. Bump it explicitly:

```caddyfile
nvr.example.com {
    reverse_proxy localhost:8080 {
        # Allow big uploads to reach the API. The API's own
        # ReadTimeout is the upper bound on the actual read window.
        transport dial {
            dial_timeout 30s
            response_header_timeout 60s
        }
        # Default Caddy buffers 1MB then streams; for 200MB+ uploads the
        # body is streamed to the backend by default. No extra config
        # needed unless a Caddy plugin is buffering.
        # Pass the original scheme + host so the API can build https://
        # URLs in the manifest (or skip this if you set
        # `[updates] public_base_url` in the API config — the recommended
        # approach).
        header_up X-Forwarded-Proto https
        header_up X-Forwarded-Host {host}
    }
}
```

For Nginx, set `client_max_body_size` to match (or exceed)
`[updates] max_build_size`, and bump `proxy_read_timeout` /
`proxy_send_timeout` to cover the upload window (the API's
`[server] read_timeout` is a sensible target).
