package playback

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
)

// errNoSamples is returned when a request produced no playable samples.
var errNoSamples = errors.New("no samples found in the requested range")

const partDuration = 1 * time.Second

type muxerFMP4Track struct {
	id        int
	timeScale uint32
	firstDTS  int64
	lastDTS   int64
	samples   []*fmp4.Sample
}

func findTrack(tracks []*muxerFMP4Track, id int) *muxerFMP4Track {
	for _, track := range tracks {
		if track.id == id {
			return track
		}
	}
	return nil
}

// muxerFMP4 streams a fragmented-MP4 (init + parts) suitable for MSE.
type muxerFMP4 struct {
	w io.Writer

	// useAVC3 emits the video sample entry as avc3 (parameter sets carried
	// in-band per keyframe) instead of avc1. Needed when gap-fill splices a
	// black frame with a different SPS: the decoder then reads SPS/PPS from the
	// bitstream and switches at the gap boundary. Recordings already carry
	// in-band SPS in keyframes, so they decode fine either way.
	useAVC3 bool

	init               *fmp4.Init
	nextSequenceNumber uint32
	tracks             []*muxerFMP4Track
	curTrack           *muxerFMP4Track
	outBuf             seekablebuffer.Buffer
}

// patchAVC1toAVC3 flips the H264 visual sample entry FourCC from "avc1" to
// "avc3" within the moov box (in place), so the decoder honors in-band SPS/PPS.
//
// It scans for the literal "avc1" only from the moov offset onward (the init we
// just marshalled), not the whole file, and our init carries no user-data boxes
// with free-form text — the only "avc1" bytes present are the video sample-entry
// FourCC(s). If that ever changes (e.g. a udta with an "avc1" string), parse the
// stsd sample entry instead of a byte scan.
func patchAVC1toAVC3(b []byte) {
	moov := bytes.Index(b, []byte("moov"))
	if moov < 0 {
		return
	}
	region := b[moov:]
	for {
		i := bytes.Index(region, []byte("avc1"))
		if i < 0 {
			break
		}
		region[i+3] = '3'
		region = region[i+4:]
	}
}

func (w *muxerFMP4) writeInit(init *fmp4.Init) {
	w.init = init
	w.tracks = make([]*muxerFMP4Track, len(init.Tracks))
	for i, track := range init.Tracks {
		w.tracks[i] = &muxerFMP4Track{
			id:        track.ID,
			timeScale: track.TimeScale,
			firstDTS:  -1,
		}
	}
}

func (w *muxerFMP4) setTrack(trackID int) {
	w.curTrack = findTrack(w.tracks, trackID)
}

func (w *muxerFMP4) writeSample(
	dts int64,
	ptsOffset int32,
	isNonSyncSample bool,
	_ uint32,
	getPayload func() ([]byte, error),
) error {
	pl, err := getPayload()
	if err != nil {
		return err
	}

	if dts >= 0 {
		if w.curTrack.firstDTS < 0 {
			w.curTrack.firstDTS = dts
			if !isNonSyncSample {
				w.curTrack.samples = w.curTrack.samples[:0]
			}
		} else {
			duration := max(dts-w.curTrack.lastDTS, 0)
			w.curTrack.samples[len(w.curTrack.samples)-1].Duration = uint32(duration)
		}

		w.curTrack.samples = append(w.curTrack.samples, &fmp4.Sample{
			PTSOffset:       ptsOffset,
			IsNonSyncSample: isNonSyncSample,
			Payload:         pl,
		})
		w.curTrack.lastDTS = dts

		partDurationMP4 := durationGoToMp4(partDuration, w.curTrack.timeScale)
		if (w.curTrack.lastDTS - w.curTrack.firstDTS) >= partDurationMP4 {
			if err = w.innerFlush(false); err != nil {
				return err
			}
		}
	} else {
		if !isNonSyncSample {
			w.curTrack.samples = w.curTrack.samples[:0]
			w.curTrack.samples = append(w.curTrack.samples, &fmp4.Sample{
				IsNonSyncSample: isNonSyncSample,
				Payload:         pl,
				PTSOffset:       ptsOffset,
			})
		} else {
			w.curTrack.samples = append(w.curTrack.samples, &fmp4.Sample{
				IsNonSyncSample: isNonSyncSample,
				Payload:         pl,
				PTSOffset:       ptsOffset,
			})
		}
	}

	return nil
}

func (w *muxerFMP4) writeFinalDTS(dts int64) {
	if len(w.curTrack.samples) != 0 && w.curTrack.firstDTS >= 0 {
		duration := max(dts-w.curTrack.lastDTS, 0)
		w.curTrack.samples[len(w.curTrack.samples)-1].Duration = uint32(duration)
	}
}

func (w *muxerFMP4) innerFlush(final bool) error {
	var part fmp4.Part

	for _, track := range w.tracks {
		if track.firstDTS >= 0 && (len(track.samples) > 1 || (final && len(track.samples) != 0)) {
			var samples []*fmp4.Sample
			if !final {
				samples = track.samples[:len(track.samples)-1]
			} else {
				samples = track.samples
			}

			part.Tracks = append(part.Tracks, &fmp4.PartTrack{
				ID:       track.id,
				BaseTime: uint64(track.firstDTS),
				Samples:  samples,
			})

			if !final {
				track.samples = track.samples[len(track.samples)-1:]
				track.firstDTS = track.lastDTS
			}
		}
	}

	if part.Tracks == nil {
		if w.init != nil {
			return errNoSamples
		}
		return nil
	}

	part.SequenceNumber = w.nextSequenceNumber
	w.nextSequenceNumber++

	if w.init != nil {
		if err := w.init.Marshal(&w.outBuf); err != nil {
			return err
		}
		if w.useAVC3 {
			patchAVC1toAVC3(w.outBuf.Bytes())
		}
		if _, err := w.w.Write(w.outBuf.Bytes()); err != nil {
			return err
		}
		w.init = nil
		w.outBuf.Reset()
	}

	if err := part.Marshal(&w.outBuf); err != nil {
		return err
	}
	if _, err := w.w.Write(w.outBuf.Bytes()); err != nil {
		return err
	}
	w.outBuf.Reset()
	return nil
}

func (w *muxerFMP4) flush() error {
	return w.innerFlush(true)
}
