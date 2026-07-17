# Talk backchannel — fill the known gaps locally

Status: **planned, not implemented.** The push-to-talk backchannel in
`go/internal/backchannel` works against thingino/prudynt today (hand-rolled RTSP
client, SDP parser, RTP send loop). Several gaps are known and **additive** to
fix without touching the public API or the send loop.

This is the **conservative** plan: extend the existing hand-rolled code where
the gaps are. For the alternative — replace the hand-rolled client with
`gortsplib.Client` — see
[`TALK-BACKCHANNEL-GORTSPLIB-CLIENT.md`](TALK-BACKCHANNEL-GORTSPLIB-CLIENT.md).

See [`doc/TALK.md`](../TALK.md) for the client protocol and
[`doc/PLANS/TALK-AUDIO-QUALITY.md`](TALK-AUDIO-QUALITY.md) for the
audio-quality roadmap (stateful resampling, AudioWorklet, etc., which are
**not** covered here — they live in their own track).

## What gaps this plan covers

| Gap | Where it bites | Severity today |
|---|---|---|
| No RTCP sender reports | Cameras that enforce RTCP liveness will eventually drop the session | Low (thingino/prudynt tolerate today) |
| `a=fmtp` ignored for AAC | `sizeLength=13`, `indexLength=3` hardcoded; HE-AAC / AAC-LD mis-framed silently | Low (de-facto AAC-LC everywhere) |
| Digest auth limited to MD5 / no qop | Some cameras require `qop=auth` with nonce counter | Low (most thingino do not) |
| `Media.IsBackChannel`-style typed SDP | Hand-rolled `findBackchannelMedia` works but is strings-on-strings | Cosmetic |

## What this plan does **not** cover

- TCP/UDP transport re-negotiation. We are already on TCP interleaved and it
  works against every camera we ship for. Adding UDP would be hundreds of
  lines (separate RTP/RTCP sockets, NAT, port allocation) for no observed
  benefit. Skip.
- Removing the hand-rolled code. That is the alternative plan.
- Browser-side changes (AudioWorklet, Opus encoding). Those are in
  `TALK-AUDIO-QUALITY.md`.
- Replacing G.711 with Opus on the back channel. Codec choice is a separate
  question; the transport improvements here are codec-agnostic.

---

## Change 1 — Send RTCP SR (Sender Reports)

### Why

RFC 3550 §6.4.1. We are the **sender** of the backchannel audio, so we send
SR (not RR). Some cameras (notably newer prudynt) drop the session if they
see RTP without periodic SR. We send nothing today.

### Design

- Build a minimal SR packet (RFC 3550 §6.4.1, layout fixed):
  - `V=2, P=0, F=0, RC=0, PT=200` (1 word header)
  - SSRC of the back channel sender (reuse the one in `buildRTPPacket`,
    `session.go:137`)
  - NTP timestamp (wall clock, 64-bit, NTP epoch)
  - RTP timestamp (mid-32 bits of the NTP timestamp × `clockRate / 1e6` or
    just the last sent RTP timestamp)
  - sender packet / octet counters (monotonic, reset on new session)
  - one report block of length 0 (we are not receiving anything on this
    session)
- Send every **5 seconds** from a dedicated goroutine started by
  `Session.start()`, stopped by `Session.Close()`. On `Close`, the goroutine
  exits via the existing `readerStop` pattern.
- Transport: same TCP interleaved channel as RTP, with the
  RTCP interleaved payload type marker (`$` + channel 1 if RTP is channel 0,
  or vice versa; the convention in our handshake is `interleaved=0-1` per
  `session.go:149`).

### Cost

- New file `go/internal/backchannel/rtcp.go` (~60 lines) with the packet
  builder + a small counter.
- New `rtcp_test.go` (~30 lines): fixed-input → fixed-binary-output
  snapshot of an SR packet.
- 4-5 lines in `session.go` to start/stop the SR goroutine alongside the
  existing send loop.

### Verification

- Wireshark trace against a real camera: SR appears every ~5s on the
  interleaved channel.
- `ENEVERRE_LOG_LEVEL=debug` shows "rtcp sr sent" lines.

---

## Change 2 — Parse the AAC `a=fmtp` from the SDP

### Why

