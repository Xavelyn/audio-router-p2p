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
	nodeB := stubPeers{id: "node-B"}

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
