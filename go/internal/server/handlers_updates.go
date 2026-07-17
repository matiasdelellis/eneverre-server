package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"eneverre/internal/updates"
)

// Supported client tracks. Each track has its own subdirectory under the
// configured updates storage dir and its own triple of routes.
var updateTracks = []string{"tv", "phone"}

// handleAppUpdate returns the current manifest for a track, or 204 No Content
// when no publish has happened yet. The response shape is a JSON object with
// a `builds` array — one entry per APK in the current release. The URL for
// each build is auto-generated from the request's host (or the configured
// public_base_url). See doc/UPDATES.md.
func (a *App) handleAppUpdate(track string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := a.updates[track]
		if !ok {
			httpError(w, http.StatusNotFound, "Unknown track: "+track)
			return
		}
		if !store.Enabled() {
			httpError(w, http.StatusServiceUnavailable, "Auto-update is not configured on this server")
			return
		}
		m, err := store.Get()
		if err != nil {
			if errors.Is(err, updates.ErrNotFound) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			slog.Error("updates get failed", "track", track, "err", err)
			httpError(w, http.StatusInternalServerError, "Failed to read update manifest")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, manifestResponse(m, track, a.cfg.UpdatesPublicBaseURL(), publicURLForRequest(r)))
	}
}

// apiBuild is the per-APK entry in the GET response. The `url` field is
// computed at request time; the rest mirrors the on-disk APKBuild.
type apiBuild struct {
	ABI         string `json:"abi"`
	APKFilename string `json:"apkFilename"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	URL         string `json:"url"`
}

// apiManifest is the JSON body the Android clients consume. Every APK in
// the current release is a first-class entry in builds; there is no
// single-APK convenience field — clients that only know about one APK
// must pick from the array.
type apiManifest struct {
	VersionName  string     `json:"versionName"`
	VersionCode  int        `json:"versionCode"`
	Mandatory    bool       `json:"mandatory"`
	ReleaseNotes string     `json:"releaseNotes,omitempty"`
	Builds       []apiBuild `json:"builds"`
}

// manifestResponse builds the apiManifest from the on-disk Manifest plus
// the base URL. Each build's URL is <base>/api/app/updates/<track>/<file>.
// base falls back from the configured public_base_url to the request host.
func manifestResponse(m *updates.Manifest, track, configuredBase, requestBase string) *apiManifest {
	base := strings.TrimRight(configuredBase, "/")
	if base == "" {
		base = strings.TrimRight(requestBase, "/")
	}
	out := &apiManifest{
		VersionName:  m.VersionName,
		VersionCode:  m.VersionCode,
		Mandatory:    m.Mandatory,
		ReleaseNotes: m.ReleaseNotes,
		Builds:       make([]apiBuild, 0, len(m.Builds)),
	}
	for _, b := range m.Builds {
		out.Builds = append(out.Builds, apiBuild{
			ABI:         b.ABI,
			APKFilename: b.Filename,
			Size:        b.Size,
			SHA256:      b.SHA256,
			URL:         fmt.Sprintf("%s/api/app/updates/%s/%s", base, track, b.Filename),
		})
	}
	return out
}

// handleAppUpdatesPublish accepts a multipart/form-data POST and publishes
// one or more APKs to the track's storage directory, persisting a manifest
// that lists them all under their ABI tags.
//
// Auth: the request MUST carry `Authorization: Bearer <token>` matching
// the configured [updates] publish_token. User/password and session Bearer
// tokens are NOT accepted. If no token is configured on the server, the
// endpoint returns 503.
//
// Body: every APK must be sent as a form file with name `apk_<abi>`,
// where <abi> is the Android ABI string the file targets. Common values:
//
//	apk_arm64-v8a       (most modern ARM devices)
//	apk_armeabi-v7a     (older 32-bit ARM)
//	apk_x86_64          (Chromebooks / emulators)
//	apk_x86             (older emulators)
//	apk_universal       (fat APK; fallback for clients that don't list a specific ABI)
//
// A single release may carry any combination of these (at least one is
// required). The form fields `versionName`, `versionCode`, `releaseNotes`
// and `mandatory` apply to the whole release; each build inherits them.
//
// Streaming: r.MultipartReader() walks the parts without buffering. Each
// `apk_<abi>` body is streamed into a temp file as soon as it is
// encountered (multipart.Reader discards unread bodies on the next
// NextPart() call). Temp files are removed by defer. Memory is O(1) per
// APK; disk briefly doubles (temp + final) per build during the publish.
//
// r.Body is wrapped in http.MaxBytesReader to cap the total request size
// (default 500 MiB, configurable via [updates] max_apk_size). On overflow
// the read returns *http.MaxBytesError, which the handler translates to
// 413.
func (a *App) handleAppUpdatesPublish(track string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.requirePublishAuth(w, r) {
			return
		}
		store, ok := a.updates[track]
		if !ok {
			httpError(w, http.StatusNotFound, "Unknown track: "+track)
			return
		}
		if !store.Enabled() {
			httpError(w, http.StatusServiceUnavailable, "Auto-update is not configured on this server")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, a.cfg.UpdatesMaxAPKSize())

		mr, err := r.MultipartReader()
		if err != nil {
			httpError(w, http.StatusBadRequest, "Expected multipart/form-data: "+err.Error())
			return
		}

		var (
			versionName    string
			versionCodeStr string
			releaseNotes   string
			mandatory      bool
			// finalize defaults to true: a single POST behaves as before
			// (writes the APK + commits the manifest). Set finalize=false
			// on the first N-1 POSTs of a multi-POST sequence so the body
			// of each request is small and the timeout risk per request
			// is bounded by one APK's upload time, not the whole release.
			finalize = true
			// Each build is a (abi, original filename, temp file path) triple.
			// The temp file is reopened for reading in store.Publish and
			// removed by the defer below.
			builds []pendingBuild
		)
		defer func() {
			for _, b := range builds {
				_ = os.Remove(b.tempPath)
			}
		}()

		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				if isMaxBytesErr(err) {
					httpError(w, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("Body exceeds the %d-byte server limit", a.cfg.UpdatesMaxAPKSize()))
					return
				}
				httpError(w, http.StatusBadRequest, "Multipart parse error: "+err.Error())
				return
			}
			name := part.FormName()
			switch {
			case strings.HasPrefix(name, "apk_") && len(name) > len("apk_"):
				abi := name[len("apk_"):]
				tempPath, ok := a.streamAPKPart(w, part, abi)
				if !ok {
					return
				}
				builds = append(builds, pendingBuild{
					abi:      abi,
					origName: part.FileName(),
					tempPath: tempPath,
				})
			case name == "versionName":
				b, _ := io.ReadAll(io.LimitReader(part, 256))
				versionName = strings.TrimSpace(string(b))
				part.Close()
			case name == "versionCode":
				b, _ := io.ReadAll(io.LimitReader(part, 64))
				versionCodeStr = strings.TrimSpace(string(b))
				part.Close()
			case name == "releaseNotes":
				b, _ := io.ReadAll(io.LimitReader(part, 64*1024))
				releaseNotes = string(b)
				part.Close()
			case name == "finalize":
				b, _ := io.ReadAll(io.LimitReader(part, 16))
				finalize = parseBool(string(b), true) // default true keeps single-POST simple
				part.Close()
			case name == "mandatory":
				b, _ := io.ReadAll(io.LimitReader(part, 16))
				mandatory = parseBool(string(b), false)
				part.Close()
			default:
				_, _ = io.Copy(io.Discard, part)
				part.Close()
			}
		}

		if versionName == "" || versionCodeStr == "" {
			httpError(w, http.StatusUnprocessableEntity, "versionName and versionCode are required")
			return
		}
		versionCode, err := strconv.Atoi(versionCodeStr)
		if err != nil || versionCode < 0 {
			httpError(w, http.StatusUnprocessableEntity, "versionCode must be a non-negative integer")
			return
		}
		if len(builds) == 0 {
			httpError(w, http.StatusUnprocessableEntity, "at least one apk_<abi> file is required (e.g. apk_arm64-v8a)")
			return
		}

		// Reopen each temp file as a Reader for store.Publish. The
		// store reads each one end-to-end into its own final temp
		// file, then atomic-renames into place.
		inputs := make([]updates.BuildInput, 0, len(builds))
		openFiles := make([]*os.File, 0, len(builds))
		for _, b := range builds {
			f, ferr := os.Open(b.tempPath)
			if ferr != nil {
				for _, of := range openFiles {
					of.Close()
				}
				httpError(w, http.StatusInternalServerError, "Failed to reopen temp APK: "+ferr.Error())
				return
			}
			openFiles = append(openFiles, f)
			inputs = append(inputs, updates.BuildInput{
				ABI:      b.abi,
				Filename: b.origName,
				Reader:   f,
			})
		}

		meta := updates.Manifest{
			VersionName:  versionName,
			VersionCode:  versionCode,
			Mandatory:    mandatory,
			ReleaseNotes: releaseNotes,
		}

		// Multi-POST lifecycle: every POST goes through the active release
		// (StartActive / AppendBuild), and finalize=true adds a final
		// CommitActive. A single POST with finalize=true (or omitted,
		// since the default is true) starts, appends, and commits in one
		// request — equivalent to the old single-shot Publish path but
		// with the active state persisted in pending.json (useful for
		// debugging and crash recovery).
		active, _ := store.GetActive()
		if active == nil || active.VersionCode != versionCode {
			if _, err := store.StartActive(meta); err != nil {
				for _, f := range openFiles {
					f.Close()
				}
				httpError(w, http.StatusInternalServerError, "Failed to start active release: "+err.Error())
				return
			}
		}
		var lastErr error
		var appended []string
		for i, p := range inputs {
			if _, err := openFiles[i].Seek(0, io.SeekStart); err != nil {
				lastErr = err
				break
			}
			if _, err := store.AppendBuild(meta, p.ABI, p.Filename, openFiles[i]); err != nil {
				lastErr = err
				break
			}
			appended = append(appended, p.ABI)
		}
		for _, f := range openFiles {
			f.Close()
		}
		if lastErr != nil {
			if isMaxBytesErr(lastErr) {
				httpError(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("Body exceeds the %d-byte server limit", a.cfg.UpdatesMaxAPKSize()))
				return
			}
			httpError(w, http.StatusUnprocessableEntity, lastErr.Error())
			return
		}

		if !finalize {
			current, err := store.GetActive()
			if err != nil || current == nil {
				httpError(w, http.StatusInternalServerError, "Active release disappeared after append")
				return
			}
			abis := make([]string, 0, len(current.Builds))
			for _, b := range current.Builds {
				abis = append(abis, b.ABI)
			}
			slog.Info("update appended",
				"track", track,
				"version", current.VersionName,
				"code", current.VersionCode,
				"abis_this_post", strings.Join(appended, ","),
				"abis_total", strings.Join(abis, ","),
			)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":            true,
				"state":         "pending",
				"versionName":   current.VersionName,
				"versionCode":   current.VersionCode,
				"builds":        len(current.Builds),
				"abis":          abis,
				"abis_appended": appended,
			})
			return
		}

		// finalize=true: promote the active release to the current
		// manifest. The active state is cleared, the APKs are now
		// referenced by manifest.json and downloadable. APKs that
		// belonged to older releases are removed from disk.
		committed, err := store.CommitActive()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "Failed to commit: "+err.Error())
			return
		}
		abis := make([]string, 0, len(committed.Builds))
		for _, b := range committed.Builds {
			abis = append(abis, b.ABI)
		}
		slog.Info("update committed",
			"track", track,
			"version", committed.VersionName,
			"code", committed.VersionCode,
			"builds", len(committed.Builds),
			"abis", strings.Join(abis, ","),
			"mandatory", committed.Mandatory,
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"state":       "committed",
			"versionName": committed.VersionName,
			"versionCode": committed.VersionCode,
			"builds":      len(committed.Builds),
			"abis":        abis,
		})
	}
}

// pendingBuild is the per-build bookkeeping the publish handler keeps
// while it streams the multipart body. The temp file lives in os.TempDir
// until store.Publish consumes it; the original multipart filename is
// kept so the store can sanitize and preserve it on disk.
type pendingBuild struct {
	abi      string
	origName string
	tempPath string
}

// streamAPKPart reads the body of an apk_<abi> multipart part into a temp
// file (mode 0600) and returns its path. On any error it writes the
// response and returns ("", false). Caller is responsible for removing
// the temp file (typically via defer on a list).
func (a *App) streamAPKPart(w http.ResponseWriter, part *multipart.Part, abi string) (string, bool) {
	f, err := os.CreateTemp("", "eneverre-upload-*.apk.tmp")
	if err != nil {
		part.Close()
		httpError(w, http.StatusInternalServerError, "Failed to allocate temp file: "+err.Error())
		return "", false
	}
	if _, err := io.Copy(f, part); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		part.Close()
		if isMaxBytesErr(err) {
			httpError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("APK exceeds the %d-byte server limit", a.cfg.UpdatesMaxAPKSize()))
			return "", false
		}
		httpError(w, http.StatusBadRequest, "Failed to read APK: "+err.Error())
		return "", false
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		part.Close()
		httpError(w, http.StatusInternalServerError, "Failed to finalize temp APK: "+err.Error())
		return "", false
	}
	part.Close()
	return f.Name(), true
}

// isMaxBytesErr reports whether err originates from http.MaxBytesReader
// hitting its cap. The error type was added in Go 1.19.
func isMaxBytesErr(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// handleAppUpdateFile serves the APK bytes. The filename in the URL is
// matched against the manifest — anything else is a 404, which keeps the
// endpoint useful (only the current build is fetchable) and prevents
// fingerprinting the disk contents.
func (a *App) handleAppUpdateFile(track string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := a.updates[track]
		if !ok {
			httpError(w, http.StatusNotFound, "Unknown track: "+track)
			return
		}
		if !store.Enabled() {
			httpError(w, http.StatusServiceUnavailable, "Auto-update is not configured on this server")
			return
		}
		name := r.PathValue("filename")
		// The manifest's Builds list is the source of truth for "current".
		// A request for a filename not in the list is treated as "not the
		// current build" and answered with 404 — old APKs (from a previous
		// release) stay on disk so in-flight downloads survive a publish,
		// but are not addressable.
		m, err := store.Get()
		if err != nil {
			if errors.Is(err, updates.ErrNotFound) {
				httpError(w, http.StatusNotFound, "No current build")
				return
			}
			httpError(w, http.StatusInternalServerError, "Failed to read manifest")
			return
		}
		allowed := false
		for _, b := range m.Builds {
			if b.Filename == name {
				allowed = true
				break
			}
		}
		if !allowed {
			httpError(w, http.StatusNotFound, "Not the current build")
			return
		}
		f, err := store.ReadAPK(name)
		if err != nil {
			if errors.Is(err, updates.ErrNotFound) {
				httpError(w, http.StatusNotFound, "APK missing on disk")
				return
			}
			httpError(w, http.StatusInternalServerError, "Failed to open APK")
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "Failed to stat APK")
			return
		}
		ctype := mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			ctype = "application/vnd.android.package-archive"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		// A 100MB+ APK on a slow link takes well over the global WriteTimeout
		// (30s); the response size is bounded by the file on disk.
		clearWriteDeadline(w, "apk download")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	}
}

// publicURLForRequest derives an "http(s)://host[:port]" string from the
// incoming request, for use as a fallback base when no public_base_url is
// configured.
//
// The scheme and host are taken in this order:
//  1. `X-Forwarded-Proto` (set by a reverse proxy that terminated TLS)
//  2. `r.TLS != nil` (TLS terminated at the Go server itself)
//  3. "http" as a last-resort fallback
//
// The host is taken from `X-Forwarded-Host` (proxy override) or `r.Host`.
// `r.Host` is what the *Go server* sees — when behind Caddy, that's the
// downstream host header (typically the public host, since Caddy
// forwards it). The operator can override the host with `X-Forwarded-Host`.
func publicURLForRequest(r *http.Request) string {
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// parseBool accepts the usual truthy/falsy strings and falls back to def.
// Empty string -> def. Mirrors configparser.getboolean semantics so the
// `mandatory` form field is forgiving.
func parseBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def
	case "1", "yes", "true", "on":
		return true
	case "0", "no", "false", "off":
		return false
	default:
		return def
	}
}

// requirePublishAuth gates the publish endpoints. The only accepted
// credential is the configured [updates] publish_token, sent as
// `Authorization: Bearer <token>`. If no token is configured on the server,
// the endpoint is treated as not provisioned and the request is rejected
// with 503 — there is no admin fallback by design, so a misconfigured
// deploy never silently grants publish access via user credentials.
//
// Returns true on success, false after writing the 401/503 response.
func (a *App) requirePublishAuth(w http.ResponseWriter, r *http.Request) bool {
	expected := a.cfg.UpdatesPublishToken()
	if expected == "" {
		slog.Warn("updates publish denied",
			"path", r.URL.Path,
			"ip", a.proxyTrust.clientIP(r),
			"reason", "publish_token not configured on server",
		)
		httpError(w, http.StatusServiceUnavailable,
			"Auto-update publish is not configured: set [updates] publish_token")
		return false
	}
	got := bearerToken(r.Header.Get("Authorization"))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		slog.Warn("updates publish denied",
			"path", r.URL.Path,
			"ip", a.proxyTrust.clientIP(r),
			"reason", "missing or invalid publish_token",
		)
		httpError(w, http.StatusUnauthorized, "Invalid publish token")
		return false
	}
	return true
}

// bearerToken extracts the token from an `Authorization: Bearer <t>` header.
// It is forgiving of case (the scheme is case-insensitive per RFC 7235) and
// of surrounding whitespace. Returns "" when the header is absent or
// malformed.
func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
