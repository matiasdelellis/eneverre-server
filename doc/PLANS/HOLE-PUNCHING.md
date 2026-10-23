# UDP hole punching for Eneverre — investigation

Status: **investigated; not recommended.** The idea does not fit this
topology — the one peer that would need to punch (the camera) cannot — and
the goal behind it ("more efficient playback, without exposing extra ports")
is already reachable over the single HTTPS port the product already
exposes. This document is the analysis, so the question does not have to
be re-litigated: it records what hole punching would mean here, the
measured constraints that decide it, and what to do instead.

Scope: the media paths between the **Android/TV client** and the
**server** (live + recording playback), and — for completeness — the
path between the client and the **camera**. The camera→server hop is
untouched by all of this.

Companion documents: [`WEBRTC.md`](WEBRTC.md) (browser live over WebRTC —
evaluated and declined, with the design decisions kept) and
[`MOQ.md`](MOQ.md) (Media-over-QUIC — investigated, not planned). The
verdict here is compatible with both: they were declined for the browser
surface, and the reasons do not change because the client is Android.

## TL;DR

| Question | Answer |
|---|---|
| Can UDP hole punching work between the Android client and the server? | Technically yes, but it buys nothing: the server is the reachable end. Punching exists to reach a peer that is *not* reachable; here the server is. To get a UDP path to it you still have to open a UDP port — the exact thing being avoided. |
| Can it work between the Android client and the camera? | Only if the camera runs an ICE/STUN agent. The fleet's cameras (RTSP-only thingino firmware) do not, and the engine only ever sees an RTSP `source` URL. This is the only pairing where hole punching would have real value (bypass the server for live), and it needs camera-side firmware work. |
| Does the *playback* path need NAT traversal at all? | No. Recording playback is plain HTTPS to a public API — the client always initiates, so there is nothing to punch. Its inefficiency is server-side CPU/disk, not transport. |
| What actually delivers the goal? | (1) Playback: serve recording playback from the **HLS/CMAF VOD** endpoints the engine already has (`/api/camera/{id}/recordings/hls/*`), which the web UI already plays with hls.js — the Android app today re-muxes on the server per 10 s window instead. (2) Live: keep the RTSP relay but carry it over the **existing HTTPS port** as a WebSocket-tunnelled RTSP session, so `:8554` stops being a public port. Neither needs a new port, a new codec path, or a new dependency. |

## Background — what "hole punching" is, in one paragraph

Two peers that cannot reach each other directly (each behind a NAT)
exchange their *reflexive* addresses through a third party — a STUN
server for the address discovery, plus an application-level signalling
channel to tell each other what it learned. Each then sends UDP packets
to the other's reflexive address; the outgoing packet creates a mapping
in the local NAT, and the incoming packet from the now-expected 5-tuple
is let through. The "hole" is that mapping, and it dies with the NAT's
idle timeout (30–300 s) or with a network change (Wi-Fi → cellular).

It is a *reachability* technique, not a *cost* technique: it lets two
NATed peers talk directly so no third party has to carry the bytes. It
does not create reachability where there is none, and it does not
encrypt anything.

## Which pair could punch?

### Android ↔ camera — the only pair with real value, and it is closed

This is the pairing that would actually pay off: the client would pull
video straight from the camera, and the server would stop being on the
media path for live (bandwidth, CPU and latency all improve at once).

It requires the camera to be an **ICE participant**: it must open a UDP
socket, learn its reflexive address via STUN, and accept media from an
arbitrary peer. Nothing in the fleet does this:

- The engine's contract with a camera is an RTSP `source` URL
  (`doc/MEDIA.md`, "Camera source"). There is no signalling channel, no
  candidate exchange, no ICE agent on the camera side — and the camera
  INI has no knob that would conjure one.
- Consumer cameras that do this (the P2P ecosystems behind the vendor
  apps) implement it **in the vendor cloud**: the camera registers with
  the vendor, the vendor's STUN/TURN infrastructure brokers the peers,
  and the vendor relays the media when punching fails. That cloud *is*
  the server, with a different owner and a per-minute bill.
