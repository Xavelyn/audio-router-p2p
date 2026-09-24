package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/sync"
	"github.com/prometheus/client_golang/prometheus"
)

type mockStreamManager struct {
	started  map[string]router.StreamRoute
	stopped  []string
	startErr error
	stopErr  error
}

func (m *mockStreamManager) StartRoute(route router.StreamRoute) error {
	if m.startErr != nil {
		return m.startErr
	}
	if m.started == nil {
		m.started = make(map[string]router.StreamRoute)
	}
	m.started[route.ID] = route
	return nil
}

func (m *mockStreamManager) StopRoute(routeID string) error {
	if m.stopErr != nil {
		return m.stopErr
	}
	m.stopped = append(m.stopped, routeID)
	return nil
}

var errTest = &testError{"test error"}

type testError struct{ msg string }
func (e *testError) Error() string { return e.msg }

func testConfig() *config.Config {
	return &config.Config{
		ListenAddr:       ":8443",
		CertFile:         "cert.pem",
		KeyFile:          "key.pem",
		AuthToken:        "test-token",
		UseEmulator:      true,
		MulticastGroup:   "239.255.0.1",
		MulticastPort:    45400,
		DiscoveryPort:    45401,
		SyncPort:         45402,
		StreamPort:       ":45678",
		AnnounceInterval: 3 * time.Second,
		PeerTTL:          10 * time.Second,
		SyncInterval:     2 * time.Second,
	}
}

func testRouter(t *testing.T) *router.Router {
	r, err := router.New("/tmp/test_routes_" + t.Name() + ".json")
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

func testDiscovery(t *testing.T) *discovery.Discovery {
	cfg := testConfig()
	d, err := discovery.New(cfg, "test-node")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	return d
}

func testSyncMgr(t *testing.T) *sync.Manager {
	cfg := testConfig()
	disc := testDiscovery(t)
	sm := sync.NewManager(cfg, disc)
	return sm
}

func newTestServer(t *testing.T, streams StreamManager) *Server {
	cfg := testConfig()
	rt := testRouter(t)
	disc := testDiscovery(t)
	sm := testSyncMgr(t)
	
	// Create server with custom prometheus registry to avoid duplicate registration
	s := &Server{
		cfg:     cfg,
		rt:      rt,
		disc:    disc,
		syncMgr: sm,
		streams: streams,
		mux:     http.NewServeMux(),
	}
	s.reqs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "audio_router_api_requests_total",
		Help: "Total number of API requests by method and path.",
	}, []string{"method", "path"})
	s.peerCnt = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "audio_router_peers",
		Help: "Current number of discovered peers.",
	})
	s.routesActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "audio_router_active_routes",
		Help: "Current number of active stream routes.",
	})
	// Use a custom registry for testing
	s.registry = prometheus.NewRegistry()
	s.registry.MustRegister(s.reqs, s.peerCnt, s.routesActive)
	s.routes()
	return s
}

func makeRequest(t *testing.T, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return httptest.NewRecorder()
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	w := makeRequest(t, method, path, body, token)
	req := w.Result().Request
	if req == nil {
		req = httptest.NewRequest(method, path, nil)
	}
	// Actually, let's just use the request we made
	req = httptest.NewRequest(method, path, nil)
	if body != nil {
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req = httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestServerAuthentication(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// No token provided - should fail
	w := doRequest(t, handler, http.MethodGet, "/status", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", w.Code)
	}

	// Wrong token - should fail
	w = doRequest(t, handler, http.MethodGet, "/status", nil, "wrong-token")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", w.Code)
	}

	// Correct token - should succeed
	w = doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with correct token, got %d", w.Code)
	}

	// Test X-Auth-Token header
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("X-Auth-Token", "test-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with X-Auth-Token, got %d", w.Code)
	}
}

func TestServerStatus(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if resp["status"] != "ok" {
		t.Errorf("status not ok: %v", resp["status"])
	}
	if resp["nodeID"] != "test-node" {
		t.Errorf("nodeID mismatch: %v", resp["nodeID"])
	}
	// peerCount depends on discovery state
	_ = resp["peerCount"]
	if resp["routeCount"] != float64(0) {
		t.Errorf("routeCount mismatch: %v", resp["routeCount"])
	}
}

func TestServerDevices(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/devices", nil, "test-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var devs []audio.Device
	json.NewDecoder(w.Body).Decode(&devs)
	// In noaudio mode, should return emulated devices
	if len(devs) == 0 {
		t.Log("no devices returned (expected in some environments)")
	}
}

func TestServerPeers(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/peers", nil, "test-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var peers []discovery.Peer
	json.NewDecoder(w.Body).Decode(&peers)
	// Discovery might not have peers yet
	_ = peers
}

func TestServerSync(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/sync", nil, "test-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var offsets map[string]sync.PeerOffset
	json.NewDecoder(w.Body).Decode(&offsets)
	// Sync might not have offsets yet
	_ = offsets
}

