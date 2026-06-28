// Package updates implements the auto-update sidecar store used by the
// Android TV and phone clients. Each client "track" (tv, phone) is backed by
// its own subdirectory under a configurable storage root; the current build
// for a track is described by a single manifest.json sidecar and the APK
// itself lives next to it.
//
// The store is intentionally small: there is no history, no version monotonicity
// check, and no DB involvement. The on-disk manifest IS the row.
package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// manifestFilename is the sidecar name. Kept in one place so callers and tests
// stay in sync.
const manifestFilename = "manifest.json"

// activeFilename holds the in-progress release when the CI is building it
// up via multiple POSTs. It is invisible to GET /api/app/<track>/update
// until the final POST with `finalize=true` promotes it to manifest.json.
const activeFilename = "pending.json"

// releasesDir holds snapshots of previous committed releases, so that
// an in-flight download of the previous version can still finish even
// after a new release was published. Each file is a full Manifest JSON
// (the same shape as manifest.json). The directory is capped at
// keepPreviousReleases entries — older ones are deleted along with
// their APKs.
const releasesDir = "releases"

// ErrNotFound is returned by Get when no manifest exists for the track. The
// HTTP layer maps it to a 204 No Content.
var ErrNotFound = errors.New("updates: no manifest")

// ErrActiveVersionMismatch is returned by AppendBuild when the active
// release has a different versionCode than the caller. The handler should
// call DiscardActive and start a new active release.
var ErrActiveVersionMismatch = errors.New("updates: active release has a different versionCode")

