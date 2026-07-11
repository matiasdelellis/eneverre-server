# Embedded media engine

Eneverre runs an in-process media engine for recording, RTSP relay and
browser live. It is the **only** streaming mode — the historical
external-[MediaMTX] integration was removed (see
[Why the embedded engine](#why-the-embedded-engine) below). The engine is
always started for every camera with a `source` URL: the live MSE feed and
the RTSP relay are **on by default**, with no `[media]` section required.
Adding a `[media]` section turns on disk recording (off by default) and
tunes paths, timing and retention. Each feature is an independent switch,
global (`[media]`) with a per-camera opt-out (see
[Independent switches](#independent-switches)).

[MediaMTX]: https://github.com/bluenviron/mediamtx

## Why the embedded engine

Eneverre was originally a thin configuration broker in front of an
external [MediaMTX] process. That meant a second long-running service
per host (with its own supervision, log file, reverse-proxy rules,
config-as-code), an HTTP auth probe (`POST /api/auth`) the proxy had to
serve, and a control API the API had to forward recording-list / clip
requests to. The rotating-credential store, the per-camera recorder,
the gap fill and the HLS VOD muxer all lived in MediaMTX.

Reimplementing the recorder (gortsplib + mediacommon + pion) and the
RTSP relay in-process in the same Go binary produced a system that, for
H264 (+AAC/G711) cameras, does the same job without the sidecar: one
binary, one systemd unit, one auth surface, one set of recording
endpoints, one log stream. The on-disk segment format is still
MediaMTX-compatible (`mtxi` box, same fMP4 layout), so the recorder's
output can still be inspected with the MediaMTX tooling if needed.

The removal of the external integration dropped `[mediamtx]` and the
`POST /api/auth` endpoint, the MediaMTX control-API proxy, the
`mediamtx_credentials` SQLite table (renamed `streamauth_credentials`),
and the `WithMediaMTXURLs` camera helper. The rotating-credential
mechanism stayed (now `streamauth.Store`) because the embedded RTSP
relay still needs it.

### Independent switches

The engine exposes three independent on/off switches — **live MSE**, **RTSP
relay**, and **recording** — each with a global default in `[media]` and a
per-camera opt-out in the camera INI. A camera is served if at least one of
the three is on for it (so a record-only camera, with MSE and relay off, still
connects and writes to disk).

| switch   | global (`[media]`) | per-camera (`cameras.d/<id>.ini`) | effect when on |
|----------|--------------------|-----------------------------------|----------------|
| `mse`    | `true`             | `true`                            | live fMP4 feed at `/api/camera/{id}/live/stream` |
| `relay`  | `true`             | `true`                            | RTSP relay at `[media].rtsp_address` (default `:8554`) |
| `record` | `false`            | `true`                            | segments on disk, indexed for `/recordings/*` |

The per-camera flag can only **turn a feature off** for that camera: the global
switch is the master, and the effective state is `global AND per-camera`. So a
per-camera `record = true` does nothing unless `[media] record = true` is also
set; use it to keep a camera in the default-on state while opting others out.

### Privacy (runtime pause)

Separately from the static switches above, each camera offers a **privacy**
toggle at runtime (`POST /api/camera/{id}/privacy?enable=true|false`). Enabling
privacy **stops recording and transmission**: the engine disconnects the
camera's recorder and parks its retry loop, so it neither writes segments nor
feeds the live MSE broadcast / RTSP relay until privacy is turned off (which
reconnects with a fresh session). While paused, `/api/cameras` withholds the
camera's `live_mse` and `rtsp` URLs. On thingino cameras privacy additionally
drives the firmware lens blackout and moves a PTZ to/from its privacy position.

Privacy is offered for every camera by default; set `privacy = false` in the
camera INI to mark an **always-on** camera that must never be paused (no privacy
toggle, `capabilities.privacy = false`, the endpoint returns 404).

Because MSE and relay default to on, the engine is fully useful with **no
`[media]` section at all** (live-only): the live feed and relay come up, but no
`[media]` means recording is off, no SQLite index is opened, and the
`/recordings/*` endpoints answer 404. Add `[media] record = true` (with a
`record_dir`) to turn recording on and enable the retention cleaner
(`[media].retain`).

## What it does

For every camera that has a `source` RTSP URL, the engine:

1. **Records** the stream to fragmented-MP4 segments on disk (H264 video +
   optional AAC/G711 audio), cataloging each segment in a shared SQLite index —
   the same layout MediaMTX wrote (including the `mtxi` box for gapless
   concatenation on playback).
2. **Relays** the live stream over RTSP (`rtsp://…:8554/<id>`) as a raw RTP
   passthrough, so many clients (e.g. the Android apps) read from Eneverre
   instead of hammering the camera. Sub-second latency, no re-encode.
3. **Broadcasts** the live stream to browsers as chunked-HTTP fMP4 for a
   MediaSource player (`GET /api/camera/<id>/live/stream`, ~1-2s latency). The
   web UI uses this instead of HLS.
4. **Serves playback** (VOD) straight from the index over the existing
   `GET /api/camera/<id>/recordings/{list,get}` endpoints — no proxy hop.
5. **Retains** recordings for a configurable age, deleting expired files, index
   rows and empty directories.

It is a single binary and a single systemd unit — no separate streamer to
install, configure or supervise.

## Codec support

**H264 video + AAC or G711 audio only** (G711 is transcoded to LPCM for fMP4).
The RTSP relay is a raw passthrough and carries whatever the camera sends, but
recording and the browser (MSE) live view require H264 (+AAC for audio in the
browser). H265/HEVC is **not** supported by the recorder or MSE view.

A camera that offers no supported video codec is detected and logged once
(`camera codec not supported … stream offers: H265`), then retried slowly; it
is neither recorded nor relayed. Adding H265 is scoped in
[`doc/PLANS/H265.md`](PLANS/H265.md): record/relay/playback would be a modest
addition, but universal web-live for H265 is limited by browser HEVC support.

## Configuration

```ini
[media]
record_dir       = /var/lib/eneverre/recordings
;index_path      = /var/lib/eneverre/recordings/index.db
;record_path     = /var/lib/eneverre/recordings/%path/%Y-%m-%d/%H/%Y-%m-%d_%H-%M-%S-%f
segment_duration = 60s
part_duration    = 1s
retain           = 240h        ; 0 = keep forever
rtsp_address     = :8554       ; RTSP relay listen address
;rtsp_host       = nvr.example.com  ; public host; only then is `rtsp` exposed
transport        = auto        ; source transport: auto (default) | tcp | udp
;rotate_hours    = 24          ; stream/relay credential rotation
```

See [`doc/example/eneverre.ini`](example/eneverre.ini) for the annotated
reference. Keep `record_dir` under `/var/lib/eneverre` (the systemd
`StateDirectory`) or add it to the unit's `ReadWritePaths`.

### Camera source

The engine records/relays from each camera's **direct** RTSP URL. Set it with
the `source` key in the camera INI. It must
point at the camera itself and carries credentials — it is **never** exposed to
clients.

```ini
[camera]
id     = calle
source = rtsp://user:pass@192.168.1.91:554/ch0
playback = true
```

## What `GET /api/cameras` returns

In embedded mode each camera's stream fields are rebuilt as:

| Field      | Value                                                            |
|------------|------------------------------------------------------------------|
| `live_mse` | `/api/camera/<id>/live/stream` (same-origin browser MSE)         |
| `rtsp`     | `rtsp://<user>:<pass>@<host>:<port>/<id>` — `host` is `rtsp_host` when set, else the host the client used to reach the API |

The credentials embedded in `rtsp` are the rotating pair (see below); the raw
camera `source` is never returned. Set `rtsp_host` to pin the host in public /
reverse-proxied deployments (where the API host and the RTSP relay host
differ); on a LAN the request-host fallback fills `rtsp` automatically.

## Credentials & rotation

The engine uses a rotating credential store (`streamauth_credentials` table,
carried over from the historical MediaMTX integration — see [Why the embedded
engine](#why-the-embedded-engine)). A random 8/8 char pair guards the RTSP
relay; it rotates every `rotate_hours` (default 24, `0` disables), and the
previous pair stays valid for one interval (grace window) so readers are not
dropped mid-rotation. The relay validates each RTSP connection against the
current **and** grace pair. The HTTP live and playback endpoints are
protected by Eneverre's own user auth (Bearer/Basic), not this pair.

## Live HTTP endpoints

| Method + path | Purpose |
|---|---|
| `GET /api/camera/{id}/live/info`   | `{available, mime}` — MSE availability + codec string |
| `GET /api/camera/{id}/live/stream` | live fMP4 (init + parts) as chunked HTTP for MediaSource |

Plus the RTSP surface: `rtsp://[user:pass@]host:8554/<id>`.

## Recordings (playback) HTTP endpoints

Backed by the in-process segment index; require user auth + the camera's
`playback` capability.

| Method + path | Purpose |
|---|---|
| `GET /api/recordings/paths` | camera ids that have recordings: `["calle", …]` (for recordings-only clients; = NVR's `/api/paths`) |
| `GET /api/camera/{id}/recordings/list?start=&end=` | segments overlapping the range: `[{start, duration}]` |
| `GET /api/camera/{id}/recordings/get?start=&duration=[&fill_gaps=]` | fMP4 clip (`video/mp4`) spanning the full window, gaps filled with black (see below); `404` + `X-Next-Available` header on a miss |
| `GET /api/camera/{id}/recordings/timeline` | recorded extent: `{start, end, count}` (start/end null if empty) |
| `GET /api/camera/{id}/recordings/gaps?start=&end=` | coverage gaps >1s: `[{start, end, duration}]` |
| `GET /api/camera/{id}/recordings/hls/playlist.m3u8?start=&end=` | HLS VOD playlist (CMAF); gaps collapsed, `EXT-X-PROGRAM-DATE-TIME` per segment |
| `GET /api/camera/{id}/recordings/hls/init.mp4` | CMAF init (referenced by the playlist) |
| `GET /api/camera/{id}/recordings/hls/segment.m4s` | CMAF media segment (referenced by the playlist) |

Timestamps are RFC3339 (UTC); `duration` is in seconds. The web timeline
plays the HLS VOD playlist via hls.js (auth via the bearer token on every
request); the playlist's init and segment URIs are relative, so they
resolve under the same `/recordings/hls/` prefix. `get` is the single-file
download.

### Gap fill in downloads (`/get`)

A downloaded clip that spans a coverage gap is **not** truncated at the gap:
the gap is filled with a black frame captioned **"NO RECORDING"** (configurable
via `[media] gap_message`, UTF-8 ok) occupying the real gap time, so the clip
always spans the full requested window and it is obvious there was no recording
(vs. looking trimmed).

- The spliced black frame has a different SPS than the recording, so the clip
  is emitted as **avc3** (H264 parameter sets in-band) — the decoder reads the
  SPS from the bitstream and switches at the gap boundary. Recordings already
  carry in-band SPS in every keyframe. **Players must support avc3**; mainstream
  ones (VLC, browsers, QuickTime, anything ffmpeg-based) do.
- The black frame is generated once per resolution+message (via ffmpeg) and
  **persisted** to `<cache_dir>/gapfill/<WxH>-<msghash>.h264` (a few KB each),
  so it is reused across restarts instead of regenerating; changing
  `gap_message` regenerates it (new hash). `cache_dir` is configurable (default
  `<record_dir>/../cache`). If ffmpeg is unavailable and nothing is cached, the
  engine falls back to the legacy behavior (truncate at the gap).
- `fill_gaps=false` forces the legacy gapless output (avc1, truncated at the
  first gap).
- Making the caption carry the gap's date/time (a running clock or the gap's
  time range) is scoped in [`doc/PLANS/GAPFILL-DYNAMIC.md`](PLANS/GAPFILL-DYNAMIC.md).
- Audio: there are no audio samples during the gap; the last pre-gap audio
  frame's presentation is stretched over it (a brief blip then silence). Video
  is black throughout.

## Client integration notes

What a client (web UI, mobile/TV app) consumes. All `/api/*` calls take
Bearer (or Basic) auth.

**Live view**
- **Web / MSE**: use `camera.live_mse` (`/api/camera/{id}/live/stream`). It is a
  chunked-HTTP fMP4 stream (init + parts) for a `MediaSource` `SourceBuffer`;
  fetch it with the Bearer token and query `live/info` first for the codec
  `mime`. ~1-2s latency.
- **Apps / RTSP**: use `camera.rtsp` (the relay `rtsp://…:8554/{id}`), present
  only when `[media] rtsp_host` is set. Standard RTSP; the embedded creds
  rotate, so re-read `/api/cameras` for a fresh URL.
- Without `[media]` the camera is returned as-is from the INI and the live
  view is unavailable (the wall shows "No live stream" — the embedded
  engine is the only streaming surface Eneverre serves itself).

**Recordings / timeline**
- `recordings/timeline` → draw the recorded extent; `recordings/gaps` → mark gaps.
- `recordings/list` → segment blocks for the range.
- **Streaming playback (scrubbable timeline)**: `recordings/hls/playlist.m3u8`
  via hls.js (add the Bearer header in `xhrSetup`; native HLS can't). Continuous
  timeline with `EXT-X-PROGRAM-DATE-TIME` for wall-clock cursor mapping.
- **Download / export**: `recordings/get` → one fMP4 spanning the full window,
  gaps shown as black "NO RECORDING". **Emitted as avc3** — ensure the target
  player decodes avc3 (all mainstream players do). `fill_gaps=false` reverts to
  legacy avc1/truncate.

**Auth reminder for HLS**: every playlist/init/segment request needs the Bearer
token. Use hls.js `xhrSetup` (or Basic-in-URL, e.g. VLC:
`http://user:pass@host/…/playlist.m3u8`).

## Reverse proxy

Live (MSE) and playback are plain HTTP under `/api/*`, so a single
`reverse_proxy` to Eneverre covers them — no HLS/WebRTC rules needed.
The RTSP relay (`:8554`) does **not** go through the proxy; expose it directly
(firewalled) for RTSP clients. See [`doc/example/Caddyfile`](example/Caddyfile).

## Without recording

There is no "without the engine" mode: the engine is always started for
cameras with a `source` URL. Omitting `[media]` only turns **recording** off —
the live MSE feed and RTSP relay still run (live-only mode), and the
`/recordings/*` endpoints answer 404. The raw camera `source` URL is never
exposed to clients; the relay `rtsp://…:8554/{id}` (rotating credentials) is
served instead.

Eneverre only streams H264 (+AAC/G711) itself. To serve a non-H264 codec,
WebRTC or HLS, front the camera with an external streamer (go2rtc, lightNVR, a
reverse proxy) and turn the built-in feeds off — set `mse = false` and
`relay = false` per camera, or globally in `[media]`.
