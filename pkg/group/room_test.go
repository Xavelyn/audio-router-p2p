package group

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
)

func TestRoomLifecycleAndMembers(t *testing.T) {
	room := NewRoom(RoomConfig{
		ID:          "test-room",
		Name:        "Test Room",
		LocalPeerID: "node-2",
	})

	if room.ID() != "test-room" {
		t.Fatalf("expected id 'test-room', got %q", room.ID())
	}
	if room.Name() != "Test Room" {
		t.Fatalf("expected name 'Test Room', got %q", room.Name())
	}
	if room.State() != RoomStateCreated {
		t.Fatalf("expected state created, got %v", room.State())
	}

	joinEvents := make(chan string, 10)
	leaveEvents := make(chan string, 10)
	leaderEvents := make(chan string, 10)

	room.SetCallbacks(
		func(m *RoomMember) { joinEvents <- m.PeerID },
		func(m *RoomMember) { leaveEvents <- m.PeerID },
		func(leaderID string) { leaderEvents <- leaderID },
		nil,
	)

	// Add local member
	room.addMember("node-2", &RoomMember{PeerID: "node-2", NodeID: "node-2"})
	// Add remote member node-3
	room.addMember("node-3", &RoomMember{PeerID: "node-3", NodeID: "node-3"})

	select {
	case id := <-joinEvents:
		if id != "node-3" {
			t.Fatalf("expected join event for node-3, got %s", id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for join event")
	}

	// Recompute leader: between node-2 and node-3, node-2 is lowest
	room.recomputeLeader()
	if !room.IsLeader() {
		t.Fatalf("expected node-2 to be leader, leader is %s", room.Leader())
	}

	// Add node-1 (lower ID)
	room.addMember("node-1", &RoomMember{PeerID: "node-1", NodeID: "node-1"})
	room.recomputeLeader()
	if room.Leader() != "node-1" {
		t.Fatalf("expected node-1 to become leader, got %s", room.Leader())
	}

	select {
	case leader := <-leaderEvents:
		if leader != "node-1" {
			t.Fatalf("expected leader change event to node-1, got %s", leader)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for leader event")
	}

	// Remove node-3
	if !room.removeMember("node-3") {
		t.Fatal("expected removeMember to return true")
	}

	select {
	case left := <-leaveEvents:
		if left != "node-3" {
			t.Fatalf("expected leave event for node-3, got %s", left)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for leave event")
	}

	// Verify JSON marshal
	b, err := json.Marshal(room)
	if err != nil {
		t.Fatalf("json.Marshal(room): %v", err)
	}
	var res map[string]interface{}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if res["id"] != "test-room" || res["leader"] != "node-1" {
		t.Fatalf("unexpected JSON: %s", string(b))
	}
}

func TestRoomJoinLeave(t *testing.T) {
	room := NewRoom(RoomConfig{
		ID:          "test-room-lifecycle",
		LocalPeerID: "node-local",
	})

	if err := room.Join(); err != nil {
		t.Fatalf("room.Join failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if err := room.Leave(); err != nil {
		t.Fatalf("room.Leave failed: %v", err)
	}
}

func TestRoomRouteCompiler_NM_Mixing(t *testing.T) {
	routesFile := t.TempDir() + "/test_routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	room := NewRoom(RoomConfig{
		ID:     "room-nm",
		Name:   "NM Mix Room",
		Router: rt,
	})

	// Add 2 inputs and 2 outputs
	if err := room.AddInput("mic-1"); err != nil {
		t.Fatalf("AddInput 1: %v", err)
	}
	if err := room.AddInput("mic-2"); err != nil {
		t.Fatalf("AddInput 2: %v", err)
	}
	if err := room.AddOutput("spk-1"); err != nil {
		t.Fatalf("AddOutput 1: %v", err)
	}
	if err := room.AddOutput("spk-2"); err != nil {
		t.Fatalf("AddOutput 2: %v", err)
	}

	// 2 inputs * 2 outputs = 4 matrix routes
	routes := rt.ListMatrix()
	if len(routes) != 4 {
		t.Fatalf("expected 4 matrix routes, got %d", len(routes))
	}

	expectedIDs := map[string]bool{
		"room-room-nm-mic-1-spk-1": true,
		"room-room-nm-mic-1-spk-2": true,
		"room-room-nm-mic-2-spk-1": true,
		"room-room-nm-mic-2-spk-2": true,
	}
	for _, r := range routes {
		if !expectedIDs[r.ID] {
			t.Errorf("unexpected route ID: %s", r.ID)
		}
		if r.Source.DeviceID != "mic-1" && r.Source.DeviceID != "mic-2" {
			t.Errorf("unexpected source: %s", r.Source.DeviceID)
		}
		if r.Destination.DeviceID != "spk-1" && r.Destination.DeviceID != "spk-2" {
			t.Errorf("unexpected destination: %s", r.Destination.DeviceID)
		}
	}

	// Adjust trim gain on mic-1 to 0.5
	room.SetInputGain("mic-1", 0.5)
	r1, ok := rt.GetMatrix("room-room-nm-mic-1-spk-1")
	if !ok {
		t.Fatal("expected route room-room-nm-mic-1-spk-1")
	}
	if r1.Coefficients[0] != 0.5 {
		t.Fatalf("expected gain coefficient 0.5, got %f", r1.Coefficients[0])
	}

	// Remove mic-2
	if err := room.RemoveInput("mic-2"); err != nil {
		t.Fatalf("RemoveInput: %v", err)
	}
	routes = rt.ListMatrix()
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes after removing mic-2, got %d", len(routes))
	}
	if _, ok := rt.GetMatrix("room-room-nm-mic-2-spk-1"); ok {
		t.Error("expected room-room-nm-mic-2-spk-1 to be deleted")
	}

	// Remove spk-1
	if err := room.RemoveOutput("spk-1"); err != nil {
		t.Fatalf("RemoveOutput: %v", err)
	}
	routes = rt.ListMatrix()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route after removing spk-1, got %d", len(routes))
	}
	if routes[0].ID != "room-room-nm-mic-1-spk-2" {
		t.Fatalf("expected route room-room-nm-mic-1-spk-2, got %s", routes[0].ID)
	}

	// Leaving the room cleans up compiled routes
	if err := room.Leave(); err != nil {
		t.Fatalf("Leave error: %v", err)
	}
	routes = rt.ListMatrix()
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes after Leave, got %d", len(routes))
	}
}