// APKBuild describes one APK variant of a release. A release can carry
// several builds — one per ABI the CI produces, plus a "universal" (fat)
// APK as fallback for clients that don't list a specific ABI. The `abi`
// value is opaque to the server: it is whatever the CI sent in the form
// field name (e.g. "arm64-v8a", "armeabi-v7a", "x86_64", "universal").
// Clients map it against `Build.SUPPORTED_ABIS`.
type APKBuild struct {
	ABI      string `json:"abi"`
	Filename string `json:"apkFilename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// BuildInput is what the publish handler hands to the store for one APK:
// the streamed reader plus the metadata to record. Filename is the
// original multipart filename (will be sanitized); ABI is the value the
// CI used in the form field (e.g. "arm64-v8a", "universal").
type BuildInput struct {
	ABI      string
	Filename string
	Reader   io.Reader
}

// Manifest is the per-track "current release" record. It is serialized to
// manifest.json inside the track's storage directory. A release carries
// one or more builds. There is no single-APK convenience field — every
// build is a first-class entry in Builds. The download URL for each build
// is computed at request time from the request host (or the configured
// public_base_url).
type Manifest struct {
	VersionName  string    `json:"versionName"`
	VersionCode  int       `json:"versionCode"`
	Mandatory    bool      `json:"mandatory"`
	ReleaseNotes string    `json:"releaseNotes,omitempty"`
	UploadedAt   time.Time `json:"uploadedAt"`
	// Builds is the canonical list. Every APK in the current release is
	// here. The order is the order the CI published them in.
	Builds []APKBuild `json:"builds"`
}

// ActiveRelease is an in-progress release being built up via multiple POSTs
// (one APK per POST, with `finalize=false`). It lives in pending.json
// until the final POST promotes it to manifest.json (CommitActive) or
// the CI starts a new release with a different versionCode (DiscardActive).
// While active, the release is invisible to GET /api/app/<track>/update —
// the previously committed manifest is served until commit.
type ActiveRelease struct {
	VersionName  string     `json:"versionName"`
	VersionCode  int        `json:"versionCode"`
	Mandatory    bool       `json:"mandatory"`
	ReleaseNotes string     `json:"releaseNotes,omitempty"`
	Builds       []APKBuild `json:"builds"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Store is the per-track store. Each track has its own Store instance; the
// shared resource is the on-disk directory, not the in-memory state.
type Store struct {
	root string // parent directory (e.g. /var/lib/eneverre/app-updates)
	dir  string // root/<track>
	// mu serializes Publish/AppendBuild/CommitActive/DiscardActive so the
	// sidecars and APKs swap atomically with respect to other publishers.
	// Get/ReadAPK do not take the lock: a partial write produces a
	// "manifest points at missing APK" condition that surfaces as a 500,
	// never as a corrupted download (the APK is written via .tmp + rename).
	mu sync.Mutex
	// active is the cached contents of pending.json. It is loaded on
	// Ensure and updated on every AppendBuild / overwritten on Discard /
	// cleared on Commit. nil means "no active release".
	active *ActiveRelease
}

// NewStore builds a Store for the given (root, track) pair. It does not touch
// the filesystem; call Enabled to check whether the feature is configured.
func NewStore(root, track string) *Store {
	return &Store{
		root: root,
		dir:  filepath.Join(root, track),
	}
}

// Enabled reports whether the store has a storage directory configured. When
// false, the HTTP layer answers every endpoint with 503.
func (s *Store) Enabled() bool { return strings.TrimSpace(s.root) != "" }

// Dir returns the absolute path to the track's storage directory. The
// directory is created on demand by Ensure.
func (s *Store) Dir() string { return s.dir }

// Ensure creates the track directory if it does not exist and loads any
// pending release from a previous run. Called at startup for every
// configured track.
func (s *Store) Ensure() error {
	if !s.Enabled() {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	// Restore any in-progress release from a previous run. A half-built
	// release is just as valid as a fresh one; the next POST either
	// continues it (matching versionCode) or replaces it (mismatched).
	ar, err := s.loadActiveFromDisk()
	if err != nil {
		return err
	}
	s.active = ar
	return nil
}

// loadActiveFromDisk reads pending.json and returns the parsed active
// release, or (nil, nil) if the file does not exist. Other I/O errors are
// returned as-is.
func (s *Store) loadActiveFromDisk() (*ActiveRelease, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, activeFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ar ActiveRelease
	if err := json.Unmarshal(b, &ar); err != nil {
		return nil, fmt.Errorf("parse pending: %w", err)
	}
	return &ar, nil
}

// Get reads the current manifest. Returns ErrNotFound when no sidecar exists
// yet (i.e. no publish has happened on this track). The manifest is
// returned as-is — there is no back-compat synthesis; operators upgrading
// from a pre-multi-ABI deployment should republish.
func (s *Store) Get() (*Manifest, error) {
	if !s.Enabled() {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir, manifestFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Publish writes each APK in parts to disk (atomic per file), computes the
// SHA-256 and size of each, and persists the manifest sidecar. The readers
// are streamed end-to-end through io.TeeReader into temp files and then
// into the SHA-256 hasher, so memory usage is O(1) regardless of APK size.
// The caller owns each reader's lifecycle — Publish does not Close them.
//
// Each APK is written to <dir>/<basename>.tmp, then atomic-renamed into
// place. The manifest is written to <dir>/manifest.json.tmp, then renamed.
// Both renames are atomic on POSIX, so a concurrent Get either sees the
// previous pair (manifest + old APK set) or the new one.
//
// meta's UploadedAt field is overwritten with the publish time; the
// other release-level fields (VersionName / VersionCode / Mandatory /
// ReleaseNotes) are kept as-is. Builds is overwritten with the new build
// list.
//
// On any error after some APKs have been written, the already-written
// files are removed on a best-effort basis so the directory does not
// retain a partial set. (Atomicity here is per-file, not per-publish.)
func (s *Store) Publish(parts []BuildInput, meta Manifest) (Manifest, error) {
	if !s.Enabled() {
		return Manifest{}, errors.New("updates: not configured")
	}
	if len(parts) == 0 {
		return Manifest{}, errors.New("updates: no builds to publish")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Ensure(); err != nil {
		return Manifest{}, err
	}

	builds := make([]APKBuild, 0, len(parts))
	for _, p := range parts {
		safe, err := SanitizeAPKFilename(p.Filename)
		if err != nil {
			s.rollbackBuilds(builds)
			return Manifest{}, err
		}
		finalAPK := filepath.Join(s.dir, safe)
		tmpAPK := finalAPK + ".tmp"
		out, err := os.Create(tmpAPK)
		if err != nil {
			s.rollbackBuilds(builds)
			return Manifest{}, err
		}
		hasher := sha256.New()
		n, err := io.Copy(out, io.TeeReader(p.Reader, hasher))
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmpAPK)
			s.rollbackBuilds(builds)
			return Manifest{}, err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmpAPK)
			s.rollbackBuilds(builds)
			return Manifest{}, err
		}
		if err := os.Rename(tmpAPK, finalAPK); err != nil {
			_ = os.Remove(tmpAPK)
			s.rollbackBuilds(builds)
			return Manifest{}, err
		}
		builds = append(builds, APKBuild{
			ABI:      p.ABI,
			Filename: safe,
			Size:     n,
			SHA256:   hex.EncodeToString(hasher.Sum(nil)),
		})
	}

	meta.Builds = builds
	meta.UploadedAt = time.Now().UTC()
	if err := writeManifest(filepath.Join(s.dir, manifestFilename), &meta); err != nil {
		s.rollbackBuilds(builds)
		return Manifest{}, err
	}
	return meta, nil
}

// rollbackBuilds removes the on-disk files for the given builds. Best-effort:
// errors are swallowed because we are already on an error path.
func (s *Store) rollbackBuilds(builds []APKBuild) {
	for _, b := range builds {
		_ = os.Remove(filepath.Join(s.dir, b.Filename))
	}
}

// ReadAPK opens the APK currently advertised by the manifest. The caller is
// responsible for closing the returned file. Returns an error wrapping
// os.ErrNotExist if the manifest names a file that is no longer on disk.
func (s *Store) ReadAPK(filename string) (*os.File, error) {
	if !s.Enabled() {
		return nil, ErrNotFound
	}
	safe, err := SanitizeAPKFilename(filename)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(s.dir, safe))
}

// --- multi-POST (incremental) release lifecycle ---

// GetActive returns the in-progress release, or (nil, nil) if none. The
// returned pointer is a snapshot — callers should treat it as read-only and
// use the AppendBuild / DiscardActive / CommitActive methods to mutate.
func (s *Store) GetActive() (*ActiveRelease, error) {
	if !s.Enabled() {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, nil
}

// StartActive begins a new in-progress release, overwriting any existing
// one. The previous release's APKs are removed from disk (best-effort);
// the in-memory active state is replaced and persisted to pending.json.
// Used by the publish handler when the new POST's versionCode does not
// match the current active release.
func (s *Store) StartActive(meta Manifest) (*ActiveRelease, error) {
	if !s.Enabled() {
		return nil, errors.New("updates: not configured")
	}
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Clean up the previous active release's APKs (best-effort).
	if s.active != nil {
		for _, b := range s.active.Builds {
			_ = os.Remove(filepath.Join(s.dir, b.Filename))
		}
	}
	ar := &ActiveRelease{
		VersionName:  meta.VersionName,
		VersionCode:  meta.VersionCode,
		Mandatory:    meta.Mandatory,
		ReleaseNotes: meta.ReleaseNotes,
		Builds:       []APKBuild{},
		UpdatedAt:    time.Now().UTC(),
	}
	if err := s.saveActiveLocked(ar); err != nil {
		return nil, err
	}
	s.active = ar
	return ar, nil
}

// AppendBuild adds one APK to the active release. If a build with the
// same ABI already exists, it is replaced (the old APK is removed from
// disk). Returns ErrActiveVersionMismatch if the active release has a
// different versionCode than meta — the caller should DiscardActive and
// StartActive first.
//
// The reader is streamed end-to-end via io.TeeReader into a temp file
// and a SHA-256 hasher; memory usage is O(1).
func (s *Store) AppendBuild(meta Manifest, abi string, filename string, reader io.Reader) (*APKBuild, error) {
	if !s.Enabled() {
		return nil, errors.New("updates: not configured")
	}
	safe, err := SanitizeAPKFilename(filename)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.VersionCode != meta.VersionCode {
		return nil, ErrActiveVersionMismatch
	}
	// Refresh the release-level fields in case the CI re-sent them.
	s.active.VersionName = meta.VersionName
	s.active.Mandatory = meta.Mandatory
	s.active.ReleaseNotes = meta.ReleaseNotes

	// If a build with this ABI already exists, remove the old APK file.
	for _, b := range s.active.Builds {
		if b.ABI == abi {
			_ = os.Remove(filepath.Join(s.dir, b.Filename))
		}
	}

	finalAPK := filepath.Join(s.dir, safe)
	tmpAPK := finalAPK + ".tmp"
	out, err := os.Create(tmpAPK)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	n, err := io.Copy(out, io.TeeReader(reader, hasher))
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmpAPK)
		return nil, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpAPK)
		return nil, err
	}
	if err := os.Rename(tmpAPK, finalAPK); err != nil {
		_ = os.Remove(tmpAPK)
		return nil, err
	}

	newBuild := APKBuild{
		ABI:      abi,
		Filename: safe,
		Size:     n,
		SHA256:   hex.EncodeToString(hasher.Sum(nil)),
	}
	// Replace any existing build for this ABI in the list.
	filtered := s.active.Builds[:0]
	for _, b := range s.active.Builds {
		if b.ABI == abi {
			continue
		}
		filtered = append(filtered, b)
	}
	s.active.Builds = append(filtered, newBuild)
	s.active.UpdatedAt = time.Now().UTC()

	if err := s.saveActiveLocked(s.active); err != nil {
		// Best-effort cleanup: remove the just-written APK to avoid
		// leaving a half-built state if the sidecar write fails.
		_ = os.Remove(finalAPK)
		return nil, err
	}
	return &newBuild, nil
}

