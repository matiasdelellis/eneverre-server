package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// serveAndShutdown starts srv in the background and blocks until stop is
// closed or the server fails. On stop it drains the server and runs cleanup.
// It returns a non-nil error only when the HTTP server itself failed — in that
// case cleanup is deliberately left to the caller's fatal path, matching the
// previous behaviour where a serve error skipped the graceful close.
//
// It is shared by the Unix and (interactive) Windows shutdown paths; the
// Windows service handler drives the same sequence via gracefulShutdown so it
// can interleave the serve loop with the Service Control Manager's requests.
func serveAndShutdown(srv *http.Server, stop <-chan struct{}, cleanup func()) error {
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	select {
	case err := <-srvErr:
		return err
	case <-stop:
	}

	gracefulShutdown(srv, cleanup)
	return nil
}

// gracefulShutdown drains in-flight HTTP requests (bounded by a 10s timeout)
// and then runs cleanup, which finalizes the media engine's open segments and
// closes the database.
func gracefulShutdown(srv *http.Server, cleanup func()) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown incomplete", "err", err)
	}
	cleanup()
}
