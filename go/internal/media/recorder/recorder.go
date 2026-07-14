// Package recorder connects to an RTSP source, extracts its video track (H264
// or H265) — and, if present, its AAC or G711 audio track — and writes them to
// disk as fragmented-MP4 segments compatible with the MediaMTX layout
// (including the "mtxi" box for gapless concatenation on playback).
//
// It is a focused, multi-track port of MediaMTX's fMP4 recorder: video (H264 or
// H265/HEVC) + optional audio (MPEG-4 Audio / AAC, or G711 converted to LPCM).
// Segments always start on a video keyframe.
//
// The implementation is split across recorder.go (orchestration), track.go
// (per-track state), segment.go (on-disk segment + part writing), init.go
// (fMP4 init/duration header writers), and helpers.go (timestamp math).
package recorder

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/google/uuid"
	"github.com/pion/rtp"

	"eneverre/internal/media/recstore"
)

// maxBasetime bounds how far apart tracks may start a segment (avoids large basetimes).
const maxBasetime = 1 * time.Second

// noVideoTimeout forces a reconnect when no video packet arrives for this long
// while connected. Well above any camera's keyframe interval, so a healthy
// stream never trips it, but a stalled one recovers instead of hanging forever.
const noVideoTimeout = 8 * time.Second

// ErrNoSupportedVideo is returned by Start when the RTSP stream carries no video
// codec the recorder can handle (H264 or H265; AV1/etc. are not). Callers can
// detect it with errors.Is to report a clear diagnostic and back off (retrying
// fast won't help until the camera's codec changes). The wrapped message lists
// the codecs the stream actually offered.
var ErrNoSupportedVideo = errors.New("no supported video codec (recorder handles H264/H265)")

// videoDTS is the DTS-extraction contract shared by H264 and H265: both
// *h264.DTSExtractor and *h265.DTSExtractor satisfy it, so the recorder holds
// one field and the per-codec process function picks the concrete type.
type videoDTS interface {
	Initialize()
	Extract(au [][]byte, pts int64) (int64, error)
}

// SegmentInfo is delivered when a segment is completely written.
type SegmentInfo struct {
	Path          string
	Start         time.Time
	Duration      time.Duration
	SegmentNumber uint64
	StreamID      string
}

// Recorder records a single RTSP stream (H264 video + optional AAC/G711 audio).
type Recorder struct {
	URL             string
	PathName        string
	PathFormat      string
	SegmentDuration time.Duration
	PartDuration    time.Duration
	MaxPartSize     uint64
	Transport       string // "auto" (default: UDP with TCP fallback) | "tcp" | "udp"
	// Record controls disk persistence. When false the recorder still
	// connects, demuxes and forwards RTP to the live RTSP relay + browser MSE
	// broadcaster, but does NOT write segments to disk and does NOT call
	// OnSegment (no index rows either). The engine sets this explicitly per
	// camera; the zero value is false (live-only).
	Record    bool
	OnSegment func(SegmentInfo)
	Logf      func(string, ...any)

	// Live relay hooks (optional). OnSource is called with the camera description
	// once connected; OnRTP is called for every incoming RTP packet (raw
	// passthrough); OnSourceLost is called when the connection ends.
	OnSource     func(*description.Session) error
	OnRTP        func(*description.Media, *rtp.Packet)
	OnSourceLost func()

	// Live web (MSE) hooks (optional). OnLiveTracks is called once the fMP4
	// tracks are known; OnLiveSample is called for every finalized sample.
	OnLiveTracks func([]*fmp4.InitTrack) error
	OnLiveSample func(trackID int, s *fmp4.Sample, dts int64)

	client   *gortsplib.Client
	pathFmt  string
	streamID uuid.UUID

	// video state. Exactly one of videoCodec / videoCodecH265 is set per
	// connection, depending on the codec the camera offered; both point at the
	// same object stored in the video track's InitTrack, so mutating SPS/PPS(/VPS)
	// here updates the fMP4 init too. dtsExtractor holds the matching extractor.
	videoCodec     *mcodecs.H264
	videoCodecH265 *mcodecs.H265
	dtsExtractor   videoDTS
	videoStarted   bool

	// lastVideoNano is the UnixNano of the most recent video RTP packet. A media
	// watchdog uses it to force a reconnect when a camera goes silent while
	// keeping the RTSP session half-alive (common on weak/distant links), which
	// the transport-level read timeout doesn't always catch.
	lastVideoNano atomic.Int64

	// shared multi-track state (guarded by mu, since RTP callbacks for
	// different medias may run on different goroutines)
	mu                sync.Mutex
	tracks            []*recTrack
	hasVideo          bool
	currentSegment    *segment
	nextSegmentNumber uint64
}