- The fleet's firmware is **thingino** (this repo's sibling,
  `thingino-pkgbuild`), so a camera-side ICE agent is *conceivable* —
  but that is a firmware project of its own (UDP agent, STUN client,
  SRTP, key management, remote config, fallback to the existing relay),
  and it would put a new attack surface on every camera. It is not a
  "hole punching" feature; it is a second media stack in the firmware.

### Android ↔ server — punching where reachability already exists

The server is the end that holds the media. In every real deployment it
is either a VPS with a public address, or a box on the operator's own
network with a port forward. Either way the client can reach it *right
now* over TCP 443. Hole punching would only be interesting if the server
were unreachable — and then a UDP path would have to be punched from a
port the server's own NAT opens on its own, with:

- **A STUN dependency** to learn the client's reflexive address (the
  client learns the server's, but a symmetric NAT on the server side
  would need a per-destination port).
- **No TLS.** The path bypasses Caddy and the API's TLS termination, so
  the media would need its own encryption (DTLS-SRTP). The RTSP relay's
  rotating credentials would also stop being the gate they are today.
- **A fragile path.** NAT mapping lifetime, network changes and
  UDP filtering break it; the design has to carry a fallback anyway
  (see the *iroh* model below: relay first, direct path opportunistic).
- **No upside on ports.** If the server has a public IP, "no extra port"
  is achieved by simply not needing one (TCP 443 already exists) — not
  by punching.

## The measured constraints

The deciding facts are about NAT behaviour, and they are well
characterised:

- **Mapping behaviour decides everything.** With *endpoint-independent
  mapping* (EIM, "cone") punching works reliably; with
  *address-and-port-dependent mapping* (APDM, "symmetric") it fails and
  ICE falls back to a TURN relay — i.e. to the very relay we already
  have, with extra machinery.
- **CGNAT is a second NAT.** RFC 6888 asks carriers for EIM mapping
  (REQ-2 paired pooling) and *recommends* endpoint-independent
  filtering (REQ-7), so a compliant CGN is punchable. Compliance is
  uneven: a 2026 measurement across European and North American mobile
  operators reports **symmetric (EDM) CGNAT on TIM, WindTre and AT&T**
  and endpoint-dependent mapping on T-Mobile (punching fails), against
  EIM behaviour on Vodafone and EE/BT (punching works). A phone on a
  home Wi-Fi behind a consumer router is usually EIM and fine.
- **UDP filtering is common** on corporate, hotel and some carrier
  networks — then only ICE-over-TCP or a relay works, i.e. the same
  byte path as today, with more steps.
- **The punched path needs keepalives and re-punching.** Mappings expire
  in 30–300 s; a phone that moves between Wi-Fi and cellular restarts
  the whole dance. Modern implementations handle it (the *iroh* model:
  relay first, direct path opportunistic, path monitoring, silent
  re-punch), but that is a networking stack to maintain, not a feature.
- **No encryption, no ordering, no retransmission** come for free — the
  application owns all of it (DTLS-SRTP, jitter buffer, NACK).

None of this is fatal *in general*; it is fatal *for this product*,
because the peer that would need to punch cannot.

## What the stack already gives us (no new ports required)

This is the part that changes the conclusion. The engine already serves
two media surfaces over the **same HTTP port as the API** — the one the
product already exposes through Caddy:

| Surface | Endpoint | Client today |
|---|---|---|
| Live fMP4 (chunked HTTP, MSE) | `GET /api/camera/{id}/live/stream` + `/live/info` | web UI only (`js/views/mse.js`) |
| Recording playback, HLS/CMAF VOD | `GET /api/camera/{id}/recordings/hls/{playlist.m3u8,init.mp4,segment.m4s}` | web UI only (hls.js) |
| RTSP relay | `rtsp://<user>:<pass>@<host>:8554/<id>` | **Android app** (this is the port we want to stop exposing) |
| Recording playback, one-shot clip | `GET /api/camera/{id}/recordings/get?start=&duration=` | **Android app** (10 s windows) |

