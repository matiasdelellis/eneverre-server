package playback

import (
	"os"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// videoFillThreshold is how long a segment may go without a video sample while
// its other tracks keep going before the span is filled. It matches the
// recorder's stall cut (a longer video gap ends the segment), so a fill only
// ever lands at a segment's leading or trailing audio-only span, never inside a
// GOP of normal footage.
const videoFillThreshold = 5 * time.Second

// videoFill splices a placeholder frame into the spans of a segment that have
// no video: a camera can stop sending video while its audio keeps flowing, and
// that audio is still a recording. Without video samples a player seeking into
// such a span waits for frames that never come and stalls; with the
// placeholder (one keyframe per second, like the gap fill) it plays the audio
// over it.
//
// The placeholder carries its own parameter sets, so the stream must be
// emitted as avc3 (see muxerFMP4.useAVC3). One videoFill covers one segment.
type videoFill struct {
	trackID   int
	timeScale uint32
	payload   []byte

	// end is the presentation time up to which the video is covered (by real
	// samples or the placeholder); lastDTS is the last video DTS emitted, so
	// a real sample landing inside an already-filled span is dropped rather
	// than written out of order. Real frames are tracked by presentation time
	// (DTS + composition offset): a frame can be shown well after it is
	// decoded (B-frames, or the offset the H264 DTS extractor builds up when
	// frames are lost — 0.6 s on a field recording), and a placeholder placed
	// by DTS alone would be shown before frames that precede it.
	end     time.Duration
	started bool
	lastDTS int64
	// needKey drops real frames after a fill until a keyframe: a P-frame would
	// be decoded against the placeholder (whose SPS differs) as garbage.
	needKey bool

	// otherEnd is how far the segment's other tracks (audio) have reached.
	otherEnd time.Duration
}

// newVideoFill resolves the placeholder frame for the H264 track of tracks,
// captioned with message, with the segment starting at output time start.
// Returns nil when there is no H264 track or the frame cannot be generated:
// the segment is then muxed as is.
func newVideoFill(tracks []*fmp4.InitTrack, cacheDir, message string, start time.Duration) *videoFill {
	for _, t := range tracks {
		h264, ok := t.Codec.(*mcodecs.H264)
		if !ok {
			continue
		}
		w, h, ok := spsDimensions(h264.SPS)
		if !ok {
			return nil
		}
		payload, err := blackFramePayload(cacheDir, message, w, h)
		if err != nil {
			return nil
		}
		return &videoFill{trackID: t.ID, timeScale: t.TimeScale, payload: payload, end: start}
	}
	return nil
}

// segmentHasH264 reports whether a segment file's video track is H264 (the
// only codec the placeholder exists for). False when unreadable.
func segmentHasH264(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	init, _, err := readHeader(f)
	if err != nil {
		return false
	}
	for _, t := range init.Tracks {
		if _, ok := t.Codec.(*mcodecs.H264); ok {
			return true
		}
	}
	return false
}

// at returns a fresh fill for a segment starting at output time start, sharing
// the resolved placeholder. Nil-safe.
func (f *videoFill) at(start time.Duration) *videoFill {
	if f == nil {
		return nil
	}
	return &videoFill{trackID: f.trackID, timeScale: f.timeScale, payload: f.payload, end: start}
}

// video is called before a real video sample at dts (track timescale, with
// composition offset ptsOffset) of the given duration is written. It fills the
// span before it when the video was missing for longer than the threshold, and
// reports whether the sample may be written (false when a fill already covered
// its position, or it is not a keyframe right after a fill).
//
// The fill stops at the sample's decode time: the placeholders (shown when
// decoded, no offset) then come before it in both decode and presentation
// order.
func (f *videoFill) video(m muxer, dts int64, ptsOffset int32, duration uint32, sync bool) bool {
	if f.started && dts <= f.lastDTS {
		return false
	}
	t := durationMp4ToGo(dts, f.timeScale)
	if t-f.end > videoFillThreshold {
		f.fillTo(m, t)
	}
	pts := durationMp4ToGo(dts+int64(ptsOffset), f.timeScale)
	if f.needKey {
		if !sync {
			return false
		}
		f.needKey = false
	}
	d := durationMp4ToGo(int64(duration), f.timeScale)
	if d > videoFillThreshold {
		d = 0 // a frame stretched over a stall: the span after it is a hole
	}
	f.end = max(f.end, pts+d)
	f.started = true
	f.lastDTS = dts
	return true
}

// other records how far a non-video track has reached.
func (f *videoFill) other(end time.Duration) {
	f.otherEnd = max(f.otherEnd, end)
}

// catchUp fills the video up to where the other tracks are, minus the
// threshold, between fragments: the filler then stays interleaved with the
// audio instead of piling up at the end of the segment. The margin leaves room
// for video that is merely late in the file.
func (f *videoFill) catchUp(m muxer) {
	if f.otherEnd-f.end > 2*videoFillThreshold {
		f.fillTo(m, f.otherEnd-videoFillThreshold)
	}
}

// finish fills the rest of the segment, up to where its other tracks end
// (capped at limit), and closes the video track's last sample.
func (f *videoFill) finish(m muxer, limit time.Duration) {
	target := min(f.otherEnd, limit)
	if target-f.end > videoFillThreshold {
		f.fillTo(m, target)
		m.writeFinalDTS(durationGoToMp4(target, f.timeScale))
	}
}

// fillTo writes the placeholder from the covered end up to target, one
// keyframe per second. Starting at the covered presentation end keeps every
// placeholder after the real frames both in decode order (a frame's DTS never
// exceeds its PTS) and in presentation order. It leaves the muxer on the video
// track.
func (f *videoFill) fillTo(m muxer, target time.Duration) {
	m.setTrack(f.trackID)
	get := func() ([]byte, error) { return f.payload, nil }
	t := f.end
	for ; t < target; t += time.Second {
		dts := durationGoToMp4(t, f.timeScale)
		if f.started && dts <= f.lastDTS {
			continue
		}
		m.writeSample(dts, 0, false, uint32(len(f.payload)), get) //nolint:errcheck
		f.started = true
		f.lastDTS = dts
		f.needKey = true
	}
	f.end = max(f.end, t)
}
