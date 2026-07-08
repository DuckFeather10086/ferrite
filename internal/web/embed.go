// Package web embeds the static web UI bundle under dist/ and exposes
// it as an fs.FS for the api router to mount.
//
// dist/ holds the output of a Next.js (Bun) static export run via
// `bun run build` in the ferrite/web/ directory. The build step calls
// `next build` (output: 'export') and copies out/ → internal/web/dist/.
// `go build` re-embeds whatever is in dist/ — no runtime Node needed.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded frontend as a subtree rooted at dist/.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
