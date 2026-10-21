package discovery

import (
	"bytes"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/hashicorp/memberlist"
)

func testConfig(group string, mcastPort int) *config.Config {
	return &config.Config{
		MulticastGroup:   group,
		MulticastPort:    mcastPort,
		DiscoveryPort:    0, // Kernel assigns ephemeral port for Memberlist
		AuthToken:        "test-token",
		AnnounceInterval: 50 * time.Millisecond,
		PeerTTL:          2 * time.Second,
		AdvHost:          "127.0.0.1",
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

// TestDiscoveryInitialization verifies Init(), OwnAddr(), OwnSyncAddr(), OwnStreamAddr(),
// and ephemeral bound port resolution.
func TestDiscoveryInitialization(t *testing.T) {
	cfg := testConfig("239.255.0.1", freeUDPPort(t))
	cfg.DiscoveryPort = 0 // Ephemeral port

	d, err := New(cfg, "node-init-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	if d.NodeID() != "node-init-1" {
		t.Errorf("NodeID() = %q, want %q", d.NodeID(), "node-init-1")
	}

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

	// Verify ephemeral port was assigned and is non-zero
	_, portStr, err := net.SplitHostPort(d.OwnAddr())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		t.Fatalf("OwnAddr() port = %d, want > 0", port)
	}

	// Verify Init() is idempotent
	if err := d.Init(); err != nil {
		t.Fatalf("Init() re-invocation failed: %v", err)
	}
	if !ValidAddr(d.OwnAddr()) {
		t.Errorf("OwnAddr() invalid after Init(): %q", d.OwnAddr())
	}

	// Verify automatic NodeID generation when empty
	d2, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "")
	if err != nil {
		t.Fatalf("New auto-id: %v", err)
	}
	defer d2.Close()
	if d2.NodeID() == "" || len(d2.NodeID()) != 32 {
		t.Errorf("expected 32-char hex generated NodeID, got %q", d2.NodeID())
	}
}

