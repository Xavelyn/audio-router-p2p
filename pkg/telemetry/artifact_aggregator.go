package telemetry

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
)

// artifactRingDepth is the fixed number of samples retained per counter: a
// 10 s window sampled at 1 s plus one baseline entry (ADR 0034 §2).
const artifactRingDepth = 11

// RouteArtifactSource provides point-in-time samples of every live route's
// artifact counters (implemented by the daemon's stream manager).
type RouteArtifactSource interface {
	RouteArtifactSamples() []RouteArtifactSample
}

// DeviceArtifactSource provides point-in-time samples of every active
// directional device controller's hardware XRun state (implemented by the
// daemon's stream manager over the audio resolver).
type DeviceArtifactSource interface {
	DeviceArtifactSamples() []DeviceArtifactSample
}

// counterRing is a fixed-depth window of monotonic counter samples. Entries
// are aligned to the tail of the aggregator's shared tick clock: vals[i] was
// sampled at tickClock[len(tickClock)-len(vals)+i].
type counterRing struct {
	vals []uint64
}

// record appends a new sample of a monotonic counter with the ADR 0034 reset
// re-base guard: if the value decreased relative to the oldest retained
// sample, the counter was reset (route recreated, stream swapped) and the
// ring re-bases to the current value, reporting a window of 0. Negative
// windows are never produced.
func (r *counterRing) record(val uint64) uint64 {
	if len(r.vals) == 0 {
		r.vals = []uint64{val}
		return 0
	}
	if val < r.vals[0] {
		r.vals = []uint64{val}
		return 0
	}
	r.vals = append(r.vals, val)
	if len(r.vals) > artifactRingDepth {
		r.vals = r.vals[len(r.vals)-artifactRingDepth:]
	}
	return val - r.vals[0]
}

type peerWindows struct {
	underruns   counterRing
	overruns    counterRing
	latePackets counterRing
}

type routeWindows struct {
	underruns         counterRing
	overruns          counterRing
	plcConcealed      counterRing
	fecRecovered      counterRing
	fecUnrecoverable  counterRing
	packetsReceived   counterRing
	packetsLost       counterRing
	playoutGaps       counterRing
	captureDropBytes  counterRing
	captureDropEvents counterRing
	outputDropBytes   counterRing
	outputDropEvents  counterRing
	peers             map[string]*peerWindows
}

type deviceWindows struct {
	input  counterRing
	output counterRing
}

// ArtifactAggregator samples route- and device-scoped artifact counters every
// interval, maintains per-entity 10 s window rings, evaluates the audibility
// scores (ADR 0034 §3), and publishes an immutable ArtifactSnapshot via an
// atomic pointer swap. It is deliberately separate from the 250 ms network
// Aggregator (ADR 0010): different cadence, keying, and snapshot contract.
type ArtifactAggregator struct {
	routes   RouteArtifactSource
	devices  DeviceArtifactSource
	interval time.Duration
	window   time.Duration

	mu         sync.Mutex
	tickClock  []time.Time // shared sample clock, len <= artifactRingDepth
	routeWins  map[string]*routeWindows
	deviceWins map[string]*deviceWindows

	snapshot atomic.Pointer[ArtifactSnapshot]

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewArtifactAggregator creates an ArtifactAggregator. interval is the
// sampling period (default 1 s); the evaluation horizon is derived from it as
// interval * (artifactRingDepth - 1) (10 s at the default), and any window
// argument is snapped to that value so the published window_s never disagrees
// with the windowed math. Either source may be nil, in which case that
// dimension stays empty. An initial empty snapshot is published so
// GetArtifactSnapshot is always safe before the first tick.
func NewArtifactAggregator(routes RouteArtifactSource, devices DeviceArtifactSource, interval, window time.Duration) *ArtifactAggregator {
	if interval <= 0 {
		interval = time.Second
	}
	// The window ring is fixed depth (artifactRingDepth samples), so the
	// effective horizon is interval * (depth - 1). Snap the requested window
	// to that value so the published window_s always matches the windowed
	// math (ADR 0034 §2: 10 s at 1 s sampling).
	window = interval * time.Duration(artifactRingDepth-1)
	a := &ArtifactAggregator{
		routes:     routes,
		devices:    devices,
		interval:   interval,
		window:     window,
		routeWins:  make(map[string]*routeWindows),
		deviceWins: make(map[string]*deviceWindows),
	}
	a.snapshot.Store(&ArtifactSnapshot{
		GeneratedAt: time.Now(),
		WindowS:     int(window.Seconds()),
		Routes:      make(map[string]RouteHealth),
		Devices:     make(map[string]DeviceHealth),
	})
	return a
}

// Start begins the background sampler goroutine (idempotent).
func (a *ArtifactAggregator) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		a.ctx, a.cancel = context.WithCancel(ctx)
		a.wg.Add(1)
		a.started.Store(true)
		go a.run()
	})
}

