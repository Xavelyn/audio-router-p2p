//go:build noaudio

package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestAllFeaturesE2E exercises route status diagnostics, probe calibration,
// per-channel alignment persistence, network condition simulation, and
// room latency presets working together on an active stream manager.
func TestAllFeaturesE2E(t *testing.T) {
	rawTrans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rawTrans.Close()

	// 1. Wrap transport in NetworkSimulator
	sim := transport.NewNetworkSimulator(rawTrans, transport.SimProfile{})
	defer sim.Close()

	cfg := &config.Config{
		AuthToken:        "e2e-feat-token",
		UseEmulator:      true,
		AnnounceInterval: time.Second,
		PeerTTL:          3 * time.Second,
		SyncInterval:     time.Second,
	}

	disc, err := discovery.New(cfg, "e2e-node")
	if err != nil {
		t.Fatal(err)
	}
	defer disc.Close()

	m := newStreamManager(cfg, sim, []string{"127.0.0.1"}, disc)
	defer m.StopAll()

	rtFile := filepath.Join(t.TempDir(), "routes.json")
	rt, err := router.New(rtFile)
	if err != nil {
		t.Fatal(err)
	}
	m.SetRouter(rt)

	// 2. Start a local loopback route
	routeID := "e2e-route-1"
	if err := m.StartRoute(router.StreamRoute{
		ID:           routeID,
		SourceDevice: 0,
		Destinations: []string{rawTrans.Addr()},
		DestDevice:   1,
		SampleRate:   48000,
		Channels:     2,
	}); err != nil {
		t.Fatalf("StartRoute: %v", err)
	}

	// Wait for route to enter playing state
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st, ok := m.RouteStatus(routeID); ok && st.Playing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("e2e-route-1 did not start playing within deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 3. Feature A: Route Playback Status Diagnostics
	st, ok := m.RouteStatus(routeID)
	if !ok {
		t.Fatal("RouteStatus returned not found for live route")
	}
	if !st.Playing {
		t.Errorf("expected Playing=true, got %+v", st)
	}
	if st.Direction != "sendrecv" {
		t.Errorf("expected Direction=sendrecv, got %s", st.Direction)
	}
	if st.Summary != "playing" {
		t.Errorf("expected Summary='playing', got %s", st.Summary)
	}
	if st.LastInput == nil {
		t.Errorf("expected non-nil last input timestamp, got input=%v", st.LastInput)
	}
	if len(st.Peers) == 0 || st.Peers[0].LastSend == nil {
		t.Errorf("expected peer with non-nil last send, got peers=%+v", st.Peers)
	}

	// 4. Feature D: Loopback Calibration (Measure + Apply)
	calibReq := v1.CalibrateRequest{
		FreqHz:     4200,
		Amplitude:  16000,
		DurationMS: 500,
		Apply:      true,
	}
	calibRes, found, err := m.CalibrateRoute(routeID, calibReq)
	if !found || err != nil {
		t.Fatalf("CalibrateRoute failed: found=%v err=%v", found, err)
	}
	if !calibRes.Detected {
		t.Fatalf("probe tone was not detected: %+v", calibRes)
	}
	if !calibRes.Applied {
		t.Errorf("expected Applied=true")
	}
	if len(calibRes.ChannelDelaysMS) != 2 || len(calibRes.OffsetsMS) != 2 {
		t.Fatalf("expected 2 channel delays and 2 offsets, got %+v", calibRes)
	}
	for i, d := range calibRes.ChannelDelaysMS {
		if d <= 0 || d > 1000 {
			t.Errorf("channel %d delay %f ms out of bounds", i, d)
		}
	}

	// Verify alignment offsets were persisted to the route table
	persistedRoute, exists := rt.Get(routeID)
	if !exists {
		t.Fatalf("route %s not in route table", routeID)
	}
	if len(persistedRoute.ChannelDelaysMS) != 2 {
		t.Fatalf("persisted ChannelDelaysMS len != 2: %+v", persistedRoute.ChannelDelaysMS)
	}

	// 5. Feature D: Manual Alignment Update
	manualOffsets := []float64{12.0, 4.0}
	applied, found, err := m.SetRouteAlignment(routeID, manualOffsets)
	if !found || err != nil {
		t.Fatalf("SetRouteAlignment failed: found=%v err=%v", found, err)
	}
	if len(applied) != 2 || applied[0] < 11.5 || applied[0] > 12.5 {
		t.Errorf("unexpected applied offsets: %+v", applied)
	}

	curOffsets, found := m.GetRouteAlignment(routeID)
	if !found || len(curOffsets) != 2 || curOffsets[0] < 11.5 || curOffsets[0] > 12.5 {
		t.Errorf("unexpected GetRouteAlignment: found=%v offsets=%+v", found, curOffsets)
	}

	// 6. Feature B: Network Simulator Runtime Profiles
	initProfiles := sim.Profiles()
	if initProfiles.In.Enabled || initProfiles.Out.Enabled {
		t.Errorf("simulator should start disabled: %+v", initProfiles)
	}

	// Apply degradation on egress and ingress
	sim.SetProfiles(transport.SimProfiles{
		In: transport.SimProfile{
			Enabled:    true,
			LossPct:    10.0,
			LatencyMS:  15.0,
			JitterMS:   5.0,
			ReorderPct: 2.0,
		},
		Out: transport.SimProfile{
			Enabled:       true,
			LossPct:       15.0,
			LatencyMS:     20.0,
			JitterMS:      10.0,
			DuplicatePct:  5.0,
			BandwidthKbps: 1024.0,
		},
	})

	activeProfiles := sim.Profiles()
	if !activeProfiles.In.Enabled || activeProfiles.In.LossPct != 10.0 {
		t.Errorf("ingress profile mismatch: %+v", activeProfiles.In)
	}
	if !activeProfiles.Out.Enabled || activeProfiles.Out.BandwidthKbps != 1024.0 {
		t.Errorf("egress profile mismatch: %+v", activeProfiles.Out)
	}

	// Reset simulator to disabled
	sim.SetProfiles(transport.SimProfiles{})
	if sim.Profiles().In.Enabled || sim.Profiles().Out.Enabled {
		t.Errorf("simulator reset failed")
	}

	// 7. Feature C: Room Latency Presets
	room := group.NewRoom(group.RoomConfig{
		ID:            "stage-a",
		Name:          "Stage A Room",
		Router:        rt,
		StreamManager: m,
	})
	if room.LatencyPreset() != "balanced" {
		t.Errorf("default preset should be balanced, got %s", room.LatencyPreset())
	}

	// Change to low preset
	if err := room.SetLatencyPreset("low"); err != nil {
		t.Fatalf("SetLatencyPreset(low): %v", err)
	}
	if room.LatencyPreset() != "low" {
		t.Errorf("expected preset low, got %s", room.LatencyPreset())
	}

	// Change to reliable preset
	if err := room.SetLatencyPreset("reliable"); err != nil {
		t.Fatalf("SetLatencyPreset(reliable): %v", err)
	}
	if room.LatencyPreset() != "reliable" {
		t.Errorf("expected preset reliable, got %s", room.LatencyPreset())
	}

	// 8. Stop route and verify status reflects stopped
	if err := m.StopRoute(routeID); err != nil {
		t.Fatalf("StopRoute: %v", err)
	}
	if _, ok := m.RouteStatus(routeID); ok {
		t.Errorf("stopped route should return ok=false from RouteStatus")
	}
}
