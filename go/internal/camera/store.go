package camera

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned by Store.Delete when no camera has the given id.
var ErrNotFound = errors.New("camera not found")

// ErrExists is returned by Store.Create when a camera with the id already exists.
var ErrExists = errors.New("camera already exists")

// Store is the DB-backed source of truth for cameras. The per-camera INI files
// are only an initial seed (see SeedFromINI); once imported, every read and
// write goes through this store. It holds no in-memory state — each call hits
// the `cameras` table — so it is safe for concurrent use (the underlying
// *sql.DB is).
type Store struct {
	db *sql.DB
}

// NewStore returns a Store over the given database. The `cameras` table must
// already exist (store.Init creates it).
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// camColumns is the column list shared by List and Get so the scan order can
// never drift from the SELECT.
const camColumns = `id, name, comment, location, source, backchannel, snapshot_url, transport,
	record, mse, relay, privacy, playback, width, height,
	thingino_url, thingino_api_key, ptz, home_x, home_y, privacy_x, privacy_y,
	pan_steps, pan_degrees, tilt_steps, tilt_degrees, fov_h`

// scanSpec reads one row (selected with camColumns) into a Spec. Zero values
// in the PTZ calibration columns are filled with the default calibration so
// rows written before the columns existed (and rows that bypassed the
// loadSpec / spec() defaulting) still get a valid camera.PTZ block — the
// DB column DEFAULT only applies at INSERT time, not on a SELECT.
func scanSpec(scan func(dest ...any) error) (Spec, error) {
	var s Spec
	err := scan(
		&s.ID, &s.Name, &s.Comment, &s.Location, &s.Source, &s.Backchannel, &s.SnapshotURL, &s.Transport,
		&s.Record, &s.MSE, &s.Relay, &s.Privacy, &s.Playback, &s.Width, &s.Height,
		&s.ThinginoURL, &s.ThinginoAPIKey, &s.PTZ, &s.HomeX, &s.HomeY, &s.PrivacyX, &s.PrivacyY,
		&s.PanSteps, &s.PanDegrees, &s.TiltSteps, &s.TiltDegrees, &s.FOVH,
	)
	if s.PanSteps <= 0 {
		s.PanSteps = DefaultPanSteps
	}
	if s.PanDegrees <= 0 {
		s.PanDegrees = DefaultPanDegrees
	}
	if s.TiltSteps <= 0 {
		s.TiltSteps = DefaultTiltSteps
	}
	if s.TiltDegrees <= 0 {
		s.TiltDegrees = DefaultTiltDegrees
	}
	if s.FOVH <= 0 {
		s.FOVH = DefaultFOVH
	}
	return s, err
}

// List returns every camera as a public Camera model, ordered by sort_order
// then id (a stable, deterministic order matching the old alphabetical INI
// load). Runtime state (privacy, live URLs) is left at its zero value for the
// server to fill per request.
func (st *Store) List() ([]Camera, error) {
	specs, err := st.ListSpecs()
	if err != nil {
		return nil, err
	}
	cams := make([]Camera, len(specs))
	for i, s := range specs {
		cams[i] = s.Camera()
	}
	return cams, nil
}