// TestDiscoveryMemberlistRoundTrip verifies two nodes discover each other via
// zero-conf LAN beacon and Memberlist SWIM roundtrip, and verifies departure.
func TestDiscoveryMemberlistRoundTrip(t *testing.T) {
	mcastPort := freeUDPPort(t)

	cfgA := testConfig("239.255.0.1", mcastPort)
	dA, err := New(cfgA, "node-roundtrip-A")
	if err != nil {
		t.Fatalf("New dA: %v", err)
	}
	defer dA.Close()

	cfgB := testConfig("239.255.0.1", mcastPort)
	dB, err := New(cfgB, "node-roundtrip-B")
	if err != nil {
		t.Fatalf("New dB: %v", err)
	}
	defer dB.Close()

	go dA.Run()
	go dB.Run()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(dA.Peers()) > 0 && len(dB.Peers()) > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	peersA := dA.Peers()
	if len(peersA) != 1 || peersA[0].ID != "node-roundtrip-B" {
		t.Fatalf("node A peers want [node-roundtrip-B], got %+v", peersA)
	}
	peersB := dB.Peers()
	if len(peersB) != 1 || peersB[0].ID != "node-roundtrip-A" {
		t.Fatalf("node B peers want [node-roundtrip-A], got %+v", peersB)
	}

	// Verify metadata addresses match
	if peersA[0].SyncAddr != dB.OwnSyncAddr() {
		t.Errorf("node A discovered syncAddr = %q, want %q", peersA[0].SyncAddr, dB.OwnSyncAddr())
	}
	if peersA[0].StreamAddr != dB.OwnStreamAddr() {
		t.Errorf("node A discovered streamAddr = %q, want %q", peersA[0].StreamAddr, dB.OwnStreamAddr())
	}
	if !ValidAddr(peersA[0].Addr) {
		t.Errorf("node A peer addr %q is not valid host:port", peersA[0].Addr)
	}

	// Verify graceful departure
	if err := dB.Close(); err != nil {
		t.Fatalf("dB.Close(): %v", err)
	}

	departDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(departDeadline) {
		if len(dA.Peers()) == 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if remaining := dA.Peers(); len(remaining) != 0 {
		t.Fatalf("expected node-B to be removed after leave, remaining: %+v", remaining)
	}
}

// TestDiscoveryTokenRejection tests token validation and rejection across delegates,
// beacons, and full cluster join.
func TestDiscoveryTokenRejection(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "node-auth-host")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	// 1. AliveDelegate.NotifyAlive unit checks
	validMeta, _ := json.Marshal(nodeMetadata{ID: "peer-valid", AuthToken: "test-token"})
	if err := d.NotifyAlive(&memberlist.Node{Name: "peer-valid", Meta: validMeta}); err != nil {
		t.Errorf("NotifyAlive rejected valid token: %v", err)
	}

	badMeta, _ := json.Marshal(nodeMetadata{ID: "peer-bad", AuthToken: "wrong-token"})
	if err := d.NotifyAlive(&memberlist.Node{Name: "peer-bad", Meta: badMeta}); err == nil {
		t.Error("NotifyAlive accepted invalid token, expected error")
	}

	if err := d.NotifyAlive(&memberlist.Node{Name: "peer-empty", Meta: nil}); err == nil {
		t.Error("NotifyAlive accepted empty metadata, expected error")
	}

	if err := d.NotifyAlive(&memberlist.Node{Name: "peer-junk", Meta: []byte("invalid-json")}); err == nil {
		t.Error("NotifyAlive accepted malformed json, expected error")
	}

	if err := d.NotifyAlive(&memberlist.Node{Name: "node-auth-host"}); err != nil {
		t.Errorf("NotifyAlive rejected self node: %v", err)
	}

	// 2. Beacon token rejection
	d.handleBeacon(beaconMessage{Magic: beaconMagic, ID: "peer-beacon-bad", Token: "bad-token"}, nil)
	if d.PeerByID("peer-beacon-bad") != nil {
		t.Error("bad beacon added peer")
	}

	// 3. End-to-end token rejection between two nodes
	mcastPort := freeUDPPort(t)
	cfgA := testConfig("239.255.0.1", mcastPort)
	cfgA.AuthToken = "cluster-secret-A"
	dA, err := New(cfgA, "node-tok-A")
	if err != nil {
		t.Fatalf("New dA: %v", err)
	}
	defer dA.Close()

	cfgB := testConfig("239.255.0.1", mcastPort)
	cfgB.AuthToken = "cluster-secret-B"
	dB, err := New(cfgB, "node-tok-B")
	if err != nil {
		t.Fatalf("New dB: %v", err)
	}
	defer dB.Close()

	go dA.Run()
	go dB.Run()

	time.Sleep(300 * time.Millisecond)

	if gotA := dA.Peers(); len(gotA) != 0 {
		t.Fatalf("node A discovered node with different token: %+v", gotA)
	}
	if gotB := dB.Peers(); len(gotB) != 0 {
		t.Fatalf("node B discovered node with different token: %+v", gotB)
	}
}

// TestDiscoverySeedPeerJoining verifies that nodes discover each other solely via
// static seed peer addresses when multicast and broadcast are disabled.
func TestDiscoverySeedPeerJoining(t *testing.T) {
	cfgA := testConfig("", 0) // No multicast
	cfgA.DiscoveryPort = 0
	dA, err := New(cfgA, "node-seed-A")
	if err != nil {
		t.Fatalf("New dA: %v", err)
	}
	defer dA.Close()

	cfgB := testConfig("", 0) // No multicast
	cfgB.DiscoveryPort = 0
	cfgB.StaticPeers = []config.StaticPeerConfig{
		{Addr: dA.OwnAddr()},
	}
	dB, err := New(cfgB, "node-seed-B")
	if err != nil {
		t.Fatalf("New dB: %v", err)
	}
	defer dB.Close()

	go dA.Run()
	go dB.Run()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(dA.Peers()) > 0 && len(dB.Peers()) > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	peersA := dA.Peers()
	if len(peersA) != 1 || peersA[0].ID != "node-seed-B" {
		t.Fatalf("node A peers want [node-seed-B], got %+v", peersA)
	}
	peersB := dB.Peers()
	if len(peersB) != 1 || peersB[0].ID != "node-seed-A" {
		t.Fatalf("node B peers want [node-seed-A], got %+v", peersB)
	}

	if peersA[0].SyncAddr != dB.OwnSyncAddr() {
		t.Errorf("node A discovered syncAddr = %q, want %q", peersA[0].SyncAddr, dB.OwnSyncAddr())
	}
	if peersA[0].StreamAddr != dB.OwnStreamAddr() {
		t.Errorf("node A discovered streamAddr = %q, want %q", peersA[0].StreamAddr, dB.OwnStreamAddr())
	}
}

