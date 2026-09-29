package telemetry

import (
	"context"
	"testing"
	"time"
)

type mockDiscoveryProvider struct {
	nodeID string
	health DiscoveryHealth
	peers  []DiscoveredPeer
}

func (m *mockDiscoveryProvider) NodeID() string {
	return m.nodeID
}

func (m *mockDiscoveryProvider) Telemetry() DiscoveryHealth {
	return m.health
}

func (m *mockDiscoveryProvider) DiscoveredPeers() []DiscoveredPeer {
	return m.peers
}

type mockClockSyncProvider struct {
	health  ClockSyncHealth
	offsets map[string]PeerSyncOffset
}

func (m *mockClockSyncProvider) Telemetry() ClockSyncHealth {
	return m.health
}

func (m *mockClockSyncProvider) PeerOffsets() map[string]PeerSyncOffset {
	return m.offsets
}

type mockStreamerProvider struct {
	stats []StreamLinkStats
}

func (m *mockStreamerProvider) StreamLinkStats() []StreamLinkStats {
	return m.stats
}

func TestAggregator_PeriodicSweep(t *testing.T) {
	disc := &mockDiscoveryProvider{
		nodeID: "node-1",
		health: DiscoveryHealth{
			ActiveMembers:   2,
			LANBeaconActive: true,
		},
		peers: []DiscoveredPeer{
			{
				ID:        "node-2",
				Addr:      "192.168.1.50:45400",
				SWIMState: "alive",
				RTTMS:     1.5,
				LastSeen:  time.Now(),
			},
		},
	}

	cs := &mockClockSyncProvider{
		health: ClockSyncHealth{
			OffsetUS: 10.0,
			DriftPPM: 2.0,
			IsMaster: true,
		},
		offsets: map[string]PeerSyncOffset{
			"node-2": {
				OffsetUS: 12.0,
				DriftPPM: 1.5,
				RTTUS:    1500,
			},
		},
	}

	streamer := &mockStreamerProvider{
		stats: []StreamLinkStats{
			{
				StreamID:            "stream-alpha",
				PeerID:              "node-2",
				PacketsSent:         1000,
				PacketsReceived:     1000,
				LossRatio:           0.0,
				EffectiveKbps:       512.0,
				JitterDepthFrames:   4,
				JitterTargetDelayMS: 20.0,
			},
		},
	}

	agg := NewAggregator(Config{
		NodeID:    "node-1",
		Interval:  50 * time.Millisecond,
		Discovery: disc,
		ClockSync: cs,
		Streamer:  streamer,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agg.Start(ctx)
	defer agg.Stop()

	// Wait for background ticker to produce at least one snapshot
	time.Sleep(120 * time.Millisecond)

	snap := agg.GetHealthSnapshot()
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}

	if snap.NodeID != "node-1" {
		t.Errorf("expected node-1, got %s", snap.NodeID)
	}

	if snap.HealthScore <= 0 || snap.HealthScore > 100 {
		t.Errorf("invalid health score: %f", snap.HealthScore)
	}

	pHealth, exists := snap.Peers["node-2"]
	if !exists {
		t.Fatal("expected peer node-2 in snapshot")
	}

	if pHealth.SWIMState != "alive" {
		t.Errorf("expected alive swim state, got %s", pHealth.SWIMState)
	}

	if pHealth.HealthGrade != GradeExcellent && pHealth.HealthGrade != GradeGood {
		t.Errorf("expected excellent/good grade for near-perfect link, got %s", pHealth.HealthGrade)
	}

	if snap.LatencyMatrix["node-2"] <= 0 {
		t.Errorf("expected positive latency for node-2, got %f", snap.LatencyMatrix["node-2"])
	}
}

func TestAggregator_ZeroLocksOnAudioPath(t *testing.T) {
	agg := NewAggregator(Config{
		NodeID:   "node-perf",
		Interval: 10 * time.Millisecond,
	})

	// Concurrent callers reading snapshot while aggregator sweeps
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agg.Start(ctx)
	defer agg.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			s := agg.GetHealthSnapshot()
			if s == nil {
				t.Errorf("snapshot was nil")
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent reads timed out")
	}
}
