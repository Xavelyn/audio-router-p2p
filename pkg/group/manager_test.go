package group

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

func TestDefaultRoomManager_Lifecycle(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}
	defer trans.Close()

	devIn := audio.NewVirtualAudioDevice()
	defer devIn.Close()
	devOut := audio.NewVirtualAudioDevice()
	defer devOut.Close()

	vp := NewVirtualDeviceProvider(devIn, devOut)
	mgr := NewDefaultRoomManager(nil, nil, nil, trans, vp)
	defer mgr.Close()

	// 1. Create Room
	info, err := mgr.CreateRoom("room-alpha", "Alpha Test Room")
	if err != nil {
		t.Fatalf("CreateRoom error: %v", err)
	}
	if info.ID != "room-alpha" || info.Name != "Alpha Test Room" {
		t.Fatalf("unexpected room info: %+v", info)
	}

	// 2. List Rooms
	rooms := mgr.ListRooms()
	if len(rooms) != 1 {
		t.Fatalf("expected 1 room, got %d", len(rooms))
	}
	if rooms[0].ID != "room-alpha" {
		t.Fatalf("expected room ID room-alpha, got %s", rooms[0].ID)
	}

	// 3. Join Room
	if err := mgr.JoinRoom("room-alpha"); err != nil {
		t.Fatalf("JoinRoom error: %v", err)
	}

	// Verify streamer was created and started
	gs, ok := mgr.GetStreamer("room-alpha")
	if !ok || gs == nil {
		t.Fatal("expected active streamer after JoinRoom")
	}
	if gs.State() != GroupPlaybackPlaying {
		t.Fatalf("expected streamer state playing, got %v", gs.State())
	}

	// Double join should be idempotent
	if err := mgr.JoinRoom("room-alpha"); err != nil {
		t.Fatalf("idempotent JoinRoom returned error: %v", err)
	}

	// 4. Leave Room
	if err := mgr.LeaveRoom("room-alpha"); err != nil {
		t.Fatalf("LeaveRoom error: %v", err)
	}

	_, ok = mgr.GetStreamer("room-alpha")
	if ok {
		t.Fatal("streamer should be inactive after LeaveRoom")
	}
}

func TestDefaultRoomManager_LeaderChangeCallback(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}
	defer trans.Close()

	vp := NewVirtualDeviceProvider(nil, nil)
	mgr := NewDefaultRoomManager(nil, nil, nil, trans, vp)
	defer mgr.Close()

	leaderEvents := make(chan string, 5)
	mgr.OnLeaderChange = func(roomID string, newLeader string) {
		leaderEvents <- newLeader
	}

	_, err = mgr.CreateRoom("room-callback", "Callback Room")
	if err != nil {
		t.Fatalf("CreateRoom error: %v", err)
	}

	room, ok := mgr.GetRoom("room-callback")
	if !ok {
		t.Fatal("room not found")
	}

	// Set initial local member "node-99"
	room.addMember("node-99", &RoomMember{PeerID: "node-99"})
	room.recomputeLeader()

	// Now introduce lower ID "node-01" to trigger a leader transfer
	room.addMember("node-01", &RoomMember{PeerID: "node-01"})
	room.recomputeLeader()

	select {
	case newLeader := <-leaderEvents:
		if newLeader != "node-01" {
			t.Fatalf("expected new leader node-01, got %s", newLeader)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for leader change callback")
	}
}

func TestDefaultRoomManager_Close(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}
	defer trans.Close()

	vp := NewVirtualDeviceProvider(nil, nil)
	mgr := NewDefaultRoomManager(nil, nil, nil, trans, vp)

	_, _ = mgr.CreateRoom("room-1", "Room 1")
	_, _ = mgr.CreateRoom("room-2", "Room 2")

	_ = mgr.JoinRoom("room-1")
	_ = mgr.JoinRoom("room-2")

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	// Subsequent operations should fail or be no-ops
	if _, err := mgr.CreateRoom("room-3", "Room 3"); err == nil {
		t.Fatal("expected error on CreateRoom after Close")
	}
	if err := mgr.JoinRoom("room-1"); err == nil {
		t.Fatal("expected error on JoinRoom after Close")
	}
}