// Start connects and records. Blocks until the connection ends or Close.
func (r *Recorder) Start() error {
	if r.Logf == nil {
		r.Logf = func(string, ...any) {}
	}
	if r.MaxPartSize == 0 {
		r.MaxPartSize = 50 * 1024 * 1024
	}
	if r.PartDuration == 0 {
		r.PartDuration = time.Second
	}
	if r.SegmentDuration == 0 {
		r.SegmentDuration = time.Hour
	}

	r.streamID = uuid.New()
	r.pathFmt = recstore.PathAddExtension(strings.ReplaceAll(r.PathFormat, "%path", r.PathName))
	// reset per-connection state
	r.tracks = nil
	r.hasVideo = false
	r.currentSegment = nil
	r.videoCodec = nil
	r.videoCodecH265 = nil
	r.dtsExtractor = nil
	r.videoStarted = false

	u, err := base.ParseURL(r.URL)
	if err != nil {
		return err
	}

	r.client = &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	// Route gortsplib's transport diagnostics through our Logf (which the
	// engine prefixes with media/recorder[<id>]:) so each line carries the
	// camera id. gortsplib's defaults log to the standard log package,
	// which loses that context.
	r.client.OnPacketsLost = func(lost uint64) {
		word := "packets"
		if lost == 1 {
			word = "packet"
		}
		r.Logf("%d RTP %s lost", lost, word)
	}
	r.client.OnDecodeError = func(err error) {
		r.Logf("decode error: %v", err)
	}
	// Transport selection. "auto" (default) is gortsplib's behaviour: try UDP and
	// fall back to TCP. "tcp" forces reliable delivery — recommended for recording
	// since UDP packet loss corrupts the H264 access-unit sequence and trips the
	// DTS extractor (frames are dropped, though recording continues).
	switch strings.ToLower(r.Transport) {
	case "", "auto":
		// leave Protocol nil
	case "tcp":
		p := gortsplib.ProtocolTCP
		r.client.Protocol = &p
	case "udp":
		p := gortsplib.ProtocolUDP
		r.client.Protocol = &p
	default:
		return fmt.Errorf("invalid transport %q (want auto|tcp|udp)", r.Transport)
	}
	if err = r.client.Start(); err != nil {
		return err
	}

	desc, _, err := r.client.Describe(u)
	if err != nil {
		return err
	}

	var offered []string
	for _, m := range desc.Medias {
		for _, f := range m.Formats {
			offered = append(offered, f.Codec())
		}
	}
	r.Logf("stream codecs: %s", strings.Join(offered, ", "))

	// Relay-only cameras (no live web broadcaster wired, recording off) don't
	// need the H264/AAC/G711 -> fMP4 sample pipeline: the RTSP relay is fed by
	// the raw-RTP OnRTP path. When neither sink is active we still connect,
	// Setup every media (so OnRTP forwards them) and run the watchdog, but skip
	// the per-packet decode + sample assembly.
	demux := r.OnLiveSample != nil || r.Record

	// --- video (required: H264 or H265) ---
	var h264Forma *format.H264
	var h265Forma *format.H265
	videoMedia := desc.FindFormat(&h264Forma)
	if videoMedia == nil {
		videoMedia = desc.FindFormat(&h265Forma)
	}
	if videoMedia == nil {
		// Return before publishing to the relay: without a decodable video track
		// there is nothing useful to record or relay. Wrap the sentinel so the
		// engine can report it clearly and slow its retries.
		return fmt.Errorf("%w; stream offers: %s", ErrNoSupportedVideo, strings.Join(offered, ", "))
	}

	// publish the source to the live relay (if wired). A relay failure must not
	// take the camera down: recording and the live MSE feed are independent
	// sinks fed from the same RTP, so log the relay off for this session and
	// keep going. (OnRTP below then no-ops harmlessly — the relay drops packets
	// for a path with no source.)
	if r.OnSource != nil {
		if err = r.OnSource(desc); err != nil {
			r.Logf("live relay disabled for this session: %v", err)
		}
	}

	// Build the video track, its RTP decoder and the per-packet assembly step
	// for whichever codec the camera sent. `videoForma` is the codec format used
	// to register the RTP callback; `assemble` decodes one packet into an access
	// unit and feeds the appropriate processX. The two branches mirror each
	// other; only the codec object, decoder type and process function differ.
	var videoForma format.Format
	var assemble func(pkt *rtp.Packet)
	switch {
	case h264Forma != nil:
		r.videoCodec = &mcodecs.H264{}
		if h264Forma.SPS != nil {
			r.videoCodec.SPS = h264Forma.SPS
		}
		if h264Forma.PPS != nil {
			r.videoCodec.PPS = h264Forma.PPS
		}
		videoTrack := r.addTrack(uint32(h264Forma.ClockRate()), r.videoCodec)
		videoForma = h264Forma
		dec, derr := h264Forma.CreateDecoder()
		if derr != nil {
			return derr
		}
		assemble = func(pkt *rtp.Packet) {
			pts, ok := r.client.PacketPTS(videoMedia, pkt)
			if !ok {
				return
			}
			au, err2 := dec.Decode(pkt)
			if err2 != nil {
				if !errors.Is(err2, rtph264.ErrNonStartingPacketAndNoPrevious) &&
					!errors.Is(err2, rtph264.ErrMorePacketsNeeded) {
					r.Logf("h264 rtp decode: %v", err2)
				}
				return
			}
			r.mu.Lock()
			r.processH264(videoTrack, au, pts, time.Now())
			r.mu.Unlock()
		}
	case h265Forma != nil:
		r.videoCodecH265 = &mcodecs.H265{}
		if h265Forma.VPS != nil {
			r.videoCodecH265.VPS = h265Forma.VPS
		}
		if h265Forma.SPS != nil {
			r.videoCodecH265.SPS = h265Forma.SPS
		}
		if h265Forma.PPS != nil {
			r.videoCodecH265.PPS = h265Forma.PPS
		}
		videoTrack := r.addTrack(uint32(h265Forma.ClockRate()), r.videoCodecH265)
		videoForma = h265Forma
		dec, derr := h265Forma.CreateDecoder()
		if derr != nil {
			return derr
		}
		assemble = func(pkt *rtp.Packet) {
			pts, ok := r.client.PacketPTS(videoMedia, pkt)
			if !ok {
				return
			}
			au, err2 := dec.Decode(pkt)
			if err2 != nil {
				if !errors.Is(err2, rtph265.ErrNonStartingPacketAndNoPrevious) &&
					!errors.Is(err2, rtph265.ErrMorePacketsNeeded) {
					r.Logf("h265 rtp decode: %v", err2)
				}
				return
			}
			r.mu.Lock()
			r.processH265(videoTrack, au, pts, time.Now())
			r.mu.Unlock()
		}
	}

	if _, err = r.client.Setup(desc.BaseURL, videoMedia, 0, 0); err != nil {
		return err
	}

	// --- audio (optional: AAC, or G711 converted to LPCM) ---
	if err := r.setupAudio(desc, demux); err != nil {
		return err
	}

	// --- video callback ---
	r.client.OnPacketRTP(videoMedia, videoForma, func(pkt *rtp.Packet) {
		r.lastVideoNano.Store(time.Now().UnixNano()) // feed the media watchdog
		if r.OnRTP != nil {                          // forward raw to live relay (lowest latency)
			r.OnRTP(videoMedia, pkt)
		}
		if !demux {
			return // relay-only: skip video decode + fMP4 assembly
		}
		assemble(pkt)
	})

	// publish fMP4 track config to the live web broadcaster (if wired)
	if r.OnLiveTracks != nil {
		its := make([]*fmp4.InitTrack, len(r.tracks))
		for i, t := range r.tracks {
			its[i] = t.initTrack
		}
		if err = r.OnLiveTracks(its); err != nil {
			return err
		}
	}

	r.Logf("recording %s -> %s", r.URL, r.pathFmt)

	if _, err = r.client.Play(nil); err != nil {
		return err
	}

	// Media watchdog: some cameras (weak/distant links) stop sending video but
	// keep the RTSP session alive, so client.Wait blocks forever and the
	// transport read timeout never fires. If no video packet arrives for
	// noVideoTimeout, close the client to unblock Wait; the engine then
	// reconnects with a fresh session, which usually recovers the stream.
	r.lastVideoNano.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-t.C:
				since := time.Since(time.Unix(0, r.lastVideoNano.Load()))
				if since > noVideoTimeout {
					r.Logf("no video for %s, forcing reconnect", since.Round(time.Second))
					r.client.Close() // unblocks Wait below
					return
				}
			}
		}
	}()

	werr := r.client.Wait()
	close(watchdogDone)

	// The source ended (disconnect / silent stall caught by the read timeout).
	// Finalize the in-progress segment so its already-written fMP4 parts are
	// given a valid moov duration and indexed via OnSegment, instead of being
	// abandoned — capitalizing on the fragmented layout so a mid-segment drop
	// keeps everything recorded up to the cut. Guarded by mu, and safe because
	// the client has stopped delivering packets by the time Wait returns. The
	// retry loop then reconnects and starts a fresh segment.
	r.mu.Lock()
	if r.currentSegment != nil {
		hadFile := r.currentSegment.fi != nil
		cerr := r.currentSegment.close()
		r.Logf("finalized in-progress segment on disconnect (hadFile=%v, err=%v)", hadFile, cerr)
		r.currentSegment = nil
	} else {
		r.Logf("disconnect with no in-progress segment to finalize")
	}
	r.mu.Unlock()

	if r.OnSourceLost != nil {
		r.OnSourceLost()
	}
	return werr
}