So the "no extra port" goal has two concrete routes that need **no new
server transport at all**, and one that needs a small tunnel.

### Route 1 — playback: use the CMAF VOD the engine already serves

The Android app plays recordings by asking the server for **10-second
windows** (`ApiClient.getPlaybackStreamUrl` →
`/recordings/get?start=&duration=10`) and feeding them to ExoPlayer as
a growing playlist (`player/GaplessPlaybackController.java`,
`CHUNK_MS = 10_000`).

Every one of those requests makes the server **re-mux from disk**:
`internal/media/playback/server.go:seekAndMux` re-opens the recorded
segment file(s), re-parses the init, and re-writes an `ftyp+moov+moof+mdat`
stream for the requested window. The response carries a *fresh* init
segment, so the decoder re-initialises at every window boundary — the
cost that the gapless controller exists to paper over, and the reason
"one error per session"/re-init handling shows up in the player.

The engine already serves the same footage as **CMAF HLS VOD**, and
that path is a *segment* path, not a *window* path: the playlist
enumerates the recorded segments (with `EXT-X-PROGRAM-DATE-TIME` for
wall-clock mapping and `EXT-X-DISCONTINUITY` at real gaps), and each
segment is fetched as one media segment. The web UI already plays it
with hls.js, over the API port, with the Bearer token.

On the client that means Media3's `HlsMediaSource`
(`media3-exoplayer-hls`, a new but standard dependency) against the
existing endpoints, with the Bearer header attached to every playlist /
init / segment request. The scrubber gets wall-clock mapping for free
from `EXT-X-PROGRAM-DATE-TIME`, which is exactly what
`PlaybackTimeHost` reconstructs by hand today.

Result: **the server stops re-muxing per 10 s window**, the client gets
real seeking (a segment list, not a chunk cursor), the round-trip count
drops by ~6×, and **no port is exposed that isn't exposed already**.
Server-side code: none. Client-side: replace the chunk cursor with an
HLS playlist + timeline mapping.

Optional follow-up on the server: cache the re-muxed CMAF segments
(the `HandleHLSSegment` path re-muxes on every request too) so repeated
playback of the same window is free — a small on-disk cache next to the
existing gap-fill cache.

### Route 2 — live: carry RTSP over the existing HTTPS port

If the goal is to stop publishing `:8554`, the RTSP session does not have
to leave the server: it can be **tunnelled over a WebSocket on the API
port**, which is exactly what MediaMTX offers (`ws://host:port/<path>`
multiplexes RTSP over WebSocket) and what the engine's own push-to-talk
path already does — `gorilla/websocket` is a dependency and
`internal/server/handlers_talk.go` already upgrades connections.

Server side: a `GET /api/camera/{id}/rtsp` endpoint that upgrades to a
WebSocket (authenticated with the same Bearer token) and splices raw
bytes to the relay's in-process listener. The RTSP handshake still
carries the rotating stream credentials, so the relay's auth is
unchanged; the client keeps using the same RTSP request URI
(`rtsp://<host>/<id>`), because the relay routes by path, not by
transport. Caddy proxies WebSocket upgrades out of the box, and the
current Caddyfile already caps protocols at h1/h2 for the talk path.

Client side: the Android app's RTSP stack
(`com.github.alexeyvasilyev:rtsp-client-android`) creates its own socket
from `host:port` inside `RtspProcessor.init` — but its `RtspClient.Builder`
already accepts an arbitrary `java.net.Socket`. The change is therefore
small and contained: a tiny fork of the library adding a
`init(socket, uri, …)` overload (this repo has a local checkout of it),
plus a `Socket` adapter in the app that reads/writes the WebSocket's
binary frames.

