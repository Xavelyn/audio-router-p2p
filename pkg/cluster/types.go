package cluster

import (
	"errors"
	"time"

	"github.com/audio-router-p2p/pkg/telemetry"
)

// Sentinel errors for cluster and remote device operations.
var (
	ErrRemoteNodeUnavailable = errors.New("remote node unavailable")
	ErrNodeNotFound          = errors.New("node not found")
	ErrDeviceNotFound        = errors.New("device not found")
	ErrInvalidParameter      = errors.New("invalid parameter")
)

// AudioDeviceSummary represents a physical soundcard or virtual loopback audio device
// situated on a local or remote node in the cluster.
type AudioDeviceSummary struct {
	ID                string   `json:"id"`
	DeviceID          string   `json:"device_id,omitempty"`
	URN               string   `json:"urn,omitempty"`
	NodeID            string   `json:"node_id"`
	NodeName          string   `json:"node_name,omitempty"`
	Name              string   `json:"name"`
	Direction         string   `json:"direction"` // "input" | "output" | "duplex"
	MaxInput          int      `json:"max_input"`
	MaxOutput         int      `json:"max_output"`
	DefaultSampleRate float64  `json:"default_sample_rate"`
	IsVirtual         bool     `json:"is_virtual"`
	Volume            float64  `json:"volume"`
	Muted             bool     `json:"muted"`
	SupportedLayouts  []string `json:"supported_layouts,omitempty"`
	State             string   `json:"state,omitempty"` // "active", "idle", etc.
}

// ClusterNodeSummary represents the health, membership, and device inventory of a node.
type ClusterNodeSummary struct {
	ID            string                    `json:"id"`
	NodeID        string                    `json:"node_id,omitempty"`
	Name          string                    `json:"name"`
	APIAddr       string                    `json:"api_addr"`
	IsLocal       bool                      `json:"is_local"`
	SWIMState     string                    `json:"swim_state"` // alive, suspect, dead
	HealthScore   float64                   `json:"health_score"`
	HealthGrade   string                    `json:"health_grade"` // EXCELLENT, GOOD, FAIR, POOR, CRITICAL
	ClockSync     telemetry.ClockSyncHealth `json:"clock_sync"`
	Devices       []AudioDeviceSummary      `json:"devices"`
	ActiveStreams int                       `json:"active_streams"`
	UptimeSeconds float64                   `json:"uptime_seconds"`
	LastSeen      time.Time                 `json:"last_seen"`
	ErrorReason   string                    `json:"error_reason,omitempty"`
}

// ClusterSnapshot captures an atomic, point-in-time view of the entire cluster.
type ClusterSnapshot struct {
	GeneratedAt    time.Time                     `json:"generated_at"`
	LocalNodeID    string                        `json:"local_node_id"`
	ClusterHealth  float64                       `json:"cluster_health"`
	TotalNodes     int                           `json:"total_nodes"`
	ActiveNodes    int                           `json:"active_nodes"`
	Nodes          map[string]ClusterNodeSummary `json:"nodes"`
	LatencyMatrix  map[string]map[string]float64 `json:"latency_matrix"`
	ClusterDevices []AudioDeviceSummary          `json:"cluster_devices"`
}

// ClusterNodesResponse formats the response for GET /api/v1/cluster/nodes.
type ClusterNodesResponse struct {
	ClusterID     string                        `json:"cluster_id,omitempty"`
	TotalNodes    int                           `json:"total_nodes"`
	HealthyNodes  int                           `json:"healthy_nodes"`
	DegradedNodes int                           `json:"degraded_nodes"`
	ActiveNodes   int                           `json:"active_nodes"`
	Timestamp     time.Time                     `json:"timestamp"`
	Nodes         []ClusterNodeSummary          `json:"nodes"`
	NodesMap      map[string]ClusterNodeSummary `json:"nodes_map,omitempty"`
	LatencyMatrix map[string]map[string]float64 `json:"latency_matrix"`
}

// ClusterDevicesResponse formats the response for GET /api/v1/cluster/devices.
type ClusterDevicesResponse struct {
	Timestamp time.Time            `json:"timestamp"`
	Devices   []AudioDeviceSummary `json:"devices"`
}

// DeviceMutationRequest captures volume and mute state updates.
type DeviceMutationRequest struct {
	Volume *float64 `json:"volume,omitempty"`
	Muted  *bool    `json:"muted,omitempty"`
}

// DeviceMutationResponse captures the result of a device mutation across the cluster.
type DeviceMutationResponse struct {
	ID        string    `json:"id,omitempty"`
	DeviceID  string    `json:"device_id"`
	NodeID    string    `json:"node_id"`
	Volume    float64   `json:"volume"`
	Muted     bool      `json:"muted"`
	UpdatedAt time.Time `json:"updated_at"`
}
