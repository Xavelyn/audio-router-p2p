package sync

import (
	"math"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
)

type stubPeers struct {
	id    string
	peers []discovery.Peer
}

func (s stubPeers) NodeID() string          { return s.id }
func (s stubPeers) Peers() []discovery.Peer { return s.peers }

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func hostPort(p int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
}

// TestManagerRoundTrip verifies that a clock offset is estimated against a peer
// over UDP on loopback.
func TestManagerRoundTrip(t *testing.T) {
	portA := freeUDPPort(t)
	portB := freeUDPPort(t)

	nodeA := stubPeers{id: "node-A", peers: []discovery.Peer{{
		ID: "node-B", SyncAddr: hostPort(portB),
	}}}
	nodeB := stubPeers{id: "node-B", peers: []discovery.Peer{{
		ID: "node-A", SyncAddr: hostPort(portA),
	}}}

	mA := NewManager(&config.Config{SyncPort: portA, SyncInterval: 50 * time.Millisecond, PeerTTL: 5 * time.Second}, nodeA)
	mB := NewManager(&config.Config{SyncPort: portB, SyncInterval: 50 * time.Millisecond, PeerTTL: 5 * time.Second}, nodeB)
	go mA.Run()
	go mB.Run()
	defer mA.Close()
	defer mB.Close()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := mA.Offsets()["node-B"]; ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	offsets := mA.Offsets()
	o, ok := offsets["node-B"]
	if !ok {
		t.Fatal("manager A never received a sync response from node-B")
	}
	if o.LastUpdate.IsZero() {
		t.Fatal("offset has no timestamp")
	}
	// Offset magnitude must be reasonable for a loopback link.
	if o.Offset < -time.Second || o.Offset > time.Second {
		t.Fatalf("implausible offset: %v", o.Offset)
	}
	if o.RTT < 0 || o.RTT > time.Second {
		t.Fatalf("implausible RTT: %v", o.RTT)
	}
}

func TestOffsetComputation(t *testing.T) {
	// NTP formula: offset = ((t2 - t1) + (t3 - t4)) / 2
	const skew = 250 * time.Millisecond
	const delay = 10 * time.Millisecond
	const proc = 5 * time.Millisecond

	base := time.Unix(0, 0)
	t1 := base
	t2 := t1.Add(delay + skew) // server clock
	t3 := t2.Add(proc)         // server clock
	t4 := t3.Add(delay - skew) // client clock (t4 = t3 - θ + δ)

	offset, rtt := ComputeOffsetAndRTT(t1.UnixNano(), t2.UnixNano(), t3.UnixNano(), t4.UnixNano())
	if offset < skew-10*time.Millisecond || offset > skew+10*time.Millisecond {
		t.Fatalf("offset formula wrong: got %v want ~%v", offset, skew)
	}
	expectedRTT := 2 * delay
	if rtt < expectedRTT-5*time.Millisecond || rtt > expectedRTT+5*time.Millisecond {
		t.Fatalf("rtt formula wrong: got %v want ~%v", rtt, expectedRTT)
	}
}

func TestComputeOffsetAndRTT_NegativeClamp(t *testing.T) {
	// If t4 < t1 due to step adjustment, RTT must clamp to 0 instead of reporting negative duration
	offset, rtt := ComputeOffsetAndRTT(1000, 500, 600, 800)
	if rtt < 0 {
		t.Fatalf("expected non-negative RTT, got %v", rtt)
	}
	if offset != -350 {
		t.Fatalf("expected offset -350ns, got %v", offset)
	}
}

