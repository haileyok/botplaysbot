// Package web embeds the built frontend (web/dist) so the AppView binary
// serves the spectator site with no external assets.
//
// The repository ships a placeholder web/dist/index.html so bare
// `go build ./...` works without Node; `make build` runs the real Vite build
// first and the fresh output is embedded from disk.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist
var distFS embed.FS

// Handler serves the embedded frontend build. Unknown paths fall back to
// index.html so client-side routes work on deep links.
func Handler() http.Handler {
	dist, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Cannot fail: "dist" is a directory in the embedded FS by construction.
		panic(err)
	}
	fileServer := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			// Not a file in the build: serve the SPA entrypoint.
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}
