package main

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api/telemetry"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
	"github.com/audio-router-p2p/pkg/transport"
)

// generateSineBlock produces 16-bit signed PCM sine wave samples.
func generateSineBlock(sampleRate, channels, frames int, freq float64, amp float64) []byte {
	buf := make([]byte, frames*channels*2)
	for i := 0; i < frames; i++ {
		sampleVal := int16(amp * 32767.0 * math.Sin(2.0*math.Pi*freq*float64(i)/float64(sampleRate)))
		for c := 0; c < channels; c++ {
			idx := (i*channels + c) * 2
			binary.LittleEndian.PutUint16(buf[idx:], uint16(sampleVal))
		}
	}
	return buf
}

func TestIntraRoomRouting_SoundAndSignalLevels(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		RoutesFile:  tmpDir + "/routes.json",
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

	disc, err := discovery.New(cfg, "test-node-intra")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	sm := syncmgr.NewManager(cfg, disc)
	defer sm.Close()

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)
	defer streamMgr.StopAll()

	// Virtual audio devices for test
	inDev := audio.NewVirtualAudioDevice()
	defer inDev.Close()
	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	// Register devices in GlobalResolver
	inURN := "urn:audio:hw:test:mic-room"
	outURN := "urn:audio:hw:test:speaker-room"

	audio.GlobalResolver().Register(audio.Device{
		ID:                inURN,
		URN:               inURN,
		Name:              "Room Test Mic",
		MaxInput:          2,
		MaxOutput:         0,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(inURN, inDev)

	audio.GlobalResolver().Register(audio.Device{
		ID:                outURN,
		URN:               outURN,
		Name:              "Room Test Speaker",
		MaxInput:          0,
		MaxOutput:         2,
		DefaultSampleRate: 48000,
		Status:            "active",
	})
	audio.GlobalResolver().Rebind(outURN, outDev)

	// Create RoomManager and Room
	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streamMgr)
	defer roomMgr.Close()

	roomID := "room-sound-check"
	if _, err := roomMgr.CreateRoom(roomID, "Room Sound Check"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// 1. Add input and output to the same room
	if err := roomMgr.AddInput(roomID, inURN); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := roomMgr.AddOutput(roomID, outURN); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	// 2. Attach TelemetryHub retroactively via SetHub
	hub := telemetry.NewTelemetryHub(30)
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	hub.Start(hubCtx)
	defer hub.Stop()

	streamMgr.SetHub(hub)

	// Check that tap was bound
	routeKey := "room-" + roomID + "-" + inURN + "-" + outURN
	routes := rt.ListMatrix()
	if len(routes) == 0 {
		t.Fatalf("expected compiled matrix route, got 0")
	}
	actualRouteID := routes[0].ID
	t.Logf("Compiled route ID: %s (expected key: %s)", actualRouteID, routeKey)

	tap, found := hub.GetTap(actualRouteID)
	if !found || tap == nil {
		t.Fatalf("expected telemetry tap for route %s to be registered in hub", actualRouteID)
	}

	// 3. Push audio into inDev (440 Hz sine wave, ~ -3 dBFS)
	frames := 960 // 20ms @ 48000 Hz
	sinePCM := generateSineBlock(48000, 2, frames, 440.0, 0.707)

	doneFeed := make(chan struct{})
	go func() {
		defer close(doneFeed)
		for i := 0; i < 30; i++ {
			inDev.InjectPush(sinePCM)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// 4. Verify output received sound (CapturedBytes > 0)
	deadline := time.Now().Add(2 * time.Second)
	captured := 0
	for time.Now().Before(deadline) {
		captured = len(outDev.CapturedBytes())
		if captured >= len(sinePCM) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if captured < len(sinePCM) {
		t.Fatalf("expected intra-room sound to reach output device: got %d bytes, want >= %d", captured, len(sinePCM))
	}
	t.Logf("SUCCESS: intra-room sound successfully routed to output device (%d bytes captured)", captured)

	// 5. Verify signal level registered on input meter (RMS dBFS > -96 dBFS silence floor)
	snap := tap.Snapshot(time.Now().UnixNano())
	t.Logf("Tap snapshot: DTXState=%s Channels=%+v", snap.DTXState, snap.Channels)
	if len(snap.Channels) == 0 {
		t.Fatalf("expected channels in tap snapshot, got 0")
	}

	for _, ch := range snap.Channels {
		if ch.RMSdBFS <= -90.0 {
			t.Fatalf("expected active signal level on channel %d, got %.2f dBFS (silent)", ch.Channel, ch.RMSdBFS)
		}
		t.Logf("Channel %d: RMS=%.2f dBFS, Peak=%.2f dBFS", ch.Channel, ch.RMSdBFS, ch.PeakdBFS)
	}

	<-doneFeed
}
