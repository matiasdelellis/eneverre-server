package camera

import (
	"database/sql"
	"fmt"
	"log/slog"

	"eneverre/internal/config"
)

// SeedFromINI performs the one-time INI → DB import. Cameras are DB-backed: the
// per-camera *.ini files under [server] cameras_dir are treated as initial
// configuration only. When the `cameras` table is empty (a fresh install, or an
// upgrade from the file-based layout) every INI is parsed and inserted; once
// any row exists the import is skipped and the INI files are ignored from then
// on. Cameras are thereafter created and deleted through the API.
//
// createdAt is the caller's wall-clock unix seconds, recorded on each imported
// row. Returns the number of cameras imported (0 when the table was already
// populated).
func SeedFromINI(db *sql.DB, cfg *config.Config, createdAt int64) (int, error) {
	st := NewStore(db)
	n, err := st.Count()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		slog.Info("cameras: DB already populated, skipping INI seed", "count", n)
		return 0, nil
	}

	specs := LoadSpecs(cfg)
	if len(specs) == 0 {
		slog.Info("cameras: no INI files to seed", "dir", cfg.CamerasDir)
		return 0, nil
	}

	// The id is derived from the name (loadSpec never reads one from the INI):
	// slug the name and give it a uniqueness suffix on collision ("patio",
	// "patio-2", …). seen tracks ids assigned so far; the table starts empty
	// (checked above), so this in-memory set is the whole population.
	seen := map[string]bool{}
	imported := 0
	for _, s := range specs {
		base := Slugify(s.Name)
		if base == "" {
			// loadSpec guarantees a name, but one of only punctuation slugs to "".
			slog.Warn("cameras: cannot derive an id from the name, skipping INI", "name", s.Name)
			continue
		}
		id := base
		for n := 2; seen[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		s.ID = id
		if _, err := st.Create(s, createdAt); err != nil {
			slog.Warn("cameras: skipping INI during seed", "id", s.ID, "err", err)
			continue
		}
		seen[s.ID] = true
		imported++
	}
	slog.Info("cameras: seeded from INI", "imported", imported, "dir", cfg.CamerasDir)
	return imported, nil
}
