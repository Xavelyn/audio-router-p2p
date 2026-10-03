package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist/*
var embeddedDistFS embed.FS

// mountSPAHandler attaches static asset serving with Single-Page Application (SPA) fallback.
func (s *Server) mountSPAHandler() {
	distFS, err := fs.Sub(embeddedDistFS, "dist")
	if err != nil {
		distFS = embeddedDistFS
	}
	fileServer := http.FileServer(http.FS(distFS))

	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Non-GET and non-HEAD requests that don't match other handlers are 404
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		cleanPath := path.Clean(r.URL.Path)

		// Bypass API, status, and metric paths (and their subpaths)
		if strings.HasPrefix(cleanPath, "/api") || cleanPath == "/metrics" || strings.HasPrefix(cleanPath, "/metrics/") || cleanPath == "/status" || strings.HasPrefix(cleanPath, "/status/") {
			http.NotFound(w, r)
			return
		}

		filePath := strings.TrimPrefix(cleanPath, "/")
		if filePath == "" {
			filePath = "index.html"
		}

		// Check if file exists in the embedded virtual filesystem (ignoring directories)
		if f, err := distFS.Open(filePath); err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !stat.IsDir() {
				if strings.HasPrefix(filePath, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Fallback to index.html for client-side routing
		indexBytes, err := fs.ReadFile(distFS, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(indexBytes)
		}
	})
}
