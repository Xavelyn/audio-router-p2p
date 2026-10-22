package v1

import (
	"time"

	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/telemetry"
)

// ErrorResponse represents standard OpenAPI 3.1 error payload.
type ErrorResponse struct {
	Error   string         `json:"error"`
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}

// SystemStatus represents node health and discovery summary.
type SystemStatus struct {
	Status          string  `json:"status"` // ok, degraded, error
	Version         string  `json:"version"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
	ActiveRoutes    int     `json:"active_routes"`
	DiscoveredPeers int     `json:"discovered_peers"`
	ClockSyncLocked bool    `json:"clock_sync_locked"`
}

// PeerNode represents a discovered mesh peer node.
type PeerNode struct {
	ID       string  `json:"id"`
	Addr     string  `json:"addr"`
	Status   string  `json:"status"` // connected, negotiating, disconnected
	RTTMS    float64 `json:"rtt_ms,omitempty"`
	LastSeen string  `json:"last_seen,omitempty"`
}

// ConnectPeerRequest initiates connection with a remote peer.
type ConnectPeerRequest struct {
	Addr           string `json:"addr"`
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
}

// ClockSyncStatus captures distributed clock synchronization telemetry.
type ClockSyncStatus struct {
	Locked   bool    `json:"locked"`
	OffsetNS int64   `json:"offset_ns"`
	DriftPPM float64 `json:"drift_ppm"`
	Source   string  `json:"source"` // ptp, ntp, internal
}

// AudioDevice describes physical sound cards or virtual loopback devices.
type AudioDevice struct {
	ID                string   `json:"id"`
	URN               string   `json:"urn,omitempty"`
	LegacyID          string   `json:"legacy_id,omitempty"`
	Name              string   `json:"name"`
	MaxInput          int      `json:"max_input"`
	MaxOutput         int      `json:"max_output"`
	DefaultSampleRate float64  `json:"default_sample_rate"`
	IsVirtual         bool     `json:"is_virtual"`
	State             string   `json:"state,omitempty"`
	Status            string   `json:"status,omitempty"`
	Volume            float64  `json:"volume"`
	Muted             bool     `json:"muted"`
	SupportedLayouts  []string `json:"supported_layouts,omitempty"`
}

// UpdateDeviceRequest adjusts master volume or mute state.
type UpdateDeviceRequest struct {
	Volume *float64 `json:"volume,omitempty"`
	Muted  *bool    `json:"muted,omitempty"`
}

// CreateVirtualDeviceRequest provisions an in-memory loopback device.
type CreateVirtualDeviceRequest struct {
	Name             string `json:"name"`
	Direction        string `json:"direction"` // input, output, duplex
	Channels         int    `json:"channels"`
	SampleRate       int    `json:"sample_rate"`
	BufferSizeFrames int    `json:"buffer_size_frames,omitempty"`
}

// CreateRouteRequest creates and activates a matrix routing session.
type CreateRouteRequest struct {
	ID           string                `json:"id,omitempty"`
	Name         string                `json:"name,omitempty"`
	Source       router.MatrixEndpoint `json:"source"`
	Destination  router.MatrixEndpoint `json:"destination"`
	Coefficients []float64             `json:"coefficients,omitempty"`
	SlewMS       float64               `json:"slew_ms,omitempty"`
}

// PatchRouteRequest provides partial updates with 20ms linear gain slewing.
type PatchRouteRequest struct {
	Name         *string   `json:"name,omitempty"`
	Coefficients []float64 `json:"coefficients,omitempty"`
	SlewMS       *float64  `json:"slew_ms,omitempty"`
	Muted        *bool     `json:"muted,omitempty"`
}

// CrosspointGainRequest updates single (out, in) gain multiplier.
type CrosspointGainRequest struct {
	OutChannel int     `json:"out_channel"`
	InChannel  int     `json:"in_channel"`
	Gain       float64 `json:"gain"`
	SlewMS     float64 `json:"slew_ms,omitempty"`
}

// RoomInfo captures distributed room session state.
type RoomInfo struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Leader        string             `json:"leader,omitempty"`
	State         string             `json:"state"` // idle, active, degraded
	Members       []string           `json:"members,omitempty"`
	Inputs        []string           `json:"inputs"`
	Outputs       []string           `json:"outputs"`
	Synced        bool               `json:"synced"`
	LatencyPreset string             `json:"latency_preset,omitempty"`
	InputGains    map[string]float64 `json:"input_gains,omitempty"`
	OutputGains   map[string]float64 `json:"output_gains,omitempty"`
}

// RoomDeviceBindingRequest assigns or updates an input or output device in a room.
type RoomDeviceBindingRequest struct {
	DeviceID string   `json:"device_id"`
	Gain     *float64 `json:"gain,omitempty"`
	Muted    *bool    `json:"muted,omitempty"`
}

// UpdateRoomBindingsRequest provisions a batch update to inputs, outputs, and sync state.
type UpdateRoomBindingsRequest struct {
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
	Synced  *bool    `json:"synced,omitempty"`
}

// SetRoomSyncRequest modifies peer synchronization status for a room.
type SetRoomSyncRequest struct {
	Synced *bool `json:"synced,omitempty"`
}

// UpdateRoomLatencyRequest selects a room's latency preset
// (low, balanced, reliable).
type UpdateRoomLatencyRequest struct {
	Preset string `json:"preset"`
}

// CreateRoomRequest provisions a new conference audio room.
type CreateRoomRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// JitterStats holds jitter and packet arrival statistics.
type JitterStats struct {
	CurrentDepthFrames int     `json:"current_depth_frames"`
	TargetDepthFrames  int     `json:"target_depth_frames"`
	Underruns          uint64  `json:"underruns"`
	Overruns           uint64  `json:"overruns"`
	JitterMS           float64 `json:"jitter_ms,omitempty"`
}

// FECStats holds forward error correction recovery metrics.
type FECStats struct {
	GroupSizeK                 int    `json:"group_size_k"`
	ParityPacketsSentTotal     uint64 `json:"parity_packets_sent_total"`
	ParityPacketsReceivedTotal uint64 `json:"parity_packets_received_total"`
	PacketsRecoveredTotal      uint64 `json:"packets_recovered_total"`
	UnrecoverableGroupsTotal   uint64 `json:"unrecoverable_groups_total"`
}

// PLCStats holds packet loss concealment metrics.
type PLCStats struct {
	ConcealedFramesTotal         uint64 `json:"concealed_frames_total"`
	PitchDetectedFramesTotal     uint64 `json:"pitch_detected_frames_total"`
	NoiseExtrapolatedFramesTotal uint64 `json:"noise_extrapolated_frames_total"`
	SilenceDecayEventsTotal      uint64 `json:"silence_decay_events_total"`
}

// ASRCStats holds asynchronous sample rate conversion telemetry.
type ASRCStats struct {
	NominalRateHz           int     `json:"nominal_rate_hz"`
	SmoothedWatermarkFrames float64 `json:"smoothed_watermark_frames,omitempty"`
	TargetWatermarkFrames   float64 `json:"target_watermark_frames,omitempty"`
	DriftPPM                float64 `json:"drift_ppm"`
	ResampleRatio           float64 `json:"resample_ratio"`
	SamplesAdjustedTotal    uint64  `json:"samples_adjusted_total"`
}

// StreamStats captures snapshot of real-time stream diagnostics.
type StreamStats struct {
	StreamID          string                 `json:"stream_id"`
	PacketsSent       uint64                 `json:"packets_sent"`
	PacketsReceived   uint64                 `json:"packets_received"`
	Underruns         uint64                 `json:"underruns"`
	Overruns          uint64                 `json:"overruns"`
	LatePackets       uint64                 `json:"late_packets"`
	EarlyPackets      uint64                 `json:"early_packets"`
	DTXState          string                 `json:"dtx_state,omitempty"`
	PacketsSuppressed uint64                 `json:"dtx_packets_suppressed_total"`
	RMSdBFS           float64                `json:"rms_dbfs"`
	PeakdBFS          float64                `json:"peak_dbfs"`
	FEC               FECStats               `json:"fec"`
	PLC               PLCStats               `json:"plc"`
	ASRC              ASRCStats              `json:"asrc"`
	Peers             map[string]JitterStats `json:"peers,omitempty"`
}

// RoutePeerStatus is the per-peer link view inside a route's status.
type RoutePeerStatus struct {
	PeerID            string     `json:"peer_id,omitempty"`
	Address           string     `json:"address"`
	Role              string     `json:"role"` // "send" (a destination) or "receive" (a learned source)
	LastSend          *time.Time `json:"last_send,omitempty"`
	LastRecv          *time.Time `json:"last_recv,omitempty"`
	LastContact       *time.Time `json:"last_contact,omitempty"`
	LastSendError     string     `json:"last_send_error,omitempty"`
	PacketsSent       uint64     `json:"packets_sent"`
	PacketsLost       uint64     `json:"packets_lost"`
	FECRecovered      uint64     `json:"fec_recovered"`
	JitterDepthFrames int        `json:"jitter_depth_frames"`
	Underruns         uint64     `json:"underruns"`
}

// RouteStatus answers "why isn't this route playing" for one live route.
// Reasons are human-readable, ordered from most to least likely cause.
type RouteStatus struct {
	RouteID           string            `json:"route_id"`
	Direction         string            `json:"direction"` // "send", "receive", or "sendrecv"
	Playing           bool              `json:"playing"`
	Summary           string            `json:"summary"`
	Reasons           []string          `json:"reasons,omitempty"`
	Peers             []RoutePeerStatus `json:"peers,omitempty"`
	PacketsReceived   uint64            `json:"packets_received"`
	Underruns         uint64            `json:"underruns"`
	Overruns          uint64            `json:"overruns"`
	JitterDepthFrames int               `json:"jitter_depth_frames"`
	DTXState          string            `json:"dtx_state,omitempty"`
	CaptureOverflows  uint64            `json:"capture_overflows"`
	OutputUnderruns   uint64            `json:"output_underflows"`
	LastInput         *time.Time        `json:"last_input,omitempty"`
	LastPlayout       *time.Time        `json:"last_playout,omitempty"`
}

// WSControlMessage represents command payloads received over live WebSocket.
type WSControlMessage struct {
	Type         string    `json:"type"` // "crosspoint", "patch_route", "mute"
	RouteID      string    `json:"route_id"`
	OutChannel   int       `json:"out_channel,omitempty"`
	InChannel    int       `json:"in_channel,omitempty"`
	Gain         float64   `json:"gain,omitempty"`
	Coefficients []float64 `json:"coefficients,omitempty"`
	SlewMS       float64   `json:"slew_ms,omitempty"`
	Muted        *bool     `json:"muted,omitempty"`
}

// NetworkHealthResponse represents the JSON response for GET /api/v1/network/health per ADR 0010.
type NetworkHealthResponse = telemetry.NetworkHealthSnapshot
