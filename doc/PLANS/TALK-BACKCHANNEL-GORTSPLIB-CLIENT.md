# Talk backchannel — swap hand-rolled client for `gortsplib.Client`

Status: **planned, not implemented.** Replace the hand-rolled RTSP client
in `go/internal/backchannel` with `github.com/bluenviron/gortsplib/v5`'s
`gortsplib.Client`, keeping the public API (`Dial`/`FeedPCM`/`FeedAU`/
`Session.Close`) stable so `handlers_talk.go` and `doc/TALK.md` do not
change.

This is the **debt-reduction** plan. For the alternative — closing the
same gaps by extending the hand-rolled code — see
[`TALK-BACKCHANNEL-LOCAL-GAPS.md`](TALK-BACKCHANNEL-LOCAL-GAPS.md).

## Why do this

`gortsplib` is **already** a dependency (`v5.6.1` in `go/go.mod`); the
embedded recorder uses `gortsplib.Client` to read camera RTP. The back
channel is the only place in the codebase that still hand-rolls an RTSP
client. The hand-rolled code is in:

- `rtsp.go` (414 lines) — RTSP/1.0 state machine, Basic + Digest auth,
  TCP interleaved framing, reader goroutine, `OPTIONS` keepalive.
- `sdp.go` (174 lines) — SDP parser, back-channel media selector,
  control-URL resolver, codec chooser.
- `rtp.go` (18 lines) — RTP packet builder.

That is **~600 lines** that the upstream library already provides and
maintains. The motivation for swapping is:

1. **Single library surface.** The recorder and the back channel both
   speak RTSP through the same client code path. If gortsplib v6
   changes the API, we have **one** place to migrate, not two.
2. **Upstream hardening against new firmwares.** When themactep (the
   thingino author) commits a fix for a quirky camera to gortsplib, we
   get it on the next `go get -u`. The example `proxy-backchannel-paired`
   in gortsplib's `examples/` directory is itself a working reference
   for our use case.
3. **No new dependency.** This is not "adopt a new library", it is
   "use the library we already pin in one more place".
4. **Same observable end state.** The camera sees a normal RTSP client.
   Our public API does not change. The browser-side WebSocket contract
   is untouched.

## What this plan does **not** cover

- Browser-side changes (AudioWorklet, Opus encoding). Those are in
  [`TALK-AUDIO-QUALITY.md`](TALK-AUDIO-QUALITY.md) and are independent
  of the server-side client.
- The "paired path" RTSP server architecture (the
  `proxy-backchannel-paired` example is **not** the target here — that
  would expose an `rtsp://eneverre/<cam>_talk` endpoint for
  non-browser publishers; it is a separate feature, deferred).
- TCP/UDP re-negotiation. gortsplib handles both natively, so we
  *inherit* UDP support for free. The default stays TCP interleaved
  (thingino/prudynt require it; gortsplib makes that a one-line
  `Protocol: &gortsplib.ProtocolTCP`).
- Codec choice. We keep G.711 + AAC. Opus is a separate question.

## Current public API (must stay)

From `doc/TALK.md` and the existing call sites in
`handlers_talk.go`:

```go
// internal/backchannel
type Session struct { ... }
func Dial(ctx context.Context, rawURL, forceCodec string) (*Session, error)
func (s *Session) FeedPCM(sampleRate int, pcm []byte) error
func (s *Session) FeedAU(au []byte) error
func (s *Session) Close() error
```

The shape of `Session` changes (internally it will wrap a
`gortsplib.Client` instead of a `*rtspClient`), but the signatures,
return types, and the single-goroutine-producer contract for
`FeedPCM`/`FeedAU` stay identical.

## Target internal shape

```go
type Session struct {
    client    *gortsplib.Client
    bcMedia   *description.Media        // back channel track
    inFormat  format.Format             // publisher format (G.711/AAC)
    outFormat format.Format             // matched camera format
    ptOut     byte                      // camera payload type for WritePacketRTP

    // G.711 path: resampler state (unchanged, lives in resample.go)
    // AAC path: AU queue (unchanged, lives in session.go)

    // Lifecycle
    ctx       context.Context
    cancel    context.CancelFunc
    done      chan struct{}
    closeOnce sync.Once
}
```

`Dial`:

