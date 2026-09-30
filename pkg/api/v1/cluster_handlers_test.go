package v1

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/telemetry"
)

type mockClusterLocalNode struct {
	nodeID  string
	devices []cluster.AudioDeviceSummary
}

func (m *mockClusterLocalNode) NodeID() string {
	return m.nodeID
}

func (m *mockClusterLocalNode) NodeName() string {
	return "Local Node"
}

func (m *mockClusterLocalNode) APIAddr() string {
	return "https://127.0.0.1:8443"
}

func (m *mockClusterLocalNode) UptimeSeconds() float64 {
	return 300.0
}

func (m *mockClusterLocalNode) ListDevices(kind string) ([]cluster.AudioDeviceSummary, error) {
	return m.devices, nil
}

func (m *mockClusterLocalNode) UpdateDeviceState(id string, vol *float64, muted *bool) (cluster.AudioDeviceSummary, bool) {
	for i, d := range m.devices {
		if d.ID == id || d.DeviceID == id {
			if vol != nil {
				m.devices[i].Volume = *vol
			}
			if muted != nil {
				m.devices[i].Muted = *muted
			}
			return m.devices[i], true
		}
	}
	return cluster.AudioDeviceSummary{}, false
}

func (m *mockClusterLocalNode) HealthSnapshot() *telemetry.NetworkHealthSnapshot {
	return &telemetry.NetworkHealthSnapshot{
		NodeID:      m.nodeID,
		HealthScore: 99.0,
		HealthGrade: telemetry.GradeExcellent,
		LatencyMatrix: map[string]float64{
			"remote-node-1": 1.25,
		},
	}
}

func (m *mockClusterLocalNode) ActiveStreams() int {
	return 2
}

type mockClusterDisc struct {
	peers map[string]discovery.Peer
}

func (m *mockClusterDisc) NodeID() string {
	return "test-node-1"
}

