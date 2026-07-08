package recorder

import (
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

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
// sample (so MSE /live/stream keeps working).
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

	// Live-only mode: skip segment creation, writing, and rotation.
	// r.currentSegment stays nil; the disconnect/Close handlers no-op on it.
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
