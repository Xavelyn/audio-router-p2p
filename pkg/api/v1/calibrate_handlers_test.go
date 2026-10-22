package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/sync"
)

// mockCalibrationStreams is a StreamManager that also implements the route
// calibration and alignment interfaces, for handler tests. Running routes
// are the keys of the map; "receive-only" is running but not calibratable.
type mockCalibrationStreams struct {
	routes map[string]bool
}

func newMockCalibrationStreams(running ...string) *mockCalibrationStreams {
	m := &mockCalibrationStreams{routes: make(map[string]bool)}
	for _, id := range running {
		m.routes[id] = true
	}
	return m
}

func (m *mockCalibrationStreams) StartRoute(route router.StreamRoute) error {
	m.routes[route.ID] = true
	return nil
}

func (m *mockCalibrationStreams) StopRoute(routeID string) error {
	delete(m.routes, routeID)
	return nil
}

func (m *mockCalibrationStreams) Stats() map[string]streamer.StreamerStats { return nil }

func (m *mockCalibrationStreams) CalibrateRoute(routeID string, req CalibrateRequest) (CalibrateResult, bool, error) {
	if !m.routes[routeID] {
		return CalibrateResult{}, false, nil
	}
	if routeID == "receive-only" {
		return CalibrateResult{}, true, ErrRouteNotCalibratable
	}
	return CalibrateResult{
		RouteID:         routeID,
		Detected:        true,
		DelayMS:         12.5,
		ChannelDelaysMS: []float64{10, 15},
		OffsetsMS:       []float64{5, 0},
		FreqHz:          req.FreqHz,
		DurationMS:      req.DurationMS,
	}, true, nil
}

func (m *mockCalibrationStreams) SetRouteAlignment(routeID string, offsetsMS []float64) ([]float64, bool, error) {
	if !m.routes[routeID] {
		return nil, false, nil
	}
	if len(offsetsMS) != 2 {
		return nil, true, fmt.Errorf("%w: want 2 offsets, got %d", ErrInvalidAlignment, len(offsetsMS))
	}
	for _, v := range offsetsMS {
		if v < 0 || v > 1000 {
			return nil, true, fmt.Errorf("%w: offset %g out of range [0, 1000]", ErrInvalidAlignment, v)
		}
	}
	return offsetsMS, true, nil
}

func (m *mockCalibrationStreams) GetRouteAlignment(routeID string) ([]float64, bool) {
	if m.routes[routeID] {
		return []float64{5, 0}, true
	}
	return nil, false
}

// newCalibrationTestServer builds a v1 server with the given stream manager.
func newCalibrationTestServer(t *testing.T, streams StreamManager) (*V1Server, *http.ServeMux) {
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
	s := NewV1Server(cfg, rt, disc, sm, streams, nil, nil)

	mux := http.NewServeMux()
	s.MountRoutes(mux)
	return s, mux
}

const calibAuth = "Bearer test-v1-token"

func TestV1RouteCalibrate(t *testing.T) {
	streams := newMockCalibrationStreams("local-1", "receive-only")
	_, mux := newCalibrationTestServer(t, streams)

	// Empty body: probe defaults.
	var res CalibrateResult
	w := doV1Request(mux, http.MethodPost, "/api/v1/routes/local-1/calibrate", nil, calibAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Detected || res.RouteID != "local-1" || len(res.ChannelDelaysMS) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Invalid probe parameters → 400.
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes/local-1/calibrate",
		map[string]any{"freq_hz": 50}, calibAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad freq, got %d: %s", w.Code, w.Body.String())
	}
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes/local-1/calibrate",
		map[string]any{"duration_ms": 9999}, calibAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad duration, got %d: %s", w.Code, w.Body.String())
	}

	// Route not running → 404.
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes/missing/calibrate", nil, calibAuth)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Running but receive-only → 501.
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes/receive-only/calibrate", nil, calibAuth)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", w.Code, w.Body.String())
	}

	// Wrong method → 405.
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/local-1/calibrate", nil, calibAuth)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestV1RouteAlignment(t *testing.T) {
	streams := newMockCalibrationStreams("local-1")
	s, mux := newCalibrationTestServer(t, streams)

	// Persisted offsets for a route that is not running.
	if err := s.router.Add(router.StreamRoute{ID: "persisted", ChannelDelaysMS: []float64{1, 2}}); err != nil {
		t.Fatalf("router.Add: %v", err)
	}

	// PUT on a running route.
	var res AlignmentInfo
	w := doV1Request(mux, http.MethodPut, "/api/v1/routes/local-1/alignment",
		UpdateAlignmentRequest{OffsetsMS: []float64{10, 20}}, calibAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.OffsetsMS) != 2 || res.OffsetsMS[0] != 10 || res.OffsetsMS[1] != 20 {
		t.Fatalf("unexpected offsets: %+v", res)
	}

	// Wrong length → 400.
	w = doV1Request(mux, http.MethodPut, "/api/v1/routes/local-1/alignment",
		UpdateAlignmentRequest{OffsetsMS: []float64{1}}, calibAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short offsets, got %d: %s", w.Code, w.Body.String())
	}

	// Out of range → 400.
	w = doV1Request(mux, http.MethodPut, "/api/v1/routes/local-1/alignment",
		UpdateAlignmentRequest{OffsetsMS: []float64{0, 2000}}, calibAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range offsets, got %d: %s", w.Code, w.Body.String())
	}

	// Route not running → 404.
	w = doV1Request(mux, http.MethodPut, "/api/v1/routes/missing/alignment",
		UpdateAlignmentRequest{OffsetsMS: []float64{0, 0}}, calibAuth)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// GET on a running route reports the live offsets.
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/local-1/alignment", nil, calibAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	res = AlignmentInfo{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.OffsetsMS) != 2 || res.OffsetsMS[0] != 5 || res.OffsetsMS[1] != 0 {
		t.Fatalf("unexpected live offsets: %+v", res)
	}

	// GET on a non-running route falls back to the persisted values.
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/persisted/alignment", nil, calibAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	res = AlignmentInfo{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.OffsetsMS) != 2 || res.OffsetsMS[0] != 1 || res.OffsetsMS[1] != 2 {
		t.Fatalf("unexpected persisted offsets: %+v", res)
	}

	// Unknown route → 404.
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/missing/alignment", nil, calibAuth)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Wrong method → 405.
	w = doV1Request(mux, http.MethodDelete, "/api/v1/routes/local-1/alignment", nil, calibAuth)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}