func (m *mockClusterDisc) Peers() []discovery.Peer {
	out := make([]discovery.Peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	return out
}

func (m *mockClusterDisc) PeerByID(id string) *discovery.Peer {
	if p, ok := m.peers[id]; ok {
		cp := p
		return &cp
	}
	return nil
}

func (m *mockClusterDisc) PeerState(id string) string {
	return "alive"
}

func TestClusterHandlers(t *testing.T) {
	// 1. Mock remote peer HTTP server
	var remoteVol float64 = 0.5
	var remoteMuted bool = false

	remotePeerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/network/health" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetry.NetworkHealthSnapshot{
				NodeID:      "remote-node-1",
				HealthScore: 95.0,
				HealthGrade: telemetry.GradeExcellent,
				LatencyMatrix: map[string]float64{
					"test-node-1": 1.25,
				},
			})

		case r.URL.Path == "/api/v1/devices" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id":                  "hw:remote-dac",
					"device_id":           "hw:remote-dac",
					"name":                "Remote DAC",
					"direction":           "output",
					"max_input":           0,
					"max_output":          2,
					"default_sample_rate": 48000.0,
					"volume":              remoteVol,
					"muted":               remoteMuted,
					"state":               "active",
				},
			})

		case r.URL.Path == "/api/v1/devices/hw:remote-dac" && r.Method == http.MethodPatch:
			var req cluster.DeviceMutationRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.Volume != nil {
				remoteVol = *req.Volume
			}
			if req.Muted != nil {
				remoteMuted = *req.Muted
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "hw:remote-dac",
				"device_id": "hw:remote-dac",
				"volume":    remoteVol,
				"muted":     remoteMuted,
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer remotePeerServer.Close()

	// 2. Setup V1Server and ClusterManager
	s, mux := newTestV1Server(t)

	localNode := &mockClusterLocalNode{
		nodeID: "test-node-1",
		devices: []cluster.AudioDeviceSummary{
			{
				ID:                "hw:local-mic",
				DeviceID:          "hw:local-mic",
				NodeID:            "test-node-1",
				NodeName:          "Local Node",
				Name:              "Local Microphone",
				Direction:         "input",
				MaxInput:          2,
				MaxOutput:         0,
				DefaultSampleRate: 48000,
				Volume:            0.9,
				Muted:             false,
				State:             "active",
			},
			{
				ID:                "virt:loopback",
				DeviceID:          "virt:loopback",
				NodeID:            "test-node-1",
				NodeName:          "Local Node",
				Name:              "Virtual Loopback",
				Direction:         "duplex",
				MaxInput:          2,
				MaxOutput:         2,
				DefaultSampleRate: 48000,
				Volume:            1.0,
				Muted:             false,
				State:             "active",
			},
		},
	}

	disc := &mockClusterDisc{
		peers: map[string]discovery.Peer{
			"remote-node-1": {
				ID:       "remote-node-1",
				Addr:     "127.0.0.1:45401",
				APIAddr:  remotePeerServer.URL,
				LastSeen: time.Now(),
			},
		},
	}

	cm := cluster.NewManager(
		cluster.Config{AuthToken: "test-v1-token"},
		localNode,
		disc,
		remotePeerServer.Client(),
	)
	defer cm.Close()

	s.SetClusterManager(cm)

	// Scrape cluster snapshot once to populate data
	cm.ScrapeCluster(t.Context())

	// Test 1: GET /api/v1/cluster/nodes (unauthenticated)
	w := doV1Request(mux, http.MethodGet, "/api/v1/cluster/nodes", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", w.Code)
	}

	// Test 2: GET /api/v1/cluster/nodes (authenticated)
	w = doV1Request(mux, http.MethodGet, "/api/v1/cluster/nodes", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	var nodesResp cluster.ClusterNodesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &nodesResp); err != nil {
		t.Fatalf("unmarshal nodesResp: %v", err)
	}
	if nodesResp.TotalNodes != 2 {
		t.Errorf("TotalNodes = %d, want 2", nodesResp.TotalNodes)
	}
	if len(nodesResp.Nodes) != 2 {
		t.Errorf("len(Nodes) = %d, want 2", len(nodesResp.Nodes))
	}
	if nodesResp.LatencyMatrix["test-node-1"]["remote-node-1"] != 1.25 {
		t.Errorf("latency = %f, want 1.25", nodesResp.LatencyMatrix["test-node-1"]["remote-node-1"])
	}

	// Test 3: GET /api/v1/cluster/devices (all)
	w = doV1Request(mux, http.MethodGet, "/api/v1/cluster/devices", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	var devsResp cluster.ClusterDevicesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &devsResp); err != nil {
		t.Fatalf("unmarshal devsResp: %v", err)
	}
	if len(devsResp.Devices) != 3 {
		t.Errorf("len(Devices) = %d, want 3", len(devsResp.Devices))
	}

	// Test 4: GET /api/v1/cluster/devices?direction=input (input + duplex)
	w = doV1Request(mux, http.MethodGet, "/api/v1/cluster/devices?direction=input", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var inputDevsResp cluster.ClusterDevicesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &inputDevsResp); err != nil {
		t.Fatalf("unmarshal inputDevsResp: %v", err)
	}
	if len(inputDevsResp.Devices) != 2 {
		t.Errorf("len(inputDevsResp.Devices) = %d, want 2 (1 input + 1 duplex)", len(inputDevsResp.Devices))
	}

	// Test 5: GET /api/v1/cluster/devices?direction=output (output + duplex)
	w = doV1Request(mux, http.MethodGet, "/api/v1/cluster/devices?direction=output", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var outputDevsResp cluster.ClusterDevicesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &outputDevsResp); err != nil {
		t.Fatalf("unmarshal outputDevsResp: %v", err)
	}
	if len(outputDevsResp.Devices) != 2 {
		t.Errorf("len(outputDevsResp.Devices) = %d, want 2 (1 output + 1 duplex)", len(outputDevsResp.Devices))
	}

	// Test 6: PATCH /api/v1/cluster/nodes/{nodeId}/devices/{deviceId} (local node)
	newLocalVol := 0.65
	newLocalMute := true
	patchLocalReq := cluster.DeviceMutationRequest{
		Volume: &newLocalVol,
		Muted:  &newLocalMute,
	}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/test-node-1/devices/hw:local-mic", patchLocalReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for local device patch, got %d: %s", w.Code, w.Body.String())
	}
	var localMutResp cluster.DeviceMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &localMutResp); err != nil {
		t.Fatalf("unmarshal localMutResp: %v", err)
	}
	if localMutResp.Volume != 0.65 || !localMutResp.Muted {
		t.Errorf("localMutResp = %+v, want vol 0.65 and muted true", localMutResp)
	}

	// Test 7: PATCH /api/v1/cluster/nodes/{nodeId}/devices/{deviceId} (remote node proxy)
	newRemoteVol := 0.85
	newRemoteMute := false
	patchRemoteReq := cluster.DeviceMutationRequest{
		Volume: &newRemoteVol,
		Muted:  &newRemoteMute,
	}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/remote-node-1/devices/hw:remote-dac", patchRemoteReq, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for remote device patch, got %d: %s", w.Code, w.Body.String())
	}
	var remoteMutResp cluster.DeviceMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &remoteMutResp); err != nil {
		t.Fatalf("unmarshal remoteMutResp: %v", err)
	}
	if remoteMutResp.Volume != 0.85 || remoteMutResp.Muted {
		t.Errorf("remoteMutResp = %+v, want vol 0.85 and muted false", remoteMutResp)
	}

	// Test 8: PATCH invalid volume (out of bounds)
	invalidVol := 3.5
	invalidReq := cluster.DeviceMutationRequest{Volume: &invalidVol}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/test-node-1/devices/hw:local-mic", invalidReq, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for out of bounds volume, got %d", w.Code)
	}

	// Test 8b: PATCH invalid volume (NaN / Inf)
	nanVol := math.NaN()
	nanReq := cluster.DeviceMutationRequest{Volume: &nanVol}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/test-node-1/devices/hw:local-mic", nanReq, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for NaN volume, got %d", w.Code)
	}

	// Test 9: PATCH unknown node
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/non-existent-node/devices/hw:local-mic", patchLocalReq, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for non-existent node, got %d", w.Code)
	}

	// Test 10: PATCH unknown device on local node
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/test-node-1/devices/unknown-dev", patchLocalReq, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for unknown device, got %d", w.Code)
	}

	// Test 11: PATCH unreachable remote node -> 502 Bad Gateway
	disc.peers["broken-node"] = discovery.Peer{
		ID:      "broken-node",
		APIAddr: "http://127.0.0.1:1", // closed port
	}
	w = doV1Request(mux, http.MethodPatch, "/api/v1/cluster/nodes/broken-node/devices/hw:dev", patchLocalReq, "Bearer test-v1-token")
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway for unreachable remote node, got %d: %s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err == nil {
		if errResp.Code != "ERR_REMOTE_NODE_UNAVAILABLE" {
			t.Errorf("expected ERR_REMOTE_NODE_UNAVAILABLE code, got %q", errResp.Code)
		}
	}
}
