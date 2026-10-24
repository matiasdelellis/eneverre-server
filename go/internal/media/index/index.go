// Package index maintains a lightweight SQLite catalog of recording segments.
// It is fed incrementally as each segment is completed, so timeline/list/gap
// queries never need to walk the disk or open media files.
package index

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// Segment is one recorded segment.
type Segment struct {
	Fpath         string    `json:"fpath"`
	Path          string    `json:"path"`
	Start         time.Time `json:"start"`
	Duration      float64   `json:"duration"` // seconds
	SegmentNumber uint64    `json:"segmentNumber"`
	StreamID      string    `json:"streamId"`
	// Video is the span the video covers; nil when unknown (a row indexed
	// before it was recorded, until the engine's backfill reaches it).
	Video *VideoExtent `json:"video,omitempty"`
}

// VideoExtent is where the video lies within a segment. A camera can stop
// sending video while its audio keeps flowing, so the segment (which lasts as
// long as its longest track) may be covered by video only in part, or not at
// all (Duration 0).
type VideoExtent struct {
	Start    float64 `json:"start"`    // seconds from the segment start
	Duration float64 `json:"duration"` // seconds
}

// VideoPartial reports whether the segment's video leaves more than tolerance
// uncovered at its start or end, or there is no video at all. False when the
// extent is unknown.
func (s Segment) VideoPartial(tolerance time.Duration) bool {
	if s.Video == nil {
		return false
	}
	tol := tolerance.Seconds()
	return s.Video.Duration == 0 || s.Video.Start > tol ||
		s.Duration-(s.Video.Start+s.Video.Duration) > tol
}

// Gap is an interruption between two consecutive segments.
type Gap struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Duration float64   `json:"duration"` // seconds
}