// IsConnected reports whether the recorder is currently receiving video, i.e.
// a video packet arrived within the last noVideoTimeout window (plus a small
// margin). This is the same signal the media watchdog uses.
func (r *Recorder) IsConnected() bool {
	// Lock-free on purpose: this is called from the metrics scrape and must not
	// contend with r.mu, which the RTP callback holds per access-unit around the
	// fMP4 assembly + disk write. lastVideoNano is atomic and is the real
	// "receiving video" signal; before the first packet it is 0, so the elapsed
	// check already reports disconnected without needing to inspect r.client.
	last := r.lastVideoNano.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) <= noVideoTimeout+2*time.Second
}

// Close stops recording and flushes the current segment.
func (r *Recorder) Close() {
	if r.client != nil {
		r.client.Close()
	}
	r.mu.Lock()
	if r.currentSegment != nil {
		r.currentSegment.close() //nolint:errcheck
		r.currentSegment = nil
	}
	r.mu.Unlock()
}

func (r *Recorder) addTrack(clockRate uint32, codec mcodecs.Codec) *recTrack {
	t := &recTrack{
		r:         r,
		clockRate: clockRate,
		initTrack: &fmp4.InitTrack{ID: len(r.tracks) + 1, TimeScale: clockRate, Codec: codec},
	}
	r.tracks = append(r.tracks, t)
	return t
}

