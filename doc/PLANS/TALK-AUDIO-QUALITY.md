# Talk audio-quality improvements — plan

Status: **partially done.** This document tracks the deferred improvements to
the push-to-talk (two-way audio) path. Several related changes are **already
implemented** and described under [Done](#done) for context.

See [`doc/TALK.md`](../TALK.md) for the client protocol and the overall talk
architecture. The backend lives in `internal/backchannel` (RTSP + RTP send
loop) and `internal/server/handlers_talk.go` (the WebSocket handler); the
browser client is `go/static/js/util/talk-client.js`.

## TL;DR

| # | Improvement | Where | Value | Effort | Status |
|---|-------------|-------|-------|--------|--------|
| — | G.711 send-loop latency cap (400 ms) | `session.go` | High (bounds worst-case latency) | 1 const | **done** |
| — | AAC first-word clip → client silence warm-up | client + `TALK.md` / `session.go` | High (no clip, no added latency) | done | **done** |
| — | AAC buffer drop-oldest | `session.go` | Medium (freshness under backlog) | done | **done** |
| 2 | Stateful resampling | `session.go` / `resample.go` | Low (subtle artifact) | ~15 lines | deferred |
| 3 | Parse AAC `a=fmtp` | `sdp.go` / `aac.go` | Medium (robustness on non-standard cameras) | ~40 lines | deferred |
| A | `AudioWorklet` capture | `talk-client.js` (+ new worklet file) | High (latency + robustness) | refactor | deferred |

- **#A (AudioWorklet)** is the biggest lever on talk latency and glitch-freeness,
  but it's a client-side capture refactor, not a one-liner. Do it as its own task.
- **#2 (stateful resampling)** is *correct in principle* — today's per-chunk
  resampling has textbook phase drift — but for 8 kHz telephony voice the audible
  benefit is marginal. Do it only if artifacts are heard in practice.
- **#3 (parse AAC fmtp)** is robustness for cameras that deviate from the de-facto
  AAC-hbr defaults. Low practical risk today; do it if such a camera turns up.

---

## Done

### G.711 send-loop latency cap

The G.711 send loop (`Session.sendLoop`, `session.go`) drains the incoming audio
buffer at a fixed 20 ms cadence and drops the oldest samples when the backlog
exceeds a cap. That cap was `TargetRate * 4` (**4 s**). Under a burst — a
backgrounded tab, or a network hiccup releasing a batch of WebSocket frames — the
loop could build seconds of backlog and then play it out at 1×, i.e. seconds of
**stale** audio. For half-duplex push-to-talk that is the worst failure mode.

The cap is now `MaxBufferSamples = TargetRate * 400 / 1000` (3200 samples ≈
**400 ms**): enough headroom for normal network/`ScriptProcessor` jitter (~85 ms
per capture block, see below), low enough that latency can't balloon. On
overflow the oldest audio is dropped, keeping the freshest speech.

### AAC first-word clip → client-side silence warm-up

A camera isn't ready to receive backchannel audio the instant it answers `PLAY`
(its decoder/speaker warm-up takes a fraction of a second), and that readiness is
**never signalled on the wire** — there is no "camera ready" event to wait on
(RTCP receiver reports on the backchannel are unreliable/absent on most cameras).
So the only robust strategy is to keep the channel warm with silence rather than
guess a delay. The G.711 send loop already does this (it streams silence from its
first tick); the AAC path is passthrough and stays silent until the first AU, so
its **client** must warm up.

**Resolution history:** an interim server-side ~700 ms AAC settle in `Dial` was
tried as a safety net, then removed once the client began streaming silence AUs
from `{"status":"ready"}` until the user speaks. `Dial` now returns as soon as the
RTSP handshake completes on **both** paths — no fixed delay anywhere — and the AAC
client warms the camera during the connect + user-reaction window: no clip, no
added latency. The client requirement is documented in
[`doc/TALK.md` → AAC warm-up](../TALK.md#aac-warm-up-required-stream-silence-until-the-user-speaks).

### AAC buffer drop-oldest

`FeedAU` used to drop the **incoming** AU when `auIn` was full, i.e. it kept stale
audio and rejected fresh — the opposite of the G.711 cap. It now sheds the
**oldest** queued AU and enqueues the new one, so a backlog drops stale audio and
keeps the freshest speech. `FeedAU` is the sole producer, so freeing one slot
guarantees room for the follow-up enqueue.

---

## #2 — Stateful resampling

### Problem

`Session.FeedPCM` (`session.go`) resamples **each WebSocket message
independently** via `lowPassForDecimation` + `resampleLinear` (`resample.go`).
Both treat every chunk as isolated:

- `lowPassForDecimation` resets its moving-average accumulator at each chunk
  boundary (a filter transient on the first `win` samples).
- `resampleLinear` recomputes `outLen = int(len*ratio)` per chunk and restarts
  the fractional read phase at 0. At 44.1 kHz → 8 kHz a 4096-sample block yields
  743 output samples but only "consumes" ~4096.5 input samples' worth, so ~0.5
  sample of misalignment per chunk is dropped and the phase resets.

The result is a faint periodic *warble* (not a loud click) at the capture-block
rate (~12/s). The lowpass transient is negligible — `win` ≈ 6 samples at
44.1 k → 8 k, i.e. < 0.15 ms — so the fix is really about the resampler phase.

### Fix

Carry resampler state across `FeedPCM` calls in the `Session` (or a small
`resampState` value):

- **fractional read position** (`phase float64`) so the linear interpolator
  continues instead of restarting at 0 each chunk;
- **last input sample** of the previous chunk, for interpolation across the
  boundary;
- **lowpass carry** — the previous `win-1` input samples — so the moving average
  spans the boundary instead of resetting.

`FeedPCM` is called from a single goroutine (the WebSocket read loop in
`handlers_talk.go`), so the state needs no lock; document that `FeedPCM` is not
safe to call concurrently with itself.

**Do not** reach for a polyphase/FIR resampler — it's overkill for band-limited
voice. A stateful linear resampler is enough.

### Verdict

Deferred. The code is technically wrong (phase drift), but the payoff for 8 kHz
voice is small. Revisit if users report artifacts.

---

## #3 — Parse the AAC `a=fmtp`

### Problem

`parseSDP` (`sdp.go`) extracts only `a=control`, direction and `a=rtpmap`; it
**ignores `a=fmtp`**. So two values are hardcoded and never verified against the
camera:

- **AU-header framing** — `aacRTPPayload` (`aac.go`) hardcodes `sizeLength=13,
  indexLength=3` (RFC 3640 AAC-hbr). If a camera advertises different lengths in
  its fmtp, the AU headers are mis-framed and the camera can't decode — with no
  error surfaced.
- **Frame length** — `AACFrameSamples = 1024` assumes AAC-LC. HE-AAC (SBR, 2048)
  or AAC-LD (512) would make the RTP timestamp increment wrong → drift.

These are the de-facto defaults for virtually every ONVIF Profile T backchannel,
and [`doc/TALK.md`](../TALK.md) already instructs the client to encode AAC-LC /
mono / the track's rate, so practical risk is low. But we're flying blind: a
non-standard camera fails silently and undiagnosably.

### Fix

Parse the MPEG4-GENERIC track's `a=fmtp` (the `config`, `sizeLength`,
`indexLength`, `mode=AAC-hbr` parameters) in `sdp.go` and thread them onto
`sdpMedia`. Then either:

- **adapt** `aacRTPPayload` to the advertised `sizeLength`/`indexLength` and
  derive the frame length from the AudioSpecificConfig, or
- at minimum, **log** the parsed config and **fail `Dial` with a clear error**
  when it isn't AAC-LC / 13 / 3, instead of emitting undecodable RTP.

### Verdict

Deferred. Robustness/observability for non-standard cameras; implement when one
actually turns up (the error path alone is a cheap first step).

---

## #A — `AudioWorklet` capture (client)

### Problem

`talk-client.js` captures the mic with
`ctx.createScriptProcessor(4096, 1, 1)` and does the float→S16LE conversion in
`onaudioprocess`. `ScriptProcessor` is **deprecated** by the Web Audio API, and
for talk it has two concrete costs:

1. **Runs on the main thread.** `onaudioprocess` fires on the same thread as
   layout, rendering, GC and app JS. When the wall re-renders tiles or a GC
   pauses, the callback lands late → capture glitches/dropouts. This is exactly
   the scenario talk hits: speaking while a camera grid is decoding video.
2. **High baseline latency.** A 4096-sample block at 48 kHz is **~85 ms** of
   capture latency before the network is even involved. `ScriptProcessor` also
   accumulates latency over time in some browsers.

### Fix

Migrate capture to `AudioWorklet`:

- **Dedicated high-priority audio thread**, isolated from the main thread → no
  glitches when the UI is busy.
- **128-sample processing blocks** (~2.7 ms at 48 k) → an order-of-magnitude
  lower baseline latency. Batch a few blocks before posting if desired, but the
  floor is far lower.
- Do the float→S16LE conversion inside the worklet's `process()` and post the
  already-`Int16` `ArrayBuffer` to the main thread via `MessagePort` — this also
  moves that loop off the main thread.

This reinforces the backend cap (#done): smaller capture blocks mean smaller
bursts to the server, so the 400 ms buffer stays naturally low instead of taking
85 ms hits at once.

### Cost / shape of the work

- The worklet must live in a **separate JS file** loaded with
  `audioContext.audioWorklet.addModule(url)` — it can't be inlined like the
  current callback. Add `go/static/js/util/talk-processor.js` and serve it.
- Communication moves from a direct buffer read to `port.postMessage`; the
  worklet accumulates 128-sample blocks and posts PCM, the main thread relays it
  over the WebSocket.
- Requires a secure origin (HTTPS/localhost) — already true for `getUserMedia`.
- Keep a `ScriptProcessor` fallback for old browsers, or gate on
  `window.AudioWorklet` support.

### Verdict

Deferred to its own task. It's a real latency/robustness win and the largest
lever on baseline talk latency, but it's a capture-path refactor, not a config
change — worth doing deliberately, not folded into unrelated fixes.
