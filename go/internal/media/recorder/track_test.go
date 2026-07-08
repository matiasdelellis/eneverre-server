package recorder

import (
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

func trackWithPending(clockRate uint32, dts int64, ntp time.Time) *recTrack {
	return &recTrack{
		clockRate:  clockRate,
		initTrack:  &fmp4.InitTrack{ID: 1, TimeScale: clockRate},
		nextSample: &sample{Sample: &fmp4.Sample{}, dts: dts, ntp: ntp},
	}
}

// nextSegmentStartingPos should pick the oldest pending sample that is still
// within maxBasetime of the newest, so the next segment starts early enough
// for every track without producing huge basetimes.
func TestNextSegmentStartingPos(t *testing.T) {
	video := time.Unix(100, 0)
	audio := time.Unix(101, 0)

	t.Run("picks oldest within maxBasetime", func(t *testing.T) {
		r := &Recorder{tracks: []*recTrack{
			trackWithPending(90000, 90000*2, video), // 2s
			trackWithPending(8000, 8000*1, audio),   // 1s (oldest)
		}}
		ntp, dts := r.nextSegmentStartingPos()
		if dts != time.Second {
			t.Fatalf("dts = %v, want 1s", dts)
		}
		if !ntp.Equal(audio) {
			t.Fatalf("ntp = %v, want %v (audio)", ntp, audio)
		}
	})

	t.Run("excludes tracks beyond maxBasetime", func(t *testing.T) {
		r := &Recorder{tracks: []*recTrack{
			trackWithPending(90000, 90000*5, video), // 5s (newest)
			trackWithPending(8000, 8000*1, audio),   // 1s -> 4s behind, excluded
		}}
		ntp, dts := r.nextSegmentStartingPos()
		if dts != 5*time.Second {
			t.Fatalf("dts = %v, want 5s", dts)
		}
		if !ntp.Equal(video) {
			t.Fatalf("ntp = %v, want %v (video)", ntp, video)
		}
	})

	t.Run("ignores tracks with no pending sample", func(t *testing.T) {
		empty := &recTrack{clockRate: 8000, initTrack: &fmp4.InitTrack{ID: 2, TimeScale: 8000}}
		r := &Recorder{tracks: []*recTrack{
			trackWithPending(90000, 90000*3, video),
			empty,
		}}
		ntp, dts := r.nextSegmentStartingPos()
		if dts != 3*time.Second {
			t.Fatalf("dts = %v, want 3s", dts)
		}
		if !ntp.Equal(video) {
			t.Fatalf("ntp = %v, want %v (video)", ntp, video)
		}
	})
}
