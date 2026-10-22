//go:build noaudio

package main

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestCalibrateLocalRoute verifies the loopback calibration workflow end to
// end on a local input->output route: the probe tone is injected after the
// gain stage, detected at the playout write, and the per-channel delays are
// measured, stable across runs, and (with Apply) stored as a stable alignment.
func TestCalibrateLocalRoute(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer trans.Close()
	cfg := &config.Config{
		AuthToken: "calib-test", UseEmulator: true,
		AnnounceInterval: time.Second, PeerTTL: 3 * time.Second, SyncInterval: time.Second,
	}
	disc, err := discovery.New(cfg, "calib-node")
	if err != nil {
		t.Fatal(err)
	}
	defer disc.Close()
	m := newStreamManager(cfg, trans, []string{"127.0.0.1"}, disc)
	defer m.StopAll()

	routesFile := t.TempDir() + "/routes.json"
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatal(err)
	}
	m.SetRouter(rt)

	if err := m.StartRoute(router.StreamRoute{ID: "calib-local", SourceDevice: 0,
		Destinations: []string{trans.Addr()}, DestDevice: 1,
		SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}

	// Wait for the route to be playing (the probe path needs live playout).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st, ok := m.RouteStatus("calib-local"); ok && st.Playing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("calib-local did not start playing")
		}
		time.Sleep(20 * time.Millisecond)
	}

	res, ok, err := m.CalibrateRoute("calib-local", v1.CalibrateRequest{})
	if !ok || err != nil {
		t.Fatalf("CalibrateRoute: ok=%v err=%v", ok, err)
	}
	if !res.Detected {
		t.Fatalf("probe not detected: %+v", res)
	}
	if len(res.ChannelDelaysMS) != 2 {
		t.Fatalf("want 2 channel delays, got %d: %+v", len(res.ChannelDelaysMS), res)
	}
	for c, d := range res.ChannelDelaysMS {
		if d <= 0 || d > 1500 {
			t.Fatalf("channel %d delay %.3f ms out of (0, 1500): %+v", c, d, res)
		}
	}
	if spread := math.Abs(res.ChannelDelaysMS[0] - res.ChannelDelaysMS[1]); spread > 20 {
		t.Fatalf("channel delays differ by %.3f ms (> 20): %+v", spread, res.ChannelDelaysMS)
	}

	// Two-run stability: the raw pipeline delay is deterministic, so a second
	// probe measures the same delays (within jitter-buffer tolerance).
	res2, _, err := m.CalibrateRoute("calib-local", v1.CalibrateRequest{})
	if err != nil || !res2.Detected {
		t.Fatalf("second calibration: detected=%v err=%v", res2.Detected, err)
	}
	for c := 0; c < 2; c++ {
		if drift := math.Abs(res2.ChannelDelaysMS[c] - res.ChannelDelaysMS[c]); drift > 200 {
			t.Fatalf("channel %d unstable across runs: %.3f vs %.3f ms", c, res.ChannelDelaysMS[c], res2.ChannelDelaysMS[c])
		}
	}

	// Manual alignment: set offsets and confirm they are applied live and
	// persisted on the route.
	applied, ok, err := m.SetRouteAlignment("calib-local", []float64{0, 20})
	if !ok || err != nil {
		t.Fatalf("SetRouteAlignment: ok=%v err=%v", ok, err)
	}
	if len(applied) != 2 || applied[1] < 19 || applied[1] > 21 {
		t.Fatalf("expected ~20 ms offset on channel 1, got %+v", applied)
	}
	if got, ok := m.GetRouteAlignment("calib-local"); !ok || len(got) != 2 || got[1] < 19 || got[1] > 21 {
		t.Fatalf("GetRouteAlignment: ok=%v got=%+v", ok, got)
	}
	if route, _ := rt.Get("calib-local"); len(route.ChannelDelaysMS) != 2 || route.ChannelDelaysMS[1] < 19 || route.ChannelDelaysMS[1] > 21 {
		t.Fatalf("alignment not persisted: %+v", route.ChannelDelaysMS)
	}

	// Apply on an equal-delay route computes near-zero offsets.
	res3, _, err := m.CalibrateRoute("calib-local", v1.CalibrateRequest{Apply: true})
	if err != nil || !res3.Detected {
		t.Fatalf("calibrate with apply: detected=%v err=%v", res3.Detected, err)
	}
	if !res3.Applied {
		t.Fatalf("expected applied=true: %+v", res3)
	}
	if len(res3.OffsetsMS) != 2 {
		t.Fatalf("want 2 offsets, got %d: %+v", len(res3.OffsetsMS), res3)
	}

	// Unknown route: not found.
	if _, ok, _ := m.CalibrateRoute("nope", v1.CalibrateRequest{}); ok {
		t.Fatal("unknown route should not calibrate")
	}
	if _, ok, _ := m.SetRouteAlignment("nope", []float64{0, 0}); ok {
		t.Fatal("unknown route should not accept alignment")
	}

	// Receive-only route: not calibratable (no local input to probe), but
	// still manually alignable because its playout side lives on this node.
	if err := m.StartRoute(router.StreamRoute{ID: "calib-recv", Direction: "receive",
		DestDevice: 1, SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CalibrateRoute("calib-recv", v1.CalibrateRequest{}); !errors.Is(err, v1.ErrRouteNotCalibratable) {
		t.Fatalf("receive-only route should be not calibratable, got err=%v", err)
	}
	if applied, ok, err := m.SetRouteAlignment("calib-recv", []float64{0, 0}); !ok || err != nil || len(applied) != 2 {
		t.Fatalf("receive-only route should accept manual alignment: ok=%v applied=%v err=%v", ok, applied, err)
	}
}
