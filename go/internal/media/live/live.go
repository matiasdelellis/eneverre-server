// Package live broadcasts the camera's live stream to web browsers as a
// continuous fragmented-MP4 byte stream (init + parts) over chunked HTTP, to be
// fed into a MediaSource SourceBuffer. Latency is ~1-2s.
//
// It muxes in memory from the recorder's samples (no disk, no re-encode). The
// video track (H264 or H265/HEVC) is included with its codec string; LPCM/G711
// audio is dropped from the live stream (browsers can't decode it) — recording
// and the RTSP relay still carry it.
//
// H265 is advertised with its hvc1 codec string and the client decides
// playability via MediaSource.isTypeSupported (Safari yes; Firefox/Chrome only
// with a system/hardware HEVC decoder) — the same mechanism the recordings
// timeline (hls.js) uses. This is not "HLS live": in every browser except Safari,
// HLS is played by hls.js, which itself feeds MSE, so it is the same decode path
// as this broadcaster and would add no HEVC capability MSE doesn't already have.
// A client that can't decode H265 shows a clear message and uses the RTSP relay.
package live

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

const (
	partDuration  = 300 * time.Millisecond // target fMP4 part length (latency knob)
	subChanBuffer = 128                    // parts queued per client before dropping (slow client)
)

// Broadcaster muxes live parts and fans them out to HTTP subscribers.
type Broadcaster struct {
	Logf func(string, ...any)

	mu         sync.Mutex
	running    bool
	warnedDrop bool              // logged the "audio dropped from live" notice once
	tracks     []*fmp4.InitTrack // MSE-compatible subset, in stable order
	timeScale  map[int]uint32    // trackID -> timescale (included tracks only)
	videoID    int
	initBytes  []byte
	mime       string
	// unsupportedVideo names the camera's video codec when it was offered but
	// cannot be MSE-broadcast (currently only H265). Empty when live is available
	// or when the camera is simply not connected yet. HandleInfo surfaces it so
	// the web UI shows a clear "can't play this codec in the browser" message
	// rather than an endless reconnect spinner.
	unsupportedVideo string
	gop              [][]byte // parts since (and including) the last keyframe part
	subs             map[*subscriber]struct{}

	// current part accumulation
	seq          uint32
	samples      map[int][]*fmp4.Sample
	baseDTS      map[int]int64
	curVideoDur  time.Duration
	partKeyframe bool
	havePartVid  bool
}

type subscriber struct {
	ch chan []byte
}

// Initialize prepares the broadcaster.
func (b *Broadcaster) Initialize() {
	if b.Logf == nil {
		b.Logf = func(string, ...any) {}
	}
	b.subs = map[*subscriber]struct{}{}
	b.resetPart()
}

