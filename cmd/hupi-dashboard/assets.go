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

// distFS embeds the built React SPA (cmd/hupi-dashboard/web, built with
// `npm run build`, output to web/dist). A minimal placeholder
// web/dist/index.html is committed so a fresh clone's
// `go build ./cmd/hupi-dashboard` never fails with a go:embed error
// before anyone has run the npm build — install.sh and the Dockerfile
// both run `npm ci && npm run build` first, overwriting the placeholder.
// Identical mechanism to cmd/hupi-admin-ui/assets.go; see that file's
// doc comment for the full rationale.
//
//go:embed web/dist
var distFS embed.FS

var (
	spaOnce    sync.Once
	spaHandler http.Handler
)

func serveSPA(w http.ResponseWriter, r *http.Request) {
	spaOnce.Do(func() {
		sub, err := fs.Sub(distFS, "web/dist")
		if err != nil {
			slog.Error("dashboard: failed to open embedded web/dist", "error", err)
			spaHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "dashboard frontend not available", http.StatusInternalServerError)
			})
			return
		}
		spaHandler = spaFallback(sub, http.FileServer(http.FS(sub)))
	})
	spaHandler.ServeHTTP(w, r)
}

// spaFallback is identical to cmd/hupi-admin-ui/assets.go's own function
// of the same name — see that file's doc comment for why each branch
// exists. Duplicated rather than shared because sharing it would mean a
// new internal/ package two single-purpose main packages both import
// for one small function, for no real reuse benefit.
func spaFallback(fsys fs.FS, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
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
		if ext := filepath.Ext(p); ext != "" {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.Error(w, "dashboard frontend not available", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}
