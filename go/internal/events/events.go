// Package events records and queries motion events. Timestamps are stored as
// unix seconds and serialized as RFC3339 UTC strings on the wire.
package events

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"eneverre/internal/timeutil"
)

// recordMu serializes RecordMotion's read-merge-write sequence; see the
// comment there.
var recordMu sync.Mutex

// Event is a recorded motion (or other) event for a camera.
type Event struct {
	ID        int64
	CameraID  string
	StartTS   int64
	EndTS     int64
	Type      string
	Source    sql.NullString
	CreatedAt int64
}

// MarshalJSON renders timestamps as RFC3339 UTC strings.
func (e Event) MarshalJSON() ([]byte, error) {
	var src *string
	if e.Source.Valid {
		s := e.Source.String
		src = &s
	}
	return json.Marshal(struct {
		ID        int64   `json:"id"`
		CameraID  string  `json:"camera_id"`
		StartTS   string  `json:"start_ts"`
		EndTS     string  `json:"end_ts"`
		Type      string  `json:"type"`
		Source    *string `json:"source"`
		CreatedAt string  `json:"created_at"`
	}{e.ID, e.CameraID, toRFC3339(e.StartTS), toRFC3339(e.EndTS), e.Type, src, toRFC3339(e.CreatedAt)})
}

func toRFC3339(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ParseTimestamp accepts unix seconds (possibly as a numeric string) or an
// RFC3339 string, returning unix seconds. Naive timestamps are treated as UTC.
func ParseTimestamp(value string) (int64, bool) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, false
	}
	if isIntString(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	if t, ok := timeutil.ParseISO(s); ok {
		return t.Unix(), true
	}
	return 0, false
}

func isIntString(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const eventCols = "id, camera_id, start_ts, end_ts, type, source, created_at"

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.CameraID, &e.StartTS, &e.EndTS, &e.Type, &e.Source, &e.CreatedAt)
	return e, err
}

// RecordMotion records a motion event, extending any overlapping row to the
// union of the two ranges. If duration is non-nil and >= 0 the range is
// [ts, ts+duration]; otherwise [ts-pre, ts+post].
func RecordMotion(db *sql.DB, cameraID string, ts int64, pre, post int, duration *int64, eventType, source string) (Event, error) {
	// The SELECT-merge-UPDATE below is not atomic: two concurrent webhooks for
	// the same camera could both see "no overlap" and insert duplicate rows, or
	// overwrite each other's merge with a shorter range. This process is the
	// database's only writer, so an in-process mutex is enough to serialize it
	// (and far simpler than SQLite immediate-transaction plumbing). Events are
	// low-rate; the contention cost is negligible.
	recordMu.Lock()
	defer recordMu.Unlock()

	now := time.Now().Unix()

	var newStart, newEnd int64
	if duration != nil && *duration >= 0 {
		newStart = ts
		newEnd = ts + *duration
	} else {
		newStart = ts - int64(pre)
		newEnd = ts + int64(post)
	}
	if newEnd < newStart {
		newEnd = newStart
	}

	var id, exStart, exEnd int64
	err := db.QueryRow(
		"SELECT id, start_ts, end_ts FROM events "+
			"WHERE camera_id = ? AND end_ts >= ? AND start_ts <= ? "+
			"ORDER BY start_ts DESC LIMIT 1",
		cameraID, newStart, newEnd,
	).Scan(&id, &exStart, &exEnd)

	switch err {
	case nil:
		mergedStart := min(exStart, newStart)
		mergedEnd := max(exEnd, newEnd)
		if _, err := db.Exec(
			"UPDATE events SET start_ts = ?, end_ts = ? WHERE id = ?",
			mergedStart, mergedEnd, id,
		); err != nil {
			return Event{}, err
		}
		return getByID(db, id)
	case sql.ErrNoRows:
		res, err := db.Exec(
			"INSERT INTO events (camera_id, start_ts, end_ts, type, source, created_at) "+
				"VALUES (?, ?, ?, ?, ?, ?)",
			cameraID, newStart, newEnd, eventType, source, now,
		)
		if err != nil {
			return Event{}, err
		}
		lastID, _ := res.LastInsertId()
		return getByID(db, lastID)
	default:
		return Event{}, err
	}
}

func getByID(db *sql.DB, id int64) (Event, error) {
	return scanEvent(db.QueryRow(
		"SELECT "+eventCols+" FROM events WHERE id = ?", id,
	))
}

// List returns events for a camera (newest first) plus the total matching
// count. since/until are optional (nil to omit).
func List(db *sql.DB, cameraID string, since, until *int64, limit, offset int) ([]Event, int, error) {
	where := []string{"camera_id = ?"}
	args := []any{cameraID}
	if since != nil {
		where = append(where, "end_ts >= ?")
		args = append(args, *since)
	}
	if until != nil {
		where = append(where, "start_ts <= ?")
		args = append(args, *until)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM events WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.Query(
		"SELECT "+eventCols+" FROM events WHERE "+clause+
			" ORDER BY start_ts DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Get returns a single event by id scoped to a camera, or false if absent.
func Get(db *sql.DB, cameraID string, id int64) (Event, bool, error) {
	e, err := scanEvent(db.QueryRow(
		"SELECT "+eventCols+" FROM events WHERE id = ? AND camera_id = ?", id, cameraID,
	))
	if err == sql.ErrNoRows {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return e, true, nil
}

// Prune deletes every event whose window ended before cutoff (unix seconds),
// returning how many rows were removed. It is the retention sweep's primitive:
// recordings are pruned by age already, so without this the events table grows
// unbounded and accumulates rows pointing at footage that no longer exists.
// Events still within the window (end_ts >= cutoff) are kept.
func Prune(db *sql.DB, cutoff int64) (int64, error) {
	res, err := db.Exec("DELETE FROM events WHERE end_ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Delete removes an event by id scoped to a camera, reporting whether a row
// was deleted.
func Delete(db *sql.DB, cameraID string, id int64) (bool, error) {
	res, err := db.Exec("DELETE FROM events WHERE id = ? AND camera_id = ?", id, cameraID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
