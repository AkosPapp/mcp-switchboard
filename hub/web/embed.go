// Package web carries the built console into the binary.
//
// It is nothing but the embed directive and a way to read it: the serving rules
// live in internal/console, which takes an fs.FS so they can be tested without
// a build of the console sitting on disk.
//
// dist/ is committed for the same reason it is embedded - `go build` has to
// work without npm, for `go install`, for the Nix build and for anyone who
// checks the repo out to fix one line of Go. CI rebuilds it and fails if the
// committed copy is stale (spec.md U1).
package web

import (
	"embed"
	"io/fs"
)

// all: includes files Go would otherwise skip, which matters because Vite emits
// dotfiles and directories starting with an underscore.
//
//go:embed all:dist
var dist embed.FS

// FS returns the console's files, rooted so that "index.html" is at the top.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: the path is a constant and the embed above guarantees it
		// exists, or the build fails.
		panic(err)
	}
	return sub
}
