package recorder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// stallFeed drives a recorder with 25 fps video and 50 packets/s audio,
// interleaved by wall-clock, where video is absent in [videoOff, videoOn) while
// the audio keeps flowing. It returns the segments the recorder finalized.
// stallRecorder builds a recording Recorder with an H264 video track and an
// 8 kHz audio track, collecting the segments it finalizes.
func stallRecorder(t *testing.T, segDur time.Duration) (*Recorder, *recTrack, *recTrack, *[]SegmentInfo) {
	t.Helper()
	segs := &[]SegmentInfo{}
	r := &Recorder{
		Record:          true,
		pathFmt:         filepath.Join(t.TempDir(), "%Y-%m-%d_%H-%M-%S-%f.mp4"),
		PartDuration:    time.Second,
		SegmentDuration: segDur,
		Logf:            func(string, ...any) {},
		OnSegment:       func(s SegmentInfo) { *segs = append(*segs, s) },
	}
	codec := &mcodecs.H264{
		SPS: []byte{
			0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
			0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
			0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20,
		},
		PPS: []byte{0x08, 0x06, 0x07, 0x08},
	}
	r.videoCodec = codec
	video := &recTrack{r: r, clockRate: 90000, initTrack: &fmp4.InitTrack{ID: 1, TimeScale: 90000, Codec: codec}}
	audio := &recTrack{r: r, clockRate: 8000, initTrack: &fmp4.InitTrack{
		ID: 2, TimeScale: 8000,
		Codec: &mcodecs.LPCM{BitDepth: 16, SampleRate: 8000, ChannelCount: 1},
	}}
	r.tracks = []*recTrack{video, audio}
	return r, video, audio, segs
}

func stallFeed(t *testing.T, segDur, videoOff, videoOn, total time.Duration) []SegmentInfo {
	t.Helper()
	r, video, audio, segs := stallRecorder(t, segDur)
	r.videoStarted = true

	t0 := time.Unix(1_700_000_000, 0)
	vi, ai := 0, 0
	for {
		vd := time.Duration(vi) * 40 * time.Millisecond
		ad := time.Duration(ai) * 20 * time.Millisecond
		if vd >= total && ad >= total {
			break
		}
		if vd >= videoOff && vd < videoOn {
			vi++ // camera sends no video here
			continue
		}
		r.mu.Lock()
		if vd < total && vd <= ad {
			video.write(&sample{
				// keyframe every second
				Sample: &fmp4.Sample{Payload: []byte{5}, IsNonSyncSample: vi%25 != 0},
				dts:    int64(vi) * 3600, ntp: t0.Add(vd),
			})
			vi++
		} else {
			audio.write(&sample{
				Sample: &fmp4.Sample{Payload: []byte{0, 0}},
				dts:    int64(ai) * 160, ntp: t0.Add(ad),
			})
			ai++
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	if r.currentSegment != nil {
		r.currentSegment.close() //nolint:errcheck
	}
	r.mu.Unlock()
	return *segs
}

func approx(d, want time.Duration) bool {
	diff := d - want
	return diff > -500*time.Millisecond && diff < 500*time.Millisecond
}

// A camera that stops sending video but keeps its audio flowing keeps the
// audio: the segment lasts as long as the audio, and its video extent says
// where the video ends. Reproduces a field recording with 1.5s of video and
// 212s of audio in one segment, minus the stretched last frame.
func TestVideoStallKeepsAudio(t *testing.T) {
	segs := stallFeed(t, time.Hour, 2*time.Second, 20*time.Second, 25*time.Second)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (stall, then the resumed video)", len(segs))
	}
	s := segs[0]
	if !approx(s.Duration, 20*time.Second) {
		t.Errorf("segment duration = %s, want ~20s (the audio)", s.Duration)
	}
	if s.VideoStart != 0 || !approx(s.VideoDuration, 2*time.Second) {
		t.Errorf("video extent = %s+%s, want 0+~2s", s.VideoStart, s.VideoDuration)
	}
	if !approx(segs[1].VideoDuration, segs[1].Duration) {
		t.Errorf("resumed segment: video %s of %s, want the whole segment", segs[1].VideoDuration, segs[1].Duration)
	}
}

// During a long outage the audio rotates segments at the usual length, and
// the video's return starts a new segment on its keyframe.
func TestVideoStallRotatesOnAudio(t *testing.T) {
	segs := stallFeed(t, 10*time.Second, 2*time.Second, 37*time.Second, 45*time.Second)
	if len(segs) < 4 {
		t.Fatalf("got %d segments, want >= 4", len(segs))
	}
	if !approx(segs[0].VideoDuration, 2*time.Second) {
		t.Errorf("first segment video = %s, want ~2s", segs[0].VideoDuration)
	}
	var audioOnly time.Duration
	for _, s := range segs[1 : len(segs)-1] {
		if s.Duration > 11*time.Second {
			t.Errorf("audio-only segment of %s, want <= ~10s", s.Duration)
		}
		if s.VideoDuration != 0 {
			t.Errorf("segment during the outage has %s of video", s.VideoDuration)
		}
		audioOnly += s.Duration
	}
	last := segs[len(segs)-1]
	if last.VideoStart != 0 || !approx(last.VideoDuration, last.Duration) {
		t.Errorf("resumed segment: video %s+%s of %s, want it to start with the video",
			last.VideoStart, last.VideoDuration, last.Duration)
	}
	if total := segs[0].Duration + audioOnly; !approx(total, 37*time.Second) {
		t.Errorf("audio recorded through the outage = %s, want ~37s", total)
	}
}

// When the audio opens a segment while the video is down and the video's
// keyframe is already waiting in lookahead with an earlier DTS (B-frames: it is
// decoded well before it is shown), the segment must start early enough to
// keep that keyframe instead of discarding it as late.
func TestAudioOpenedSegmentKeepsPendingKeyframe(t *testing.T) {
	r, video, audio, _ := stallRecorder(t, time.Hour)
	r.videoStarted = true
	r.hasVideo = true
	r.resyncKeyframe = true
	r.connStart = time.Now().Add(-time.Minute) // video down long enough: audio may open
	t0 := time.Unix(1_700_000_000, 0)
	const lag = 600 * time.Millisecond // keyframe shown 0.6s after it decodes

	r.mu.Lock()
	defer r.mu.Unlock()
	audio.write(&sample{Sample: &fmp4.Sample{Payload: []byte{0, 0}}, dts: 80000, ntp: t0.Add(10 * time.Second)})
	video.write(&sample{
		Sample: &fmp4.Sample{Payload: []byte{5}, PTSOffset: int32(lag.Seconds() * 90000)},
		dts:    int64(9.8 * 90000), ntp: t0.Add(9800 * time.Millisecond),
	})
	// the next audio packet writes the first one: it opens the segment
	audio.write(&sample{Sample: &fmp4.Sample{Payload: []byte{0, 0}}, dts: 80160, ntp: t0.Add(10020 * time.Millisecond)})
	if r.currentSegment == nil {
		t.Fatal("audio did not open a segment while the video was down")
	}
	// the next video frame writes the pending keyframe
	video.write(&sample{
		Sample: &fmp4.Sample{Payload: []byte{1}, IsNonSyncSample: true},
		dts:    int64(9.84 * 90000), ntp: t0.Add(9840 * time.Millisecond),
	})
	if !r.currentSegment.hasVideo {
		t.Fatal("the pending keyframe was discarded as late; the segment has no video")
	}
	if got, want := r.currentSegment.videoStart-r.currentSegment.startDTS, lag; got != want {
		t.Errorf("video starts %s into the segment, want %s (its presentation time)", got, want)
	}
}
