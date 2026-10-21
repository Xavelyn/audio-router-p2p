package sync

import (
	"net"
	"strconv"
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
	if o.RTT <= 0 || o.RTT > time.Second {
		t.Fatalf("implausible RTT: %v", o.RTT)
	}
}

func TestOffsetComputation(t *testing.T) {
	// NTP formula: offset = ((t2 - t1) + (t3 - t4)) / 2
	// t1: client send (client clock)
	// t2: server receive (server clock = client + θ)
	// t3: server send (server clock)
	// t4: client receive (client clock)
	// θ = clock offset (server ahead of client), δ = network delay (symmetric)
	// t2 = t1 + δ + θ
	// t3 = t2 + proc
	// t4 = t3 + δ - θ (in client clock)
	const skew = 250 * time.Millisecond
	const delay = 10 * time.Millisecond
	const proc = 5 * time.Millisecond

	base := time.Unix(0, 0)
	t1 := base
	t2 := t1.Add(delay + skew) // server clock
	t3 := t2.Add(proc)         // server clock
	t4 := t3.Add(delay - skew) // client clock (t4 = t3 - θ + δ)

	offset := ((t2.UnixNano() - t1.UnixNano()) + (t3.UnixNano() - t4.UnixNano())) / 2
	if time.Duration(offset) < skew-10*time.Millisecond || time.Duration(offset) > skew+10*time.Millisecond {
		t.Fatalf("offset formula wrong: got %v want ~%v", time.Duration(offset), skew)
	}
}

// TestSyncIgnoresClaimedIdentity verifies that a response from a discovered
// peer is attributed to that peer's ID, not to the sender-claimed ID (finding
// 4.1). The claimed value remains on the wire for protocol compatibility.
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
// restricted to current discovery peers: an unknown source cannot add an
// offset or induce a response. This is admission control, not a replacement
// for cryptographic message authentication.
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
