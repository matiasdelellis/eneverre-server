package backchannel

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpsimpleaudio"
	"github.com/pion/rtp"
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

	// OpusFrameSamples is the RTP timestamp increment per forwarded Opus
	// packet: one 20 ms frame at the 48 kHz clock (RFC 7587). Clients must
	// encode 20 ms Opus frames for the passthrough path.
	OpusFrameSamples = 960

	// rtspTimeout bounds each RTSP read and write. The handshake as a whole is
	// bounded by the caller's context (see Dial).
	rtspTimeout = 10 * time.Second
)

// Session is a live audio backchannel to one camera. Open one with Dial, push
// audio with FeedPCM (G.711), FeedAU (AAC) or FeedOpus (Opus), and release it
// with Close. It is safe to call FeedPCM, FeedAU, FeedOpus, and Close from
// different goroutines than the one that opened it.
type Session struct {
	client *gortsplib.Client
	media  *description.Media
	codec  string

	// Exactly one encoder is set, matching codec. They turn a frame of audio
	// into RTP packets with the negotiated payload type, and own the sequence
	// numbers and the SSRC.
	g711Enc *rtplpcm.Encoder
	aacEnc  *rtpmpeg4audio.Encoder
	opusEnc *rtpsimpleaudio.Encoder

	// aacTSStep is the RTP timestamp increment per AAC access unit.
	aacTSStep uint32

	audioIn chan []int16 // G.711 path: native-rate PCM to resample + encode
	auIn    chan []byte  // AAC path: raw access units to forward
	opusIn  chan []byte  // Opus path: raw 20 ms packets to forward

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	// Running RTP timestamp and marker state. The encoders emit timestamps
	// relative to zero on every call, so the running value lives here and is
	// added on the way out. Both are touched only by the send loop.
	ts     uint32
	marked bool
}

// Codec returns the negotiated backchannel codec: "PCMA", "PCMU", "AAC" or
// "OPUS".
func (s *Session) Codec() string { return s.codec }

// Done is closed when the send loop has exited — on Close, or on its own when
// an RTP write fails (the camera dropped the RTSP connection). After that the
// Feed* methods silently discard audio, so the owner should watch Done and end
// the session instead of leaving a client talking into nothing.
func (s *Session) Done() <-chan struct{} { return s.done }

// Dial opens the RTSP backchannel to rawURL (rtsp://user:pass@host:port/path)
// and starts the RTP send loop. forceCodec may be "PCMA"/"PCMU" to pin a G.711
// track, "AAC" to pin an MPEG4-GENERIC track (raw AUs are fed via FeedAU),
// "OPUS" to pin an Opus track (raw packets via FeedOpus), or "" to
// auto-select G.711 from the SDP. In the G.711 path the returned Session
// sends silence until the caller feeds audio; in the AAC/Opus paths it stays
// quiet until the first AU/packet arrives.
func Dial(ctx context.Context, rawURL, forceCodec string) (*Session, error) {
	c, rawSDP, err := connect(rawURL)
	if err != nil {
		return nil, err
	}

	// Honor ctx across the whole handshake, not just the TCP dial: each RTSP
	// step below runs with its own socket deadline, so with auth retries a dead
	// camera could hold the caller for several times the intended budget.
	// Closing the client unblocks whichever step is in flight; after Dial
	// returns (handshakeDone) the session's lifetime is Close()'s business,
	// not ctx's.
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-handshakeDone:
		}
	}()

	u, err := base.ParseURL(rawURL)
	if err != nil {
		c.Close()
		return nil, err
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("DESCRIBE: %w", err)
	}
	for _, m := range desc.Medias {
		slog.Debug("sdp media", "type", m.Type, "control", m.Control,
			"backchannel", m.IsBackChannel, "formats", formatSummary(m))
	}

	pick, err := selectBackchannel(desc, *rawSDP, forceCodec)
	if err != nil {
		c.Close()
		return nil, err
	}
	pick.prepareMedia()

	s := &Session{
		client: c,
		media:  pick.media,
		codec:  pick.codec,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if err = s.buildEncoder(pick.forma); err != nil {
		c.Close()
		return nil, err
	}

	// SETUP the back channel alone: setting up every media would pull a second
	// copy of the camera's video down on each push-to-talk, on top of the
	// session the recorder already holds open.
	_, err = c.Setup(desc.BaseURL, pick.media, 0, 0)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("SETUP: %w", err)
	}

	// Brief pause after SETUP: some cameras need the transport state to settle
	// before PLAY takes effect.
	if err = sleepCtx(ctx, 100*time.Millisecond); err != nil {
		c.Close()
		return nil, err
	}

	if _, err = c.Play(nil); err != nil {
		c.Close()
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
	//
	// RTSP keepalives and the RFC 3550 §6.4.1 sender reports that some cameras
	// (newer prudynt builds among them) require are gortsplib's job now.
	switch s.codec {
	case codecAAC:
		s.auIn = make(chan []byte, 64)
		go s.sendLoopAAC()
	case codecOPUS:
		s.opusIn = make(chan []byte, 64)
		go s.sendLoopOpus()
	default:
		s.audioIn = make(chan []int16, 64)
		go s.sendLoopG711()
	}

	slog.Debug("backchannel live", "codec", s.codec,
		"pt", pick.forma.PayloadType(), "clock", pick.forma.ClockRate())

	return s, nil
}

