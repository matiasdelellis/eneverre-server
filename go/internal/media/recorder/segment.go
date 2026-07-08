package recorder

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"

	"eneverre/internal/media/recstore"
)

// segment represents one on-disk fMP4 segment, composed of one init (ftyp+moov)
// and one or more parts (moof+mdat). Segments are rotated on video keyframes.
type segment struct {
	r        *Recorder
	startDTS time.Duration
	startNTP time.Time
	number   uint64

	path           string
	fi             *os.File
	curPart        *part
	endDTS         time.Duration
	nextPartNumber uint32
}

func (s *segment) write(t *recTrack, smp *sample, dts time.Duration) error {
	endDTS := dts + timestampToDuration(int64(smp.Duration), int(t.clockRate))
	if endDTS > s.endDTS {
		s.endDTS = endDTS
	}

	if s.curPart == nil {
		s.curPart = newPart(s.startDTS, s.nextPartNumber, dts, s.r.MaxPartSize)
		s.nextPartNumber++
	} else if s.curPart.duration() >= s.r.PartDuration {
		if err := s.closeCurPart(); err != nil {
			s.curPart = nil
			return err
		}
		s.curPart = newPart(s.startDTS, s.nextPartNumber, dts, s.r.MaxPartSize)
		s.nextPartNumber++
	}

	return s.curPart.write(t, smp, dts)
}

func (s *segment) closeCurPart() error {
	if s.fi == nil {
		s.path = recstore.Path{Start: s.startNTP}.Encode(s.r.pathFmt)
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			return err
		}
		fi, err := os.Create(s.path)
		if err != nil {
			return err
		}
		if err = writeInit(fi, s.r.streamID, s.number, s.startDTS, s.startNTP, s.r.tracks); err != nil {
			fi.Close()
			return err
		}
		s.fi = fi
	}
	return s.curPart.close(s.fi)
}

func (s *segment) close() error {
	var err error
	if s.curPart != nil {
		err = s.closeCurPart()
	}
	if s.fi != nil {
		duration := s.endDTS - s.startDTS
		if e := writeDuration(s.fi, duration); err == nil {
			err = e
		}
		e := s.fi.Close()
		if err == nil {
			err = e
		}
		if e == nil && s.r.OnSegment != nil {
			s.r.OnSegment(SegmentInfo{
				Path:          s.path,
				Start:         s.startNTP,
				Duration:      duration,
				SegmentNumber: s.number,
				StreamID:      s.r.streamID.String(),
			})
		}
	}
	return err
}

// part is one fMP4 fragment (moof+mdat) within a segment.
type part struct {
	segmentStartDTS time.Duration
	number          uint32
	startDTS        time.Duration
	endDTS          time.Duration
	maxSize         uint64

	partTracks map[*recTrack]*fmp4.PartTrack
	size       uint64
}

func newPart(segmentStartDTS time.Duration, number uint32, startDTS time.Duration, maxSize uint64) *part {
	return &part{
		segmentStartDTS: segmentStartDTS,
		number:          number,
		startDTS:        startDTS,
		endDTS:          startDTS,
		maxSize:         maxSize,
		partTracks:      make(map[*recTrack]*fmp4.PartTrack),
	}
}

func (p *part) write(t *recTrack, smp *sample, dts time.Duration) error {
	size := uint64(len(smp.Payload))
	if p.maxSize > 0 && (p.size+size) > p.maxSize {
		return fmt.Errorf("reached maximum part size")
	}
	p.size += size

	pt, ok := p.partTracks[t]
	if !ok {
		// dts is guaranteed >= segmentStartDTS by the "received too late" guard
		// in recTrack.write (a sample older than the current segment is dropped
		// before it reaches here). Clamp anyway: a negative delta would wrap the
		// uint64 BaseTime into a garbage value, corrupting the whole part.
		baseDelta := int64(dts - p.segmentStartDTS)
		if baseDelta < 0 {
			baseDelta = 0
		}
		pt = &fmp4.PartTrack{
			ID:       t.initTrack.ID,
			BaseTime: uint64(multiplyAndDivide(baseDelta, int64(t.clockRate), int64(time.Second))),
		}
		p.partTracks[t] = pt
	}
	pt.Samples = append(pt.Samples, smp.Sample)

	endDTS := dts + timestampToDuration(int64(smp.Duration), int(t.clockRate))
	if endDTS > p.endDTS {
		p.endDTS = endDTS
	}
	return nil
}

func (p *part) close(w io.Writer) error {
	tracks := make([]*fmp4.PartTrack, 0, len(p.partTracks))
	for _, pt := range p.partTracks {
		tracks = append(tracks, pt)
	}
	fpart := &fmp4.Part{SequenceNumber: p.number, Tracks: tracks}

	var buf seekablebuffer.Buffer
	if err := fpart.Marshal(&buf); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func (p *part) duration() time.Duration { return p.endDTS - p.startDTS }