Result: **`:8554` stops being a public port** (it can bind to
`127.0.0.1`), latency stays sub-second (same TCP characteristics as
today), no codec change, no new dependency, no DTLS/SRTP work, no
STUN dependency, and the failure modes stay exactly the ones the app
already handles.

The cost, stated plainly: RTSP-over-WebSocket is still TCP, so it
inherits TCP head-of-line blocking (it already does today) and pays a
small framing overhead; and the "live" path can no longer be opened by
a plain `nc`/VLC against the public host — which is a feature, not a
bug, for a product whose selling point is one port.

### Route 3 — live: the MSE feed the engine already serves, on the API port

The engine's live broadcaster (`internal/media/live`) already emits
init + parts as chunked fMP4 over `/api/camera/{id}/live/stream`, with
~1–2 s latency end to end — of which **1.2 s is the web client's own
jitter buffer**, deliberately sized to hide TCP head-of-line blocking
from `MediaSource` (`MOQ.md`, "Where the current ~1-2 s actually goes").

A native client does not need that buffer: ExoPlayer's `SourceBuffer`
path can run a few hundred ms. Consuming it in Media3 means a small
custom `MediaSource` (read the chunked response, append init + parts to
a `SourceBuffer`); Media3 1.11 ships `FragmentedMp4Extractor` — the
component behind its own fMP4 HLS/DASH paths — which is the natural
building block, and the browser's `mse.js` is the reference
implementation. This route is a pure client-side change and needs no
server work at all.

It is the *fallback* candidate, not the recommendation: it trades
latency for the removal of `:8554`, and it also drops the G711 audio
that the browser live path drops (the broadcaster drops non-AAC audio —
MSE cannot decode it). If `:8554` must go and RTSP semantics must stay,
Route 2 is the better trade.

### Route 4 — WebRTC/WHEP: the "proper" ICE path, and why it is not the answer here

WebRTC is the transport that *does* implement hole punching properly
(ICE: STUN-discovered srflx candidates, peer-reflexive candidates,
connectivity checks, and a TURN fallback). It is also the most
expensive option on the table:

- **Server**: `pion/webrtc` + `ice`/`dtls`/`sctp`/`stun` are *not* in
  `go.mod` today (only `pion/rtp`, `rtcp`, `sdp`, `srtp`, `transport` are,
  as indirect deps). `WEBRTC.md` estimates 600–900 new lines and 4–7
  days for the server side alone, and it was declined for the browser.
- **Client**: an Android WebRTC SDK is a ~15–21 MB AAR
  (`com.infobip:google-webrtc` 1.0.4824x is 14.8 MB; the community
  `getstream/webrtc-android` fork is what most teams use now that
  Google no longer publishes a maintained prebuilt AAR). That is a
  large APK change for a surveillance client.
- **Ports**: WebRTC needs a reachable UDP port (MediaMTX's
  `webrtcLocalUDPAddress` / `webrtcICEUDPMuxAddress`, or a static
  TCP+UDP mux on one port), *or* a STUN server and the random-port
  hole-punching mode — which is the very idea evaluated here, with the
  failure modes of §"The measured constraints" attached.

Verdict: not for the current goal. It would be the answer if the goal
became "sub-200 ms live with audio on the browser", which is not it.

### Route 5 — camera-side P2P (thingino)

The only route where hole punching has genuine value, and the one that
is out of reach without a firmware project. Noted here so the option is
not lost: if thingino ever grew an ICE agent, the client could pull
live straight from the camera and the server would drop off the live
media path entirely. The server would keep recording and relaying as
the fallback. Cost: a firmware feature, not a server feature.

## The modern shape of the idea (for completeness)

The "hole punching" idea has a well-solved modern form, and it is worth
naming because it is the *right* shape if this ever comes up: **relay
first, direct path opportunistic** (the *iroh* model). The client
always has a working path (the server relay — which exists here), and
in parallel tries to establish a direct path; when it succeeds, traffic
is promoted to it; when it dies (mapping expiry, network change), the
session falls back to the relay without the user noticing. MediaMTX's
WebRTC stack does the same thing, with a STUN server as the third
party and TURN as the last resort.