func TestRoomFanoutAndSumming_MultiRoom(t *testing.T) {
	routesFile := t.TempDir() + "/test_fanout_routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	roomA := NewRoom(RoomConfig{ID: "room-A", Name: "Room A", Router: rt})
	roomB := NewRoom(RoomConfig{ID: "room-B", Name: "Room B", Router: rt})

	// 1:N Fanout: mic-shared is in both Room A and Room B
	_ = roomA.AddInput("mic-shared")
	_ = roomA.AddOutput("spk-A")

	_ = roomB.AddInput("mic-shared")
	_ = roomB.AddOutput("spk-B")

	if _, ok := rt.GetMatrix("room-room-A-mic-shared-spk-A"); !ok {
		t.Error("missing Room A fanout route")
	}
	if _, ok := rt.GetMatrix("room-room-B-mic-shared-spk-B"); !ok {
		t.Error("missing Room B fanout route")
	}

	// N:1 Summing: spk-summed is output in both Room A and Room B
	_ = roomA.AddOutput("spk-summed")
	_ = roomB.AddOutput("spk-summed")
	_ = roomB.AddInput("mic-guest")

	// Routes expected:
	// Room A: (mic-shared -> spk-A), (mic-shared -> spk-summed) [2 routes]
	// Room B: (mic-shared -> spk-B), (mic-shared -> spk-summed), (mic-guest -> spk-B), (mic-guest -> spk-summed) [4 routes]
	routes := rt.ListMatrix()
	if len(routes) != 6 {
		t.Fatalf("expected 6 total routes for 1:N fanout + N:1 summing, got %d", len(routes))
	}

	// Both rooms feed spk-summed simultaneously
	if _, ok := rt.GetMatrix("room-room-A-mic-shared-spk-summed"); !ok {
		t.Error("missing room-A feed to spk-summed")
	}
	if _, ok := rt.GetMatrix("room-room-B-mic-guest-spk-summed"); !ok {
		t.Error("missing room-B feed to spk-summed")
	}

	// Tearing down Room A should NOT affect Room B
	_ = roomA.Leave()
	routes = rt.ListMatrix()
	if len(routes) != 4 {
		t.Fatalf("expected 4 routes remaining in Room B, got %d", len(routes))
	}
	for _, r := range routes {
		if r.Destination.DeviceID == "spk-A" {
			t.Errorf("Room A route was not cleaned up: %s", r.ID)
		}
	}
}

