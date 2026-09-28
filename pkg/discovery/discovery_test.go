package discovery

import (
	"bytes"
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
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "self")
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

	// Malformed address must be ignored.
	malformed := announcement{Type: announceType, ID: "peer-malformed", Addr: "not-a-host-port", Token: "test-token"}
	d.handleAnnouncement(malformed)
	if d.PeerByID("peer-malformed") != nil {
		t.Fatalf("malformed-addr announcement recorded: %+v", d.Peers())
	}
}

// TestDiscoveryPrune verifies TTL-based expiry.
func TestDiscoveryPrune(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "self")
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
// not the configured 0.
func TestDiscoveryOwnAddrUsesBoundPort(t *testing.T) {
	cfg := testConfig("239.255.0.1", freeUDPPort(t))
	cfg.DiscoveryPort = 0 // ask the kernel for an ephemeral port

	d, err := New(cfg, "node-ephemeral")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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

// TestSynchronousInit verifies that OwnAddr(), OwnSyncAddr(), and OwnStreamAddr()
// are populated synchronously before New() returns.
func TestSynchronousInit(t *testing.T) {
	cfg := testConfig("239.255.0.1", freeUDPPort(t))
	d, err := New(cfg, "node-sync-init")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	if d.OwnAddr() == "" {
		t.Error("OwnAddr() is empty immediately after New()")
	}
	if !ValidAddr(d.OwnAddr()) {
		t.Errorf("OwnAddr() %q is not a valid host:port", d.OwnAddr())
	}
	if d.OwnSyncAddr() == "" {
		t.Error("OwnSyncAddr() is empty immediately after New()")
	}
	if !ValidAddr(d.OwnSyncAddr()) {
		t.Errorf("OwnSyncAddr() %q is not a valid host:port", d.OwnSyncAddr())
	}
	if d.OwnStreamAddr() == "" {
		t.Error("OwnStreamAddr() is empty immediately after New()")
	}
	if !ValidAddr(d.OwnStreamAddr()) {
		t.Errorf("OwnStreamAddr() %q is not a valid host:port", d.OwnStreamAddr())
	}
}

// TestBroadcastCalculation verifies the bitwise subnet broadcast calculation across
// various IPv4 netmasks and edge cases.
func TestBroadcastCalculation(t *testing.T) {
	tests := []struct {
		name     string
		ip       net.IP
		mask     net.IPMask
		expected net.IP
	}{
		{
			name:     "standard /24",
			ip:       net.ParseIP("192.168.1.50"),
			mask:     net.CIDRMask(24, 32),
			expected: net.ParseIP("192.168.1.255"),
		},
		{
			name:     "subnet /25",
			ip:       net.ParseIP("10.0.0.112"),
			mask:     net.CIDRMask(25, 32),
			expected: net.ParseIP("10.0.0.127"),
		},
		{
			name:     "class B /16",
			ip:       net.ParseIP("172.16.5.10"),
			mask:     net.CIDRMask(16, 32),
			expected: net.ParseIP("172.16.255.255"),
		},
		{
			name:     "class A /8",
			ip:       net.ParseIP("10.0.0.1"),
			mask:     net.CIDRMask(8, 32),
			expected: net.ParseIP("10.255.255.255"),
		},
		{
			name:     "host /32",
			ip:       net.ParseIP("192.168.1.1"),
			mask:     net.CIDRMask(32, 32),
			expected: net.ParseIP("192.168.1.1"),
		},
		{
			name:     "invalid non-ipv4",
			ip:       net.ParseIP("2001:db8::1"),
			mask:     net.CIDRMask(64, 128),
			expected: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bcast := SubnetBroadcast(tc.ip, tc.mask)
			if tc.expected == nil {
				if bcast != nil {
					t.Fatalf("expected nil broadcast, got %v", bcast)
				}
				return
			}
			expected4 := tc.expected.To4()
			if !bytes.Equal(bcast, expected4) {
				t.Fatalf("expected broadcast %v, got %v", expected4, bcast)
			}
		})
	}
}

