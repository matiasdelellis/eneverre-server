package recovery

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A sample stretched over a camera stall counts as one frame interval: the
// previous sample's, or 1/25 s when it is the only one. Neither the video
// extent nor the segment may come out as zero-length (read as "no video").
func TestMeasureCapsStretchedSample(t *testing.T) {
	dir := t.TempDir()
	start := time.Unix(1_700_000_000, 0)

	only := filepath.Join(dir, "only.mp4")
	writeSegment(t, only, uuid.New(), 1, start, 1, 211*90000)
	dur, video, err := Measure(only)
	if err != nil {
		t.Fatal(err)
	}
	if dur != 40*time.Millisecond || video.Duration != 0.04 {
		t.Errorf("single stretched sample: dur=%s video=%+v, want 40ms / 0.04", dur, video)
	}

	// 25 frames at 25 fps: a regular segment is measured as is.
	regular := filepath.Join(dir, "regular.mp4")
	writeSegment(t, regular, uuid.New(), 2, start, 25, 3600)
	dur, video, err = Measure(regular)
	if err != nil {
		t.Fatal(err)
	}
	if dur != time.Second || video.Start != 0 || video.Duration != 1 {
		t.Errorf("regular segment: dur=%s video=%+v, want 1s / 0+1", dur, video)
	}
}
