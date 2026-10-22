package transport

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// simHarness connects a simulated source socket to a recording sink. The
// source simulator applies the egress profile; the sink simulator applies
// the ingress profile, mirroring two separate nodes.
type simHarness struct {
	t    *testing.T
	sim  *NetworkSimulator // the egress (sender-side) simulator
	sink *UDPTransport
	got  chan []byte
}

func newSimHarness(t *testing.T, out, in SimProfile) *simHarness {
	t.Helper()

	sink, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("sink transport: %v", err)
	}

	src, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("source transport: %v", err)
	}

	srcSim := NewNetworkSimulator(src, out)
	srcSim.SetProfiles(SimProfiles{Out: out})
	sinkSim := NewNetworkSimulator(sink, SimProfile{})
	sinkSim.SetProfiles(SimProfiles{In: in})
	t.Cleanup(func() {
		sinkSim.Close()
		srcSim.Close()
	})

	got := make(chan []byte, 4096)
	sinkSim.Receive(func(data []byte, _ *net.UDPAddr) {
		select {
		case got <- append([]byte(nil), data...):
		default:
			// The test is not draining fast enough; dropping is fine for a
			// harness that only asserts on paced, promptly drained traffic.
		}
	})

	return &simHarness{t: t, sim: srcSim, sink: sink, got: got}
}

// sendSeq transmits n packets with a 4-byte sequence number payload.
func (h *simHarness) sendSeq(n int) {
	payload := make([]byte, 4)
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint32(payload, uint32(i))
		if err := h.sim.Send(h.sink.Addr(), payload); err != nil {
			h.t.Fatalf("send %d: %v", i, err)
		}
	}
}

// collect waits up to timeout for n datagrams.
func (h *simHarness) collect(n int, timeout time.Duration) [][]byte {
	var out [][]byte
	deadline := time.Now().Add(timeout)
	for len(out) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case p := <-h.got:
			out = append(out, p)
		case <-time.After(remaining):
		}
	}
	return out
}

// seq extracts the sequence number from a payload.
func seq(p []byte) int {
	if len(p) < 4 {
		return -1
	}
	return int(binary.BigEndian.Uint32(p))
}

func TestSimPassthroughWhenDisabled(t *testing.T) {
	h := newSimHarness(t, SimProfile{}, SimProfile{})
	h.sendSeq(50)
	got := h.collect(50, 2*time.Second)
	if len(got) != 50 {
		t.Fatalf("disabled simulator must pass all packets through: got %d of 50", len(got))
	}
	for i, p := range got {
		if seq(p) != i {
			t.Fatalf("packet %d arrived out of order (seq %d)", i, seq(p))
		}
	}
}

func TestSimEgressLoss(t *testing.T) {
	// 100% loss: nothing arrives.
	h := newSimHarness(t, SimProfile{Enabled: true, LossPct: 100}, SimProfile{})
	h.sendSeq(10)
	time.Sleep(200 * time.Millisecond)
	if n := len(h.got); n != 0 {
		t.Fatalf("100%% loss must drop everything: %d arrived", n)
	}

	// 50% loss: roughly half arrives (wide bounds keep it race-free).
	h2 := newSimHarness(t, SimProfile{Enabled: true, LossPct: 50}, SimProfile{})
	h2.sendSeq(200)
	time.Sleep(500 * time.Millisecond) // no delays configured: survivors are in
	n := 0
drain:
	for {
		select {
		case <-h2.got:
			n++
		default:
			break drain
		}
	}
	if n < 40 || n > 160 {
		t.Fatalf("50%% loss over 200 packets should keep ~100, kept %d", n)
	}
}

