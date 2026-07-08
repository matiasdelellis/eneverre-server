package recorder

import (
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

func testTrack(clockRate uint32) *recTrack {
	return &recTrack{clockRate: clockRate, initTrack: &fmp4.InitTrack{ID: 1, TimeScale: clockRate}}
}

func newSample(payload []byte) *sample {
	return &sample{Sample: &fmp4.Sample{Payload: payload}}
}

// A write that would push a part past maxSize is rejected and leaves the
// running size untouched, so the next (smaller) write can still succeed.
func TestPartMaxSize(t *testing.T) {
	tr := testTrack(90000)
	p := newPart(0, 0, 0, 10)

	if err := p.write(tr, newSample(make([]byte, 6)), 0); err != nil {
		t.Fatalf("first write (6 <= 10): unexpected error %v", err)
	}
	if err := p.write(tr, newSample(make([]byte, 6)), 0); err == nil {
		t.Fatal("second write (6+6 > 10): expected error, got nil")
	}
	if p.size != 6 {
		t.Fatalf("size = %d, want 6 (rejected write must not count)", p.size)
	}
	// A write that fits the remaining room still succeeds.
	if err := p.write(tr, newSample(make([]byte, 4)), 0); err != nil {
		t.Fatalf("third write (6+4 == 10): unexpected error %v", err)
	}
}

// maxSize == 0 means unbounded (production default is set in Start; a zero-value
// part must not reject anything).
func TestPartUnboundedWhenMaxSizeZero(t *testing.T) {
	tr := testTrack(90000)
	p := newPart(0, 0, 0, 0)
	if err := p.write(tr, newSample(make([]byte, 1<<20)), 0); err != nil {
		t.Fatalf("unbounded part rejected a large write: %v", err)
	}
}

// BaseTime is the track-clock offset of the part's first sample from the
// segment start, and a sample older than the segment start is clamped to 0
// rather than wrapping the uint64.
func TestPartBaseTime(t *testing.T) {
	t.Run("positive delta", func(t *testing.T) {
		tr := testTrack(90000)
		p := newPart(time.Second, 0, 2*time.Second, 0) // segment starts at 1s
		if err := p.write(tr, newSample([]byte{0}), 2*time.Second); err != nil {
			t.Fatalf("write: %v", err)
		}
		pt := p.partTracks[tr]
		if pt == nil {
			t.Fatal("no part track recorded")
		}
		// 1s of offset at 90kHz = 90000 ticks.
		if pt.BaseTime != 90000 {
			t.Fatalf("BaseTime = %d, want 90000", pt.BaseTime)
		}
	})

	t.Run("negative delta clamped to zero", func(t *testing.T) {
		tr := testTrack(90000)
		p := newPart(2*time.Second, 0, time.Second, 0) // segment starts at 2s
		if err := p.write(tr, newSample([]byte{0}), time.Second); err != nil {
			t.Fatalf("write: %v", err)
		}
		if pt := p.partTracks[tr]; pt == nil || pt.BaseTime != 0 {
			t.Fatalf("BaseTime = %v, want 0 (clamped)", pt)
		}
	})
}
