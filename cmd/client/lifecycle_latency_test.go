package main

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestLifecycleLatency_SilenceCutoffUnder10ms verifies ADR 0029 requirement:
// < 10ms silence cutoff upon endpoint deletion/unbinding during active playback.
func TestLifecycleLatency_SilenceCutoffUnder10ms(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		RoutesFile:  filepath.Join(tmpDir, "routes.json"),
		RoomsFile:   filepath.Join(tmpDir, "rooms.json"),
		UseEmulator: false,
	}

	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	disc, err := discovery.New(cfg, "test-node-latency-1")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	sm := syncmgr.NewManager(cfg, disc)
	defer sm.Close()

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)
	defer streamMgr.StopAll()

	inDev := audio.NewVirtualAudioDevice()
	defer inDev.Close()
	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	inURN := "urn:audio:virt:test-mic-cutoff"
	outURN := "urn:audio:virt:test-spk-cutoff"

	audio.GlobalResolver().Register(audio.Device{
		ID:                inURN,
		URN:               inURN,
		Name:              "Test Mic Cutoff",
		MaxInput:          2,
		MaxOutput:         0,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(inURN, inDev)

	audio.GlobalResolver().Register(audio.Device{
		ID:                outURN,
		URN:               outURN,
		Name:              "Test Spk Cutoff",
		MaxInput:          0,
		MaxOutput:         2,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(outURN, outDev)

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streamMgr)
	defer roomMgr.Close()

	roomID := "room-latency-cutoff"
	if _, err := roomMgr.CreateRoom(roomID, "Cutoff Room"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	if err := roomMgr.AddInput(roomID, inURN); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := roomMgr.AddOutput(roomID, outURN); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	// Verify room compiled routes and stream started
	room, ok := roomMgr.GetRoom(roomID)
	if !ok {
		t.Fatalf("room %s not found in manager", roomID)
	}
	compiled := room.CompiledRoutes()
	if len(compiled) != 1 {
		t.Fatalf("expected 1 compiled route, got %d", len(compiled))
	}
	activeRouteID := compiled[0]

	// Feed continuous audio
	var feeding atomic.Bool
	feeding.Store(true)
	defer feeding.Store(false)

	sineBlock := generateSineBlock(48000, 2, 480, 440.0, 0.707) // 10ms frame
	go func() {
		for feeding.Load() {
			_, _ = inDev.InjectPush(sineBlock)
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Wait until playback is actively received by outDev
	deadline := time.Now().Add(1 * time.Second)
	for len(outDev.CapturedBytes()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for active playback on output device")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Verify route is present in streamManager
	streamMgr.mu.Lock()
	_, existsBefore := streamMgr.routes[activeRouteID]
	streamMgr.mu.Unlock()
	if !existsBefore {
		t.Fatalf("expected active route %s in streamManager before cutoff", activeRouteID)
	}

	// Trigger endpoint deletion/unbinding and measure silence cutoff latency
	start := time.Now()
	audio.GlobalEventHub().Dispatch(audio.DeviceLifecycleEvent{
		Type:      audio.DeviceRemoved,
		DeviceURN: outURN,
		Timestamp: time.Now(),
	})

	// Wait for route to be stopped in stream manager
	stopped := false
	for time.Since(start) < 20*time.Millisecond {
		streamMgr.mu.Lock()
		_, exists := streamMgr.routes[activeRouteID]
		streamMgr.mu.Unlock()
		if !exists {
			stopped = true
			break
		}
		time.Sleep(50 * time.Microsecond)
	}

	cutoffDuration := time.Since(start)
	if !stopped {
		t.Fatalf("route %s was not stopped", activeRouteID)
	}

	if cutoffDuration >= 10*time.Millisecond {
		t.Fatalf("ADR 0029 VIOLATION: silence cutoff took %v (must be < 10ms)", cutoffDuration)
	}
	t.Logf("SUCCESS: Silence cutoff upon endpoint deletion/unbinding completed in %v (< 10ms threshold)", cutoffDuration)
}

// TestLifecycleLatency_NonBlockingConcurrentStreamStopUnder15ms verifies ADR 0029 requirement:
// Non-blocking stopping of multiple concurrent streams completes in < 15ms.
func TestLifecycleLatency_NonBlockingConcurrentStreamStopUnder15ms(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		RoutesFile:  filepath.Join(tmpDir, "routes.json"),
		UseEmulator: false,
	}

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	disc, err := discovery.New(cfg, "test-node-latency-stop")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)

	const numStreams = 15
	for i := 0; i < numStreams; i++ {
		inDev := audio.NewVirtualAudioDevice()
		outDev := audio.NewVirtualAudioDevice()
		defer inDev.Close()
		defer outDev.Close()

		inURN := fmt.Sprintf("urn:audio:virt:stop-in-%d", i)
		outURN := fmt.Sprintf("urn:audio:virt:stop-out-%d", i)

		audio.GlobalResolver().Register(audio.Device{
			ID:                inURN,
			URN:               inURN,
			Name:              fmt.Sprintf("Stop In %d", i),
			MaxInput:          2,
			DefaultSampleRate: 48000,
			Status:            "active",
		})
		audio.GlobalResolver().Rebind(inURN, inDev)

		audio.GlobalResolver().Register(audio.Device{
			ID:                outURN,
			URN:               outURN,
			Name:              fmt.Sprintf("Stop Out %d", i),
			MaxOutput:         2,
			DefaultSampleRate: 48000,
			Status:            "active",
		})
		audio.GlobalResolver().Rebind(outURN, outDev)

		sr := router.StreamRoute{
			ID:              fmt.Sprintf("route-stop-%d", i),
			SourceDeviceURN: inURN,
			DestDeviceURN:   outURN,
			PeerID:          "local",
			SampleRate:      48000,
			Channels:        2,
		}
		if err := streamMgr.StartRoute(sr); err != nil {
			t.Fatalf("failed to start stream %d: %v", i, err)
		}
	}

	streamMgr.mu.Lock()
	count := len(streamMgr.routes)
	streamMgr.mu.Unlock()
	if count != numStreams {
		t.Fatalf("expected %d active streams, got %d", numStreams, count)
	}

	start := time.Now()
	streamMgr.StopAll()
	elapsed := time.Since(start)

	if elapsed >= 15*time.Millisecond {
		t.Fatalf("ADR 0029 VIOLATION: StopAll for %d streams took %v (must be < 15ms)", numStreams, elapsed)
	}
	t.Logf("SUCCESS: Non-blocking stopping of %d concurrent streams completed in %v (< 15ms threshold)", numStreams, elapsed)

	streamMgr.mu.Lock()
	remaining := len(streamMgr.routes)
	streamMgr.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected 0 remaining streams, got %d", remaining)
	}
}

// TestLifecycleLatency_PlayoutResumptionUnder50ms verifies ADR 0029 requirement:
// Playout resumption upon endpoint connection/recreation completes in < 50ms.
func TestLifecycleLatency_PlayoutResumptionUnder50ms(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		RoutesFile:  filepath.Join(tmpDir, "routes.json"),
		RoomsFile:   filepath.Join(tmpDir, "rooms.json"),
		UseEmulator: false,
	}

	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	disc, err := discovery.New(cfg, "test-node-latency-resume")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	sm := syncmgr.NewManager(cfg, disc)
	defer sm.Close()

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)
	defer streamMgr.StopAll()

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streamMgr)
	defer roomMgr.Close()

	inDev := audio.NewVirtualAudioDevice()
	defer inDev.Close()
	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	inURN := "urn:audio:virt:resume-mic"
	outURN := "urn:audio:virt:resume-spk"

	// Register output device
	audio.GlobalResolver().Register(audio.Device{
		ID:                outURN,
		URN:               outURN,
		Name:              "Resume Speaker",
		MaxOutput:         2,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(outURN, outDev)

	// Register input device in resolver
	audio.GlobalResolver().Register(audio.Device{
		ID:                inURN,
		URN:               inURN,
		Name:              "Resume Mic",
		MaxInput:          2,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(inURN, inDev)

	roomID := "room-latency-resumption"
	if _, err := roomMgr.CreateRoom(roomID, "Resumption Room"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// Bind input and output to room
	if err := roomMgr.AddInput(roomID, inURN); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := roomMgr.AddOutput(roomID, outURN); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	// Trigger device teardown via DeviceRemoved event
	audio.GlobalEventHub().Dispatch(audio.DeviceLifecycleEvent{
		Type:      audio.DeviceRemoved,
		DeviceURN: inURN,
		LegacyID:  "resume-mic",
		Timestamp: time.Now(),
	})

	time.Sleep(10 * time.Millisecond)

	// Ensure routes were torn down
	room, ok := roomMgr.GetRoom(roomID)
	if !ok {
		t.Fatalf("room %s not found", roomID)
	}
	if len(room.CompiledRoutes()) != 0 {
		t.Fatalf("expected 0 compiled routes after teardown, got %d", len(room.CompiledRoutes()))
	}

	// Re-add input endpoint to room bindings (waiting for hotplug arrival)
	if err := roomMgr.AddInput(roomID, inURN); err != nil {
		t.Fatalf("AddInput: %v", err)
	}

	// Inject continuous audio into inDev
	sineBlock := generateSineBlock(48000, 2, 480, 440.0, 0.707)
	var feeding atomic.Bool
	feeding.Store(true)
	defer feeding.Store(false)

	go func() {
		for feeding.Load() {
			_, _ = inDev.InjectPush(sineBlock)
			time.Sleep(1 * time.Millisecond)
		}
	}()

	start := time.Now()
	// Dispatch DeviceCreated event through GlobalEventHub
	audio.GlobalEventHub().Dispatch(audio.DeviceLifecycleEvent{
		Type:      audio.DeviceCreated,
		DeviceURN: inURN,
		LegacyID:  "resume-mic",
		Timestamp: time.Now(),
	})

	// Wait until playout resumes (active route in streamManager and outDev receiving audio)
	resumed := false
	for time.Since(start) < 50*time.Millisecond {
		streamMgr.mu.Lock()
		activeCount := len(streamMgr.routes)
		streamMgr.mu.Unlock()
		if activeCount > 0 && len(outDev.CapturedBytes()) > 0 {
			resumed = true
			break
		}
		time.Sleep(500 * time.Microsecond)
	}

	elapsed := time.Since(start)
	if !resumed {
		t.Fatalf("ADR 0029 VIOLATION: playout resumption timed out after %v (must be < 50ms)", elapsed)
	}
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("ADR 0029 VIOLATION: playout resumption took %v (must be < 50ms)", elapsed)
	}
	t.Logf("SUCCESS: Playout resumption upon endpoint connection/recreation completed in %v (< 50ms threshold)", elapsed)
}

// TestLifecycleLatency_BurstEventAggregationDebounce verifies ADR 0029 requirement:
// Burst device arrival events are aggregated in the debounce window without dropping any events.
func TestLifecycleLatency_BurstEventAggregationDebounce(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		RoutesFile:  filepath.Join(tmpDir, "routes.json"),
		RoomsFile:   filepath.Join(tmpDir, "rooms.json"),
		UseEmulator: false,
	}

	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	disc, err := discovery.New(cfg, "test-node-burst")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	sm := syncmgr.NewManager(cfg, disc)
	defer sm.Close()

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)
	defer streamMgr.StopAll()

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streamMgr)
	defer roomMgr.Close()

	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()
	outURN := "urn:audio:virt:burst-spk"

	audio.GlobalResolver().Register(audio.Device{
		ID:                outURN,
		URN:               outURN,
		Name:              "Burst Spk",
		MaxOutput:         2,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(outURN, outDev)

	roomID := "room-burst-aggregation"
	if _, err := roomMgr.CreateRoom(roomID, "Burst Room"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if err := roomMgr.AddOutput(roomID, outURN); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	const numBurst = 3
	var inURNs []string
	for i := 1; i <= numBurst; i++ {
		inDev := audio.NewVirtualAudioDevice()
		defer inDev.Close()
		urn := fmt.Sprintf("urn:audio:virt:burst-mic-%d", i)
		inURNs = append(inURNs, urn)

		audio.GlobalResolver().Register(audio.Device{
			ID:                urn,
			URN:               urn,
			Name:              fmt.Sprintf("Burst Mic %d", i),
			MaxInput:          2,
			DefaultSampleRate: 48000,
			Status:            "active",
		})
		audio.GlobalResolver().Rebind(urn, inDev)

		if err := roomMgr.AddInput(roomID, urn); err != nil {
			t.Fatalf("AddInput %s failed: %v", urn, err)
		}
	}

	// Dispatch burst of DeviceCreated events with sub-millisecond spacing
	start := time.Now()
	for _, urn := range inURNs {
		audio.GlobalEventHub().Dispatch(audio.DeviceLifecycleEvent{
			Type:      audio.DeviceCreated,
			DeviceURN: urn,
			Timestamp: time.Now(),
		})
		time.Sleep(500 * time.Microsecond)
	}

	// Wait for debounce window (5ms) + processing time
	deadline := time.Now().Add(50 * time.Millisecond)
	allStarted := false
	for time.Now().Before(deadline) {
		streamMgr.mu.Lock()
		activeCount := len(streamMgr.routes)
		streamMgr.mu.Unlock()

		if activeCount == numBurst {
			allStarted = true
			break
		}
		time.Sleep(1 * time.Millisecond)
	}

	elapsed := time.Since(start)
	if !allStarted {
		t.Fatalf("ADR 0029 VIOLATION: not all %d burst routes were started after %v (events were dropped!)", numBurst, elapsed)
	}
	t.Logf("SUCCESS: Burst aggregation of %d events succeeded in %v without dropped events", numBurst, elapsed)
}
