package playback

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"

	"eneverre/internal/media/mtxi"
)

const (
	sampleFlagIsNonSyncSample = 1 << 16
	concatenationTolerance    = 1 * time.Second
)

var errTerminated = errors.New("terminated")

type readSeekerAt interface {
	io.Reader
	io.Seeker
	io.ReaderAt
}

func durationGoToMp4(v time.Duration, timeScale uint32) int64 {
	timeScale64 := int64(timeScale)
	secs := v / time.Second
	dec := v % time.Second
	return int64(secs)*timeScale64 + int64(dec)*timeScale64/int64(time.Second)
}

func durationMp4ToGo(v int64, timeScale uint32) time.Duration {
	timeScale64 := int64(timeScale)
	secs := v / timeScale64
	dec := v % timeScale64
	return time.Duration(secs)*time.Second + time.Duration(dec)*time.Second/time.Duration(timeScale64)
}

func findInitTrack(tracks []*fmp4.InitTrack, id int) *fmp4.InitTrack {
	for _, track := range tracks {
		if track.ID == id {
			return track
		}
	}
	return nil
}

func tracksAreEqual(tracks1, tracks2 []*fmp4.InitTrack) bool {
	if len(tracks1) != len(tracks2) {
		return false
	}
	for i, t1 := range tracks1 {
		t2 := tracks2[i]
		if t1.ID != t2.ID || t1.TimeScale != t2.TimeScale ||
			reflect.TypeOf(t1.Codec) != reflect.TypeOf(t2.Codec) {
			return false
		}
	}
	return true
}

// segmentCanBeConcatenated decides whether two consecutive segments form a
// continuous timeline (via mtxi StreamID + SegmentNumber, or legacy tolerance).
func segmentCanBeConcatenated(prevInit *fmp4.Init, prevEnd time.Time, curInit *fmp4.Init, curStart time.Time) bool {
	m1 := mtxi.Find(prevInit.UserData)
	m2 := mtxi.Find(curInit.UserData)

	switch {
	case m1 == nil && m2 != nil:
		return false
	case m1 != nil && m2 == nil:
		return false
	case m1 == nil && m2 == nil: // legacy
		return tracksAreEqual(prevInit.Tracks, curInit.Tracks) &&
			!curStart.Before(prevEnd.Add(-concatenationTolerance)) &&
			!curStart.After(prevEnd.Add(concatenationTolerance))
	default:
		return bytes.Equal(m1.StreamID[:], m2.StreamID[:]) &&
			(m1.SegmentNumber+1) == m2.SegmentNumber
	}
}

// readHeader parses ftyp+moov and returns the init and duration (from mvhd).
func readHeader(r io.ReadSeeker) (*fmp4.Init, time.Duration, error) {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, 0, err
	}
	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return nil, 0, fmt.Errorf("ftyp box not found")
	}
	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	if _, err := r.Seek(int64(ftypSize), io.SeekStart); err != nil {
		return nil, 0, err
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, 0, err
	}
	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return nil, 0, fmt.Errorf("moov box not found")
	}
	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	if _, err := r.Seek(8, io.SeekCurrent); err != nil {
		return nil, 0, err
	}

	var mvhd amp4.Mvhd
	if _, err := amp4.Unmarshal(r, uint64(moovSize-8), &mvhd, amp4.Context{}); err != nil {
		return nil, 0, err
	}
	d := time.Duration(mvhd.DurationV0) * time.Second / time.Duration(mvhd.Timescale)

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	buf = make([]byte, uint64(ftypSize+moovSize))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, 0, err
	}

	var init fmp4.Init
	if err := init.Unmarshal(bytes.NewReader(buf)); err != nil {
		return nil, 0, err
	}
	return &init, d, nil
}

// muxParts reads the moof/mdat fragments of one segment and feeds samples to
// the muxer, shifting DTS by startDTS and stopping once duration is reached.
// Returns the actual duration muxed from this segment.
func muxParts(
	r readSeekerAt,
	startDTS time.Duration,
	duration time.Duration,
	tracks []*fmp4.InitTrack,
	m muxer,
) (time.Duration, error) {
	var startDTSMP4 int64
	var durationMP4 int64
	moofOffset := uint64(0)
	var tfhd *amp4.Tfhd
	var tfdt *amp4.Tfdt
	var timeScale uint32
	var segmentDuration time.Duration
	breakAtNextMdat := false

	_, err := amp4.ReadBoxStructure(r, func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof":
			moofOffset = h.BoxInfo.Offset
			return h.Expand()

		case "traf":
			return h.Expand()

		case "tfhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfhd = box.(*amp4.Tfhd)

		case "tfdt":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdt = box.(*amp4.Tfdt)

			track := findInitTrack(tracks, int(tfhd.TrackID))
			if track == nil {
				return nil, fmt.Errorf("invalid track ID: %v", tfhd.TrackID)
			}

			m.setTrack(int(tfhd.TrackID))
			timeScale = track.TimeScale
			startDTSMP4 = durationGoToMp4(startDTS, track.TimeScale)
			durationMP4 = durationGoToMp4(duration, track.TimeScale)

		case "trun":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*amp4.Trun)

			dataOffset := moofOffset + uint64(trun.DataOffset)
			dts := int64(tfdt.BaseMediaDecodeTimeV1) + startDTSMP4

			for _, e := range trun.Entries {
				if dts >= durationMP4 {
					breakAtNextMdat = true
					break
				}

				sampleOffset := dataOffset
				sampleSize := e.SampleSize

				err = m.writeSample(
					dts,
					e.SampleCompositionTimeOffsetV1,
					(e.SampleFlags&sampleFlagIsNonSyncSample) != 0,
					e.SampleSize,
					func() ([]byte, error) {
						payload := make([]byte, sampleSize)
						n, err2 := r.ReadAt(payload, int64(sampleOffset))
						if err2 != nil {
							return nil, err2
						}
						if n != int(sampleSize) {
							return nil, fmt.Errorf("partial read")
						}
						return payload, nil
					},
				)
				if err != nil {
					return nil, err
				}

				dataOffset += uint64(e.SampleSize)
				dts += int64(e.SampleDuration)
			}

			m.writeFinalDTS(dts)

			segmentElapsed := durationMp4ToGo(dts-startDTSMP4, timeScale)
			if segmentElapsed > segmentDuration {
				segmentDuration = segmentElapsed
			}

		case "mdat":
			if breakAtNextMdat {
				return nil, errTerminated
			}
		}
		return nil, nil
	})
	if err != nil && !errors.Is(err, errTerminated) {
		return 0, err
	}
	return segmentDuration, nil
}
