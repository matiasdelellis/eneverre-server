// Package recorder connects to an RTSP source, extracts its H264 video track
// (and, if present, its AAC audio track) and writes them to disk as
// fragmented-MP4 segments compatible with the MediaMTX layout (including the
// "mtxi" box for gapless concatenation on playback).
//
// It is a focused, multi-track port of MediaMTX's fMP4 recorder: video (H264) +
// optional audio (MPEG-4 Audio / AAC). Segments always start on a video keyframe.
package recorder

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/google/uuid"
	"github.com/pion/rtp"

	"eneverre/internal/media/mtxi"
	"eneverre/internal/media/recstore"
)

// this bounds how far apart tracks may start a segment (avoids large basetimes)
const maxBasetime = 1 * time.Second

// ErrNoSupportedVideo is returned by Start when the RTSP stream carries no video
// codec the recorder can handle (currently H264 only; H265/AV1/etc. are not).
// Callers can detect it with errors.Is to report a clear diagnostic and back off
// (retrying fast won't help until the camera's codec changes). The wrapped
// message lists the codecs the stream actually offered.
var ErrNoSupportedVideo = errors.New("no supported video codec (recorder handles H264 only)")

// SegmentInfo is delivered when a segment is completely written.
type SegmentInfo struct {
	Path          string
	Start         time.Time
	Duration      time.Duration
	SegmentNumber uint64
	StreamID      string
}

// Recorder records a single RTSP stream (H264 video + optional AAC audio).
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
	Record          bool
	OnSegment       func(SegmentInfo)
	Logf            func(string, ...any)

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

	// video state
	videoCodec   *mcodecs.H264
	dtsExtractor *h264.DTSExtractor
	videoStarted bool

	// lastVideoNano is the UnixNano of the most recent video RTP packet. A media
	// watchdog (see Start) uses it to force a reconnect when a camera goes silent
	// while keeping the RTSP session half-alive (common on weak/distant links),
	// which the transport-level read timeout doesn't always catch.
	lastVideoNano atomic.Int64

	// shared multi-track state (guarded by mu, since RTP callbacks for
	// different medias may run on different goroutines)
	mu                sync.Mutex
	tracks            []*recTrack
	hasVideo          bool
	currentSegment    *segment
	nextSegmentNumber uint64
}

