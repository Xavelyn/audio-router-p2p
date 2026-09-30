package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/telemetry"
)

type mockLocalNode struct {
	mu            sync.RWMutex
	nodeID        string
	nodeName      string
	apiAddr       string
	uptime        float64
	devices       []AudioDeviceSummary
	healthSnap    *telemetry.NetworkHealthSnapshot
	activeStreams int
}

func (m *mockLocalNode) NodeID() string {
	return m.nodeID
}

func (m *mockLocalNode) NodeName() string {
	return m.nodeName
}

func (m *mockLocalNode) APIAddr() string {
	return m.apiAddr
}

func (m *mockLocalNode) UptimeSeconds() float64 {
	return m.uptime
}

func (m *mockLocalNode) ListDevices(kind string) ([]AudioDeviceSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]AudioDeviceSummary, len(m.devices))
	copy(res, m.devices)
	return res, nil
}

func (m *mockLocalNode) UpdateDeviceState(id string, vol *float64, muted *bool) (AudioDeviceSummary, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	return AudioDeviceSummary{}, false
}

func (m *mockLocalNode) HealthSnapshot() *telemetry.NetworkHealthSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.healthSnap
}

func (m *mockLocalNode) ActiveStreams() int {
	return m.activeStreams
}

type mockDiscovery struct {
	mu     sync.RWMutex
	nodeID string
	peers  map[string]discovery.Peer
	states map[string]string
}

func newMockDiscovery(nodeID string) *mockDiscovery {
	return &mockDiscovery{
		nodeID: nodeID,
		peers:  make(map[string]discovery.Peer),
		states: make(map[string]string),
	}
}

func (m *mockDiscovery) NodeID() string {
	return m.nodeID
}

func (m *mockDiscovery) Peers() []discovery.Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]discovery.Peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	return out
}

func (m *mockDiscovery) PeerByID(id string) *discovery.Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.peers[id]; ok {
		cp := p
		return &cp
	}
	return nil
}

func (m *mockDiscovery) PeerState(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.states[id]; ok {
		return s
	}
	return "alive"
}

func (m *mockDiscovery) addPeer(p discovery.Peer, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers[p.ID] = p
	m.states[p.ID] = state
}

