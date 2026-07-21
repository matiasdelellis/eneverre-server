package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eneverre/internal/config"
	"eneverre/internal/updates"
)

// withUpdatesApp builds an App backed by an updates.Registry rooted at a
// temp dir, with an optional publish token. Other App fields are zero (no
// DB, no cameras, no static fs) — these tests only exercise the update
// endpoints. Tracks are not pre-created: the registry lazily creates one
// per track name on first use, exactly like the real server.
func withUpdatesApp(t *testing.T, publishToken string) (*App, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{Updates: config.Section{}}
	if publishToken != "" {
		cfg.Updates["publish_token"] = publishToken
	}
	a := &App{cfg: cfg, updatesRoot: root, updatesRegistry: updates.NewRegistry(root)}
	return a, root
}

// withTrack sets the {track} path parameter on r the way the real mux
// would after matching "GET /api/app/{track}/update" etc. Handlers now
// read the track via r.PathValue("track") instead of a closure parameter,
// so tests that call a handler directly (bypassing the mux) must set this
// themselves.
func withTrack(r *http.Request, track string) *http.Request {
	r.SetPathValue("track", track)
	return r
}

// doPublish posts a minimal valid build artifact to the publish endpoint.
// The file is sent as `build_universal` so the multi-variant form path is
// exercised. Returns the response (the caller inspects status / body).
func doPublish(t *testing.T, a *App, track, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("build_universal", "eneverre-tv-universal-1.0.0.apk")
	if err != nil {
		t.Fatal(err)
	}
	// Use a deterministic payload so the SHA-256 is easy to assert.
	payload := []byte("fake-apk-bytes-for-tests")
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("versionName", "1.0.0")
	_ = mw.WriteField("versionCode", "10000")
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/admin/app/updates/"+track, body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	a.handleAppUpdatesPublish(rec, withTrack(r, track))
	return rec
}

func TestPublish_NoTokenConfigured_RejectsAllAuth(t *testing.T) {
	a, root := withUpdatesApp(t, "") // no token → publish must be disabled
	// No auth header at all.
	rec := doPublish(t, a, "tv", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "publish_token") {
		t.Fatalf("expected error to mention publish_token, got %q", rec.Body.String())
	}
	// Basic auth is rejected outright when no token is configured — there is
	// no user/password fallback (the creds are never even validated).
	rec = doPublish(t, a, "tv", "Basic YWRtaW46c29tZS1wYXNz") // admin:some-pass
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin creds must be rejected when no token: got %d %s", rec.Code, rec.Body.String())
	}
	// And with a session Bearer.
	rec = doPublish(t, a, "tv", "Bearer some-session-token")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bearer must be rejected when no token: got %d %s", rec.Code, rec.Body.String())
	}
	// No directory was created — feature is off, not "on with a default token".
	if _, err := os.Stat(filepath.Join(root, "tv", "manifest.json")); err == nil {
		t.Fatal("manifest should not have been written")
	}
}

