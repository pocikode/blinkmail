// Package web embeds and serves the Blinkmail frontend.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
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
// It uses two cache policies. index.html is served with no-store, because a
// stale shell would keep pointing at an old client. The real assets (app.js,
// styles.css) are served as immutable for a year, which is only safe because
// the shell references them as /app.js?v=<hash> and /styles.css?v=<hash>: the
// hash covers the whole embedded asset set, so a release changes the URL and
// the browser fetches the new file. Without that suffix Cloudflare's four-hour
// edge TTL would pin the previous app.js under an unchanging URL long after a
// deploy.
func Handler() (http.Handler, error) {
	sub, err := Assets()
	if err != nil {
		return nil, err
	}
	return handlerFor(sub)
}

// handlerFor builds the frontend handler over sub. The asset version is
// computed once here, so every request sees the same suffix.
func handlerFor(sub fs.FS) (http.Handler, error) {
	version, err := assetVersion(sub)
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
			serveIndex(w, r, sub, version)
			return
		}

		if _, err := fs.Stat(sub, path); err != nil {
			// Unknown path: fall back to the shell for client-side handling.
			w.Header().Set("Cache-Control", "no-store")
			serveIndex(w, r, sub, version)
			return
		}

		// Safe to keep for a year: the URL carries the asset version.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	}), nil
}

// assetVersion returns a short digest of every regular file in sub, feeding
// name + "\x00" + content in sorted-name order into a sha256. The same asset
// set always yields the same string; changing any byte changes it.
func assetVersion(sub fs.FS) (string, error) {
	var names []string
	err := fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		names = append(names, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)

	sum := sha256.New()
	for _, name := range names {
		body, err := fs.ReadFile(sub, name)
		if err != nil {
			return "", err
		}
		sum.Write([]byte(name))
		sum.Write([]byte{0})
		sum.Write(body)
	}

	return hex.EncodeToString(sum.Sum(nil))[:10], nil
}

// serveIndex writes index.html with its asset URLs pinned to version.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS, version string) {
	body, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "frontend assets unavailable", http.StatusInternalServerError)
		return
	}

	// The shell is ours, so a plain replacement is enough and the strings are
	// unique in it.
	html := strings.ReplaceAll(string(body), "/app.js", "/app.js?v="+version)
	html = strings.ReplaceAll(html, "/styles.css", "/styles.css?v="+version)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(html))
}
