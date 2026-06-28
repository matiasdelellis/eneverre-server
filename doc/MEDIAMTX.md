# MediaMTX integration

How Eneverre brokers RTSP / HLS / WebRTC playback through
[MediaMTX][mediamtx], and — in particular — how the `POST /api/auth`
probe that MediaMTX calls actually works.

[mediamtx]: https://github.com/bluenviron/mediamtx

## Overview

When the `[mediamtx]` section is present in `eneverre.ini`, Eneverre
takes over the public stream URLs of every camera:

1. On first run it generates a random username / password pair (8
   alphanumeric chars each) and persists it to the
   `mediamtx_credentials` SQLite table.
2. Every `GET /api/cameras` response embeds that pair in the camera's
   `rtsp` / `hls` / `webrtc` (and `live`) URLs.
3. The pair is rotated every `rotate_hours` (default 24h). The previous
   pair stays valid for one more interval — a *grace window* — so a
   live stream is not dropped the instant credentials rotate.
4. For every RTSP / HLS / WebRTC request, MediaMTX is configured to
   delegate auth to Eneverre's `POST /api/auth`. Eneverre replies 200 if
   the credentials match the current *or* grace pair, 401 otherwise.

This lets you put the cameras on the public internet through a reverse
proxy *without* handing out a static secret — the secret is regenerated
on a schedule, MediaMTX re-checks on every connection, and clients pick
up the new URL on their next `GET /api/cameras`.

When the `[mediamtx]` section is absent, Eneverre serves the raw
`live` / `hls` / `webrtc` URLs from each camera's INI file as-is, and
`/api/auth` returns 404. In that mode, securing the URLs is the
responsibility of whatever fronts the streamer (Caddy, go2rtc,
lightNVR…).

## Server-side configuration

```ini
[mediamtx]
server        = nvr.example.com
rtsp_port     = 8554
hls_path      = /hls/
webrtc_path   = /whep/
playback_port = 9996
rotate_hours  = 24
```

See [`doc/example/eneverre.ini`](example/eneverre.ini) for the full
reference and the meaning of every key. Required:

  * `server` — the **public** hostname clients use to reach MediaMTX
    (not `127.0.0.1`; that would put the credentials in URLs no one
    outside the box can use).
  * `rtsp_port` — defaults to `8554`.
  * `hls_path` / `webrtc_path` — defaults to `/hls/` and `/whep/`.
    The prefixes (and the trailing slash on `hls_path`) must match what
    MediaMTX actually publishes under; see the Caddyfile comments for
    why this matters when a reverse proxy is in front.

`rotate_hours` is optional (default `24`; `0` or a negative number
disables rotation — the pair from first run is kept forever).

## What `GET /api/cameras` returns

Each camera gets four URL fields, rebuilt on every request from the
in-memory current credentials:

| Field    | Pattern                                                       |
|----------|---------------------------------------------------------------|
| `rtsp`   | `rtsp://<user>:<pass>@<server>:<rtsp_port>/<id>`              |
| `hls`    | `https://<user>:<pass>@<server><hls_path>/<id>/index.m3u8`    |
| `webrtc` | `https://<user>:<pass>@<server><webrtc_path>/<id>`            |
| `live`   | mirrors `rtsp` (legacy alias)                                 |

The camera's `id` (from its INI `[camera] id = …`) **must match the
path the camera is published under in MediaMTX** — that is the
identifier MediaMTX echoes back in the auth probe (see below). If they
don't match, the probe will arrive with a `path` Eneverre has no
context for, and the `user` / `password` check still passes but the
stream is the wrong one.

The raw `live` / `hls` / `webrtc` keys in the camera INI are
**ignored** when `[mediamtx]` is set. Eneverre does not need to know
where the camera RTSP *source* lives — that lives in MediaMTX's own
config.

## MediaMTX configuration

In `mediamtx.yml`:

```yaml
authMethod: http
authHttpAddress: http://127.0.0.1:8080/api/auth
```

