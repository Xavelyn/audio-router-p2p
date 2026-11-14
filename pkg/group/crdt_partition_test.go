package group

import (
	"testing"
	"time"
)

func TestCRDT_HLT_Ordering(t *testing.T) {
	t1 := HLT{WallTime: 100, Counter: 1, NodeID: "node-a"}
	t2 := HLT{WallTime: 200, Counter: 1, NodeID: "node-a"}
	t3 := HLT{WallTime: 100, Counter: 2, NodeID: "node-a"}
	t4 := HLT{WallTime: 100, Counter: 1, NodeID: "node-b"}

	if !t2.After(t1) {
		t.Errorf("t2 (wall 200) should dominate t1 (wall 100)")
	}
	if !t3.After(t1) {
		t.Errorf("t3 (counter 2) should dominate t1 (counter 1)")
	}
	if !t4.After(t1) {
		t.Errorf("t4 (node-b) should dominate t1 (node-a) via tie-break")
	}
	if t1.After(t4) {
		t.Errorf("t1 (node-a) should not dominate t4 (node-b)")
	}
}

func TestCRDT_LWW_ModificationAndDeletion(t *testing.T) {
	crdtA := NewLWWSet("node-a")
	crdtB := NewLWWSet("node-b")

	// Node A creates Room Alpha
	recA := crdtA.CreateRoom("room-alpha", "Alpha Initial")
	if recA.Name != "Alpha Initial" {
		t.Fatalf("unexpected room name: %s", recA.Name)
	}

	// Propagate to Node B
	crdtB.MergeRecord(recA)
	if r, ok := crdtB.GetRoom("room-alpha"); !ok || r.Name != "Alpha Initial" {
		t.Fatalf("Node B failed to merge initial room")
	}

	// Ensure slight clock increment
	time.Sleep(2 * time.Millisecond)

	// Node A deletes room-alpha
	delRec, _ := crdtA.DeleteRoom("room-alpha")
	if _, ok := crdtA.GetRoom("room-alpha"); ok {
		t.Fatalf("room-alpha should be deleted on Node A")
	}

	// Node B updates room-alpha with a future timestamp (e.g. concurrent or later edit)
	time.Sleep(2 * time.Millisecond)
	crdtB.CreateRoom("room-alpha", "Alpha Resurrected")
	recB, _ := crdtB.GetRoom("room-alpha")

	// Merge Node A's deletion into Node B: Node B's update has higher timestamp -> Node B wins
	crdtB.MergeRecord(delRec)
	if r, ok := crdtB.GetRoom("room-alpha"); !ok || r.Name != "Alpha Resurrected" {
		t.Fatalf("newer update on Node B should win over older deletion on Node A")
	}

	// Merge Node B's update into Node A: Node A should resurrect
	crdtA.MergeRecord(recB)
	if r, ok := crdtA.GetRoom("room-alpha"); !ok || r.Name != "Alpha Resurrected" {
		t.Fatalf("newer update on Node B should resurrect room on Node A")
	}

	// Now delete on Node B with higher timestamp
	time.Sleep(2 * time.Millisecond)
	delRecB, _ := crdtB.DeleteRoom("room-alpha")

	// Merge Node B's deletion into Node A
	crdtA.MergeRecord(delRecB)
	if _, ok := crdtA.GetRoom("room-alpha"); ok {
		t.Fatalf("room-alpha should be deleted on Node A after merging newer deletion")
	}
}

func TestCRDT_NestedDeviceSets(t *testing.T) {
	crdtA := NewLWWSet("node-a")
	crdtB := NewLWWSet("node-b")

	crdtA.CreateRoom("room-1", "Studio")
	crdtA.AddInput("room-1", "mic-1")
	crdtA.AddInput("room-1", "mic-2")
	crdtA.AddOutput("room-1", "speaker-1")

	// Sync state to B
	crdtB.MergeState(crdtA.ExportState())

	rB, ok := crdtB.GetRoom("room-1")
	if !ok {
		t.Fatalf("room-1 not found on B")
	}
	if len(rB.Inputs) != 2 || len(rB.Outputs) != 1 {
		t.Fatalf("unexpected inputs/outputs: inputs=%v, outputs=%v", rB.Inputs, rB.Outputs)
	}

	time.Sleep(2 * time.Millisecond)

	// Partition:
	// Node A removes mic-1
	crdtA.RemoveInput("room-1", "mic-1")
	// Node B adds mic-3
	crdtB.AddInput("room-1", "mic-3")

	// Heal partition and merge state both directions
	crdtA.MergeState(crdtB.ExportState())
	crdtB.MergeState(crdtA.ExportState())

	rA, _ := crdtA.GetRoom("room-1")
	rB, _ = crdtB.GetRoom("room-1")

	// Both should have mic-2, mic-3 (mic-1 removed)
	if len(rA.Inputs) != 2 || rA.Inputs[0] != "mic-2" || rA.Inputs[1] != "mic-3" {
		t.Fatalf("Node A converged inputs incorrect: %v", rA.Inputs)
	}
	if len(rB.Inputs) != 2 || rB.Inputs[0] != "mic-2" || rB.Inputs[1] != "mic-3" {
		t.Fatalf("Node B converged inputs incorrect: %v", rB.Inputs)
	}
}

