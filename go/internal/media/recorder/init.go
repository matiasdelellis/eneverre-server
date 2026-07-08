package recorder

import (
	"bytes"
	"fmt"
	"io"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/google/uuid"

	"eneverre/internal/media/mtxi"
)

// writeInit marshals the fMP4 init segment (ftyp + moov with mtxi box) and
// writes it to the file.
func writeInit(f io.Writer, streamID uuid.UUID, segNumber uint64, dts time.Duration, ntp time.Time, tracks []*recTrack) error {
	fmp4Tracks := make([]*fmp4.InitTrack, len(tracks))
	for i, t := range tracks {
		fmp4Tracks[i] = t.initTrack
	}

	init := fmp4.Init{
		Tracks: fmp4Tracks,
		UserData: []amp4.IBox{
			&mtxi.Box{
				FullBox:       amp4.FullBox{Version: 0},
				StreamID:      [16]byte(streamID),
				SegmentNumber: segNumber,
				DTS:           int64(dts),
				NTP:           ntp.UnixNano(),
			},
		},
	}
	var buf seekablebuffer.Buffer
	if err := init.Marshal(&buf); err != nil {
		return err
	}
	_, err := f.Write(buf.Bytes())
	return err
}

// writeDuration rewrites the total duration into mvhd (timescale 1000).
func writeDuration(f io.ReadWriteSeeker, d time.Duration) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	buf := make([]byte, 8)
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return fmt.Errorf("ftyp box not found")
	}
	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	if _, err := f.Seek(int64(ftypSize), io.SeekStart); err != nil {
		return err
	}
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return fmt.Errorf("moov box not found")
	}
	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	moovPos, err := f.Seek(8, io.SeekCurrent)
	if err != nil {
		return err
	}

	var mvhd amp4.Mvhd
	if _, err = amp4.Unmarshal(f, uint64(moovSize-8), &mvhd, amp4.Context{}); err != nil {
		return err
	}
	mvhd.DurationV0 = uint32(d / time.Millisecond)

	if _, err = f.Seek(moovPos, io.SeekStart); err != nil {
		return err
	}
	_, err = amp4.Marshal(f, &mvhd, amp4.Context{})
	return err
}
