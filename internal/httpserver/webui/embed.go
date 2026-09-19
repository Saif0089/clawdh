// Package webui embeds clawdh's static web UI directly into the binary,
// so the whole tool ships as one file with no separate assets to
// install or go missing.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler serves the embedded UI at "/".
//
// Every response is marked no-cache. These files are embedded, so they change
// exactly when clawdh updates itself — and an updated service answering a
// browser that kept yesterday's app.js is a broken page (a stale script against
// a new API). The files are small and served over loopback, so re-fetching them
// on every load costs nothing and guarantees the page always matches the
// service behind it. (The hosted panel is built with hashed asset names and
// needs none of this.)
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // unreachable: "static" is embedded above
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		files.ServeHTTP(w, r)
	})
}