type mockDiscoveryProvider struct {
	peers     []discovery.Peer
	livePeers []discovery.Peer
	nodeID    string
}

func (m *mockDiscoveryProvider) Peers() []discovery.Peer     { return m.peers }
func (m *mockDiscoveryProvider) LivePeers() []discovery.Peer { return m.livePeers }
func (m *mockDiscoveryProvider) NodeID() string              { return m.nodeID }

func TestRoomLocalityClassification(t *testing.T) {
	const localNode = "node-alpha"

	tests := []struct {
		name          string
		inputs        []string
		outputs       []string
		wantLocalOnly bool
	}{
		{
			name:          "hardware endpoints only",
			inputs:        []string{"urn:audio:hw:alsa:c=0:d=0"},
			outputs:       []string{"urn:audio:hw:alsa:c=0:d=1"},
			wantLocalOnly: true,
		},
		{
			name:          "virtual loopback endpoints only",
			inputs:        []string{"urn:audio:virt:loopback:in"},
			outputs:       []string{"urn:audio:virt:loopback:out"},
			wantLocalOnly: true,
		},
		{
			name:          "matching local node prefix",
			inputs:        []string{"urn:audio:node:node-alpha:dev:mic"},
			outputs:       []string{"urn:audio:node:node-alpha:dev:speaker"},
			wantLocalOnly: true,
		},
		{
			name:          "legacy unqualified device names",
			inputs:        []string{"mic-1"},
			outputs:       []string{"speaker-out"},
			wantLocalOnly: true,
		},
		{
			name:          "mixed local hardware and local node prefix",
			inputs:        []string{"urn:audio:hw:alsa:c=0:d=0", "urn:audio:node:node-alpha:dev:linein"},
			outputs:       []string{"urn:audio:virt:monitor", "urn:audio:hw:pulse:d=0"},
			wantLocalOnly: true,
		},
		{
			name:          "remote input with local output",
			inputs:        []string{"urn:audio:node:node-remote-1:dev:mic"},
			outputs:       []string{"urn:audio:hw:alsa:c=0:d=1"},
			wantLocalOnly: false,
		},
		{
			name:          "local input with remote output",
			inputs:        []string{"urn:audio:hw:alsa:c=0:d=0"},
			outputs:       []string{"urn:audio:node:node-remote-2:dev:speaker"},
			wantLocalOnly: false,
		},
		{
			name:          "both remote inputs and outputs",
			inputs:        []string{"urn:audio:node:node-remote-1:dev:stream"},
			outputs:       []string{"urn:audio:node:node-remote-2:dev:dac"},
			wantLocalOnly: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRoom(RoomConfig{
				ID:          RoomID("loc-" + tc.name),
				LocalPeerID: localNode,
			})
			for _, in := range tc.inputs {
				if err := r.AddInput(in); err != nil {
					t.Fatalf("AddInput(%q) failed: %v", in, err)
				}
			}
			for _, out := range tc.outputs {
				if err := r.AddOutput(out); err != nil {
					t.Fatalf("AddOutput(%q) failed: %v", out, err)
				}
			}
			if got := r.IsLocalOnly(); got != tc.wantLocalOnly {
				t.Errorf("IsLocalOnly() = %v, want %v", got, tc.wantLocalOnly)
			}
		})
	}
}

