package recorder

import (
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// liveSample is a finalized sample as the live broadcaster receives it.
type liveSample struct {
	track     int
	dts       int64
	duration  uint32
	ptsOffset int32
	sync      bool
}

func captureLive(r *Recorder) *[]liveSample {
	out := &[]liveSample{}
	r.OnLiveSample = func(id int, s *fmp4.Sample, dts int64) {
		*out = append(*out, liveSample{id, dts, s.Duration, s.PTSOffset, !s.IsNonSyncSample})
	}
	return out
}

func videoOnly(s []liveSample) []liveSample {
	var v []liveSample
	for _, x := range s {
		if x.track == 1 {
			v = append(v, x)
		}
	}
	return v
}

var (
	h264IDR = []byte{0x65, 0x88, 0x84}
	h264P   = []byte{0x41, 0x9a}
)

const frame = 3600 // 40 ms at 90 kHz

// feedH264 sends one access unit per PTS (in 40 ms frames), an IDR where key
// is true.
func feedH264(r *Recorder, t *recTrack, ptsFrames []int64, key func(i int) bool) {
	t0 := time.Unix(1_700_000_000, 0)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, f := range ptsFrames {
		nalu := h264P
		if key(i) {
			nalu = h264IDR
		}
		pts := f * frame
		r.processH264(t, [][]byte{nalu}, pts, t0.Add(time.Duration(pts)*time.Second/90000))
	}
}

// A stream without B-frames that loses frames keeps DTS = PTS: the frame
// before a loss lasts until the next one (80 ms), instead of the extractor's
// 1 ms frame and a PTS-DTS offset that grows with every loss.
func TestDTSFromPTSAcrossLostFrames(t *testing.T) {
	r, video, _, _ := stallRecorder(t, time.Hour)
	r.Record = false
	live := captureLive(r)

	// 0, 40, 80, 120, (160 lost), 200, 240, 280 ms
	feedH264(r, video, []int64{0, 1, 2, 3, 5, 6, 7}, func(i int) bool { return i == 0 })

	got := videoOnly(*live)
	want := []struct{ dts, dur int64 }{{0, 1}, {1, 1}, {2, 1}, {3, 2}, {5, 1}, {6, 1}}
	if len(got) != len(want) {
		t.Fatalf("got %d finalized frames, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.dts != w.dts*frame || int64(g.duration) != w.dur*frame || g.ptsOffset != 0 {
			t.Errorf("frame %d: dts=%d dur=%d offset=%d, want dts=%d dur=%d offset=0",
				i, g.dts, g.duration, g.ptsOffset, w.dts*frame, w.dur*frame)
		}
	}
	if r.reordering {
		t.Error("lost frames were taken for reordering")
	}
}

// A frame shown before one decoded earlier means B-frames: the extractor takes
// over from the next keyframe, only the video is dropped meanwhile, and the
// segment (with its audio) goes on.
func TestDTSFromPTSHandsOverOnBFrames(t *testing.T) {
	r, video, audio, segs := stallRecorder(t, time.Hour)
	live := captureLive(r)
	t0 := time.Unix(1_700_000_000, 0)

	// decode order I0 P3 B1 B2 P6: B1 is shown before P3
	feedH264(r, video, []int64{0, 3, 1, 2, 6}, func(i int) bool { return i == 0 })
	if !r.reordering {
		t.Fatal("B-frame not detected")
	}
	if !r.videoStarted {
		t.Fatal("video gate closed on the hand-over: audio would be dropped")
	}
	if v := videoOnly(*live); len(v) != 1 || v[0].dts != 0 {
		t.Errorf("video after the hand-over = %+v, want only I0 finalized (B/P dropped until a keyframe)", v)
	}

	// audio keeps flowing into the same segment
	seg := r.currentSegment
	r.mu.Lock()
	for i := 0; i < 50; i++ {
		audio.write(&sample{Sample: &fmp4.Sample{Payload: []byte{0, 0}}, dts: int64(i) * 160, ntp: t0.Add(time.Duration(i) * 20 * time.Millisecond)})
	}
	r.mu.Unlock()
	if r.currentSegment == nil || r.currentSegment != seg || len(*segs) != 0 {
		t.Error("the hand-over ended the segment")
	}
	var audioOut int
	for _, s := range *live {
		if s.track == 2 {
			audioOut++
		}
	}
	if audioOut < 40 {
		t.Errorf("only %d audio samples went through after the hand-over", audioOut)
	}
}

// A clock that goes back further than reordering can explain is a time-base
// discontinuity: the segment ends, the stale lookahead frame is dropped, and
// video resumes on the next keyframe with DTS = PTS in the new base — no
// extractor, no negative or clamped durations.
func TestDTSFromPTSClockJumpResyncs(t *testing.T) {
	r, video, _, segs := stallRecorder(t, time.Hour)
	live := captureLive(r)

	// 100 s worth of frames, then the camera clock restarts near 10 s on a P,
	// then a keyframe
	frames := []int64{2500, 2501, 2502, 250, 251, 252, 253}
	feedH264(r, video, frames, func(i int) bool { return i == 0 || i == 4 })

	if r.reordering {
		t.Fatal("a clock jump was taken for B-frames")
	}
	if len(*segs) != 1 {
		t.Errorf("got %d finalized segments, want 1 (closed at the jump)", len(*segs))
	}
	got := videoOnly(*live)
	for _, s := range got {
		if s.dts == 2502*frame {
			t.Error("the stale lookahead frame from the old time base was written")
		}
		if s.dts == 250*frame {
			t.Error("the non-key frame right after the jump was written")
		}
		if s.duration > 2*frame {
			t.Errorf("frame at %d stretched to %d across the jump", s.dts, s.duration)
		}
	}
	last := got[len(got)-1]
	if last.dts != 252*frame || last.ptsOffset != 0 {
		t.Errorf("last frame dts=%d offset=%d, want %d / 0 (DTS = PTS in the new base)", last.dts, last.ptsOffset, 252*frame)
	}
}

// H265 takes the same path.
func TestDTSFromPTSH265(t *testing.T) {
	r := &Recorder{Logf: func(string, ...any) {}}
	codec := &mcodecs.H265{VPS: []byte{0x40, 0x01}, SPS: []byte{0x42, 0x01}, PPS: []byte{0x44, 0x01}}
	r.videoCodecH265 = codec
	video := &recTrack{r: r, clockRate: 90000, initTrack: &fmp4.InitTrack{ID: 1, TimeScale: 90000, Codec: codec}}
	r.tracks = []*recTrack{video}
	live := captureLive(r)

	idr := []byte{19 << 1, 0x01}  // IDR_W_RADL
	trail := []byte{1 << 1, 0x01} // TRAIL_R
	t0 := time.Unix(1_700_000_000, 0)
	r.mu.Lock()
	for i, f := range []int64{0, 1, 2, 4, 5} { // frame 3 lost
		nalu := trail
		if i == 0 {
			nalu = idr
		}
		r.processH265(video, [][]byte{nalu}, f*frame, t0)
	}
	r.mu.Unlock()

	got := *live
	want := []struct{ dts, dur int64 }{{0, 1}, {1, 1}, {2, 2}, {4, 1}}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].dts != w.dts*frame || int64(got[i].duration) != w.dur*frame || got[i].ptsOffset != 0 {
			t.Errorf("frame %d = %+v, want dts=%d dur=%d", i, got[i], w.dts*frame, w.dur*frame)
		}
	}
}