func TestComputeDriftPPM(t *testing.T) {
	// 1. Initial measurement (hasPrev == false)
	drift := ComputeDriftPPM(0, 10*time.Millisecond, 0, time.Second, false)
	if drift != 0.0 {
		t.Fatalf("expected initial drift 0, got %f", drift)
	}

	// 2. dt < MinDriftInterval (50ms) -> keep previous
	drift2 := ComputeDriftPPM(0, 10*time.Millisecond, 5.0, 20*time.Millisecond, true)
	if drift2 != 5.0 {
		t.Fatalf("expected retained drift 5.0, got %f", drift2)
	}

	// 3. dt > MaxDriftGap (60s) -> reset
	drift3 := ComputeDriftPPM(0, 10*time.Millisecond, 5.0, 70*time.Second, true)
	if drift3 != 0.0 {
		t.Fatalf("expected reset drift 0.0 on stale interval, got %f", drift3)
	}

	// 4. Normal EWMA drift calculation
	// 100 microseconds drift over 1 second = 100 PPM instant.
	// prev = 0.0, alpha = 0.2 -> result = 20.0 PPM
	drift4 := ComputeDriftPPM(0, 100*time.Microsecond, 0.0, time.Second, true)
	if math.Abs(drift4-20.0) > 0.001 {
		t.Fatalf("expected EWMA drift 20.0, got %f", drift4)
	}

	// 5. Extreme step clamp: 1 second jump over 1 second (1,000,000 PPM) must be clamped to MaxPlausibleDriftPPM (2000.0)
	drift5 := ComputeDriftPPM(0, time.Second, 0.0, time.Second, true)
	expectedClamped := 0.2 * MaxPlausibleDriftPPM
	if math.Abs(drift5-expectedClamped) > 0.001 {
		t.Fatalf("expected clamped drift %f, got %f", expectedClamped, drift5)
	}
}

// TestSyncIgnoresClaimedIdentity verifies that a response from a discovered
// peer is attributed to that peer's ID, not to the sender-claimed ID.
func TestSyncIgnoresClaimedIdentity(t *testing.T) {
	peerPort := freeUDPPort(t)
	mgr := NewManager(
		&config.Config{SyncPort: freeUDPPort(t), SyncInterval: time.Hour, PeerTTL: 5 * time.Second},
		stubPeers{id: "node-A", peers: []discovery.Peer{{ID: "node-B", SyncAddr: hostPort(peerPort)}}},
	)
	defer mgr.Close()

	const nonce = uint64(1234)
	mgr.mu.Lock()
	mgr.pending[nonce] = time.Unix(0, 100)
	mgr.mu.Unlock()

	from := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: peerPort}
	mgr.handle(from, &message{
		Type: msgResponse, ID: "attacker", Nonce: nonce,
		T1: 100, T2: 200, T3: 200, T4: 300,
	})

	offsets := mgr.Offsets()
	if _, ok := offsets["attacker"]; ok {
		t.Fatal("sender-claimed ID was trusted for offset accounting")
	}
	if _, ok := offsets["node-B"]; !ok {
		t.Fatal("response from discovered source was not attributed to its peer ID")
	}
}

// TestSyncRejectsUndiscoveredSources verifies that both sync directions are
// restricted to current discovery peers.
func TestSyncRejectsUndiscoveredSources(t *testing.T) {
	knownPeerPort := freeUDPPort(t)
	mgr := NewManager(
		&config.Config{SyncPort: freeUDPPort(t), SyncInterval: time.Hour, PeerTTL: 5 * time.Second},
		stubPeers{id: "node-A", peers: []discovery.Peer{{ID: "node-B", SyncAddr: hostPort(knownPeerPort)}}},
	)

	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen receiver: %v", err)
	}
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		receiver.Close()
		t.Fatalf("listen sender: %v", err)
	}
	defer sender.Close()
	mgr.mu.Lock()
	mgr.conn = receiver
	const nonce = uint64(5678)
	mgr.pending[nonce] = time.Unix(0, 100)
	mgr.mu.Unlock()
	defer mgr.Close()

	unknown := sender.LocalAddr().(*net.UDPAddr)
	mgr.handle(unknown, &message{
		Type: msgResponse, ID: "node-B", Nonce: nonce,
		T1: 100, T2: 200, T3: 200, T4: 300,
	})
	if offsets := mgr.Offsets(); len(offsets) != 0 {
		t.Fatalf("undiscovered source created offsets: %v", offsets)
	}

	mgr.handle(unknown, &message{Type: msgRequest, ID: "node-B", Nonce: 1, T1: 100})
	if err := sender.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("set sender read deadline: %v", err)
	}
	buf := make([]byte, 256)
	if n, _, err := sender.ReadFromUDP(buf); err == nil {
		t.Fatalf("undiscovered source induced a %d-byte sync response", n)
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("reading from undiscovered source: %v", err)
	}
}

