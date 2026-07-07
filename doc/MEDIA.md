# Embedded media engine

Eneverre can run its own in-process media engine instead of delegating to an
external [MediaMTX][mediamtx]. Enable it with a `[media]` section in
`eneverre.ini`. When present it **takes precedence** over `[mediamtx]`.

[mediamtx]: https://github.com/bluenviron/mediamtx

## What it does

For every camera that has a `source` (or `live`) RTSP URL, the engine:

1. **Records** the stream to fragmented-MP4 segments on disk (H264 video +
   optional AAC/G711 audio), cataloging each segment in a shared SQLite index —
   the same layout MediaMTX writes (including the `mtxi` box for gapless
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
the `source` key in the camera INI (falls back to `live` when omitted). It must
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
| `hls`, `webrtc` | empty (not served by the engine)                            |

The credentials embedded in `rtsp` are the rotating pair (see below). `hls` and
`webrtc` are cleared, and the raw camera `source` is never returned. Set
`rtsp_host` to pin the host in public / reverse-proxied deployments (where the
API host and the RTSP relay host differ); on a LAN the request-host fallback
fills `rtsp` automatically.

## Credentials & rotation

The engine reuses the rotating credential store (`mediamtx_credentials` table).
A random 8/8 char pair guards the RTSP relay; it rotates every `rotate_hours`
(default 24, `0` disables), and the previous pair stays valid for one interval
(grace window) so readers are not dropped mid-rotation. The relay validates
each RTSP connection against the current **and** grace pair. The HTTP live and
playback endpoints are protected by Eneverre's own user auth (Bearer/Basic),
not this pair.

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

`list`/`get` also work in MediaMTX mode (proxied); `timeline`/`gaps` and the
HLS VOD endpoints are embedded-engine only. Timestamps are RFC3339 (UTC);
`duration` is in seconds. The web timeline plays the HLS VOD playlist via
hls.js (auth via the bearer token on every request); the playlist's init and
segment URIs are relative, so they resolve under the same `/recordings/hls/`
prefix. `get` is the single-file download.

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

What a client (web UI, mobile/TV app) consumes, and what changes vs. the
external MediaMTX mode. All `/api/*` calls take Bearer (or Basic) auth.

**Live view**
- **Web / MSE**: use `camera.live_mse` (`/api/camera/{id}/live/stream`). It is a
  chunked-HTTP fMP4 stream (init + parts) for a `MediaSource` `SourceBuffer`;
  fetch it with the Bearer token and query `live/info` first for the codec
  `mime`. ~1-2s latency. (This replaces playing `camera.hls` with hls.js.)
- **Apps / RTSP**: use `camera.rtsp` (the relay `rtsp://…:8554/{id}`), present
  only when `[media] rtsp_host` is set. Standard RTSP; the embedded creds
  rotate, so re-read `/api/cameras` for a fresh URL.
- In embedded mode `camera.hls` and `camera.webrtc` are **empty**. Clients must
  branch on `live_mse` (embedded) vs `hls`/`webrtc` (MediaMTX).

**Recordings / timeline**
- `playback/timeline` → draw the recorded extent; `playback/gaps` → mark gaps.
- `playback/list` → segment blocks for the range.
- **Streaming playback (scrubbable timeline)**: `playback/hls/playlist.m3u8`
  via hls.js (add the Bearer header in `xhrSetup`; native HLS can't). Continuous
  timeline with `EXT-X-PROGRAM-DATE-TIME` for wall-clock cursor mapping.
- **Download / export**: `playback/get` → one fMP4 spanning the full window,
  gaps shown as black "NO RECORDING". **Emitted as avc3** — ensure the target
  player decodes avc3 (all mainstream players do). `fill_gaps=false` reverts to
  legacy avc1/truncate.

**Auth reminder for HLS**: every playlist/init/segment request needs the Bearer
token. Use hls.js `xhrSetup` (or Basic-in-URL, e.g. VLC:
`http://user:pass@host/…/playlist.m3u8`).

## Reverse proxy

Live (MSE) and playback are plain HTTP under `/api/*`, so a single
`reverse_proxy` to Eneverre covers them — no HLS/WebRTC/MediaMTX rules needed.
The RTSP relay (`:8554`) does **not** go through the proxy; expose it directly
(firewalled) for RTSP clients. See [`doc/example/Caddyfile`](example/Caddyfile).

## Choosing between modes

- **Embedded (`[media]`)** — one binary, one unit; H264(+AAC/G711); browser
  live over MSE; Android over RTSP. Recommended default.
- **External (`[mediamtx]`)** — delegate to MediaMTX for broader codecs,
  WebRTC, and HLS. See [`doc/MEDIAMTX.md`](MEDIAMTX.md).

With neither section, Eneverre serves the raw `live`/`hls`/`webrtc` URLs from
each camera INI as-is (front it with go2rtc, lightNVR or a plain proxy).