func TestDefaultRoomManager_BindingsAndSync(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}
	defer trans.Close()

	routesFile := t.TempDir() + "/manager_routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	vp := NewVirtualDeviceProvider(nil, nil)
	mgr := NewDefaultRoomManager(nil, nil, nil, trans, vp)
	mgr.SetRouter(rt)
	defer mgr.Close()

	// 1. Create Room
	info, err := mgr.CreateRoom("room-bus-mgr", "Bus Manager Room")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if info.Synced {
		t.Fatal("expected room to be unsynced initially")
	}

	// 2. Add Inputs & Outputs
	if err := mgr.AddInput("room-bus-mgr", "in-dev-1"); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := mgr.AddOutput("room-bus-mgr", "out-dev-1"); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	// Verify route synthesized in router
	routeID := "room-room-bus-mgr-in-dev-1-out-dev-1"
	if _, ok := rt.GetMatrix(routeID); !ok {
		t.Fatalf("expected matrix route %s in router", routeID)
	}

	// 3. Set Room Sync
	if err := mgr.SetRoomSync("room-bus-mgr", true); err != nil {
		t.Fatalf("SetRoomSync: %v", err)
	}
	rooms := mgr.ListRooms()
	if len(rooms) != 1 || !rooms[0].Synced {
		t.Fatal("expected room to be synced")
	}

	// 4. Set Room Bindings (bulk update)
	if err := mgr.SetRoomBindings("room-bus-mgr", []string{"in-dev-2"}, []string{"out-dev-2"}, false); err != nil {
		t.Fatalf("SetRoomBindings: %v", err)
	}
	if _, ok := rt.GetMatrix(routeID); ok {
		t.Fatalf("old route %s should have been removed", routeID)
	}
	newRouteID := "room-room-bus-mgr-in-dev-2-out-dev-2"
	if _, ok := rt.GetMatrix(newRouteID); !ok {
		t.Fatalf("new route %s should exist", newRouteID)
	}

	// 5. Test errors on non-existent room
	if err := mgr.AddInput("non-existent", "dev"); err == nil {
		t.Fatal("expected error on AddInput for non-existent room")
	}
	if err := mgr.RemoveInput("non-existent", "dev"); err == nil {
		t.Fatal("expected error on RemoveInput for non-existent room")
	}
	if err := mgr.AddOutput("non-existent", "dev"); err == nil {
		t.Fatal("expected error on AddOutput for non-existent room")
	}
	if err := mgr.RemoveOutput("non-existent", "dev"); err == nil {
		t.Fatal("expected error on RemoveOutput for non-existent room")
	}
	if err := mgr.SetRoomSync("non-existent", true); err == nil {
		t.Fatal("expected error on SetRoomSync for non-existent room")
	}
	if err := mgr.SetRoomBindings("non-existent", nil, nil, true); err == nil {
		t.Fatal("expected error on SetRoomBindings for non-existent room")
	}
}

func TestDefaultRoomManager_ConcurrentAccess(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}
	defer trans.Close()

	routesFile := t.TempDir() + "/concurrent_routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	vp := NewVirtualDeviceProvider(nil, nil)
	mgr := NewDefaultRoomManager(nil, nil, nil, trans, vp)
	mgr.SetRouter(rt)
	defer mgr.Close()

	roomID := "race-room"
	_, err = mgr.CreateRoom(roomID, "Race Room")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	var wg sync.WaitGroup
	const iterations = 50

	// Goroutine 1: Rapidly add and remove inputs
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			devID := fmt.Sprintf("mic-%d", i%5)
			_ = mgr.AddInput(roomID, devID)
			_ = mgr.RemoveInput(roomID, devID)
		}
	}()

	// Goroutine 2: Rapidly add and remove outputs
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			devID := fmt.Sprintf("spk-%d", i%5)
			_ = mgr.AddOutput(roomID, devID)
			_ = mgr.RemoveOutput(roomID, devID)
		}
	}()

	// Goroutine 3: Toggle sync and bulk bindings
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.SetRoomSync(roomID, i%2 == 0)
			_ = mgr.SetRoomBindings(roomID, []string{"in-a", "in-b"}, []string{"out-a"}, i%2 == 1)
		}
	}()

	// Goroutine 4: Read room info concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.ListRooms()
		}
	}()

	wg.Wait()
}

