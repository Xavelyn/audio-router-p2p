package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func newTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<!DOCTYPE html><html><head><title>App</title></head><body>Root</body></html>"),
		},
		"assets/app-12345.js": &fstest.MapFile{
			Data: []byte("console.log('audio-router');"),
		},
		"assets/style-67890.css": &fstest.MapFile{
			Data: []byte("body { background: black; }"),
		},
	}
}

func TestHandler_SecurityHeaders(t *testing.T) {
	h := NewHandler(newTestFS())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	expectedHeaders := map[string]string{
		"Content-Security-Policy": headerCSP,
		"X-Content-Type-Options":  headerContentTypeOpt,
		"X-Frame-Options":         headerFrameOpt,
		"Referrer-Policy":         headerReferrerPolicy,
	}

	for header, expected := range expectedHeaders {
		if val := rec.Header().Get(header); val != expected {
			t.Errorf("header %s: got %q, want %q", header, val, expected)
		}
	}
}

func TestHandler_RootIndex(t *testing.T) {
	h := NewHandler(newTestFS())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if body != "<!DOCTYPE html><html><head><title>App</title></head><body>Root</body></html>" {
		t.Fatalf("unexpected body: %s", body)
	}
	if ctype := rec.Header().Get("Content-Type"); ctype != "text/html; charset=utf-8" {
		t.Errorf("unexpected content-type: %s", ctype)
	}
	if cache := rec.Header().Get("Cache-Control"); cache != cacheControlNoCache {
		t.Errorf("expected no-cache, got %s", cache)
	}
	if etag := rec.Header().Get("ETag"); etag == "" {
		t.Errorf("missing ETag on index.html")
	}
}

func TestHandler_SPARouteFallback(t *testing.T) {
	h := NewHandler(newTestFS())

	routes := []string{
		"/matrix",
		"/devices",
		"/rooms/studio-a",
		"/telemetry/meters",
	}

	for _, route := range routes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, route, nil)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("route %s: expected 200, got %d", route, rec.Code)
		}
		body := rec.Body.String()
		if body != "<!DOCTYPE html><html><head><title>App</title></head><body>Root</body></html>" {
			t.Errorf("route %s: failed to fallback to index.html: %s", route, body)
		}
		if cache := rec.Header().Get("Cache-Control"); cache != cacheControlNoCache {
			t.Errorf("route %s: expected no-cache, got %s", route, cache)
		}
	}
}

func TestHandler_ImmutableAssets(t *testing.T) {
	h := NewHandler(newTestFS())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/app-12345.js", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if cache := rec.Header().Get("Cache-Control"); cache != cacheControlImmutable {
		t.Errorf("expected immutable cache control, got %s", cache)
	}
	if ctype := rec.Header().Get("Content-Type"); !strings.Contains(ctype, "javascript") {
		t.Errorf("expected javascript content-type, got %s", ctype)
	}
	if body := rec.Body.String(); body != "console.log('audio-router');" {
		t.Errorf("unexpected body: %s", body)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag for asset")
	}

	// Test 304 Not Modified
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/assets/app-12345.js", nil)
	req2.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("expected empty body on 304, got %d bytes", rec2.Body.Len())
	}
}

func TestHandler_MissingFileReturns404(t *testing.T) {
	h := NewHandler(newTestFS())

	missingPaths := []string{
		"/assets/missing.js",
		"/styles/missing.css",
		"/favicon.ico",
	}

	for _, p := range missingPaths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("path %s: expected 404, got %d", p, rec.Code)
		}
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := NewHandler(newTestFS())

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, m := range methods {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m, "/", nil)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: expected 405, got %d", m, rec.Code)
		}
	}
}

func TestHandler_HeadMethod(t *testing.T) {
	h := NewHandler(newTestFS())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected empty body for HEAD, got %d bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("unexpected content type: %s", rec.Header().Get("Content-Type"))
	}
}

func TestHandler_ConcurrencyRace(t *testing.T) {
	h := NewHandler(newTestFS())
	const workers = 20
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	paths := []string{
		"/",
		"/assets/app-12345.js",
		"/assets/style-67890.css",
		"/matrix",
		"/rooms/studio-b",
	}

	for i := 0; i < workers; i++ {
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				p := paths[(workerID+j)%len(paths)]
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, p, nil)
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("worker %d req %s failed with %d", workerID, p, rec.Code)
				}
				_, _ = io.Copy(io.Discard, rec.Body)
			}
		}(i)
	}

	wg.Wait()
}