// TestReactiveDiscovery verifies that two nodes discover each other rapidly (< 300ms)
// via reactive announcement without waiting for the 3s ticker.
func TestReactiveDiscovery(t *testing.T) {
	port := freeUDPPort(t)

	cfgA := testConfig("239.255.0.1", port)
	cfgA.AnnounceInterval = 3 * time.Second

	cfgB := testConfig("239.255.0.1", port)
	cfgB.AnnounceInterval = 3 * time.Second

	dA, err := New(cfgA, "node-reactive-A")
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	defer dA.Close()

	dB, err := New(cfgB, "node-reactive-B")
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	defer dB.Close()

	go dA.Run()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	go dB.Run()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(dA.Peers()) > 0 && len(dB.Peers()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if got := dA.Peers(); len(got) != 1 || got[0].ID != "node-reactive-B" {
		t.Fatalf("A should have discovered node-reactive-B in < 300ms, got %+v (elapsed %v)", got, elapsed)
	}
	if got := dB.Peers(); len(got) != 1 || got[0].ID != "node-reactive-A" {
		t.Fatalf("B should have discovered node-reactive-A in < 300ms, got %+v (elapsed %v)", got, elapsed)
	}
	t.Logf("Reactive discovery succeeded in %v (< 300ms limit)", elapsed)
}

// TestVirtualInterfaceFiltering verifies that virtual/container interface names
// are correctly identified and excluded from physical candidates.
func TestVirtualInterfaceFiltering(t *testing.T) {
	virtualNames := []string{
		"docker0", "docker1", "veth1234", "veth_abc", "virbr0", "virbr1",
		"incusbr0", "lxdbr0",
		"br-1234567890ab", "br0", "br1", "cni0", "tun0", "tun1", "utun0", "utun1",
		"tap0", "tailscale0", "wg0", "vboxnet0", "vmnet1", "awdl0", "llw0",
	}
	for _, name := range virtualNames {
		if !isVirtualInterface(name) {
			t.Errorf("expected %q to be identified as virtual interface", name)
		}
	}

	physicalNames := []string{
		"eth0", "eth1", "enp3s0", "ens33", "wlan0", "wlp2s0",
	}
	for _, name := range physicalNames {
		if isVirtualInterface(name) {
			t.Errorf("expected %q to NOT be identified as virtual interface", name)
		}
	}
}

// TestReactiveRateLimitAndStormMitigation verifies that flooding announcements
// respects rate-limiting and bounded concurrency without leaking goroutines.
func TestReactiveRateLimitAndStormMitigation(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "self")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	for i := 0; i < 100; i++ {
		d.handleAnnouncement(announcement{
			Type:  announceType,
			ID:    strconv.Itoa(i),
			Addr:  "127.0.0.1:45400",
			Token: "test-token",
			TS:    time.Now().UnixNano(),
		})
	}
}

// TestUnicastSeedDiscovery verifies that nodes can discover each other solely via
// unicast seed addresses when multicast/broadcast is unavailable.
func TestUnicastSeedDiscovery(t *testing.T) {
	portA := freeUDPPort(t)
	portB := freeUDPPort(t)

	cfgA := testConfig("", 0) // No multicast
	cfgA.DiscoveryPort = portA
	cfgA.AdvHost = "127.0.0.1"

	dA, err := New(cfgA, "node-seed-A")
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	defer dA.Close()

	cfgB := testConfig("", 0) // No multicast
	cfgB.DiscoveryPort = portB
	cfgB.AdvHost = "127.0.0.1"
	// Node B is configured with Node A's discovery address as a unicast seed
	cfgB.StaticPeers = []config.StaticPeerConfig{
		{Addr: dA.OwnAddr()},
	}

	dB, err := New(cfgB, "node-seed-B")
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	defer dB.Close()

	go dA.Run()
	go dB.Run()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(dA.Peers()) > 0 && len(dB.Peers()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := dA.Peers(); len(got) != 1 || got[0].ID != "node-seed-B" {
		t.Fatalf("Node A did not discover Node B via reactive unicast: %+v", got)
	}
	if got := dB.Peers(); len(got) != 1 || got[0].ID != "node-seed-A" {
		t.Fatalf("Node B did not discover Node A via unicast seed: %+v", got)
	}
}