func TestDefaultRoomManager_Persistence(t *testing.T) {
	tmpDir := t.TempDir()
	roomsFile := tmpDir + "/rooms.json"

	vp := NewVirtualDeviceProvider(nil, nil)
	mgr1 := NewDefaultRoomManager(nil, nil, nil, nil, vp)
	mgr1.SetRoomsFile(roomsFile)

	// Create room and configure bindings
	_, err := mgr1.CreateRoom("room-studio", "Main Studio")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	micURN := "urn:audio:hw:usb:v=041e:p=3232:s=0000005Z:d=1"
	spkURN := "urn:audio:hw:usb:v=041e:p=3232:s=0000005Z:d=0"

	if err := mgr1.AddInput("room-studio", micURN); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := mgr1.AddOutput("room-studio", spkURN); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}
	if err := mgr1.SetInputGain("room-studio", micURN, 1.25); err != nil {
		t.Fatalf("SetInputGain: %v", err)
	}
	if err := mgr1.SetOutputGain("room-studio", spkURN, 0.85); err != nil {
		t.Fatalf("SetOutputGain: %v", err)
	}
	if err := mgr1.SetRoomSync("room-studio", true); err != nil {
		t.Fatalf("SetRoomSync: %v", err)
	}

	mgr1.Close()

	// Spin up a brand new room manager instance and load from file
	mgr2 := NewDefaultRoomManager(nil, nil, nil, nil, vp)
	mgr2.SetRoomsFile(roomsFile)
	if err := mgr2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer mgr2.Close()

	rooms := mgr2.ListRooms()
	if len(rooms) != 1 {
		t.Fatalf("expected 1 restored room, got %d", len(rooms))
	}
	r := rooms[0]
	if r.ID != "room-studio" || r.Name != "Main Studio" {
		t.Fatalf("restored room ID/name mismatch: %+v", r)
	}
	normMicURN := "local/" + micURN
	normSpkURN := "local/" + spkURN
	if len(r.Inputs) != 1 || r.Inputs[0] != normMicURN {
		t.Fatalf("restored inputs mismatch: got %v, want [%s]", r.Inputs, normMicURN)
	}
	if len(r.Outputs) != 1 || r.Outputs[0] != normSpkURN {
		t.Fatalf("restored outputs mismatch: got %v, want [%s]", r.Outputs, normSpkURN)
	}
	if !r.Synced {
		t.Fatalf("restored synced mismatch: got false, want true")
	}
	if g, ok := r.InputGains[normMicURN]; !ok || g != 1.25 {
		t.Fatalf("restored input gain mismatch: got %v, want 1.25", r.InputGains)
	}
	if g, ok := r.OutputGains[normSpkURN]; !ok || g != 0.85 {
		t.Fatalf("restored output gain mismatch: got %v, want 0.85", r.OutputGains)
	}

	// Test deletion and persistence
	if err := mgr2.DeleteRoom("room-studio"); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	if len(mgr2.ListRooms()) != 0 {
		t.Fatalf("expected 0 rooms after DeleteRoom")
	}

	// Reload again to verify deletion persisted
	mgr3 := NewDefaultRoomManager(nil, nil, nil, nil, vp)
	mgr3.SetRoomsFile(roomsFile)
	if err := mgr3.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer mgr3.Close()

	if len(mgr3.ListRooms()) != 0 {
		t.Fatalf("expected 0 rooms in mgr3 after deletion persisted, got %d", len(mgr3.ListRooms()))
	}
}

