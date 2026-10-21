package telemetry

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// DiscoveredPeer captures member information supplied by the discovery subsystem.
type DiscoveredPeer struct {
	ID         string
	Addr       string
	APIAddr    string
	SyncAddr   string
	StreamAddr string
	LastSeen   time.Time
	SWIMState  string
	RTTMS      float64
}

// PeerSyncOffset captures clock offset information supplied by the clock sync subsystem.
type PeerSyncOffset struct {
	OffsetUS   float64
	DriftPPM   float64
	RTTUS      float64
	LastUpdate time.Time
}

// DiscoveryProvider defines the discovery capabilities required by the telemetry aggregator.
type DiscoveryProvider interface {
	NodeID() string
	Telemetry() DiscoveryHealth
	DiscoveredPeers() []DiscoveredPeer
}

// ClockSyncProvider defines the clock sync capabilities required by the telemetry aggregator.
type ClockSyncProvider interface {
	Telemetry() ClockSyncHealth
	PeerOffsets() map[string]PeerSyncOffset
}

// StreamerProvider defines the media transport capabilities required by the telemetry aggregator.
type StreamerProvider interface {
	StreamLinkStats() []StreamLinkStats
}

type peerHistory struct {
	lastBytesRecv   uint64
	lastCheckTime   time.Time
	calculatedKbps  float64
	underrunSamples []timestampedCount
}

type timestampedCount struct {
	t     time.Time
	count uint64
}

// Aggregator collects telemetry from discovery, streaming, and clock sync subsystems,
// evaluates health scores, and publishes immutable snapshots via atomic pointer swaps.
type Aggregator struct {
	nodeID    string
	disc      DiscoveryProvider
	clockSync ClockSyncProvider
	streamer  StreamerProvider

	interval time.Duration
	snapshot atomic.Pointer[NetworkHealthSnapshot]

	historyMu   sync.Mutex
	peerHist    map[string]*peerHistory
	localHWHist []timestampedCount

	localHWUnderruns atomic.Uint64

	startOnce sync.Once
	stopOnce  sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Config provides configuration options for Aggregator.
type Config struct {
	NodeID    string
	Interval  time.Duration
	Discovery DiscoveryProvider
	ClockSync ClockSyncProvider
	Streamer  StreamerProvider
}

// NewAggregator creates an initialized Aggregator instance.
func NewAggregator(cfg Config) *Aggregator {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}

	nodeID := cfg.NodeID
	if nodeID == "" && cfg.Discovery != nil {
		nodeID = cfg.Discovery.NodeID()
	}

	a := &Aggregator{
		nodeID:    nodeID,
		disc:      cfg.Discovery,
		clockSync: cfg.ClockSync,
		streamer:  cfg.Streamer,
		interval:  interval,
		peerHist:  make(map[string]*peerHistory),
	}

	initial := a.emptySnapshot(time.Now())
	a.snapshot.Store(initial)
	a.CollectSnapshot()

	return a
}

// Start begins periodic background telemetry aggregation (idempotent).
func (a *Aggregator) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		a.ctx, a.cancel = context.WithCancel(ctx)
		a.wg.Add(1)
		go a.run()
	})
}

// Stop shuts down the background aggregator worker (idempotent).
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		a.wg.Wait()
	})
}

// Close implements io.Closer for clean component lifecycle management.
func (a *Aggregator) Close() error {
	a.Stop()
	return nil
}

// RecordLocalHardwareUnderrun increments the local audio hardware underrun counter.
func (a *Aggregator) RecordLocalHardwareUnderrun() {
	a.localHWUnderruns.Add(1)
}

// GetHealthSnapshot returns an immutable point-in-time snapshot with O(1) read performance.
func (a *Aggregator) GetHealthSnapshot() *NetworkHealthSnapshot {
	s := a.snapshot.Load()
	if s == nil {
		return a.emptySnapshot(time.Now())
	}
	return s
}

