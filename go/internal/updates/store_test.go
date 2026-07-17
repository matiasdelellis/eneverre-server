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
		[]BuildInput{{Variant: "universal", Filename: "eneverre-tv-universal-1.0.0.apk", Reader: bytes.NewReader(payload)}},
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
	if b.Variant != "universal" {
		t.Errorf("Variant: got %q want universal", b.Variant)
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
		t.Fatalf("build file not on disk: %v", err)
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

func TestPublish_MultiVariant(t *testing.T) {
	s := newTestStore(t)
	parts := []BuildInput{
		{Variant: "arm64-v8a", Filename: "eneverre-tv-arm64-1.0.0.apk", Reader: bytes.NewReader([]byte("arm64-bytes"))},
		{Variant: "armeabi-v7a", Filename: "eneverre-tv-armv7-1.0.0.apk", Reader: bytes.NewReader([]byte("armv7-bytes"))},
		{Variant: "universal", Filename: "eneverre-tv-universal-1.0.0.apk", Reader: bytes.NewReader([]byte("universal-bytes"))},
	}
	got, err := s.Publish(parts, Manifest{VersionName: "1.0.0", VersionCode: 10000})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(got.Builds) != 3 {
		t.Fatalf("expected 3 builds, got %d", len(got.Builds))
	}
	wantVariants := []string{"arm64-v8a", "armeabi-v7a", "universal"}
	for i, b := range got.Builds {
		if b.Variant != wantVariants[i] {
			t.Errorf("build[%d].Variant: got %q want %q", i, b.Variant, wantVariants[i])
		}
	}
	// All three files on disk, all hashes correct.
	for _, part := range parts {
		if _, err := os.Stat(filepath.Join(s.dir, part.Filename)); err != nil {
			t.Errorf("%s missing: %v", part.Filename, err)
		}
	}
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
		[]BuildInput{{Variant: "arm64-v8a", Filename: "eneverre-tv-arm64-1.0.0.apk", Reader: bytes.NewReader([]byte("v1"))}},
		Manifest{VersionName: "1.0.0", VersionCode: 10000},
	); err != nil {
		t.Fatal(err)
	}
	// Second release: the previous build file is kept on disk (in-flight
	// downloads), but the manifest now lists the new one.
	if _, err := s.Publish(
		[]BuildInput{{Variant: "arm64-v8a", Filename: "eneverre-tv-arm64-1.0.1.apk", Reader: bytes.NewReader([]byte("v2-bigger"))}},
		Manifest{VersionName: "1.0.1", VersionCode: 10001, Mandatory: true},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.0.apk")); err != nil {
		t.Errorf("v1 build file should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.1.apk")); err != nil {
		t.Errorf("v2 build file missing: %v", err)
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
	// first build file was already written.
	errReader := &errAfterReader{after: 5, err: io.ErrUnexpectedEOF}
	parts := []BuildInput{
		{Variant: "arm64-v8a", Filename: "eneverre-tv-arm64.apk", Reader: bytes.NewReader([]byte("arm64-ok-bytes"))},
		{Variant: "armeabi-v7a", Filename: "eneverre-tv-armv7.apk", Reader: errReader},
	}
	_, err := s.Publish(parts, Manifest{VersionName: "1.0.0", VersionCode: 10000})
	if err == nil {
		t.Fatal("expected Publish to fail")
	}
	// The first build (which succeeded) must have been rolled back so the
	// dir does not retain a half-published set.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64.apk")); !os.IsNotExist(err) {
		t.Errorf("arm64 build file should have been rolled back, got: %v", err)
	}
	// And no manifest should have been written.
	if _, err := os.Stat(filepath.Join(s.dir, manifestFilename)); !os.IsNotExist(err) {
		t.Errorf("manifest should not exist, got: %v", err)
	}
}

