# Live over Media-over-QUIC (MoQ)

Status: **investigated, not planned.** This is a decision record, not a
work order: it says what MoQ would buy Eneverre, what it would cost, why
the answer today is "not yet", and which concrete signals should reopen
the question.

The subject is **browser live** only — the `live` broadcaster
(`go/internal/media/live/live.go`) and its client
(`go/static/js/views/mse.js`). Recording, the RTSP relay and HLS VOD are
out of scope: MoQ offers them nothing they don't already have.

See [`doc/MEDIA.md`](../MEDIA.md) for the engine as it stands.

> **Premise superseded (2026-08-15).** The shared motivation below —
> that browser live latency is worth reducing — was subsequently
> rejected as a product goal when [`WEBRTC.md`](WEBRTC.md) was declined:
> the Android/TV clients already have sub-second live over the RTSP
> relay, and the web UI is for consultation, where ~1-2 s is adequate.
> That verdict applies to this document too, and more strongly — MoQ
> costs more than WebRTC for the same benefit. Anything below stands as
> analysis, not as a case for doing the work.

## Why this is on the table

The live path is documented at **~1-2 s** of latency. For watching a
driveway that is fine. For the two closed loops in the product it is not:

- **PTZ.** Double-clicking a wall tile centers the camera on that point
  (`videoClickToPanTilt` in `js/views/wall.js`). The operator is aiming
  at where the subject was 1.5 s ago, then waits another 1.5 s to see
  whether the move landed.
- **Push-to-talk.** `doc/TALK.md`'s backchannel is one-way audio out; the
  operator's only feedback that it worked is the video, 1.5 s later.

MoQ's headline is sub-300 ms. That is the whole reason to look at it.

## Where the current ~1-2 s actually goes

Worth being precise, because most of it is **not** the transport, and one
part of it is deliberate:

| Contributor | Value | Where |
|---|---|---|
| Camera encoder + RTSP ingest | ~50-150 ms | out of our hands |
| fMP4 part accumulation | ≤ 300 ms | `live.go:33` `partDuration`; a keyframe also forces a boundary, so real cadence follows the camera GOP |
| Network + chunked-HTTP flush | ~10-50 ms LAN | `HandleStream` flushes per part |
| **Client jitter buffer** | **1.2 s target, drifting to 2.0 s** | `mse.js:16` `TARGET`; catch-up at 1.08x only once `behind > 2.0 s`, hard re-seek at 5 s |

So the dominant term is the client's own buffer, and **it is not
accidental**. The play head rides 1.2 s behind the live edge because the
transport underneath is TCP: one lost packet stalls the whole byte
stream, MSE has no way to skip a late part, and a `<video>` that
underruns goes to `waiting` and shows the buffering overlay. The 1.2 s is
the price of hiding TCP head-of-line blocking from MediaSource.

**That is the actual argument for MoQ**: not that QUIC is faster on the
wire, but that per-object streams with drop-old semantics let the jitter
buffer be 200 ms instead of 1200 ms without the picture falling apart on
a bad link. Any plan that just lowers `TARGET` on the current transport
trades latency for stutter.

## MoQ state of the world (August 2026)

- `draft-ietf-moq-transport` is at **-18** (30 Jul 2026), Standards
  Track, approaching WG Last Call. Monthly revisions — the wire format
  still moves.
- **WebTransport went Baseline in March 2026** with Safari 26.4 / iOS
  26.4. This removed the historical blocker: until 2025 any MoQ player
  was Chrome-and-Firefox-only, which for a product with iOS viewers was
  disqualifying. It no longer is.
- Cloudflare runs MoQ relays on its edge; 11 vendors interoperated at
  NAB 2026.

### MediaMTX already ships it

Relevant because Eneverre's engine is a reimplementation of what
MediaMTX did, on the same bluenviron libraries, and MediaMTX is MIT.

| | |
|---|---|
| Since | `v1.20.0` (publish + read), draft-16/17; later releases already on draft-19 |
| Transports | WebTransport (HTTP/3) for browsers + native QUIC server-to-server |
| Built on | `quic-go` + `webtransport-go` — pure Go, no cgo |
| Codecs | H264, H265, VP8/VP9, AV1 + AAC, **G711**, LPCM, Opus, FLAC |
| Ports | `moqHTTP2Address :8892` (TCP, hosts the web client), `moqHTTP3Address :8892` (UDP, WebTransport), `moqQUICAddress :8893` |
| TLS | its own `moqServerKey` / `moqServerCert` |
| Web client | included; WebTransport + WebCodecs + Web Audio |

Two consequences for us:

1. The implementation lives under MediaMTX's `internal/`, so it is **not
   importable** as a module — unlike `gortsplib` / `mediacommon` /
   `gohlslib`, which we already depend on. Adopting it means copying MIT
   code and owning it.
2. bluenviron's pattern is to *extract* libraries out of MediaMTX (that
   is where gortsplib and gohlslib came from). **A published, importable
   `gomoq`-style library is the single strongest signal to revisit this
   plan** — it collapses the server half of the work from weeks to days.

