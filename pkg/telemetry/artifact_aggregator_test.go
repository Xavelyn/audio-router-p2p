package telemetry

import (
	"context"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
)

// fakeArtifactRoutes is a controlled-time fake RouteArtifactSource: tests
// mutate the counter values between synchronous CollectSnapshot calls.
type fakeArtifactRoutes struct {
	mu      sync.Mutex
	samples map[string]RouteArtifactSample
}

func (f *fakeArtifactRoutes) RouteArtifactSamples() []RouteArtifactSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RouteArtifactSample, 0, len(f.samples))
	for _, s := range f.samples {
		out = append(out, s)
	}
	return out
}

func (f *fakeArtifactRoutes) set(routeID string, mutate func(*RouteArtifactSample)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.samples[routeID]
	s.RouteID = routeID
	mutate(&s)
	f.samples[routeID] = s
}

func (f *fakeArtifactRoutes) remove(routeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.samples, routeID)
}

// fakeArtifactDevices is a controlled fake DeviceArtifactSource.
type fakeArtifactDevices struct {
	mu      sync.Mutex
	samples []DeviceArtifactSample
}

func (f *fakeArtifactDevices) DeviceArtifactSamples() []DeviceArtifactSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DeviceArtifactSample, len(f.samples))
	copy(out, f.samples)
	return out
}

func (f *fakeArtifactDevices) set(s DeviceArtifactSample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, cur := range f.samples {
		if cur.URN == s.URN && cur.Direction == s.Direction {
			f.samples[i] = s
			return
		}
	}
	f.samples = append(f.samples, s)
}

func (f *fakeArtifactDevices) remove(urn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.samples[:0]
	for _, s := range f.samples {
		if s.URN != urn {
			out = append(out, s)
		}
	}
	f.samples = out
}

func TestArtifactAggregator_SnapshotSafeBeforeFirstTick(t *testing.T) {
	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	devices := &fakeArtifactDevices{}
	agg := NewArtifactAggregator(routes, devices, time.Second, 10*time.Second)

	snap := agg.GetArtifactSnapshot()
	if snap == nil {
		t.Fatal("snapshot must be non-nil before the first tick")
	}
	if snap.Routes == nil || snap.Devices == nil {
		t.Fatal("snapshot maps must be non-nil")
	}
	if len(snap.Routes) != 0 || len(snap.Devices) != 0 {
		t.Errorf("expected empty snapshot, got %d routes / %d devices", len(snap.Routes), len(snap.Devices))
	}
	if snap.WindowS != 10 {
		t.Errorf("WindowS = %d; want 10", snap.WindowS)
	}
}

func TestArtifactAggregator_WindowDelta(t *testing.T) {
	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	agg := NewArtifactAggregator(routes, nil, time.Second, 10*time.Second)

	// Tick 1: baseline (ring of one → window 0, clean score).
	routes.set("r1", func(s *RouteArtifactSample) { s.PacketsReceived = 1000 })
	snap := agg.CollectSnapshot()
	rh := snap.Routes["r1"]
	if rh.Underruns != 0 {
		t.Fatalf("baseline window = %d; want 0", rh.Underruns)
	}
	if rh.Score != 100 || rh.Severity != SeverityNone {
		t.Fatalf("baseline score/severity = %v/%s; want 100/NONE", rh.Score, rh.Severity)
	}
	if rh.WindowS != 10 {
		t.Fatalf("WindowS = %d; want 10", rh.WindowS)
	}

	// Increment by 1 per tick; after the 12th tick the 11-deep ring spans
	// ticks 2..12 → window exactly 10.
	for tick := 1; tick <= 11; tick++ {
		routes.set("r1", func(s *RouteArtifactSample) {
			s.JitterUnderruns = uint64(tick)
			s.PacketsReceived = 1000 + 100*uint64(tick)
		})
		snap = agg.CollectSnapshot()
	}
	rh = snap.Routes["r1"]
	if rh.Underruns != 10 {
		t.Errorf("window after 12 ticks = %d; want 10", rh.Underruns)
	}
	if rh.Score != 40 || rh.Severity != SeverityMajor {
		t.Errorf("10 underruns: score/severity = %v/%s; want 40/MAJOR", rh.Score, rh.Severity)
	}
	if rh.FECRecovered != 0 {
		t.Errorf("FECRecovered window = %d; want 0", rh.FECRecovered)
	}
}

