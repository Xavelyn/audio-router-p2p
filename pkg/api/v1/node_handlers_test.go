package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/sync"
)

func newTestV1Server(t *testing.T) (*V1Server, *http.ServeMux) {
	t.Helper()
	cfg := &config.Config{
		AuthToken:        "test-v1-token",
		ListenAddr:       ":8443",
		DiscoveryPort:    0,
		MulticastPort:    0,
		SyncPort:         0,
		StreamPort:       ":0",
		AnnounceInterval: 1 * time.Second,
		PeerTTL:          4 * time.Second,
		SyncInterval:     1 * time.Second,
	}

	routesFile := filepath.Join(t.TempDir(), "routes.json")
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	disc, err := discovery.New(cfg, "test-node-1")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}

	sm := sync.NewManager(cfg, disc)
	s := NewV1Server(cfg, rt, disc, sm, nil, nil, nil)

	mux := http.NewServeMux()
	s.MountRoutes(mux)

	return s, mux
}

func doV1Request(mux http.Handler, method, path string, body any, authHeader string) *httptest.ResponseRecorder {
	var bodyReader *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewBuffer(b)
	} else {
		bodyReader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestV1AuthenticationModalities(t *testing.T) {
	_, mux := newTestV1Server(t)

	// 1. Missing auth -> 401
	w := doV1Request(mux, http.MethodGet, "/api/v1/system/status", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", w.Code)
	}

	// 2. Bearer header -> 200
	w = doV1Request(mux, http.MethodGet, "/api/v1/system/status", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with Bearer, got %d", w.Code)
	}

	// 3. X-Auth-Token header -> 200
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	req.Header.Set("X-Auth-Token", "test-v1-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with X-Auth-Token, got %d", rec.Code)
	}

	// 4. Cookie: audio_session=<token> -> 200
	req = httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	req.AddCookie(&http.Cookie{Name: "audio_session", Value: "test-v1-token"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with cookie, got %d", rec.Code)
	}

	// 5. Query param: ?token=<token> -> 200
	req = httptest.NewRequest(http.MethodGet, "/api/v1/system/status?token=test-v1-token", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with query token, got %d", rec.Code)
	}
}

func TestV1SystemStatus(t *testing.T) {
	_, mux := newTestV1Server(t)

	w := doV1Request(mux, http.MethodGet, "/api/v1/system/status", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var status SystemStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if status.Status != "ok" {
		t.Errorf("expected status ok, got %s", status.Status)
	}
	if status.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %s", status.Version)
	}
}

func TestV1SystemPeers(t *testing.T) {
	_, mux := newTestV1Server(t)

	// GET peers
	w := doV1Request(mux, http.MethodGet, "/api/v1/system/peers", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var peers []PeerNode
	if err := json.Unmarshal(w.Body.Bytes(), &peers); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// POST peer connect
	body := ConnectPeerRequest{Addr: "192.168.1.100:7001"}
	w = doV1Request(mux, http.MethodPost, "/api/v1/system/peers", body, "Bearer test-v1-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var connected PeerNode
	if err := json.Unmarshal(w.Body.Bytes(), &connected); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if connected.Addr != "192.168.1.100:7001" {
		t.Errorf("addr mismatch: %s", connected.Addr)
	}

	// Bad POST
	w = doV1Request(mux, http.MethodPost, "/api/v1/system/peers", ConnectPeerRequest{}, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty addr, got %d", w.Code)
	}
}

func TestV1SystemClockSync(t *testing.T) {
	_, mux := newTestV1Server(t)

	w := doV1Request(mux, http.MethodGet, "/api/v1/system/clock-sync", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var syncStatus ClockSyncStatus
	if err := json.Unmarshal(w.Body.Bytes(), &syncStatus); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
}
