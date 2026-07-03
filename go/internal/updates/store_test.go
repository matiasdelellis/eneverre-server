package updates

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ioNopCloser is a tiny shim so the test file does not need to import
// io alone for a single helper.
func ioNopCloser(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(r)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	s := NewStore(root, "tv")
	if err := s.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return s
}

func readManifest(t *testing.T, s *Store) Manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, manifestFilename))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func TestGetReturnsNotFoundOnEmptyStore(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPublish_SingleBuild(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("fake-apk-bytes")
	got, err := s.Publish(
		[]BuildInput{{ABI: "universal", Filename: "eneverre-tv-universal-1.0.0.apk", Reader: bytes.NewReader(payload)}},
		Manifest{VersionName: "1.0.0", VersionCode: 10000},
	)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.UploadedAt.IsZero() {
		t.Fatal("expected UploadedAt to be set")
	}
	if len(got.Builds) != 1 {
		t.Fatalf("expected 1 build, got %d", len(got.Builds))
	}
	b := got.Builds[0]
	if b.ABI != "universal" {
		t.Errorf("ABI: got %q want universal", b.ABI)
	}
	if b.Filename != "eneverre-tv-universal-1.0.0.apk" {
		t.Errorf("Filename: got %q", b.Filename)
	}
	if b.SHA256 != sha256Hex(payload) {
		t.Errorf("SHA256: got %s want %s", b.SHA256, sha256Hex(payload))
	}
	if b.Size != int64(len(payload)) {
		t.Errorf("Size: got %d want %d", b.Size, len(payload))
	}
	// File is on disk.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-universal-1.0.0.apk")); err != nil {
		t.Fatalf("APK not on disk: %v", err)
	}
	// Manifest roundtrips.
	persisted := readManifest(t, s)
	if persisted.VersionName != "1.0.0" || persisted.VersionCode != 10000 {
		t.Fatalf("manifest: %+v", persisted)
	}
	if len(persisted.Builds) != 1 {
		t.Fatalf("persisted builds: %+v", persisted.Builds)
	}
}

func TestPublish_MultiABI(t *testing.T) {
	s := newTestStore(t)
	parts := []BuildInput{
		{ABI: "arm64-v8a", Filename: "eneverre-tv-arm64-1.0.0.apk", Reader: bytes.NewReader([]byte("arm64-bytes"))},
		{ABI: "armeabi-v7a", Filename: "eneverre-tv-armv7-1.0.0.apk", Reader: bytes.NewReader([]byte("armv7-bytes"))},
		{ABI: "universal", Filename: "eneverre-tv-universal-1.0.0.apk", Reader: bytes.NewReader([]byte("universal-bytes"))},
	}
	got, err := s.Publish(parts, Manifest{VersionName: "1.0.0", VersionCode: 10000})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(got.Builds) != 3 {
		t.Fatalf("expected 3 builds, got %d", len(got.Builds))
	}
	wantABIs := []string{"arm64-v8a", "armeabi-v7a", "universal"}
	for i, b := range got.Builds {
		if b.ABI != wantABIs[i] {
			t.Errorf("build[%d].ABI: got %q want %q", i, b.ABI, wantABIs[i])
		}
	}
	// All three files on disk, all hashes correct.
	for _, part := range parts {
		ap, err := os.Stat(filepath.Join(s.dir, part.Filename))
		if err != nil {
			t.Errorf("%s missing: %v", part.Filename, err)
			continue
		}
		if ap.Size() != int64(len("")) {
			// check via SHA
		}
		// find build with this filename
		var found *APKBuild
		for i := range got.Builds {
			if got.Builds[i].Filename == part.Filename {
				found = &got.Builds[i]
				break
			}
		}
		if found == nil {
			t.Errorf("no build for %s", part.Filename)
			continue
		}
		// We need the original payload to check; reach into the input.
		_ = found
	}
	// Specific hash check
	want := map[string]string{
		"eneverre-tv-arm64-1.0.0.apk":     sha256Hex([]byte("arm64-bytes")),
		"eneverre-tv-armv7-1.0.0.apk":     sha256Hex([]byte("armv7-bytes")),
		"eneverre-tv-universal-1.0.0.apk": sha256Hex([]byte("universal-bytes")),
	}
	for _, b := range got.Builds {
		if w, ok := want[b.Filename]; ok && b.SHA256 != w {
			t.Errorf("%s: SHA256 got %s want %s", b.Filename, b.SHA256, w)
		}
	}
}