// Stop shuts down the sampler goroutine (idempotent). A Stop issued before
// Start is a no-op that does NOT consume the stop token, so a later Start
// still produces a stoppable sampler (ADR 0034 §5 lifecycle contract).
func (a *ArtifactAggregator) Stop() {
	if !a.started.Load() {
		return
	}
	a.stopOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		a.wg.Wait()
	})
}

// run is the ticker loop. Missed beats are dropped by the ticker (no
// queueing), per ADR 0034 §5.
func (a *ArtifactAggregator) run() {
	defer a.wg.Done()
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.CollectSnapshot()
		}
	}
}

// GetArtifactSnapshot returns the latest immutable snapshot with O(1)
// lock-free read performance. Before the first tick it returns a safe empty
// snapshot.
func (a *ArtifactAggregator) GetArtifactSnapshot() *ArtifactSnapshot {
	if s := a.snapshot.Load(); s != nil {
		return s
	}
	return &ArtifactSnapshot{
		GeneratedAt: time.Now(),
		WindowS:     int(a.window.Seconds()),
		Routes:      make(map[string]RouteHealth),
		Devices:     make(map[string]DeviceHealth),
	}
}

// CollectSnapshot performs one synchronous sampling cycle: sweep all live
// counters (atomic loads only), update the window rings, evaluate scores, and
// publish a new immutable snapshot. It is safe to call concurrently with
// Start's ticker loop and with GetArtifactSnapshot.
func (a *ArtifactAggregator) CollectSnapshot() *ArtifactSnapshot {
	now := time.Now()

	var routeSamples []RouteArtifactSample
	if a.routes != nil {
		routeSamples = a.routes.RouteArtifactSamples()
	}
	var deviceSamples []DeviceArtifactSample
	if a.devices != nil {
		deviceSamples = a.devices.DeviceArtifactSamples()
	}

	a.mu.Lock()

	// Shared tick clock: one entry per sampling cycle, aligned with the tail
	// of every per-entity ring.
	a.tickClock = append(a.tickClock, now)
	if len(a.tickClock) > artifactRingDepth {
		a.tickClock = a.tickClock[len(a.tickClock)-artifactRingDepth:]
	}

	// ---- Devices: record XRun windows, build DeviceHealth ----
	devLive := make(map[string]*deviceWindows, len(deviceSamples))
	devState := make(map[string]struct {
		inW, outW         uint64
		inTotal, outTotal uint64
		hasIn, hasOut     bool
		ghost             bool
	}, len(deviceSamples))
	for _, smp := range deviceSamples {
		if smp.URN == "" {
			continue
		}
		dw := a.deviceWins[smp.URN]
		if dw == nil {
			dw = &deviceWindows{}
			a.deviceWins[smp.URN] = dw
		}
		st := devState[smp.URN]
		switch smp.Direction {
		case audio.DirectionInput:
			st.hasIn = true
			st.inW = dw.input.record(smp.HWXRuns)
			st.inTotal = smp.HWXRuns
		case audio.DirectionOutput:
			st.hasOut = true
			st.outW = dw.output.record(smp.HWXRuns)
			st.outTotal = smp.HWXRuns
		}
		st.ghost = st.ghost || smp.Ghost
		devState[smp.URN] = st
		devLive[smp.URN] = dw
	}
	for urn, dw := range a.deviceWins {
		if _, ok := devLive[urn]; !ok {
			delete(a.deviceWins, urn)
			continue
		}
		// A direction controller absent this tick has been destroyed: reset
		// its ring so a re-created controller re-baselines cleanly instead of
		// appending to stale entries misaligned with the tick clock (the
		// spanSeconds tail-alignment invariant).
		st := devState[urn]
		if !st.hasIn {
			dw.input = counterRing{}
		}
		if !st.hasOut {
			dw.output = counterRing{}
		}
	}

	devices := make(map[string]DeviceHealth, len(devState))
	for urn, st := range devState {
		score := round2(CalculateDeviceArtifactScore(st.inW, st.outW, st.hasIn, st.hasOut))
		dh := DeviceHealth{
			URN:      urn,
			Ghost:    st.ghost,
			Score:    score,
			Severity: ArtifactSeverityFor(score),
		}
		if st.hasIn {
			dh.Input = &DeviceDirArtifacts{XRunWindow: st.inW, XRunTotal: st.inTotal}
		}
		if st.hasOut {
			dh.Output = &DeviceDirArtifacts{XRunWindow: st.outW, XRunTotal: st.outTotal}
		}
		devices[urn] = dh
	}

	// ---- Routes: record counters, evaluate RouteHealth ----
	routes := make(map[string]RouteHealth, len(routeSamples))
	for _, smp := range routeSamples {
		if smp.RouteID == "" {
			continue
		}
		rw := a.routeWins[smp.RouteID]
		if rw == nil {
			rw = &routeWindows{peers: make(map[string]*peerWindows)}
			a.routeWins[smp.RouteID] = rw
		}

		undW := rw.underruns.record(smp.JitterUnderruns)
		overW := rw.overruns.record(smp.JitterOverruns)
		plcW := rw.plcConcealed.record(smp.PLCConcealed)
		fecRecW := rw.fecRecovered.record(smp.FECRecovered)
		fecUnrecW := rw.fecUnrecoverable.record(smp.FECUnrecoverable)
		recvW := rw.packetsReceived.record(smp.PacketsReceived)
		lostW := rw.packetsLost.record(smp.PacketsLost)
		playoutW := rw.playoutGaps.record(smp.PlayoutGaps)
		_ = rw.captureDropBytes.record(smp.CaptureDropBytes)
		capEvW := rw.captureDropEvents.record(smp.CaptureDropEvents)
		_ = rw.outputDropBytes.record(smp.OutputDropBytes)
		outEvW := rw.outputDropEvents.record(smp.OutputDropEvents)

		ringDrops := capEvW + outEvW

		// Per-peer windowed triple (underruns / overruns / late_packets).
		peerHealth := make(map[string]PeerArtifactHealth, len(smp.Peers))
		livePeers := make(map[string]bool, len(smp.Peers))
		for _, p := range smp.Peers {
			if p.Addr == "" {
				continue
			}
			livePeers[p.Addr] = true
			pw := rw.peers[p.Addr]
			if pw == nil {
				pw = &peerWindows{}
				rw.peers[p.Addr] = pw
			}
			peerHealth[p.Addr] = PeerArtifactHealth{
				Underruns:   pw.underruns.record(p.Underruns),
				Overruns:    pw.overruns.record(p.Overruns),
				LatePackets: pw.latePackets.record(p.LatePackets),
			}
		}
		for addr := range rw.peers {
			if !livePeers[addr] {
				delete(rw.peers, addr)
			}
		}

		// Windowed loss ratio (for C_lossRatio).
		var lossRatio float64
		if recvW+lostW > 0 {
			lossRatio = float64(lostW) / float64(recvW+lostW)
		}

		score := round2(CalculateRouteArtifactScore(undW, overW, ringDrops, fecUnrecW, lossRatio, plcW))

		// Device-shared XRun rate for this route: sum of the in-window XRuns
		// across the route's distinct devices (source and destination).
		var xrunW uint64
		var xrunSpan float64
		for _, urn := range distinctDeviceURNs(smp.SourceDeviceURN, smp.DestDeviceURN) {
			if urn == "" {
				continue
			}
			if dw, ok := devLive[urn]; ok {
				st := devState[urn]
				xrunW += st.inW + st.outW
				if sp := a.spanSeconds(&dw.input); sp > xrunSpan {
					xrunSpan = sp
				}
				if sp := a.spanSeconds(&dw.output); sp > xrunSpan {
					xrunSpan = sp
				}
			}
		}

		// Drop-rate span: the wider of the two drop-event rings, so a re-base
		// on one side cannot skew the shared rate.
		dropSpan := a.spanSeconds(&rw.captureDropEvents)
		if sp := a.spanSeconds(&rw.outputDropEvents); sp > dropSpan {
			dropSpan = sp
		}

		routes[smp.RouteID] = RouteHealth{
			Score:            score,
			Severity:         ArtifactSeverityFor(score),
			WindowS:          int(a.window.Seconds()),
			Underruns:        undW,
			Overruns:         overW,
			RingDrops:        ringDrops,
			PLCConcealed:     plcW,
			FECUnrecoverable: fecUnrecW,
			PacketsLost:      lostW,
			FECRecovered:     fecRecW,
			PlayoutGaps:      playoutW,
			Rates: ArtifactRates{
				UnderrunPerS: round4(ratePerSecond(undW, a.spanSeconds(&rw.underruns))),
				OverrunPerS:  round4(ratePerSecond(overW, a.spanSeconds(&rw.overruns))),
				XrunPerS:     round4(ratePerSecond(xrunW, xrunSpan)),
				RingDropPerS: round4(ratePerSecond(ringDrops, dropSpan)),
			},
			Ghost: smp.SourceGhost || smp.DestGhost,
			Peers: peerHealth,
		}
	}
	for id := range a.routeWins {
		if _, ok := routes[id]; !ok {
			delete(a.routeWins, id)
		}
	}

	snap := &ArtifactSnapshot{
		GeneratedAt: now,
		WindowS:     int(a.window.Seconds()),
		Routes:      routes,
		Devices:     devices,
	}
	a.mu.Unlock()

	a.snapshot.Store(snap)
	return snap
}

// spanSeconds returns the true elapsed time between the endpoints of a ring,
// read from the shared tick clock (ramp-up safe: a ring with 4 samples spans
// 3 s, not the full window). Returns 0 when the span is unknown or zero.
func (a *ArtifactAggregator) spanSeconds(r *counterRing) float64 {
	n := len(a.tickClock)
	if n < 2 || len(r.vals) == 0 {
		return 0
	}
	oldest := n - len(r.vals)
	if oldest < 0 {
		oldest = 0
	}
	if oldest >= n-1 {
		return 0
	}
	return a.tickClock[n-1].Sub(a.tickClock[oldest]).Seconds()
}

// ratePerSecond computes window / actualSpanSeconds, safe for zero spans.
func ratePerSecond(window uint64, spanSeconds float64) float64 {
	if spanSeconds <= 0 || math.IsNaN(spanSeconds) {
		return 0
	}
	return float64(window) / spanSeconds
}

// distinctDeviceURNs returns the route's source and destination device URNs
// without duplicates (a local loop route uses one device for both ends).
func distinctDeviceURNs(src, dst string) []string {
	if src == dst {
		return []string{src}
	}
	return []string{src, dst}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}