func TestPublish_RejectsBadFilename(t *testing.T) {
	s := newTestStore(t)
	cases := []string{"", "../escape.apk", "foo/bar.apk", "manifest.json", "pending.json", "upload.tmp"}
	for _, in := range cases {
		_, err := s.Publish(
			[]BuildInput{{Variant: "universal", Filename: in, Reader: bytes.NewReader([]byte("x"))}},
			Manifest{},
		)
		if err == nil {
			t.Errorf("Publish(%q): expected an error, got none", in)
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

func TestSanitizeBuildFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"foo.apk", "foo.apk", false},
		{"eneverre-tv-arm64-v8a-1.0.1.apk", "eneverre-tv-arm64-v8a-1.0.1.apk", false},
		{"UPPER.APK", "UPPER.APK", false},
		// Any extension (or none) is legal now — builds aren't assumed to
		// be APKs.
		{"foo.txt", "foo.txt", false},
		{"foo", "foo", false},
		{"foo.apk.bak", "foo.apk.bak", false},
		{"foo.ipa", "foo.ipa", false},
		{"", "", true},
		{"  ", "", true},
		{"../etc/passwd.apk", "", true},
		{"subdir/foo.apk", "", true},
		{"foo\\bar.apk", "", true},
		{"foo\x00.apk", "", true},
		{"..foo.apk", "", true},
		{"foo..apk", "", true},
		// Reserved sidecar / in-progress-write names must never be usable
		// as a build filename — otherwise a build could masquerade as (or
		// clobber) the store's own bookkeeping files.
		{"manifest.json", "", true},
		{"pending.json", "", true},
		{"upload.tmp", "", true},
	}
	for _, tc := range cases {
		got, err := SanitizeBuildFilename(tc.in)
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

func TestReadBuildRejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ReadBuild("../etc/passwd.apk"); err == nil {
		t.Fatal("expected traversal attempt to fail")
	}
	if _, err := s.ReadBuild("manifest.json"); err == nil {
		t.Fatal("expected reserved sidecar name to fail")
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
	if _, err := s.ReadBuild("anything.apk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadBuild on disabled store: %v", err)
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
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", "", bytes.NewReader([]byte("arm64"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "universal", "eneverre-tv-universal-2.0.0.apk", "", bytes.NewReader([]byte("universal"))); err != nil {
		t.Fatal(err)
	}
	ar, err := s.GetActive()
	if err != nil {
		t.Fatal(err)
	}
	if ar == nil || len(ar.Builds) != 2 {
		t.Fatalf("expected 2 builds, got %+v", ar)
	}
	// On-disk: both build files present.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-2.0.0.apk")); err != nil {
		t.Errorf("arm64 build file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-universal-2.0.0.apk")); err != nil {
		t.Errorf("universal build file missing: %v", err)
	}
}

func TestActive_AppendRecordsContentType(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	b, err := s.AppendBuild(meta, "ios", "app.ipa", "application/octet-stream", bytes.NewReader([]byte("ios-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if b.ContentType != "application/octet-stream" {
		t.Errorf("ContentType: got %q", b.ContentType)
	}
	ar, _ := s.GetActive()
	if ar.Builds[0].ContentType != "application/octet-stream" {
		t.Errorf("persisted ContentType: got %q", ar.Builds[0].ContentType)
	}
}

func TestActive_Commit(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "2.0.0", VersionCode: 20000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", "", bytes.NewReader([]byte("arm64"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "universal", "eneverre-tv-universal-2.0.0.apk", "", bytes.NewReader([]byte("universal"))); err != nil {
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
	// Both build files still on disk and addressable.
	for _, b := range committed.Builds {
		if _, err := s.ReadBuild(b.Filename); err != nil {
			t.Errorf("build file %s not readable: %v", b.Filename, err)
		}
	}
}

func TestActive_AppendReplacesByVariant(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	// First upload of arm64.
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", "", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	// Re-upload with the same variant but a different filename and contents.
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-1.0.1.apk", "", bytes.NewReader([]byte("v2-bigger"))); err != nil {
		t.Fatal(err)
	}
	ar, _ := s.GetActive()
	if ar == nil || len(ar.Builds) != 1 {
		t.Fatalf("expected 1 build, got %+v", ar)
	}
	if ar.Builds[0].Filename != "eneverre-tv-arm64-1.0.1.apk" {
		t.Errorf("Filename: got %q", ar.Builds[0].Filename)
	}
	// The old build file was removed.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.0.apk")); !os.IsNotExist(err) {
		t.Errorf("old build file should be removed: %v", err)
	}
}

