package recovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/google/uuid"

	"eneverre/internal/media/index"
	"eneverre/internal/media/mtxi"
	"eneverre/internal/media/recstore"
)

// A minimal but valid H264 SPS/PPS (constrained baseline, tiny frame) so the
// fMP4 init marshals with a real video track — the recorder always writes video.
var (
	testSPS = []byte{0x67, 0x42, 0xc0, 0x0a, 0xf8, 0x41, 0xa2}
	testPPS = []byte{0x68, 0xce, 0x38, 0x80}
)

// writeSegment lays down an fMP4 segment exactly as the recorder does — ftyp+moov
// (with an mtxi box carrying start/stream/segment) followed by one moof+mdat part
// of `frames` video samples at `ticksPerFrame` (90 kHz) — but never patches the
// mvhd duration and never touches the index, i.e. what a hard crash leaves behind.
func writeSegment(t *testing.T, path string, streamID uuid.UUID, segNum uint64, start time.Time, frames int, ticksPerFrame uint32) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	track := &fmp4.InitTrack{
		ID:        1,
		TimeScale: 90000,
		Codec:     &mcodecs.H264{SPS: testSPS, PPS: testPPS},
	}
	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{track},
		UserData: []amp4.IBox{&mtxi.Box{
			FullBox:       amp4.FullBox{Version: 0},
			StreamID:      [16]byte(streamID),
			SegmentNumber: segNum,
			DTS:           0,
			NTP:           start.UnixNano(),
		}},
	}
	var initBuf seekBuf
	if err := init.Marshal(&initBuf); err != nil {
		t.Fatalf("init marshal: %v", err)
	}
	if _, err := f.Write(initBuf.Bytes()); err != nil {
		t.Fatal(err)
	}

	samples := make([]*fmp4.Sample, frames)
	for i := range samples {
		samples[i] = &fmp4.Sample{
			Duration:        ticksPerFrame,
			Payload:         []byte{0, 0, 0, 1, 0x65}, // arbitrary; probe reads sizes/durations only
			IsNonSyncSample: i != 0,
		}
	}
	part := fmp4.Part{
		SequenceNumber: 0,
		Tracks:         []*fmp4.PartTrack{{ID: 1, BaseTime: 0, Samples: samples}},
	}
	var partBuf seekBuf
	if err := part.Marshal(&partBuf); err != nil {
		t.Fatalf("part marshal: %v", err)
	}
	if _, err := f.Write(partBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

// seekBuf is the tiny seekable buffer fmp4 marshalling needs.
type seekBuf struct {
	buf []byte
	pos int64
}

func (b *seekBuf) Write(p []byte) (int, error) {
	end := b.pos + int64(len(p))
	if end > int64(len(b.buf)) {
		b.buf = append(b.buf, make([]byte, end-int64(len(b.buf)))...)
	}
	copy(b.buf[b.pos:end], p)
	b.pos = end
	return len(p), nil
}

func (b *seekBuf) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		b.pos = offset
	case 1:
		b.pos += offset
	case 2:
		b.pos = int64(len(b.buf)) + offset
	}
	return b.pos, nil
}

func (b *seekBuf) Bytes() []byte { return b.buf }

const testRecordPathTail = "%path/%Y-%m-%d/%H/%Y-%m-%d_%H-%M-%S-%f"

// A crashed segment on disk but absent from the index is re-indexed with the
// start/stream/segment from its mtxi box and the duration summed from its
// fragments — and the scan walks only forward from the watermark.
func TestRecoverOrphan(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, testRecordPathTail)
	fmtExt := recstore.PathAddExtension(recordPath)

	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	stream := uuid.New()
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)

	// Watermark: segment 0 is already indexed (the last clean rotation).
	wmStart := base
	wmPath := recstore.Path{Start: wmStart, Path: "cam1"}.Encode(fmtExt)
	writeSegment(t, wmPath, stream, 0, wmStart, 60, 90000/30) // 60 frames @30fps = 2s
	if err := idx.Insert(index.Segment{
		Fpath: wmPath, Path: "cam1", Start: wmStart, Duration: 2, SegmentNumber: 0, StreamID: stream.String(),
	}); err != nil {
		t.Fatal(err)
	}

	// Orphan: segment 1 was written right after but the crash skipped indexing.
	orphStart := base.Add(2 * time.Second)
	orphPath := recstore.Path{Start: orphStart, Path: "cam1"}.Encode(fmtExt)
	writeSegment(t, orphPath, stream, 1, orphStart, 45, 90000/30) // 45 frames @30fps = 1.5s

	n, total, err := Recover(idx, recordPath, "cam1", 60*time.Second, func(f string, a ...any) { t.Logf(f, a...) })
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d, want 1", n)
	}
	if total < 1400*time.Millisecond || total > 1600*time.Millisecond {
		t.Fatalf("total duration = %v, want ~1.5s", total)
	}

	// The orphan is now queryable with the right metadata.
	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	segs, err := idx.Range("cam1", &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("index has %d segments, want 2", len(segs))
	}
	got := segs[1]
	if got.Fpath != orphPath {
		t.Errorf("fpath = %q, want %q", got.Fpath, orphPath)
	}
	if got.SegmentNumber != 1 {
		t.Errorf("segNumber = %d, want 1", got.SegmentNumber)
	}
	if got.StreamID != stream.String() {
		t.Errorf("streamID = %q, want %q", got.StreamID, stream.String())
	}
	if !got.Start.Equal(orphStart) {
		t.Errorf("start = %v, want %v", got.Start.UTC(), orphStart)
	}
	if got.Duration < 1.4 || got.Duration > 1.6 {
		t.Errorf("duration = %v, want ~1.5", got.Duration)
	}
}

// A second run is a no-op: the recovered segment is now indexed, so it is not
// re-probed or duplicated.
func TestRecoverIdempotent(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, testRecordPathTail)
	fmtExt := recstore.PathAddExtension(recordPath)

	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	stream := uuid.New()
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	wmStart := base
	wmPath := recstore.Path{Start: wmStart, Path: "cam1"}.Encode(fmtExt)
	writeSegment(t, wmPath, stream, 0, wmStart, 60, 90000/30)
	if err := idx.Insert(index.Segment{Fpath: wmPath, Path: "cam1", Start: wmStart, Duration: 2, SegmentNumber: 0, StreamID: stream.String()}); err != nil {
		t.Fatal(err)
	}
	orphStart := base.Add(2 * time.Second)
	orphPath := recstore.Path{Start: orphStart, Path: "cam1"}.Encode(fmtExt)
	writeSegment(t, orphPath, stream, 1, orphStart, 30, 90000/30)

	if n, _, err := Recover(idx, recordPath, "cam1", 60*time.Second, nil); err != nil || n != 1 {
		t.Fatalf("first Recover: n=%d err=%v, want 1/nil", n, err)
	}
	if n, _, err := Recover(idx, recordPath, "cam1", 60*time.Second, nil); err != nil || n != 0 {
		t.Fatalf("second Recover: n=%d err=%v, want 0/nil", n, err)
	}
}