// Timeline describes the recorded extent of a path.
type Timeline struct {
	Path  string    `json:"path"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Count int       `json:"count"`
}

// Index is the segment catalog.
type Index struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite index at the given file path, creating its
// parent directory if needed — otherwise SQLite fails with CANTOPEN (14) when
// the record dir does not exist yet (fresh install / first recording).
func Open(dbPath string) (*Index, error) {
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// WAL lets the recorder write while the API reads without blocking each
	// other; NORMAL sync is safe under WAL and much faster for the per-segment
	// inserts; busy_timeout makes concurrent writers wait for the lock instead
	// of failing with SQLITE_BUSY.
	//
	// The PRAGMAs MUST be passed in the DSN, not via db.Exec: database/sql
	// keeps a connection pool, and a PRAGMA run through db.Exec only affects
	// the single connection that happened to serve it. busy_timeout and
	// synchronous are per-connection, so any other pooled connection (opened
	// on demand when several cameras insert at once, plus the API's reads)
	// would lack the timeout and return SQLITE_BUSY immediately. Encoding them
	// in the DSN makes the driver apply them to every new connection.
	dsn := dbPath + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)"},
	}.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS segments (
			fpath       TEXT PRIMARY KEY,
			path        TEXT NOT NULL,
			start_ns    INTEGER NOT NULL,
			duration_ns INTEGER NOT NULL,
			seg_number  INTEGER NOT NULL,
			stream_id   TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_path_start ON segments(path, start_ns);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// Columns added after the first release. -1 marks a row indexed before
	// the video extent was recorded; the engine backfills it from the file.
	for _, stmt := range []string{
		`ALTER TABLE segments ADD COLUMN video_start_ns INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE segments ADD COLUMN video_duration_ns INTEGER NOT NULL DEFAULT -1`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	return &Index{db: db}, nil
}

// segmentColumns is the column list every segment SELECT scans, in
// scanSegment's order.
const segmentColumns = `fpath,path,start_ns,duration_ns,seg_number,stream_id,video_start_ns,video_duration_ns`

type rowScanner interface{ Scan(dest ...any) error }

func scanSegment(row rowScanner) (Segment, error) {
	var s Segment
	var startNs, durNs, vStartNs, vDurNs int64
	if err := row.Scan(&s.Fpath, &s.Path, &startNs, &durNs, &s.SegmentNumber, &s.StreamID, &vStartNs, &vDurNs); err != nil {
		return s, err
	}
	s.Start = time.Unix(0, startNs)
	s.Duration = float64(durNs) / 1e9
	if vDurNs >= 0 {
		s.Video = &VideoExtent{Start: float64(vStartNs) / 1e9, Duration: float64(vDurNs) / 1e9}
	}
	return s, nil
}

// videoColumns maps a segment's video extent to its two columns (-1 duration
// when unknown).
func videoColumns(s Segment) (startNs, durNs int64) {
	if s.Video == nil {
		return 0, -1
	}
	return int64(s.Video.Start * 1e9), int64(s.Video.Duration * 1e9)
}

// Close closes the index.
func (i *Index) Close() error { return i.db.Close() }

// Insert adds (or replaces) a segment.
func (i *Index) Insert(s Segment) error {
	vStart, vDur := videoColumns(s)
	_, err := i.db.Exec(
		`INSERT OR REPLACE INTO segments(`+segmentColumns+`)
		 VALUES(?,?,?,?,?,?,?,?)`,
		s.Fpath, s.Path, s.Start.UnixNano(), int64(s.Duration*1e9), s.SegmentNumber, s.StreamID,
		vStart, vDur,
	)
	return err
}

// Expired returns up to `limit` segments that ended at or before cutoff
// (oldest first). Use limit > 0 to bound the result set and the time
// the read transaction is held; the retention cleaner iterates with a
// batch size and re-queries until the result is empty. limit <= 0
// means no limit (returns every expired row in one shot — only safe
// when the corpus is small).
func (i *Index) Expired(cutoff time.Time, limit int) ([]Segment, error) {
	q := `SELECT ` + segmentColumns + ` FROM segments
	      WHERE (start_ns + duration_ns) <= ? ORDER BY start_ns ASC`
	args := []any{cutoff.UnixNano()}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return i.querySegments(q, args...)
}

// Oldest returns up to `limit` segments in start-time ascending order
// (oldest first), regardless of age. Used by the engine's emergency
// disk-space purge, which needs to drop the oldest footage first when
// free space crosses below the low-water mark — without depending on
// the configured retain window. Same limit semantics as Expired.
func (i *Index) Oldest(limit int) ([]Segment, error) {
	q := `SELECT ` + segmentColumns + ` FROM segments
	      ORDER BY start_ns ASC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return i.querySegments(q, args...)
}

