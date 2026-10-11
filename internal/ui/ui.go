// Package ui serves the web interface. It is plain HTML, CSS and JavaScript
// modules embedded in the binary, so building vigil needs nothing but Go.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var files embed.FS

// Handler serves the UI. The files themselves are public; every API call
// they make is authenticated by the server.
func Handler() http.Handler {
	static, _ := fs.Sub(files, "static")
	fileServer := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Files only: a directory answers with a list of what is in it.
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
