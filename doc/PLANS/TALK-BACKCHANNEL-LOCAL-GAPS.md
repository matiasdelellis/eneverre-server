# Talk backchannel — fill the known gaps locally

Status: **implemented.** The push-to-talk backchannel in
`go/internal/backchannel` works against thingino/prudynt today (hand-rolled RTSP
client, SDP parser, RTP send loop). All four gaps below are closed: per-PT codec
table (a functional fix, see Change 4), AAC `a=fmtp` parsing, RTCP Sender
Reports, and Digest `qop=auth`. Each change was additive — no public API change.

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

## Change 1 — Send RTCP SR (Sender Reports) (**done**)

### Why

RFC 3550 §6.4.1. We are the **sender** of the backchannel audio, so we send
SR (not RR). Some cameras (notably newer prudynt) drop the session if they
see RTP without periodic SR. We send nothing today.

### Implemented

`rtcp.go` carries `buildRTCPReport` (fixed 28-byte SR: V=2/PT=200/RC=0,
sender SSRC, `ntpNow()` wall clock, last RTP timestamp, packet/octet
counters) and `ntpNow()`. `Dial` generates the session SSRC once and starts
`s.srLoop(ssrc)`: a ticker sends one SR every 5 s on interleaved channel 1
(RTP rides channel 0), exits via `s.srDone` on `Close`. The send loops
update `lastRTPTS`/`sentPackets`/`sentOctets` with atomics (lock-free
hand-off to the SR goroutine); octets count RTP payload bytes.

Deviations: none. Wireshark verification against a live camera is still
pending; unit tests pin the packet layout (`TestBuildRTCPReport`,
`TestNTPNow`).

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

## Change 2 — Parse the AAC `a=fmtp` from the SDP (**done**)

### Why

`parseSDP` (`sdp.go:24`) only reads `m=`, `a=control:`, direction, and
`a=rtpmap:`. It **ignores `a=fmtp`**. The result: `sizeLength=13` and
`indexLength=3` are hardcoded in `aacRTPPayload` (`aac.go`). If a camera
advertises different lengths in fmtp, AU headers are mis-framed and the
camera cannot decode — with **no error surfaced**.

Also: the plan currently assumes `AACFrameSamples = 1024` (AAC-LC). HE-AAC
(2048) or AAC-LD (512) would make the RTP timestamp increment wrong →
silent drift.

### Implemented

`a=fmtp:<pt>` params are stored **per payload type** on `sdpFormat.fmtp`
(the table from Change 4 already keys by PT). `parseAACParams` (`aac.go`)
parses RFC 3640 §4.1: `sizelength`/`indexlength` default to 13/3 (keys are
case-insensitive — the real thingino SDP sends lowercase `sizelength`),
`mode` must be `AAC-hbr` or absent, and `config=` is **required**. `Dial`
fails with a clear error when the chosen AAC PT has no fmtp/config, or when
the parsed params are invalid. Frame length comes from the
AudioSpecificConfig's `audioObjectType` (AAC-LD → 512, SBR/PS → 2048, else
1024); `aacRTPPayload(au, sizeLen, indexLen)` frames the AU header
generically and `sendLoopAAC` increments the RTP timestamp by
`aacParams.frameSamples`.

Deviations: non-`AAC-hbr` modes **fail closed** with an error instead of
the planned "log and proceed on an allowlist" (there is no mode variant we
can remux transparently, so the allowlist would be empty). Tests:
`TestParseAACParams` (thingino fmtp, HE-AAC, AAC-LD, custom sizes, and the
negative paths), `TestAACRTPPayloadCustomFraming`.

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

## Change 3 — Digest auth qop=auth + nonce counter (**done**)

### Why

Our `rtspClient.request` (`rtsp.go:76`) implements Basic auth and Digest
auth without `qop`. RFC 7616 — and a growing number of cameras (recent
thingino builds) — require `qop=auth` with a `nc=` counter and a
`cnonce=` on each request. Without it, the camera returns `401` with
`stale=true` on the second request and the session loops.

### Implemented

`rtspClient` grew `qop`/`nc`/`cnonce` plus an `authMu` (the keepalive
OPTIONS and the Close-time TEARDOWN build Authorization headers from
different goroutines). `authHeader` takes the `qop=auth` branch:
`response = MD5(HA1 : nonce : nc : cnonce : qop : HA2)` with `nc` as
8-hex-digits incrementing per request and a 16-byte crypto/rand `cnonce`.
`handleAuth` parses `qop=` from the `WWW-Authenticate` challenge and
distinguishes a fresh nonce (reset `nc=1`, regenerate `cnonce`) from a
stale retry under the same nonce (keep counting). The no-qop MD5 path is
unchanged.

