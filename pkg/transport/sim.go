package transport

import (
	"math/rand"
	"net"
	"sync"
	"time"
)

// maxBurstBytes caps how much delayed traffic a token bucket may bank, so a
// silent stretch cannot turn into a burst that floods the receiver or the
// kernel socket buffer. 64 KiB is well above any sustained audio rate yet
// small enough to fit a default UDP socket write buffer.
const maxBurstBytes = 64 << 10

// SimProfile describes the network conditions simulated in one direction.
// Percentages are 0-100 and delays are milliseconds; zero values are no-ops.
type SimProfile struct {
	Enabled       bool    `json:"enabled"`
	LossPct       float64 `json:"loss_pct"`       // drop this fraction of packets
	LatencyMS     float64 `json:"latency_ms"`     // fixed extra delay per packet
	JitterMS      float64 `json:"jitter_ms"`      // uniform random 0..N extra delay
	ReorderPct    float64 `json:"reorder_pct"`    // swap this fraction of packets with the next one
	DuplicatePct  float64 `json:"duplicate_pct"`  // deliver this fraction of packets twice
	BandwidthKbps float64 `json:"bandwidth_kbps"` // sustained rate cap; 0 = unlimited
}

// normalized clamps fields into their valid ranges.
func (p SimProfile) normalized() SimProfile {
	p.LossPct = clampPct(p.LossPct)
	p.LatencyMS = nonNeg(p.LatencyMS)
	p.JitterMS = nonNeg(p.JitterMS)
	p.ReorderPct = clampPct(p.ReorderPct)
	p.DuplicatePct = clampPct(p.DuplicatePct)
	p.BandwidthKbps = nonNeg(p.BandwidthKbps)
	return p
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func nonNeg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// SimProfiles holds the per-direction profiles applied by a NetworkSimulator.
type SimProfiles struct {
	In  SimProfile `json:"in"`  // ingress: packets received, before handler dispatch
	Out SimProfile `json:"out"` // egress: packets handed to the socket
}

// NetworkSimulator decorates a UDPTransport with configurable network
// conditions: loss, added latency, jitter, reordering, duplication, and a
// bandwidth cap, each independently per direction. It is a development/QA
// tool for exercising the jitter buffer, FEC, and PLC paths headlessly; it is
// not used for DTLS, which is a reliable stream. Profiles can be changed at
// runtime without disturbing in-flight audio.
type NetworkSimulator struct {
	inner     *UDPTransport
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.RWMutex
	profiles SimProfiles

	outBucket tokenBucket
	inBucket  tokenBucket
}

// NewNetworkSimulator wraps inner. out is the initial egress profile (for
// example from the -sim-* flags); ingress starts disabled.
func NewNetworkSimulator(inner *UDPTransport, out SimProfile) *NetworkSimulator {
	return &NetworkSimulator{
		inner:    inner,
		done:     make(chan struct{}),
		profiles: SimProfiles{Out: out.normalized()},
	}
}

// Addr delegates to the wrapped transport.
func (s *NetworkSimulator) Addr() string { return s.inner.Addr() }

// LocalAddr delegates to the wrapped transport.
func (s *NetworkSimulator) LocalAddr() *net.UDPAddr { return s.inner.LocalAddr() }

// Send applies the egress profile and returns immediately; delayed packets
// are delivered asynchronously. Drops return nil, like a real network.
func (s *NetworkSimulator) Send(to string, data []byte) error {
	prof := s.egressProfile()
	if !prof.Enabled {
		return s.inner.Send(to, data)
	}
	if hitsPct(prof.LossPct) {
		return nil
	}
	pkt := append([]byte(nil), data...)
	dup, extra := rollExtras(prof)
	s.scheduleOut(prof, to, pkt, extra)
	if dup {
		s.scheduleOut(prof, to, pkt, extra)
	}
	return nil
}

// Receive wraps the handler with the ingress profile.
func (s *NetworkSimulator) Receive(handler func([]byte, *net.UDPAddr)) (unregister func()) {
	return s.inner.Receive(func(data []byte, addr *net.UDPAddr) {
		prof := s.ingressProfile()
		if !prof.Enabled {
			handler(data, addr)
			return
		}
		if hitsPct(prof.LossPct) {
			return
		}
		pkt := append([]byte(nil), data...)
		dup, extra := rollExtras(prof)
		s.scheduleIn(prof, handler, addr, pkt, extra)
		if dup {
			s.scheduleIn(prof, handler, addr, pkt, extra+time.Duration(10+rand.Intn(40))*time.Millisecond)
		}
	})
}

// Wait delegates to the wrapped transport.
func (s *NetworkSimulator) Wait() { s.inner.Wait() }

// Closed delegates to the wrapped transport.
func (s *NetworkSimulator) Closed() bool { return s.inner.Closed() }

// Close stops pending simulated deliveries and closes the wrapped transport.
// It is idempotent.
func (s *NetworkSimulator) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return s.inner.Close()
}