Use `127.0.0.1:8080` when MediaMTX and Eneverre run on the same host
(recommended). If they run on different hosts, point at the URL Eneverre
is reachable on from MediaMTX (typically a LAN address; the endpoint
itself is unauthenticated and meant to be private).

The credentials you configured the *camera* in MediaMTX with (the
onboarding user, the `users` list, etc.) are **irrelevant** to Eneverre.
MediaMTX delegates the check to `/api/auth` and forwards whatever
`user` / `password` were sent on the wire — those need to match the
rotating pair, not whatever you typed into MediaMTX's own auth.

That's the entire MediaMTX-side wiring. No restart of MediaMTX is
needed when Eneverre rotates: the new pair is live in the in-memory
store, and the auth probe consults the current + grace pair on every
request.

## `POST /api/auth` — the auth protocol

MediaMTX POSTs a JSON body to this endpoint for **every** RTSP, HLS,
and WebRTC request — including each HLS segment request, not just the
playlist. Eneverre's reply is HTTP status only; the body is just
`{"message": "..."}` (or `{"detail": "..."}` on errors, matching the
FastAPI-compatible error shape used everywhere else in the API).

### Request body

```json
{
  "user":     "uK7p2xYz",
  "password": "aB3cD9eF",
  "ip":       "190.1.2.3",
  "action":   "read",
  "path":     "calle",
  "protocol": "rtsp",
  "id":       "...",
  "query":    "..."
}
```

Fields (all present in MediaMTX ≥ 1.0):

  * **`user`** *(string)* — username from the request line. This is
    the Eneverre-rotated random user.
  * **`password`** *(string)* — same, for the password. **This is the
    only field Eneverre needs to make a decision**, and it is the only
    field Eneverre does **not** log.
  * `ip` *(string)* — client IP as seen by MediaMTX.
  * `action` *(string)* — `read` for playback, `publish` for a push
    source. Eneverre accepts both; the decision is the same.
  * `path` *(string)* — the camera id in MediaMTX (matches the camera
    `id` in Eneverre, see above).
  * `protocol` *(string)* — one of `rtsp`, `hls`, `webrtc`, `rtmp`,
    `srt`…
  * `id` *(string)* — MediaMTX's internal request id, for tracing on
    MediaMTX's side. Not used by Eneverre.
  * `query` *(string)* — query string from the request, if any
    (HLS clients sometimes add one). Not used by Eneverre.

### Response

| Status | Body                          | Meaning                                          |
|--------|-------------------------------|--------------------------------------------------|
| 200    | `{"message": "Authorized"}`   | Credentials match the current **or** grace pair  |
| 401    | `{"detail": "Unauthorized"}`  | No match                                         |
| 404    | `{"detail": "Not Found"}`     | Eneverre has no `[mediamtx]` section configured  |
| 400    | `{"detail": "..."}`           | Body was not valid JSON                          |

The handler is registered only when `[mediamtx]` is configured. Without
that section, the route is mounted but immediately returns 404 — so
misconfiguring MediaMTX's `authHttpAddress` against an Eneverre
instance with no `[mediamtx]` block fails closed, not open.

### Validation

The probe is served from an in-memory `mediamtx.Store`:

  * `Current` — the pair that goes into *new* stream URLs.
  * `Previous` — the pair from the last rotation; valid for one
    `rotate_hours` interval (the *grace window*).

`Validate(user, pass)` is **constant time** on both the current and
previous pair, so a timing side-channel cannot be used to probe which
is which. The DB is only touched at startup (`NewStore`) and on
rotation (`Rotate`); the per-request path never queries the database,
and the auth handler itself does not allocate per-request structures
beyond a single `slog` call.

## Credential rotation

The background goroutine (`mediamtx.Store.StartRotation`) ticks every
`rotate_hours`. On each tick:

  1. A new 8-char alphanumeric pair is generated.
  2. The previous *current* pair is demoted to the grace slot.
  3. The new pair is upserted into `mediamtx_credentials` (the
     single-row table) so a restart preserves it.
  4. The previously-grace pair is discarded.