Deviations: the test vector is RFC 2617 §3.5 (the MD5 `qop=auth` example) —
RFC 7616 §3.9.1 vectors are SHA-256, and this client speaks MD5. Tests:
`TestAuthHeaderDigestQop` (RFC vector), `TestAuthHeaderDigestLegacy`
(RFC 2069 vector), `TestHandleAuthQop` (stale vs fresh nonce semantics).

Also fixed while verifying against a live thingino camera: `options()` sent
no Authorization header, and that camera 401s OPTIONS without one — so `Dial`
and `ProbeCodecs` failed before DESCRIBE ever ran. `options()` now uses the
same auth-retry pattern as `describe()` (first attempt gets the challenge,
the retry carries the credentials).

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

## Change 4 — Per-payload-type codec table (**done**)

### Why

`findBackchannelMedia` (`sdp.go`) did string matching on a single
`m.codecName`, but `parseSDP` overwrote that field with **every** `a=rtpmap`
line, so only the *last* codec of a track survived. Verified against a real
thingino prudynt camera (`rtsp://thingino:thingino@192.168.1.91/ch0`,
DESCRIBE with the ONVIF backchannel Require): its backchannel `track0` is one
`a=sendonly` audio track advertising **four** payload types — AAC
mpeg4-generic/48k (PT 97), Opus (102), PCMU (0), PCMA (8). The old code
selected PCMA but `chooseCodec` returned `payloads[0]` = **97**, so A-law bytes
went out labelled as AAC — the camera can't decode that. `forceCodec="PCMU"`
and `"AAC"` also failed, because the track's codec table had collapsed to
PCMA. This was a functional bug, not a readability one.

### What was done

`sdpMedia` now carries `formats map[int]sdpFormat`, a per-payload-type table
built from every `a=rtpmap` line. `findBackchannelMedia` returns the track
*and* the codec hint (PCMA preferred over PCMU within a track, G.711 over
AAC across tracks), and `chooseCodec(m, want)` resolves the **matching** PT.
`ProbeCodecs`' labeling was extracted to `probeLabels`, which walks the whole
codec table — so this camera's capabilities now correctly report
`["aac", "g711"]` (Opus is skipped) instead of just `["g711"]`.

Tests: `TestThinginoMultiCodecBackchannel` pins the verbatim camera SDP
(auto → PCMA/8, forced PCMA/PCMU/AAC → 8/0/97), `TestChooseCodec` gained the
multi-codec cases, and `TestProbeLabels` covers the single-/multi-track
shapes. Static-PT inference and the legacy fallback (PCMA + first PT for
unknown tracks) are preserved.

### Verification

- `go test ./internal/backchannel` (all pass, including the real-camera fixture).
- Full RTSP handshake against `192.168.1.91` done manually (OPTIONS/DESCRIBE
  with Require → 200, SETUP `track0` interleaved=0-1 → 200, PLAY → 200,
  RTP accepted, TEARDOWN → 200).

---

## Total cost

| Change | Where | Tests | Behaviour change |
|---|---|---|---|
| 1. RTCP SR | done (`rtcp.go`, `session.go`) | done (`TestBuildRTCPReport`, `TestNTPNow`) | Cameras that drop on no-SR stop dropping |
| 2. AAC fmtp | done (`aac.go`, `sdp.go`, `session.go`) | done (`TestParseAACParams`, `TestAACRTPPayloadCustomFraming`) | Non-standard AAC fmtp surfaces a clear error instead of silent mis-frame |
| 3. Digest qop | done (`rtsp.go`) | done (`TestAuthHeaderDigestQop`, `TestAuthHeaderDigestLegacy`, `TestHandleAuthQop`) | Cameras enforcing qop=auth no longer 401-loop |
| 4. Per-PT codec table | done (`sdp.go`, `session.go`, tests) | done | **Fixed:** multi-codec backchannel tracks (thingino) select the right PT; probe reports `aac` + `g711` |

All four changes landed; strictly additive, no API change.

## Work phases

The changes are independent — order them by "most-impact-per-effort":

1. ~~**Change 2 (AAC fmtp)**~~ — **done**.
2. ~~**Change 1 (RTCP SR)**~~ — **done**.
3. ~~**Change 3 (Digest qop)**~~ — **done**.
4. ~~**Change 4 (typed selection)**~~ — **done**, and it turned out to be a
   functional fix for multi-codec thingino backchannel tracks (see above).

Remaining verification (not blocking): a Wireshark/`ENEVERRE_LOG_LEVEL=debug`
smoke against a live thingino camera to confirm SRs flow on the interleaved
channel and a `qop=auth`-enforcing camera completes the handshake.

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