func TestArtifactAggregator_ResetRebase(t *testing.T) {
	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	agg := NewArtifactAggregator(routes, nil, time.Second, 10*time.Second)

	routes.set("r1", func(s *RouteArtifactSample) { s.JitterUnderruns = 100 })
	snap := agg.CollectSnapshot()
	if got := snap.Routes["r1"].Underruns; got != 0 {
		t.Fatalf("baseline window = %d; want 0", got)
	}

	routes.set("r1", func(s *RouteArtifactSample) { s.JitterUnderruns = 105 })
	snap = agg.CollectSnapshot()
	if got := snap.Routes["r1"].Underruns; got != 5 {
		t.Fatalf("window after +5 = %d; want 5", got)
	}

	// Counter reset (route recreated / stream swapped): window must re-base
	// to 0, never negative.
	routes.set("r1", func(s *RouteArtifactSample) { s.JitterUnderruns = 3 })
	snap = agg.CollectSnapshot()
	if got := snap.Routes["r1"].Underruns; got != 0 {
		t.Fatalf("window after reset = %d; want 0 (re-base)", got)
	}

	// Subsequent windows compute from the new base.
	routes.set("r1", func(s *RouteArtifactSample) { s.JitterUnderruns = 7 })
	snap = agg.CollectSnapshot()
	if got := snap.Routes["r1"].Underruns; got != 4 {
		t.Fatalf("window after re-base +4 = %d; want 4", got)
	}
}

func TestArtifactAggregator_RemovalPruned(t *testing.T) {
	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	devices := &fakeArtifactDevices{}
	agg := NewArtifactAggregator(routes, devices, time.Second, 10*time.Second)

	routes.set("r1", func(s *RouteArtifactSample) {})
	routes.set("r2", func(s *RouteArtifactSample) {})
	devices.set(DeviceArtifactSample{URN: "alsa://hw:0,0", Direction: audio.DirectionInput})

	snap := agg.CollectSnapshot()
	if len(snap.Routes) != 2 || len(snap.Devices) != 1 {
		t.Fatalf("initial entities: %d routes / %d devices; want 2/1", len(snap.Routes), len(snap.Devices))
	}

	// Entities no longer present in the source are pruned within one tick.
	routes.remove("r2")
	devices.remove("alsa://hw:0,0")
	snap = agg.CollectSnapshot()
	if _, ok := snap.Routes["r2"]; ok {
		t.Error("removed route r2 still present in snapshot")
	}
	if _, ok := snap.Routes["r1"]; !ok {
		t.Error("live route r1 missing from snapshot")
	}
	if len(snap.Devices) != 0 {
		t.Errorf("removed device still present: %v", snap.Devices)
	}
}

func TestArtifactAggregator_PeerWindowedTriple(t *testing.T) {
	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	agg := NewArtifactAggregator(routes, nil, time.Second, 10*time.Second)

	routes.set("r1", func(s *RouteArtifactSample) {
		s.Peers = []PeerArtifactSample{{Addr: "10.0.0.2:45678", Underruns: 2, Overruns: 0, LatePackets: 1}}
	})
	agg.CollectSnapshot()

	routes.set("r1", func(s *RouteArtifactSample) {
		s.Peers = []PeerArtifactSample{{Addr: "10.0.0.2:45678", Underruns: 5, Overruns: 1, LatePackets: 3}}
	})
	snap := agg.CollectSnapshot()

	peer, ok := snap.Routes["r1"].Peers["10.0.0.2:45678"]
	if !ok {
		t.Fatal("peer missing from route health")
	}
	if peer.Underruns != 3 || peer.Overruns != 1 || peer.LatePackets != 2 {
		t.Errorf("peer windowed triple = %+v; want {3 1 2}", peer)
	}
}

