package index

import (
	"path/filepath"
	"testing"
	"time"
)

// TestOldestReturnsByStartTime covers the ordering and limit semantics of
// Oldest, the index helper the engine's emergency-purge goroutine uses to
// pick the segments to delete when free space drops below [media]
// min_free_bytes. Ordering is start_ns ASC (oldest first); limit > 0 caps
// the result, limit <= 0 returns everything.
func TestOldestReturnsByStartTime(t *testing.T) {
	dir := t.TempDir()
	idx, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer idx.Close()

	base := time.Unix(1700000000, 0).UTC()
	for i := 0; i < 10; i++ {
		if err := idx.Insert(Segment{
			Fpath:         filepath.Join(dir, "seg", timeSegmentName(i)),
			Path:          "cam",
			Start:         base.Add(time.Duration(i) * time.Minute),
			Duration:      60,
			SegmentNumber: uint64(i),
			StreamID:      "s",
		}); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	all, err := idx.Oldest(0)
	if err != nil {
		t.Fatalf("Oldest(0): %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("Oldest(0) = %d, want 10", len(all))
	}
	for i, s := range all {
		want := base.Add(time.Duration(i) * time.Minute)
		if !s.Start.Equal(want) {
			t.Errorf("Oldest[%d] start = %s, want %s", i, s.Start, want)
		}
	}

	limited, err := idx.Oldest(3)
	if err != nil {
		t.Fatalf("Oldest(3): %v", err)
	}
	if len(limited) != 3 {
		t.Fatalf("Oldest(3) = %d, want 3", len(limited))
	}
	for i, s := range limited {
		want := base.Add(time.Duration(i) * time.Minute)
		if !s.Start.Equal(want) {
			t.Errorf("Oldest(3)[%d] start = %s, want %s", i, s.Start, want)
		}
	}
}

func timeSegmentName(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
