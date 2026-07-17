package backchannel

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand"
	"time"
)

// TargetRate is the G.711 sample rate (8 kHz).
// FrameSamples is one 20 ms RTP frame at 8 kHz (160 samples).
// MaxBufferSamples caps the send-loop backlog at ~400 ms of 8 kHz audio: enough
// headroom to ride out normal network/ScriptProcessor jitter, but low enough
// that a burst (a backgrounded tab, a network hiccup releasing a batch) can't
// build seconds of latency. On overflow the oldest audio is dropped, keeping
// push-to-talk responsive with the freshest speech.
const (
	TargetRate       = 8000
	FrameSamples     = 160
	MaxBufferSamples = TargetRate * 400 / 1000 // 3200 samples ≈ 400 ms
)

// Session is a live audio backchannel to one camera. Open one with Dial, push
// audio with FeedPCM (G.711) or FeedAU (AAC), and release it with Close.
// It is safe to call FeedPCM, FeedAU, and Close from different goroutines
// than the one that opened it.
type Session struct {
	*rtspClient
	codec         string
	pt            byte
	clockRate     int
	uri           string
	audioIn       chan []int16 // G.711 path: native-rate PCM to resample + encode
	auIn          chan []byte  // AAC path: raw access units to forward
	stop          chan struct{}
	done          chan struct{}
	keepaliveDone chan struct{}
}

// sleepCtx blocks for d or until ctx is cancelled, returning ctx.Err() if the
// context fires first. Used for the inter-step pauses in Dial so a cancelled
// context aborts the handshake instead of always waiting out the delay.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// keepalive sends OPTIONS requests every 25 seconds to keep the RTSP session
// alive. Errors are logged at debug level only — a lost keepalive is not fatal
// and the camera will likely close the session on its own if the RTP flow stops.
func keepalive(c *rtspClient, uri string, done chan struct{}) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			slog.Debug("rtsp keepalive OPTIONS")
			if err := c.writeRequest("OPTIONS", uri); err != nil {
				slog.Debug("keepalive failed", "err", err)
			}
		case <-done:
			return
		}
	}
}

// Codec returns the negotiated backchannel codec: "PCMA", "PCMU", or "AAC".
func (s *Session) Codec() string { return s.codec }

// Dial opens the RTSP backchannel to rawURL (rtsp://user:pass@host:port/path)
// and starts the RTP send loop. forceCodec may be "PCMA"/"PCMU" to pin a G.711
// track, "AAC" to pin an MPEG4-GENERIC track (raw AUs are fed via FeedAU), or ""
// to auto-select G.711 from the SDP. In the G.711 path the returned Session
// sends silence until the caller feeds audio; in the AAC path it stays quiet
// until the first AU arrives.
func Dial(ctx context.Context, rawURL, forceCodec string) (*Session, error) {
	c, err := dialRTSP(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	// Honor ctx across the whole handshake, not just the TCP dial: each RTSP
	// step below runs with its own 10s socket deadline, so with auth retries a
	// dead camera could hold the caller for several times the intended budget.
	// Closing the conn unblocks whichever step is in flight; after Dial
	// returns (handshakeDone) the session's lifetime is Close()'s business,
	// not ctx's.
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			c.conn.Close()
		case <-handshakeDone:
		}
	}()

	s := &Session{
		rtspClient: c,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}

	uri := c.baseURL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	s.uri = uri

	slog.Debug("backchannel connected", "host", c.baseURL.Host)

	if err := c.options(uri); err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("OPTIONS: %w", err)
	}

	sdpRaw, err := c.describe(uri)
	if err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("DESCRIBE: %w", err)
	}

	medias := parseSDP(sdpRaw)
	for _, m := range medias {
		slog.Debug("sdp media", "type", m.mediaType, "dir", m.direction,
			"control", m.control, "codec", m.codecName, "clock", m.clockRate, "pt", m.payloads)
	}

	bcMedia, err := findBackchannelMedia(medias, forceCodec)
	if err != nil {
		c.conn.Close()
		return nil, err
	}

	codec, pt := chooseCodec(bcMedia)
	s.codec = codec
	s.pt = pt
	s.clockRate = bcMedia.clockRate

	controlURL := resolveControlURL(c.baseURL, bcMedia.control)
	transport := "RTP/AVP/TCP;unicast;interleaved=0-1"
	if err := c.setup(controlURL, transport); err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("SETUP: %w", err)
	}
	slog.Debug("backchannel SETUP ok", "session", c.sessionID)

	// Brief pause after SETUP: some cameras need the transport state to settle
	// before PLAY takes effect.
	if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
		c.conn.Close()
		return nil, err
	}

	if err := c.play(uri); err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("PLAY: %w", err)
	}

	// No post-PLAY settle delay on either path: a camera isn't ready to receive
	// backchannel audio the instant it answers PLAY, but that readiness is never
	// signalled on the wire, so the only robust strategy is to keep the channel
	// warm with silence rather than wait a fixed guess. The G.711 send loop does
	// this itself (it streams silence from its first tick); the AAC path is
	// passthrough, so its client is expected to stream silence AUs until the user
	// speaks (see doc/TALK.md → AAC warm-up). Either way Dial returns as soon as
	// the RTSP handshake completes, so the client flips to "talking" that sooner.
	c.startReader()
	s.keepaliveDone = make(chan struct{})
	go keepalive(c, uri, s.keepaliveDone)

	if codec == "AAC" {
		s.auIn = make(chan []byte, 64)
		go s.sendLoopAAC()
	} else {
		s.audioIn = make(chan []int16, 64)
		go s.sendLoop()
	}

	slog.Debug("backchannel live", "codec", codec, "pt", pt, "clock", s.clockRate)

	return s, nil
}