func TestActive_VersionCodeMismatch(t *testing.T) {
	s := newTestStore(t)
	v1 := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(v1, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", "", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	v2 := Manifest{VersionName: "2.0.0", VersionCode: 20000}
	// Append with the wrong versionCode: must error and NOT modify the active release.
	_, err := s.AppendBuild(v2, "arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", "", bytes.NewReader([]byte("v2")))
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
	if _, err := s.AppendBuild(v1, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", "", bytes.NewReader([]byte("v1"))); err != nil {
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
	// The v1 build file is removed.
	if _, err := os.Stat(filepath.Join(s.dir, "eneverre-tv-arm64-1.0.0.apk")); !os.IsNotExist(err) {
		t.Errorf("v1 build file should be removed: %v", err)
	}
}

func TestActive_Discard(t *testing.T) {
	s := newTestStore(t)
	meta := Manifest{VersionName: "1.0.0", VersionCode: 10000}
	if _, err := s.StartActive(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64.apk", "", bytes.NewReader([]byte("x"))); err != nil {
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
		t.Errorf("build file should be removed: %v", err)
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
		if _, err := s.AppendBuild(meta, "arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", "", bytes.NewReader([]byte("arm64"))); err != nil {
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
		// can tell which build belongs to which release.
		if p.Filename == "" {
			t.Fatalf("part %d: empty filename", i)
		}
		_, err := s.AppendBuild(Manifest{VersionName: "v", VersionCode: versionCode}, p.Variant, p.Filename, p.ContentType, p.Reader)
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

func TestRotation_DeletesPreviousReleaseBuilds(t *testing.T) {
	s := newTestStore(t)
	// v1 — one .apk and one arbitrary-extension build, to prove the
	// keep-set-based cleanup isn't specific to .apk.
	publishCommit(t, s, 1, []BuildInput{
		{Variant: "arm64-v8a", Filename: "v1-arm64.apk", Reader: bytes.NewReader([]byte("v1-arm64"))},
		{Variant: "ios", Filename: "v1-ios.ipa", Reader: bytes.NewReader([]byte("v1-ios"))},
	})
	// v2 — different filenames
	publishCommit(t, s, 2, []BuildInput{
		{Variant: "arm64-v8a", Filename: "v2-arm64.apk", Reader: bytes.NewReader([]byte("v2-arm64"))},
		{Variant: "ios", Filename: "v2-ios.ipa", Reader: bytes.NewReader([]byte("v2-ios"))},
	})
	// After two publishes: v1 build files are gone, v2 build files remain.
	for _, fn := range []string{"v1-arm64.apk", "v1-ios.ipa"} {
		if _, err := os.Stat(filepath.Join(s.dir, fn)); !os.IsNotExist(err) {
			t.Errorf("v1 build file %s should be deleted: %v", fn, err)
		}
	}
	for _, fn := range []string{"v2-arm64.apk", "v2-ios.ipa"} {
		if _, err := os.Stat(filepath.Join(s.dir, fn)); err != nil {
			t.Errorf("v2 build file %s should remain: %v", fn, err)
		}
	}
	// No history is kept: there is no releases/ directory.
	if _, err := os.Stat(filepath.Join(s.dir, "releases")); !os.IsNotExist(err) {
		t.Errorf("releases/ should not exist: %v", err)
	}
}

func TestRotation_PreservesReusedFilenames(t *testing.T) {
	// Edge case: the CI sends the same build filename for two consecutive
	// releases. The rotation should NOT delete the file (the new release
	// still references it): the v1 file has the same name as the v2
	// file, so the new release's keep set includes it and the rotation
	// leaves it alone.
	s := newTestStore(t)
	publishCommit(t, s, 1, []BuildInput{
		{Variant: "arm64-v8a", Filename: "app.apk", Reader: bytes.NewReader([]byte("content"))},
	})
	publishCommit(t, s, 2, []BuildInput{
		{Variant: "arm64-v8a", Filename: "app.apk", Reader: bytes.NewReader([]byte("content"))},
	})
	if _, err := os.Stat(filepath.Join(s.dir, "app.apk")); err != nil {
		t.Errorf("app.apk should remain (still in v2's keep set): %v", err)
	}
}

func TestRotation_PreservesSidecarsButDeletesStrayFiles(t *testing.T) {
	// The rotation must never touch manifest.json, pending.json, or an
	// in-progress .tmp write. But since builds can now carry any
	// extension (or none), there is no longer a way to distinguish "an
	// orphaned build with an unusual name" from "an unrelated stray
	// file" — anything else not in the new release's keep set is
	// deleted.
	s := newTestStore(t)
	publishCommit(t, s, 1, []BuildInput{
		{Variant: "arm64-v8a", Filename: "v1.apk", Reader: bytes.NewReader([]byte("v1"))},
	})
	if err := os.WriteFile(filepath.Join(s.dir, "stale.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "in-progress.tmp"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	publishCommit(t, s, 2, []BuildInput{
		{Variant: "arm64-v8a", Filename: "v2.apk", Reader: bytes.NewReader([]byte("v2"))},
	})
	for _, fn := range []string{"manifest.json", "in-progress.tmp"} {
		if _, err := os.Stat(filepath.Join(s.dir, fn)); err != nil {
			t.Errorf("%s should remain: %v", fn, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("unreferenced stale.txt should have been deleted, got: %v", err)
	}
}