1. Parse the URL.
2. Create a `gortsplib.Client` with
   `RequestBackChannels: true, Protocol: &gortsplib.ProtocolTCP,
   Transport: &gortsplib.TransportTCP, User: u.User`.
3. `client.Start()` → `client.Describe(u)` → split medias by
   `Media.IsBackChannel` (the typed equivalent of our
   `findBackchannelMedia`).
4. `client.SetupAll(desc.BaseURL, desc.Medias)` to SETUP every media
   (including the back channel).
5. Walk `backChannelMedia.Formats` to find a match against
   `forceCodec` (G.711/AAC), exactly as `findBackchannelMedia` does
   today. The selection logic is preserved; only the input type
   changes from `sdpMedia` to `format.Format`.
6. `client.Play(nil)` to start the session.
7. Return the wrapped `*Session`.

`FeedPCM` (G.711 path): unchanged — `lowPassForDecimation` →
`resampleLinear` → G.711 encode → build an `*rtp.Packet` →
`client.WritePacketRTP(bcMedia, pkt)`. The send loop goroutine that
paced 20 ms frames is replaced by the cadence handled inside
`Session.sendLoop` (gortsplib does not pace for us; we still own the
20 ms tick). **No change to the public pacing contract.**

`FeedAU` (AAC path): unchanged in shape — the AU is queued, then
`Session.sendAUFrame` (or its replacement) pulls the next AU, wraps
it in an `*rtp.Packet` with the right RTP timestamp increment
(parsed from the ASC's `samplingFrequencyIndex` per
[`TALK-BACKCHANNEL-LOCAL-GAPS.md`](TALK-BACKCHANNEL-LOCAL-GAPS.md)
change 2), and calls `client.WritePacketRTP(bcMedia, pkt)`.

`Session.Close`: `client.Close()` + cancel the send loop + drain
queues. The `closeOnce` pattern keeps Close idempotent (preserves the
contract `TestSessionCloseIdempotent` already enforces).

## What gets deleted

| File | Lines | Replaced by |
|---|---|---|
| `rtsp.go` | 414 | `gortsplib.Client` (RTSP state machine, auth, transport, reader) |
| `sdp.go` (parser + helpers) | ~110 | `description.Session` + `description.Media` types |
| `rtp.go` | 18 | `pion/rtp.Packet` (already a transitive dep via gortsplib) |
| **Total deleted** | **~540** | |

`sdp.go`'s `findBackchannelMedia`/`chooseCodec` are rewritten on top
of `format.Format` (~50 lines kept, but using the typed API).

## What gets rewritten

- `backchannel_test.go` (384 lines) — most of the unit tests for the
  parser/auth/SDP/control-URL helpers no longer apply. They are
  replaced by:
  - **Integration tests** against a real camera (manual smoke — no
    new CI dependency).
  - **Unit tests** for the parts that survive: `formatNames`,
    codec-selection against `format.Format` (small, ~30 lines).
  - **In-process RTSP server harness** (gortsplib ships examples of
    `gortsplib.Server` for tests) — covers `Dial` happy path, Digest
    qop=auth retry, 401 stale=true, source-lost reconnect. ~150
    lines.
- `session_test.go` (50 lines) — `TestSessionCloseIdempotent` is
  preserved; new tests for `FeedPCM`/`FeedAU` happy paths against the
  in-process harness.

## Risks and mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| `gortsplib.Client` keeps a single `Protocol` per client; we need to **receive** RTP from the camera (forward audio) AND **send** to the back channel. Today the back channel is send-only. | We never read forward audio from the back channel, so this is fine — but gortsplib's API may want us to declare a handler. | Use `client.OnPacketRTPAny` only if we ever want forward audio; today the handler is nil and the read loop drops packets. Verify with a test. |
| Auth flow in gortsplib differs subtly from our hand-rolled: gortsplib handles `WWW-Authenticate` on 401 transparently, **including Digest qop=auth**. | Low — gortsplib's auth is battle-tested. | One integration test against a real camera in the existing "manual smoke" path. |
| The send loop's 20 ms pacing is our responsibility, not gortsplib's. | High — gortsplib does not pace. | Keep the existing `Session.sendLoop` (G.711) and `sendAUFrame` (AAC) goroutines. They call `client.WritePacketRTP` instead of `writeInterleaved`. |
| Behavioural change in edge cases (TEARDOWN, source-lost, transport reset). | Medium | Reuse the existing `TestSessionCloseIdempotent` plus the new in-process harness. The original code has been hardened against source-lost in production; the new code should be exercised by the same integration smoke. |
| Re-creating the keepalive `OPTIONS` cadence we have today (gortsplib has its own; the timing may differ). | Low | Audit `gortsplib.Client` keepalive interval; document any change in `doc/TALK.md`. |