func TestRoomLocalOnlyLeadershipImmunity(t *testing.T) {
	// Local node with lexicographically high ID
	const localNode = "node-zzz-local"

	disc := &mockDiscoveryProvider{
		nodeID: localNode,
		peers: []discovery.Peer{
			{ID: "node-aaa-remote", Addr: "127.0.0.1:4001", LastSeen: time.Now()},
		},
		livePeers: []discovery.Peer{
			{ID: "node-aaa-remote", Addr: "127.0.0.1:4001", LastSeen: time.Now()},
		},
	}

	room := NewRoom(RoomConfig{
		ID:          "local-immune-room",
		Name:        "Local Immune Room",
		LocalPeerID: localNode,
		Discovery:   disc,
	})

	if err := room.AddInput("urn:audio:hw:alsa:c=0:d=0"); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := room.AddOutput("urn:audio:hw:alsa:c=0:d=1"); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	if !room.IsLocalOnly() {
		t.Fatal("expected room to be local-only")
	}

	// 1. Initial leadership evaluation should assert localNode immediately
	room.recomputeLeader()
	if room.Leader() != localNode {
		t.Fatalf("expected leader %q, got %q", localNode, room.Leader())
	}
	if !room.IsLeader() {
		t.Fatal("expected IsLeader() == true")
	}
	if room.State() != RoomStateActive {
		t.Fatalf("expected RoomStateActive, got %v", room.State())
	}

	// 2. Add remote peer with lower ID ("node-aaa-remote" < "node-zzz-local")
	room.addMember("node-aaa-remote", &RoomMember{
		PeerID:   "node-aaa-remote",
		NodeID:   "node-aaa-remote",
		LastSeen: time.Now(),
	})

	// Recompute leader: local-only room must NOT be usurped by remote peer
	room.recomputeLeader()
	if room.Leader() != localNode {
		t.Fatalf("local room leader hijacked by remote peer: expected %q, got %q", localNode, room.Leader())
	}
	if room.State() != RoomStateActive {
		t.Fatalf("local room state changed from Active: got %v", room.State())
	}

	// 3. Remote peer becomes dead / removed from LivePeers
	disc.livePeers = nil
	// Remote peer LastSeen goes stale
	if m := room.Member("node-aaa-remote"); m != nil {
		m.LastSeen = time.Now().Add(-1 * time.Hour)
	}

	// Recompute leader again: local room must remain active, immune to remote failure
	room.recomputeLeader()
	if room.Leader() != localNode {
		t.Fatalf("expected leader to stay %q, got %q", localNode, room.Leader())
	}
	if room.State() != RoomStateActive {
		t.Fatalf("local room stalled in non-active state: %v", room.State())
	}
}