Effect on live streams:

  * **HLS** — already-buffered segments keep playing from cache; on
    the next `.m3u8` poll the client hits `/api/auth` with the **new**
    creds (because Eneverre rebuilt the URL on its next
    `GET /api/cameras`), so the request succeeds. If a client is slow
    to pick up the new URL, its requests with the *old* creds are
    accepted for one more `rotate_hours` thanks to the grace window,
    and then start returning 401.
  * **RTSP** — the TCP connection is authenticated on connect. A
    long-lived RTSP stream survives the rotation (MediaMTX doesn't
    re-probe), but the next reconnect uses whatever URL the client
    has, which is the grace pair at worst — still accepted for one
    interval.
  * **WebRTC** — the WHEP session is authorized at the HTTP
    signaling step. The ICE / DTLS media path is independent of the
    auth probe, so a live WebRTC session is not interrupted by a
    rotation; the next reconnect uses the new URL.

Each successful rotation logs:

```
level=INFO msg="mediamtx credentials rotated"
```

## Reverse-proxy considerations

| Protocol  | Goes through HTTP proxy? | Notes |
|-----------|--------------------------|-------|
| HLS       | Yes                      | Plain HTTP, fine behind Caddy / Nginx. |
| WebRTC (WHEP signaling) | Yes  | Plain HTTP. |
| WebRTC (media)         | **No** | UDP (RTP / ICE). Expose MediaMTX's WebRTC UDP ports directly or tunnel them. |
| RTSP      | **No**                   | TCP control, RTP/UDP media. Expose MediaMTX's `rtsp_port` (default 8554) directly. |
| MediaMTX control API   | Yes   | Optional — used by `playback` and the recordings list. Default port `9997`. |

A ready-to-use Caddyfile covering the UI, REST API, HLS, and MediaMTX
control API lives at [`doc/example/Caddyfile`](example/Caddyfile). It
deliberately does **not** proxy RTSP / WebRTC media — the comments
explain the rationale.

## Debugging

`ENEVERRE_LOG_LEVEL=debug` (or `[server] log_level = debug`) makes
every successful MediaMTX authorization visible. Denials always log at
**WARN** regardless of the level. The password is **never** logged;
the other request fields are.

```
level=DEBUG msg="mediamtx auth"        user=uK7p2xYz action=read path=calle  protocol=rtsp   ip=190.1.2.3 authorized=true
level=WARN  msg="mediamtx auth denied" user=uK7p2xYz action=read path=calle  protocol=rtsp   ip=190.1.2.3 authorized=false
```

What to look for when nothing plays:

  1. **No log line at all** — MediaMTX is not reaching Eneverre.
     Check `authHttpAddress` and that the host:port is reachable from
     MediaMTX. Watch the access log
     (`level=INFO msg=request method=POST path=/api/auth …`) to see
     the probes.
  2. **Always 401** — the credentials in the request don't match
     either pair. Common causes: client cached an *old* URL older than
     one `rotate_hours`, or `server` in `[mediamtx]` resolves to
     something the client can't reach (URLs were built with the wrong
     host). Force a `GET /api/cameras` and use the freshest URL.
  3. **HTTP 404 from `/api/auth`** — Eneverre has no `[mediamtx]`
     section. The camera URLs returned by `/api/cameras` are the raw
     INI values; either add the section or stop configuring MediaMTX
     to call this endpoint.
  4. **403 from MediaMTX even though Eneverre returned 200** —
     MediaMTX's own `users` / `user` block is denying the request
     before it reaches `authMethod: http`. The `authMethod: http`
     block is authoritative; remove (or empty out) the static
     `users:` list.

## Without MediaMTX

Eneverre works equally well with go2rtc, lightNVR, or a plain reverse
proxy. In that case:

  * Omit the `[mediamtx]` section from `eneverre.ini`. The
    `mediamtx_credentials` table is never created; `Rotate` is never
    called; `/api/auth` is not mounted.
  * Set the `live` URL on each camera INI to whatever public URL your
    streamer exposes (or whatever URL your reverse proxy serves), and
    set `playback = true` if the streamer also serves recordings.

The web UI and the Android apps do not care which path was taken.
