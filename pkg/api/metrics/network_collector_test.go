package metrics

import (
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type mockSnapshotProvider struct {
	snapshot *telemetry.NetworkHealthSnapshot
}

func (m *mockSnapshotProvider) GetHealthSnapshot() *telemetry.NetworkHealthSnapshot {
	return m.snapshot
}

func TestNetworkMetricsCollector_Describe(t *testing.T) {
	provider := &mockSnapshotProvider{}
	collector := NewNetworkMetricsCollector(provider)

	ch := make(chan *prometheus.Desc, 32)
	collector.Describe(ch)
	close(ch)

	var count int
	for range ch {
		count++
	}

	const expectedDescs = 19
	if count != expectedDescs {
		t.Fatalf("expected %d metric descriptors, got %d", expectedDescs, count)
	}
}

func TestNetworkMetricsCollector_Collect_NilSnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	provider := &mockSnapshotProvider{snapshot: nil}
	collector := NewNetworkMetricsCollector(provider)
	if err := reg.Register(collector); err != nil {
		t.Fatalf("register collector: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) != 0 {
		t.Fatalf("expected 0 metric families on nil snapshot, got %d", len(families))
	}
}

func TestNetworkMetricsCollector_Collect_All19Metrics(t *testing.T) {
	now := time.Now()
	snap := &telemetry.NetworkHealthSnapshot{
		NodeID:      "test-node-collector",
		Timestamp:   now,
		HealthScore: 92.5,
		HealthGrade: telemetry.GradeGood,
		Cluster: telemetry.DiscoveryHealth{
			ActiveMembers:     2,
			SuspectMembers:    1,
			DeadMembers:       0,
			BeaconsSent:       120,
			BeaconsReceived:   115,
			TokenRejections:   2,
			SeedJoinAttempts:  3,
			SeedJoinSuccesses: 3,
		},
		Sync: telemetry.ClockSyncHealth{
			OffsetUS:    8.5,
			DriftPPM:    -0.3,
			IsMaster:    true,
			PacketsSent: 250,
			PacketsLost: 2,
		},
		Peers: map[string]telemetry.PeerLinkHealth{
			"peer-node-1": {
				PeerID:          "peer-node-1",
				Address:         "10.0.0.2:45401",
				HealthScore:     95.0,
				HealthGrade:     telemetry.GradeExcellent,
				SWIMState:       "alive",
				RTTMS:           12.0,
				LastSeen:        now.Add(-50 * time.Millisecond),
				LossRatio:       0.002,
				BitrateKbps:     1411.2,
				Streaming: telemetry.StreamLinkStats{
					StreamID:            "stream-101",
					PeerID:              "peer-node-1",
					JitterDepthFrames:   3,
					JitterTargetDelayMS: 20.0,
					Underruns:           1,
					Overruns:            0,
				},
			},
		},
		Streams: map[string]telemetry.StreamLinkStats{
			"stream-101": {
				StreamID:     "stream-101",
				FECRecovered: 4,
				PLCConcealed: 1,
			},
		},
	}

	provider := &mockSnapshotProvider{snapshot: snap}
	collector := NewNetworkMetricsCollector(provider)

	reg := prometheus.NewRegistry()
	if err := reg.Register(collector); err != nil {
		t.Fatalf("register collector: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	familyMap := make(map[string]*dto.MetricFamily)
	for _, fam := range families {
		familyMap[fam.GetName()] = fam
	}

	required19Metrics := []string{
		"audio_router_network_node_health_score",
		"audio_router_network_cluster_nodes",
		"audio_router_network_lan_beacons_total",
		"audio_router_network_token_rejections_total",
		"audio_router_network_seed_joins_total",
		"audio_router_network_clock_offset_microseconds",
		"audio_router_network_clock_drift_ppm",
		"audio_router_network_clock_is_master",
		"audio_router_peer_health_score",
		"audio_router_peer_rtt_seconds",
		"audio_router_peer_last_seen_seconds",
		"audio_router_peer_packet_loss_ratio",
		"audio_router_peer_bitrate_kbps",
		"audio_router_stream_jitter_buffer_depth_frames",
		"audio_router_stream_jitter_target_delay_ms",
		"audio_router_stream_underruns_total",
		"audio_router_stream_overruns_total",
		"audio_router_stream_fec_recovered_packets_total",
		"audio_router_stream_plc_concealed_frames_total",
	}

	for _, metricName := range required19Metrics {
		fam, ok := familyMap[metricName]
		if !ok {
			t.Errorf("missing required metric family: %s", metricName)
			continue
		}
		if len(fam.GetMetric()) == 0 {
			t.Errorf("metric family %s has no samples", metricName)
		}
	}

	// Verify specific metric values
	if fam := familyMap["audio_router_network_node_health_score"]; fam != nil {
		val := fam.GetMetric()[0].GetGauge().GetValue()
		if val != 92.5 {
			t.Errorf("node health score = %f; want 92.5", val)
		}
	}
	if fam := familyMap["audio_router_network_clock_is_master"]; fam != nil {
		val := fam.GetMetric()[0].GetGauge().GetValue()
		if val != 1.0 {
			t.Errorf("clock is master = %f; want 1.0", val)
		}
	}
	if fam := familyMap["audio_router_peer_health_score"]; fam != nil {
		val := fam.GetMetric()[0].GetGauge().GetValue()
		if val != 95.0 {
			t.Errorf("peer health score = %f; want 95.0", val)
		}
	}
	if fam := familyMap["audio_router_peer_rtt_seconds"]; fam != nil {
		val := fam.GetMetric()[0].GetGauge().GetValue()
		if val != 0.012 {
			t.Errorf("peer rtt seconds = %f; want 0.012", val)
		}
	}
	if fam := familyMap["audio_router_stream_fec_recovered_packets_total"]; fam != nil {
		val := fam.GetMetric()[0].GetCounter().GetValue()
		if val != 4.0 {
			t.Errorf("fec recovered = %f; want 4.0", val)
		}
	}
	if fam := familyMap["audio_router_stream_plc_concealed_frames_total"]; fam != nil {
		val := fam.GetMetric()[0].GetCounter().GetValue()
		if val != 1.0 {
			t.Errorf("plc concealed = %f; want 1.0", val)
		}
	}
}
