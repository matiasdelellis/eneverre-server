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

	/**
	 * Duration of the last finalized sample, in the track's clock rate units.
	 * Used to give the last sample before a stall its usual duration instead of
	 * stretching it over the outage.
	 */
	lastDuration uint32

	/**
	 * Recording clock anchor: the first finalized sample's DTS and NTP. The
	 * difference between stream time and wall-clock is the drift that reveals a
	 * timestamp jump (the same detector MediaMTX uses).
	 */
	startInitialized bool
	startDTS         time.Duration
	startNTP         time.Time
}

/**
 * A jump larger than this between two samples of the same track means the
 * camera stalled (not a normal frame interval). The previous sample must not be
 * stretched over it: the segment is closed after it and the outage stays a gap
 * between segments.
 */
const maxSampleGap = 5 * time.Second

/**
 * Maximum distance between the stream's own clock (DTS) and wall-clock (NTP)
 * before the recording is considered out of sync. MediaMTX uses the same
 * tolerance: past it, sample durations no longer describe real time and the
 * segment is finalized and resynced instead of stretched.
 */
const ntpDriftTolerance = 5 * time.Second

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

	dts := timestampToDuration(smp.dts, int(t.clockRate))

	// Anchor the stream clock on the first sample and watch it against
	// wall-clock. A jump means the camera's timestamps no longer describe real
	// time (an RTP timestamp jump after packet loss), so the segment is
	// finalized and the next one starts on a keyframe instead of writing a
	// bogus multi-minute sample. Same detector and tolerance as MediaMTX.
	if !t.startInitialized {
		t.startDTS = dts
		t.startNTP = smp.ntp
		t.startInitialized = true
	} else {
		drift := smp.ntp.Sub(t.startNTP) - (dts - t.startDTS)
		if drift < -ntpDriftTolerance || drift > ntpDriftTolerance {
			r.Logf("track %d clock drifted %s from wall-clock, resyncing",
				t.initTrack.ID, drift.Round(time.Second))
			if r.currentSegment != nil {
				if err := r.currentSegment.close(); err != nil {
					r.Logf("segment close on drift: %v", err)
				}
				r.currentSegment = nil
			}
			r.resyncKeyframe = true
			// Re-anchor on the new time base so the following samples are not
			// seen as drifted again; the outage stays a gap between segments.
			t.startDTS = dts
			t.startNTP = smp.ntp
			return
		}
	}

	duration := t.nextSample.dts - smp.dts
	if duration < 0 {
		t.nextSample.dts = smp.dts
		duration = 0
	} else {
		// The sample's duration is the gap until the next one. If the NEXT
		// sample already drifted, that gap is not real time: zero it so this
		// segment is not inflated before the drift is handled on the next call.
		nextDTS := timestampToDuration(t.nextSample.dts, int(t.clockRate))
		nextDrift := t.nextSample.ntp.Sub(t.startNTP) - (nextDTS - t.startDTS)
		if nextDrift < -ntpDriftTolerance || nextDrift > ntpDriftTolerance {
			duration = 0
		}
	}

	// A gap much larger than a frame means the camera stalled. Do not stretch
	// the previous sample across it: give it its usual duration (or one frame)
	// and end the segment after it, so the outage stays a gap between segments.
	gap := duration > int64(t.clockRate)*int64(maxSampleGap/time.Second)
	if gap {
		if t.lastDuration > 0 {
			duration = int64(t.lastDuration)
		} else {
			duration = int64(t.clockRate) / 25
			if duration <= 0 {
				duration = 1
			}
		}
	}
	smp.Duration = uint32(duration)
	t.lastDuration = smp.Duration

	// feed the same finalized sample to the live web broadcaster, whether or
	// not it is persisted. Audio reaches it only once video started: the
	// broadcaster's stream begins on a video keyframe.
	if r.OnLiveSample != nil && (r.videoStarted || t.initTrack.Codec.IsVideo()) {
		r.OnLiveSample(t.initTrack.ID, smp.Sample, smp.dts)
	}

	// Live-only mode: skip segment creation, writing, and rotation.
	// r.currentSegment stays nil; the disconnect/Close handlers no-op on it.
	if !r.Record {
		return
	}

	if r.currentSegment == nil {
		// After a write error the segment was dropped mid-GOP. Starting the
		// next one on whatever sample arrives would make its first frames
		// reference a keyframe that lives in the previous file — undecodable
		// until the next IDR in playback/HLS. Wait for a video keyframe (and
		// drop audio until then, so the segment still starts on video).
		//
		// While the video is down there is no keyframe to wait for: the audio
		// opens the segment instead, and the video's return is handled below
		// (its first frame in the segment must be a keyframe).
		if r.resyncKeyframe && r.hasVideo {
			isVideo := t.initTrack.Codec.IsVideo()
			switch {
			case isVideo && !smp.IsNonSyncSample:
			case !isVideo && r.audioOnly():
			default:
				return
			}
			r.resyncKeyframe = false
		}
		r.currentSegment = r.newSegment(r.segmentStart(t, dts, smp.ntp))
	} else if (dts - r.currentSegment.startDTS) < 0 {
		r.Logf("sample of track %d received too late, discarding", t.initTrack.ID)
		return
	} else if t.initTrack.Codec.IsVideo() && !r.currentSegment.hasVideo {
		// The camera sent only audio so far in this segment (video stalled,
		// or the session started without it) and the video is back. Its first
		// frame must be a keyframe, or the segment's video would start
		// undecodable; and past a stall, the audio-only span stays its own
		// segment and the next one starts on this keyframe.
		if smp.IsNonSyncSample {
			return
		}
		if dts-r.currentSegment.startDTS > maxSampleGap {
			if err := r.currentSegment.close(); err != nil {
				r.Logf("segment close on video start: %v", err)
			}
			r.currentSegment = r.newSegment(r.segmentStart(t, dts, smp.ntp))
		}
	}

	if err := r.currentSegment.write(t, smp, dts); err != nil {
		r.Logf("segment write: %v", err)
		r.currentSegment.close() //nolint:errcheck
		r.currentSegment = nil
		r.resyncKeyframe = true
		return
	}

	// The sample before a stall was the last one of the segment: close it here
	// so the outage is a gap between segments, not a stretched duration. The
	// next segment starts on the first keyframe after the outage.
	if gap && (!r.hasVideo || t.initTrack.Codec.IsVideo()) {
		if err := r.currentSegment.close(); err != nil {
			r.Logf("segment close after gap: %v", err)
		}
		r.currentSegment = nil
		r.resyncKeyframe = true
		return
	}

	// rotate only on a video keyframe once the minimum duration elapsed. While
	// the video is down and only audio flows, the audio rotates instead, so an
	// outage of many minutes still yields segments of the usual length.
	nextDTS := timestampToDuration(t.nextSample.dts, int(t.clockRate))
	if (!r.hasVideo || t.initTrack.Codec.IsVideo() || r.currentSegment.videoStalled(dts)) &&
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

// segmentStart picks where a segment opened by a sample of track t (at dts,
// ntp) starts: the oldest of that sample and the other tracks' pending samples
// within maxBasetime of it. Those pending samples are written next, and one
// older than the segment start would be discarded as late — with composition
// offsets the video keyframe waiting in lookahead decodes before audio that
// arrived first,
// so an audio-opened segment would otherwise lose it and show no video until
// the next GOP.
func (r *Recorder) segmentStart(t *recTrack, dts time.Duration, ntp time.Time) (time.Duration, time.Time) {
	for _, o := range r.tracks {
		if o == t || o.nextSample == nil {
			continue
		}
		od := timestampToDuration(o.nextSample.dts, int(o.clockRate))
		if od < dts && dts-od <= maxBasetime {
			dts, ntp = od, o.nextSample.ntp
		}
	}
	return dts, ntp
}

func (r *Recorder) newSegment(startDTS time.Duration, startNTP time.Time) *segment {
	s := &segment{r: r, startDTS: startDTS, startNTP: startNTP, number: r.nextSegmentNumber, endDTS: startDTS}
	r.nextSegmentNumber++
	return s
}