func TestArtifactAggregator_DeviceXrunWindow(t *testing.T) {
	devices := &fakeArtifactDevices{}
	agg := NewArtifactAggregator(nil, devices, time.Second, 10*time.Second)

	devices.set(DeviceArtifactSample{URN: "alsa://hw:1,0", Direction: audio.DirectionOutput, HWXRuns: 12})
	agg.CollectSnapshot()

	devices.set(DeviceArtifactSample{URN: "alsa://hw:1,0", Direction: audio.DirectionOutput, HWXRuns: 14, Ghost: true})
	snap := agg.CollectSnapshot()

	dh, ok := snap.Devices["alsa://hw:1,0"]
	if !ok {
		t.Fatal("device missing from snapshot")
	}
	if dh.Input != nil {
		t.Errorf("input direction object must be omitted for an output-only device, got %+v", dh.Input)
	}
	if dh.Output == nil {
		t.Fatal("output direction object missing")
	}
	if dh.Output.XRunWindow != 2 || dh.Output.XRunTotal != 14 {
		t.Errorf("output xruns = window %d / total %d; want 2 / 14", dh.Output.XRunWindow, dh.Output.XRunTotal)
	}
	// 2 in-window xruns → C_xrun = 0.50; missing input direction → C = 1.0.
	if dh.Score != 50 || dh.Severity != SeverityMajor {
		t.Errorf("device score/severity = %v/%s; want 50/MAJOR", dh.Score, dh.Severity)
	}
	if !dh.Ghost {
		t.Error("ghost flag not propagated to DeviceHealth")
	}
}

func TestArtifactAggregator_RampUpRatesSane(t *testing.T) {
	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	agg := NewArtifactAggregator(routes, nil, time.Second, 10*time.Second)

	routes.set("r1", func(s *RouteArtifactSample) { s.JitterUnderruns = 0 })
	agg.CollectSnapshot()

	routes.set("r1", func(s *RouteArtifactSample) { s.JitterUnderruns = 10 })
	snap := agg.CollectSnapshot()

	rh := snap.Routes["r1"]
	if rh.Underruns != 10 {
		t.Fatalf("ramp-up window = %d; want 10", rh.Underruns)
	}
	// Rate = window / actualSpanSeconds: with a 2-sample ring the span is
	// tiny, so the rate is large but must be finite and non-negative.
	for name, rate := range map[string]float64{
		"underrun_per_s":  rh.Rates.UnderrunPerS,
		"overrun_per_s":   rh.Rates.OverrunPerS,
		"xrun_per_s":      rh.Rates.XrunPerS,
		"ring_drop_per_s": rh.Rates.RingDropPerS,
	} {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			t.Errorf("%s = %v; want finite and >= 0", name, rate)
		}
	}
	if rh.Rates.UnderrunPerS <= 0 {
		t.Errorf("underrun_per_s = %v; want > 0 for a 10-event window", rh.Rates.UnderrunPerS)
	}
}

func TestArtifactAggregator_StopIdempotentNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	routes := &fakeArtifactRoutes{samples: map[string]RouteArtifactSample{}}
	devices := &fakeArtifactDevices{}
	agg := NewArtifactAggregator(routes, devices, 10*time.Millisecond, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agg.Start(ctx)
	agg.Start(ctx)                    // idempotent
	time.Sleep(60 * time.Millisecond) // let a few ticks run

	agg.Stop()
	agg.Stop()                        // idempotent
	time.Sleep(50 * time.Millisecond) // let the worker exit

	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}

	// Snapshot reads must stay safe after Stop.
	if snap := agg.GetArtifactSnapshot(); snap == nil || snap.Routes == nil || snap.Devices == nil {
		t.Error("snapshot must remain safe after Stop")
	}
}

// countingRouteSource counts RouteArtifactSamples calls (for lifecycle tests).
type countingRouteSource struct {
	count *atomic.Int64
}

func (c *countingRouteSource) RouteArtifactSamples() []RouteArtifactSample {
	c.count.Add(1)
	return nil
}