// Profiles returns a copy of the current per-direction profiles.
func (s *NetworkSimulator) Profiles() SimProfiles {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.profiles
}

// egressProfile and ingressProfile are the hot-path profile reads.
func (s *NetworkSimulator) egressProfile() SimProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.profiles.Out
}

func (s *NetworkSimulator) ingressProfile() SimProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.profiles.In
}

// SetProfiles replaces both profiles atomically and clamps them into range.
func (s *NetworkSimulator) SetProfiles(p SimProfiles) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = SimProfiles{In: p.In.normalized(), Out: p.Out.normalized()}
}

// hitsPct reports whether a uniform roll lands below pct (0-100).
func hitsPct(pct float64) bool {
	if pct >= 100 {
		return true
	}
	return pct > 0 && rand.Float64()*100 < pct
}

// rollExtras rolls the duplicate and reorder dice for one packet. It returns
// whether the packet should be delivered twice and the extra delay to apply
// beyond the profile's base latency/jitter. A reordered packet trails by a
// few packet times so that later packets overtake it.
func rollExtras(prof SimProfile) (dup bool, extra time.Duration) {
	if hitsPct(prof.ReorderPct) {
		extra += 20*time.Millisecond + time.Duration(rand.Intn(80))*time.Millisecond
	}
	if hitsPct(prof.DuplicatePct) {
		dup = true
		extra += time.Duration(10+rand.Intn(40)) * time.Millisecond
	}
	return dup, extra
}

// simDelay computes the per-packet simulated delay: fixed latency plus a
// uniform random jitter component.
func (s *NetworkSimulator) simDelay(prof SimProfile) time.Duration {
	ms := prof.LatencyMS
	if prof.JitterMS > 0 {
		ms += rand.Float64() * prof.JitterMS
	}
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// scheduleOut delivers one egress packet after the simulated delay and
// bandwidth budget. The caller owns pkt (it must not be reused).
func (s *NetworkSimulator) scheduleOut(prof SimProfile, to string, pkt []byte, extra time.Duration) {
	go func() {
		delay := s.simDelay(prof) + extra
		if wait := s.outBucket.wait(len(pkt), prof.BandwidthKbps); wait > 0 {
			delay += wait
		}
		if delay > 0 {
			s.sleepOrDone(delay)
		}
		if s.inner.Closed() {
			return
		}
		_ = s.inner.Send(to, pkt)
	}()
}

// scheduleIn dispatches one ingress packet to the handler after the
// simulated delay and bandwidth budget.
func (s *NetworkSimulator) scheduleIn(prof SimProfile, handler func([]byte, *net.UDPAddr), addr *net.UDPAddr, pkt []byte, extra time.Duration) {
	go func() {
		delay := s.simDelay(prof) + extra
		if wait := s.inBucket.wait(len(pkt), prof.BandwidthKbps); wait > 0 {
			delay += wait
		}
		if delay > 0 {
			s.sleepOrDone(delay)
		}
		if s.inner.Closed() {
			return
		}
		handler(pkt, addr)
	}()
}

func (s *NetworkSimulator) sleepOrDone(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-s.done:
	}
}

// tokenBucket paces packet delivery to a sustained bandwidth.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// wait returns how long the caller should sleep before n bytes may pass at
// kbps (kilobits per second). Successive calls reserve capacity
// sequentially, so a burst of precomputed waits spaces deliveries out even
// when they are computed back-to-back.
func (b *tokenBucket) wait(n int, kbps float64) time.Duration {
	if kbps <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rate := kbps * 1000 / 8 // bytes per second
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
		b.tokens = maxBurstBytes
	}
	// Refill for real elapsed time, but never before the reservation horizon.
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * rate
		b.last = now
	}
	if b.tokens > maxBurstBytes {
		b.tokens = maxBurstBytes
	}
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return 0
	}
	// Not enough capacity now: reserve the deficit at the rate. The
	// reservation end moves forward with each caller, so successive waits
	// space out; the caller sleeps until its own reservation end.
	need := float64(n) - b.tokens
	end := b.last.Add(time.Duration(need / rate * float64(time.Second)))
	b.tokens = 0
	b.last = end
	if end.After(now) {
		return end.Sub(now)
	}
	return 0
}
