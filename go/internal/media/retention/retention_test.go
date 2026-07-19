package retention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"eneverre/internal/media/index"
)

// seedSegments writes n placeholder segment files under dir and inserts a row
// for each into idx, oldest first. The files don't have to be playable — the
// purge only unlinks them and drops the row.
func seedSegments(t *testing.T, idx *index.Index, dir string, n int) []string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir segs: %v", err)
	}
	base := time.Unix(1700000000, 0).UTC()
	fpaths := make([]string, n)
	for i := 0; i < n; i++ {
		fp := filepath.Join(dir, fmt.Sprintf("seg-%04d.mp4", i))
		if err := os.WriteFile(fp, []byte("x"), 0o644); err != nil {
			t.Fatalf("write seg %d: %v", i, err)
		}
		fpaths[i] = fp
		if err := idx.Insert(index.Segment{
			Fpath:         fp,
			Path:          "cam",
			Start:         base.Add(time.Duration(i) * time.Minute),
			Duration:      60,
			SegmentNumber: uint64(i),
			StreamID:      "s",
		}); err != nil {
			t.Fatalf("index insert %d: %v", i, err)
		}
	}
	return fpaths
}

// TestPurgeToFreeDeletesOldest covers the full loop: an index seeded with
// enough segments to need multiple batches (600 > purgeBatchSize=500) and a
// fake statfs that reports "low" for the first two polls and "recovered" for
// the third. The purge should delete batch 1, delete batch 2, then exit on
// poll 3's target check with every segment gone and the index empty.
//
// This is the regression guard for the original failure mode: recording used
// to just hit ENOSPC and reconnect-spam. PurgeToFree proactively sacrifices
// the oldest footage so the rest of the system keeps running.
func TestPurgeToFreeDeletesOldest(t *testing.T) {
	dir := t.TempDir()
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	const n = 600
	fpaths := seedSegments(t, idx, filepath.Join(dir, "segs"), n)

	c := &Cleaner{Index: idx, RecordPath: filepath.Join(dir, "segs")}

	// Polls 1, 2 report low (force two batches); poll 3+ reports recovered
	// (4 MiB, above the 2 MiB target).
	const target = uint64(2 << 20)
	var calls int
	statfs := func(string) (uint64, error) {
		calls++
		if calls <= 2 {
			return 0, nil
		}
		return 4 << 20, nil
	}
	removed := c.PurgeToFree(context.Background(), dir, target, statfs)

	if removed != n {
		t.Errorf("removed = %d, want %d", removed, n)
	}
	if calls < 3 {
		t.Errorf("statfs called %d times, want >= 3 (two low + one recovered)", calls)
	}
	for i, fp := range fpaths {
		if _, err := os.Stat(fp); !os.IsNotExist(err) {
			t.Errorf("seg %d (%s) still on disk after purge: stat err = %v", i, fp, err)
		}
	}
	all, err := idx.Oldest(0)
	if err != nil {
		t.Fatalf("Oldest: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("indexed segments after purge = %d, want 0", len(all))
	}
}

// TestPurgeToFreeStopsWhenIndexEmpty covers the no-more-footage branch: statfs
// keeps reporting low but the index is empty, so the loop must exit instead of
// spinning forever. This is the "operator must free space by hand" path.
func TestPurgeToFreeStopsWhenIndexEmpty(t *testing.T) {
	dir := t.TempDir()
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	c := &Cleaner{Index: idx, RecordPath: dir}

	calls := 0
	statfs := func(string) (uint64, error) {
		calls++
		return 0, nil // always low
	}
	removed := c.PurgeToFree(context.Background(), dir, 1<<20, statfs)

	if removed != 0 {
		t.Errorf("removed = %d, want 0 (empty index)", removed)
	}
	// statfs (low), query (empty), log, return — exactly one poll.
	if calls != 1 {
		t.Errorf("statfs called %d times with empty index, want 1", calls)
	}
}

// TestPurgeToFreeStopsWhenAlreadyAboveTarget verifies the loop deletes nothing
// when the first poll already reports enough free space.
func TestPurgeToFreeStopsWhenAlreadyAboveTarget(t *testing.T) {
	dir := t.TempDir()
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	fpaths := seedSegments(t, idx, filepath.Join(dir, "segs"), 10)
	c := &Cleaner{Index: idx, RecordPath: filepath.Join(dir, "segs")}

	statfs := func(string) (uint64, error) { return 8 << 20, nil } // above target
	if removed := c.PurgeToFree(context.Background(), dir, 2<<20, statfs); removed != 0 {
		t.Errorf("removed = %d, want 0 (already above target)", removed)
	}
	for _, fp := range fpaths {
		if _, err := os.Stat(fp); err != nil {
			t.Errorf("seg %s deleted despite ample free space: %v", fp, err)
		}
	}
}
