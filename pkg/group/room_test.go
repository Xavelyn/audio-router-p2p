package group

import (
	"encoding/json"
	"testing"
	"time"
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