// CollectSnapshot synchronously sweeps subsystems, evaluates scores, and publishes a new snapshot.
func (a *Aggregator) CollectSnapshot() *NetworkHealthSnapshot {
	now := time.Now()
	nodeID := a.nodeID
	if nodeID == "" && a.disc != nil {
		nodeID = a.disc.NodeID()
	}

	var discHealth DiscoveryHealth
	var peers []DiscoveredPeer
	if a.disc != nil {
		discHealth = a.disc.Telemetry()
		peers = a.disc.DiscoveredPeers()
	}

	var syncHealth ClockSyncHealth
	var peerOffsets map[string]PeerSyncOffset
	if a.clockSync != nil {
		syncHealth = a.clockSync.Telemetry()
		peerOffsets = a.clockSync.PeerOffsets()
	}

	var streamStats []StreamLinkStats
	if a.streamer != nil {
		streamStats = a.streamer.StreamLinkStats()
	}

	peerStreamMap := make(map[string]StreamLinkStats, len(streamStats))
	streamMap := make(map[string]StreamLinkStats, len(streamStats))
	for _, st := range streamStats {
		if st.StreamID != "" {
			streamMap[st.StreamID] = st
		}
		if st.PeerID != "" {
			peerStreamMap[st.PeerID] = st
		}
	}

	a.historyMu.Lock()
	defer a.historyMu.Unlock()

	// Update local hardware underrun sliding window (10s) with underflow protection
	hwTotal := a.localHWUnderruns.Load()
	a.localHWHist = append(a.localHWHist, timestampedCount{t: now, count: hwTotal})
	a.localHWHist = pruneOldSamples(a.localHWHist, now.Add(-10*time.Second))
	var localHWUnderruns10s uint64
	if hwTotal >= a.localHWHist[0].count {
		localHWUnderruns10s = hwTotal - a.localHWHist[0].count
	} else {
		a.localHWHist = []timestampedCount{{t: now, count: hwTotal}}
	}

	peerHealthMap := make(map[string]PeerLinkHealth, len(peers))
	latencyMatrix := make(map[string]float64, len(peers))
	var peerScores []float64

	for _, p := range peers {
		if p.ID == "" {
			continue
		}

		hist, ok := a.peerHist[p.ID]
		if !ok {
			hist = &peerHistory{lastCheckTime: now}
			a.peerHist[p.ID] = hist
		}

		st, hasStreaming := peerStreamMap[p.ID]
		if !hasStreaming {
			st = StreamLinkStats{PeerID: p.ID}
		}

		// Calculate underruns over 10s sliding window with underflow guard
		hist.underrunSamples = append(hist.underrunSamples, timestampedCount{t: now, count: st.Underruns})
		hist.underrunSamples = pruneOldSamples(hist.underrunSamples, now.Add(-10*time.Second))
		var underruns10s uint64
		if st.Underruns >= hist.underrunSamples[0].count {
			underruns10s = st.Underruns - hist.underrunSamples[0].count
		} else {
			hist.underrunSamples = []timestampedCount{{t: now, count: st.Underruns}}
		}

		// Calculate effective bitrate: prioritize streamer byte-measured rate,
		// falling back to packet-rate estimation only when streamer rate is unavailable.
		totalBytes := (st.PacketsReceived + st.PacketsSent) * 160
		dt := now.Sub(hist.lastCheckTime).Seconds()
		if st.EffectiveKbps > 0 {
			hist.calculatedKbps = st.EffectiveKbps
		} else if dt >= 0.2 && hist.lastBytesRecv > 0 && totalBytes >= hist.lastBytesRecv {
			hist.calculatedKbps = (float64(totalBytes-hist.lastBytesRecv) * 8.0) / (dt * 1000.0)
		}
		hist.lastBytesRecv = totalBytes
		hist.lastCheckTime = now

		// Peer clock sync
		var peerSync ClockSyncHealth
		if peerOffsets != nil {
			if po, ok := peerOffsets[p.ID]; ok {
				peerSync = ClockSyncHealth{
					OffsetUS:  po.OffsetUS,
					DriftPPM:  po.DriftPPM,
					SyncRTTUS: po.RTTUS,
					IsMaster:  syncHealth.IsMaster,
				}
			}
		}

		// Loss ratio & percentage
		lossRatio := st.LossRatio
		if lossRatio <= 0 && (st.PacketsReceived+st.PacketsLost) > 0 {
			lossRatio = float64(st.PacketsLost) / float64(st.PacketsReceived+st.PacketsLost)
		}
		lossPercent := lossRatio * 100.0

		// Jitter delay
		jitterMS := st.JitterTargetDelayMS
		if jitterMS <= 0 && p.RTTMS > 0 {
			jitterMS = p.RTTMS / 2.0
		}

		swimState := p.SWIMState
		if swimState == "" {
			swimState = "alive"
		}

		hPeer := CalculatePeerHealthScore(
			lossPercent,
			underruns10s,
			jitterMS,
			peerSync.OffsetUS,
			peerSync.DriftPPM,
			swimState,
			false,
		)

		rtt := p.RTTMS
		if rtt <= 0 && peerSync.SyncRTTUS > 0 {
			rtt = peerSync.SyncRTTUS / 1000.0
		}

		latencyMatrix[p.ID] = rtt
		peerScores = append(peerScores, hPeer)

		peerHealthMap[p.ID] = PeerLinkHealth{
			PeerID:           p.ID,
			Address:          p.Addr,
			HealthScore:      math.Round(hPeer*100) / 100,
			HealthGrade:      CalculateGrade(hPeer),
			SWIMState:        swimState,
			DirectPingFailed: false,
			RTTMS:            rtt,
			LastSeen:         p.LastSeen,
			LossRatio:        math.Round(lossRatio*10000) / 10000,
			BitrateKbps:      math.Round(hist.calculatedKbps*10) / 10,
			Streaming:        st,
			Sync:             peerSync,
		}
	}

	// Prune departed peers
	activePeerIDs := make(map[string]bool, len(peers))
	for _, p := range peers {
		activePeerIDs[p.ID] = true
	}
	for id := range a.peerHist {
		if !activePeerIDs[id] {
			delete(a.peerHist, id)
		}
	}

	hNode := CalculateNodeHealthScore(peerScores, localHWUnderruns10s)
	hNode = math.Round(hNode*100) / 100

	snap := &NetworkHealthSnapshot{
		NodeID:         nodeID,
		Timestamp:      now,
		HealthScore:    hNode,
		HealthGrade:    CalculateGrade(hNode),
		Cluster:        discHealth,
		Sync:           syncHealth,
		LatencyMatrix:  latencyMatrix,
		Peers:          peerHealthMap,
		Streams:        streamMap,
		LocalUnderruns: hwTotal,
	}

	a.snapshot.Store(snap)
	return snap
}

func (a *Aggregator) run() {
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

func (a *Aggregator) emptySnapshot(now time.Time) *NetworkHealthSnapshot {
	return &NetworkHealthSnapshot{
		NodeID:        a.nodeID,
		Timestamp:     now,
		HealthScore:   100.0,
		HealthGrade:   GradeExcellent,
		LatencyMatrix: make(map[string]float64),
		Peers:         make(map[string]PeerLinkHealth),
		Streams:       make(map[string]StreamLinkStats),
	}
}

func pruneOldSamples(samples []timestampedCount, cutoff time.Time) []timestampedCount {
	if len(samples) <= 1 {
		return samples
	}
	idx := -1
	for i, s := range samples {
		if s.t.After(cutoff) {
			idx = i
			break
		}
	}
	if idx > 0 {
		return samples[idx-1:]
	} else if idx == -1 {
		// All samples are before cutoff; retain only the latest sample
		return samples[len(samples)-1:]
	}
	return samples
}
