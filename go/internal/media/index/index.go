// Package index maintains a lightweight SQLite catalog of recording segments.
// It is fed incrementally as each segment is completed, so timeline/list/gap
// queries never need to walk the disk or open media files.
package index

import (
	"database/sql"
	"net/url"
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

// Open opens (or creates) the SQLite index at the given file path.
func Open(dbPath string) (*Index, error) {
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
	return &Index{db: db}, nil
}

// Close closes the index.
func (i *Index) Close() error { return i.db.Close() }

// Insert adds (or replaces) a segment.
func (i *Index) Insert(s Segment) error {
	_, err := i.db.Exec(
		`INSERT OR REPLACE INTO segments(fpath,path,start_ns,duration_ns,seg_number,stream_id)
		 VALUES(?,?,?,?,?,?)`,
		s.Fpath, s.Path, s.Start.UnixNano(), int64(s.Duration*1e9), s.SegmentNumber, s.StreamID,
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
	q := `SELECT fpath,path,start_ns,duration_ns,seg_number,stream_id FROM segments
	      WHERE (start_ns + duration_ns) <= ? ORDER BY start_ns ASC`
	args := []any{cutoff.UnixNano()}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Segment
	for rows.Next() {
		var s Segment
		var startNs, durNs int64
		if err := rows.Scan(&s.Fpath, &s.Path, &startNs, &durNs, &s.SegmentNumber, &s.StreamID); err != nil {
			return nil, err
		}
		s.Start = time.Unix(0, startNs)
		s.Duration = float64(durNs) / 1e9
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
	q := `SELECT fpath,path,start_ns,duration_ns,seg_number,stream_id FROM segments WHERE path=?`
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
		var s Segment
		var startNs, durNs int64
		if err := rows.Scan(&s.Fpath, &s.Path, &startNs, &durNs, &s.SegmentNumber, &s.StreamID); err != nil {
			return nil, err
		}
		s.Start = time.Unix(0, startNs)
		s.Duration = float64(durNs) / 1e9
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