// TestDiscoveryAddPeerMockHelper verifies that AddPeer inserts mock peers into the
// table and all query methods resolve them correctly.
func TestDiscoveryAddPeerMockHelper(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "self")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	mockPeer := &Peer{
		ID:         "mock-peer-1",
		Addr:       "127.0.0.1:45401",
		SyncAddr:   "127.0.0.1:45402",
		StreamAddr: "127.0.0.1:45678",
		LastSeen:   time.Now(),
	}

	d.AddPeer(mockPeer)

	peers := d.Peers()
	if len(peers) != 1 || peers[0].ID != "mock-peer-1" {
		t.Fatalf("Peers() want [mock-peer-1], got %+v", peers)
	}

	if p := d.PeerByID("mock-peer-1"); p == nil || p.ID != "mock-peer-1" {
		t.Fatalf("PeerByID('mock-peer-1') = %+v, want mock-peer-1", p)
	}
	if p := d.PeerByID("unknown"); p != nil {
		t.Fatalf("PeerByID('unknown') = %+v, want nil", p)
	}

	if p := d.PeerByAddr("127.0.0.1:45678"); p == nil || p.ID != "mock-peer-1" {
		t.Fatalf("PeerByAddr streamAddr = %+v, want mock-peer-1", p)
	}
	if p := d.PeerByAddr("127.0.0.1:45401"); p == nil || p.ID != "mock-peer-1" {
		t.Fatalf("PeerByAddr ctrlAddr = %+v, want mock-peer-1", p)
	}
	if p := d.PeerByAddr("127.0.0.1:9999"); p != nil {
		t.Fatalf("PeerByAddr unknown = %+v, want nil", p)
	}

	addrs := d.PeerAddrs()
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:45401" {
		t.Fatalf("PeerAddrs() = %+v, want [127.0.0.1:45401]", addrs)
	}
}

func TestValidAPIAddr(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:8443",
		"https://192.168.1.100:8443",
		"http://stagebox.local:8080",
		"https://[::1]:8443",
		"http://node1:80",
	}
	for _, u := range valid {
		if !ValidAPIAddr(u) {
			t.Errorf("ValidAPIAddr(%q) = false, want true", u)
		}
	}

	invalid := []string{
		"",
		"ftp://127.0.0.1:21",
		"file:///etc/passwd",
		"http://user:pass@127.0.0.1:8080",
		"http://",
		"http://:8080",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:0",
		"not-a-url",
	}
	for _, u := range invalid {
		if ValidAPIAddr(u) {
			t.Errorf("ValidAPIAddr(%q) = true, want false", u)
		}
	}
}

// TestDiscoveryPrune verifies TTL expiry of peers.
func TestDiscoveryPrune(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "self")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	d.AddPeer(&Peer{ID: "p1", Addr: "127.0.0.1:5000", LastSeen: time.Now().Add(-time.Minute)})
	d.AddPeer(&Peer{ID: "p2", Addr: "127.0.0.1:5001", LastSeen: time.Now()})

	d.prune()

	peers := d.Peers()
	if len(peers) != 1 || peers[0].ID != "p2" {
		t.Fatalf("expected only p2 to survive prune, got %+v", peers)
	}
}

// TestBroadcastCalculation verifies the bitwise subnet broadcast calculation across IPv4 masks.
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

// TestFindBroadcastAddresses verifies discovering broadcast addresses without panicking.
func TestFindBroadcastAddresses(t *testing.T) {
	bcasts := findBroadcastAddresses()
	for _, b := range bcasts {
		if b.To4() == nil {
			t.Errorf("expected IPv4 address, got %v", b)
		}
	}
}