func TestClusterManagerSnapshotAggregation(t *testing.T) {
	// Setup remote mock HTTP server
	var remoteVol float64 = 0.8
	var remoteMuted bool = false

	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/network/health":
			snap := telemetry.NetworkHealthSnapshot{
				NodeID:      "node-remote-1",
				HealthScore: 92.5,
				HealthGrade: telemetry.GradeExcellent,
				LatencyMatrix: map[string]float64{
					"node-local-1": 1.45,
				},
				Sync: telemetry.ClockSyncHealth{
					OffsetUS: 5.2,
					DriftPPM: 0.8,
					IsMaster: false,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snap)

		case "/api/v1/devices":
			devs := []map[string]any{
				{
					"id":                  "hw:usb-out",
					"name":                "USB Output",
					"max_input":           0,
					"max_output":          2,
					"default_sample_rate": 48000.0,
					"volume":              remoteVol,
					"muted":               remoteMuted,
					"state":               "active",
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(devs)

		default:
			http.NotFound(w, r)
		}
	}))
	defer remoteServer.Close()

	local := &mockLocalNode{
		nodeID:   "node-local-1",
		nodeName: "Local Gateway",
		apiAddr:  "http://127.0.0.1:8443",
		uptime:   120.0,
		devices: []AudioDeviceSummary{
			{
				ID:                "hw:local-mic",
				DeviceID:          "hw:local-mic",
				NodeID:            "node-local-1",
				NodeName:          "Local Gateway",
				Name:              "Local Microphone",
				Direction:         "input",
				MaxInput:          2,
				MaxOutput:         0,
				DefaultSampleRate: 48000,
				Volume:            1.0,
				Muted:             false,
				State:             "active",
			},
		},
		healthSnap: &telemetry.NetworkHealthSnapshot{
			NodeID:      "node-local-1",
			HealthScore: 98.0,
			HealthGrade: telemetry.GradeExcellent,
			LatencyMatrix: map[string]float64{
				"node-remote-1": 1.45,
			},
		},
		activeStreams: 1,
	}

	disc := newMockDiscovery("node-local-1")
	disc.addPeer(discovery.Peer{
		ID:       "node-remote-1",
		Addr:     "127.0.0.1:45401",
		APIAddr:  remoteServer.URL,
		LastSeen: time.Now(),
	}, "alive")

	cfg := Config{
		ScrapeInterval: 100 * time.Millisecond,
		ScrapeTimeout:  500 * time.Millisecond,
		MaxWorkers:     4,
		AuthToken:      "secret-token",
	}

	mgr := NewManager(cfg, local, disc, remoteServer.Client())
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	snap := mgr.ScrapeCluster(ctx)
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	if snap.TotalNodes != 2 {
		t.Fatalf("snap.TotalNodes = %d, want 2", snap.TotalNodes)
	}
	if snap.ActiveNodes != 2 {
		t.Fatalf("snap.ActiveNodes = %d, want 2", snap.ActiveNodes)
	}

	// Verify nodes
	localSummary, ok := snap.Nodes["node-local-1"]
	if !ok || !localSummary.IsLocal {
		t.Fatalf("expected local node in snapshot: %+v", localSummary)
	}
	if localSummary.HealthScore != 98.0 {
		t.Errorf("local node health = %f, want 98.0", localSummary.HealthScore)
	}

	remoteSummary, ok := snap.Nodes["node-remote-1"]
	if !ok || remoteSummary.IsLocal {
		t.Fatalf("expected remote node in snapshot: %+v", remoteSummary)
	}
	if remoteSummary.HealthScore != 92.5 {
		t.Errorf("remote node health = %f, want 92.5", remoteSummary.HealthScore)
	}

	// Verify devices aggregation
	if len(snap.ClusterDevices) != 2 {
		t.Fatalf("snap.ClusterDevices len = %d, want 2", len(snap.ClusterDevices))
	}

	// Verify latency matrix
	if snap.LatencyMatrix["node-local-1"]["node-remote-1"] != 1.45 {
		t.Errorf("latency local->remote = %f, want 1.45", snap.LatencyMatrix["node-local-1"]["node-remote-1"])
	}
	if snap.LatencyMatrix["node-remote-1"]["node-local-1"] != 1.45 {
		t.Errorf("latency remote->local = %f, want 1.45", snap.LatencyMatrix["node-remote-1"]["node-local-1"])
	}
}

func TestClusterManagerCircuitBreaker(t *testing.T) {
	var requestCount atomic.Int32

	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer failingServer.Close()

	local := &mockLocalNode{
		nodeID: "node-local",
	}

	disc := newMockDiscovery("node-local")
	disc.addPeer(discovery.Peer{
		ID:       "node-fail-1",
		Addr:     "127.0.0.1:45401",
		APIAddr:  failingServer.URL,
		LastSeen: time.Now(),
	}, "alive")

	cfg := Config{
		ScrapeInterval:          50 * time.Millisecond,
		ScrapeTimeout:           100 * time.Millisecond,
		MaxWorkers:              2,
		CircuitBreakerThreshold: 3,
		CircuitBreakerCooldown:  1 * time.Second,
	}

	mgr := NewManager(cfg, local, disc, failingServer.Client())
	defer mgr.Close()

	ctx := context.Background()

	// Scrape 1: failure 1
	mgr.ScrapeCluster(ctx)
	// Scrape 2: failure 2
	mgr.ScrapeCluster(ctx)
	// Scrape 3: failure 3 -> triggers circuit open
	snap3 := mgr.ScrapeCluster(ctx)

	failSummary := snap3.Nodes["node-fail-1"]
	if failSummary.ErrorReason == "" {
		t.Error("expected non-empty ErrorReason on failed scrape")
	}

	beforeSkipRequests := requestCount.Load()

	// Scrape 4: circuit breaker should be OPEN and skip HTTP requests
	snap4 := mgr.ScrapeCluster(ctx)
	afterSkipRequests := requestCount.Load()

	if afterSkipRequests != beforeSkipRequests {
		t.Errorf("expected circuit breaker to skip HTTP requests, got count %d want %d", afterSkipRequests, beforeSkipRequests)
	}

	nodeSummary4 := snap4.Nodes["node-fail-1"]
	if nodeSummary4.ErrorReason != "circuit breaker open (consecutive failures)" {
		t.Errorf("expected circuit breaker open error reason, got %q", nodeSummary4.ErrorReason)
	}
}

