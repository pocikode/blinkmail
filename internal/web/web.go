// Package web embeds and serves the Blinkmail frontend.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// Assets returns the embedded frontend rooted at the assets directory.
func Assets() (fs.FS, error) {
	return fs.Sub(assetsFS, "assets")
}

// Handler serves the embedded frontend.
//
// index.html is returned for the root path and for any unknown path that is
// not a static asset, so the single-page client keeps working when the browser
// requests a nested route. Assets are immutable content-addressed files in
// practice, so they get a modest cache lifetime; index.html is never cached,
// because a stale shell would pin an old app.js.
func Handler() (http.Handler, error) {
	sub, err := Assets()
	if err != nil {
		return nil, err
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// Never cache the HTML shell.
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-store")
			serveIndex(w, r, sub)
			return
		}

		if _, err := fs.Stat(sub, path); err != nil {
			// Unknown path: fall back to the shell for client-side handling.
			w.Header().Set("Cache-Control", "no-store")
			serveIndex(w, r, sub)
			return
		}

		w.Header().Set("Cache-Control", "public, max-age=300")
		fileServer.ServeHTTP(w, r)
	}), nil
}

// serveIndex writes index.html.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	body, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "frontend assets unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(body)))
}
