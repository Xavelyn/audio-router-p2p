package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestNewEndpointsAuthEnforcement tests that all newly introduced endpoints
// (route status, calibration, alignment, room latency, and network simulation)
// strictly enforce authentication when AuthToken is configured.
func TestNewEndpointsAuthEnforcement(t *testing.T) {
	cfg := &config.Config{AuthToken: "secret-v1-token"}
	rt, err := router.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	udp, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sim := transport.NewNetworkSimulator(udp, transport.SimProfile{})
	defer sim.Close()

	calibStreams := newMockCalibrationStreams("route-1")
	s := NewV1Server(cfg, rt, nil, nil, calibStreams, nil, nil)
	s.SetSimulator(sim)

	mux := http.NewServeMux()
	s.MountRoutes(mux)

	endpoints := []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodGet, path: "/api/v1/routes/route-1/status", body: nil},
		{method: http.MethodPost, path: "/api/v1/routes/route-1/calibrate", body: nil},
		{method: http.MethodGet, path: "/api/v1/routes/route-1/alignment", body: nil},
		{method: http.MethodPut, path: "/api/v1/routes/route-1/alignment", body: UpdateAlignmentRequest{OffsetsMS: []float64{0, 0}}},
		{method: http.MethodPut, path: "/api/v1/rooms/room-1/latency", body: UpdateRoomLatencyRequest{Preset: "low"}},
		{method: http.MethodGet, path: "/api/v1/network/sim", body: nil},
		{method: http.MethodPut, path: "/api/v1/network/sim", body: transport.SimProfiles{}},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			// 1. Missing Authorization header -> 401 Unauthorized
			w := doV1Request(mux, ep.method, ep.path, ep.body, "")
			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 without auth, got %d: %s", w.Code, w.Body.String())
			}

			// 2. Invalid Bearer token -> 401 Unauthorized
			w = doV1Request(mux, ep.method, ep.path, ep.body, "Bearer wrong-token")
			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 with invalid token, got %d: %s", w.Code, w.Body.String())
			}

			// 3. Valid Bearer token -> Passes authentication (status != 401)
			w = doV1Request(mux, ep.method, ep.path, ep.body, "Bearer secret-v1-token")
			if w.Code == http.StatusUnauthorized {
				t.Errorf("got 401 with valid Bearer token")
			}

			// 4. Valid X-Auth-Token header -> Passes authentication (status != 401)
			req, _ := http.NewRequest(ep.method, ep.path, nil)
			req.Header.Set("X-Auth-Token", "secret-v1-token")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("got 401 with valid X-Auth-Token header")
			}

			// 5. Valid query parameter token -> Passes authentication (status != 401)
			req, _ = http.NewRequest(ep.method, ep.path+"?token=secret-v1-token", nil)
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("got 401 with valid token query parameter")
			}
		})
	}
}
