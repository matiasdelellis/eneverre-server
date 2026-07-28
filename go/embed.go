package main

import "embed"

// staticFiles is the vanilla-JS web UI, embedded into the binary so a single
// file is enough to run. The canonical source lives in go/static/; this is
// the copy go:embed reads at build time (embed cannot reference paths outside
// the module).
//
// At runtime the embedded copy is what gets served from / — unless
// ENEVERRE_STATIC_DIR points at a directory on disk, which takes precedence so
// live UI edits don't require a rebuild (`ENEVERRE_STATIC_DIR=go/static`).
// That env var is the only override; the server never guesses a static dir
// from the working directory.
//
//go:embed all:static
var staticFiles embed.FS