## Work phases

1. **Phase 0 — Spike (no merge).** Half a day. Add a `Dial` variant
   `dialWithGortsplib` next to the existing `Dial`; verify it can
   SETUP, PLAY, send a single G.711 frame, and Close against a real
   thingino camera. No public API change. If the spike works, the
   plan is greenlit; if not, we keep the hand-rolled client and
   pivot to [`TALK-BACKCHANNEL-LOCAL-GAPS.md`](TALK-BACKCHANNEL-LOCAL-GAPS.md).
2. **Phase 1 — `Dial` rewrite.** Replace `dialRTSP` and the SDP
   parser/selector with `gortsplib.Client` + `description.Media`.
   Keep `FeedPCM`/`FeedAU`/`Close` **unchanged** (still hand-rolled
   send loop + G.711/AAC encoding). Verify the spike's smoke
   scenario.
3. **Phase 2 — Send loop rewrite.** Replace
   `rtspClient.writeInterleaved` with `client.WritePacketRTP`. The
   pacing goroutine, the 400 ms buffer cap
   ([TALK-AUDIO-QUALITY §G.711 latency cap](TALK-AUDIO-QUALITY.md)),
   the AAC drop-oldest, and the AAC warm-up contract
   ([TALK-AUDIO-QUALITY §AAC first-word clip](TALK-AUDIO-QUALITY.md))
   are preserved.
4. **Phase 3 — Test rewrite.** Replace the SDP/auth/control-URL
   unit tests with the in-process RTSP harness tests
   (`gortsplib.Server` as a stand-in camera). Re-run the manual
   smoke against at least two real cameras (G.711-only, AAC-capable).
5. **Phase 4 — Delete the dead code.** Once phases 1-3 land and
   `backchannel_test.go` covers the new path, `rtsp.go`,
   the SDP parser parts of `sdp.go`, and `rtp.go` are removed.

## Total cost

| Phase | Effort | Risk |
|---|---|---|
| 0. Spike | 0.5 day | None (additive) |
| 1. `Dial` | 1-2 days | Low (public API stable) |
| 2. Send loop | 1-1.5 days | Medium (this is where most regressions would land) |
| 3. Tests | 1-1.5 days | Low (replaces known-good tests) |
| 4. Delete | 0.25 day | None |
| **Total** | **4-6.5 days** | Medium overall |

Versus the **local-gaps** plan (1-2 days, low risk), the gortsplib
plan is **3-5× the effort** for the same end-state functionality.
The return is **-540 lines of code we no longer own**.

## Recommendation

This plan is the right call when **any** of these is true:

- The hand-rolled RTSP code is generating maintenance pain (a new
  vendor SDP quirk, a recurring auth bug, a transport question we
  answer for the third time).
- The recorder side of the project is about to bump gortsplib (v6)
  and migrating both call sites in one go is cheaper than two.
- A feature wants RTSP-side capabilities we don't have today (e.g.
  serving the back channel as an RTSP endpoint to a third-party
  publisher — the "paired path" model from the gortsplib example).

It is the wrong call when the goal is **only** "ship robust talk on
today's cameras" — in that case the
[`TALK-BACKCHANNEL-LOCAL-GAPS.md`](TALK-BACKCHANNEL-LOCAL-GAPS.md)
plan is the right tool.

## End state on the wire

Both plans converge on the same observable behaviour:

- TCP interleaved RTSP client to the camera.
- SETUP all medias including the back channel.
- PLAY.
- G.711 or AAC frames sent at 20 ms / per-AU cadence with the camera
  payload type, fresh SSRC per session, continuous sequence numbers,
  timestamps derived from the camera clock rate.
- TEARDOWN on Close.
- Digest Basic+`qop=auth` (plan A) or whatever gortsplib negotiates
  (plan B) — both are RFC 7616 compliant.
- RTCP SR every 5 s (plan A) or whatever cadence gortsplib ships
  (plan B) — both within the RFC's tolerance.

Cameras cannot tell the two implementations apart.