func TestClusterManagerDeadNodeSWIMBypass(t *testing.T) {
	var httpHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	local := &mockLocalNode{nodeID: "local-node"}
	disc := newMockDiscovery("local-node")
	disc.addPeer(discovery.Peer{
		ID:       "dead-node",
		APIAddr:  server.URL,
		LastSeen: time.Now(),
	}, "dead") // SWIM state dead

	mgr := NewManager(Config{}, local, disc, server.Client())
	defer mgr.Close()

	snap := mgr.ScrapeCluster(context.Background())
	if httpHits.Load() != 0 {
		t.Errorf("expected 0 HTTP hits for dead SWIM node, got %d", httpHits.Load())
	}

	node := snap.Nodes["dead-node"]
	if node.SWIMState != "dead" {
		t.Errorf("expected SWIMState 'dead', got %q", node.SWIMState)
	}
	if node.ErrorReason != "SWIM failure detector reported dead" {
		t.Errorf("expected error reason about dead SWIM, got %q", node.ErrorReason)
	}
}

func TestClusterManagerConcurrentReads(t *testing.T) {
	local := &mockLocalNode{nodeID: "local-node"}
	disc := newMockDiscovery("local-node")

	mgr := NewManager(Config{
		ScrapeInterval: 20 * time.Millisecond,
		ScrapeTimeout:  50 * time.Millisecond,
	}, local, disc)
	defer mgr.Close()

	mgr.Start(context.Background())

	var wg sync.WaitGroup
	const readers = 10
	const iterations = 50

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				snap := mgr.GetSnapshot()
				if snap == nil {
					t.Errorf("snapshot was nil")
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	wg.Wait()
}

func TestMutateRemoteDevice(t *testing.T) {
	var remoteMuted bool
	var remoteVol float64 = 0.5

	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/api/v1/devices/not-found" {
			http.Error(w, "device not found", http.StatusNotFound)
			return
		}
		if r.URL.Path != "/api/v1/devices/hw:remote-1" {
			http.Error(w, "bad device path", http.StatusNotFound)
			return
		}
		var req DeviceMutationRequest
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

		resp := map[string]any{
			"id":        "hw:remote-1",
			"device_id": "hw:remote-1",
			"volume":    remoteVol,
			"muted":     remoteMuted,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer remoteServer.Close()

	local := &mockLocalNode{
		nodeID: "node-local",
		devices: []AudioDeviceSummary{
			{
				ID:       "hw:local-1",
				DeviceID: "hw:local-1",
				Volume:   1.0,
				Muted:    false,
			},
		},
	}

	disc := newMockDiscovery("node-local")
	disc.addPeer(discovery.Peer{
		ID:      "node-remote",
		APIAddr: remoteServer.URL,
	}, "alive")

	mgr := NewManager(Config{
		AuthToken: "test-auth-token",
	}, local, disc, remoteServer.Client())
	defer mgr.Close()

	ctx := context.Background()

	// 1. Mutate local device
	newVol := 0.75
	newMute := true
	localResp, err := mgr.MutateRemoteDevice(ctx, "node-local", "hw:local-1", DeviceMutationRequest{
		Volume: &newVol,
		Muted:  &newMute,
	})
	if err != nil {
		t.Fatalf("MutateRemoteDevice local error: %v", err)
	}
	if localResp.Volume != 0.75 || !localResp.Muted {
		t.Errorf("localResp = %+v, want vol 0.75 and muted true", localResp)
	}

	// 2. Mutate remote device
	newRemoteVol := 0.90
	newRemoteMute := false
	remoteResp, err := mgr.MutateRemoteDevice(ctx, "node-remote", "hw:remote-1", DeviceMutationRequest{
		Volume: &newRemoteVol,
		Muted:  &newRemoteMute,
	})
	if err != nil {
		t.Fatalf("MutateRemoteDevice remote error: %v", err)
	}
	if remoteResp.Volume != 0.90 || remoteResp.Muted {
		t.Errorf("remoteResp = %+v, want vol 0.90 and muted false", remoteResp)
	}

	// 3. Mutate non-existent device on remote -> ErrDeviceNotFound
	_, err = mgr.MutateRemoteDevice(ctx, "node-remote", "not-found", DeviceMutationRequest{
		Volume: &newRemoteVol,
	})
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound, got: %v", err)
	}

	// 4. Mutate non-existent node -> ErrNodeNotFound
	_, err = mgr.MutateRemoteDevice(ctx, "non-existent-node", "hw:remote-1", DeviceMutationRequest{
		Volume: &newRemoteVol,
	})
	if !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("expected ErrNodeNotFound, got: %v", err)
	}

	// 5. Mutate node with broken APIAddr -> ErrRemoteNodeUnavailable
	disc.addPeer(discovery.Peer{
		ID:      "unreachable-node",
		APIAddr: "http://127.0.0.1:1", // closed port
	}, "alive")

	_, err = mgr.MutateRemoteDevice(ctx, "unreachable-node", "hw:remote-1", DeviceMutationRequest{
		Volume: &newRemoteVol,
	})
	if !errors.Is(err, ErrRemoteNodeUnavailable) {
		t.Errorf("expected ErrRemoteNodeUnavailable, got: %v", err)
	}
}

