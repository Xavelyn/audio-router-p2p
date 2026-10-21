package group

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
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