// TestDiscoveryGracefulShutdown verifies multiple Close() calls are safe and idempotent.
func TestDiscoveryGracefulShutdown(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "node-shutdown")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Errorf("first Close() returned error: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close() returned error: %v", err)
	}
}

func TestDiscoveryTelemetryCounters(t *testing.T) {
	d, err := New(testConfig("239.255.0.1", freeUDPPort(t)), "node-telem-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	d.AddPeer(&Peer{
		ID:         "peer-test-1",
		Addr:       "127.0.0.1:45400",
		SyncAddr:   "127.0.0.1:45402",
		StreamAddr: "127.0.0.1:45678",
		LastSeen:   time.Now(),
	})

	d.beaconsSent.Store(42)
	d.beaconsRecv.Store(55)
	d.tokenRejections.Store(3)
	d.seedJoinAttempts.Store(5)
	d.seedJoinSuccesses.Store(4)

	telem := d.Telemetry()
	if telem.BeaconsSent != 42 || telem.BeaconsReceived != 55 {
		t.Errorf("unexpected beacon counts: %+v", telem)
	}
	if telem.TokenRejections != 3 {
		t.Errorf("expected 3 token rejections, got %d", telem.TokenRejections)
	}
	if telem.SeedJoinAttempts != 5 || telem.SeedJoinSuccesses != 4 {
		t.Errorf("unexpected seed join counts: %+v", telem)
	}

	peers := d.DiscoveredPeers()
	if len(peers) != 1 || peers[0].ID != "peer-test-1" {
		t.Errorf("unexpected discovered peers: %+v", peers)
	}
	if state := d.PeerState("peer-test-1"); state != "alive" {
		t.Errorf("expected alive state, got %s", state)
	}
}

// TestDiscoveryAPIAddr verifies APIAddr derivation for TLS/plaintext and NodeMeta propagation.
func TestDiscoveryAPIAddr(t *testing.T) {
	// TLS enabled (default)
	cfgTLS := testConfig("239.255.0.1", freeUDPPort(t))
	cfgTLS.ListenAddr = "10.0.1.10:8443"
	cfgTLS.AdvHost = "10.0.1.10"
	cfgTLS.NoTLS = false

	dTLS, err := New(cfgTLS, "node-tls")
	if err != nil {
		t.Fatalf("New dTLS: %v", err)
	}
	defer dTLS.Close()

	if dTLS.APIAddr() != "https://10.0.1.10:8443" {
		t.Errorf("dTLS.APIAddr() = %q, want %q", dTLS.APIAddr(), "https://10.0.1.10:8443")
	}

	metaBytes := dTLS.NodeMeta(0)
	var meta nodeMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.APIAddr != "https://10.0.1.10:8443" {
		t.Errorf("meta.APIAddr = %q, want %q", meta.APIAddr, "https://10.0.1.10:8443")
	}

	// Plaintext HTTP (NoTLS = true)
	cfgPlain := testConfig("239.255.0.1", freeUDPPort(t))
	cfgPlain.ListenAddr = ":8080"
	cfgPlain.AdvHost = "192.168.1.100"
	cfgPlain.NoTLS = true

	dPlain, err := New(cfgPlain, "node-plain")
	if err != nil {
		t.Fatalf("New dPlain: %v", err)
	}
	defer dPlain.Close()

	if dPlain.APIAddr() != "http://192.168.1.100:8080" {
		t.Errorf("dPlain.APIAddr() = %q, want %q", dPlain.APIAddr(), "http://192.168.1.100:8080")
	}

	// Add peer with APIAddr and check DiscoveredPeers
	dTLS.AddPeer(&Peer{
		ID:         "peer-remote-1",
		Addr:       "10.0.1.11:45401",
		APIAddr:    "https://10.0.1.11:8443",
		SyncAddr:   "10.0.1.11:45402",
		StreamAddr: "10.0.1.11:45678",
		LastSeen:   time.Now(),
	})

	peers := dTLS.DiscoveredPeers()
	if len(peers) != 1 || peers[0].APIAddr != "https://10.0.1.11:8443" {
		t.Errorf("DiscoveredPeers APIAddr = %q, want %q", peers[0].APIAddr, "https://10.0.1.11:8443")
	}
}
