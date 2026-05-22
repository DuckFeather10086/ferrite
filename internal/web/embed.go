// Package web embeds the Next.js static export bundle.
//
// Build flow (Next.js side):
//   cd web && npm run build   # next build with output: 'export'
//                             # distDir: '../internal/web/dist'
//
// This package then exposes the dist tree as an fs.FS for api to mount.
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