The codec row matters too: MoQ carries G711 natively, which kills the
obvious "we'd have to transcode our audio" objection.

## What Eneverre would gain

1. **Latency 1.2 s → ~200-300 ms**, for the reason argued above. Fixes
   the PTZ and talk loops.
2. **Audio on G711-only cameras.** Today `SetTracks` drops every non-AAC
   audio track from the browser stream (`live.go:194`) and logs
   *"browser stream is video-only"* once (`live.go:208`) — MSE cannot
   decode G711/LPCM. A WebCodecs/Web-Audio player decodes G711 in ~30
   lines of JS. This is a **feature gain, not just a latency gain**, and
   it applies to the ONVIF cameras that ship G711 only.
3. **One connection, N tracks, with priorities.** A 16-tile wall is 16
   independent chunked-HTTP streams today. MoQ makes it one QUIC
   connection where the zoomed tile can outrank the rest and the others
   degrade instead of competing. This maps unusually well onto the wall's
   existing zoom behaviour.
4. **Behaves on bad links.** No head-of-line blocking; connection
   migration survives wifi↔cellular handover, which currently kills the
   `<video>` and triggers the reconnect backoff.

What it does **not** buy:

- **H265 in browsers.** WebCodecs goes through the same platform decoder
  as MSE, so the gate documented in
  [`doc/MEDIA.md`](../MEDIA.md#h265hevc--browser-live-is-client-gated) is
  unchanged.
- **Anything for recording, VOD or the RTSP relay.** The segment index,
  HLS VOD and the relay are already the right tools.
- **Anything for the Android/TV clients.** They read the RTSP relay and
  that works.

## What it would cost

### Server side — the cheap half

The broadcaster is already in the right shape. `writeSample` receives
finalized `fmp4.Sample`s with durations and keyframe flags and groups
them into parts at GOP boundaries; a MoQ publisher wants exactly that,
as objects grouped per GOP. The fan-out (`subs`, drop-on-slow) is the
same pattern MoQ subscribers need.

- New deps: `quic-go` + `webtransport-go`. Both pure Go, so
  `CGO_ENABLED=0` and the `linux-arm` / `linux-arm64` release targets
  survive. Caveat: quic-go wants a raised UDP receive buffer
  (`net.core.rmem_max`) and logs a warning otherwise — one more line in
  the systemd install notes.
- No importable library. `mengelbart/moqtransport` is Go but sits on
  **draft-11**, seven revisions behind. `moq.dev` is Rust with C/FFI
  bindings — adopting it would mean cgo, which breaks the pure-Go static
  build and the cross-compiled arm releases. So the realistic path is
  porting MediaMTX's MIT implementation.
- Draft churn is then ours to track: MediaMTX has already moved
  16 → 17 → 19 in months.

### Browser side — the expensive half

MoQ playback is not `<video>` + MSE. It is WebTransport +
`VideoDecoder` + canvas + Web Audio + a hand-written jitter buffer and
A/V sync, in a frontend with **no build step**. And `mse.js` is not 270
lines of boilerplate — it is 270 lines of hard-won behaviour that all
has to be rebuilt:

- reconnect with exponential backoff, the connection-lost overlay and
  the Retry button;
- `pause()`/`resume()` so hidden tabs and off-screen tiles stop decoding
  *and* stop pulling bytes;
- `setCamStatus` connecting/online/offline transitions and the buffering
  overlay;
- the latency-control loop (catch-up rate, hard re-seek);
- the codec-support gate that today is one `MediaSource.isTypeSupported`
  call and would become a `VideoDecoder.isConfigSupported` promise.

Two more couplings that are easy to miss:

- **Thumbnails.** `captureVideoFrame` grabs frames off the playing
  `<video>` for sidebar thumbnails — that is deliberately how we avoid a
  server snapshot endpoint. A canvas-rendered player needs a different
  capture path.
- **PTZ click mapping.** `videoClickToPanTilt` maps a click to pan/tilt
  using the `<video>`'s displayed rect and `object-fit: contain`
  letterboxing. A `<canvas>` changes that geometry.

And the recurring cost: WebCodecs decode is hardware-accelerated, but
the JS glue and canvas compositing are not free per tile. WINK's
surveillance-focused MoQ work measured ~45% browser CPU and flagged
multi-stream monitoring specifically. A 16-tile wall is that scenario.

### Deployment — the part that breaks the product promise

This is the objection I weight highest, because it cuts against the
stated reason the embedded engine exists at all: *one binary, one systemd
unit, one auth surface, one port.*

- **Eneverre never terminates TLS.** Caddy does
  (`doc/example/Caddyfile`); the binary speaks plain HTTP on
  `127.0.0.1:8080`. WebTransport needs QUIC/UDP with a real certificate
  at the endpoint. Either Caddy proxies it — **its WebTransport
  passthrough was still experimental as of mid-2026** — or Eneverre grows
  its own cert handling and a UDP listener. Both are new operational
  surface where there is currently none.
- **It collides head-on with the talk workaround.** Our Caddyfile sets a
  global `protocols h1 h2` because Caddy will not translate
  WebSocket-over-HTTP/3 (see
  [`doc/TALK.md`](../TALK.md#deployment-gotcha-websocket-over-http3)).
  WebTransport *requires* HTTP/3. Under the Caddyfile we ship today,
  MoQ-over-WebTransport cannot work at all. (The flip side: if we ever do
  go WebTransport, push-to-talk should move onto it too and that whole
  gotcha disappears — a genuine consolidation, not a workaround.)
- **Auth is undesigned.** Every live/playback request carries a Bearer
  token today. Over WebTransport there is no per-request header; the
  token has to ride the connect URL or a setup message, which is a new
  auth path to get right — the same design problem the talk WebSocket
  solved with `Sec-WebSocket-Protocol`.
- UDP/443 is filtered on some corporate and guest networks, so a TCP
  fallback stays mandatory regardless. We cannot delete the MSE path.

## Cheaper routes to the same goal

| Option | Gets us to | Cost | Ceiling |
|---|---|---|---|
| Tune the existing MSE path | ~600-800 ms | days | Hard floor: below ~500 ms, TCP HOL blocking shows up as stutter on anything but a LAN |
| WebRTC (pion) | 200-500 ms | weeks | ICE/DTLS/SRTP complexity; no H265; but **G711 is a native WebRTC codec**, and it could carry the talk backchannel in the same session |
| MoQ | 200-300 ms | weeks + draft maintenance + new ops surface | Best long-term answer; simpler to route than WebRTC; not settled yet |

The WebRTC row deserves more weight than it usually gets here, and it is
now worked out in full in [`WEBRTC.md`](WEBRTC.md): `pion/rtp` and
`pion/sdp` are already in the tree, browser support is universal and a
decade old, H265 shipped in Chrome 136+/Safari 18, and it is the only
option that could collapse live video and two-way audio into one
session. Crucially it keeps the `<video>` element, so the thumbnail and
PTZ-click couplings listed above cost nothing.

There is also a historical pattern worth naming: WebRTC did not survive
the move to the embedded engine because its media does not traverse the
reverse proxy (UDP/ICE). That is structurally the *same* objection this
document raises against MoQ (UDP/HTTP-3 outside Caddy). This project has
already traded low latency for "one binary, one unit" once — see
[`WEBRTC.md`](WEBRTC.md#background--why-there-is-no-webrtc-today).

## Recommendation

**Do not adopt MoQ now.** Phased:

0. **Measure.** Instrument end-to-end glass-to-glass latency on a real
   camera and confirm the budget table above. Cheap, zero risk, and it
   settles whether the transport is even the problem.
1. **Tune.** Lower `TARGET`, tighten the catch-up thresholds, consider
   dropping `partDuration` below 300 ms. Accept the stutter/latency
   trade-off consciously instead of by default. If ~700 ms is enough for
   PTZ and talk, this plan ends here.
2. **If sub-500 ms is genuinely required**, run the MoQ-vs-WebRTC
   decision as its own document, with the deployment constraints above
   as first-class criteria rather than footnotes.
3. **Prototype behind a flag** (`[media] moq = false` by default,
   separate listener, MSE untouched) before anything ships. The MSE path
   stays the default and the UDP-blocked fallback permanently.

### Triggers to revisit

- An **importable Go MoQ library** appears — especially a bluenviron
  extraction, following the gortsplib/gohlslib precedent.
- The draft reaches **WG Last Call / RFC** and the wire format settles.
- **Caddy ships stable WebTransport passthrough**, so the deployment
  story stays "one reverse_proxy line".
- **Multi-site federation** becomes a product goal. This is the case
  where MoQ stops being "another live transport" and becomes the
  architecture: several NVRs across sites feeding a central operator is
  exactly the pub/sub + relay model MoQ was designed for, and nothing in
  the current stack addresses it.

### The escape hatch meanwhile

Anyone who needs MoQ today can run MediaMTX alongside and point it at
the camera, with `mse = false` / `relay = false` on that camera — the
same pattern [`doc/MEDIA.md`](../MEDIA.md) already documents for H265 and
WebRTC. Ironic given that MediaMTX was removed as a dependency, but it is
a zero-code answer and worth remembering before writing a QUIC stack.

## References

- [draft-ietf-moq-transport-18](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/)
- [MediaMTX — reading with Media-over-QUIC](https://mediamtx.org/docs/read/moq)
- [MediaMTX v1.20.0 release notes](https://github.com/bluenviron/mediamtx/releases/tag/v1.20.0)
- [WebTransport is now Baseline](https://webrtc.ventures/2026/04/webtransport-is-now-baseline-what-it-means-for-real-time-media/)
- [mengelbart/moqtransport (Go, draft-11)](https://github.com/mengelbart/moqtransport)
- [moq.dev (Rust + FFI)](https://doc.moq.dev/)
- [WINK — MoQ implementation analysis (surveillance)](https://www.wink.co/documentation/WINK-MoQ-Implementation-Analysis-2025.php)
- [Caddy — experimental WebTransport passthrough](https://github.com/caddyserver/caddy/actions/runs/26424097198)