`parseSDP` (`sdp.go:24`) only reads `m=`, `a=control:`, direction, and
`a=rtpmap:`. It **ignores `a=fmtp`**. The result: `sizeLength=13` and
`indexLength=3` are hardcoded in `aacRTPPayload` (`aac.go`). If a camera
advertises different lengths in fmtp, AU headers are mis-framed and the
camera cannot decode — with **no error surfaced**.

Also: the plan currently assumes `AACFrameSamples = 1024` (AAC-LC). HE-AAC
(2048) or AAC-LD (512) would make the RTP timestamp increment wrong →
silent drift.

### Design

Extend `sdpMedia` (`sdp.go:11`) with fmtp-derived fields:

```go
type sdpMedia struct {
    // ... existing fields ...
    fmtpConfig   string // raw `config=...` value
    fmtpSizeLen  int    // sizeLength; default 13 if absent
    fmtpIndexLen int    // indexLength; default 3 if absent
    fmtpMode     string // AAC-hbr / AAC-hbr-adts / empty
}
```

Parsing rules (RFC 3640 §4.1 + ONVIF Profile T backchannel convention):

- `a=fmtp:<pt> config=<hex AudioSpecificConfig>; ...
  sizeLength=<N>; indexLength=<N>; mode=AAC-hbr`
- `config` is **required** for AAC-hbr. We already read it indirectly via
  `aac.go`'s hardcoded defaults; now we read it from the SDP and **fail
  `Dial` with a clear error** if it is absent.
- `sizeLength` and `indexLength` default to 13/3 if absent.
- `mode` must be `AAC-hbr` or empty. If it is something else (`AAC-hbr-adts`,
  etc.), log a warning at `INFO` and proceed only if a small allowlist matches
  the variants we can transport transparently. **Fail closed** on anything
  we cannot guarantee to remux.

After `parseSDP` returns, in `chooseCodec` (`sdp.go:129`), thread the parsed
fields onto the returned `codec` and `pt`. Then in `aac.go`:
- `aacRTPPayload` reads `sizeLength` / `indexLength` from the call site
  (passed in alongside the AU), not from a package-level const.