// setupAudio discovers the audio track (AAC or G711), wires its RTP callback,
// and returns an error when the decoder or RTSP SETUP fails. A missing audio
// track (neither AAC nor G711 in the SDP) is not an error — the recorder
// continues video-only.
func (r *Recorder) setupAudio(desc *description.Session, demux bool) error {
	audioOK := false

	// AAC (MPEG-4 Audio)
	var aacForma *format.MPEG4Audio
	if aacMedia := desc.FindFormat(&aacForma); aacMedia != nil && aacForma.Config != nil {
		if err := r.setupAAC(desc, aacMedia, aacForma, demux); err != nil {
			return err
		}
		audioOK = true
	}

	// G711 (A-law / mu-law) -> LPCM 16-bit big-endian (fMP4 can't carry G711)
	if !audioOK {
		var g711Forma *format.G711
		if g711Media := desc.FindFormat(&g711Forma); g711Media != nil {
			if err := r.setupG711(desc, g711Media, g711Forma, demux); err != nil {
				return err
			}
			audioOK = true
		}
	}

	if !audioOK {
		r.Logf("no supported audio track found; recording video only")
	}
	return nil
}

func (r *Recorder) setupAAC(desc *description.Session, aacMedia *description.Media, aacForma *format.MPEG4Audio, demux bool) error {
	audioTrack := r.addTrack(uint32(aacForma.ClockRate()),
		&mcodecs.MPEG4Audio{Config: *aacForma.Config})
	aacDec, err := aacForma.CreateDecoder()
	if err != nil {
		return fmt.Errorf("aac decoder: %w", err)
	}
	if _, err = r.client.Setup(desc.BaseURL, aacMedia, 0, 0); err != nil {
		return fmt.Errorf("aac setup: %w", err)
	}
	r.Logf("audio track: AAC %d Hz", aacForma.ClockRate())

	r.wireAudio(audioTrack, aacMedia, aacForma, demux, func(pkt *rtp.Packet, pts int64) []*sample {
		aus, err := aacDec.Decode(pkt)
		if err != nil {
			if !errors.Is(err, rtpmpeg4audio.ErrMorePacketsNeeded) {
				r.Logf("aac rtp decode: %v", err)
			}
			return nil
		}
		// One RTP packet may carry several access units; each is a sample with
		// its own PTS/NTP (SamplesPerAccessUnit apart at the track clock rate).
		ntp := time.Now()
		samples := make([]*sample, len(aus))
		for i, a := range aus {
			p := pts + int64(i)*mpeg4audio.SamplesPerAccessUnit
			samples[i] = &sample{
				Sample: &fmp4.Sample{Payload: a},
				dts:    p,
				ntp:    ntp.Add(timestampToDuration(p-pts, int(audioTrack.clockRate))),
			}
		}
		return samples
	})
	return nil
}

