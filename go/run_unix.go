//go:build !windows

package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// resolveLogWriter keeps logs on stderr on non-Windows platforms (systemd,
// launchd and the like capture stderr into the journal / log files).
func resolveLogWriter() io.Writer { return os.Stderr }

// runServer serves until SIGINT/SIGTERM (e.g. `systemctl stop`), then drains
// the HTTP server and runs cleanup.
func runServer(srv *http.Server, cleanup func()) error {
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		slog.Info("shutting down", "signal", s.String())
		close(stop)
	}()
	return serveAndShutdown(srv, stop, cleanup)
}
