// Package web embeds the static web UI bundle under dist/ and exposes
// it as an fs.FS for the api router to mount.
//
// dist/ currently holds a hand-written, no-build single-page app
// (index.html + app.js + styles.css, plain fetch + hls.js from CDN).
// It needs no toolchain — edit the files and `go build` re-embeds them.
// If a framework build is ever preferred, point its static export at
// this dir (e.g. Next.js `output: 'export'`, distDir
// '../internal/web/dist') and the embed picks it up unchanged.
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