- `Session.sendAUFrame` (or equivalent) computes the RTP timestamp
  increment from `AACFrameSamples` carried in the session (default 1024,
  parsed from the ASC's `samplingFrequencyIndex` if it is HE-AAC).

### Cost

- ~40 lines added to `sdp.go` (parser + fields) and ~10 to `aac.go`
  (parameterise the consts).
- ~25 lines in `backchannel_test.go`: SDP fixtures covering
  `config=...sizeLength=13;indexLength=3;mode=AAC-hbr`, ASC with HE-AAC,
  and a negative test (no `config=` → `Dial` returns an error).

### Verification

- A non-standard camera that previously failed silently now fails
  `Dial` with `"aac fmtp: config= missing"`.
- The test fixture exercises the negative path.

---

## Change 3 — Digest auth qop=auth + nonce counter

### Why

Our `rtspClient.request` (`rtsp.go:76`) implements Basic auth and Digest
auth without `qop`. RFC 7616 — and a growing number of cameras (recent
thingino builds) — require `qop=auth` with a `nc=` counter and a
`cnonce=` on each request. Without it, the camera returns `401` with
`stale=true` on the second request and the session loops.

### Design

Extend the auth fields in `rtspClient` (`rtsp.go:22`):

```go
type rtspClient struct {
    // ... existing fields ...
    qop        string // "" / "auth" — read from WWW-Authenticate on 401
    nc         int    // request counter, starts at 1
    cnonce     string // random per session, generated on first auth challenge
    nonceCount func() string // returns next nc as 8-hex-digit string
}
```

On `401`:

1. Parse `WWW-Authenticate: Digest realm="...", nonce="...", qop="auth"`.
2. If `qop="auth"`:
   - Generate a 16-hex-digit `cnonce` once per session.
   - `nc` starts at 1, incremented before each request that uses the
     same nonce.
   - Compute `response = MD5(MD5(user:realm:pass) ":" nonce ":" nc ":"
     cnonce ":" MD5(method:uri))`.
3. If `qop=""` (existing path): keep computing
   `MD5(MD5(user:realm:pass) ":" nonce ":" MD5(method:uri))` — unchanged.
4. On `401 stale=true` with the **same** nonce: just increment `nc` and
   retry, do not regenerate cnonce.
5. On `401` with a **new** nonce: reset `nc = 1`, regenerate `cnonce`,
   retry.

Retry once. If the second 401, return the error to the caller — `Dial`
fails with the underlying auth error.

### Cost

- ~80 lines in `rtsp.go` (parse the qop variant of WWW-Authenticate, add
  the cnonce/nc fields, branch the response calculation, handle
  stale=true).
- ~40 lines in `backchannel_test.go`: digest-response vectors from
  RFC 7616 §3.9.1 (qop=auth), and a retry-after-stale test.

### Verification

- Test fixtures carry the exact digest vectors from the RFC.
- Against a camera that enforces qop=auth, the talk handshake completes
  and stays up.

---

## Change 4 — Typed codec selection (helper, not a new type system)

### Why

`findBackchannelMedia` (`sdp.go:81`) does string matching on
`m.codecName`. It works, but it duplicates what a typed
`description.Media` (gortsplib's type) would do. We do **not** want to
adopt gortsplib here — that is the alternative plan — but a small
internal typed value can make the selection logic easier to read and
extend.

### Design

Replace the `sdpMedia` field-by-field string compares with a tiny
`format` helper:

```go
type bcFormat struct {
    codec    string // "PCMA" / "PCMU" / "AAC" / "OPUS"
    sampleHz int    // 8000 for G.711, parsed for AAC/Opus
    mulaw    bool   // G.711 only
}

func (m sdpMedia) backchannelFormat() (bcFormat, bool) { ... }
```

The selection logic (`sdp.go:108-122`) becomes a loop over
`backchannelFormat()` results with the existing preference order
(G.711 → AAC → any audio) preserved.

This is **purely a refactor** of `sdp.go` — no behavior change, no
additional tests needed beyond what `TestFindBackchannelMedia*` already
covers.

### Cost

- ~30 lines in `sdp.go` (one helper + a small rewrite of
  `findBackchannelMedia`).
- No new tests (existing `TestFindBackchannelMedia` and
  `TestFindBackchannelMediaAAC` exercise all paths).

### Verification

- The two existing `TestFindBackchannelMedia*` tests pass without
  modification.
- Manual smoke against thingino cameras (PR validation).

---

## Total cost

| Change | Lines | Tests | Behaviour change |
|---|---|---|---|
| 1. RTCP SR | +60 in `rtcp.go`, +4 in `session.go` | +30 | Cameras that drop on no-SR stop dropping |
| 2. AAC fmtp | +40 in `sdp.go`, +10 in `aac.go` | +25 | Non-standard AAC fmtp surfaces a clear error instead of silent mis-frame |
| 3. Digest qop | +80 in `rtsp.go` | +40 | Cameras enforcing qop=auth no longer 401-loop |
| 4. Typed selection | +30 in `sdp.go` | 0 (reuses existing) | None — internal refactor only |
| **Total** | **+224** | **+95** | Strictly additive; no API change |

Estimated wall time: **1-2 days** including tests and a manual smoke
against at least one thingino camera per change.

## Work phases

The changes are independent — order them by "most-impact-per-effort":

1. **Change 2 (AAC fmtp)** — small, well-scoped, surfaces silent
   misconfigurations. The error path is a cheap first step even if we
   defer the rest of the change.
2. **Change 1 (RTCP SR)** — additive, low-risk, protects against a
   future camera regression.
3. **Change 3 (Digest qop)** — only if/when a camera in our support
   matrix starts requiring qop=auth. Otherwise it sits as code that's
   not exercised in production.
4. **Change 4 (typed selection)** — last; it's a refactor, no
   behaviour change. Do it when the file is already open for one of
   the other changes.

## Why not the alternative

If the goal is also to **remove the hand-rolled code** (not just close
gaps), see
[`TALK-BACKCHANNEL-GORTSPLIB-CLIENT.md`](TALK-BACKCHANNEL-GORTSPLIB-CLIENT.md).
That plan is a **swap**, not an extension: same observable behaviour, but
~600 lines of hand-rolled RTSP/SDP/RTP code is deleted in exchange for
~100-150 lines of `gortsplib.Client` integration.

The local plan is the right choice when:

- The motivation is "ship robust talk on the cameras we already
  support", and
- The hand-rolled code is not causing maintenance pain yet.

The gortsplib plan is the right choice when the motivation is
**debt-reduction** or "absorb upstream gortsplib fixes for free".

Both plans target the same end state on the wire.