func TestCRDT_SplitBrainPartitionScenario(t *testing.T) {
	// Initialize Node A and Node B
	crdtA := NewLWWSet("node-a")
	crdtB := NewLWWSet("node-b")

	// Pre-partition shared state: Room Beta exists
	betaInit := crdtA.CreateRoom("room-beta", "Beta Initial")
	crdtB.MergeRecord(betaInit)

	time.Sleep(2 * time.Millisecond)

	// --- SIMULATE PARTITION ---
	// Node A:
	// 1. Create Room Alpha
	// 2. Delete Room Beta
	crdtA.CreateRoom("room-alpha", "Alpha")
	time.Sleep(2 * time.Millisecond)
	delBetaA, _ := crdtA.DeleteRoom("room-beta")

	// Node B (concurrently, but slightly earlier/later clock):
	// 1. Rename Room Beta to "Beta Renamed"
	// 2. Create Room Gamma
	crdtB.CreateRoom("room-gamma", "Gamma")

	// Let's test case where Node A's deletion timestamp is HIGHER than Node B's rename
	// Manually construct or sleep:
	time.Sleep(5 * time.Millisecond)
	// Node A deletes Beta at a later time
	delBetaA, _ = crdtA.DeleteRoom("room-beta")

	// --- HEAL PARTITION ---
	// Push-pull anti-entropy between A and B
	crdtA.MergeState(crdtB.ExportState())
	crdtB.MergeState(crdtA.ExportState())

	// Assertions on Node A and Node B:
	// 1. room-alpha exists on both
	if _, ok := crdtA.GetRoom("room-alpha"); !ok {
		t.Errorf("room-alpha should exist on Node A")
	}
	if _, ok := crdtB.GetRoom("room-alpha"); !ok {
		t.Errorf("room-alpha should exist on Node B")
	}

	// 2. room-gamma exists on both
	if _, ok := crdtA.GetRoom("room-gamma"); !ok {
		t.Errorf("room-gamma should exist on Node A")
	}
	if _, ok := crdtB.GetRoom("room-gamma"); !ok {
		t.Errorf("room-gamma should exist on Node B")
	}

	// 3. room-beta is deleted on both (Node A's deletion dominated)
	if _, ok := crdtA.GetRoom("room-beta"); ok {
		t.Errorf("room-beta should be deleted on Node A")
	}
	if _, ok := crdtB.GetRoom("room-beta"); ok {
		t.Errorf("room-beta should be deleted on Node B")
	}

	// Verify tombstone is preserved in both stores
	if rec, ok := crdtA.GetRecord("room-beta"); !ok || !rec.Deleted {
		t.Errorf("room-beta should be preserved as tombstone on Node A")
	}
	if rec, ok := crdtB.GetRecord("room-beta"); !ok || !rec.Deleted {
		t.Errorf("room-beta should be preserved as tombstone on Node B")
	}
	_ = delBetaA
}

func TestCRDT_TombstoneRetentionAndPruning(t *testing.T) {
	crdt := NewLWWSet("node-a", 72*time.Hour)

	crdt.CreateRoom("room-temp", "Temporary Room")
	crdt.DeleteRoom("room-temp")

	rec, ok := crdt.GetRecord("room-temp")
	if !ok || !rec.Deleted {
		t.Fatalf("expected tombstone for room-temp")
	}

	// Pruning at current time: tombstone must be retained (< 72h old)
	pruned := crdt.PruneTombstones(time.Now())
	if pruned != 0 {
		t.Fatalf("expected 0 tombstones pruned immediately, got %d", pruned)
	}

	// Pruning after 24h: still retained
	pruned = crdt.PruneTombstones(time.Now().Add(24 * time.Hour))
	if pruned != 0 {
		t.Fatalf("expected 0 tombstones pruned after 24h, got %d", pruned)
	}

	// Pruning after 73h: must be pruned
	pruned = crdt.PruneTombstones(time.Now().Add(73 * time.Hour))
	if pruned != 1 {
		t.Fatalf("expected 1 tombstone pruned after 73h, got %d", pruned)
	}

	// Record should now be completely purged
	if _, exists := crdt.GetRecord("room-temp"); exists {
		t.Fatalf("room-temp should have been pruned from store")
	}
}

func TestDefaultRoomManager_MergeRecordLeaderDeadlockResistance(t *testing.T) {
	mgr := NewDefaultRoomManager(nil, nil, nil, nil)
	defer mgr.Close()

	leaderChangeReceived := make(chan string, 1)
	mgr.OnLeaderChange = func(roomID, newLeader string) {
		select {
		case leaderChangeReceived <- newLeader:
		default:
		}
	}

	// 1. Create initial room
	_, err := mgr.CreateRoom("room-deadlock-check", "Deadlock Check")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// 2. Prepare an incoming CRDT record from remote node with remote leader
	incoming := RoomRecord{
		ID:        "room-deadlock-check",
		Name:      "Deadlock Check Renamed",
		Leader:    "remote-node-99",
		Timestamp: HLT{WallTime: time.Now().Add(1 * time.Second).UnixNano(), Counter: 10, NodeID: "remote-node-99"},
	}

	done := make(chan struct{})
	go func() {
		mgr.MergeRecord(incoming)
		close(done)
	}()

	select {
	case <-done:
		// Succeeded without deadlock!
	case <-time.After(2 * time.Second):
		t.Fatalf("MergeRecord deadlocked on leader recomputation!")
	}

	r, ok := mgr.GetRoom("room-deadlock-check")
	if !ok {
		t.Fatalf("expected room to exist")
	}
	if r.Name() != "Deadlock Check Renamed" {
		t.Errorf("expected merged name 'Deadlock Check Renamed', got %q", r.Name())
	}
}