func TestPublish_TokenConfigured_RejectsUserPassword(t *testing.T) {
	a, _ := withUpdatesApp(t, "secret-abc")
	// Basic auth is never accepted for publish; only the token is (the creds
	// are rejected without being validated).
	rec := doPublish(t, a, "tv", "Basic YWRtaW46c29tZS1wYXNz") // admin:some-pass
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPublish_TokenConfigured_RejectsMissingHeader(t *testing.T) {
	a, _ := withUpdatesApp(t, "secret-abc")
	rec := doPublish(t, a, "tv", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("token-only auth must not advertise Basic, got %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestPublish_TokenConfigured_RejectsWrongToken(t *testing.T) {
	a, _ := withUpdatesApp(t, "secret-abc")
	rec := doPublish(t, a, "tv", "Bearer wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPublish_TokenConfigured_AcceptsCorrectToken(t *testing.T) {
	const token = "s3cret-token-xyz"
	a, root := withUpdatesApp(t, token)
	rec := doPublish(t, a, "tv", "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Verify the manifest is on disk and the SHA-256 matches.
	store, err := a.updatesRegistry.Get("tv")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.Get()
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	if len(manifest.Builds) != 1 {
		t.Fatalf("expected 1 build, got %d", len(manifest.Builds))
	}
	got := manifest.Builds[0]
	want := sha256.Sum256([]byte("fake-apk-bytes-for-tests"))
	if got.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("sha256 mismatch: got %s want %s", got.SHA256, hex.EncodeToString(want[:]))
	}
	// And the file is there.
	if _, err := os.Stat(filepath.Join(root, "tv", got.Filename)); err != nil {
		t.Fatalf("build file missing: %v", err)
	}
}

func TestPublish_TokenConfigured_AcceptsLowercaseScheme(t *testing.T) {
	const token = "tok"
	a, _ := withUpdatesApp(t, token)
	rec := doPublish(t, a, "tv", "bearer "+token) // lowercase, RFC 7235 says it's valid
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPublish_EnvVarOverride(t *testing.T) {
	// The env var resolution happens in UpdatesPublishToken(). Simulate by
	// directly setting the section value the same way the config loader would
	// after seeing the env var.
	a, _ := withUpdatesApp(t, "")
	a.cfg.Updates["publish_token"] = "from-env" // stand-in for ENEVERRE_UPDATES_PUBLISH_TOKEN
	rec := doPublish(t, a, "tv", "Bearer from-env")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBearingTokenParse(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"Bearer  abc  ", "abc"},
		{"Basic Zm9vOmJhcg==", ""},
		{"abc", ""},
		{"", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
	}
	for _, tc := range cases {
		if got := bearerToken(tc.in); got != tc.want {
			t.Errorf("bearerToken(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidTrack(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"tv", true},
		{"phone", true},
		{"tablet", true},
		{"ios", true},
		{"web-beta", true},
		{"android_tv_2", true},
		{"", false},
		{"-leading-dash", false},
		{"Track", false}, // uppercase not allowed
		{"has space", false},
		{"has/slash", false},
		{"../escape", false},
		{strings.Repeat("a", 41), false}, // too long
		{strings.Repeat("a", 40), true},  // exactly at the limit
	}
	for _, tc := range cases {
		if got := validTrack(tc.in); got != tc.want {
			t.Errorf("validTrack(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}

// TestPublish_NewTrack_NoConfigChangeNeeded is the regression test for the
// actual motivating request: the server has no fixed track allowlist, so
// publishing to a brand-new track name (here "tablet", which never existed
// in any prior config or code) must work out of the box, and the manifest
// + download must both be reachable afterwards.
func TestPublish_NewTrack_NoConfigChangeNeeded(t *testing.T) {
	const token = "tok"
	a, root := withUpdatesApp(t, token)

	rec := doPublish(t, a, "tablet", "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish to new track: %d %s", rec.Code, rec.Body.String())
	}

	g := httptest.NewRequest(http.MethodGet, "/api/app/tablet/update", nil)
	gre := httptest.NewRecorder()
	a.handleAppUpdate(gre, withTrack(g, "tablet"))
	if gre.Code != http.StatusOK {
		t.Fatalf("GET new track: %d %s", gre.Code, gre.Body.String())
	}
	var body struct {
		Builds []struct {
			Filename string `json:"filename"`
			URL      string `json:"url"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(gre.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Builds) != 1 {
		t.Fatalf("expected 1 build, got %+v", body)
	}
	if _, err := os.Stat(filepath.Join(root, "tablet", body.Builds[0].Filename)); err != nil {
		t.Fatalf("build file for new track missing on disk: %v", err)
	}

	// Download it too.
	fr := httptest.NewRequest(http.MethodGet, "/api/app/updates/tablet/"+body.Builds[0].Filename, nil)
	fr.SetPathValue("filename", body.Builds[0].Filename)
	frr := httptest.NewRecorder()
	a.handleAppUpdateFile(frr, withTrack(fr, "tablet"))
	if frr.Code != http.StatusOK {
		t.Fatalf("download from new track: %d %s", frr.Code, frr.Body.String())
	}
}

func TestUpdate_InvalidTrackName_Returns400(t *testing.T) {
	a, _ := withUpdatesApp(t, "tok")
	g := httptest.NewRequest(http.MethodGet, "/api/app/../escape/update", nil)
	gre := httptest.NewRecorder()
	a.handleAppUpdate(gre, withTrack(g, "../escape"))
	if gre.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid track, got %d: %s", gre.Code, gre.Body.String())
	}
}

// quick manifest response sanity check: with a configured token, the GET
// endpoint remains anonymous — the token is publish-only.
func TestGet_RemainsAnonymousWhenTokenSet(t *testing.T) {
	a, _ := withUpdatesApp(t, "tok")
	// Publish first using the token.
	if rec := doPublish(t, a, "tv", "Bearer tok"); rec.Code != http.StatusOK {
		t.Fatalf("seed publish: %d %s", rec.Code, rec.Body.String())
	}
	// Now GET with no auth header.
	r := httptest.NewRequest(http.MethodGet, "/api/app/tv/update", nil)
	rec := httptest.NewRecorder()
	a.handleAppUpdate(rec, withTrack(r, "tv"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on anonymous GET, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	builds, _ := body["builds"].([]any)
	if len(builds) == 0 {
		t.Fatalf("expected non-empty builds array, got %s", rec.Body.String())
	}
	first, _ := builds[0].(map[string]any)
	if first["sha256"] == "" || first["sha256"] == nil {
		t.Fatal("expected sha256 in first build")
	}
	// Make sure we don't accidentally include the token in the response.
	for k, v := range body {
		vs, _ := v.(string)
		if strings.Contains(vs, "tok") {
			t.Errorf("top-level field %q leaks token-like value: %q", k, vs)
		}
	}
	for _, b := range builds {
		for k, v := range b.(map[string]any) {
			vs, _ := v.(string)
			if strings.Contains(vs, "tok") {
				t.Errorf("build field %q leaks token-like value: %q", k, vs)
			}
		}
	}
}

// doPublishLarge posts a build artifact of the given size, generated as a
// deterministic pseudorandom byte stream so the SHA-256 is predictable.
// The body is built with a real multipart.Writer so the parser exercises
// the same code path as a real CI upload.
func doPublishLarge(t *testing.T, a *App, track, authHeader string, apkSize int) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// Writer goroutine: feeds the build body and the small form fields.
	go func() {
		defer pw.Close()
		fw, err := mw.CreateFormFile("build_universal", "eneverre-tv-universal-1.0.0.apk")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		// 1 MiB chunks of a deterministic byte pattern (i * 251 mod 251)
		// so we can recompute the SHA-256 from a small generator in the
		// caller instead of allocating the whole payload in memory.
		const chunk = 1 << 20
		buf := make([]byte, chunk)
		written := 0
		for written < apkSize {
			n := chunk
			if apkSize-written < n {
				n = apkSize - written
			}
			for j := 0; j < n; j++ {
				buf[j] = byte((written + j) % 251)
			}
			if _, err := fw.Write(buf[:n]); err != nil {
				pw.CloseWithError(err)
				return
			}
			written += n
		}
		_ = mw.WriteField("versionName", "1.0.0")
		_ = mw.WriteField("versionCode", "10000")
		_ = mw.Close()
	}()

	r := httptest.NewRequest(http.MethodPost, "/api/admin/app/updates/"+track, pr)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	r.ContentLength = -1 // chunked / unknown
	rec := httptest.NewRecorder()
	a.handleAppUpdatesPublish(rec, withTrack(r, track))

	// Recompute the expected SHA-256 with the same generator (avoids
	// holding 20 MiB in memory just to assert the hash).
	h := sha256.New()
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	written := 0
	for written < apkSize {
		n := chunk
		if apkSize-written < n {
			n = apkSize - written
		}
		for j := 0; j < n; j++ {
			buf[j] = byte((written + j) % 251)
		}
		h.Write(buf[:n])
		written += n
	}
	expected := h.Sum(nil)
	return rec, expected
}

func TestPublish_LargeBuild_StreamsAndHashes(t *testing.T) {
	a, root := withUpdatesApp(t, "tok")
	const size = 20 << 20 // 20 MiB — enough to confirm streaming, fast enough to run in tests
	rec, expected := doPublishLarge(t, a, "tv", "Bearer tok", size)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK       bool     `json:"ok"`
		Builds   int      `json:"builds"`
		Variants []string `json:"variants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Builds != 1 || len(body.Variants) != 1 || body.Variants[0] != "universal" {
		t.Fatalf("unexpected body: %+v", body)
	}
	// Look up the build in the on-disk manifest and verify the hash.
	store, err := a.updatesRegistry.Get("tv")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Builds) != 1 {
		t.Fatalf("expected 1 build on disk, got %d", len(manifest.Builds))
	}
	got := manifest.Builds[0]
	if got.SHA256 != hex.EncodeToString(expected) {
		t.Fatalf("sha256 mismatch: got %s want %s", got.SHA256, hex.EncodeToString(expected))
	}
	// The file on disk must be the full size and have the same hash.
	fp := filepath.Join(root, "tv", got.Filename)
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("build file not on disk: %v", err)
	}
	if fi.Size() != int64(size) {
		t.Fatalf("size mismatch: got %d want %d", fi.Size(), size)
	}
	f, err := os.Open(fp)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	diskHash := sha256.New()
	if _, err := io.Copy(diskHash, f); err != nil {
		t.Fatal(err)
	}
	if disk := hex.EncodeToString(diskHash.Sum(nil)); disk != got.SHA256 {
		t.Fatalf("disk hash != manifest hash: %s vs %s", disk, got.SHA256)
	}
	// No temp file should have been left behind.
	if matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "eneverre-upload-*.build.tmp")); len(matches) > 0 {
		t.Errorf("temp file leaked: %v", matches)
	}
}

func TestPublish_OverMaxSize_Returns413(t *testing.T) {
	a, _ := withUpdatesApp(t, "tok")
	// Cap at a tiny value so we don't have to allocate 500 MiB.
	a.cfg.Updates["max_build_size"] = "1024" // 1 KiB
	// Send 64 KiB — well over the cap.
	rec, _ := doPublishLarge(t, a, "tv", "Bearer tok", 64*1024)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPublish_MaxSizeAcceptsUnits(t *testing.T) {
	a, _ := withUpdatesApp(t, "tok")
	a.cfg.Updates["max_build_size"] = "1K" // 1024 bytes
	// Send 2 KiB — should be rejected.
	rec, _ := doPublishLarge(t, a, "tv", "Bearer tok", 2*1024)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
	// 512 bytes — should be accepted (under the 1K cap).
	rec = doPublish(t, a, "tv", "Bearer tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("small payload should be accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublish_MultiVariant_EndToEnd(t *testing.T) {
	a, root := withUpdatesApp(t, "tok")

	// Build a multipart body with three build_<variant> files, in a
	// deterministic order. Iterating a Go map would be random, which
	// would make the test flaky.
	type entry struct {
		variant string
		p       []byte
		name    string
	}
	parts := []entry{
		{"arm64-v8a", []byte("arm64-bytes-1234567890"), "eneverre-tv-arm64-v8a-1.0.0.apk"},
		{"armeabi-v7a", []byte("armv7-bytes"), "eneverre-tv-armeabi-v7a-1.0.0.apk"},
		{"universal", []byte("universal-bytes-X"), "eneverre-tv-universal-1.0.0.apk"},
	}
	payloads := make(map[string][]byte, len(parts))
	for _, e := range parts {
		payloads[e.variant] = e.p
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for _, e := range parts {
		fw, err := mw.CreateFormFile("build_"+e.variant, e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(e.p); err != nil {
			t.Fatal(err)
		}
	}
	_ = mw.WriteField("versionName", "1.0.0")
	_ = mw.WriteField("versionCode", "10000")
	_ = mw.WriteField("mandatory", "true")
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/admin/app/updates/tv", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	a.handleAppUpdatesPublish(rec, withTrack(r, "tv"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool     `json:"ok"`
		Builds   int      `json:"builds"`
		Variants []string `json:"variants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Builds != 3 {
		t.Fatalf("unexpected body: %+v", resp)
	}
	wantVariants := []string{"arm64-v8a", "armeabi-v7a", "universal"}
	for i, v := range resp.Variants {
		if v != wantVariants[i] {
			t.Errorf("Variants[%d]: got %q want %q", i, v, wantVariants[i])
		}
	}

	// GET returns builds array with per-variant URLs.
	g := httptest.NewRequest(http.MethodGet, "/api/app/tv/update", nil)
	gre := httptest.NewRecorder()
	a.handleAppUpdate(gre, withTrack(g, "tv"))
	if gre.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", gre.Code, gre.Body.String())
	}
	var getBody struct {
		VersionName string `json:"versionName"`
		VersionCode int    `json:"versionCode"`
		Mandatory   bool   `json:"mandatory"`
		Builds      []struct {
			Variant  string `json:"variant"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
			SHA256   string `json:"sha256"`
			URL      string `json:"url"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(gre.Body.Bytes(), &getBody); err != nil {
		t.Fatal(err)
	}
	if getBody.VersionName != "1.0.0" || getBody.VersionCode != 10000 || !getBody.Mandatory {
		t.Errorf("top-level: %+v", getBody)
	}
	if len(getBody.Builds) != 3 {
		t.Fatalf("expected 3 builds, got %d", len(getBody.Builds))
	}
	for i, b := range getBody.Builds {
		if b.Variant != wantVariants[i] {
			t.Errorf("build[%d].variant: got %q want %q", i, b.Variant, wantVariants[i])
		}
		want := sha256Hex(payloads[b.Variant])
		if b.SHA256 != want {
			t.Errorf("build[%d].sha256: got %s want %s", i, b.SHA256, want)
		}
		if b.Size != int64(len(payloads[b.Variant])) {
			t.Errorf("build[%d].size: got %d want %d", i, b.Size, len(payloads[b.Variant]))
		}
		expectedURL := "/api/app/updates/tv/" + b.Filename
		if !strings.HasSuffix(b.URL, expectedURL) {
			t.Errorf("build[%d].url: got %q, want suffix %q", i, b.URL, expectedURL)
		}
	}

	// Download each build file and verify content + hash.
	for _, b := range getBody.Builds {
		fp := filepath.Join(root, "tv", b.Filename)
		content, err := os.ReadFile(fp)
		if err != nil {
			t.Errorf("%s missing on disk: %v", b.Filename, err)
			continue
		}
		if hex.EncodeToString(sha256New(content)) != b.SHA256 {
			t.Errorf("%s: hash mismatch", b.Filename)
		}
		if !bytes.Equal(content, payloads[b.Variant]) {
			t.Errorf("%s: content mismatch", b.Filename)
		}
	}

	// Old build (not in Builds list) returns 404 from the download endpoint.
	// (We never published one, but we can verify a random unknown name is 404.)
	fr := httptest.NewRequest(http.MethodGet, "/api/app/updates/tv/never-published.apk", nil)
	fr.SetPathValue("filename", "never-published.apk")
	fr.Header.Set("Authorization", "Bearer tok")
	frr := httptest.NewRecorder()
	a.handleAppUpdateFile(frr, withTrack(fr, "tv"))
	if frr.Code != http.StatusNotFound {
		t.Errorf("unknown filename: got %d want 404", frr.Code)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256New(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// doPublishOneVariantPost sends a single variant's POST with optional
// finalize. Used to simulate the per-variant CI workflow (one POST per
// variant). versionName and versionCode are taken from the args; if
// versionCode is empty, 20000 is used (the value the multi-variant flow
// tests expect).
func doPublishOneVariantPost(t *testing.T, a *App, track, authHeader, variant, filename string, payload []byte, finalize, versionName, versionCode string) *httptest.ResponseRecorder {
	t.Helper()
	if versionName == "" {
		versionName = "2.0.0"
	}
	if versionCode == "" {
		versionCode = "20000"
	}
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("build_"+variant, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("versionName", versionName)
	_ = mw.WriteField("versionCode", versionCode)
	if finalize != "" {
		_ = mw.WriteField("finalize", finalize)
	}
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/admin/app/updates/"+track, body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	a.handleAppUpdatesPublish(rec, withTrack(r, track))
	return rec
}

func TestPublish_MultiPOST_PerVariant_Finalize(t *testing.T) {
	const token = "tok"
	a, root := withUpdatesApp(t, token)

	// POST 1: arm64, finalize=false (stage)
	rec := doPublishOneVariantPost(t, a, "tv", "Bearer "+token,
		"arm64-v8a", "eneverre-tv-arm64-2.0.0.apk", []byte("arm64-bytes"), "false", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post1: %d %s", rec.Code, rec.Body.String())
	}
	var r1 struct {
		OK               bool     `json:"ok"`
		State            string   `json:"state"`
		Builds           int      `json:"builds"`
		Variants         []string `json:"variants"`
		VariantsAppended []string `json:"variants_appended"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r1); err != nil {
		t.Fatal(err)
	}
	if r1.State != "pending" {
		t.Errorf("state: got %q want pending", r1.State)
	}
	if r1.Builds != 1 || len(r1.Variants) != 1 || r1.Variants[0] != "arm64-v8a" {
		t.Errorf("post1 unexpected: %+v", r1)
	}

	// GET should still return 204 — the staged release is invisible.
	g := httptest.NewRequest(http.MethodGet, "/api/app/tv/update", nil)
	gre := httptest.NewRecorder()
	a.handleAppUpdate(gre, withTrack(g, "tv"))
	if gre.Code != http.StatusNoContent {
		t.Fatalf("GET during pending: %d", gre.Code)
	}

	// POST 2: armv7, finalize=false (stage)
	rec = doPublishOneVariantPost(t, a, "tv", "Bearer "+token,
		"armeabi-v7a", "eneverre-tv-armv7-2.0.0.apk", []byte("armv7-bytes"), "false", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post2: %d %s", rec.Code, rec.Body.String())
	}
	var r2 struct {
		State    string   `json:"state"`
		Builds   int      `json:"builds"`
		Variants []string `json:"variants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r2); err != nil {
		t.Fatal(err)
	}
	if r2.State != "pending" {
		t.Errorf("state: got %q", r2.State)
	}
	if r2.Builds != 2 {
		t.Errorf("builds: got %d want 2", r2.Builds)
	}

	// POST 3: universal, finalize=true (commit)
	rec = doPublishOneVariantPost(t, a, "tv", "Bearer "+token,
		"universal", "eneverre-tv-universal-2.0.0.apk", []byte("universal-bytes"), "true", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post3: %d %s", rec.Code, rec.Body.String())
	}
	var r3 struct {
		State    string   `json:"state"`
		Builds   int      `json:"builds"`
		Variants []string `json:"variants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r3); err != nil {
		t.Fatal(err)
	}
	if r3.State != "committed" {
		t.Errorf("state: got %q want committed", r3.State)
	}
	if r3.Builds != 3 {
		t.Errorf("builds: got %d want 3", r3.Builds)
	}

	// GET should now return the committed manifest.
	g = httptest.NewRequest(http.MethodGet, "/api/app/tv/update", nil)
	gre = httptest.NewRecorder()
	a.handleAppUpdate(gre, withTrack(g, "tv"))
	if gre.Code != http.StatusOK {
		t.Fatalf("GET after commit: %d", gre.Code)
	}
	var manifest struct {
		VersionCode int `json:"versionCode"`
		Builds      []struct {
			Variant  string `json:"variant"`
			Filename string `json:"filename"`
			SHA256   string `json:"sha256"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(gre.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.VersionCode != 20000 || len(manifest.Builds) != 3 {
		t.Errorf("manifest: %+v", manifest)
	}
	wantVariants := []string{"arm64-v8a", "armeabi-v7a", "universal"}
	for i, b := range manifest.Builds {
		if b.Variant != wantVariants[i] {
			t.Errorf("builds[%d].variant: got %q want %q", i, b.Variant, wantVariants[i])
		}
	}

	// All three build files on disk, downloadable.
	for _, b := range manifest.Builds {
		fp := filepath.Join(root, "tv", b.Filename)
		f, err := os.Open(fp)
		if err != nil {
			t.Errorf("%s not on disk: %v", b.Filename, err)
			continue
		}
		f.Close()
	}
}

func TestPublish_FinalizeDefaultsToTrue(t *testing.T) {
	const token = "tok"
	a, _ := withUpdatesApp(t, token)
	// Send a POST without the finalize field. The single build should be
	// committed immediately (state=committed).
	rec := doPublishOneVariantPost(t, a, "tv", "Bearer "+token,
		"arm64-v8a", "eneverre-tv-arm64.apk", []byte("arm64"), "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	var r struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.State != "committed" {
		t.Errorf("state: got %q want committed (default finalize=true)", r.State)
	}
}

func TestPublish_VersionCodeMismatchStartsFresh(t *testing.T) {
	const token = "tok"
	a, _ := withUpdatesApp(t, token)
	// Stage v1 with arm64.
	rec := doPublishOneVariantPost(t, a, "tv", "Bearer "+token,
		"arm64-v8a", "eneverre-tv-arm64-1.0.0.apk", []byte("v1"), "false", "1.0.0", "10000")
	if rec.Code != http.StatusOK {
		t.Fatalf("post1: %d", rec.Code)
	}
	// New POST with v2 and a different variant. The versionCode differs,
	// so the previous active release is discarded and a new one starts.
	rec = doPublishOneVariantPost(t, a, "tv", "Bearer "+token,
		"armeabi-v7a", "eneverre-tv-armv7-2.0.0.apk", []byte("v2"), "true", "2.0.0", "20000")
	if rec.Code != http.StatusOK {
		t.Fatalf("post2: %d %s", rec.Code, rec.Body.String())
	}
	// The committed manifest should have only the v2 build.
	store, err := a.updatesRegistry.Get("tv")
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := store.Get()
	if manifest.VersionCode != 20000 {
		t.Errorf("versionCode: got %d want 20000", manifest.VersionCode)
	}
	if len(manifest.Builds) != 1 || manifest.Builds[0].Variant != "armeabi-v7a" {
		t.Errorf("builds: %+v", manifest.Builds)
	}
}

func TestPublicURLForRequest_Scheme(t *testing.T) {
	cases := []struct {
		name            string
		xForwardedProto string
		tls             bool
		xForwardedHost  string
		host            string
		want            string
	}{
		{"plain http", "", false, "", "example.com", "http://example.com"},
		{"plain https", "", true, "", "example.com", "https://example.com"},
		{"proxy says https", "https", false, "public.example.com", "internal:8080", "https://public.example.com"},
		{"proxy says http", "http", true, "", "internal", "http://internal"},
		{"xforwardedhost only", "", false, "public.example.com", "internal:8080", "http://public.example.com"},
		{"empty proto is ignored", " ", false, "", "h", "http://h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest("GET", "/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.xForwardedProto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xForwardedProto)
			}
			if tc.xForwardedHost != "" {
				r.Header.Set("X-Forwarded-Host", tc.xForwardedHost)
			}
			r.Host = tc.host
			if got := publicURLForRequest(r); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestManifestResponse_PublicBaseURL_OverridesRequest(t *testing.T) {
	m := &updates.Manifest{
		VersionName: "1.0.0",
		VersionCode: 10000,
		Builds: []updates.Build{
			{Variant: "arm64-v8a", Filename: "app-arm64.apk", Size: 100, SHA256: "abc"},
		},
	}
	// Even when the request is plain HTTP, the configured base URL
	// wins (it's the operator's authoritative source of truth).
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "internal:8080"
	out := manifestResponse(m, "tv", "https://nvr.delellis.com.ar", publicURLForRequest(r))
	if got := out.Builds[0].URL; got != "https://nvr.delellis.com.ar/api/app/updates/tv/app-arm64.apk" {
		t.Errorf("URL: got %q, want https://...", got)
	}
}

func TestManifestResponse_EmptyPublicBaseURL_FallsBackToRequest(t *testing.T) {
	m := &updates.Manifest{
		VersionName: "1.0.0", VersionCode: 10000,
		Builds: []updates.Build{{Variant: "arm64-v8a", Filename: "app.apk", Size: 1, SHA256: "x"}},
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "nvr.example.com")
	out := manifestResponse(m, "tv", "", publicURLForRequest(r))
	if got := out.Builds[0].URL; got != "https://nvr.example.com/api/app/updates/tv/app.apk" {
		t.Errorf("URL: got %q", got)
	}
}
