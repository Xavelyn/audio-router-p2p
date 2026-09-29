package v1

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/telemetry"
)

type mockDiscoveryProvider struct {
	nodeID    string
	telemetry telemetry.DiscoveryHealth
	peers     []telemetry.DiscoveredPeer
}

func (m *mockDiscoveryProvider) NodeID() string {
	if m.nodeID != "" {
		return m.nodeID
	}
	return "mock-node-1"
}

func (m *mockDiscoveryProvider) Telemetry() telemetry.DiscoveryHealth {
	return m.telemetry
}

func (m *mockDiscoveryProvider) DiscoveredPeers() []telemetry.DiscoveredPeer {
	return m.peers
}

type mockClockSyncProvider struct {
	telemetry telemetry.ClockSyncHealth
	offsets   map[string]telemetry.PeerSyncOffset
}

func (m *mockClockSyncProvider) Telemetry() telemetry.ClockSyncHealth {
	return m.telemetry
}

func (m *mockClockSyncProvider) PeerOffsets() map[string]telemetry.PeerSyncOffset {
	return m.offsets
}

type mockStreamerProvider struct {
	links []telemetry.StreamLinkStats
}

func (m *mockStreamerProvider) StreamLinkStats() []telemetry.StreamLinkStats {
	return m.links
}

func TestV1NetworkHealthEndpoint_Authentication(t *testing.T) {
	_, mux := newTestV1Server(t)

	// Missing authorization header -> 401
	w := doV1Request(mux, http.MethodGet, "/api/v1/network/health", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized, got %d", w.Code)
	}

	// Invalid token -> 401
	w = doV1Request(mux, http.MethodGet, "/api/v1/network/health", nil, "Bearer invalid-token")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized, got %d", w.Code)
	}

	// Valid token -> 200
	w = doV1Request(mux, http.MethodGet, "/api/v1/network/health", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
}

func TestV1NetworkHealthEndpoint_MethodNotAllowed(t *testing.T) {
	_, mux := newTestV1Server(t)

	w := doV1Request(mux, http.MethodPost, "/api/v1/network/health", nil, "Bearer test-v1-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", w.Code)
	}
}

func TestV1NetworkHealthEndpoint_DefaultSnapshot(t *testing.T) {
	_, mux := newTestV1Server(t)

	w := doV1Request(mux, http.MethodGet, "/api/v1/network/health", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp NetworkHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal NetworkHealthResponse: %v", err)
	}

	if resp.NodeID != "test-node-1" {
		t.Errorf("node_id = %s; want test-node-1", resp.NodeID)
	}
	if resp.HealthScore < 0.0 || resp.HealthScore > 100.0 {
		t.Errorf("health_score out of range: %f", resp.HealthScore)
	}
	if resp.HealthGrade == "" {
		t.Errorf("health_grade is empty")
	}
	if resp.LatencyMatrix == nil {
		t.Errorf("latency_matrix is nil")
	}
}

func TestV1NetworkHealthEndpoint_WithAggregator(t *testing.T) {
	s, mux := newTestV1Server(t)

	disc := &mockDiscoveryProvider{
		nodeID: "node-agg-integrated",
		telemetry: telemetry.DiscoveryHealth{
			ActiveMembers:   2,
			BeaconsSent:     10,
			BeaconsReceived: 9,
		},
		peers: []telemetry.DiscoveredPeer{
			{
				ID:        "peer-bravo",
				Addr:      "192.168.1.100:45401",
				SWIMState: "alive",
				LastSeen:  time.Now(),
				RTTMS:     8.0,
			},
		},
	}
	syncP := &mockClockSyncProvider{
		telemetry: telemetry.ClockSyncHealth{
			OffsetUS:  15.0,
			DriftPPM:  0.5,
			SyncRTTUS: 4000.0,
			IsMaster:  false,
		},
		offsets: map[string]telemetry.PeerSyncOffset{
			"peer-bravo": {
				OffsetUS:   15.0,
				DriftPPM:   0.5,
				RTTUS:      4000.0,
				LastUpdate: time.Now(),
			},
		},
	}
	streamP := &mockStreamerProvider{
		links: []telemetry.StreamLinkStats{
			{
				StreamID:            "stream-audio-1",
				PeerID:              "peer-bravo",
				PacketsSent:         500,
				PacketsReceived:     498,
				LossRatio:           0.004,
				EffectiveKbps:       1411.2,
				JitterDepthFrames:   2,
				JitterTargetDelayMS: 15.0,
				FECRecovered:        2,
			},
		},
	}

	agg := telemetry.NewAggregator(telemetry.Config{
		NodeID:    "node-agg-integrated",
		Discovery: disc,
		ClockSync: syncP,
		Streamer:  streamP,
	})

	s.SetAggregator(agg)

	w := doV1Request(mux, http.MethodGet, "/api/v1/network/health", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp NetworkHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal NetworkHealthResponse: %v", err)
	}

	if resp.NodeID != "node-agg-integrated" {
		t.Errorf("node_id = %s; want node-agg-integrated", resp.NodeID)
	}
	if resp.HealthScore <= 0.0 || resp.HealthScore > 100.0 {
		t.Errorf("health_score unexpected: %f", resp.HealthScore)
	}
	if resp.HealthGrade != telemetry.GradeExcellent && resp.HealthGrade != telemetry.GradeGood {
		t.Errorf("health_grade unexpected: %s", resp.HealthGrade)
	}
	if peer, ok := resp.Peers["peer-bravo"]; !ok {
		t.Errorf("missing peer-bravo in peers map")
	} else {
		if peer.Streaming.StreamID != "stream-audio-1" {
			t.Errorf("peer streaming stream_id = %s; want stream-audio-1", peer.Streaming.StreamID)
		}
	}
	if resp.Cluster.ActiveMembers != 2 {
		t.Errorf("cluster active members = %d; want 2", resp.Cluster.ActiveMembers)
	}
	if resp.Sync.OffsetUS != 15.0 {
		t.Errorf("sync offset_us = %f; want 15.0", resp.Sync.OffsetUS)
	}
}
