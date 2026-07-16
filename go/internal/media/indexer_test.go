package media

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"eneverre/internal/media/index"
)

// TestAsyncIndexerDrainsOnClose verifies the off-hot-path segment indexer: rows
// handed to enqueueIndex are inserted by the drainer goroutine, and Close drains
// every queued row before closing the index (so a clean shutdown never loses a
// segment that was written to disk but not yet indexed).
func TestAsyncIndexerDrainsOnClose(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.db")
	e, err := New(Options{
		RecordEnabled: true,
		RelayEnabled:  false, // don't bind :8554 in the test
		RecordDir:     dir,
		CacheDir:      filepath.Join(dir, "cache"),
		IndexPath:     idxPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := time.Unix(1700000000, 0).UTC()
	const n = 50
	for i := 0; i < n; i++ {
		e.enqueueIndex(index.Segment{
			Fpath:         fmt.Sprintf("/rec/cam/seg-%d.mp4", i),
			Path:          "cam",
			Start:         base.Add(time.Duration(i) * time.Minute),
			Duration:      60,
			SegmentNumber: uint64(i),
			StreamID:      "s",
		})
	}

	// Close must block until the drainer has flushed every queued insert.
	e.Close()

	idx, err := index.Open(idxPath)
	if err != nil {
		t.Fatalf("reopen index: %v", err)
	}
	defer idx.Close()
	from := base.Add(-time.Hour)
	to := base.Add(24 * time.Hour)
	segs, err := idx.Range("cam", &from, &to)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(segs) != n {
		t.Errorf("indexed %d segments after Close, want %d", len(segs), n)
	}
}