func TestSimLatencyAddsDelay(t *testing.T) {
	h := newSimHarness(t, SimProfile{Enabled: true, LatencyMS: 100}, SimProfile{})
	start := time.Now()
	h.sendSeq(1)
	got := h.collect(1, 3*time.Second)
	if len(got) != 1 {
		t.Fatal("packet lost despite 0% loss")
	}
	elapsed := time.Since(start)
	if elapsed < 95*time.Millisecond {
		t.Fatalf("expected >= ~100 ms one-way delay, got %s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("simulated delay looks unbounded: %s", elapsed)
	}
}

func TestSimJitterVariesDelay(t *testing.T) {
	h := newSimHarness(t, SimProfile{Enabled: true, LatencyMS: 20, JitterMS: 150}, SimProfile{})
	intervals := make([]time.Duration, 0, 20)
	for i := 0; i < 20; i++ {
		start := time.Now()
		h.sendSeq(1)
		if got := h.collect(1, 3*time.Second); len(got) != 1 {
			t.Fatalf("packet %d lost", i)
		}
		intervals = append(intervals, time.Since(start))
	}
	min, max := intervals[0], intervals[0]
	for _, d := range intervals[1:] {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	if max-min < 20*time.Millisecond {
		t.Fatalf("jitter should vary per-packet delay (min %s, max %s)", min, max)
	}
}

func TestSimReorderSwapsAdjacentPackets(t *testing.T) {
	h := newSimHarness(t, SimProfile{Enabled: true, ReorderPct: 100, LatencyMS: 5}, SimProfile{})
	// Paced sends so the simulator sees one packet at a time.
	payload := make([]byte, 4)
	for i := 0; i < 10; i++ {
		binary.BigEndian.PutUint32(payload, uint32(i))
		if err := h.sim.Send(h.sink.Addr(), payload); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := h.collect(10, 3*time.Second)
	if len(got) != 10 {
		t.Fatalf("expected 10 reordered packets, got %d", len(got))
	}
	inversions := 0
	for i := 1; i < len(got); i++ {
		if seq(got[i]) < seq(got[i-1]) {
			inversions++
		}
	}
	if inversions == 0 {
		t.Fatalf("100%% reorder produced in-order arrival: %v", seqs(got))
	}
}

func seqs(ps [][]byte) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = seq(p)
	}
	return out
}

func TestSimDuplicateDeliversTwice(t *testing.T) {
	h := newSimHarness(t, SimProfile{Enabled: true, DuplicatePct: 100, LatencyMS: 5}, SimProfile{})
	h.sendSeq(5)
	got := h.collect(10, 3*time.Second)
	if len(got) != 10 {
		t.Fatalf("100%% duplicate should deliver 10 datagrams, got %d: %v", len(got), seqs(got))
	}
	seen := map[int]int{}
	for _, p := range got {
		seen[seq(p)]++
	}
	for i := 0; i < 5; i++ {
		if seen[i] != 2 {
			t.Fatalf("sequence %d delivered %d times, want 2", i, seen[i])
		}
	}
}

func TestSimIngressConditions(t *testing.T) {
	// Ingress loss on the receiver side: the sender is clean.
	h := newSimHarness(t, SimProfile{}, SimProfile{Enabled: true, LossPct: 100})
	h.sendSeq(10)
	time.Sleep(200 * time.Millisecond)
	if n := len(h.got); n != 0 {
		t.Fatalf("100%% ingress loss must drop everything: %d arrived", n)
	}

	// Ingress latency delays handler dispatch.
	h2 := newSimHarness(t, SimProfile{}, SimProfile{Enabled: true, LatencyMS: 80})
	start := time.Now()
	h2.sendSeq(1)
	if got := h2.collect(1, 3*time.Second); len(got) != 1 {
		t.Fatal("ingress packet lost")
	}
	if elapsed := time.Since(start); elapsed < 75*time.Millisecond {
		t.Fatalf("expected >= ~80 ms ingress delay, got %s", elapsed)
	}
}

func TestSimBandwidthPacesTraffic(t *testing.T) {
	// Within the burst allowance everything passes instantly.
	h := newSimHarness(t, SimProfile{Enabled: true, BandwidthKbps: 50}, SimProfile{})
	big := make([]byte, 1024)
	for i := 0; i < 20; i++ {
		binary.BigEndian.PutUint32(big, uint32(i))
		if err := h.sim.Send(h.sink.Addr(), big); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.collect(20, 3*time.Second); len(got) != 20 {
		t.Fatalf("20 KiB is within the burst allowance, got %d of 20", len(got))
	}

	// Past the burst, the cap paces delivery: 300 x 1 KiB at 500 kbps
	// (62.5 KB/s) means ~236 KiB of traffic is held back for ~3.8 s.
	h2 := newSimHarness(t, SimProfile{Enabled: true, BandwidthKbps: 500}, SimProfile{})
	start := time.Now()
	for i := 0; i < 300; i++ {
		binary.BigEndian.PutUint32(big, uint32(i))
		if err := h2.sim.Send(h2.sink.Addr(), big); err != nil {
			t.Fatal(err)
		}
	}
	got := h2.collect(300, 20*time.Second)
	elapsed := time.Since(start)
	if len(got) != 300 {
		t.Fatalf("expected 300 packets, got %d", len(got))
	}
	if elapsed < 2500*time.Millisecond {
		t.Fatalf("bandwidth cap not pacing: 300 x 1 KiB at 500 kbps took %s", elapsed)
	}
}

func TestSimLiveReconfigure(t *testing.T) {
	h := newSimHarness(t, SimProfile{}, SimProfile{})
	h.sendSeq(10)
	if got := h.collect(10, 3*time.Second); len(got) != 10 {
		t.Fatalf("disabled simulator dropped packets: %d", len(got))
	}

	// Turn on total egress loss at runtime.
	h.sim.SetProfiles(SimProfiles{Out: SimProfile{Enabled: true, LossPct: 100}})
	h.sendSeq(10)
	time.Sleep(200 * time.Millisecond)
	if n := len(h.got); n != 0 {
		t.Fatalf("runtime-enabled loss must drop everything: %d arrived", n)
	}

	// Disable again.
	h.sim.SetProfiles(SimProfiles{Out: SimProfile{}})
	h.sendSeq(5)
	if got := h.collect(5, 3*time.Second); len(got) != 5 {
		t.Fatalf("disabling must restore passthrough: %d", len(got))
	}
}

func TestSimConcurrentSendAndReconfigure(t *testing.T) {
	h := newSimHarness(t, SimProfile{}, SimProfile{})
	stop := make(chan struct{})
	errs := make(chan error, 8)

	go func() {
		payload := make([]byte, 4)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			binary.BigEndian.PutUint32(payload, uint32(i))
			if err := h.sim.Send(h.sink.Addr(), payload); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	go func() {
		profiles := []SimProfiles{
			{},
			{Out: SimProfile{Enabled: true, LossPct: 25, JitterMS: 5}},
			{In: SimProfile{Enabled: true, ReorderPct: 10}, Out: SimProfile{Enabled: true, DuplicatePct: 5}},
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.sim.SetProfiles(profiles[i%len(profiles)])
			_ = h.sim.Profiles()
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)

	select {
	case err := <-errs:
		t.Fatalf("send error under concurrent reconfiguration: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Drain the receive channel briefly so cleanup does not log noise.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-h.got:
		default:
			return
		}
	}
}