// SetTracks (re)initializes the live stream from the source tracks. Called by the
// recorder on connect. Any current subscribers are dropped so they reconnect
// with the new init.
func (b *Broadcaster) SetTracks(all []*fmp4.InitTrack) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var incl []*fmp4.InitTrack
	var dropped []string
	b.timeScale = map[int]uint32{}
	b.videoID = 0
	b.unsupportedVideo = ""
	codecs := ""
	unsupportedVideo := ""

	for _, t := range all {
		switch c := t.Codec.(type) {
		case *mcodecs.H264:
			incl = append(incl, t)
			b.timeScale[t.ID] = t.TimeScale
			if b.videoID == 0 {
				b.videoID = t.ID
			}
			codecs = appendCodec(codecs, avcCodec(c.SPS))
		case *mcodecs.MPEG4Audio:
			incl = append(incl, t)
			b.timeScale[t.ID] = t.TimeScale
			codecs = appendCodec(codecs, "mp4a.40.2")
		case *mcodecs.H265:
			// H265/HEVC: browser MSE support varies (Safari yes; Firefox/Chrome
			// only with a system/hardware HEVC decoder). We advertise it with the
			// proper hvc1 codec string and let each client's
			// MediaSource.isTypeSupported decide — the same mechanism the
			// recordings timeline (hls.js) already relies on. Clients that can't
			// decode it show a clear message and fall back to the RTSP relay.
			cs := hvcCodec(c.SPS)
			if cs == "" {
				// SPS unparseable → no codec string → can't broadcast it. Record
				// the reason so HandleInfo can still explain the codec.
				if unsupportedVideo == "" {
					unsupportedVideo = "H265"
				}
				dropped = append(dropped, fmt.Sprintf("%T", t.Codec))
				break
			}
			incl = append(incl, t)
			b.timeScale[t.ID] = t.TimeScale
			if b.videoID == 0 {
				b.videoID = t.ID
			}
			codecs = appendCodec(codecs, cs)
		default:
			// LPCM / G711 / others: not MSE-decodable, skip for live.
			dropped = append(dropped, fmt.Sprintf("%T", t.Codec))
		}
	}

	if b.videoID == 0 {
		// Persist the reason (if any) before returning so HandleInfo can report it.
		b.unsupportedVideo = unsupportedVideo
		if unsupportedVideo != "" {
			return fmt.Errorf("no MSE-compatible video track (camera uses %s; browsers can't play it — use the RTSP relay)", unsupportedVideo)
		}
		return fmt.Errorf("no MSE-compatible video track")
	}

	// Cameras without AAC (e.g. G711/LPCM-only ONVIF) play silent in the browser.
	// Warn once so the operator knows the wall has no sound by design, not a bug.
	if len(dropped) > 0 && !b.warnedDrop {
		b.warnedDrop = true
		b.Logf("live: browser stream is video-only; audio track(s) not MSE-decodable, dropped: %v (recording and RTSP relay keep audio)", dropped)
	}

	var buf seekablebuffer.Buffer
	if err := (&fmp4.Init{Tracks: incl}).Marshal(&buf); err != nil {
		return err
	}

	b.tracks = incl
	b.initBytes = append([]byte(nil), buf.Bytes()...)
	b.mime = `video/mp4; codecs="` + codecs + `"`
	b.gop = nil
	b.seq = 0
	b.running = true
	b.resetPartLocked()

	// reset existing viewers
	for s := range b.subs {
		close(s.ch)
		delete(b.subs, s)
	}
	b.Logf("live source ready (%s)", b.mime)
	return nil
}

// IsRunning reports whether the broadcaster has an active live source (the
// camera is connected and delivering samples).
func (b *Broadcaster) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running && b.initBytes != nil
}

// Stop ends the current live stream (camera disconnected).
func (b *Broadcaster) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running = false
	b.initBytes = nil
	b.gop = nil
	for s := range b.subs {
		close(s.ch)
		delete(b.subs, s)
	}
}

// WriteSample feeds one finalized sample (Duration already set) from the recorder.
func (b *Broadcaster) WriteSample(trackID int, s *fmp4.Sample, dts int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return
	}
	ts, ok := b.timeScale[trackID]
	if !ok {
		return // excluded track (e.g. LPCM)
	}

	isVideo := trackID == b.videoID
	isKeyframe := isVideo && !s.IsNonSyncSample

	// Drop near-zero-duration video frames from the live stream. Some cameras
	// occasionally emit two frames <1ms apart (a capture/timestamp glitch); the
	// recorder's lookahead then gives the first of the pair a ~zero duration.
	// Browser MediaSource is strict about a well-spaced, monotonic timeline and
	// stalls on such a sample (black), whereas ffmpeg and file playback tolerate
	// it — so recording keeps the frame and only the live broadcast skips it.
	// Keyframes are never dropped (a GOP must start on one).
	if isVideo && !isKeyframe {
		minDur := 3 * int64(ts) / 1000 // ~3ms in the track's clock ticks
		if minDur < 1 {
			minDur = 1
		}
		if int64(s.Duration) < minDur {
			return
		}
	}

	// force a part boundary at each keyframe so a new part starts a GOP
	if isKeyframe && b.havePartVid {
		b.flushPartLocked()
	}

	if _, ok := b.baseDTS[trackID]; !ok {
		b.baseDTS[trackID] = dts
	}
	b.samples[trackID] = append(b.samples[trackID], s)

	if isVideo {
		if isKeyframe && len(b.samples[trackID]) == 1 {
			b.partKeyframe = true
		}
		b.havePartVid = true
		b.curVideoDur += time.Duration(int64(s.Duration) * int64(time.Second) / int64(ts))
		if b.curVideoDur >= partDuration {
			b.flushPartLocked()
		}
	}
}