// ProbeCodecs opens a short-lived RTSP session (DESCRIBE with the ONVIF
// backchannel Require header) and returns the client-facing talk codec labels
// for every send-capable audio track the camera advertises: "aac" for an
// MPEG4-GENERIC track, "g711" for PCMA/PCMU, "opus" for Opus. Labels are
// deduplicated and ordered as they appear in the SDP. No RTP is set up; the
// connection is closed before returning. Used at startup to populate camera
// capabilities so clients need not guess which codecs a camera accepts, and by
// the camera wizard's probe step. ctx bounds the whole handshake, not just the
// TCP dial.
func ProbeCodecs(ctx context.Context, rawURL string) ([]string, error) {
	c, rawSDP, err := connect(rawURL)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	probeDone := make(chan struct{})
	defer close(probeDone)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-probeDone:
		}
	}()

	u, err := base.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		return nil, fmt.Errorf("DESCRIBE: %w", err)
	}

	return probeLabels(desc, *rawSDP), nil
}

// connect starts an RTSP client aimed at rawURL, asking for back channels and
// capturing the raw SDP of the DESCRIBE response (selectBackchannel re-reads
// the media directions from it). The returned string pointer is filled in by
// the time Describe returns.
func connect(rawURL string) (*gortsplib.Client, *string, error) {
	u, err := base.ParseURL(rawURL)
	if err != nil {
		return nil, nil, err
	}

	// TCP only: the backchannel rides the RTSP connection itself, which keeps
	// it working through NAT and matches what every camera we have tested
	// expects.
	protocol := gortsplib.ProtocolTCP
	rawSDP := new(string)

	c := &gortsplib.Client{
		Scheme:              u.Scheme,
		Host:                u.Host,
		Protocol:            &protocol,
		RequestBackChannels: true,
		ReadTimeout:         rtspTimeout,
		WriteTimeout:        rtspTimeout,
		UserAgent:           "eneverre",
		OnRequest: func(req *base.Request) {
			slog.Debug("rtsp >", "method", req.Method, "url", req.URL)
		},
		OnResponse: func(res *base.Response) {
			slog.Debug("rtsp <", "status", res.StatusCode, "message", res.StatusMessage)
			if len(res.Body) > 0 && strings.Contains(strings.Join(res.Header["Content-Type"], ","), "sdp") {
				*rawSDP = string(res.Body)
			}
		},
	}

	if err = c.Start(); err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	slog.Debug("backchannel connected", "host", u.Host)
	return c, rawSDP, nil
}

// buildEncoder creates the RTP encoder for the negotiated format. The encoders
// own sequence numbering and the SSRC; their timestamps are relative to zero,
// so the session adds its running value in writePackets.
func (s *Session) buildEncoder(forma format.Format) error {
	switch f := forma.(type) {
	case *format.G711:
		enc, err := f.CreateEncoder()
		if err != nil {
			return fmt.Errorf("G711 encoder: %w", err)
		}
		s.g711Enc = enc
	case *format.MPEG4Audio:
		enc, err := f.CreateEncoder()
		if err != nil {
			return fmt.Errorf("AAC encoder: %w", err)
		}
		s.aacEnc = enc
		s.aacTSStep = aacFrameSamples(f)
		slog.Debug("backchannel aac framing", "sizeLen", f.SizeLength,
			"indexLen", f.IndexLength, "frameSamples", s.aacTSStep)
	case *format.Opus:
		enc, err := f.CreateEncoder()
		if err != nil {
			return fmt.Errorf("Opus encoder: %w", err)
		}
		s.opusEnc = enc
	default:
		return fmt.Errorf("no RTP encoder for %s", forma.Codec())
	}
	return nil
}

// writePackets stamps and sends one frame's worth of RTP packets, then advances
// the running timestamp by tsStep.
//
// The encoders number timestamps from zero on every call, so the running value
// is added here; and none of them raise the marker bit for the start of a
// talkspurt (RFC 3550), so the first packet of the session gets it. The AAC
// encoder does set the marker per access unit — that is left alone, this only
// ever raises the bit.
func (s *Session) writePackets(pkts []*rtp.Packet, tsStep uint32) error {
	for _, pkt := range pkts {
		pkt.Timestamp += s.ts
		if !s.marked {
			pkt.Marker = true
			s.marked = true
		}
		if err := s.client.WritePacketRTP(s.media, pkt); err != nil {
			return err
		}
	}
	s.ts += tsStep
	return nil
}

