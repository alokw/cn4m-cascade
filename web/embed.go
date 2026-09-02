// Package web carries the compiled React SPA into the binary (SPEC.md §3, §9).
//
// The embed directive cannot reach outside its own package directory, which is
// why this file sits beside the Vite output rather than under internal/. The
// package exposes the build as an fs.FS and nothing else; internal/api owns
// how it is served.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

// AssetDir is the subdirectory Vite writes hashed bundles to. internal/api
// treats it as immutable and cacheable, and refuses to fall back to the shell
// for anything underneath it.
const AssetDir = "assets"

// dist holds the build output. The `all:` prefix is required twice over:
// without it embed skips files whose names begin with `.` or `_` — bundlers
// emit both — and it is also what lets the committed dist/.gitkeep satisfy the
// directive in a tree that has never run a frontend build.
//
// That placeholder is the whole reason `go build` works on a fresh checkout.
// It also means a binary can be built with no real UI in it, which is why
// Build reports that as an error rather than pretending.
//
//go:embed all:dist
var dist embed.FS

// Build returns the SPA build rooted at dist, or an error if the binary was
// compiled without a real frontend build. Callers should degrade rather than
// fail: the API is fully usable without the UI.
func Build() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("opening the embedded web build: %w", err)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, fmt.Errorf("the embedded web build has no index.html: %w", err)
	}
	return sub, nil
}