// noVideoTimeout forces a reconnect when no video packet arrives for this long
// while connected. Well above any camera's keyframe interval, so a healthy
// stream never trips it, but a stalled one recovers instead of hanging forever.
const noVideoTimeout = 8 * time.Second

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

	// Log the codecs the stream advertises, so an unsupported camera (e.g. one
	// that only sends H265) is diagnosable instead of just failing to record.
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

	// --- video (required) ---
	var videoForma *format.H264
	videoMedia := desc.FindFormat(&videoForma)
	if videoMedia == nil {
		// Return before publishing to the relay: without a decodable video track
		// there is nothing useful to record or relay. Wrap the sentinel so the
		// engine can report it clearly and slow its retries.
		return fmt.Errorf("%w; stream offers: %s", ErrNoSupportedVideo, strings.Join(offered, ", "))
	}

	// publish the source to the live relay (if wired)
	if r.OnSource != nil {
		if err = r.OnSource(desc); err != nil {
			return err
		}
	}
	r.videoCodec = &mcodecs.H264{}
	if videoForma.SPS != nil {
		r.videoCodec.SPS = videoForma.SPS
	}
	if videoForma.PPS != nil {
		r.videoCodec.PPS = videoForma.PPS
	}
	videoTrack := r.addTrack(uint32(videoForma.ClockRate()), r.videoCodec)

	videoDec, err := videoForma.CreateDecoder()
	if err != nil {
		return err
	}
	if _, err = r.client.Setup(desc.BaseURL, videoMedia, 0, 0); err != nil {
		return err
	}

	// --- audio (optional: AAC, or G711 converted to LPCM) ---
	audioOK := false

	// AAC (MPEG-4 Audio)
	var aacForma *format.MPEG4Audio
	if aacMedia := desc.FindFormat(&aacForma); aacMedia != nil && aacForma.Config != nil {
		audioTrack := r.addTrack(uint32(aacForma.ClockRate()),
			&mcodecs.MPEG4Audio{Config: *aacForma.Config})
		aacDec, err2 := aacForma.CreateDecoder()
		if err2 != nil {
			return err2
		}
		if _, err2 = r.client.Setup(desc.BaseURL, aacMedia, 0, 0); err2 != nil {
			return err2
		}
		r.Logf("audio track: AAC %d Hz", aacForma.ClockRate())

		r.client.OnPacketRTP(aacMedia, aacForma, func(pkt *rtp.Packet) {
			if r.OnRTP != nil {
				r.OnRTP(aacMedia, pkt)
			}
			if !demux {
				return // relay-only: raw RTP already forwarded above
			}
			pts, ok := r.client.PacketPTS(aacMedia, pkt)
			if !ok {
				return
			}
			aus, err3 := aacDec.Decode(pkt)
			if err3 != nil {
				if !errors.Is(err3, rtpmpeg4audio.ErrMorePacketsNeeded) {
					r.Logf("aac rtp decode: %v", err3)
				}
				return
			}
			ntp := time.Now()
			r.mu.Lock()
			if r.videoStarted { // drop audio until first video keyframe
				for i, a := range aus {
					p := pts + int64(i)*mpeg4audio.SamplesPerAccessUnit
					audioTrack.write(&sample{
						Sample: &fmp4.Sample{Payload: a},
						dts:    p,
						ntp:    ntp.Add(timestampToDuration(p-pts, int(audioTrack.clockRate))),
					})
				}
			}
			r.mu.Unlock()
		})
		audioOK = true
	}

	// G711 (A-law / mu-law) -> LPCM 16-bit big-endian (fMP4 can't carry G711)
	if !audioOK {
		var g711Forma *format.G711
		if g711Media := desc.FindFormat(&g711Forma); g711Media != nil {
			audioTrack := r.addTrack(uint32(g711Forma.ClockRate()), &mcodecs.LPCM{
				LittleEndian: false,
				BitDepth:     16,
				SampleRate:   g711Forma.SampleRate,
				ChannelCount: g711Forma.ChannelCount,
			})
			g711Dec, err2 := g711Forma.CreateDecoder()
			if err2 != nil {
				return err2
			}
			if _, err2 = r.client.Setup(desc.BaseURL, g711Media, 0, 0); err2 != nil {
				return err2
			}
			law := "A-law"
			if g711Forma.MULaw {
				law = "mu-law"
			}
			r.Logf("audio track: G711 %s %d Hz -> LPCM", law, g711Forma.ClockRate())

			mulaw := g711Forma.MULaw
			r.client.OnPacketRTP(g711Media, g711Forma, func(pkt *rtp.Packet) {
				if r.OnRTP != nil {
					r.OnRTP(g711Media, pkt)
				}
				if !demux {
					return // relay-only: raw RTP already forwarded above
				}
				pts, ok := r.client.PacketPTS(g711Media, pkt)
				if !ok {
					return
				}
				enc, err3 := g711Dec.Decode(pkt)
				if err3 != nil {
					r.Logf("g711 rtp decode: %v", err3)
					return
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
				ntp := time.Now()
				r.mu.Lock()
				if r.videoStarted { // drop audio until first video keyframe
					audioTrack.write(&sample{
						Sample: &fmp4.Sample{Payload: lpcm},
						dts:    pts,
						ntp:    ntp,
					})
				}
				r.mu.Unlock()
			})
			audioOK = true
		}
	}

	if !audioOK {
		r.Logf("no supported audio track found; recording video only")
	}

	// --- video callback ---
	r.client.OnPacketRTP(videoMedia, videoForma, func(pkt *rtp.Packet) {
		r.lastVideoNano.Store(time.Now().UnixNano()) // feed the media watchdog
		if r.OnRTP != nil {              // forward raw to live relay (lowest latency)
			r.OnRTP(videoMedia, pkt)
		}
		if !demux {
			return // relay-only: skip H264 decode + fMP4 assembly
		}
		pts, ok := r.client.PacketPTS(videoMedia, pkt)
		if !ok {
			return
		}
		au, err2 := videoDec.Decode(pkt)
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

// --- sample + track (port of MediaMTX format_fmp4_track) ---

type sample struct {
	*fmp4.Sample
	dts int64 // in the track's clock rate units
	ntp time.Time
}

type recTrack struct {
	r         *Recorder
	clockRate uint32
	initTrack *fmp4.InitTrack

	nextSample *sample
}

// write buffers one sample of lookahead (to compute the previous sample's
// duration) and drives segment creation/rotation. Caller must hold r.mu.
//
// In live-only mode (r.Record == false) the segment creation/writing and
// rotation are skipped entirely, but the live broadcaster still receives the
// sample (so MSE /live/stream keeps working). The on-disk layout is unchanged
// for cameras that do record.
func (t *recTrack) write(smp *sample) {
	r := t.r
	if t.initTrack.Codec.IsVideo() {
		r.hasVideo = true
	}

	smp, t.nextSample = t.nextSample, smp
	if smp == nil {
		return
	}

	duration := t.nextSample.dts - smp.dts
	if duration < 0 {
		t.nextSample.dts = smp.dts
		duration = 0
	}
	smp.Duration = uint32(duration)

	// feed the same finalized sample to the live web broadcaster. Done
	// unconditionally — the broadcaster is independent of disk persistence.
	if r.OnLiveSample != nil {
		r.OnLiveSample(t.initTrack.ID, smp.Sample, smp.dts)
	}

	// Live-only mode: skip segment creation, writing, and rotation. r.currentSegment
	// stays nil; the disconnect/Close handlers no-op on it.
	if !r.Record {
		return
	}

	dts := timestampToDuration(smp.dts, int(t.clockRate))

	if r.currentSegment == nil {
		r.currentSegment = r.newSegment(dts, smp.ntp)
	} else if (dts - r.currentSegment.startDTS) < 0 {
		r.Logf("sample of track %d received too late, discarding", t.initTrack.ID)
		return
	}

	if err := r.currentSegment.write(t, smp, dts); err != nil {
		r.Logf("segment write: %v", err)
		r.currentSegment.close() //nolint:errcheck
		r.currentSegment = nil
		return
	}

	// rotate only on a video keyframe once the minimum duration elapsed
	nextDTS := timestampToDuration(t.nextSample.dts, int(t.clockRate))
	if (!r.hasVideo || t.initTrack.Codec.IsVideo()) &&
		!t.nextSample.IsNonSyncSample &&
		(nextDTS-r.currentSegment.startDTS) >= r.SegmentDuration {
		if err := r.currentSegment.close(); err != nil {
			r.Logf("segment close: %v", err)
		}
		oldestNTP, oldestDTS := r.nextSegmentStartingPos()
		r.currentSegment = r.newSegment(oldestDTS, oldestNTP)
	}
}

// nextSegmentStartingPos picks the oldest pending sample across tracks (within
// maxBasetime of the newest) so the next segment starts early enough for every
// track, avoiding negative or huge basetimes.
func (r *Recorder) nextSegmentStartingPos() (time.Time, time.Duration) {
	var maxDTS time.Duration
	for _, t := range r.tracks {
		if t.nextSample != nil {
			dts := timestampToDuration(t.nextSample.dts, int(t.clockRate))
			if dts > maxDTS {
				maxDTS = dts
			}
		}
	}
	var oldestNTP time.Time
	oldestDTS := maxDTS
	for _, t := range r.tracks {
		if t.nextSample != nil {
			dts := timestampToDuration(t.nextSample.dts, int(t.clockRate))
			if (maxDTS-dts) <= maxBasetime && dts <= oldestDTS {
				oldestNTP = t.nextSample.ntp
				oldestDTS = dts
			}
		}
	}
	return oldestNTP, oldestDTS
}

func (r *Recorder) newSegment(startDTS time.Duration, startNTP time.Time) *segment {
	s := &segment{r: r, startDTS: startDTS, startNTP: startNTP, number: r.nextSegmentNumber, endDTS: startDTS}
	r.nextSegmentNumber++
	return s
}

// --- segment ---

type segment struct {
	r        *Recorder
	startDTS time.Duration
	startNTP time.Time
	number   uint64

	path           string
	fi             *os.File
	curPart        *part
	endDTS         time.Duration
	nextPartNumber uint32
}

func (s *segment) write(t *recTrack, smp *sample, dts time.Duration) error {
	endDTS := dts + timestampToDuration(int64(smp.Duration), int(t.clockRate))
	if endDTS > s.endDTS {
		s.endDTS = endDTS
	}

	if s.curPart == nil {
		s.curPart = newPart(s.startDTS, s.nextPartNumber, dts)
		s.nextPartNumber++
	} else if s.curPart.duration() >= s.r.PartDuration {
		if err := s.closeCurPart(); err != nil {
			s.curPart = nil
			return err
		}
		s.curPart = newPart(s.startDTS, s.nextPartNumber, dts)
		s.nextPartNumber++
	}

	return s.curPart.write(t, smp, dts)
}

func (s *segment) closeCurPart() error {
	if s.fi == nil {
		s.path = recstore.Path{Start: s.startNTP}.Encode(s.r.pathFmt)
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			return err
		}
		fi, err := os.Create(s.path)
		if err != nil {
			return err
		}
		if err = writeInit(fi, s.r.streamID, s.number, s.startDTS, s.startNTP, s.r.tracks); err != nil {
			fi.Close()
			return err
		}
		s.fi = fi
	}
	return s.curPart.close(s.fi)
}

func (s *segment) close() error {
	var err error
	if s.curPart != nil {
		err = s.closeCurPart()
	}
	if s.fi != nil {
		duration := s.endDTS - s.startDTS
		if e := writeDuration(s.fi, duration); err == nil {
			err = e
		}
		e := s.fi.Close()
		if err == nil {
			err = e
		}
		if e == nil && s.r.OnSegment != nil {
			s.r.OnSegment(SegmentInfo{
				Path:          s.path,
				Start:         s.startNTP,
				Duration:      duration,
				SegmentNumber: s.number,
				StreamID:      s.r.streamID.String(),
			})
		}
	}
	return err
}

// --- part (fMP4 fragment, multi-track) ---

type part struct {
	segmentStartDTS time.Duration
	number          uint32
	startDTS        time.Duration
	endDTS          time.Duration

	partTracks map[*recTrack]*fmp4.PartTrack
	size       uint64
}

func newPart(segmentStartDTS time.Duration, number uint32, startDTS time.Duration) *part {
	return &part{
		segmentStartDTS: segmentStartDTS,
		number:          number,
		startDTS:        startDTS,
		endDTS:          startDTS,
		partTracks:      make(map[*recTrack]*fmp4.PartTrack),
	}
}

func (p *part) write(t *recTrack, smp *sample, dts time.Duration) error {
	size := uint64(len(smp.Payload))
	if t.r.MaxPartSize > 0 && (p.size+size) > t.r.MaxPartSize {
		return fmt.Errorf("reached maximum part size")
	}
	p.size += size

	pt, ok := p.partTracks[t]
	if !ok {
		// dts is guaranteed >= segmentStartDTS by the "received too late" guard
		// in recTrack.write (a sample older than the current segment is dropped
		// before it reaches here). Clamp anyway: a negative delta would wrap the
		// uint64 BaseTime into a garbage value, corrupting the whole part.
		baseDelta := int64(dts - p.segmentStartDTS)
		if baseDelta < 0 {
			baseDelta = 0
		}
		pt = &fmp4.PartTrack{
			ID:       t.initTrack.ID,
			BaseTime: uint64(multiplyAndDivide(baseDelta, int64(t.clockRate), int64(time.Second))),
		}
		p.partTracks[t] = pt
	}
	pt.Samples = append(pt.Samples, smp.Sample)

	endDTS := dts + timestampToDuration(int64(smp.Duration), int(t.clockRate))
	if endDTS > p.endDTS {
		p.endDTS = endDTS
	}
	return nil
}

func (p *part) close(w io.Writer) error {
	tracks := make([]*fmp4.PartTrack, 0, len(p.partTracks))
	for _, pt := range p.partTracks {
		tracks = append(tracks, pt)
	}
	fpart := &fmp4.Part{SequenceNumber: p.number, Tracks: tracks}

	var buf seekablebuffer.Buffer
	if err := fpart.Marshal(&buf); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func (p *part) duration() time.Duration { return p.endDTS - p.startDTS }

// --- fMP4 init/duration writers ---

func writeInit(f io.Writer, streamID uuid.UUID, segNumber uint64, dts time.Duration, ntp time.Time, tracks []*recTrack) error {
	fmp4Tracks := make([]*fmp4.InitTrack, len(tracks))
	for i, t := range tracks {
		fmp4Tracks[i] = t.initTrack
	}

	init := fmp4.Init{
		Tracks: fmp4Tracks,
		UserData: []amp4.IBox{
			&mtxi.Box{
				FullBox:       amp4.FullBox{Version: 0},
				StreamID:      [16]byte(streamID),
				SegmentNumber: segNumber,
				DTS:           int64(dts),
				NTP:           ntp.UnixNano(),
			},
		},
	}
	var buf seekablebuffer.Buffer
	if err := init.Marshal(&buf); err != nil {
		return err
	}
	_, err := f.Write(buf.Bytes())
	return err
}

// writeDuration rewrites the total duration into mvhd (timescale 1000).
func writeDuration(f io.ReadWriteSeeker, d time.Duration) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	buf := make([]byte, 8)
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return fmt.Errorf("ftyp box not found")
	}
	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	if _, err := f.Seek(int64(ftypSize), io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return fmt.Errorf("moov box not found")
	}
	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	moovPos, err := f.Seek(8, io.SeekCurrent)
	if err != nil {
		return err
	}

	var mvhd amp4.Mvhd
	if _, err = amp4.Unmarshal(f, uint64(moovSize-8), &mvhd, amp4.Context{}); err != nil {
		return err
	}
	mvhd.DurationV0 = uint32(d / time.Millisecond)

	if _, err = f.Seek(moovPos, io.SeekStart); err != nil {
		return err
	}
	_, err = amp4.Marshal(f, &mvhd, amp4.Context{})
	return err
}

// --- timestamp helpers ---

func multiplyAndDivide(v, m, d int64) int64 {
	secs := v / d
	dec := v % d
	return secs*m + dec*m/d
}

func timestampToDuration(t int64, clockRate int) time.Duration {
	return time.Duration(multiplyAndDivide(t, int64(time.Second), int64(clockRate)))
}