func TestDefaultRoomManager_CentralizedFactoryInjection(t *testing.T) {
	cm := &mockClusterProvider{
		snapshot: &cluster.ClusterSnapshot{
			ClusterDevices: []cluster.AudioDeviceSummary{
				{ID: "dev-cm-1", NodeID: "remote-node"},
			},
		},
	}
	devRes := &mockDeviceResolver{
		devices: map[string]audio.Device{
			"dev-res-1": {URN: "dev-res-1", Name: "Mock Dev"},
		},
	}

	mgr := NewDefaultRoomManager(nil, nil, nil, nil)
	defer mgr.Close()

	mgr.SetClusterManager(cm)
	mgr.SetDeviceResolver(devRes)

	// 1. Verify CreateRoom uses centralized factory with DI
	info, err := mgr.CreateRoom("room-di-create", "Created Room")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	rCreate, ok := mgr.GetRoom(info.ID)
	if !ok || rCreate == nil {
		t.Fatalf("room %s not found", info.ID)
	}
	if rCreate.clusterMgr == nil {
		t.Errorf("rCreate.clusterMgr is nil")
	}
	if rCreate.devResolver == nil {
		t.Errorf("rCreate.devResolver is nil")
	}

	// 2. Verify applyRecordLocked (via MergeRecord) uses centralized factory with DI
	rec := RoomRecord{
		ID:        "room-di-gossip",
		Name:      "Gossip Room",
		Timestamp: HLT{WallTime: time.Now().UnixNano(), Counter: 1, NodeID: "node-remote"},
	}
	mgr.MergeRecord(rec)
	rGossip, ok := mgr.GetRoom("room-di-gossip")
	if !ok || rGossip == nil {
		t.Fatalf("room-di-gossip not found after MergeRecord")
	}
	if rGossip.clusterMgr == nil {
		t.Errorf("rGossip.clusterMgr is nil")
	}
	if rGossip.devResolver == nil {
		t.Errorf("rGossip.devResolver is nil")
	}

	// 3. Verify Load uses centralized factory with DI and normalizes un-prefixed devices
	roomsFile := t.TempDir() + "/load_di_rooms.json"
	mgr.SetRoomsFile(roomsFile)
	// Add an un-prefixed input/output to room-di-create before saving
	if err := mgr.AddInput(info.ID, "raw-unprefixed-input"); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := mgr.AddOutput(info.ID, "raw-unprefixed-output"); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	mgrLoaded := NewDefaultRoomManager(nil, nil, nil, nil)
	defer mgrLoaded.Close()
	mgrLoaded.SetClusterManager(cm)
	mgrLoaded.SetDeviceResolver(devRes)
	mgrLoaded.SetRoomsFile(roomsFile)
	if err := mgrLoaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	rLoaded, ok := mgrLoaded.GetRoom(info.ID)
	if !ok || rLoaded == nil {
		t.Fatalf("rLoaded not found")
	}
	if rLoaded.clusterMgr == nil {
		t.Errorf("rLoaded.clusterMgr is nil")
	}
	if rLoaded.devResolver == nil {
		t.Errorf("rLoaded.devResolver is nil")
	}

	// Verify un-prefixed devices normalized to "local/..."
	inputs := rLoaded.Inputs()
	if len(inputs) != 1 || inputs[0] != "local/raw-unprefixed-input" {
		t.Errorf("expected normalized input local/raw-unprefixed-input, got %v", inputs)
	}
	outputs := rLoaded.Outputs()
	if len(outputs) != 1 || outputs[0] != "local/raw-unprefixed-output" {
		t.Errorf("expected normalized output local/raw-unprefixed-output, got %v", outputs)
	}

	// 4. Verify SetClusterManager dynamically updates all existing rooms
	cm2 := &mockClusterProvider{
		snapshot: &cluster.ClusterSnapshot{
			ClusterDevices: []cluster.AudioDeviceSummary{
				{ID: "cm-new-device", NodeID: "new-node"},
			},
		},
	}
	mgrLoaded.SetClusterManager(cm2)
	if rLoaded.clusterMgr != cm2 {
		t.Errorf("SetClusterManager did not update rLoaded.clusterMgr")
	}
}