// DiscardActive drops the in-progress release (if any) and removes its
// APKs from disk. The next POST can start fresh. Safe to call when there
// is no active release.
func (s *Store) DiscardActive() error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		// Even if in-memory says no, the file may be left from a crash
		// before Ensure ran. Try to remove it.
		_ = os.Remove(filepath.Join(s.dir, activeFilename))
		return nil
	}
	for _, b := range s.active.Builds {
		_ = os.Remove(filepath.Join(s.dir, b.Filename))
	}
	s.active = nil
	_ = os.Remove(filepath.Join(s.dir, activeFilename))
	return nil
}

// CommitActive promotes the active release to the current manifest. The
// active state is cleared. The committed manifest is returned. Returns
// an error if there is no active release.
//
// Rotation: at every commit, all APKs in the track directory that are
// not part of the new release are deleted from disk. This is the
// "rotation" semantic — disk usage is bounded by the current release's
// APKs (typically a few hundred MB), not the entire publish history.
//
// For record-keeping / recovery, the previous manifests (just the JSON
// metadata, not the APKs) are kept in releases/<unixnano>.json. The
// releases/ directory is capped at keepPreviousReleases entries. With
// keepPreviousReleases=0, no records are kept. The previous manifests
// are useful for diagnosing "what did the operator publish last week?"
// but they are NOT addressable via the download endpoint — a client
// that tries to download an APK from a previous release will get a
// 404. If you need true in-flight support across publishes, use the
// multi-POST finalize=false flow so a single release is built up
// before any old APK is deleted.
func (s *Store) CommitActive(keepPreviousReleases int) (*Manifest, error) {
	if !s.Enabled() {
		return nil, errors.New("updates: not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil, errors.New("updates: no active release to commit")
	}

	// 1. Read the current (about-to-be-replaced) manifest so we can
	//    snapshot it for record-keeping.
	oldM, _ := s.readManifestFromDisk()

	// 2. Write the new manifest. This replaces the current one.
	m := Manifest{
		VersionName:  s.active.VersionName,
		VersionCode:  s.active.VersionCode,
		Mandatory:    s.active.Mandatory,
		ReleaseNotes: s.active.ReleaseNotes,
		UploadedAt:   time.Now().UTC(),
		Builds:       s.active.Builds,
	}
	if err := writeManifest(filepath.Join(s.dir, manifestFilename), &m); err != nil {
		return nil, err
	}

	// 3. Clear the active state.
	s.active = nil
	_ = os.Remove(filepath.Join(s.dir, activeFilename))

	// 4. Snapshot the OLD manifest into releases/ (if keepPreviousReleases
	//    > 0 and there was one). This becomes the newest previous
	//    entry. The previous manifest's APKs are NOT kept on disk
	//    beyond this commit — see step 6.
	if keepPreviousReleases > 0 && oldM != nil && len(oldM.Builds) > 0 {
		if err := s.snapshotReleaseLocked(oldM); err != nil {
			return &m, fmt.Errorf("snapshot previous release: %w", err)
		}
	}

	// 5. Prune releases/ to the most recent N entries.
	if err := s.pruneReleasesLocked(keepPreviousReleases); err != nil {
		return &m, fmt.Errorf("prune releases: %w", err)
	}

	// 6. Delete every .apk on disk that is not in the new release.
	//    The previous release's APKs are NOT part of the kept set
	//    (even though their manifests are kept for record in step 4):
	//    the download endpoint has no way to reach them, and keeping
	//    them around would defeat the rotation. The kept set is just
	//    the new manifest's builds.
	kept := map[string]bool{}
	for _, b := range m.Builds {
		kept[b.Filename] = true
	}
	if err := s.deleteOrphanedAPKsLocked(kept); err != nil {
		return &m, fmt.Errorf("delete orphaned apks: %w", err)
	}

	return &m, nil
}

// readManifestFromDisk returns the current manifest, or (nil, nil) if
// the file does not exist. Caller must NOT hold s.mu.
func (s *Store) readManifestFromDisk() (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, manifestFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// snapshotReleaseLocked writes m to releases/<unixnano>.json. Caller
// must hold s.mu. The releases/ directory is created if it does not
// exist. The filename uses unix-nanoseconds so two releases with the
// same versionCode still get distinct files.
func (s *Store) snapshotReleaseLocked(m *Manifest) error {
	if err := os.MkdirAll(filepath.Join(s.dir, releasesDir), 0o755); err != nil {
		return err
	}
	name := strconv.FormatInt(time.Now().UTC().UnixNano(), 10) + ".json"
	return writeManifest(filepath.Join(s.dir, releasesDir, name), m)
}

// pruneReleasesLocked keeps only the most recent keepN files in
// releases/ (by mtime) and deletes the rest. Caller must hold s.mu.
// keepN <= 0 deletes all entries.
func (s *Store) pruneReleasesLocked(keepN int) error {
	entries, err := os.ReadDir(filepath.Join(s.dir, releasesDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Sort by mtime descending (newest first).
	type entry struct {
		path    string
		modTime time.Time
	}
	all := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, entry{
			path:    filepath.Join(s.dir, releasesDir, e.Name()),
			modTime: fi.ModTime(),
		})
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].modTime.After(all[j].modTime)
	})
	for i, e := range all {
		if i < keepN {
			continue
		}
		_ = os.Remove(e.path)
	}
	return nil
}

