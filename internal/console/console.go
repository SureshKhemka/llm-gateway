// Package console serves the embedded web console [ADM-02, ADM-03]. It is a static
// client of the admin API; it holds no privileges of its own.
package console

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the console under /console/ with security headers [NFR-13].
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.StripPrefix("/console/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// Mount registers the console on mux.
func Mount(mux *http.ServeMux) {
	mux.Handle("/console/", Handler())
	mux.Handle("GET /console", http.RedirectHandler("/console/", http.StatusFound))
}
