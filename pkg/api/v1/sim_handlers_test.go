package v1

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestNetworkSimEndpointUnavailable verifies read-only servers (no UDP
// simulator attached) report 501.
func TestNetworkSimEndpointUnavailable(t *testing.T) {
	_, mux := newTestV1Server(t)
	w := doV1Request(mux, http.MethodGet, "/api/v1/network/sim", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 without a simulator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNetworkSimEndpoint(t *testing.T) {
	udp, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sim := transport.NewNetworkSimulator(udp, transport.SimProfile{})
	defer sim.Close()

	cfg := &config.Config{AuthToken: "test-v1-token"}
	rt, err := router.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewV1Server(cfg, rt, nil, nil, nil, nil, nil)
	s.SetSimulator(sim)
	mux := http.NewServeMux()
	s.MountRoutes(mux)

	// GET: both profiles start disabled.
	w := doV1Request(mux, http.MethodGet, "/api/v1/network/sim", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var profiles transport.SimProfiles
	if err := json.Unmarshal(w.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if profiles.In.Enabled || profiles.Out.Enabled {
		t.Errorf("profiles should start disabled: %+v", profiles)
	}

	// PUT: update both directions; out-of-range values are clamped.
	w = doV1Request(mux, http.MethodPut, "/api/v1/network/sim", transport.SimProfiles{
		In:  transport.SimProfile{Enabled: true, LossPct: 15, ReorderPct: 101},
		Out: transport.SimProfile{Enabled: true, JitterMS: 12, LatencyMS: -5, BandwidthKbps: 500},
	}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on PUT, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if !profiles.In.Enabled || profiles.In.LossPct != 15 || profiles.In.ReorderPct != 100 {
		t.Errorf("ingress profile not applied/clamped: %+v", profiles.In)
	}
	if !profiles.Out.Enabled || profiles.Out.JitterMS != 12 || profiles.Out.LatencyMS != 0 || profiles.Out.BandwidthKbps != 500 {
		t.Errorf("egress profile not applied/clamped: %+v", profiles.Out)
	}

	// GET reflects the stored state.
	w = doV1Request(mux, http.MethodGet, "/api/v1/network/sim", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after PUT, got %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if profiles.Out.JitterMS != 12 {
		t.Errorf("GET does not reflect PUT: %+v", profiles)
	}

	// Wrong method.
	w = doV1Request(mux, http.MethodPost, "/api/v1/network/sim", nil, "Bearer test-v1-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}
