# Zaguán

Eneverre can publish its cameras to a [Zaguán](https://github.com/matiasdelellis/zaguan)
hub through the hub's [Source API](https://github.com/matiasdelellis/zaguan/blob/main/docs/source-api.md).
The hub owns the devices of the home; Eneverre is one provider, so the family
app lists the cameras next to the lights, the sensors and the alarms.

The integration is **off by default**. It is enabled by the `[zaguan]` section
of `eneverre.ini`; the full key reference is in
[`doc/example/README.md`](example/README.md#zaguan).

## Setup

1. In the hub UI open **Sources → New**, choose kind `eneverre` and create the
   source. The hub shows the `zsrc_...` token **once**; it stores only its
   SHA-256 hash.
2. Put the token in a file only the Eneverre user can read and point the
   config at it:

   ```ini
   [zaguan]
   url = http://192.168.1.20:8081
   token_file = /etc/eneverre/zaguan.token
   ;public_url = https://cams.example.com
   ```

   Alternatively set `token` inline, or use `ENEVERRE_ZAGUAN_URL` /
   `ENEVERRE_ZAGUAN_TOKEN`. The file form is recommended.
3. Restart Eneverre. The log says `zaguan source enabled` and the hub's
   Sources page shows the last-seen time. No restart of the hub is needed.
   If the hub token has an expiry, replace the file and restart before it
   lapses; the publisher keeps retrying with the rejected token and logs the
   failure.

Eneverre only makes outbound HTTP requests to the hub (snapshot, heartbeat and
the command long poll), so the hub does not need to reach the NVR API. The NVR
API does not need a public address either, except for the optional media links
described below.

## What is published

One source carries every camera. Each camera is one device with one `camera`
entity:

| Zaguán | Eneverre |
|---|---|
| source | this server |
| device `eneverre:<camera-id>` | one camera; `Manufacturer = Eneverre` |
| entity `eneverre:<camera-id>/camera` | the camera itself, domain `camera` |
| area `eneverre:<location-slug>` | the camera's `location`, when set |
| `streaming` | the NVR has the camera (connected, enabled, not paused) |
| `idle` | disconnected, disabled, in privacy, or outside its schedule |
| unavailable | same conditions as `idle` |

An **disabled** camera stays in the hub for diagnostics but is published with
`disabled`, so the family app does not show it. A camera in **privacy** or
outside its **recording schedule** is unavailable: its pipeline is paused, so
there is no stream to show.

Capabilities are published only when the camera has them:

| Capability | When |
|---|---|
| `live` | the camera has live MSE or the RTSP relay enabled |
| `snapshot` | it has a Thingino key or a `snapshot_url` |
| `recording` | recording is enabled for the camera |
| `ptz` | the camera exposes PTZ motor control |

The entity carries these attributes (empty values are omitted):

| Attribute | Meaning |
|---|---|
| `enabled`, `privacy`, `schedule_off`, `recording` | the live flags, the same the API reports |
| `comment`, `aspect_ratio` | camera metadata (`16:9`, the sensor aspect ratio) |
| `rtsp_url` | the authenticated relay URL on the RTSP port, with the rotating credentials |
| `mse_url` | absolute live MSE (fMP4) stream for a browser `MediaSource` |
| `mse_info_url` | absolute live MSE codec/availability JSON (`{available, mime}`) |
| `hls_url` | absolute HLS **VOD** playlist for the last hour (needs recordings) |
| `snapshot_url` | absolute thumbnail URL |
| `playback`, `talk`, `ptz` | the interactive capabilities the camera has |

The links are regenerated on every snapshot, so the hub keeps usable URLs
across credential rotations. They need a reachable host: `[media] rtsp_host`
or `[zaguan] public_url` supplies it. The URLs carry the relay's rotating
username/password as `user`/`pass` query parameters, and the media endpoints
accept that pair only while `[zaguan]` is configured. It is the same pair
already embedded in the RTSP URL, so the hub's web client can play MSE, HLS
and thumbnails without an Eneverre login. Treat the hub store with the same
care as the Eneverre API: anyone who can read a camera's attributes can read
its media.

There is **no HLS live** mode in Eneverre: live is MSE (or the RTSP relay), and
HLS is the VOD playlist of what is already on disk. `hls_url` covers the last
hour of the camera's timeline and is a normal VOD playlist that any HLS
player (hls.js, VLC, ExoPlayer, AVPlayer) can open. A player that wants another
window calls the same endpoint with its own `start`/`end`; the playlist
propagates the `user`/`pass` pair to its init and segment URIs.

## Commands

The `camera` domain in the Zaguán catalog offers `snapshot`, `stream_start`,
`stream_stop` and the PTZ commands. Eneverre advertises only what the camera
can actually do; a camera without snapshot or PTZ publishes an explicit empty
command list, which the hub renders as read-only:

- **`snapshot`** asks the NVR to fetch a fresh still from the camera (the same
  path as `GET /api/camera/{id}/thumbnail`). It succeeds only when the camera
  is enabled, not paused (privacy/schedule) and has a snapshot endpoint; the
  hub applies the confirmed state and attributes to the entity.
- **`ptz_move`** takes `pan` and `tilt` in degrees, relative to the current
  position; **`ptz_home`** goes to the configured home position and
  **`ptz_recalibrate`** runs the motor calibration. They are advertised only
  for cameras with PTZ and Thingino credentials.
- There is no `stream_start` / `stream_stop`: the embedded engine records and
  relays the camera continuously. Pausing the pipeline is the privacy toggle
  in the Eneverre UI/API, not a per-viewer stream command. Advertising the
  commands would make the hub show buttons that cannot do what they say.

The Source API has no binary command result, so `snapshot` does not return the
JPEG itself; a camera view that wants to paint it fetches `snapshot_url` (the
thumbnail URL with the media credentials).

## Lifecycle

- The first snapshot is sent immediately, and the source metadata (name,
  kind, version) once at startup.
- The inventory is rebuilt every `sync_interval` (default 30s) and published
  as a new snapshot. That is what refreshes connectivity, privacy and schedule
  state in the hub; a failed publication is retried, and the loop survives a
  hub restart. Version numbers are wall-clock based and bumped on a `409` from
  the hub.
- A heartbeat keeps the lease alive every 30s. Without it the hub marks the
  source offline and its entities unavailable, keeping the last state.
- Commands arrive through a 25s long poll. If the hub is down the publisher
  keeps retrying; the cameras and the NVR are unaffected.

## Not part of this integration (yet)

- No motion events: the hub does not hear about `POST /api/camera/{id}/events`
  yet, so a motion sensor entity is not published.
- No recording timeline or clip API exposed to the hub: `hls_url` is a
  ready-to-play last-24h VOD playlist, not a browsable archive.
- No privacy switch entity. Privacy is toggled in Eneverre; the hub only
  reflects the resulting availability.
- No HLS **live**. The live feed is MSE (`mse_url`) or RTSP; HLS is VOD. The
  Zaguán camera view plays MSE, refreshes the still, opens the HLS recording
  link and operates the PTZ pad when the camera has PTZ. Talk is still only
  advertised as a flag.