func TestPublish_OverwritesPreviousRelease(t *testing.T) {
	s := newTestStore(t)
	// First release.
	if _, err := s.Publish(
		[]BuildInput{{ABI: "arm64-v8a", Filename: "eneverre-tv-arm64-1.0.0.apk", Reader: bytes.NewReader([]byte("v1"))}},
		Manifest{VersionName: "1.0.0", VersionCode: 10000},
	); err != nil {
		t.Fatal(err)
	}
	// Second release: the previous APK is kept on disk (in-flight downloads),
	// but the manifest now lists the new one.
	if _, err := s.Publish(
		[]BuildInput{{ABI: "arm64-v8a", Filename: "eneverre-tv-arm64-1.0.1.apk", Reader: bytes.NewReader([]byte("v2-bigger"))}},
		Manifest{VersionName: "1.0.1", VersionCode: 10001, Mandatory: true},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.0.apk")); err != nil {
		t.Errorf("v1 APK should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.1.apk")); err != nil {
		t.Errorf("v2 APK missing: %v", err)
	}
	m, _ := s.Get()
	if m.VersionName != "1.0.1" || len(m.Builds) != 1 || m.Builds[0].Filename != "eneverre-tv-arm64-1.0.1.apk" {
		t.Fatalf("manifest after overwrite: %+v", m)
	}
	if !m.Mandatory {
		t.Error("Mandatory not preserved")
	}
}

func TestPublish_RollbackOnError(t *testing.T) {
	s := newTestStore(t)
	// A reader that errors mid-stream simulates a network blip after the
	// first APK was already written.
	errReader := &errAfterReader{after: 5, err: io.ErrUnexpectedEOF}
	parts := []BuildInput{
		{ABI: "arm64-v8a", Filename: "eneverre-tv-arm64.apk", Reader: bytes.NewReader([]byte("arm64-ok-bytes"))},
		{ABI: "armeabi-v7a", Filename: "eneverre-tv-armv7.apk", Reader: errReader},
	}
	_, err := s.Publish(parts, Manifest{VersionName: "1.0.0", VersionCode: 10000})
	if err == nil {
		t.Fatal("expected Publish to fail")
	}
	// The first build (which succeeded) must have been rolled back so the
	// dir does not retain a half-published set.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64.apk")); !os.IsNotExist(err) {
		t.Errorf("arm64 APK should have been rolled back, got: %v", err)
	}
	// And no manifest should have been written.
	if _, err := os.Stat(filepath.Join(s.dir, manifestFilename)); !os.IsNotExist(err) {
		t.Errorf("manifest should not exist, got: %v", err)
	}
}

func TestPublish_RejectsBadFilename(t *testing.T) {
	s := newTestStore(t)
	cases := []string{"", "noext", "foo/bar.apk", "../escape.apk", "foo.txt"}
	for _, in := range cases {
		_, err := s.Publish(
			[]BuildInput{{ABI: "universal", Filename: in, Reader: bytes.NewReader([]byte("x"))}},
			Manifest{},
		)
		if err == nil || !strings.Contains(err.Error(), "apk") {
			t.Errorf("Publish(%q): expected apk filename error, got %v", in, err)
		}
	}
}

func TestPublish_RejectsEmptyBuilds(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Publish(nil, Manifest{VersionName: "1.0.0", VersionCode: 10000})
	if err == nil {
		t.Fatal("expected error for empty builds")
	}
}

func TestSanitizeAPKFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"foo.apk", "foo.apk", false},
		{"eneverre-tv-arm64-v8a-1.0.1.apk", "eneverre-tv-arm64-v8a-1.0.1.apk", false},
		{"UPPER.APK", "UPPER.APK", false},
		{"", "", true},
		{"  ", "", true},
		{"foo.txt", "", true},
		{"foo", "", true},
		{"foo.apk.bak", "", true},
		{"../etc/passwd.apk", "", true},
		{"subdir/foo.apk", "", true},
		{"foo\\bar.apk", "", true},
		{"foo\x00.apk", "", true},
		{"..foo.apk", "", true},
		{"foo..apk", "", true},
	}
	for _, tc := range cases {
		got, err := SanitizeAPKFilename(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("Sanitize(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Sanitize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Sanitize(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadAPKRejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ReadAPK("../etc/passwd.apk"); err == nil {
		t.Fatal("expected traversal attempt to fail")
	}
	if _, err := s.ReadAPK("not-an-apk.txt"); err == nil {
		t.Fatal("expected non-apk filename to fail")
	}
}

func TestDisabledStoreReturnsNotFound(t *testing.T) {
	s := NewStore("", "tv")
	if s.Enabled() {
		t.Fatal("store with empty root should be disabled")
	}
	if _, err := s.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on disabled store: %v", err)
	}
	if _, err := s.ReadAPK("anything.apk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadAPK on disabled store: %v", err)
	}
	if _, err := s.Publish(nil, Manifest{}); err == nil {
		t.Fatal("Publish on disabled store should fail")
	}
}

// errAfterReader returns the first n bytes, then yields err.
type errAfterReader struct {
	after int
	n     int
	err   error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.n >= r.after {
		return 0, r.err
	}
	remaining := r.after - r.n
	if remaining > len(p) {
		remaining = len(p)
	}
	for i := 0; i < remaining; i++ {
		p[i] = byte(r.n + i)
	}
	r.n += remaining
	return remaining, nil
}

// --- multi-POST (incremental) tests ---

func TestActive_NoActiveOnFreshStore(t *testing.T) {
	s := newTestStore(t)
	ar, err := s.GetActive()
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if ar != nil {
		t.Fatalf("expected nil active on fresh store, got %+v", ar)
	}
}

func TestActive_StartAndAppend(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "2.0.0", VersionCode: 20000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	// Append two builds.
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", bytes.NewReader([]byte("arm64"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "universal", "eneverre-tv-universal-2.0.0.apk", bytes.NewReader([]byte("universal"))); err != nil {
		t.Fatal(err)
	}
	ar, err := s.GetActive()
	if err != nil {
		t.Fatal(err)
	}
	if ar == nil || len(ar.Builds) != 2 {
		t.Fatalf("expected 2 builds, got %+v", ar)
	}
	// On-disk: both APKs present.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-2.0.0.apk")); err != nil {
		t.Errorf("arm64 APK missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-universal-2.0.0.apk")); err != nil {
		t.Errorf("universal APK missing: %v", err)
	}
}

func TestActive_Commit(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "2.0.0", VersionCode: 20000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", bytes.NewReader([]byte("arm64"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "universal", "eneverre-tv-universal-2.0.0.apk", bytes.NewReader([]byte("universal"))); err != nil {
		t.Fatal(err)
	}
	committed, err := s.CommitActive()
	if err != nil {
		t.Fatalf("CommitActive: %v", err)
	}
	if len(committed.Builds) != 2 {
		t.Fatalf("expected 2 builds, got %d", len(committed.Builds))
	}
	// The active state is cleared.
	ar, _ := s.GetActive()
	if ar != nil {
		t.Errorf("active should be nil after commit, got %+v", ar)
	}
	// pending.json removed.
	if _, err := os.Stat(filepath.Join(s.dir, "pending.json")); !os.IsNotExist(err) {
		t.Errorf("pending.json should be gone, got: %v", err)
	}
	// manifest.json is the committed release.
	got, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.VersionCode != 20000 || len(got.Builds) != 2 {
		t.Fatalf("manifest: %+v", got)
	}
	// Both APKs still on disk and addressable.
	for _, b := range committed.Builds {
		if _, err := s.ReadAPK(b.Filename); err != nil {
			t.Errorf("APK %s not readable: %v", b.Filename, err)
		}
	}
}

