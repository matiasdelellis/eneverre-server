package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/google/uuid"
)

// writeInit followed by writeDuration must produce a file whose moov/mvhd
// carries the duration in the 1000-timescale expected by players. Uses an LPCM
// track to avoid needing a valid H264 SPS for the init to marshal.
func TestWriteInitThenDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seg.mp4")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	tracks := []*recTrack{{
		clockRate: 8000,
		initTrack: &fmp4.InitTrack{
			ID:        1,
			TimeScale: 8000,
			Codec:     &mcodecs.LPCM{BitDepth: 16, SampleRate: 8000, ChannelCount: 1},
		},
	}}

	if err := writeInit(f, uuid.New(), 0, 0, time.Unix(1_700_000_000, 0), tracks); err != nil {
		f.Close()
		t.Fatalf("writeInit: %v", err)
	}
	if err := writeDuration(f, 1500*time.Millisecond); err != nil {
		f.Close()
		t.Fatalf("writeDuration: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	boxes, err := amp4.ExtractBoxWithPayload(rf, nil, amp4.BoxPath{amp4.BoxTypeMoov(), amp4.BoxTypeMvhd()})
	if err != nil {
		t.Fatalf("extract mvhd: %v", err)
	}
	if len(boxes) != 1 {
		t.Fatalf("found %d mvhd boxes, want 1", len(boxes))
	}
	mvhd, ok := boxes[0].Payload.(*amp4.Mvhd)
	if !ok {
		t.Fatalf("payload is %T, want *mp4.Mvhd", boxes[0].Payload)
	}
	// 1500ms at the mvhd timescale of 1000 = 1500 units.
	if mvhd.DurationV0 != 1500 {
		t.Fatalf("mvhd.DurationV0 = %d, want 1500", mvhd.DurationV0)
	}
}

// writeDuration on a file that is not an fMP4 segment must fail clearly rather
// than corrupting bytes.
func TestWriteDurationRejectsNonFmp4(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(path, []byte("not an mp4 file at all........"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := writeDuration(f, time.Second); err == nil {
		t.Fatal("expected error on non-fMP4 input, got nil")
	}
}