func TestServerRoutesGet(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/routes", nil, "test-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var routes []router.StreamRoute
	json.NewDecoder(w.Body).Decode(&routes)
	if len(routes) != 0 {
		t.Errorf("expected 0 routes, got %d", len(routes))
	}
}

func TestServerRoutesPost(t *testing.T) {
	msm := &mockStreamManager{}
	s := newTestServer(t, msm)
	handler := s.Handler()

	route := router.StreamRoute{
		ID:           "test-route",
		SourceDevice: 0,
		Destinations: []string{"127.0.0.1:45678"},
		SampleRate:   44100,
		Channels:     1,
	}

	w := doRequest(t, handler, http.MethodPost, "/routes", route, "test-token")

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp router.StreamRoute
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.ID != "test-route" {
		t.Errorf("route ID mismatch: %s", resp.ID)
	}

	if len(msm.started) != 1 {
		t.Errorf("stream manager not called: %d", len(msm.started))
	}
}

func TestServerRoutesPostInvalid(t *testing.T) {
	msm := &mockStreamManager{}
	s := newTestServer(t, msm)
	handler := s.Handler()

	route := router.StreamRoute{
		SourceDevice: 0,
		Destinations: []string{"127.0.0.1:45678"},
	}

	w := doRequest(t, handler, http.MethodPost, "/routes", route, "test-token")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestServerRoutesPostStreamManagerError(t *testing.T) {
	msm := &mockStreamManager{startErr: errTest}
	s := newTestServer(t, msm)
	handler := s.Handler()

	route := router.StreamRoute{
		ID:           "test-route",
		SourceDevice: 0,
		Destinations: []string{"127.0.0.1:45678"},
		SampleRate:   44100,
		Channels:     1,
	}

	w := doRequest(t, handler, http.MethodPost, "/routes", route, "test-token")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServerRoutesDelete(t *testing.T) {
	msm := &mockStreamManager{}
	s := newTestServer(t, msm)
	handler := s.Handler()

	// First add a route via the server's router (need to access it)
	// We can't directly access s.rt, so add via POST first
	route := router.StreamRoute{
		ID:           "to-delete",
		SourceDevice: 0,
		Destinations: []string{"127.0.0.1:45678"},
		SampleRate:   44100,
		Channels:     1,
	}
	w := doRequest(t, handler, http.MethodPost, "/routes", route, "test-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("failed to add route: %d", w.Code)
	}

	w = doRequest(t, handler, http.MethodDelete, "/routes?id=to-delete", nil, "test-token")

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	if len(msm.stopped) != 1 || msm.stopped[0] != "to-delete" {
		t.Errorf("stream manager stop not called: %v", msm.stopped)
	}
}

func TestServerRoutesDeleteMissingID(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodDelete, "/routes", nil, "test-token")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestServerRoutesDeleteNotFound(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodDelete, "/routes?id=nonexistent", nil, "test-token")

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}
}

func TestServerMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// PUT on /status
	w := doRequest(t, handler, http.MethodPut, "/status", nil, "test-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}

	// PATCH on /routes
	w = doRequest(t, handler, http.MethodPatch, "/routes", nil, "test-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestServerPanicRecovery(t *testing.T) {
	cfg := testConfig()
	rt := testRouter(t)
	disc := testDiscovery(t)
	s := NewServer(cfg, rt, disc, nil, nil)
	// Nil syncMgr no longer panics in handleStatus (3.3 mirrors handleSync),
	// so exercise the recovery middleware deterministically via a panic route.
	s.mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/boom", nil, "test-token")

	// Should recover and return 500
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on panic, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "internal error" {
		t.Errorf("unexpected error message: %v", resp)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/metrics", nil, "test-token")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /metrics, got %d", w.Code)
	}
	if len(w.Body.String()) == 0 {
		t.Error("metrics endpoint returned empty body")
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"key": "value"})

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("wrong content type: %s", w.Header().Get("Content-Type"))
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["key"] != "value" {
		t.Errorf("value mismatch: %v", resp)
	}
}

func TestConstantTimeCompare(t *testing.T) {
	token := []byte("test-token")
	if subtle.ConstantTimeCompare(token, token) != 1 {
		t.Error("constant time compare failed for equal")
	}
	if subtle.ConstantTimeCompare(token, []byte("wrong")) == 1 {
		t.Error("constant time compare should fail for different")
	}
}
func TestStatusNilSyncMgr(t *testing.T) {
	// Server without a sync manager must serve /status in a read-only mode
	// (200, syncPeers: 0) instead of dereferencing a nil syncMgr (3.3).
	cfg := testConfig()
	rt := testRouter(t)
	disc := testDiscovery(t)
	s := NewServer(cfg, rt, disc, nil, nil)
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with nil syncMgr, got %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	if got := int(body["syncPeers"].(float64)); got != 0 {
		t.Fatalf("expected syncPeers 0 with nil syncMgr, got %d", got)
	}
}
