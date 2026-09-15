package main

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// distFS embeds the built React SPA (cmd/hupi-admin-ui/web, built with
// `npm run build`, output to web/dist). `web/dist` is not itself source
// code — see web/README.md and docs/ADMIN_UI.md for the full story — but
// `go:embed` requires the directory to exist at compile time, so a
// minimal placeholder web/dist/index.html is committed so a fresh clone's
// `go build ./cmd/hupi-admin-ui` never fails with a go:embed error before
// anyone has run the npm build. `install.sh` and the Dockerfile both run
// `npm ci && npm run build` before building this binary, which overwrites
// that placeholder with the real app.
//
//go:embed web/dist
var distFS embed.FS

// spaFS is fs.Sub(distFS, "web/dist") wrapped in an http.FileServer, plus
// SPA-fallback logic: if the requested path doesn't exist as a static
// file, index.html is served instead so React Router's client-side routes
// (e.g. a hard refresh on /users/alice) resolve correctly. serveSPA
// deliberately sits outside requireOperatorAuth (see main.go's run()) — a
// login screen has to load before there's any credential to send, so
// nothing under here can require auth the way /api/* does.
var (
	spaOnce    sync.Once
	spaHandler http.Handler
)

func serveSPA(w http.ResponseWriter, r *http.Request) {
	spaOnce.Do(func() {
		sub, err := fs.Sub(distFS, "web/dist")
		if err != nil {
			// Can't happen with a valid embed directive, but fail loudly
			// rather than serving nothing if it ever does.
			slog.Error("admin UI: failed to open embedded web/dist", "error", err)
			spaHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "admin UI frontend not available", http.StatusInternalServerError)
			})
			return
		}
		spaHandler = spaFallback(sub, http.FileServer(http.FS(sub)))
	})
	spaHandler.ServeHTTP(w, r)
}

// spaFallback serves static files out of fsys via next, falling back to
// index.html for any path that doesn't correspond to a real file — the
// standard client-side-routing fallback every SPA hosted this way needs.
// Only GET/HEAD requests for paths without a file extension are treated
// as routes; anything else that 404s (e.g. a genuinely missing asset)
// still 404s rather than masking the problem as an HTML response.
func spaFallback(fsys fs.FS, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			// Root path: http.FileServer already serves index.html for
			// "/" on its own. Handled separately from the fs.Stat check
			// below because "." (fs.FS's name for its own root) isn't a
			// real file and, worse, filepath.Ext(".") == "." — which
			// would otherwise trip the "looks like a missing asset"
			// 404 branch further down for every request to "/".
			next.ServeHTTP(w, r)
			return
		}
		if info, err := fs.Stat(fsys, p); err == nil && !info.IsDir() {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		// Not a real file: hand the router index.html instead of a 404,
		// unless it clearly asked for a static asset by extension (a
		// missing hashed JS/CSS bundle should 404, not silently return
		// HTML that then fails to parse as a script).
		if ext := filepath.Ext(p); ext != "" {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.Error(w, "admin UI frontend not available", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}
