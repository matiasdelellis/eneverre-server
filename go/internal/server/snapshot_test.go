package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetchSnapshotRejectsActiveContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	}))
	defer srv.Close()

	if _, _, err := fetchSnapshot(context.Background(), srv.URL); err == nil {
		t.Fatal("image/svg+xml was accepted")
	}
}

func TestFetchSnapshotDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpeg"))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	if _, _, err := fetchSnapshot(context.Background(), redirector.URL); err == nil {
		t.Fatal("redirect was followed")
	}
	if targetHits.Load() != 0 {
		t.Error("redirect target was fetched")
	}
}

func TestFetchSnapshotAcceptsRasterImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg; charset=binary")
		_, _ = w.Write([]byte("jpeg"))
	}))
	defer srv.Close()

	body, ct, err := fetchSnapshot(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("JPEG rejected: %v", err)
	}
	if ct != "image/jpeg" || string(body) != "jpeg" {
		t.Errorf("body/ct = %q/%q", body, ct)
	}
}

func TestRedactURLStripsCredentials(t *testing.T) {
	got := redactURL("http://user:pass@camera.local/snap.jpg?token=secret")
	if strings.Contains(got, "user") || strings.Contains(got, "pass") || strings.Contains(got, "secret") {
		t.Errorf("redactURL left credentials: %s", got)
	}
	if got != "http://camera.local/snap.jpg" {
		t.Errorf("redactURL = %q", got)
	}
}