func TestSyncTelemetry(t *testing.T) {
	disc := stubPeers{id: "local-node"}
	cfg := &config.Config{
		SyncPort:     0,
		SyncInterval: time.Second,
		PeerTTL:      time.Second,
	}
	m := NewManager(cfg, disc)
	defer m.Close()

	m.SetMaster(true)
	if !m.IsMaster() {
		t.Error("expected IsMaster to be true")
	}

	m.mu.Lock()
	m.offsets["peer-1"] = PeerOffset{
		Offset:     -15 * time.Microsecond,
		RTT:        1200 * time.Microsecond,
		DriftPPM:   1.5,
		LastUpdate: time.Now(),
	}
	m.mu.Unlock()

	telem := m.Telemetry()
	if !telem.IsMaster {
		t.Error("expected telem.IsMaster true")
	}
	if telem.OffsetUS != -15.0 || telem.SyncRTTUS != 1200.0 || telem.DriftPPM != 1.5 {
		t.Errorf("unexpected sync telemetry: %+v", telem)
	}

	po := m.PeerOffsets()
	if len(po) != 1 || po["peer-1"].OffsetUS != -15.0 {
		t.Errorf("unexpected peer offsets: %+v", po)
	}
}

func TestProtocolEncodingAndDecoding(t *testing.T) {
	orig := message{
		Type:  msgRequest,
		ID:    "test-node",
		Nonce: 99887766,
		T1:    123456789,
	}

	encoded, err := encodeMessage(&orig)
	if err != nil {
		t.Fatalf("encodeMessage failed: %v", err)
	}

	decoded, err := decodeMessage(encoded)
	if err != nil {
		t.Fatalf("decodeMessage failed: %v", err)
	}

	if decoded.Type != orig.Type || decoded.ID != orig.ID || decoded.Nonce != orig.Nonce || decoded.T1 != orig.T1 {
		t.Fatalf("decoded mismatch: got %+v, want %+v", decoded, orig)
	}

	// Malformed inputs
	if _, err := decodeMessage(nil); err == nil {
		t.Fatal("expected error on nil input")
	}
	if _, err := decodeMessage([]byte(`{"type":"invalid"}`)); err == nil {
		t.Fatal("expected error on invalid type")
	}
	if _, err := decodeMessage([]byte(`{"type":"sync-request","nonce":0}`)); err == nil {
		t.Fatal("expected error on zero nonce")
	}
}

func TestEndpointMatching(t *testing.T) {
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.100"), Port: 45402}

	// Match exact numeric IPv4
	if !syncAddrMatches("192.168.1.100:45402", src) {
		t.Fatal("expected matching numeric IPv4 endpoint")
	}

	// Port mismatch
	if syncAddrMatches("192.168.1.100:45403", src) {
		t.Fatal("expected port mismatch")
	}

	// IP mismatch
	if syncAddrMatches("192.168.1.101:45402", src) {
		t.Fatal("expected IP mismatch")
	}

	// Invalid format
	if syncAddrMatches("invalid-no-port", src) {
		t.Fatal("expected invalid endpoint to fail")
	}
}

func TestManagerConfigDefaults(t *testing.T) {
	// Zero/negative interval and TTL must not panic or cause immediate drops
	m := NewManager(&config.Config{SyncInterval: -1, PeerTTL: 0}, stubPeers{id: "node"})
	if m.syncInterval != defaultSyncInterval {
		t.Fatalf("expected default sync interval %v, got %v", defaultSyncInterval, m.syncInterval)
	}
	if m.peerTTL != defaultPeerTTL {
		t.Fatalf("expected default peer TTL %v, got %v", defaultPeerTTL, m.peerTTL)
	}
	m.Close()
}

func TestSweepPending(t *testing.T) {
	m := NewManager(&config.Config{PeerTTL: 100 * time.Millisecond}, stubPeers{id: "node"})
	defer m.Close()

	m.mu.Lock()
	m.pending[1] = time.Now().Add(-200 * time.Millisecond) // expired
	m.pending[2] = time.Now()                              // fresh
	m.mu.Unlock()

	m.sweepPending()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, exists := m.pending[1]; exists {
		t.Fatal("expected nonce 1 to be swept")
	}
	if _, exists := m.pending[2]; !exists {
		t.Fatal("expected nonce 2 to be preserved")
	}
	if m.packetsLost.Load() != 1 {
		t.Fatalf("expected 1 lost packet, got %d", m.packetsLost.Load())
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	m := NewManager(&config.Config{SyncInterval: 10 * time.Millisecond, PeerTTL: time.Second}, stubPeers{id: "node"})
	defer m.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				m.SetMaster(id%2 == 0)
				_ = m.IsMaster()
				_ = m.Offsets()
				_ = m.Telemetry()
				_ = m.PeerOffsets()
			}
		}(i)
	}
	wg.Wait()
}
