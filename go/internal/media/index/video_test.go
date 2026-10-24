package index

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// An index created before the video extent existed gains the columns on Open,
// its rows read back as unknown, and the backfill fills them in.
func TestVideoExtentMigrationAndBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	old, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE segments (
			fpath TEXT PRIMARY KEY, path TEXT NOT NULL, start_ns INTEGER NOT NULL,
			duration_ns INTEGER NOT NULL, seg_number INTEGER NOT NULL, stream_id TEXT NOT NULL);
		INSERT INTO segments VALUES ('/r/old.mp4', 'cam', 1700000000000000000, 212000000000, 1, 's');
	`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	idx, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migration): %v", err)
	}
	defer idx.Close()

	base := time.Unix(1700000300, 0)
	if err := idx.Insert(Segment{
		Fpath: "/r/new.mp4", Path: "cam", Start: base, Duration: 60, StreamID: "s",
		Video: &VideoExtent{Start: 0, Duration: 2.5},
	}); err != nil {
		t.Fatal(err)
	}

	segs, err := idx.Range("cam", nil, nil)
	if err != nil || len(segs) != 2 {
		t.Fatalf("Range = %v, %v", segs, err)
	}
	if segs[0].Video != nil {
		t.Errorf("old row video = %+v, want unknown", segs[0].Video)
	}
	if v := segs[1].Video; v == nil || v.Duration != 2.5 {
		t.Errorf("new row video = %+v, want 2.5s", v)
	}
	if !segs[1].VideoPartial(2 * time.Second) {
		t.Error("2.5s of video in a 60s segment should be partial")
	}

	todo, err := idx.VideoUnmeasured(Segment{}, 10)
	if err != nil || len(todo) != 1 || todo[0].Fpath != "/r/old.mp4" {
		t.Fatalf("VideoUnmeasured = %v, %v", todo, err)
	}
	todo[0].Video = &VideoExtent{Start: 0, Duration: 1.16}
	if err := idx.SetVideo(todo); err != nil {
		t.Fatal(err)
	}
	if todo, _ := idx.VideoUnmeasured(Segment{}, 10); len(todo) != 0 {
		t.Errorf("still unmeasured after SetVideo: %v", todo)
	}

	// Re-opening an already-migrated index is a no-op.
	idx2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	idx2.Close()
}

// Paging the backfill one row at a time visits rows that share a start time
// (two cameras can) instead of skipping all but one.
func TestVideoUnmeasuredPagesTiedStarts(t *testing.T) {
	idx, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	start := time.Unix(1700000000, 0)
	for _, p := range []string{"/r/a.mp4", "/r/b.mp4", "/r/c.mp4"} {
		if err := idx.Insert(Segment{Fpath: p, Path: "cam", Start: start, Duration: 60, StreamID: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	var cursor Segment
	for {
		page, err := idx.VideoUnmeasured(cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		cursor = page[0]
		seen = append(seen, page[0].Fpath)
	}
	if len(seen) != 3 {
		t.Fatalf("paged %v, want all 3 rows", seen)
	}
}
