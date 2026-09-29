package metrics

import (
	"time"

	"github.com/audio-router-p2p/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

// SnapshotProvider supplies atomic network health snapshots for Prometheus scraping.
type SnapshotProvider interface {
	GetHealthSnapshot() *telemetry.NetworkHealthSnapshot
}

// NetworkMetricsCollector implements prometheus.Collector to expose network health,
// connection telemetry, and node statistics per ADR 0010.
type NetworkMetricsCollector struct {
	provider SnapshotProvider

	nodeHealthScoreDesc *prometheus.Desc
	clusterNodesDesc    *prometheus.Desc
	lanBeaconsDesc      *prometheus.Desc
	tokenRejectionsDesc *prometheus.Desc
	seedJoinsDesc       *prometheus.Desc
	clockOffsetDesc     *prometheus.Desc
	clockDriftDesc      *prometheus.Desc
	clockIsMasterDesc   *prometheus.Desc

	peerHealthScoreDesc *prometheus.Desc
	peerRTTDesc         *prometheus.Desc
	peerLastSeenDesc    *prometheus.Desc
	peerLossRatioDesc   *prometheus.Desc
	peerBitrateDesc     *prometheus.Desc

	jbDepthDesc       *prometheus.Desc
	jbTargetDelayDesc *prometheus.Desc
	underrunsDesc     *prometheus.Desc
	overrunsDesc      *prometheus.Desc
	fecRecoveredDesc  *prometheus.Desc
	plcConcealedDesc  *prometheus.Desc
}

// NewNetworkMetricsCollector creates and initializes a NetworkMetricsCollector.
func NewNetworkMetricsCollector(provider SnapshotProvider) *NetworkMetricsCollector {
	return &NetworkMetricsCollector{
		provider: provider,

		nodeHealthScoreDesc: prometheus.NewDesc(
			"audio_router_network_node_health_score",
			"Composite 0-100 overall node health score.",
			nil, nil,
		),
		clusterNodesDesc: prometheus.NewDesc(
			"audio_router_network_cluster_nodes",
			"Current count of cluster member nodes by SWIM status.",
			[]string{"state"}, nil,
		),
		lanBeaconsDesc: prometheus.NewDesc(
			"audio_router_network_lan_beacons_total",
			"Total zero-conf LAN discovery UDP beacons sent and received.",
			[]string{"direction"}, nil,
		),
		tokenRejectionsDesc: prometheus.NewDesc(
			"audio_router_network_token_rejections_total",
			"Total peer authorization token rejection events.",
			nil, nil,
		),
		seedJoinsDesc: prometheus.NewDesc(
			"audio_router_network_seed_joins_total",
			"Total seed node join attempts and successful joins.",
			[]string{"status"}, nil,
		),
		clockOffsetDesc: prometheus.NewDesc(
			"audio_router_network_clock_offset_microseconds",
			"Estimated clock phase offset in microseconds.",
			nil, nil,
		),
		clockDriftDesc: prometheus.NewDesc(
			"audio_router_network_clock_drift_ppm",
			"Measured clock frequency drift in parts-per-million.",
			nil, nil,
		),
		clockIsMasterDesc: prometheus.NewDesc(
			"audio_router_network_clock_is_master",
			"Whether this node acts as the clock synchronization master (1 or 0).",
			nil, nil,
		),

		peerHealthScoreDesc: prometheus.NewDesc(
			"audio_router_peer_health_score",
			"Composite 0-100 peer link health score.",
			[]string{"peer_id"}, nil,
		),
		peerRTTDesc: prometheus.NewDesc(
			"audio_router_peer_rtt_seconds",
			"Round-trip latency to peer in seconds.",
			[]string{"peer_id"}, nil,
		),
		peerLastSeenDesc: prometheus.NewDesc(
			"audio_router_peer_last_seen_seconds",
			"Elapsed seconds since last packet or beacon seen from peer.",
			[]string{"peer_id"}, nil,
		),
		peerLossRatioDesc: prometheus.NewDesc(
			"audio_router_peer_packet_loss_ratio",
			"Real-time moving average packet loss ratio for peer.",
			[]string{"peer_id"}, nil,
		),
		peerBitrateDesc: prometheus.NewDesc(
			"audio_router_peer_bitrate_kbps",
			"Calculated effective media throughput in kilobits per second.",
			[]string{"peer_id"}, nil,
		),

		jbDepthDesc: prometheus.NewDesc(
			"audio_router_stream_jitter_buffer_depth_frames",
			"Instantaneous jitter buffer depth in frames.",
			[]string{"stream_id", "peer_id"}, nil,
		),
		jbTargetDelayDesc: prometheus.NewDesc(
			"audio_router_stream_jitter_target_delay_ms",
			"Adaptive jitter buffer target playout delay in milliseconds.",
			[]string{"stream_id", "peer_id"}, nil,
		),
		underrunsDesc: prometheus.NewDesc(
			"audio_router_stream_underruns_total",
			"Total jitter buffer buffer starvation underrun events.",
			[]string{"stream_id", "peer_id"}, nil,
		),
		overrunsDesc: prometheus.NewDesc(
			"audio_router_stream_overruns_total",
			"Total jitter buffer overflow overrun events.",
			[]string{"stream_id", "peer_id"}, nil,
		),
		fecRecoveredDesc: prometheus.NewDesc(
			"audio_router_stream_fec_recovered_packets_total",
			"Total packets recovered using forward error correction.",
			[]string{"stream_id"}, nil,
		),
		plcConcealedDesc: prometheus.NewDesc(
			"audio_router_stream_plc_concealed_frames_total",
			"Total missing audio frames concealed via PSOLA/noise synthesis.",
			[]string{"stream_id"}, nil,
		),
	}
}

// Describe sends all metric descriptions to the Prometheus descriptor channel.
func (c *NetworkMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.nodeHealthScoreDesc
	ch <- c.clusterNodesDesc
	ch <- c.lanBeaconsDesc
	ch <- c.tokenRejectionsDesc
	ch <- c.seedJoinsDesc
	ch <- c.clockOffsetDesc
	ch <- c.clockDriftDesc
	ch <- c.clockIsMasterDesc
	ch <- c.peerHealthScoreDesc
	ch <- c.peerRTTDesc
	ch <- c.peerLastSeenDesc
	ch <- c.peerLossRatioDesc
	ch <- c.peerBitrateDesc
	ch <- c.jbDepthDesc
	ch <- c.jbTargetDelayDesc
	ch <- c.underrunsDesc
	ch <- c.overrunsDesc
	ch <- c.fecRecoveredDesc
	ch <- c.plcConcealedDesc
}

// Collect reads the latest immutable snapshot and emits Prometheus metrics.
func (c *NetworkMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	if c.provider == nil {
		return
	}
	snap := c.provider.GetHealthSnapshot()
	if snap == nil {
		return
	}

	// 1. Overall Node Health Score
	ch <- prometheus.MustNewConstMetric(c.nodeHealthScoreDesc, prometheus.GaugeValue, snap.HealthScore)

	// 2. Cluster Membership Nodes by State
	ch <- prometheus.MustNewConstMetric(c.clusterNodesDesc, prometheus.GaugeValue, float64(snap.Cluster.ActiveMembers), "healthy")
	ch <- prometheus.MustNewConstMetric(c.clusterNodesDesc, prometheus.GaugeValue, float64(snap.Cluster.SuspectMembers), "suspect")
	ch <- prometheus.MustNewConstMetric(c.clusterNodesDesc, prometheus.GaugeValue, float64(snap.Cluster.DeadMembers), "dead")

	// 3. LAN Beacons
	ch <- prometheus.MustNewConstMetric(c.lanBeaconsDesc, prometheus.CounterValue, float64(snap.Cluster.BeaconsSent), "sent")
	ch <- prometheus.MustNewConstMetric(c.lanBeaconsDesc, prometheus.CounterValue, float64(snap.Cluster.BeaconsReceived), "recv")

	// 4. Token Rejections
	ch <- prometheus.MustNewConstMetric(c.tokenRejectionsDesc, prometheus.CounterValue, float64(snap.Cluster.TokenRejections))

	// 5. Seed Joins
	ch <- prometheus.MustNewConstMetric(c.seedJoinsDesc, prometheus.CounterValue, float64(snap.Cluster.SeedJoinAttempts), "attempt")
	ch <- prometheus.MustNewConstMetric(c.seedJoinsDesc, prometheus.CounterValue, float64(snap.Cluster.SeedJoinSuccesses), "success")

	// 6. Clock Sync
	ch <- prometheus.MustNewConstMetric(c.clockOffsetDesc, prometheus.GaugeValue, snap.Sync.OffsetUS)
	ch <- prometheus.MustNewConstMetric(c.clockDriftDesc, prometheus.GaugeValue, snap.Sync.DriftPPM)
	isMasterVal := 0.0
	if snap.Sync.IsMaster {
		isMasterVal = 1.0
	}
	ch <- prometheus.MustNewConstMetric(c.clockIsMasterDesc, prometheus.GaugeValue, isMasterVal)

	now := time.Now()

	// 7. Per-Peer Metrics
	for peerID, peer := range snap.Peers {
		ch <- prometheus.MustNewConstMetric(c.peerHealthScoreDesc, prometheus.GaugeValue, peer.HealthScore, peerID)
		ch <- prometheus.MustNewConstMetric(c.peerRTTDesc, prometheus.GaugeValue, peer.RTTMS/1000.0, peerID)

		lastSeenSec := 0.0
		if !peer.LastSeen.IsZero() {
			lastSeenSec = now.Sub(peer.LastSeen).Seconds()
			if lastSeenSec < 0 {
				lastSeenSec = 0
			}
		}
		ch <- prometheus.MustNewConstMetric(c.peerLastSeenDesc, prometheus.GaugeValue, lastSeenSec, peerID)
		ch <- prometheus.MustNewConstMetric(c.peerLossRatioDesc, prometheus.GaugeValue, peer.LossRatio, peerID)
		ch <- prometheus.MustNewConstMetric(c.peerBitrateDesc, prometheus.GaugeValue, peer.BitrateKbps, peerID)

		// Per-Peer Stream Metrics
		st := peer.Streaming
		streamID := st.StreamID
		if streamID == "" {
			streamID = "default"
		}
		ch <- prometheus.MustNewConstMetric(c.jbDepthDesc, prometheus.GaugeValue, float64(st.JitterDepthFrames), streamID, peerID)
		ch <- prometheus.MustNewConstMetric(c.jbTargetDelayDesc, prometheus.GaugeValue, st.JitterTargetDelayMS, streamID, peerID)
		ch <- prometheus.MustNewConstMetric(c.underrunsDesc, prometheus.CounterValue, float64(st.Underruns), streamID, peerID)
		ch <- prometheus.MustNewConstMetric(c.overrunsDesc, prometheus.CounterValue, float64(st.Overruns), streamID, peerID)
	}

	// 8. Stream Summary Metrics
	for streamID, st := range snap.Streams {
		ch <- prometheus.MustNewConstMetric(c.fecRecoveredDesc, prometheus.CounterValue, float64(st.FECRecovered), streamID)
		ch <- prometheus.MustNewConstMetric(c.plcConcealedDesc, prometheus.CounterValue, float64(st.PLCConcealed), streamID)
	}
}
