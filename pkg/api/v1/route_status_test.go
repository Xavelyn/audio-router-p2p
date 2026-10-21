package v1

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
)

type statusTestManager struct {
	status RouteStatus
	found  bool
}

func (m *statusTestManager) StartRoute(route router.StreamRoute) error { return nil }
func (m *statusTestManager) StopRoute(routeID string) error            { return nil }
func (m *statusTestManager) Stats() map[string]streamer.StreamerStats  { return nil }
func (m *statusTestManager) RouteStatus(routeID string) (RouteStatus, bool) {
	if m.found && m.status.RouteID == routeID {
		return m.status, true
	}
	return RouteStatus{}, false
}

func TestRouteStatusEndpoint(t *testing.T) {
	mgr := &statusTestManager{
		found: true,
		status: RouteStatus{
			RouteID:   "diag-1",
			Direction: "receive",
			Playing:   false,
			Summary:   "no audio received from any peer yet",
			Reasons:   []string{"no audio received from any peer yet"},
			Peers: []RoutePeerStatus{{
				Address:           "10.0.0.2:45678",
				PeerID:            "node-b",
				Role:              "receive",
				PacketsLost:       3,
				FECRecovered:      2,
				JitterDepthFrames: 12,
			}},
		},
	}

	cfg := &config.Config{AuthToken: "test-v1-token"}
	rt, err := router.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	s := NewV1Server(cfg, rt, nil, nil, mgr, nil, nil)
	mux := http.NewServeMux()
	s.MountRoutes(mux)

	// Healthy route status.
	w := doV1Request(mux, http.MethodGet, "/api/v1/routes/diag-1/status", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var st RouteStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.RouteID != "diag-1" || st.Playing || st.Summary == "" {
		t.Errorf("unexpected status payload: %+v", st)
	}
	if len(st.Peers) != 1 || st.Peers[0].PeerID != "node-b" || st.Peers[0].FECRecovered != 2 {
		t.Errorf("unexpected peers: %+v", st.Peers)
	}

	// Unknown route.
	w = doV1Request(mux, http.MethodGet, "/api/v1/routes/nope/status", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown route, got %d", w.Code)
	}

	// Wrong method.
	w = doV1Request(mux, http.MethodPost, "/api/v1/routes/diag-1/status", nil, "Bearer test-v1-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

func TestRouteStatusEndpointUnavailable(t *testing.T) {
	// A server without a status provider (e.g. read-only mode) reports 501.
	_, mux := newTestV1Server(t)
	w := doV1Request(mux, http.MethodGet, "/api/v1/routes/whatever/status", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501 without a status provider, got %d", w.Code)
	}
}