// sendLoopG711 paces 20 ms G.711 frames, sending silence whenever the caller
// has not fed audio: the channel has to stay warm because a camera gives no
// signal that it is ready to play what we send.
func (s *Session) sendLoopG711() {
	defer close(s.done)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var buf []int16

	for {
		select {
		case <-s.stop:
			return
		case samples := <-s.audioIn:
			buf = append(buf, samples...)
			if len(buf) > MaxBufferSamples {
				buf = buf[len(buf)-MaxBufferSamples:]
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
			if s.codec == codecPCMU {
				payload = encodeULaw(frame)
			} else {
				payload = encodeALaw(frame)
			}

			pkts, err := s.g711Enc.Encode(payload)
			if err != nil {
				slog.Warn("backchannel G711 packetize failed", "err", err)
				return
			}
			if err := s.writePackets(pkts, FrameSamples); err != nil {
				slog.Warn("backchannel RTP send failed", "err", err)
				return
			}
		}
	}
}

// sendLoopAAC forwards client-encoded AAC access units as they arrive; the RTP
// timestamp advances by the track's frame length per AU.
func (s *Session) sendLoopAAC() {
	defer close(s.done)

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
			pkts, err := s.aacEnc.Encode([][]byte{au})
			if err != nil {
				slog.Warn("backchannel AAC packetize failed", "err", err)
				return
			}
			if err := s.writePackets(pkts, s.aacTSStep); err != nil {
				slog.Warn("backchannel AAC send failed", "err", err)
				return
			}
		}
	}
}

// sendLoopOpus forwards client-encoded Opus packets (RFC 7587): each RTP
// payload is one raw 20 ms packet and the timestamp advances 960 samples on
// the 48 kHz clock.
func (s *Session) sendLoopOpus() {
	defer close(s.done)

	for {
		select {
		case <-s.stop:
			return
		case frame := <-s.opusIn:
			if len(frame) == 0 {
				continue
			}
			pkt, err := s.opusEnc.Encode(frame)
			if err != nil {
				slog.Warn("backchannel Opus packetize failed", "err", err)
				return
			}
			if err := s.writePackets([]*rtp.Packet{pkt}, OpusFrameSamples); err != nil {
				slog.Warn("backchannel Opus send failed", "err", err)
				return
			}
		}
	}
}

// FeedPCM decodes native-rate mono S16LE PCM, resamples it to 8 kHz when needed,
// and queues it for transmission. Oversized bursts are dropped rather than
// blocking the caller (the RTP loop paces at a fixed 20 ms).
func (s *Session) FeedPCM(pcm []byte, nativeRate int) {
	if s.audioIn == nil || len(pcm) < 2 {
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
	enqueueDropOldest(s.auIn, b, "backchannel AAC buffer full, dropping oldest AU")
}

// FeedOpus queues one raw Opus packet for transmission on the Opus backchannel
// (RFC 7587: one packet per RTP payload, no AU framing). It is a no-op on a
// non-Opus session. The packet must be a single 20 ms frame so the RTP
// timestamp increments stay aligned; oversized bursts are dropped rather than
// blocking the caller.
func (s *Session) FeedOpus(pkt []byte) {
	if s.opusIn == nil || len(pkt) == 0 {
		return
	}
	b := make([]byte, len(pkt))
	copy(b, pkt)
	enqueueDropOldest(s.opusIn, b, "backchannel Opus buffer full, dropping oldest packet")
}

// enqueueDropOldest queues b onto ch, shedding the OLDEST queued element when
// the buffer is full — a backlog drops stale audio and keeps the freshest
// speech, instead of rejecting new audio and letting latency grow. The caller
// is the sole producer, so once a slot is freed the follow-up send has room.
func enqueueDropOldest(ch chan []byte, b []byte, fullMsg string) {
	select {
	case ch <- b:
	default:
		select {
		case <-ch:
			slog.Debug(fullMsg)
		default:
		}
		select {
		case ch <- b:
		default:
		}
	}
}

// Close stops the send loop and tears down the RTSP session (gortsplib sends
// the TEARDOWN and closes the connection). Idempotent: a sync.Once guards the
// body so the second caller is a no-op instead of panicking on the
// already-closed `stop` channel. This matters because two owners can race to
// close the same session — the talk handler's deferred Close and the shutdown
// path's CloseAllTalk.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.client.Close()
	})
}

// sleepCtx blocks for d or until ctx is cancelled, returning ctx.Err() if the
// context fires first. Used for the inter-step pause in Dial so a cancelled
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

// formatSummary renders a media's formats for the debug log.
func formatSummary(m *description.Media) string {
	out := ""
	for i, f := range m.Formats {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%d=%s/%d", f.PayloadType(), f.Codec(), f.ClockRate())
	}
	return out
}
