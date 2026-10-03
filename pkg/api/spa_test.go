package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestSPAFallbackAndStaticAssetServing(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	t.Run("Root path serves index.html", func(t *testing.T) {
		w := doRequest(t, handler, http.MethodGet, "/", nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200 for root, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "<!DOCTYPE html>") && !strings.Contains(body, "<html") {
			t.Errorf("expected HTML content in root response, got: %s", body)
		}
	})

	t.Run("Deep SPA routes fallback to index.html with 200", func(t *testing.T) {
		deepRoutes := []string{
			"/rooms",
			"/matrix",
			"/telemetry",
			"/devices",
			"/rooms/studio-a",
			"/matrix/crosspoint/42",
		}
		for _, r := range deepRoutes {
			w := doRequest(t, handler, http.MethodGet, r, nil, "")
			if w.Code != http.StatusOK {
				t.Errorf("expected status 200 for deep route %s, got %d", r, w.Code)
			}
			contentType := w.Header().Get("Content-Type")
			if !strings.Contains(contentType, "text/html") {
				t.Errorf("expected text/html for deep route %s, got %s", r, contentType)
			}
			cacheControl := w.Header().Get("Cache-Control")
			if cacheControl != "no-cache" {
				t.Errorf("expected Cache-Control: no-cache, got %s", cacheControl)
			}
			if !strings.Contains(w.Body.String(), "<html") {
				t.Errorf("expected index.html body for deep route %s", r)
			}
		}
	})

	t.Run("Non-GET and non-HEAD methods on SPA paths return 404", func(t *testing.T) {
		w := doRequest(t, handler, http.MethodPost, "/rooms", map[string]string{"foo": "bar"}, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("expected status 404 for POST /rooms, got %d", w.Code)
		}

		w = doRequest(t, handler, http.MethodDelete, "/matrix", nil, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("expected status 404 for DELETE /matrix, got %d", w.Code)
		}
	})

	t.Run("API, status, and metrics routes are not swallowed by SPA fallback", func(t *testing.T) {
		// /status should be JSON
		w := doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for /status, got %d", w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("expected application/json for /status, got %s", w.Header().Get("Content-Type"))
		}

		// /metrics should be Prometheus metrics
		w = doRequest(t, handler, http.MethodGet, "/metrics", nil, "")
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for /metrics, got %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "<html") {
			t.Errorf("/metrics returned HTML instead of Prometheus metrics")
		}

		// /api/v1/routes should be handled by API
		w = doRequest(t, handler, http.MethodGet, "/api/v1/routes", nil, "test-token")
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for /api/v1/routes, got %d", w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("expected application/json for /api/v1/routes, got %s", w.Header().Get("Content-Type"))
		}
	})

	t.Run("HEAD requests are supported on SPA routes", func(t *testing.T) {
		w := doRequest(t, handler, http.MethodHead, "/rooms", nil, "")
		if w.Code != http.StatusOK {
			t.Errorf("expected status 200 for HEAD /rooms, got %d", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("expected empty body for HEAD, got %d bytes", w.Body.Len())
		}
	})

	t.Run("Status and metrics subpaths return 404 and are not swallowed by SPA", func(t *testing.T) {
		unmatchedBypasses := []string{
			"/status/",
			"/status/subpath",
			"/metrics/",
			"/metrics/subpath",
			"/api/nonexistent",
		}
		for _, p := range unmatchedBypasses {
			w := doRequest(t, handler, http.MethodGet, p, nil, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("expected 404 for %s, got %d", p, w.Code)
			}
		}
	})
}