func TestRoomDynamicEndpointLocalityTransitions(t *testing.T) {
	const localNode = "node-mmm-local"
	const lowerRemoteNode = "node-aaa-remote"

	disc := &mockDiscoveryProvider{
		nodeID: localNode,
		peers: []discovery.Peer{
			{ID: lowerRemoteNode, Addr: "127.0.0.1:4002", LastSeen: time.Now()},
		},
		livePeers: []discovery.Peer{
			{ID: lowerRemoteNode, Addr: "127.0.0.1:4002", LastSeen: time.Now()},
		},
	}

	room := NewRoom(RoomConfig{
		ID:          "dynamic-transition-room",
		Name:        "Dynamic Transition Room",
		LocalPeerID: localNode,
		Discovery:   disc,
	})

	// Start purely local
	_ = room.AddInput("urn:audio:hw:alsa:c=0:d=0")
	_ = room.AddOutput("urn:audio:virt:loopback:0")
	room.addMember(localNode, &RoomMember{PeerID: localNode, NodeID: localNode, LastSeen: time.Now()})
	room.addMember(lowerRemoteNode, &RoomMember{PeerID: lowerRemoteNode, NodeID: lowerRemoteNode, LastSeen: time.Now()})

	if !room.IsLocalOnly() {
		t.Fatal("expected room to be local-only initially")
	}

	room.recomputeLeader()
	if room.Leader() != localNode {
		t.Fatalf("expected local leader %q, got %q", localNode, room.Leader())
	}

	// Dynamically transition to DistributedMesh by adding a remote input
	remoteURN := "urn:audio:node:" + lowerRemoteNode + ":dev:mic1"
	if err := room.AddInput(remoteURN); err != nil {
		t.Fatalf("AddInput remote: %v", err)
	}

	if room.IsLocalOnly() {
		t.Fatal("expected room to be distributed mesh after adding remote input")
	}

	// In distributed mesh, lowest-ID rule applies: lowerRemoteNode < localNode
	room.recomputeLeader()
	if room.Leader() != lowerRemoteNode {
		t.Fatalf("expected remote leader %q for distributed room, got %q", lowerRemoteNode, room.Leader())
	}

	// Dynamically transition back to LocalOnly by removing the remote input
	if err := room.RemoveInput(remoteURN); err != nil {
		t.Fatalf("RemoveInput remote: %v", err)
	}

	if !room.IsLocalOnly() {
		t.Fatal("expected room to return to local-only after removing remote input")
	}

	room.recomputeLeader()
	if room.Leader() != localNode {
		t.Fatalf("expected leader to revert to localNode %q, got %q", localNode, room.Leader())
	}
	if room.State() != RoomStateActive {
		t.Fatalf("expected RoomStateActive, got %v", room.State())
	}
}

type mockClusterProvider struct {
	snapshot *cluster.ClusterSnapshot
}

func (m *mockClusterProvider) GetSnapshot() *cluster.ClusterSnapshot {
	return m.snapshot
}

type mockDeviceResolver struct {
	devices map[string]audio.Device
}

func (m *mockDeviceResolver) Resolve(ref string) (audio.Device, bool) {
	d, ok := m.devices[ref]
	return d, ok
}