func (r *Recorder) setupG711(desc *description.Session, g711Media *description.Media, g711Forma *format.G711, demux bool) error {
	audioTrack := r.addTrack(uint32(g711Forma.ClockRate()), &mcodecs.LPCM{
		LittleEndian: false,
		BitDepth:     16,
		SampleRate:   g711Forma.SampleRate,
		ChannelCount: g711Forma.ChannelCount,
	})
	g711Dec, err := g711Forma.CreateDecoder()
	if err != nil {
		return fmt.Errorf("g711 decoder: %w", err)
	}
	if _, err = r.client.Setup(desc.BaseURL, g711Media, 0, 0); err != nil {
		return fmt.Errorf("g711 setup: %w", err)
	}
	law := "A-law"
	if g711Forma.MULaw {
		law = "mu-law"
	}
	r.Logf("audio track: G711 %s %d Hz -> LPCM", law, g711Forma.ClockRate())

	mulaw := g711Forma.MULaw
	r.wireAudio(audioTrack, g711Media, g711Forma, demux, func(pkt *rtp.Packet, pts int64) []*sample {
		enc, err := g711Dec.Decode(pkt)
		if err != nil {
			r.Logf("g711 rtp decode: %v", err)
			return nil
		}
		var lpcm []byte
		if mulaw {
			var m g711.Mulaw
			m.Unmarshal(enc)
			lpcm = m
		} else {
			var a g711.Alaw
			a.Unmarshal(enc)
			lpcm = a
		}
		return []*sample{{
			Sample: &fmp4.Sample{Payload: lpcm},
			dts:    pts,
			ntp:    time.Now(),
		}}
	})
	return nil
}

// wireAudio registers the RTP-callback envelope shared by every audio codec:
// raw OnRTP passthrough, the relay-only short-circuit (!demux), PTS extraction,
// and the mu-guarded "drop until first video keyframe" gate. decode turns one
// packet into zero or more samples (nil on a decode error it already logged);
// they are written to the track under r.mu only once video has started.
func (r *Recorder) wireAudio(t *recTrack, media *description.Media, forma format.Format, demux bool, decode func(pkt *rtp.Packet, pts int64) []*sample) {
	r.client.OnPacketRTP(media, forma, func(pkt *rtp.Packet) {
		if r.OnRTP != nil {
			r.OnRTP(media, pkt)
		}
		if !demux {
			return
		}
		pts, ok := r.client.PacketPTS(media, pkt)
		if !ok {
			return
		}
		samples := decode(pkt, pts)
		if len(samples) == 0 {
			return
		}
		r.mu.Lock()
		if r.videoStarted { // drop audio until first video keyframe
			for _, s := range samples {
				t.write(s)
			}
		}
		r.mu.Unlock()
	})
}

