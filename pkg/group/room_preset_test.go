package group

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
)

type captureStreamManager struct {
	mu     sync.Mutex
	starts []router.StreamRoute
}

func (c *captureStreamManager) StartRoute(route router.StreamRoute) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts = append(c.starts, route)
	return nil
}

func (c *captureStreamManager) StopRoute(id string) error { return nil }

func (c *captureStreamManager) lastStart() (router.StreamRoute, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.starts) == 0 {
		return router.StreamRoute{}, false
	}
	return c.starts[len(c.starts)-1], true
}

func TestRoomLatencyPreset(t *testing.T) {
	routesFile := t.TempDir() + "/preset_routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	capture := &captureStreamManager{}

	room := NewRoom(RoomConfig{
		ID:            "room-preset",
		Name:          "Preset Room",
		Router:        rt,
		StreamManager: capture,
	})

	// Default preset is balanced.
	if got := room.LatencyPreset(); got != "balanced" {
		t.Fatalf("default preset = %q, want balanced", got)
	}

	if err := room.AddInput("mic-1"); err != nil {
		t.Fatalf("AddInput: %v", err)
	}
	if err := room.AddOutput("spk-1"); err != nil {
		t.Fatalf("AddOutput: %v", err)
	}

	routeID := "room-room-preset-mic-1-spk-1"
	mr, ok := rt.GetMatrix(routeID)
	if !ok || mr.LatencyPreset != "balanced" {
		t.Fatalf("compiled matrix route preset = %+v, ok=%v", mr, ok)
	}
	sr, ok := rt.Get(routeID)
	if !ok || sr.LatencyPreset != "balanced" {
		t.Fatalf("persisted stream route preset = %+v, ok=%v", sr, ok)
	}
	last, ok := capture.lastStart()
	if !ok || last.LatencyPreset != "balanced" {
		t.Fatalf("StartRoute did not carry default preset: %+v, ok=%v", last, ok)
	}

	// Switching to reliable recompiles every room route with the new preset.
	if err := room.SetLatencyPreset("reliable"); err != nil {
		t.Fatalf("SetLatencyPreset(reliable): %v", err)
	}
	if got := room.LatencyPreset(); got != "reliable" {
		t.Fatalf("preset after set = %q", got)
	}
	mr, _ = rt.GetMatrix(routeID)
	if mr.LatencyPreset != "reliable" {
		t.Fatalf("matrix route preset after recompile = %q", mr.LatencyPreset)
	}
	sr, _ = rt.Get(routeID)
	if sr.LatencyPreset != "reliable" {
		t.Fatalf("persisted route preset after recompile = %q", sr.LatencyPreset)
	}
	last, _ = capture.lastStart()
	if last.LatencyPreset != "reliable" {
		t.Fatalf("recompiled StartRoute preset = %q", last.LatencyPreset)
	}

	// Unknown presets are rejected and leave the room unchanged.
	if err := room.SetLatencyPreset("ultra"); err == nil {
		t.Fatal("expected error for unknown preset")
	}
	if err := room.SetLatencyPreset(""); err == nil {
		t.Fatal("expected error for empty preset")
	}
	if got := room.LatencyPreset(); got != "reliable" {
		t.Fatalf("preset changed after rejected set: %q", got)
	}

	// The preset is part of the room's JSON state.
	data, err := json.Marshal(room)
	if err != nil {
		t.Fatalf("marshal room: %v", err)
	}
	if !strings.Contains(string(data), `"latency_preset":"reliable"`) {
		t.Fatalf("room JSON missing latency_preset: %s", data)
	}

	// Leaving the room cleans up compiled routes.
	if err := room.Leave(); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if got := len(rt.ListMatrix()); got != 0 {
		t.Fatalf("expected 0 routes after Leave, got %d", got)
	}
}

func TestGroupStreamerAppliesRoomPreset(t *testing.T) {
	room := NewRoom(RoomConfig{ID: "room-gs-preset", Name: "GS Preset"})

	// The streamer reads the room's preset at construction...
	gs, err := NewGroupStreamer(GroupStreamerConfig{
		Room:       room,
		SampleRate: 48000,
		Channels:   2,
	})
	if err != nil {
		t.Fatalf("NewGroupStreamer: %v", err)
	}
	want := streamer.LatencyPresetBalanced.Timing()
	if gs.jitterFrames != want.JitterFrames || gs.minDelay != want.MinDelay || gs.maxDelay != want.MaxDelay {
		t.Fatalf("group streamer timing = %d/%s/%s, want %d/%s/%s",
			gs.jitterFrames, gs.minDelay, gs.maxDelay,
			want.JitterFrames, want.MinDelay, want.MaxDelay)
	}

	// ...and ApplyPreset retunes a running buffer without panicking (nil
	// buffer before Start is a no-op).
	gs.ApplyPreset(streamer.LatencyPresetReliable)

	if err := room.SetLatencyPreset("reliable"); err != nil {
		t.Fatalf("SetLatencyPreset(reliable): %v", err)
	}
	gs2, err := NewGroupStreamer(GroupStreamerConfig{
		Room:       room,
		SampleRate: 48000,
		Channels:   2,
	})
	if err != nil {
		t.Fatalf("NewGroupStreamer after preset change: %v", err)
	}
	want = streamer.LatencyPresetReliable.Timing()
	if gs2.jitterFrames != want.JitterFrames || gs2.minDelay != want.MinDelay || gs2.maxDelay != want.MaxDelay {
		t.Fatalf("group streamer timing after preset change = %d/%s/%s, want %d/%s/%s",
			gs2.jitterFrames, gs2.minDelay, gs2.maxDelay,
			want.JitterFrames, want.MinDelay, want.MaxDelay)
	}
}
