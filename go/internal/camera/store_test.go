package camera

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"eneverre/internal/config"
	"eneverre/internal/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Init(db); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sampleSpec() Spec {
	return Spec{
		ID:       "frente",
		Name:     "Frente",
		Location: "Exterior",
		Source:   "rtsp://user:pass@10.0.0.1/ch0",
		Record:   true,
		MSE:      true,
		Relay:    true,
		Privacy:  true,
		Width:    1920,
		Height:   1080,
		HomeX:    -1, HomeY: -1, PrivacyX: -1, PrivacyY: -1,
	}
}

func TestStoreCreateGetDelete(t *testing.T) {
	st := NewStore(testDB(t))

	if n, err := st.Count(); err != nil || n != 0 {
		t.Fatalf("Count on empty = %d, %v; want 0, nil", n, err)
	}

	cam, err := st.Create(sampleSpec(), 1000)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cam.ID != "frente" || cam.Name != "Frente" {
		t.Fatalf("Create returned %+v", cam)
	}

	got, ok, err := st.Get("frente")
	if err != nil || !ok {
		t.Fatalf("Get = ok:%v err:%v", ok, err)
	}
	if got.Source != "rtsp://user:pass@10.0.0.1/ch0" || got.Width != 1920 {
		t.Errorf("Get returned wrong data: %+v", got)
	}

	if ex, _ := st.Exists("frente"); !ex {
		t.Error("Exists(frente) = false; want true")
	}
	if ex, _ := st.Exists("nope"); ex {
		t.Error("Exists(nope) = true; want false")
	}

	if err := st.Delete("frente"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := st.Get("frente"); ok {
		t.Error("Get after Delete returned ok=true")
	}
	if err := st.Delete("frente"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing = %v; want ErrNotFound", err)
	}
}

func TestStoreUpdate(t *testing.T) {
	st := NewStore(testDB(t))
	if _, err := st.Create(sampleSpec(), 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update every editable field; id stays the key.
	upd := sampleSpec()
	upd.Name = "Renamed"
	upd.Source = "rtsp://new:secret@10.0.0.9/ch1"
	upd.Transport = "tcp"
	upd.Record = false
	upd.PTZ = true
	upd.ThinginoURL = "http://10.0.0.9"
	upd.ThinginoAPIKey = "k2"
	upd.HomeX = 5
	if err := st.Update(upd); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, ok, err := st.GetSpec("frente")
	if err != nil || !ok {
		t.Fatalf("GetSpec = ok:%v err:%v", ok, err)
	}
	if got.Name != "Renamed" || got.Source != "rtsp://new:secret@10.0.0.9/ch1" ||
		got.Transport != "tcp" || got.Record != false || !got.PTZ ||
		got.ThinginoAPIKey != "k2" || got.HomeX != 5 {
		t.Errorf("Update did not persist: %+v", got)
	}

	// Updating a missing camera is ErrNotFound.
	miss := sampleSpec()
	miss.ID = "ghost"
	if err := st.Update(miss); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing = %v; want ErrNotFound", err)
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	st := NewStore(testDB(t))
	if _, err := st.Create(sampleSpec(), 1); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := st.Create(sampleSpec(), 2); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Create = %v; want ErrExists", err)
	}
}

// TestStoreListOrder verifies List preserves insertion order (via sort_order),
// not id order — a camera added later stays last regardless of its id.
func TestStoreListOrder(t *testing.T) {
	st := NewStore(testDB(t))
	first := sampleSpec()
	first.ID = "zebra"
	second := sampleSpec()
	second.ID = "alpha"
	if _, err := st.Create(first, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(second, 2); err != nil {
		t.Fatal(err)
	}
	cams, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 2 || cams[0].ID != "zebra" || cams[1].ID != "alpha" {
		t.Errorf("List order = %v; want [zebra alpha] (insertion order)", []string{cams[0].ID, cams[1].ID})
	}
}

// TestStoreCapabilitiesDerived checks the DB round-trip derives capabilities the
// same way the INI loader does: thumbnail from a thingino key, talk from a
// backchannel URL, PTZ from the thingino ptz flag.
func TestStoreSnapshotURL(t *testing.T) {
	st := NewStore(testDB(t))
	s := sampleSpec()
	s.ID = "snapcam"
	s.SnapshotURL = "http://user:pass@10.0.0.2/snapshot.jpg"
	if _, err := st.Create(s, 1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A snapshot_url (without any Thingino key) derives the thumbnail capability.
	got, ok, err := st.Get("snapcam")
	if err != nil || !ok {
		t.Fatalf("Get = ok:%v err:%v", ok, err)
	}
	if !got.Capabilities.Thumbnail {
		t.Error("snapshot_url should derive Thumbnail capability")
	}
	// The URL (which may carry credentials) round-trips for the engine/proxy but
	// stays out of the public JSON (json:"-").
	if got.SnapshotURL != "http://user:pass@10.0.0.2/snapshot.jpg" {
		t.Errorf("SnapshotURL lost on round-trip: %q", got.SnapshotURL)
	}

	// No snapshot_url and no Thingino key -> no thumbnail capability.
	plain := sampleSpec()
	plain.ID = "plaincam"
	if _, err := st.Create(plain, 2); err != nil {
		t.Fatalf("Create plain: %v", err)
	}
	p, _, _ := st.Get("plaincam")
	if p.Capabilities.Thumbnail {
		t.Error("camera without snapshot_url or Thingino key must not advertise thumbnail")
	}

	// A Thingino API key WITHOUT a thingino_url must not advertise thumbnail:
	// the handler's firmware path needs both, so a key alone would 404.
	keyOnly := sampleSpec()
	keyOnly.ID = "keyonly"
	keyOnly.ThinginoAPIKey = "k"
	if _, err := st.Create(keyOnly, 3); err != nil {
		t.Fatalf("Create keyonly: %v", err)
	}
	ko, _, _ := st.Get("keyonly")
	if ko.Capabilities.Thumbnail {
		t.Error("Thingino API key without thingino_url must not advertise thumbnail")
	}
}

func TestStoreCapabilitiesDerived(t *testing.T) {
	st := NewStore(testDB(t))
	s := sampleSpec()
	s.ID = "ptzcam"
	s.Backchannel = "rtsp://user:pass@10.0.0.1:554/ch0"
	s.ThinginoURL = "http://10.0.0.1"
	s.ThinginoAPIKey = "key123"
	s.PTZ = true
	s.HomeX, s.HomeY = 1065, 800
	if _, err := st.Create(s, 1); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.Get("ptzcam")
	if err != nil || !ok {
		t.Fatalf("Get = ok:%v err:%v", ok, err)
	}
	caps := got.Capabilities
	if !caps.PTZ || !caps.Thumbnail || !caps.Talk || !caps.Privacy {
		t.Errorf("capabilities = %+v; want PTZ+Thumbnail+Talk+Privacy all true", caps)
	}
	if got.HomeX != 1065 || got.HomeY != 800 {
		t.Errorf("home coords = %v,%v; want 1065,800", got.HomeX, got.HomeY)
	}
	// Private credential fields must survive the round-trip (they drive the
	// engine and thingino calls) even though they are json:"-" in responses.
	if got.ThinginoAPIKey != "key123" || got.Backchannel == "" {
		t.Errorf("credentials lost on round-trip: %+v", got)
	}
}

func TestSeedFromINI(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("frente.ini", "[camera]\nid = frente\nname = Frente\nsource = rtsp://x/frente\n")
	write("calle.ini", "[camera]\nid = calle\nname = Calle\nsource = rtsp://x/calle\ntransport = tcp\n")

	db := testDB(t)
	cfg := &config.Config{CamerasDir: dir}

	n, err := SeedFromINI(db, cfg, 1234)
	if err != nil {
		t.Fatalf("SeedFromINI: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d; want 2", n)
	}

	st := NewStore(db)
	cams, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 2 {
		t.Fatalf("List after seed = %d; want 2", len(cams))
	}
	// Sorted by filename during import: calle.ini before frente.ini.
	if cams[0].ID != "calle" || cams[1].ID != "frente" {
		t.Errorf("seed order = %v; want [calle frente]", []string{cams[0].ID, cams[1].ID})
	}

	// Re-seeding is a no-op once the table is populated.
	n2, err := SeedFromINI(db, cfg, 5678)
	if err != nil {
		t.Fatalf("second SeedFromINI: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second seed imported %d; want 0 (skip when populated)", n2)
	}
}
