package main

import "embed"

// staticFiles is the vanilla-JS web UI, embedded into the binary so a single
// file is enough to run. The canonical source lives in go/static/; this is
// the copy go:embed reads at build time (embed cannot reference paths outside
// the module).
//
// At runtime the server prefers an on-disk static dir when one is found
// (ENEVERRE_STATIC_DIR, or ./app/static / ../app/static relative to the
// current working directory), so live edits don't require a rebuild; the
// embedded copy is the fallback served from /. Set ENEVERRE_STATIC_DIR
// during development to iterate without rebuilding.
//
//go:embed all:static
var staticFiles embed.FS