func TestRoom_ResolveDeviceEndpointWaterfall(t *testing.T) {
	const localNode = "local-node-1"

	clusterSnap := &cluster.ClusterSnapshot{
		ClusterDevices: []cluster.AudioDeviceSummary{
			{
				ID:     "remote-mic",
				URN:    "urn:audio:hw:remote-mic",
				NodeID: "remote-node-A",
			},
			{
				ID:     "collision-dev",
				URN:    "urn:audio:hw:collision-dev",
				NodeID: localNode,
			},
			{
				ID:     "collision-dev",
				URN:    "urn:audio:hw:collision-dev",
				NodeID: "remote-node-B",
			},
			{
				ID:     "multi-remote",
				URN:    "urn:audio:hw:multi-remote",
				NodeID: "node-z",
			},
			{
				ID:     "multi-remote",
				URN:    "urn:audio:hw:multi-remote",
				NodeID: "node-b",
			},
		},
	}

	cm := &mockClusterProvider{snapshot: clusterSnap}
	devRes := &mockDeviceResolver{
		devices: map[string]audio.Device{
			"urn:audio:hw:remote-mic": {URN: "urn:audio:hw:remote-mic", Name: "Hijack Attempt"},
			"urn:audio:hw:local-dev":  {URN: "urn:audio:hw:local-dev", Name: "Local Card"},
		},
	}

	disc := &mockDiscoveryProvider{
		nodeID: localNode,
		livePeers: []discovery.Peer{
			{ID: "single-live-peer"},
		},
	}

	room := NewRoom(RoomConfig{
		ID:             "room-waterfall",
		LocalPeerID:    localNode,
		ClusterManager: cm,
		DeviceResolver: devRes,
		Discovery:      disc,
	})

	tests := []struct {
		name        string
		input       string
		wantNode    string
		wantCleanID string
	}{
		{
			name:        "Step 1: explicit canonical prefix with slash",
			input:       "custom-node/urn:audio:hw:card",
			wantNode:    "custom-node",
			wantCleanID: "urn:audio:hw:card",
		},
		{
			name:        "Step 2: RFC 8141 node-scoped URN",
			input:       "urn:audio:node:stage-box:dev:urn:audio:hw:alsa:c=USB:d=0",
			wantNode:    "stage-box",
			wantCleanID: "urn:audio:hw:alsa:c=USB:d=0",
		},
		{
			name:        "Step 3: Cluster consensus takes precedence over local resolver (anti-hijacking)",
			input:       "urn:audio:hw:remote-mic",
			wantNode:    "remote-node-A",
			wantCleanID: "urn:audio:hw:remote-mic",
		},
		{
			name:        "Step 3: Cluster multi-node collision prefers local node if owned",
			input:       "urn:audio:hw:collision-dev",
			wantNode:    localNode,
			wantCleanID: "urn:audio:hw:collision-dev",
		},
		{
			name:        "Step 3: Cluster multi-node collision picks deterministic lowest remote node if local does not own",
			input:       "urn:audio:hw:multi-remote",
			wantNode:    "node-b",
			wantCleanID: "urn:audio:hw:multi-remote",
		},
		{
			name:        "Step 4: Local device resolver fallback when not in cluster snapshot",
			input:       "urn:audio:hw:local-dev",
			wantNode:    localNode,
			wantCleanID: "urn:audio:hw:local-dev",
		},
		{
			name:        "Step 5: Single live peer fallback for unknown device",
			input:       "unknown-device-123",
			wantNode:    "single-live-peer",
			wantCleanID: "unknown-device-123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotNode, gotClean := room.resolveDeviceEndpoint(tc.input)
			if gotNode != tc.wantNode || gotClean != tc.wantCleanID {
				t.Errorf("resolveDeviceEndpoint(%q) = (%q, %q), want (%q, %q)",
					tc.input, gotNode, gotClean, tc.wantNode, tc.wantCleanID)
			}
		})
	}
}

func TestRoomRouteCompiler_MultiNodeIdenticalHardwareNoCollision(t *testing.T) {
	routesFile := t.TempDir() + "/test_routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	room := NewRoom(RoomConfig{
		ID:          "room-multi-hw",
		Name:        "Multi HW Test",
		LocalPeerID: "local-node",
		Router:      rt,
	})

	// Inputs from two different nodes with identical soundcard device IDs
	in1 := "node-alpha/urn:audio:hw:name:default:37a8eec1"
	in2 := "node-beta/urn:audio:hw:name:default:37a8eec1"
	out1 := "node-alpha/urn:audio:hw:name:speaker:9999"

	if err := room.AddInput(in1); err != nil {
		t.Fatalf("AddInput(in1): %v", err)
	}
	if err := room.AddInput(in2); err != nil {
		t.Fatalf("AddInput(in2): %v", err)
	}
	if err := room.AddOutput(out1); err != nil {
		t.Fatalf("AddOutput(out1): %v", err)
	}

	room.mu.RLock()
	compiledCount := len(room.compiledRoutes)
	routes := make([]string, 0, compiledCount)
	for rID := range room.compiledRoutes {
		routes = append(routes, rID)
	}
	room.mu.RUnlock()

	// Both distinct node inputs must synthesize separate matrix routes without colliding
	if compiledCount != 2 {
		t.Fatalf("expected 2 distinct compiled routes for multi-node identical hardware, got %d: %v", compiledCount, routes)
	}

	for _, rID := range routes {
		mr, ok := rt.GetMatrix(rID)
		if !ok {
			t.Errorf("route %s not found in router", rID)
			continue
		}
		if mr.Destination.DeviceID != "urn:audio:hw:name:speaker:9999" {
			t.Errorf("unexpected destination device: %s", mr.Destination.DeviceID)
		}
	}
}