// deleteOrphanedAPKsLocked removes every .apk file in the track
// directory that is not in the keep set. Non-APK files (manifest.json,
// pending.json, releases/*.json) are left alone. Caller must hold
// s.mu. Best-effort: a failed remove is logged via the returned error
// (we keep going for the rest).
func (s *Store) deleteOrphanedAPKsLocked(keep map[string]bool) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".apk") {
			continue
		}
		if keep[name] {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// saveActiveLocked serializes ar to pending.json. Caller must hold s.mu.
func (s *Store) saveActiveLocked(ar *ActiveRelease) error {
	b, err := json.MarshalIndent(ar, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, activeFilename+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, activeFilename))
}

// writeManifest serializes m to path via a .tmp + rename. Callers pass the
// final path (e.g. <dir>/manifest.json).
func writeManifest(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SanitizeAPKFilename validates and normalizes an APK filename. The input
// must be a plain basename — no path separators (forward or back), no NUL,
// no `..` segments, must end in `.apk`. Centralized so the GET handler and
// Publish agree on what is acceptable and so a misconfigured CI cannot smuggle
// in a traversal.
func SanitizeAPKFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("invalid apk filename: empty")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "\x00") {
		return "", fmt.Errorf("invalid apk filename: %q", name)
	}
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid apk filename: %q", name)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".apk") {
		return "", fmt.Errorf("apk filename must end in .apk: %q", name)
	}
	return name, nil
}
