# Browser live (and talk) over WebRTC

Status: **evaluated and declined (2026-08-15).** Not because the plan
doesn't work — the technical path is clear and cheaper than it looks —
but because the premise doesn't hold for this product. See
[Outcome](#outcome--declined). The rest of the document is kept as the
decision record: it is the analysis that would otherwise be redone from
scratch, and it names the conditions under which the answer changes.

Scope: the **browser** live path (`go/internal/media/live` +
`go/static/js/views/mse.js`) and, additively, the **browser** leg of
push-to-talk. Recording, the segment index, HLS VOD, the RTSP relay and
the Android/TV clients are out of scope and unaffected.

Companion document: [`MOQ.md`](MOQ.md) evaluates Media-over-QUIC for the
same goal and concludes "not yet". This plan is the near-term
alternative — read [MOQ.md's latency budget](MOQ.md#where-the-current-1-2-s-actually-goes)
first, it is the shared premise.

## Background — why there is no WebRTC today

Worth recording, because it was never written down and the omission looks
like a decision it wasn't.

In the MediaMTX era (`404fab8`, 2026-06-28) `webrtc` was a **URL field**
on the camera model — a WHEP endpoint served by MediaMTX. Eneverre only
brokered authorization and handed clients the URL with rotating
credentials. There was never any WebRTC code in this tree.

Then, all on 2026-07-07:

- `40398ef` *("We simply stole mediaMtx 🙉")* reimplemented the recorder,
  RTSP relay, live MSE and HLS VOD in-process. WebRTC was **not**
  reimplemented; the docs of that commit simply state
  `` `hls`, `webrtc` | empty (not served by the engine) ``.
- `fa774e6` deleted `doc/MEDIAMTX.md` and the external mode.
- `22d4386` removed the `hls`/`webrtc` fields from the camera model
  entirely, leaving today's line in `doc/MEDIA.md`: *"front the camera
  with an external streamer"*.

So it was **an omission, not a rejection** — no commit or document ever
evaluated and declined WebRTC. The one recorded reservation is the
reverse-proxy table in the deleted `doc/MEDIAMTX.md`:

> | WebRTC (WHEP signaling) | Yes | Plain HTTP. |
> | WebRTC (media) | **No** | UDP (RTP / ICE). Expose MediaMTX's WebRTC UDP ports directly or tunnel them. |

That is still the main operational objection, and it is addressed below.

## What changed since

1. **H265 over WebRTC shipped.** Chrome 136+ carries HEVC in WebRTC by
   default when the platform has a hardware decoder; Safari since 18.
   Firefox never will. Against the MSE gate documented in
   [`doc/MEDIA.md`](../MEDIA.md#h265hevc--browser-live-is-client-gated)
   this is near-parity, with one small regression: a Firefox with a
   system HEVC decoder plays H265 over MSE today and never would over
   WebRTC.
2. **Every camera in the fleet can emit G711**; AAC is a per-camera
   quality preference, not a constraint. This is what makes the audio
   question below tractable rather than blocking.

## What WebRTC would buy

1. **Latency 1.2 s → 200-500 ms.** The current 1.2 s is the client-side
   jitter buffer that hides TCP head-of-line blocking from MediaSource
   (`mse.js:16`); WebRTC does not need it. This fixes the two closed
   loops: aiming a PTZ camera by double-clicking a tile, and hearing the
   result of a push-to-talk.
2. **Audio on every camera.** Today `SetTracks` drops every non-AAC audio
   track from the browser stream (`live.go:194`) and logs *"browser
   stream is video-only"* once — MSE cannot decode G711/LPCM. G711 is a
   **native WebRTC codec**, so with every camera able to emit it, any
   tile the operator unmutes actually produces sound. Today that only
   works on cameras configured for AAC.
3. **The `<video>` element survives.** `pc.ontrack → video.srcObject`.
   This is the decisive difference against MoQ: `captureVideoFrame`
   (sidebar thumbnails grabbed off the playing tile) keeps working, and
   `videoClickToPanTilt` — which maps a click through the `<video>`'s
   displayed rect and `object-fit: contain` letterboxing — is untouched.
   A canvas-based player breaks both.
4. **Auth and signaling need no new design.** WHEP is an HTTP `POST` of
   an SDP offer; the Bearer token rides the same header as every other
   endpoint. It also passes through Caddy unchanged and does **not**
   collide with the `protocols h1 h2` cap that push-to-talk requires
   (unlike WebTransport, which mandates HTTP/3).
5. **Better browser-side talk audio** — see [Talk](#talk-additive-not-a-replacement).

What it does **not** buy: anything for recording, VOD, or the RTSP relay;
anything for the Android/TV clients (they read RTSP and that works).

## The audio trade-off

The codec matrix is an exact mirror image, and this is the only genuine
functional tension in the whole plan:

| Camera audio | Live MSE (today) | Live WebRTC |
|---|---|---|
| G711 | **silent** (dropped, `live.go:194`) | **audible** |
| AAC | audible | **silent** (browsers do not decode AAC over WebRTC) |

Transcoding out of the tension is **not available**: there is no
production-grade pure-Go AAC decoder or Opus encoder (`pion/opus`'s
encoder is not one), and reaching for ffmpeg or a cgo binding breaks
`CGO_ENABLED=0` and the cross-compiled `linux-arm` / `linux-arm64`
release targets. This is a hard constraint, not a preference.

Because every camera can emit G711, the resolution is a **documented
per-camera choice** rather than a blocker:

- **G711 source** → browser live audio + talk + low latency. Recording
  audio is narrowband (telephone quality), which for voice
  intelligibility — what surveillance audio is actually for — is
  adequate.
- **AAC source** → better recording fidelity; browser live is
  video-only, exactly as G711 cameras are today.

Either way, recording and the RTSP relay carry whatever the camera sends,
unchanged. The choice only affects what the browser hears.

## Where it plugs in — server

The integration point already exists and is a one-liner. `engine.go:708`
fans the recorder's RTP packets to the relay:

```go
rec.OnRTP = func(m *description.Media, pkt *rtp.Packet) { e.relay.WritePacketRTP(id, m, pkt) }
```

A WebRTC publisher is a **second consumer of that same callback** — the
same packets, no new demuxing, no re-encode. New package
`go/internal/media/webrtc`, shaped like `liverelay` (a `SetSource` /
`ClearSource` / `WritePacketRTP` triple plus per-subscriber fan-out).

Details that are already solved or need deciding:

- **H264 packetization mode.** Browsers require mode 1.
  `promoteH264PacketizationMode` (`liverelay.go:111`) already exists and
  already documents that mode-0 cameras in fact send FU-A, so promotion
  is safe and passthrough works. Nothing new needed.
- **Keyframe on join.** A new subscriber cannot decode until an IDR
  arrives. Either wait for the camera's next one (adds up to one GOP of
  join latency) or reuse the current-GOP buffer the live broadcaster
  already maintains (`live.go` `gop`). The second is better and the data
  structure exists.
- **SRTP per subscriber** is the new recurring cost. Today one fMP4 mux
  is shared by all viewers; WebRTC encrypts per peer. On a 16-tile wall
  with several operators this is real CPU, and the release targets
  include 32-bit ARM. Go uses the ARMv8 crypto extensions, so it is
  probably fine — **measure on the actual target before committing**, do
  not assume.
- **Dependencies:** `pion/webrtc/v4` plus ice/dtls/sctp/stun/interceptor.
  All pure Go, so `CGO_ENABLED=0` and the arm builds survive. The
  incremental surface is smaller than it looks: `pion/rtp`, `pion/rtcp`,
  `pion/sdp/v3`, `pion/srtp/v3` and `pion/transport/v4` are **already**
  in `go.mod` as indirect deps via gortsplib.

## Where it plugs in — client

New `go/static/js/views/webrtc.js`, roughly 200 lines against `mse.js`'s
303, because the hard parts become the browser's job. It reuses the
existing UI machinery verbatim: `ensureTileBuffering`, `setCamStatus`
connecting/online/offline, the reconnect backoff and Retry button, and
`pause()`/`resume()` so hidden tabs and off-screen tiles stop decoding
and stop pulling bytes.

What changes: `MediaSource` + `SourceBuffer` + the latency-control
interval all collapse into `video.srcObject = stream`. The codec gate
moves from `MediaSource.isTypeSupported` to
`RTCRtpReceiver.getCapabilities("video")`, which is what decides the
H265 message today.

## Talk — additive, not a replacement

**Correction to an earlier estimate**: this does *not* delete the
existing backchannel code. [`doc/TALK.md`](../TALK.md) documents the
WebSocket+PCM protocol as the **Android** client contract (Kotlin/OkHttp,
with a full reference implementation). Android is not migrating, so the
WS ingest path, the resampler and the G711 encoder all stay.

What WebRTC adds for the **browser** leg:

- The browser can be steered (via `setCodecPreferences`) to send
  **PCMU/PCMA at 8 kHz mono** — exactly what the camera's backchannel
  wants. The server then repacketizes RTP toward the camera instead of
  running anti-alias LPF → linear resample → G711 encode → 20 ms
  framing.
- Free from the browser: acoustic echo cancellation, AGC, noise
  suppression, jitter buffering and packet-loss concealment. Several of
  these are open items in
  [`TALK-AUDIO-QUALITY.md`](TALK-AUDIO-QUALITY.md) (AudioWorklet
  capture, stateful resampling) that would simply stop mattering for
  browser clients.
- Live and talk share one `RTCPeerConnection` (sendrecv audio) instead
  of a video stream plus a separate WebSocket.

Cost: a **second ingest path** into `internal/backchannel` — a
`FeedG711RTP`-style entry point alongside `FeedPCM` (~50 lines,
additive). The one-session-per-camera lock in `handlers_talk.go` and the
camera-side RTSP session are unchanged.

## Deployment

- **Signaling** (WHEP `POST`) goes through Caddy with the existing single
  `reverse_proxy` line. No new rules, no HTTP/3 requirement.
- **Media** does not traverse the proxy — but the precedent already
  exists: the RTSP relay on `:8554` is documented as exposed directly and
  firewalled. Following MediaMTX's design, WebRTC needs **one fixed UDP
  port** (e.g. `:8189`) with an optional **ICE-TCP listener on the same
  number** as a fallback for networks that filter UDP. So the product
  promise at stake is "one binary, one systemd unit" — which survives —
  not "one port", which was never true.

### The NAT question, stated correctly

WebRTC's reputation for NAT pain comes from the peer-to-peer case: two
browsers, each behind its own NAT, hole-punching. **This is not that
case.** Here the browser is a client connecting to a known server, so it
*initiates*: its NAT opens an outbound UDP mapping like for any other
traffic, and pion replies to the source address it observes. That works
through **any** client-side NAT, symmetric included.

The only question is therefore: **can the browser reach one UDP port on
the NVR?** Which is precisely the question the RTSP relay on `:8554`
already answers in every deployment.

The server needs two things: a reachable UDP port, and knowledge of its
own public address to advertise as an ICE candidate (pion only knows its
local IPs; behind a home router it would otherwise advertise
`192.168.1.x`). The second already has an exact precedent in this
codebase — `[media] rtsp_host` exists to pin the public host in
reverse-proxied deployments for the same reason. A `webrtc_host` key is
the same pattern, not new conceptual surface.

| Topology | Needs | Outcome |
|---|---|---|
| LAN (browser and NVR on one network) | nothing | works unconfigured |
| NVR on a public IP, Caddy on the same host | nothing — the host candidate *is* public | works unconfigured |
| NVR at home, Caddy on the same host, router port-forwarding | `webrtc_host` + forward UDP 8189 | works; one config key, one firewall rule |
| Caddy on a **different** host from the NVR (VPS fronting a home NVR) | DNS points at the VPS, not the NVR — media has no path | **falls back to MSE** |
| CGNAT / no ability to open a port | TURN or a tunnel | **falls back to MSE** |
| Client network filters outbound UDP | ICE-TCP on 8189, if that network permits it | **falls back to MSE** |

**TURN is out of scope.** It is only relevant when the NVR itself is
unreachable (the last three rows), and for those the answer is the
retained MSE path, not a TURN deployment.

## Cost

| Piece | Work | Estimate |
|---|---|---|
| `internal/media/webrtc`: WHEP endpoint, peer lifecycle, RTP passthrough off `rec.OnRTP`, keyframe-on-join | ~600-900 new lines | 4-7 days |
| `js/views/webrtc.js` reusing the existing overlays/status/pause machinery | ~200 lines | 2-3 days |
| Browser talk over the same PeerConnection + `FeedG711RTP` ingest | ~50 lines server, ~80 client | 3-5 days |
| `[media] webrtc_*` config, Caddyfile note, firewall guidance, `doc/MEDIA.md` + `doc/TALK.md` updates | — | 1-2 days |
| Cross-browser (Chrome/Firefox/Safari), LAN + remote, and an ARM CPU measurement | — | 2-3 days |
| **Total** | | **~2.5-3 weeks**, medium risk |

Compared with [MoQ](MOQ.md): less work, no draft-tracking treadmill, no
new auth design, no TLS/UDP termination where we terminate none today,
and it keeps the `<video>` element. The one thing MoQ would do better is
per-track priorities across a multi-tile wall.

## Outcome — declined

**The latency premise does not hold.** The Android/TV clients already
have sub-second live through the RTSP relay, which is where operational
use actually happens. The web UI is for **consultation** — checking a
camera, reviewing the timeline — and ~1-2 s is entirely adequate for
that. Three weeks of work plus a permanent second live path buys a
latency improvement on the surface that needs it least.

Two facts measured during the evaluation reinforce it (see
[Verify before building](#verify-before-building)): Firefox advertises
only **baseline** H264 over WebRTC while the fleet encodes **Main**, and
Firefox has no HEVC in WebRTC at all. So a slice of browser users would
have stayed on MSE regardless, further shrinking the return.

One thing is knowingly traded away and worth stating plainly: PTZ
aiming (double-click to centre a tile) and push-to-talk are **browser**
features whose closed loop keeps the ~1-2 s delay. That is accepted as
the cost of not maintaining a second live transport.

### What would reopen it

- The web UI acquires a genuinely operational role — live PTZ driving,
  or talk used as a real conversation rather than an announcement.
- The fleet moves to Baseline H264 + G711 for other reasons, which would
  make WebRTC work everywhere with no compatibility caveats at all.
- Browser live becomes the primary surface for some deployment (no
  Android clients), so the RTSP relay stops covering the low-latency
  case.

### Design decisions reached before declining

Kept because they were settled, and a future revisit should not
re-litigate them:

1. **Two live paths is accepted.** `AGENTS.md` records that the HLS live
   fallback was deliberately removed ("There is no live fallback
   anymore"), so this reverses a prior simplification knowingly: MSE
   stays for UDP-filtered networks, for unreachable-NVR topologies and
   for AAC-audio cameras. The maintenance cost is permanent, not a
   one-off, and is accepted.
2. **Both transports are published; the client chooses.** Not a
   server-side fallback. `GET /api/cameras` grows a `live_webrtc` field
   alongside `live_mse`, each present only when that feature resolves on
   for the camera (the same `ResolveFeatures` pattern the existing
   switches use), and every client decides for itself which it prefers.

   This is deliberately the shape the API already had — `rtsp`, `hls`
   and `webrtc` were published side by side in the MediaMTX era — and it
   is strictly better than an automatic fallback: no ICE-failure
   detection, no switching rule, and no tile that can flap between
   transports. A client on a UDP-filtered network, or one that simply
   prefers the `<video>`+MSE path, just uses the URL it wants. It also
   makes the AAC case a client-side observation rather than a
   server-side policy: the camera advertises both, and a client that
   wants audio from an AAC source picks MSE.
3. **No TURN.** See [the NAT question](#the-nat-question-stated-correctly):
   client-side NAT is irrelevant, and the topologies that TURN would
   rescue are exactly the ones the retained MSE path covers.

### Left unanswered

Moot now, but these were the remaining unknowns: which cameras stay on
AAC (a per-camera fidelity call), and per-subscriber SRTP CPU on the
32-bit ARM target — the one number that could still have changed the
plan's shape.

## Verify before building

None of this needs code. Two steps, in order:

### 1. Enumerate what each browser will actually receive

In the console of each browser that matters (Chrome, Firefox, Safari, on
the machines operators really use):

```js
RTCRtpReceiver.getCapabilities("video").codecs
  .filter(c => /H26[45]/i.test(c.mimeType))
  .map(c => `${c.mimeType}  ${c.sdpFmtpLine || ""}`)
```

This settles the H265 question definitively per browser and machine
(HEVC in WebRTC is hardware-gated, so it is a per-machine answer, not a
per-browser one).

It also answers a question worth more attention than H265: **which H264
profiles** each browser offers. `profile-level-id` in `sdpFmtpLine`
distinguishes constrained baseline (`42e0…`) from main (`4d00…`) and
high (`640c…`). Every camera in the fleet is H264, so this matters far
more than the H265 edge case. Run the same query with `"audio"` to
confirm PCMU/PCMA are offered.

**Measured, 2026-08-15 — Firefox:**

```
video/H264  profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1
video/H264  profile-level-id=42e01f;level-asymmetry-allowed=1
video/H264  profile-level-id=42001f;level-asymmetry-allowed=1;packetization-mode=1
video/H264  profile-level-id=42001f;level-asymmetry-allowed=1
```

No H265 at all, and H264 **baseline only** (`42e0` constrained baseline,
`4200` baseline) — no `4d00` (main), no `640c` (high).

**Measured — camera `jardin`**, read straight off the existing live
endpoint, whose `mime` carries the codec string `avcCodec()` builds from
the SPS (`avc1.<profile><constraints><level>`):

```console
$ curl -s -u USER:PASS host:8080/api/camera/jardin/live/info
{"available":true,"mime":"video/mp4; codecs=\"avc1.4d0032\""}
```

`4d` = **Main profile**, level 5.0. So the fleet encodes a profile
Firefox does not advertise. The level mismatch (5.0 vs 3.1) is secondary:
`level-asymmetry-allowed=1` permits it and cameras routinely over-declare
level.

This does **not** prove Firefox would fail to decode Main — advertised
capabilities are what a browser offers to negotiate, not necessarily what
its decoder handles, and on platforms where Firefox decodes through the
system decoder Main plays fine. Resolving it was the job of the spike
below. It did establish that a meaningful share of browser users would
have stayed on MSE anyway, which is part of why this plan was declined.

### 2. End-to-end spike with MediaMTX, before writing any Go

MediaMTX implements exactly the design this plan proposes — RTP
passthrough from an RTSP source into pion, no re-encode — on the same
libraries. Point a container at one real camera and open its WebRTC page.
In an afternoon that validates, against the real fleet: H264 profile
compatibility per browser, H265 where present, G711 audio actually
arriving, real glass-to-glass latency, and per-subscriber CPU on the ARM
target.

If the spike fails, it fails for free. If it passes, the plan's main
risks are retired before the first line of `internal/media/webrtc`.

### Known: Firefox and H265

Firefox decodes HEVC via MSE where a hardware decoder exists, but
**does not expose it to WebRTC** and has made no commitment to
(mozilla/standards-positions#1188). So the regression in the table above
is real, though its blast radius is only: an H265 camera **and** a
Firefox user **and** a machine with an HEVC decoder. With both transports
published (decision 2), that user simply picks `live_mse`.

## References

- [Chrome: H265 (HEVC) codec support in WebRTC](https://chromestatus.com/feature/5153479456456704)
- [MediaMTX — WebRTC-specific features (single-port UDP/TCP ICE)](https://mediamtx.org/docs/features/webrtc-specific-features)
- [pion/webrtc](https://github.com/pion/webrtc)
- [pion/opus](https://github.com/pion/opus)
