package group

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

type mockStreamManager struct {
	mu           sync.Mutex
	started      []router.StreamRoute
	stopped      []string
	startNotify  chan string
	stopNotify   chan string
	stopLatency  time.Duration
	startLatency time.Duration
}

func newMockStreamManager() *mockStreamManager {
	return &mockStreamManager{
		startNotify: make(chan string, 100),
		stopNotify:  make(chan string, 100),
	}
}

func (m *mockStreamManager) StartRoute(route router.StreamRoute) error {
	m.mu.Lock()
	if m.startLatency > 0 {
		time.Sleep(m.startLatency)
	}
	m.started = append(m.started, route)
	m.mu.Unlock()
	select {
	case m.startNotify <- route.ID:
	default:
	}
	return nil
}

func (m *mockStreamManager) StopRoute(id string) error {
	m.mu.Lock()
	if m.stopLatency > 0 {
		time.Sleep(m.stopLatency)
	}
	m.stopped = append(m.stopped, id)
	m.mu.Unlock()
	select {
	case m.stopNotify <- id:
	default:
	}
	return nil
}

func (m *mockStreamManager) isStopped(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.stopped {
		if s == id {
			return true
		}
	}
	return false
}

// TestReactiveLifecycle_TeardownUnder50ms verifies ADR 0029 requirement:
// device removal must deterministically halt route streaming in < 50ms (sub-millisecond target).
func TestReactiveLifecycle_TeardownUnder50ms(t *testing.T) {
	tmpDir := t.TempDir()
	rt, err := router.New(filepath.Join(tmpDir, "routes.json"))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}

	sm := newMockStreamManager()
	localNode := "node-reactive-A"

	room := NewRoom(RoomConfig{
		ID:            "room-reactive-teardown",
		Name:          "Reactive Teardown Room",
		LocalPeerID:   localNode,
		Router:        rt,
		StreamManager: sm,
	})

	// Add input and output to compile active route
	room.AddInput("node-reactive-A/vdev-mic-1")
	room.AddOutput("node-reactive-A/vdev-spk-1")

	routes := room.CompiledRoutes()
	if len(routes) != 1 {
		t.Fatalf("expected 1 active route before teardown, got %d", len(routes))
	}
	activeRouteID := routes[0]

	// Create event hub and wire room as listener
	hub := audio.NewDeviceEventHub()
	hub.RegisterListener(room)

	// Measure teardown latency
	start := time.Now()

	// Dispatch DeviceRemoved event
	hub.Dispatch(audio.DeviceLifecycleEvent{
		Type:      audio.DeviceRemoved,
		DeviceURN: "urn:audio:virt:vdev-mic-1",
		LegacyID:  "vdev-mic-1",
		Timestamp: time.Now(),
	})

	// Wait for StopRoute signal with 50ms hard threshold
	select {
	case stoppedID := <-sm.stopNotify:
		elapsed := time.Since(start)
		if stoppedID != activeRouteID {
			t.Errorf("expected stopped route %q, got %q", activeRouteID, stoppedID)
		}
		if elapsed >= 50*time.Millisecond {
			t.Fatalf("ADR 0029 VIOLATION: teardown took %v (must be < 50ms)", elapsed)
		}
		t.Logf("SUCCESS: Route teardown completed in %v (< 50ms threshold)", elapsed)

	case <-time.After(50 * time.Millisecond):
		t.Fatalf("ADR 0029 VIOLATION: route teardown timed out after 50ms")
	}

	// Verify route is completely removed from room and router
	if len(room.CompiledRoutes()) != 0 {
		t.Errorf("expected 0 compiled routes in room, got %d", len(room.CompiledRoutes()))
	}
	if len(room.Inputs()) != 0 {
		t.Errorf("expected 0 inputs remaining, got %d", len(room.Inputs()))
	}
	if !sm.isStopped(activeRouteID) {
		t.Errorf("expected route %q to be marked stopped in stream manager", activeRouteID)
	}
}