For Eneverre the relay is not a fallback, it is the product: the server
records, rotates credentials, hides the cameras' addresses, and serves
the timeline. A direct path would save *bandwidth*, not *ports*.

## Recommendation

1. **Do not implement hole punching.** It does not fit the topology, and
   it does not deliver the stated goal.
2. **Playback efficiency first (no new port, no server code):** move the
   Android app's recording playback to the existing HLS/CMAF VOD
   endpoints (`/api/camera/{id}/recordings/hls/*`) with Media3's
   `HlsMediaSource`. This is where the actual waste is: the server
   re-muxing a fresh fMP4 per 10 s window, and the decoder re-initialising
   at every window. Measure before/after: time and CPU of one 10-minute
   playback session, and the number of bytes the server re-muxes.
3. **Then, if `:8554` should stop being public:** add the
   RTSP-over-WebSocket tunnel on the API port (Route 2) and bind the
   relay to loopback. Server ≈ 150 lines; client ≈ a small fork of
   `rtsp-client-android` (socket injection) plus a WebSocket `Socket`
   adapter.
4. **Optional, cheap:** `media3-datasource-okhttp` for HTTP/2 on the
   client, and a small server-side cache of muxed CMAF segments so
   repeated playback windows are free.
5. **Keep the door open for the one case that would change the verdict:**
   if the goal becomes "the server must stop carrying live video at all"
   (bandwidth cost, not port count), then camera-side P2P (Route 5) is
   the only route that removes the server from the media path, and it is
   a thingino firmware project.

## What would reopen this

- The fleet's cameras gain a P2P/ICE mode (vendor firmware, or a
  thingino feature) → Route 5 becomes real and hole punching returns,
  this time between the client and the camera.
- The product needs sub-200 ms live **and** one public port →
  WebRTC with ICE-TCP mux on the HTTPS port (Route 4), accepting the
  AAR cost.
- The operator's network blocks TCP 443 but allows UDP → then a UDP
  media path becomes attractive, and ICE (not bespoke hole punching) is
  the way to build it.

## References

- [RFC 6888 — Common Requirements for Carrier-Grade NATs (CGNs)](https://www.rfc-editor.org/info/rfc6888/)
  (REQ-2 paired pooling, REQ-7 recommends endpoint-independent filtering)
- [RFC 4787 — NAT Behavioral Requirements](https://www.rfc-editor.org/info/rfc4787/)
  (mapping/filtering taxonomy: EIM, ADM, APDM)
- [RFC 8445 — ICE](https://www.rfc-editor.org/info/rfc8445/) /
  [RFC 8839 — WebRTC: Overview](https://www.rfc-editor.org/info/rfc8839/)
- [Empirical Measurement of Carrier-Grade NAT (2026)](https://pxm2.io/research/cgnat-ip-reputation/)
  (operator mapping behaviour: EIM vs EDM)
- [NAT traversal and hole punching: from UDP to QUIC with iroh (2025)](https://rikettsie.github.io/posts/nat-traversal-hole-punching-iroh/)
  (relay-first, direct-path-upgrade model)
- [MediaMTX — WebRTC-specific features](https://mediamtx.org/docs/features/webrtc-specific-features)
  (static UDP port → static TCP port → STUN + hole punching → TURN)
- [MediaMTX — publish and read streams](https://mediamtx.org/docs/features/publish-and-read)
  (RTSP over WebSocket on the HTTP port)
- [`doc/MEDIA.md`](../MEDIA.md) — the embedded engine, its endpoints and
  its reverse-proxy model
- [`doc/PLANS/WEBRTC.md`](WEBRTC.md), [`doc/PLANS/MOQ.md`](MOQ.md) —
  the two adjacent evaluations this one lines up with