func TestActive_AppendReplacesByABI(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	// First upload of arm64.
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	// Re-upload with the same ABI but a different filename and contents.
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-1.0.1.apk", bytes.NewReader([]byte("v2-bigger"))); err != nil {
		t.Fatal(err)
	}
	ar, _ := s.GetActive()
	if ar == nil || len(ar.Builds) != 1 {
		t.Fatalf("expected 1 build, got %+v", ar)
	}
	if ar.Builds[0].Filename != "eneverre-tv-arm64-1.0.1.apk" {
		t.Errorf("Filename: got %q", ar.Builds[0].Filename)
	}
	// The old APK was removed.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.0.apk")); !os.IsNotExist(err) {
		t.Errorf("old APK should be removed: %v", err)
	}
}

func TestActive_VersionCodeMismatch(t *testing.T) {
	s := newTestStore(t)
	v1 := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(v1, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	v2 := Manifest{VersionName: "2.0.0", VersionCode: 20000}
	// Append with the wrong versionCode: must error and NOT modify the active release.
	_, err := s.AppendBuild(v2, "arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", bytes.NewReader([]byte("v2")))
	if !errors.Is(err, ErrActiveVersionMismatch) {
		t.Fatalf("expected ErrActiveVersionMismatch, got %v", err)
	}
	ar, _ := s.GetActive()
	if ar.VersionCode != 10000 {
		t.Errorf("active versionCode changed: %d", ar.VersionCode)
	}
}

func TestActive_StartDiscardsPrevious(t *testing.T) {
	s := newTestStore(t)
	v1 := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(v1, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	v2 := Manifest{VersionName: "2.0.0", VersionCode: 20000}
	ar, err := s.StartActive(v2)
	if err != nil {
		t.Fatal(err)
	}
	if ar.VersionCode != 20000 || len(ar.Builds) != 0 {
		t.Errorf("fresh active: %+v", ar)
	}
	// The v1 APK is removed.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.0.apk")); !os.IsNotExist(err) {
		t.Errorf("v1 APK should be removed: %v", err)
	}
}

func TestActive_Discard(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64.apk", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := s.DiscardActive(); err != nil {
		t.Fatal(err)
	}
	ar, _ := s.GetActive()
	if ar != nil {
		t.Errorf("active should be nil after discard, got %+v", ar)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64.apk")); !os.IsNotExist(err) {
		t.Errorf("APK should be removed: %v", err)
	}
}

func TestActive_CommitWithoutActive(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CommitActive()
	if err == nil {
		t.Fatal("expected error when committing without an active release")
	}
}

func TestActive_PersistsAcrossReopen(t *testing.T) {
	root := t.TempDir()
	{
		s := NewStore(root, "tv")
		if err := s.Ensure(); err != nil {
			t.Fatal(err)
		}
		meta := Manifest{VersionName: "1.0.0", VersionCode: 10000}
		if _, err := s.StartActive(meta); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", bytes.NewReader([]byte("arm64"))); err != nil {
			t.Fatal(err)
		}
		// Not committed; the Store is dropped here.
	}
	{
		s := NewStore(root, "tv")
		if err := s.Ensure(); err != nil {
			t.Fatal(err)
		}
		ar, err := s.GetActive()
		if err != nil {
			t.Fatal(err)
		}
		if ar == nil || ar.VersionCode != 10000 || len(ar.Builds) != 1 {
			t.Fatalf("active not restored: %+v", ar)
		}
	}
}

// --- rotation tests ---

