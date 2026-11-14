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

// EmbeddedDistFS returns the embedded filesystem containing static assets.
func EmbeddedDistFS() embed.FS {
	return embeddedDistFS
}

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
				} else if filePath == "index.html" {
					w.Header().Set("Cache-Control", "no-cache")
				}
				if strings.HasSuffix(filePath, ".js") {
					w.Header().Set("Content-Type", "application/javascript")
				} else if strings.HasSuffix(filePath, ".css") {
					w.Header().Set("Content-Type", "text/css")
				} else if strings.HasSuffix(filePath, ".html") {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Missing assets under assets/ or directory accesses must return 404 rather than
		// falling back to index.html (which would cause syntax/MIME errors in browsers).
		if strings.HasPrefix(filePath, "assets/") || filePath == "assets" {
			http.NotFound(w, r)
			return
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
