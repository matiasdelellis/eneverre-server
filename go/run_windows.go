//go:build windows

package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/svc"
)

// resolveLogWriter picks where logs go on Windows. When launched by the
// Service Control Manager there is no console attached, so if ENEVERRE_LOG_FILE
// is set we append to it (the installer sets it to <DataDir>\eneverre.log);
// this is where the one-time first-run admin password ends up. Run from a
// console — or with the var unset — logs stay on stderr.
//
// The file handle is intentionally not closed: logWriter is a package-level
// io.Writer that lives until the process exits, and the slog handler that
// wraps it is the only writer for every log line (including the
// graceful-shutdown path that finalizes recordings).
func resolveLogWriter() io.Writer {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return os.Stderr
	}
	path := strings.TrimSpace(os.Getenv("ENEVERRE_LOG_FILE"))
	if path == "" {
		return os.Stderr
	}
	// MkdirAll so a fresh data dir doesn't fail just because eneverre.log's
	// parent hasn't been touched yet (the installer creates the dir, but a
	// user running the binary by hand may not have).
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// The logger isn't installed yet, so write directly to stderr. Under
		// the SCM stderr is discarded, but at least an interactive run
		// surfaces the misconfiguration. The structured log handler will
		// log the same error once it's wired up, so this is purely a
		// bootstrap hint.
		fmt.Fprintf(os.Stderr, "eneverre: could not open ENEVERRE_LOG_FILE=%q: %v\n", path, err)
		return os.Stderr
	}
	return f
}

// runServer serves until shutdown. Under the Service Control Manager it runs as
// a real Windows service, so a `sc stop` / machine shutdown triggers the same
// graceful drain (and fMP4 segment finalization) as Ctrl+C. Run from a console
// it stops on Ctrl+C.
func runServer(srv *http.Server, cleanup func()) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if isSvc {
		return runService(srv, cleanup)
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt) // Ctrl+C / Ctrl+Break
	go func() {
		s := <-sig
		slog.Info("shutting down", "signal", s.String())
		close(stop)
	}()
	return serveAndShutdown(srv, stop, cleanup)
}

// windowsService adapts the HTTP server to the svc.Handler interface so the
// binary can run under the Service Control Manager with no external wrapper.
type windowsService struct {
	srv     *http.Server
	cleanup func()
	// result is unbuffered: svc.Run blocks until Execute returns, so the
	// read in runService happens-after the write inside Execute and a
	// rendezvous is sufficient.
	result chan error
}

// runService hands control to the SCM. svc.Run blocks until Execute returns;
// the serve error (if any) is surfaced afterwards so main can exit non-zero.
func runService(srv *http.Server, cleanup func()) error {
	h := &windowsService{srv: srv, cleanup: cleanup, result: make(chan error)}
	if err := svc.Run("eneverre", h); err != nil {
		return err
	}
	return <-h.result
}

// Execute is the SCM control loop: report Running, serve, and on a Stop/Shutdown
// control (or a fatal serve error) drain the server and finalize recordings
// while holding the SCM in StopPending so it does not hard-kill us mid-flush.
func (w *windowsService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	changes <- svc.Status{State: svc.StartPending}

	srvErr := make(chan error, 1)
	go func() {
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	slog.Info("running as a windows service")

	var runErr error
loop:
	for {
		select {
		case runErr = <-srvErr:
			break loop
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				slog.Info("shutting down", "signal", "service stop")
				break loop
			default:
				slog.Warn("unexpected service control", "cmd", uint32(c.Cmd))
			}
		}
	}

	// Ask the SCM for time while we drain HTTP and finalize the in-progress
	// fMP4 segment; WaitHint must comfortably exceed the 10s HTTP drain.
	changes <- svc.Status{State: svc.StopPending, WaitHint: 20000}
	gracefulShutdown(w.srv, w.cleanup)

	w.result <- runErr
	if runErr != nil {
		// A specific exit code (1) lets `sc query` show the failure distinctly
		// from a normal stop; the structured log line above carries the
		// underlying error (port already in use, missing cert, etc.).
		return true, 1
	}
	return false, 0
}