// TestArtifactAggregator_StartAfterStop is the ADR 0034 §5 lifecycle
// regression guard: a Stop issued before Start must not consume the stop
// token, so a later Start still yields a stoppable sampler.
func TestArtifactAggregator_StartAfterStop(t *testing.T) {
	before := runtime.NumGoroutine()

	var ticks atomic.Int64
	routes := &countingRouteSource{count: &ticks}
	agg := NewArtifactAggregator(routes, nil, 5*time.Millisecond, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agg.Stop() // Stop-before-Start: must be a harmless no-op
	agg.Start(ctx)
	time.Sleep(40 * time.Millisecond) // let a few ticks run

	if ticks.Load() == 0 {
		t.Fatal("sampler never ticked after Start")
	}
	agg.Stop()
	time.Sleep(30 * time.Millisecond) // let the worker exit

	stable := ticks.Load()
	time.Sleep(30 * time.Millisecond)
	if got := ticks.Load(); got != stable {
		t.Errorf("sampler kept ticking after Stop: %d -> %d", stable, got)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

// replaceDeviceSamples swaps the fake device source's sample set.
func replaceDeviceSamples(t *testing.T, f *fakeArtifactDevices, samples []DeviceArtifactSample) {
	t.Helper()
	f.mu.Lock()
	f.samples = append([]DeviceArtifactSample(nil), samples...)
	f.mu.Unlock()
}

// TestArtifactAggregator_DeviceDirectionReset guards the window-alignment
// invariant: when a direction controller is destroyed while the URN stays
// alive (via the other direction) and is later re-created, its ring must
// re-baseline instead of appending to stale, tick-clock-misaligned entries.
func TestArtifactAggregator_DeviceDirectionReset(t *testing.T) {
	devices := &fakeArtifactDevices{}
	agg := NewArtifactAggregator(nil, devices, time.Second, 10*time.Second)

	urn := "alsa://hw:2,0"
	// Tick 1: both directions live, zero XRuns.
	replaceDeviceSamples(t, devices, []DeviceArtifactSample{
		{URN: urn, Direction: audio.DirectionInput},
		{URN: urn, Direction: audio.DirectionOutput, HWXRuns: 0},
	})
	agg.CollectSnapshot()

	// Tick 2: the output controller is destroyed; input keeps the URN alive.
	replaceDeviceSamples(t, devices, []DeviceArtifactSample{
		{URN: urn, Direction: audio.DirectionInput},
	})
	snap := agg.CollectSnapshot()
	if dh := snap.Devices[urn]; dh.Output != nil {
		t.Fatalf("destroyed direction still rendered: %+v", dh.Output)
	}

	// Tick 3: output controller re-created (fresh lifetime counter) with 4
	// lifetime XRuns already on the board.
	replaceDeviceSamples(t, devices, []DeviceArtifactSample{
		{URN: urn, Direction: audio.DirectionInput},
		{URN: urn, Direction: audio.DirectionOutput, HWXRuns: 4},
	})
	snap = agg.CollectSnapshot()
	dh := snap.Devices[urn]
	if dh.Output == nil {
		t.Fatal("re-created direction missing from snapshot")
	}
	// First sample of the re-baselined ring: the window is 0, never a delta
	// against the stale pre-destruction entry.
	if dh.Output.XRunWindow != 0 || dh.Output.XRunTotal != 4 {
		t.Errorf("re-created direction = window %d / total %d; want 0 / 4", dh.Output.XRunWindow, dh.Output.XRunTotal)
	}

	// Tick 4: one more XRun → window of exactly 1 from the fresh base.
	replaceDeviceSamples(t, devices, []DeviceArtifactSample{
		{URN: urn, Direction: audio.DirectionInput},
		{URN: urn, Direction: audio.DirectionOutput, HWXRuns: 5},
	})
	snap = agg.CollectSnapshot()
	dh = snap.Devices[urn]
	if got := dh.Output.XRunWindow; got != 1 {
		t.Errorf("window after re-create +1 = %d; want 1", got)
	}
	// 1 in-window xrun → C_xrun = 0.70; input direction still clean.
	if dh.Score != 70 || dh.Severity != SeverityMinor {
		t.Errorf("device score/severity = %v/%s; want 70/MINOR", dh.Score, dh.Severity)
	}
}