func (b *Broadcaster) flushPartLocked() {
	part := &fmp4.Part{}
	for _, t := range b.tracks {
		ss := b.samples[t.ID]
		if len(ss) > 0 {
			part.Tracks = append(part.Tracks, &fmp4.PartTrack{
				ID:       t.ID,
				BaseTime: uint64(b.baseDTS[t.ID]),
				Samples:  ss,
			})
		}
	}
	if len(part.Tracks) == 0 {
		b.resetPartLocked()
		return
	}
	part.SequenceNumber = b.seq
	b.seq++

	var buf seekablebuffer.Buffer
	if err := part.Marshal(&buf); err != nil {
		b.Logf("live part marshal: %v", err)
		b.resetPartLocked()
		return
	}
	data := append([]byte(nil), buf.Bytes()...)

	// maintain the current-GOP buffer (reset on a keyframe part)
	if b.partKeyframe {
		b.gop = [][]byte{data}
	} else {
		b.gop = append(b.gop, data)
	}

	// fan out (drop for slow clients rather than block the recorder)
	for s := range b.subs {
		select {
		case s.ch <- data:
		default:
		}
	}
	b.resetPartLocked()
}

func (b *Broadcaster) resetPart() { b.resetPartLocked() }

func (b *Broadcaster) resetPartLocked() {
	b.samples = map[int][]*fmp4.Sample{}
	b.baseDTS = map[int]int64{}
	b.curVideoDur = 0
	b.partKeyframe = false
	b.havePartVid = false
}

// --- HTTP ---

// HandleInfo reports whether live is available and the MSE mime type. When live
// is unavailable because the camera's video codec cannot be played in a browser
// (H265), it adds {"reason":"unsupported_codec","codec":"H265"} so the web UI
// can show a clear, permanent message instead of retrying forever.
func (b *Broadcaster) HandleInfo(w http.ResponseWriter, _ *http.Request) {
	b.mu.Lock()
	avail := b.running && b.initBytes != nil
	mime := b.mime
	unsupported := b.unsupportedVideo
	b.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if avail {
		fmt.Fprintf(w, `{"available":true,"mime":%q}`, mime)
		return
	}
	if unsupported != "" {
		fmt.Fprintf(w, `{"available":false,"reason":"unsupported_codec","codec":%q}`, unsupported)
		return
	}
	w.Write([]byte(`{"available":false}`)) //nolint:errcheck
}

