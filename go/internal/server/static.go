package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// staticAsset is a precomputed embedded file: its body, an optional
// precompressed gzip body, content type, and a content-hash ETag.
type staticAsset struct {
	contentType string
	body        []byte
	gzipped     []byte // nil when gzip is not smaller / not compressible
	etag        string
}

// buildStaticAssets reads every file from the embedded UI once at startup,
// computing content type, ETag, and (for text assets) a gzip body. Serving
// from this map lets the handler answer conditional requests with 304 and
// serve precompressed bytes — neither of which http.FileServer does for an
// embed.FS (its files have a zero modtime, so no Last-Modified/ETag).
func buildStaticAssets(fsys fs.FS) map[string]staticAsset {
	assets := map[string]staticAsset{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = http.DetectContentType(body)
		}
		sum := sha256.Sum256(body)
		a := staticAsset{
			contentType: ct,
			body:        body,
			etag:        `"` + hex.EncodeToString(sum[:])[:16] + `"`,
		}
		if compressible(ct) {
			if gz := gzipBytes(body); len(gz) < len(body) {
				a.gzipped = gz
			}
		}
		assets["/"+p] = a
		return nil
	})
	return assets
}

func compressible(ct string) bool {
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg") ||
		strings.Contains(ct, "xml")
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

// serveStatic serves the embedded UI with ETag-based revalidation (304) and
// gzip when the client accepts it. Cache-Control is "no-cache" so the browser
// always revalidates with If-None-Match — correct across redeploys (no
// content-hashed filenames) while the 304 still avoids re-downloading ~550KB.
func (a *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "/" || p == "" {
		p = "/index.html"
	}
	asset, ok := a.assets[p]
	if !ok {
		http.NotFound(w, r)
		return
	}

	useGzip := asset.gzipped != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	etag := asset.etag
	if useGzip {
		// Distinct representation → distinct validator.
		etag = strings.TrimSuffix(asset.etag, `"`) + `-gzip"`
	}

	h := w.Header()
	h.Set("Content-Type", asset.contentType)
	h.Set("Cache-Control", a.staticCacheControl)
	h.Set("ETag", etag)
	if asset.gzipped != nil {
		h.Add("Vary", "Accept-Encoding")
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := asset.body
	if useGzip {
		h.Set("Content-Encoding", "gzip")
		body = asset.gzipped
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// etagMatches reports whether the If-None-Match header (a comma-separated list,
// or "*") contains the given ETag.
func etagMatches(header, etag string) bool {
	for _, tok := range strings.Split(header, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "*" || tok == etag {
			return true
		}
	}
	return false
}