// ProbeCodecs opens a short-lived RTSP session (OPTIONS + DESCRIBE with the
// ONVIF backchannel Require header) and returns the client-facing talk codec
// labels for every send-capable audio track the camera advertises: "aac" for an
// MPEG4-GENERIC track, "g711" for PCMA/PCMU. Labels are deduplicated and ordered
// as they appear in the SDP. No RTP is set up; the connection is closed before
// returning. Used at startup to populate camera capabilities so clients need not
// guess which codecs a camera accepts.
func ProbeCodecs(rawURL string) ([]string, error) {
	c, err := dialRTSP(context.Background(), rawURL)
	if err != nil {
		return nil, err
	}
	defer c.conn.Close()

	uri := c.baseURL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	if err := c.options(uri); err != nil {
		return nil, fmt.Errorf("OPTIONS: %w", err)
	}
	sdpRaw, err := c.describe(uri)
	if err != nil {
		return nil, fmt.Errorf("DESCRIBE: %w", err)
	}

	var codecs []string
	seen := map[string]bool{}
	for _, m := range parseSDP(sdpRaw) {
		if m.mediaType != "audio" || (m.direction != "sendonly" && m.direction != "sendrecv") {
			continue
		}
		var label string
		switch m.codecName {
		case "MPEG4-GENERIC", "AAC":
			label = "aac"
		case "PCMA", "PCMU":
			label = "g711"
		default:
			continue
		}
		if !seen[label] {
			seen[label] = true
			codecs = append(codecs, label)
		}
	}
	return codecs, nil
}

func (s *Session) sendLoop() {
	defer close(s.done)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var buf []int16
	maxBuf := MaxBufferSamples

	ssrc := rand.Uint32()
	seq := uint16(rand.Intn(65536))
	ts := uint32(rand.Intn(65536))

	for {
		select {
		case <-s.stop:
			return
		case samples := <-s.audioIn:
			buf = append(buf, samples...)
			if len(buf) > maxBuf {
				buf = buf[len(buf)-maxBuf:]
			}
		case <-ticker.C:
			var frame []int16
			if len(buf) >= FrameSamples {
				frame = buf[:FrameSamples]
				buf = buf[FrameSamples:]
			} else {
				frame = make([]int16, FrameSamples)
				copy(frame, buf)
				buf = buf[:0]
			}

			var payload []byte
			if s.codec == "PCMU" {
				payload = encodeULaw(frame)
			} else {
				payload = encodeALaw(frame)
			}

			packet := buildRTPPacket(s.pt, seq, ts, ssrc, seq == 0, payload)
			if err := s.writeInterleaved(0, packet); err != nil {
				slog.Warn("backchannel RTP send failed", "err", err)
				return
			}
			seq++
			ts += FrameSamples
		}
	}
}

// FeedPCM decodes native-rate mono S16LE PCM, resamples it to 8 kHz when needed,
// and queues it for transmission. Oversized bursts are dropped rather than
// blocking the caller (the RTP loop paces at a fixed 20 ms).
func (s *Session) FeedPCM(pcm []byte, nativeRate int) {
	if len(pcm) < 2 {
		return
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	if nativeRate > TargetRate {
		samples = lowPassForDecimation(samples, nativeRate, TargetRate)
		samples = resampleLinear(samples, nativeRate, TargetRate)
	}
	select {
	case s.audioIn <- samples:
	default:
		slog.Debug("backchannel buffer full, dropping samples", "n", len(samples))
	}
}

func (s *Session) sendLoopAAC() {
	defer close(s.done)

	ssrc := rand.Uint32()
	seq := uint16(rand.Intn(65536))
	ts := uint32(rand.Intn(65536))

	for {
		select {
		case <-s.stop:
			return
		case au := <-s.auIn:
			if isADTS(au) {
				au = au[adtsHeaderLen(au):]
			}
			if len(au) == 0 {
				continue
			}
			packet := buildRTPPacket(s.pt, seq, ts, ssrc, true, aacRTPPayload(au))
			if err := s.writeInterleaved(0, packet); err != nil {
				slog.Warn("backchannel AAC send failed", "err", err)
				return
			}
			seq++
			ts += AACFrameSamples
		}
	}
}

// FeedAU queues one raw AAC-LC access unit (optionally ADTS-wrapped) for
// transmission on the AAC backchannel. It is a no-op on a G.711 session. The AU
// must match the track's advertised config (AAC-LC, the SDP clock rate, mono);
// oversized bursts are dropped rather than blocking the caller.
func (s *Session) FeedAU(au []byte) {
	if s.auIn == nil || len(au) == 0 {
		return
	}
	b := make([]byte, len(au))
	copy(b, au)
	select {
	case s.auIn <- b:
	default:
		// Buffer full: shed the OLDEST queued AU and enqueue this one, so a
		// backlog drops stale audio and keeps the freshest speech (mirroring the
		// G.711 send loop's latency cap) rather than rejecting new audio and
		// letting latency grow. FeedAU is the only producer, so once we free a
		// slot the follow-up send has room.
		select {
		case <-s.auIn:
			slog.Debug("backchannel AAC buffer full, dropping oldest AU")
		default:
		}
		select {
		case s.auIn <- b:
		default:
		}
	}
}

// Close stops the send loop, tears down the RTSP session and closes the TCP
// connection. It is idempotent-safe to call once per Session.
func (s *Session) Close() {
	close(s.stop)
	<-s.done
	close(s.keepaliveDone)
	_ = s.writeRequest("TEARDOWN", s.uri)
	time.Sleep(200 * time.Millisecond)
	s.stopReader()
	s.conn.Close()
}
