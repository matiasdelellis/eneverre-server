package main

import (
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
)

// startPprof serves the Go runtime profiles on ENEVERRE_PPROF (e.g.
// "127.0.0.1:6060") when set. It is a separate listener on the default mux,
// which nothing else uses, so the profiles never reach the API port. Bind it
// to loopback: the profiles expose memory contents and are unauthenticated.
func startPprof() {
	addr := os.Getenv("ENEVERRE_PPROF")
	if addr == "" {
		return
	}
	go func() {
		slog.Info("pprof listening", "addr", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			slog.Warn("pprof listener failed", "addr", addr, "err", err)
		}
	}()
}
