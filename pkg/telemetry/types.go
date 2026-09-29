package telemetry

import "time"

// NodeHealthGrade categorizes composite health scores into operational tiers.
type NodeHealthGrade string

const (
	GradeExcellent NodeHealthGrade = "EXCELLENT" // Score >= 90
	GradeGood      NodeHealthGrade = "GOOD"      // Score >= 75
	GradeFair      NodeHealthGrade = "FAIR"      // Score >= 50
	GradePoor      NodeHealthGrade = "POOR"      // Score >= 25
	GradeCritical  NodeHealthGrade = "CRITICAL"  // Score < 25
)

// DiscoveryHealth captures cluster membership and LAN bootstrap metrics.
type DiscoveryHealth struct {
	ActiveMembers     int    `json:"active_members"`
	SuspectMembers    int    `json:"suspect_members"`
	DeadMembers       int    `json:"dead_members"`
	LANBeaconActive   bool   `json:"lan_beacon_active"`
	BeaconsSent       uint64 `json:"beacons_sent"`
	BeaconsReceived   uint64 `json:"beacons_received"`
	TokenRejections   uint64 `json:"token_rejections"`
	SeedRetryActive   bool   `json:"seed_retry_active"`
	SeedJoinAttempts  uint64 `json:"seed_join_attempts"`
	SeedJoinSuccesses uint64 `json:"seed_join_successes"`
}

// ClockSyncHealth captures clock synchronization telemetry.
type ClockSyncHealth struct {
	OffsetUS    float64 `json:"offset_us"`    // Clock phase offset in microseconds
	DriftPPM    float64 `json:"drift_ppm"`    // Clock frequency drift in parts-per-million
	SyncRTTUS   float64 `json:"sync_rtt_us"`  // Sync round-trip time in microseconds
	PacketsSent uint64  `json:"packets_sent"` // Total sync requests sent
	PacketsLost uint64  `json:"packets_lost"` // Total timed-out sync requests
	IsMaster    bool    `json:"is_master"`    // True if node acts as clock sync master
}

// StreamLinkStats captures media transport metrics for an audio stream.
type StreamLinkStats struct {
	StreamID            string  `json:"stream_id"`
	PeerID              string  `json:"peer_id,omitempty"`
	PacketsSent         uint64  `json:"packets_sent"`
	PacketsReceived     uint64  `json:"packets_received"`
	PacketsLost         uint64  `json:"packets_lost"`
	LossRatio           float64 `json:"loss_ratio"`
	EffectiveKbps       float64 `json:"effective_kbps"`
	JitterDepthFrames   int     `json:"jitter_depth_frames"`
	JitterTargetDelayMS float64 `json:"jitter_target_delay_ms"`
	Underruns           uint64  `json:"underruns"`
	Overruns            uint64  `json:"overruns"`
	FECRecovered        uint64  `json:"fec_recovered"`
	PLCConcealed        uint64  `json:"plc_concealed"`
}

// PeerLinkHealth captures composite telemetry for a specific peer link.
type PeerLinkHealth struct {
	PeerID           string          `json:"peer_id"`
	Address          string          `json:"address"`
	HealthScore      float64         `json:"health_score"`
	HealthGrade      NodeHealthGrade `json:"health_grade"`
	SWIMState        string          `json:"swim_state"` // alive, suspect, dead
	DirectPingFailed bool            `json:"direct_ping_failed"`
	RTTMS            float64         `json:"rtt_ms"`
	LastSeen         time.Time       `json:"last_seen"`
	LossRatio        float64         `json:"loss_ratio"`
	BitrateKbps      float64         `json:"bitrate_kbps"`
	Streaming        StreamLinkStats `json:"streaming"`
	Sync             ClockSyncHealth `json:"sync"`
}

// NetworkHealthSnapshot provides an atomic point-in-time view of network health.
type NetworkHealthSnapshot struct {
	NodeID          string                    `json:"node_id"`
	Timestamp       time.Time                 `json:"timestamp"`
	HealthScore     float64                   `json:"health_score"`
	HealthGrade     NodeHealthGrade           `json:"health_grade"`
	Cluster         DiscoveryHealth           `json:"cluster"`
	Sync            ClockSyncHealth           `json:"sync"`
	LatencyMatrix   map[string]float64        `json:"latency_matrix"` // peer_id -> RTT in ms
	Peers           map[string]PeerLinkHealth `json:"peers"`
	Streams         map[string]StreamLinkStats`json:"streams,omitempty"`
	LocalUnderruns  uint64                    `json:"local_underruns"`
}

// NetworkHealthFrame represents a compact frame emitted over SSE / WebSocket channels.
type NetworkHealthFrame struct {
	NodeID        string             `json:"node_id"`
	TimestampNS   int64              `json:"timestamp_ns"`
	HealthScore   float64            `json:"health_score"`
	HealthGrade   NodeHealthGrade    `json:"health_grade"`
	ActiveMembers int                `json:"active_members"`
	LatencyMatrix map[string]float64 `json:"latency_matrix"`
	PeerScores    map[string]float64 `json:"peer_scores"`
}