// HandleStream streams init + live parts as chunked fMP4 for MediaSource.
func (b *Broadcaster) HandleStream(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	if !b.running || b.initBytes == nil {
		b.mu.Unlock()
		http.Error(w, "no live source", http.StatusServiceUnavailable)
		return
	}
	sub := &subscriber{ch: make(chan []byte, subChanBuffer)}
	b.subs[sub] = struct{}{}
	init := b.initBytes
	gop := append([][]byte(nil), b.gop...) // start at the last keyframe
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if _, ok := b.subs[sub]; ok {
			delete(b.subs, sub)
			close(sub.ch)
		}
		b.mu.Unlock()
	}()

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")

	if _, err := w.Write(init); err != nil {
		return
	}
	for _, p := range gop {
		if _, err := w.Write(p); err != nil {
			return
		}
	}
	if fl != nil {
		fl.Flush()
	}

	for {
		select {
		case data, ok := <-sub.ch:
			if !ok {
				return // stream reset/stopped
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

// --- helpers ---

func appendCodec(cur, c string) string {
	if cur == "" {
		return c
	}
	return cur + "," + c
}

// avcCodec builds the "avc1.PPCCLL" codec string from an H264 SPS NALU.
func avcCodec(sps []byte) string {
	if len(sps) < 4 {
		return "avc1.42e01e"
	}
	return fmt.Sprintf("avc1.%02x%02x%02x", sps[1], sps[2], sps[3])
}

// hvcCodec builds the "hvc1.*" MSE codec string from an H265 SPS NALU by parsing
// its profile-tier-level. Format and field mapping mirror bluenviron/gohlslib's
// codecparams marshaller (which builds the same string for HLS CODECS). Returns
// "" if the SPS can't be parsed, so the caller falls back to the unsupported
// path. The client then decides playability via MediaSource.isTypeSupported.
func hvcCodec(spsNALU []byte) string {
	var sps h265.SPS
	if err := sps.Unmarshal(spsNALU); err != nil {
		return ""
	}
	ptl := &sps.ProfileTierLevel

	profileSpace := ""
	if s := ptl.GeneralProfileSpace; s >= 1 && s <= 3 {
		profileSpace = string(rune('A' + (s - 1)))
	}

	// general_profile_compatibility_flag[0..31] as a hex value.
	var compat uint32
	for i, b := range ptl.GeneralProfileCompatibilityFlag {
		if b {
			compat |= 1 << i
		}
	}

	tier := "L"
	if ptl.GeneralTierFlag > 0 {
		tier = "H"
	}

	return fmt.Sprintf("hvc1.%s%d.%x.%s%d.%s",
		profileSpace, ptl.GeneralProfileIdc,
		compat,
		tier, ptl.GeneralLevelIdc,
		hvcConstraintFlags(ptl))
}

// hvcConstraintFlags encodes the 6-byte general_constraint_indicator_flags of an
// H265 SPS as dot-separated hex bytes with trailing zero bytes omitted (only the
// first two bytes carry flags mediacommon parses). Mirrors gohlslib.
func hvcConstraintFlags(v *h265.SPS_ProfileTierLevel) string {
	var o1 uint8
	if v.GeneralProgressiveSourceFlag {
		o1 |= 1 << 7
	}
	if v.GeneralInterlacedSourceFlag {
		o1 |= 1 << 6
	}
	if v.GeneralNonPackedConstraintFlag {
		o1 |= 1 << 5
	}
	if v.GeneralFrameOnlyConstraintFlag {
		o1 |= 1 << 4
	}
	if v.GeneralMax12bitConstraintFlag {
		o1 |= 1 << 3
	}
	if v.GeneralMax10bitConstraintFlag {
		o1 |= 1 << 2
	}
	if v.GeneralMax8bitConstraintFlag {
		o1 |= 1 << 1
	}
	if v.GeneralMax422ChromeConstraintFlag {
		o1 |= 1 << 0
	}
	ret := []string{fmt.Sprintf("%x", o1)}

	var o2 uint8
	if v.GeneralMax420ChromaConstraintFlag {
		o2 |= 1 << 7
	}
	if v.GeneralMaxMonochromeConstraintFlag {
		o2 |= 1 << 6
	}
	if v.GeneralIntraConstraintFlag {
		o2 |= 1 << 5
	}
	if v.GeneralOnePictureOnlyConstraintFlag {
		o2 |= 1 << 4
	}
	if v.GeneralLowerBitRateConstraintFlag {
		o2 |= 1 << 3
	}
	if v.GeneralMax14BitConstraintFlag {
		o2 |= 1 << 2
	}
	if o2 != 0 {
		ret = append(ret, fmt.Sprintf("%x", o2))
	}

	return strings.Join(ret, ".")
}
