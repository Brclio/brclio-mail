package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// content is compiled into the server binary so a release has no external UI
// files to keep in sync.
//
//go:embed index.html app.css app.js assets/*
var content embed.FS

// Handler serves immutable front-end assets and falls back to index.html for
// client-side routes. API routes are registered before this handler.
func Handler() http.Handler {
	files := http.FileServer(http.FS(content))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(content, name); err != nil {
			request := r.Clone(r.Context())
			request.URL.Path = "/index.html"
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, request)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
