package group

import (
	"encoding/json"
	"testing"
	"time"

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