func TestClusterManagerLifecycleIdempotence(t *testing.T) {
	local := &mockLocalNode{nodeID: "local-node"}
	disc := newMockDiscovery("local-node")

	mgr := NewManager(Config{}, local, disc)
	ctx := context.Background()

	// Multiple Start calls
	mgr.Start(ctx)
	mgr.Start(ctx)

	// Multiple Stop calls
	mgr.Stop()
	mgr.Stop()

	// Multiple Close calls
	if err := mgr.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("Close (2nd) returned error: %v", err)
	}
}

func TestClusterManagerCanceledContext(t *testing.T) {
	local := &mockLocalNode{nodeID: "local-node"}
	disc := newMockDiscovery("local-node")
	for i := 0; i < 5; i++ {
		disc.addPeer(discovery.Peer{
			ID:       fmt.Sprintf("peer-%d", i),
			APIAddr:  "http://127.0.0.1:9999",
			LastSeen: time.Now(),
		}, "alive")
	}

	mgr := NewManager(Config{
		MaxWorkers:    2,
		ScrapeTimeout: 5 * time.Second,
	}, local, disc)
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled immediately

	done := make(chan struct{})
	go func() {
		mgr.ScrapeCluster(ctx)
		close(done)
	}()

	select {
	case <-done:
		// success, did not deadlock
	case <-time.After(2 * time.Second):
		t.Fatal("ScrapeCluster deadlocked on canceled context")
	}
}

func TestMutateRemoteDevice_NaNValidation(t *testing.T) {
	local := &mockLocalNode{nodeID: "local-node"}
	disc := newMockDiscovery("local-node")
	mgr := NewManager(Config{}, local, disc)
	defer mgr.Close()

	nanVol := math.NaN()
	_, err := mgr.MutateRemoteDevice(context.Background(), "local-node", "dev-1", DeviceMutationRequest{
		Volume: &nanVol,
	})
	if !errors.Is(err, ErrInvalidParameter) {
		t.Errorf("expected ErrInvalidParameter on NaN volume, got %v", err)
	}

	infVol := math.Inf(1)
	_, err = mgr.MutateRemoteDevice(context.Background(), "local-node", "dev-1", DeviceMutationRequest{
		Volume: &infVol,
	})
	if !errors.Is(err, ErrInvalidParameter) {
		t.Errorf("expected ErrInvalidParameter on Inf volume, got %v", err)
	}
}
