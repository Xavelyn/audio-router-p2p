package discovery

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
)

func testConfig(group string, port int) *config.Config {
	return &config.Config{
		MulticastGroup:   group,
		MulticastPort:    port,
		AuthToken:        "test-token",
		AnnounceInterval: 150 * time.Millisecond,
		PeerTTL:          2 * time.Second,
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// TestDiscoveryIgnoresInvalidPeers covers the validation rules directly.
func TestDiscoveryIgnoresInvalidPeers(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", 47000), "self")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	valid := announcement{
		Type: announceType, ID: "peer-1", Addr: "10.0.0.1:5000", Token: "test-token",
	}
	d.handleAnnouncement(valid)
	if got := d.Peers(); len(got) != 1 || got[0].ID != "peer-1" {
		t.Fatalf("expected peer-1 discovered, got %+v", got)
	}

	// Self announcements must be ignored.
	self := announcement{Type: announceType, ID: "self", Addr: "10.0.0.1:5001", Token: "test-token"}
	d.handleAnnouncement(self)
	if got := d.Peers(); len(got) != 1 {
		t.Fatalf("self announcement recorded: %+v", got)
	}

	// Wrong token must be ignored.
	bad := announcement{Type: announceType, ID: "peer-2", Addr: "10.0.0.1:5002", Token: "nope"}
	d.handleAnnouncement(bad)
	if got := d.Peers(); len(got) != 1 {
		t.Fatalf("bad-token announcement recorded: %+v", got)
	}

	// Empty address must be ignored.
	empty := announcement{Type: announceType, ID: "peer-3", Token: "test-token"}
	d.handleAnnouncement(empty)
	if got := d.Peers(); len(got) != 1 {
		t.Fatalf("empty-addr announcement recorded: %+v", got)
	}
}

// TestDiscoveryPrune verifies TTL-based expiry.
func TestDiscoveryPrune(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", 47001), "self")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	d.handleAnnouncement(announcement{Type: announceType, ID: "p1", Addr: "10.0.0.1:5000", Token: "test-token"})
	d.handleAnnouncement(announcement{Type: announceType, ID: "p2", Addr: "10.0.0.1:5001", Token: "test-token"})

	d.mu.Lock()
	d.peers["p1"].LastSeen = time.Now().Add(-time.Minute) // stale
	d.mu.Unlock()

	d.prune()
	peers := d.Peers()
	if len(peers) != 1 || peers[0].ID != "p2" {
		t.Fatalf("expected only p2 to survive, got %+v", peers)
	}
}

// TestDiscoveryMulticastRoundTrip verifies two nodes discover each other over
// multicast on the same group/port.
func TestDiscoveryMulticastRoundTrip(t *testing.T) {
	port := freeUDPPort(t)

	dA, err := New(testConfig("239.255.0.1", port), "node-A")
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	dB, err := New(testConfig("239.255.0.1", port), "node-B")
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	go dA.Run()
	go dB.Run()
	defer dA.Close()
	defer dB.Close()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(dA.Peers()) > 0 && len(dB.Peers()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got := dA.Peers(); len(got) != 1 || got[0].ID != "node-B" {
		t.Fatalf("A should have discovered node-B, got %+v", got)
	}
	if got := dB.Peers(); len(got) != 1 || got[0].ID != "node-A" {
		t.Fatalf("B should have discovered node-A, got %+v", got)
	}
	if !ValidAddr(dA.Peers()[0].Addr) {
		t.Fatalf("peer addr %q not a host:port", dA.Peers()[0].Addr)
	}
}

// TestDiscoveryOwnAddrUsesBoundPort verifies that with DiscoveryPort=0
// (kernel-assigned ephemeral port) ownAddr advertises the port actually bound,
// not the configured 0 (finding 3.4).
func TestDiscoveryOwnAddrUsesBoundPort(t *testing.T) {
	cfg := testConfig("239.255.0.1", freeUDPPort(t))
	cfg.DiscoveryPort = 0 // ask the kernel for an ephemeral port

	d, err := New(cfg, "node-ephemeral")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// setup() binds synchronously (Run() calls it before starting any
	// goroutine), so drive it directly: no race, no sleeping, deterministic.
	d.setup()
	defer d.Close()

	if d.control == nil {
		t.Fatal("control socket not bound")
	}
	bound := d.control.LocalAddr().(*net.UDPAddr).Port
	if bound == 0 {
		t.Fatalf("control socket bound to port 0, want an ephemeral port")
	}

	_, portStr, err := net.SplitHostPort(d.OwnAddr())
	if err != nil {
		t.Fatalf("OwnAddr %q is not host:port: %v", d.OwnAddr(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("OwnAddr port %q not an int: %v", portStr, err)
	}
	if port != bound {
		t.Fatalf("OwnAddr port = %d, want real bound port %d", port, bound)
	}
}