// TestReactiveLifecycle_ReconnectUnder50ms verifies ADR 0029 fast-path route synthesis
// within 50ms upon device arrival (debounced over 5ms).
func TestReactiveLifecycle_ReconnectUnder50ms(t *testing.T) {
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

	tmpDir := t.TempDir()
	rt, err := router.New(filepath.Join(tmpDir, "routes-mgr.json"))
	if err != nil {
		t.Fatalf("failed to create router: %v", err)
	}
	sm := newMockStreamManager()

	// Create room
	roomInfo, err := mgr.CreateRoom("room-reconnect-fast", "Fast Reconnect Room")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	room, ok := mgr.GetRoom(roomInfo.ID)
	if !ok || room == nil {
		t.Fatalf("failed to retrieve room %s", roomInfo.ID)
	}
	room.SetRouter(rt)
	room.SetStreamManager(sm)

	// Bind input and output
	room.AddInput("local/hotplug-mic")
	room.AddOutput("local/hotplug-spk")

	// Trigger teardown by removing hotplug-mic
	hub := audio.NewDeviceEventHub()
	hub.RegisterListener(mgr)

	hub.Dispatch(audio.DeviceLifecycleEvent{
		Type:      audio.DeviceRemoved,
		DeviceURN: "urn:audio:virt:hotplug-mic",
		LegacyID:  "hotplug-mic",
		Timestamp: time.Now(),
	})

	// Verify route pruned
	time.Sleep(10 * time.Millisecond)
	if len(room.CompiledRoutes()) != 0 {
		t.Fatalf("expected 0 compiled routes after device removal, got %d", len(room.CompiledRoutes()))
	}

	// Re-add endpoint to room inputs (simulating operator intention or re-bound CRDT state)
	room.AddInput("local/hotplug-mic")

	// Now hotplug device arrival and measure fast-path reconnection latency
	start := time.Now()
	hub.Dispatch(audio.DeviceLifecycleEvent{
		Type:      audio.DeviceCreated,
		DeviceURN: "urn:audio:virt:hotplug-mic",
		LegacyID:  "hotplug-mic",
		Timestamp: time.Now(),
	})

	select {
	case startedID := <-sm.startNotify:
		elapsed := time.Since(start)
		if elapsed >= 50*time.Millisecond {
			t.Fatalf("ADR 0029 VIOLATION: fast-path reconnection took %v (must be < 50ms)", elapsed)
		}
		t.Logf("SUCCESS: Fast-path route reconnection completed in %v (< 50ms threshold, routeID=%s)", elapsed, startedID)

	case <-time.After(50 * time.Millisecond):
		t.Fatalf("ADR 0029 VIOLATION: fast-path route reconnection timed out after 50ms")
	}

	if len(room.CompiledRoutes()) != 1 {
		t.Errorf("expected 1 active compiled route after reconnection, got %d", len(room.CompiledRoutes()))
	}
}

// TestReactiveLifecycle_ConcurrentChurnNoDeadlock stress tests rapid lifecycle event churn
// across multiple goroutines to verify deadlock-free and race-free non-blocking teardown.
func TestReactiveLifecycle_ConcurrentChurnNoDeadlock(t *testing.T) {
	tmpDir := t.TempDir()
	rt, err := router.New(filepath.Join(tmpDir, "routes-churn.json"))
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	sm := newMockStreamManager()

	room := NewRoom(RoomConfig{
		ID:            "room-churn-stress",
		Name:          "Concurrent Churn Room",
		LocalPeerID:   "node-stress-local",
		Router:        rt,
		StreamManager: sm,
	})

	hub := audio.NewDeviceEventHub()
	hub.RegisterListener(room)

	var wg sync.WaitGroup
	workers := 8
	iterations := 50

	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			devURN := "urn:audio:virt:stress-dev"
			devLegacy := "stress-dev"

			for i := 0; i < iterations; i++ {
				if workerID%2 == 0 {
					room.AddInput("node-stress-local/" + devLegacy)
					room.AddOutput("node-stress-local/spk-sink")
					hub.Dispatch(audio.DeviceLifecycleEvent{
						Type:      audio.DeviceCreated,
						DeviceURN: devURN,
						LegacyID:  devLegacy,
						Timestamp: time.Now(),
					})
				} else {
					hub.Dispatch(audio.DeviceLifecycleEvent{
						Type:      audio.DeviceRemoved,
						DeviceURN: devURN,
						LegacyID:  devLegacy,
						Timestamp: time.Now(),
					})
					room.RemoveInput(devLegacy)
				}
				time.Sleep(100 * time.Microsecond)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("SUCCESS: Concurrent lifecycle churn completed with zero deadlocks")
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK detected during concurrent lifecycle churn")
	}
}