// processH264 turns an access unit into a sample and feeds it to the video track.
// Caller must hold r.mu.
func (r *Recorder) processH264(t *recTrack, au [][]byte, pts int64, ntp time.Time) {
	randomAccess := false
	hasSPS, hasPPS, hasVCL := false, false, false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeSPS:
			hasSPS = true
			if !bytes.Equal(r.videoCodec.SPS, nalu) {
				r.videoCodec.SPS = nalu
			}
		case h264.NALUTypePPS:
			hasPPS = true
			if !bytes.Equal(r.videoCodec.PPS, nalu) {
				r.videoCodec.PPS = nalu
			}
		case h264.NALUTypeIDR:
			randomAccess = true
		}
		if typ >= 1 && typ <= 5 {
			hasVCL = true
		}
	}

	// RTSP delivers SPS/PPS out-of-band (SDP); inject them into keyframe AUs.
	if randomAccess {
		var prefix [][]byte
		if !hasSPS && r.videoCodec.SPS != nil {
			prefix = append(prefix, r.videoCodec.SPS)
		}
		if !hasPPS && r.videoCodec.PPS != nil {
			prefix = append(prefix, r.videoCodec.PPS)
		}
		if prefix != nil {
			au = append(prefix, au...)
		}
	}

	// AUs without a coded slice (params/SEI/AUD only) carry no picture.
	if !hasVCL {
		return
	}

	if r.dtsExtractor == nil {
		if !randomAccess {
			return // wait for the first keyframe
		}
		r.dtsExtractor = &h264.DTSExtractor{}
		r.dtsExtractor.Initialize()
		r.videoStarted = true
	}

	dts, err := r.dtsExtractor.Extract(au, pts)
	if err != nil {
		// reset so the extractor recovers on the next keyframe instead of
		// staying in a bad state (e.g. after packet loss / reordering).
		r.Logf("dts extract: %v (resetting)", err)
		r.dtsExtractor = nil
		return
	}

	var s fmp4.Sample
	if err = s.FillH264(int32(pts-dts), au); err != nil {
		r.Logf("fill h264: %v", err)
		return
	}
	t.write(&sample{Sample: &s, dts: dts, ntp: ntp})
}

// processH265 turns an H265 access unit into a sample and feeds it to the video
// track. Parallel to processH264: the differences are the NALU type encoding
// (6-bit type in bits 1..6 of the first byte), the extra VPS parameter set, the
// VCL type range (0..31), and the h265 DTS extractor / IsRandomAccess helpers.
// Caller must hold r.mu.
func (r *Recorder) processH265(t *recTrack, au [][]byte, pts int64, ntp time.Time) {
	hasVPS, hasSPS, hasPPS, hasVCL := false, false, false, false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		typ := h265.NALUType((nalu[0] >> 1) & 0b111111)
		switch typ {
		case h265.NALUType_VPS_NUT:
			hasVPS = true
			if !bytes.Equal(r.videoCodecH265.VPS, nalu) {
				r.videoCodecH265.VPS = nalu
			}
		case h265.NALUType_SPS_NUT:
			hasSPS = true
			if !bytes.Equal(r.videoCodecH265.SPS, nalu) {
				r.videoCodecH265.SPS = nalu
			}
		case h265.NALUType_PPS_NUT:
			hasPPS = true
			if !bytes.Equal(r.videoCodecH265.PPS, nalu) {
				r.videoCodecH265.PPS = nalu
			}
		}
		if typ <= 31 { // VCL NAL unit types are 0..31
			hasVCL = true
		}
	}

	// IDR_W_RADL / IDR_N_LP / CRA_NUT — the AUs a decoder can seek to.
	randomAccess := h265.IsRandomAccess(au)

	// RTSP delivers VPS/SPS/PPS out-of-band (SDP); inject them into random-access
	// AUs, same rationale as the H264 SPS/PPS injection.
	if randomAccess {
		var prefix [][]byte
		if !hasVPS && r.videoCodecH265.VPS != nil {
			prefix = append(prefix, r.videoCodecH265.VPS)
		}
		if !hasSPS && r.videoCodecH265.SPS != nil {
			prefix = append(prefix, r.videoCodecH265.SPS)
		}
		if !hasPPS && r.videoCodecH265.PPS != nil {
			prefix = append(prefix, r.videoCodecH265.PPS)
		}
		if prefix != nil {
			au = append(prefix, au...)
		}
	}

	// AUs without a coded slice (params/SEI/AUD only) carry no picture.
	if !hasVCL {
		return
	}

	if r.dtsExtractor == nil {
		if !randomAccess {
			return // wait for the first random-access AU
		}
		ex := &h265.DTSExtractor{}
		ex.Initialize()
		r.dtsExtractor = ex
		r.videoStarted = true
	}

	dts, err := r.dtsExtractor.Extract(au, pts)
	if err != nil {
		// reset so the extractor recovers on the next keyframe instead of
		// staying in a bad state (e.g. after packet loss / reordering).
		r.Logf("dts extract: %v (resetting)", err)
		r.dtsExtractor = nil
		return
	}

	var s fmp4.Sample
	if err = s.FillH265(int32(pts-dts), au); err != nil {
		r.Logf("fill h265: %v", err)
		return
	}
	t.write(&sample{Sample: &s, dts: dts, ntp: ntp})
}
