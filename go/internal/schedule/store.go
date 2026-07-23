package schedule

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// ErrNotFound is returned when no schedule has the given id.
var ErrNotFound = errors.New("schedule not found")

// ErrExists is returned by Create when a schedule with the id already exists.
var ErrExists = errors.New("schedule already exists")

// Store is the DB-backed source of truth for recording schedules. It holds no
// in-memory state — each call hits the `schedules` table — so it is safe for
// concurrent use (the underlying *sql.DB is). The per-weekday windows are stored
// as a JSON object in the `rules` column.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store over the given database. The `schedules` table must
// already exist (store.Init creates it).
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// scanRow reads one (id, name, rules, created_at) row into a Schedule, decoding
// the rules JSON into Days. A malformed rules blob degrades to an empty map
// rather than failing the read.
func scanRow(scan func(dest ...any) error) (Schedule, error) {
	var s Schedule
	var rules string
	if err := scan(&s.ID, &s.Name, &rules, &s.CreatedAt); err != nil {
		return Schedule{}, err
	}
	s.Days = map[string][]string{}
	if rules != "" {
		if err := json.Unmarshal([]byte(rules), &s.Days); err != nil {
			s.Days = map[string][]string{}
		}
	}
	// A rules value of the JSON literal `null` (a hand-edited DB, say) unmarshals
	// the map back to nil — which would serialize as `"days": null` and hand a nil
	// map to Normalize. Keep it an empty map so the shape stays consistent.
	if s.Days == nil {
		s.Days = map[string][]string{}
	}
	return s, nil
}

// List returns every schedule, ordered by name then id (stable, deterministic).
func (st *Store) List() ([]Schedule, error) {
	rows, err := st.db.Query(`SELECT id, name, rules, created_at FROM schedules ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		s, err := scanRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns the schedule with the given id, or (Schedule{}, false).
func (st *Store) Get(id string) (Schedule, bool, error) {
	row := st.db.QueryRow(`SELECT id, name, rules, created_at FROM schedules WHERE id = ?`, id)
	s, err := scanRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, false, nil
	}
	if err != nil {
		return Schedule{}, false, err
	}
	return s, true, nil
}

// Create inserts a new schedule, returning ErrExists if the id is taken. The
// rules are normalized before storage. createdAt is the caller's unix seconds.
func (st *Store) Create(s Schedule, createdAt int64) (Schedule, error) {
	s.Days = Normalize(s.Days)
	rules, err := json.Marshal(s.Days)
	if err != nil {
		return Schedule{}, err
	}
	_, err = st.db.Exec(
		`INSERT INTO schedules (id, name, rules, created_at) VALUES (?, ?, ?, ?)`,
		s.ID, s.Name, string(rules), createdAt,
	)
	if err != nil {
		// modernc sqlite reports a UNIQUE/PK violation in the error string.
		if isUniqueViolation(err) {
			return Schedule{}, ErrExists
		}
		return Schedule{}, err
	}
	s.CreatedAt = createdAt
	return s, nil
}

// Update overwrites the name and rules of an existing schedule (identified by
// s.ID); the id and created_at are left unchanged. Returns ErrNotFound when no
// schedule has that id.
func (st *Store) Update(s Schedule) error {
	s.Days = Normalize(s.Days)
	rules, err := json.Marshal(s.Days)
	if err != nil {
		return err
	}
	res, err := st.db.Exec(
		`UPDATE schedules SET name = ?, rules = ? WHERE id = ?`,
		s.Name, string(rules), s.ID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes the schedule with the given id, returning ErrNotFound when it
// does not exist. Callers must ensure no camera still references it.
func (st *Store) Delete(id string) error {
	res, err := st.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE/PRIMARY KEY
// constraint failure. The pure-Go driver surfaces it in the message.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