// ListSpecs returns the raw persisted specs (used where the caller needs the
// config rather than the derived model, e.g. export/backup).
func (st *Store) ListSpecs() ([]Spec, error) {
	rows, err := st.db.Query(`SELECT ` + camColumns + ` FROM cameras ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var specs []Spec
	for rows.Next() {
		s, err := scanSpec(rows.Scan)
		if err != nil {
			return nil, err
		}
		specs = append(specs, s)
	}
	return specs, rows.Err()
}

// Get returns the single camera with the given id, or (Camera{}, false) when
// none exists.
func (st *Store) Get(id string) (Camera, bool, error) {
	row := st.db.QueryRow(`SELECT `+camColumns+` FROM cameras WHERE id = ?`, id)
	s, err := scanSpec(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Camera{}, false, nil
	}
	if err != nil {
		return Camera{}, false, err
	}
	return s.Camera(), true, nil
}

// Exists reports whether a camera with the id is stored.
func (st *Store) Exists(id string) (bool, error) {
	var one int
	err := st.db.QueryRow(`SELECT 1 FROM cameras WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Create inserts a new camera from its spec and returns the derived public
// model. It appends to the end of the display order (max sort_order + 1) and
// returns ErrExists if the id is already taken. createdAt is the caller's
// wall-clock unix seconds (the package takes no clock dependency).
func (st *Store) Create(s Spec, createdAt int64) (Camera, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return Camera{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	var exists int
	switch err := tx.QueryRow(`SELECT 1 FROM cameras WHERE id = ?`, s.ID).Scan(&exists); {
	case err == nil:
		return Camera{}, ErrExists
	case !errors.Is(err, sql.ErrNoRows):
		return Camera{}, err
	}

	var maxOrder sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(sort_order) FROM cameras`).Scan(&maxOrder); err != nil {
		return Camera{}, err
	}
	sortOrder := int64(0)
	if maxOrder.Valid {
		sortOrder = maxOrder.Int64 + 1
	}

	if _, err := tx.Exec(
		`INSERT INTO cameras (
			id, name, comment, location, source, backchannel, snapshot_url, transport,
			record, mse, relay, privacy, playback, width, height,
			thingino_url, thingino_api_key, ptz, home_x, home_y, privacy_x, privacy_y,
			pan_steps, pan_degrees, tilt_steps, tilt_degrees, fov_h,
			sort_order, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.Comment, s.Location, s.Source, s.Backchannel, s.SnapshotURL, s.Transport,
		s.Record, s.MSE, s.Relay, s.Privacy, s.Playback, s.Width, s.Height,
		s.ThinginoURL, s.ThinginoAPIKey, s.PTZ, s.HomeX, s.HomeY, s.PrivacyX, s.PrivacyY,
		s.PanSteps, s.PanDegrees, s.TiltSteps, s.TiltDegrees, s.FOVH,
		sortOrder, createdAt,
	); err != nil {
		return Camera{}, err
	}
	if err := tx.Commit(); err != nil {
		return Camera{}, err
	}
	return s.Camera(), nil
}

// GetSpec returns the raw persisted spec (config, including credentials) for the
// given id, or (Spec{}, false) when none exists. Used by the admin edit endpoint
// to prefill the form; the public Camera model (from Get) never carries the
// credential fields.
func (st *Store) GetSpec(id string) (Spec, bool, error) {
	row := st.db.QueryRow(`SELECT `+camColumns+` FROM cameras WHERE id = ?`, id)
	s, err := scanSpec(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Spec{}, false, nil
	}
	if err != nil {
		return Spec{}, false, err
	}
	return s, true, nil
}

// Update overwrites every editable column of an existing camera (identified by
// s.ID) from the spec; the id, sort_order and created_at are left unchanged.
// Returns ErrNotFound when no camera has that id.
func (st *Store) Update(s Spec) error {
	res, err := st.db.Exec(
		`UPDATE cameras SET
			name = ?, comment = ?, location = ?, source = ?, backchannel = ?, snapshot_url = ?, transport = ?,
			record = ?, mse = ?, relay = ?, privacy = ?, playback = ?, width = ?, height = ?,
			thingino_url = ?, thingino_api_key = ?, ptz = ?, home_x = ?, home_y = ?, privacy_x = ?, privacy_y = ?,
			pan_steps = ?, pan_degrees = ?, tilt_steps = ?, tilt_degrees = ?, fov_h = ?
		 WHERE id = ?`,
		s.Name, s.Comment, s.Location, s.Source, s.Backchannel, s.SnapshotURL, s.Transport,
		s.Record, s.MSE, s.Relay, s.Privacy, s.Playback, s.Width, s.Height,
		s.ThinginoURL, s.ThinginoAPIKey, s.PTZ, s.HomeX, s.HomeY, s.PrivacyX, s.PrivacyY,
		s.PanSteps, s.PanDegrees, s.TiltSteps, s.TiltDegrees, s.FOVH,
		s.ID,
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

// Delete removes the camera with the given id, returning ErrNotFound when it
// does not exist. Recorded segments on disk are not touched — retention prunes
// them on its own schedule.
func (st *Store) Delete(id string) error {
	res, err := st.db.Exec(`DELETE FROM cameras WHERE id = ?`, id)
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

// Count returns the number of stored cameras. Used by the seed to decide
// whether the INI import should run.
func (st *Store) Count() (int, error) {
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM cameras`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count cameras: %w", err)
	}
	return n, nil
}
