package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
)

const (
	// Security headers as defined in ADR 0005
	headerCSP            = "default-src 'self'; connect-src 'self' ws: wss:; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self';"
	headerContentTypeOpt = "nosniff"
	headerFrameOpt       = "DENY"
	headerReferrerPolicy = "strict-origin-when-cross-origin"

	cacheControlImmutable = "public, max-age=31536000, immutable"
	cacheControlNoCache   = "no-cache"
)

// Handler serves static assets from an fs.FS with SPA routing fallback,
// SHA-256 ETag caching, immutable asset headers, and strict security policies.
type Handler struct {
	fsys       fs.FS
	indexHTML  []byte
	indexETag  string
	etagCache  sync.Map // map[string]string: path -> sha256 hex
}

// NewHandler initializes a static SPA handler backed by fsys.
func NewHandler(fsys fs.FS) *Handler {
	h := &Handler{
		fsys: fsys,
	}

	// Pre-load and hash index.html if available
	if fsys != nil {
		if data, err := fs.ReadFile(fsys, "index.html"); err == nil {
			h.indexHTML = data
			sum := sha256.Sum256(data)
			h.indexETag = `"` + hex.EncodeToString(sum[:]) + `"`
		}
	}

	// Fallback minimal index if not found in embedded filesystem
	if len(h.indexHTML) == 0 {
		h.indexHTML = []byte(defaultIndexHTML)
		sum := sha256.Sum256(h.indexHTML)
		h.indexETag = `"` + hex.EncodeToString(sum[:]) + `"`
	}

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Enforce hardened security headers
	w.Header().Set("Content-Security-Policy", headerCSP)
	w.Header().Set("X-Content-Type-Options", headerContentTypeOpt)
	w.Header().Set("X-Frame-Options", headerFrameOpt)
	w.Header().Set("Referrer-Policy", headerReferrerPolicy)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	urlPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if urlPath == "" || urlPath == "." {
		h.serveIndex(w, r)
		return
	}

	// 2. Attempt to open asset from filesystem
	file, err := h.fsys.Open(urlPath)
	if err != nil {
		// Not found: Check if request is targeting static asset path like assets/...
		if strings.HasPrefix(urlPath, "assets/") || path.Ext(urlPath) != "" {
			http.NotFound(w, r)
			return
		}
		// Client-side SPA route (e.g. /matrix, /devices, /rooms/room-1) -> fallback to index.html
		h.serveIndex(w, r)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		h.serveIndex(w, r)
		return
	}

	// 3. Compute/cache ETag and cache headers
	isAsset := strings.HasPrefix(urlPath, "assets/")
	if isAsset {
		w.Header().Set("Cache-Control", cacheControlImmutable)
	} else {
		w.Header().Set("Cache-Control", cacheControlNoCache)
	}

	etag := h.getOrComputeETag(urlPath, file)
	if etag != "" {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// 4. Resolve Content-Type
	ext := path.Ext(urlPath)
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		switch ext {
		case ".js", ".mjs":
			ctype = "application/javascript"
		case ".css":
			ctype = "text/css"
		case ".svg":
			ctype = "image/svg+xml"
		case ".wasm":
			ctype = "application/wasm"
		case ".json":
			ctype = "application/json"
		default:
			ctype = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", ctype)

	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, stat.Name(), stat.ModTime(), seeker)
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", cacheControlNoCache)
	w.Header().Set("ETag", h.indexETag)

	if r.Header.Get("If-None-Match") == h.indexETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(h.indexHTML)
	}
}

func (h *Handler) getOrComputeETag(urlPath string, file fs.File) string {
	if val, ok := h.etagCache.Load(urlPath); ok {
		return val.(string)
	}

	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		return ""
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, seeker); err != nil {
		_, _ = seeker.Seek(0, io.SeekStart)
		return ""
	}
	_, _ = seeker.Seek(0, io.SeekStart)

	etag := `"` + hex.EncodeToString(hasher.Sum(nil)) + `"`
	h.etagCache.Store(urlPath, etag)
	return etag
}

const defaultIndexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Audio Router P2P Control Plane</title>
  <style>
    body {
      margin: 0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: #0f172a;
      color: #f8fafc;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
    }
    .card {
      background: #1e293b;
      border: 1px solid #334155;
      border-radius: 12px;
      padding: 2.5rem;
      max-width: 520px;
      text-align: center;
      box-shadow: 0 10px 25px -5px rgba(0, 0, 0, 0.5);
    }
    h1 { margin-top: 0; font-size: 1.75rem; color: #38bdf8; }
    p { color: #94a3b8; line-height: 1.6; }
    .status {
      display: inline-block;
      margin: 1.5rem 0;
      padding: 0.5rem 1rem;
      background: #0284c7;
      color: white;
      border-radius: 9999px;
      font-weight: 600;
      font-size: 0.875rem;
    }
    .links a {
      color: #38bdf8;
      text-decoration: none;
      margin: 0 0.5rem;
    }
    .links a:hover { text-decoration: underline; }
  </style>
</head>
<body>
  <div class="card">
    <h1>Audio Router P2P</h1>
    <div class="status">System Operational &bull; OpenAPI 3.1 Ready</div>
    <p>High-performance distributed audio matrix routing with sub-millisecond P2P synchronization and lock-free telemetry streaming.</p>
    <div class="links">
      <a href="/api/v1/system/status">System Status</a> &bull;
      <a href="/api/v1/routes">Active Routes</a> &bull;
      <a href="/api/v1/devices">Devices</a> &bull;
      <a href="/metrics">Prometheus Metrics</a>
    </div>
  </div>
</body>
</html>`