// querySegments is the shared row-scan path for Expired / Oldest (and any
// other SELECT-by-start-time that gets added later). Returning a typed
// `[]Segment` keeps callers free of *sql.Rows.
func (i *Index) querySegments(q string, args ...any) ([]Segment, error) {
	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Segment
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteBatch removes multiple segment rows in a single transaction. One
// prepared statement, one commit, one fsync — instead of N round-trips
// with N fsyncs. Empty input is a no-op (returns nil). Rows whose file
// could not be removed (e.g. permission denied) are intentionally left
// out of the caller's list so they stay in the index for the next
// retention pass to retry.
func (i *Index) DeleteBatch(fpaths []string) error {
	if len(fpaths) == 0 {
		return nil
	}
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`DELETE FROM segments WHERE fpath=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, fp := range fpaths {
		if _, err := stmt.Exec(fp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Paths returns all distinct recorded paths.
func (i *Index) Paths() ([]string, error) {
	rows, err := i.db.Query(`SELECT DISTINCT path FROM segments ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// HasRecordings reports whether any segment is indexed for the given path
// (camera id). It uses EXISTS so SQLite stops at the first matching row,
// seeking straight into idx_path_start — cheap no matter how many segments a
// camera has accumulated.
func (i *Index) HasRecordings(path string) (bool, error) {
	var n int
	err := i.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM segments WHERE path=?)`, path).Scan(&n)
	return n != 0, err
}

// Timeline returns the recorded extent (first start .. last end) of a path.
// This is the cheap answer to "hasta cuándo hay grabación".
func (i *Index) Timeline(path string) (Timeline, error) {
	var minStart, maxEnd sql.NullInt64
	var count int
	err := i.db.QueryRow(
		`SELECT MIN(start_ns), MAX(start_ns+duration_ns), COUNT(*) FROM segments WHERE path=?`,
		path,
	).Scan(&minStart, &maxEnd, &count)
	if err != nil {
		return Timeline{}, err
	}
	t := Timeline{Path: path, Count: count}
	if minStart.Valid {
		t.Start = time.Unix(0, minStart.Int64)
		t.End = time.Unix(0, maxEnd.Int64)
	}
	return t, nil
}

// Range returns segments overlapping [from,to], ordered by start.
// A nil bound means unbounded on that side.
func (i *Index) Range(path string, from, to *time.Time) ([]Segment, error) {
	q := `SELECT ` + segmentColumns + ` FROM segments WHERE path=?`
	args := []any{path}
	if to != nil {
		q += ` AND start_ns < ?`
		args = append(args, to.UnixNano())
	}
	if from != nil {
		q += ` AND (start_ns + duration_ns) > ?`
		args = append(args, from.UnixNano())
	}
	q += ` ORDER BY start_ns ASC`

	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Segment
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Gaps returns interruptions in coverage within [from,to]. A gap is reported
// when the distance between one segment's end and the next segment's start
// exceeds tolerance.
func (i *Index) Gaps(path string, from, to *time.Time, tolerance time.Duration) ([]Gap, error) {
	segs, err := i.Range(path, from, to)
	if err != nil {
		return nil, err
	}
	var gaps []Gap
	for k := 1; k < len(segs); k++ {
		prevEnd := segs[k-1].Start.Add(time.Duration(segs[k-1].Duration * float64(time.Second)))
		curStart := segs[k].Start
		if curStart.Sub(prevEnd) > tolerance {
			gaps = append(gaps, Gap{
				Start:    prevEnd,
				End:      curStart,
				Duration: curStart.Sub(prevEnd).Seconds(),
			})
		}
	}
	return gaps, nil
}

// VideoUnmeasured returns up to limit segments whose video extent is unknown
// (rows indexed before it was recorded) and that come before `after` in
// (start, fpath) order, newest first: the footage most likely to be played
// back is backfilled first. Paging by that key lets the caller step past rows
// it could not measure, including rows that share a start time (two cameras
// can). A zero `after` starts from the newest.
func (i *Index) VideoUnmeasured(after Segment, limit int) ([]Segment, error) {
	if after.Start.IsZero() {
		return i.querySegments(`SELECT `+segmentColumns+` FROM segments
			WHERE video_duration_ns < 0 ORDER BY start_ns DESC, fpath DESC LIMIT ?`, limit)
	}
	return i.querySegments(`SELECT `+segmentColumns+` FROM segments
		WHERE video_duration_ns < 0 AND (start_ns < ? OR (start_ns = ? AND fpath < ?))
		ORDER BY start_ns DESC, fpath DESC LIMIT ?`,
		after.Start.UnixNano(), after.Start.UnixNano(), after.Fpath, limit)
}

// SetVideo records the measured video extent of several segments in one
// transaction. Each Segment needs Fpath and Video.
func (i *Index) SetVideo(segs []Segment) error {
	if len(segs) == 0 {
		return nil
	}
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE segments SET video_start_ns=?, video_duration_ns=? WHERE fpath=?`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer stmt.Close()
	for _, s := range segs {
		vStart, vDur := videoColumns(s)
		if _, err := stmt.Exec(vStart, vDur, s.Fpath); err != nil {
			tx.Rollback() //nolint:errcheck
			return err
		}
	}
	return tx.Commit()
}
