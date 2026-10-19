package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// The web UI is a static page that talks to the same /v1 API with the
// user's token. It has no build step and no third-party code, so a strict
// CSP holds: only this origin's own scripts, styles and API.
//
//go:embed ui
var uiFiles embed.FS

const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; form-action 'none'; base-uri 'none'; frame-ancestors 'none'"

func uiRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(uiFiles, "ui")
	files := http.StripPrefix("/ui/", http.FileServerFS(sub))
	mux.Handle("GET /ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", uiCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
}