// publishCommit is a small helper that starts an active release with
// `parts`, commits it, and returns the committed manifest. Tests use it
// to chain multiple publishes and inspect what gets rotated.
func publishCommit(t *testing.T, s *Store, versionCode int, parts []BuildInput) *Manifest {
	t.Helper()
	if _, err := s.StartActive(Manifest{VersionName: "v", VersionCode: versionCode}); err != nil {
		t.Fatal(err)
	}
	for i, p := range parts {
		// Each part must have a unique filename so the rotation logic
		// can tell which APK belongs to which release.
		if p.Filename == "" {
			t.Fatalf("part %d: empty filename", i)
		}
		_, err := s.AppendBuild(Manifest{VersionName: "v", VersionCode: versionCode}, p.ABI, p.Filename, p.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.CommitActive()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRotation_DeletesPreviousReleaseAPKs(t *testing.T) {
	s := newTestStore(t)
	// v1
	publishCommit(t, s, 1, []BuildInput{
		{ABI: "arm64-v8a", Filename: "v1-arm64.apk", Reader: bytes.NewReader([]byte("v1-arm64"))},
		{ABI: "universal", Filename: "v1-univ.apk", Reader: bytes.NewReader([]byte("v1-univ"))},
	})
	// v2 — different filenames
	publishCommit(t, s, 2, []BuildInput{
		{ABI: "arm64-v8a", Filename: "v2-arm64.apk", Reader: bytes.NewReader([]byte("v2-arm64"))},
		{ABI: "universal", Filename: "v2-univ.apk", Reader: bytes.NewReader([]byte("v2-univ"))},
	})
	// After two publishes: v1 APKs are gone, v2 APKs remain.
	for _, fn := range []string{"v1-arm64.apk", "v1-univ.apk"} {
		if _, err := os.Stat(filepath.Join(s.dir, fn)); !os.IsNotExist(err) {
			t.Errorf("v1 APK %s should be deleted: %v", fn, err)
		}
	}
	for _, fn := range []string{"v2-arm64.apk", "v2-univ.apk"} {
		if _, err := os.Stat(filepath.Join(s.dir, fn)); err != nil {
			t.Errorf("v2 APK %s should remain: %v", fn, err)
		}
	}
	// No history is kept: there is no releases/ directory.
	if _, err := os.Stat(filepath.Join(s.dir, "releases")); !os.IsNotExist(err) {
		t.Errorf("releases/ should not exist: %v", err)
	}
}

func TestRotation_PreservesReusedFilenames(t *testing.T) {
	// Edge case: the CI sends the same APK filename for two consecutive
	// releases. The rotation should NOT delete the file (the new release
	// still references it): the v1 file has the same name as the v2
	// file, so the new release's keep set includes it and the rotation
	// leaves it alone.
	s := newTestStore(t)
	publishCommit(t, s, 1, []BuildInput{
		{ABI: "arm64-v8a", Filename: "app.apk", Reader: bytes.NewReader([]byte("content"))},
	})
	publishCommit(t, s, 2, []BuildInput{
		{ABI: "arm64-v8a", Filename: "app.apk", Reader: bytes.NewReader([]byte("content"))},
	})
	if _, err := os.Stat(filepath.Join(s.dir, "app.apk")); err != nil {
		t.Errorf("app.apk should remain (still in v2's keep set): %v", err)
	}
}

func TestRotation_PreservesNonAPKFiles(t *testing.T) {
	// The rotation must not touch manifest.json or pending.json — only
	// .apk files that are not in the keep set.
	s := newTestStore(t)
	publishCommit(t, s, 1, []BuildInput{
		{ABI: "arm64-v8a", Filename: "v1.apk", Reader: bytes.NewReader([]byte("v1"))},
	})
	// Create a stale file with a non-APK name; rotation should leave it
	// alone.
	if err := os.WriteFile(filepath.Join(s.dir, "stale.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	publishCommit(t, s, 2, []BuildInput{
		{ABI: "arm64-v8a", Filename: "v2.apk", Reader: bytes.NewReader([]byte("v2"))},
	})
	for _, fn := range []string{"manifest.json", "stale.txt"} {
		if _, err := os.Stat(filepath.Join(s.dir, fn)); err != nil {
			t.Errorf("%s should remain: %v", fn, err)
		}
	}
}
