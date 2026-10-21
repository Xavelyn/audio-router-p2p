package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
	"github.com/audio-router-p2p/pkg/transport"
	"github.com/pion/rtp"
)

// TestMultiRoomSync tests multi-room synchronization with leader election
func TestMultiRoomSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	// Create 3 nodes for room testing
	nodeConfigs := make([]*nodeSetup, 3)
	for i := 0; i < 3; i++ {
		nodeConfigs[i] = setupNode(t, tmpDir, certFile, keyFile, i)
		defer nodeConfigs[i].cleanup()
	}

	// Start all nodes
	for _, n := range nodeConfigs {
		n.start()
	}
	time.Sleep(2 * time.Second) // Wait for discovery

	// Manually add peers to discovery for testing (bypasses multicast)
	for i, n := range nodeConfigs {
		for j, m := range nodeConfigs {
			if i != j {
				n.disc.AddPeer(&discovery.Peer{
					ID:         m.disc.NodeID(),
					Addr:       m.disc.OwnAddr(),
					SyncAddr:   m.disc.OwnSyncAddr(),
					StreamAddr: m.disc.OwnStreamAddr(),
					LastSeen:   time.Now(),
				})
			}
		}
	}
	time.Sleep(500 * time.Millisecond) // Allow peer info to propagate

	// Create room on all nodes
	roomID := group.RoomID("test-room-1")
	roomName := "Test Room"
	var rooms []*group.Room

	for _, n := range nodeConfigs {
		room := group.NewRoom(group.RoomConfig{
			ID:          roomID,
			Name:        roomName,
			LocalPeerID: n.disc.NodeID(),
			Config:      n.cfg,
			Discovery:   n.disc,
			SyncManager: n.sync,
			Transport:   n.trans,
		})
		room.SetCallbacks(
			func(m *group.RoomMember) { t.Logf("Node %d: member joined %s", n.idx, m.PeerID) },
			func(m *group.RoomMember) { t.Logf("Node %d: member left %s", n.idx, m.PeerID) },
			func(leaderID string) { t.Logf("Node %d: leader changed to %s", n.idx, leaderID) },
			func(state group.RoomState) { t.Logf("Node %d: state %v", n.idx, state) },
		)
		if err := room.Join(); err != nil {
			t.Fatalf("Node %d failed to join room: %v", n.idx, err)
		}
		rooms = append(rooms, room)
	}

	// Wait for leader election
	deadline := time.Now().Add(15 * time.Second)
	var leaderFound bool
	for time.Now().Before(deadline) {
		leaders := make(map[string]int)
		for _, r := range rooms {
			leaders[r.Leader()]++
		}
		for leaderID, count := range leaders {
			if count == 3 && leaderID != "" {
				leaderFound = true
				t.Logf("Leader elected: %s", leaderID)
				break
			}
		}
		if leaderFound {
			break
		}
		// Log current state for debugging
		for i, r := range rooms {
			t.Logf("Node %d: leader=%s, members=%d, state=%v", i, r.Leader(), len(r.Members()), r.State())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !leaderFound {
		t.Fatal("Leader election failed or inconsistent across nodes")
	}

	// Verify all nodes see same leader
	leaderID := rooms[0].Leader()
	for i, r := range rooms {
		if r.Leader() != leaderID {
			t.Errorf("Node %d has different leader: %s vs %s", i, r.Leader(), leaderID)
		}
	}

	// Test member count
	for i, r := range rooms {
		members := r.Members()
		if len(members) != 3 {
			t.Errorf("Node %d: expected 3 members, got %d", i, len(members))
		}
	}

	// Test room state
	for i, r := range rooms {
		if r.State() != group.RoomStateActive {
			t.Errorf("Node %d: expected Active state, got %v", i, r.State())
		}
	}

	// Test GroupStreamer
	testGroupStreamer(t, rooms)

	// Cleanup rooms
	for _, r := range rooms {
		r.Leave()
	}
	time.Sleep(500 * time.Millisecond)
}

func testGroupStreamer(t *testing.T, rooms []*group.Room) {
	var streamers []*group.GroupStreamer

	for _, r := range rooms {
		in, err := audio.OpenInput(0, 44100, 1, true)
		if err != nil {
			t.Fatalf("OpenInput error: %v", err)
		}
		defer in.Close()

		out, err := audio.OpenOutput(1, 44100, 1, true)
		if err != nil {
			t.Fatalf("OpenOutput error: %v", err)
		}
		defer out.Close()

		gs, err := group.NewGroupStreamer(group.GroupStreamerConfig{
			Room:         r,
			Input:        in,
			Output:       out,
			StartLatency: 100 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewGroupStreamer error: %v", err)
		}
		streamers = append(streamers, gs)
	}

	// Start all streamers
	for _, gs := range streamers {
		if err := gs.Start(); err != nil {
			t.Fatalf("GroupStreamer Start error: %v", err)
		}
	}

	// Let audio flow
	time.Sleep(1 * time.Second)

	// Check all are playing
	for i, gs := range streamers {
		if gs.State() != group.GroupPlaybackPlaying {
			t.Errorf("Streamer %d: expected Playing, got %v", i, gs.State())
		}
		stats := gs.Stats()
		if stats.MemberCount != 2 { // 2 remote members per node
			t.Errorf("Streamer %d: expected 2 members, got %d", i, stats.MemberCount)
		}
	}

	// Stop all
	for _, gs := range streamers {
		gs.Stop()
	}
}

// TestDTLSEncryption tests DTLS/SRTP encryption path
func TestDTLSEncryption(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	// Generate certs
	cfg := &config.Config{
		CertFile: certFile,
		KeyFile:  keyFile,
	}
	if err := ensureTLS(cfg); err != nil {
		t.Fatalf("TLS setup: %v", err)
	}

	// Node A (server)
	portA := freeUDPPort(t)
	cfgA := &config.Config{
		ListenAddr:     "127.0.0.1:0",
		CertFile:       certFile,
		KeyFile:        keyFile,
		AuthToken:      "dtls-test-token",
		UseEmulator:    true,
		StreamPort:     "127.0.0.1:" + itoa(portA),
		Encrypt:        true,
		MulticastGroup: "239.255.0.1",
		MulticastPort:  47100,
		DiscoveryPort:  freeUDPPort(t),
		SyncPort:       freeUDPPort(t),
	}

	// Node B (client)
	portB := freeUDPPort(t)
	cfgB := &config.Config{
		ListenAddr:     "127.0.0.1:0",
		CertFile:       certFile,
		KeyFile:        keyFile,
		AuthToken:      "dtls-test-token",
		UseEmulator:    true,
		StreamPort:     "127.0.0.1:" + itoa(portB),
		Encrypt:        true,
		MulticastGroup: "239.255.0.1",
		MulticastPort:  47100,
		DiscoveryPort:  freeUDPPort(t),
		SyncPort:       freeUDPPort(t),
	}

	// Setup transports
	dtlsA, err := transport.NewDTLSTransport(transport.DTLSConfig{
		ListenAddr: cfgA.StreamPort,
		CertFile:   certFile,
		KeyFile:    keyFile,
		IsServer:   true,
	})
	if err != nil {
		t.Fatalf("DTLS server: %v", err)
	}
	defer dtlsA.Close()

	dtlsB, err := transport.NewDTLSTransport(transport.DTLSConfig{
		ListenAddr: cfgB.StreamPort,
		RemoteAddr: cfgA.StreamPort, // connect to node A (server)
		CertFile:   certFile,
		KeyFile:    keyFile,
		IsServer:   false,
	})
	if err != nil {
		t.Fatalf("DTLS client: %v", err)
	}
	defer dtlsB.Close()

	// Test handshake by doing a read/write
	// Do a write to trigger handshake
	testPayload := []byte("handshake-test")
	if err := dtlsA.Send(dtlsA.RemoteAddr().String(), testPayload); err != nil {
		t.Fatalf("DTLS A handshake: %v", err)
	}

	// Give time for handshake
	time.Sleep(500 * time.Millisecond)

	// Verify SRTP profile negotiated
	profile, ok := dtlsA.SelectedSRTPProfile()
	if !ok {
		t.Fatal("SRTP profile not negotiated on A")
	}
	t.Logf("Node A SRTP profile: %v", profile)

	profile, ok = dtlsB.SelectedSRTPProfile()
	if !ok {
		t.Fatal("SRTP profile not negotiated on B")
	}
	t.Logf("Node B SRTP profile: %v", profile)

	// Test encrypted RTP packet
	header := &rtp.Header{
		Version:        2,
		SequenceNumber: 1,
		Timestamp:      0,
		SSRC:           0x12345678,
	}
	payload := make([]byte, 160) // 20ms @ 44.1kHz mono
	rand.Read(payload)

	pkt := &rtp.Packet{Header: *header, Payload: payload}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal RTP packet: %v", err)
	}

	dst := make([]byte, len(raw)+20)
	encrypted, err := dtlsA.EncryptRTP(dst, raw, header)
	if err != nil {
		t.Fatalf("EncryptRTP: %v", err)
	}
	t.Logf("Encrypted packet size: %d (plaintext: %d)", len(encrypted), len(raw))

	// Decrypt
	decrypted, err := dtlsB.DecryptRTP(dst, encrypted, header)
	if err != nil {
		t.Fatalf("DecryptRTP: %v", err)
	}
	if !bytes.Equal(decrypted, raw) {
		t.Fatal("Decrypted packet mismatch")
	}
	t.Log("DTLS/SRTP encryption round-trip successful")
}

// TestRoutePeerBinding tests route-to-peer binding
func TestRoutePeerBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")
	routesFile := filepath.Join(tmpDir, "routes.json")

	nodeA := setupNode(t, tmpDir, certFile, keyFile, 0)
	defer nodeA.cleanup()
	nodeB := setupNode(t, tmpDir, certFile, keyFile, 1)
	defer nodeB.cleanup()

	nodeA.start()
	nodeB.start()
	time.Sleep(2 * time.Second)

	// Create route on A bound to B
	rt, err := router.New(routesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	route := router.StreamRoute{
		ID:           "bound-route",
		SourceDevice: 0,
		Destinations: []string{nodeB.udp.Addr()},
		SampleRate:   44100,
		Channels:     1,
		PeerID:       nodeB.disc.NodeID(),
	}
	rt.Add(route)

	// Verify route has peer binding
	savedRoute, _ := rt.Get("bound-route")
	if savedRoute.PeerID != nodeB.disc.NodeID() {
		t.Errorf("Route PeerID not saved: %s", savedRoute.PeerID)
	}

	// Test stream manager binds to peer
	in, _ := audio.OpenInput(0, 44100, 1, true)
	defer in.Close()

	streamCfg := streamer.DefaultStreamConfig(44100, 1)
	st, _ := streamer.NewStreamer(nodeA.udp, nodeA.localIPs, streamCfg, nodeA.disc.NodeID())
	defer st.Stop()
	_ = st.Wait()
}

// TestAutoReconnect tests automatic reconnection after network disruption
func TestAutoReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	nodeA := setupNode(t, tmpDir, certFile, keyFile, 0)
	defer nodeA.cleanup()
	nodeB := setupNode(t, tmpDir, certFile, keyFile, 1)
	defer nodeB.cleanup()

	nodeA.start()
	nodeB.start()
	time.Sleep(2 * time.Second)

	// Verify initial connection
	peersA := nodeA.disc.Peers()
	peersB := nodeB.disc.Peers()
	if len(peersA) == 0 || len(peersB) == 0 {
		t.Fatal("Initial discovery failed")
	}

	// Simulate network disruption by closing and reopening transport
	nodeB.udp.Close()
	time.Sleep(500 * time.Millisecond)

	// Reopen transport
	newUDP, err := transport.NewUDPTransport(nodeB.cfg.StreamPort)
	if err != nil {
		t.Fatalf("Reopen UDP: %v", err)
	}
	nodeB.udp = newUDP
	nodeB.trans = newUDP

	// Restart discovery with new transport
	nodeB.disc.Close()
	nodeB.disc, _ = discovery.New(nodeB.cfg, nodeB.disc.NodeID())
	go nodeB.disc.Run()

	// Wait for re-discovery
	deadline := time.Now().Add(10 * time.Second)
	reconnected := false
	for time.Now().Before(deadline) {
		peersA := nodeA.disc.Peers()
		peersB := nodeB.disc.Peers()
		if len(peersA) > 0 && len(peersB) > 0 {
			reconnected = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !reconnected {
		t.Fatal("Auto-reconnect failed: peers not rediscovered")
	}

	// Verify sync still works
	time.Sleep(1 * time.Second)
	offsetsA := nodeA.sync.Offsets()
	offsetsB := nodeB.sync.Offsets()
	if len(offsetsA) == 0 || len(offsetsB) == 0 {
		t.Fatal("Sync not re-established after reconnect")
	}
}

// TestHealthMonitoring tests health monitoring and failure detection
func TestHealthMonitoring(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	nodeA := setupNode(t, tmpDir, certFile, keyFile, 0)
	defer nodeA.cleanup()
	nodeB := setupNode(t, tmpDir, certFile, keyFile, 1)
	defer nodeB.cleanup()

	nodeA.start()
	nodeB.start()
	time.Sleep(3 * time.Second)

	// Start room with health monitoring
	roomID := group.RoomID("health-test")
	roomA := group.NewRoom(group.RoomConfig{
		ID:          roomID,
		Name:        "Health Test",
		LocalPeerID: nodeA.disc.NodeID(),
		Config:      nodeA.cfg,
		Discovery:   nodeA.disc,
		SyncManager: nodeA.sync,
		Transport:   nodeA.trans,
	})
	roomB := group.NewRoom(group.RoomConfig{
		ID:          roomID,
		Name:        "Health Test",
		LocalPeerID: nodeB.disc.NodeID(),
		Config:      nodeB.cfg,
		Discovery:   nodeB.disc,
		SyncManager: nodeB.sync,
		Transport:   nodeB.trans,
	})

	nodeBPeerID := nodeB.disc.NodeID()

	// Use separate channels per room: when node B's sockets are closed, room B
	// also stops hearing node A and fires its own leave event. Mixing both
	// rooms onto one channel makes the state assertion below race on whose
	// leave event was consumed first.
	eventsA := make(chan string, 10)
	eventsB := make(chan string, 10)
	roomA.SetCallbacks(
		func(m *group.RoomMember) { eventsA <- "join:" + m.PeerID },
		func(m *group.RoomMember) { eventsA <- "leave:" + m.PeerID },
		func(leader string) { eventsA <- "leader:" + leader },
		func(s group.RoomState) { eventsA <- "state:" + roomStateString(s) },
	)
	roomB.SetCallbacks(
		func(m *group.RoomMember) { eventsB <- "join:" + m.PeerID },
		func(m *group.RoomMember) { eventsB <- "leave:" + m.PeerID },
		func(leader string) { eventsB <- "leader:" + leader },
		func(s group.RoomState) { eventsB <- "state:" + roomStateString(s) },
	)

	roomA.Join()
	roomB.Join()
	time.Sleep(5 * time.Second)

	// Verify both rooms detected the peer join (one join event per room)
	for i := 0; i < 2; i++ {
		var events <-chan string
		if i == 0 {
			events = eventsA
		} else {
			events = eventsB
		}
		select {
		case e := <-events:
			if !strings.HasPrefix(e, "join:") {
				t.Fatalf("Expected join event on room %d, got %q", i, e)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Join event timeout on room %d", i)
		}
	}

	// Simulate node B failure (close transport)
	nodeB.udp.Close()
	nodeB.disc.Close()
	nodeB.sync.Close()

	// Wait for room A to detect node B's leave specifically; room B's own
	// leave events are irrelevant here.
	leaveDetected := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case e := <-eventsA:
			if e == "leave:"+nodeBPeerID {
				leaveDetected = true
			}
		case e := <-eventsB:
			_ = e // drain, room B events don't drive this assertion
		case <-time.After(500 * time.Millisecond):
		}
		if leaveDetected {
			break
		}
	}
	if !leaveDetected {
		t.Fatal("Health monitoring failed to detect peer leave")
	}

	// Verify room state updated
	if roomA.State() != group.RoomStateElection {
		t.Errorf("Expected Election state after leader leave, got %v", roomA.State())
	}

	roomA.Leave()
	roomB.Leave()
}

// TestMetricsAPI tests Prometheus metrics endpoint
func TestMetricsAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	node := setupNode(t, tmpDir, certFile, keyFile, 0)
	defer node.cleanup()
	node.start()
	time.Sleep(1 * time.Second)

	// First make a request to /status to trigger the metrics counter
	apiPort := extractPort(node.cfg.ListenAddr)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	statusURL := "https://127.0.0.1:" + apiPort + "/status"
	req, _ := http.NewRequest("GET", statusURL, nil)
	req.Header.Set("Authorization", "Bearer integration-test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Status request failed: %v", err)
	}
	resp.Body.Close()

	// Query metrics endpoint
	url := "https://127.0.0.1:" + apiPort + "/metrics"
	req, _ = http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer integration-test-token")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Metrics request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Metrics endpoint returned %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	metrics := string(body)
	t.Logf("Metrics output: %s", metrics)

	// Check for expected metrics
	expectedMetrics := []string{
		"audio_router_api_requests_total",
		"audio_router_peers",
		"audio_router_active_routes",
	}
	for _, m := range expectedMetrics {
		if !strings.Contains(metrics, m) {
			t.Errorf("Missing metric: %s", m)
		}
	}
}

// TestConcurrentRooms tests multiple concurrent rooms
func TestConcurrentRooms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	nodeA := setupNode(t, tmpDir, certFile, keyFile, 0)
	defer nodeA.cleanup()
	nodeB := setupNode(t, tmpDir, certFile, keyFile, 1)
	defer nodeB.cleanup()
	nodeC := setupNode(t, tmpDir, certFile, keyFile, 2)
	defer nodeC.cleanup()

	nodeA.start()
	nodeB.start()
	nodeC.start()
	time.Sleep(2 * time.Second)

	// Create two different rooms
	room1 := group.RoomID("room-1")
	room2 := group.RoomID("room-2")

	roomsA := []*group.Room{
		group.NewRoom(group.RoomConfig{ID: room1, Name: "Room 1", LocalPeerID: nodeA.disc.NodeID(), Config: nodeA.cfg, Discovery: nodeA.disc, SyncManager: nodeA.sync, Transport: nodeA.trans}),
		group.NewRoom(group.RoomConfig{ID: room2, Name: "Room 2", LocalPeerID: nodeA.disc.NodeID(), Config: nodeA.cfg, Discovery: nodeA.disc, SyncManager: nodeA.sync, Transport: nodeA.trans}),
	}
	roomsB := []*group.Room{
		group.NewRoom(group.RoomConfig{ID: room1, Name: "Room 1", LocalPeerID: nodeB.disc.NodeID(), Config: nodeB.cfg, Discovery: nodeB.disc, SyncManager: nodeB.sync, Transport: nodeB.trans}),
		group.NewRoom(group.RoomConfig{ID: room2, Name: "Room 2", LocalPeerID: nodeB.disc.NodeID(), Config: nodeB.cfg, Discovery: nodeB.disc, SyncManager: nodeB.sync, Transport: nodeB.trans}),
	}
	roomsC := []*group.Room{
		group.NewRoom(group.RoomConfig{ID: room1, Name: "Room 1", LocalPeerID: nodeC.disc.NodeID(), Config: nodeC.cfg, Discovery: nodeC.disc, SyncManager: nodeC.sync, Transport: nodeC.trans}),
		group.NewRoom(group.RoomConfig{ID: room2, Name: "Room 2", LocalPeerID: nodeC.disc.NodeID(), Config: nodeC.cfg, Discovery: nodeC.disc, SyncManager: nodeC.sync, Transport: nodeC.trans}),
	}

	for _, r := range roomsA {
		r.Join()
	}
	for _, r := range roomsB {
		r.Join()
	}
	for _, r := range roomsC {
		r.Join()
	}
	time.Sleep(5 * time.Second)

	// Verify each room has 3 members and a leader
	allRooms := append(append(roomsA, roomsB...), roomsC...)
	for _, r := range allRooms {
		if len(r.Members()) != 3 {
			t.Errorf("Room %s: expected 3 members, got %d", r.ID(), len(r.Members()))
		}
		if r.Leader() == "" {
			t.Errorf("Room %s: no leader elected", r.ID())
		}
		if r.State() != group.RoomStateActive {
			t.Errorf("Room %s: expected Active, got %v", r.ID(), r.State())
		}
	}

	// Leave all rooms
	for _, r := range allRooms {
		r.Leave()
	}
}

// TestNodeFailureRecovery tests full node failure and recovery
func TestNodeFailureRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")
	routesFile := filepath.Join(tmpDir, "routes.json")

	// Start 3 nodes
	nodes := make([]*nodeSetup, 3)
	for i := 0; i < 3; i++ {
		nodes[i] = setupNode(t, tmpDir, certFile, keyFile, i)
		defer nodes[i].cleanup()
		nodes[i].start()
	}
	time.Sleep(2 * time.Second)

	// Create route on all nodes
	for _, n := range nodes {
		rt, err := router.New(routesFile)
		if err != nil {
			t.Fatalf("router.New: %v", err)
		}
		rt.Add(router.StreamRoute{
			ID:           "recovery-route",
			SourceDevice: 0,
			Destinations: []string{n.udp.Addr()},
			SampleRate:   44100,
			Channels:     1,
		})
	}

	// Start streaming on all nodes
	for _, n := range nodes {
		in, _ := audio.OpenInput(0, 44100, 1, true)
		out, _ := audio.OpenOutput(1, 44100, 1, true)
		streamCfg := streamer.DefaultStreamConfig(44100, 1)
		st, _ := streamer.NewStreamer(n.udp, n.localIPs, streamCfg, n.disc.NodeID())
		st.Start(in, []string{n.udp.Addr()}, out)
		defer st.Stop()
		defer in.Close()
		defer out.Close()
	}

	time.Sleep(1 * time.Second)

	// Kill middle node (node 1)
	nodes[1].udp.Close()
	nodes[1].disc.Close()
	nodes[1].sync.Close()

	// Wait for remaining nodes to detect failure
	time.Sleep(5 * time.Second)

	// Verify remaining nodes still operational
	peers0 := nodes[0].disc.Peers()
	peers2 := nodes[2].disc.Peers()
	if len(peers0) == 0 || len(peers2) == 0 {
		t.Fatal("Remaining nodes lost all peers")
	}

	// Restart middle node
	nodes[1] = setupNode(t, tmpDir, certFile, keyFile, 1)
	defer nodes[1].cleanup()
	nodes[1].start()

	// Wait for re-integration
	deadline := time.Now().Add(15 * time.Second)
	reintegrated := false
	for time.Now().Before(deadline) {
		peers0 := nodes[0].disc.Peers()
		peers1 := nodes[1].disc.Peers()
		peers2 := nodes[2].disc.Peers()
		if len(peers0) >= 2 && len(peers1) >= 2 && len(peers2) >= 2 {
			reintegrated = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !reintegrated {
		t.Fatal("Node reintegration failed")
	}
}

type nodeSetup struct {
	idx      int
	cfg      *config.Config
	udp      *transport.UDPTransport
	trans    streamer.Transport
	disc     *discovery.Discovery
	sync     *syncmgr.Manager
	apiSrv   *http.Server
	localIPs []string
}

func setupNode(t *testing.T, tmpDir, certFile, keyFile string, idx int) *nodeSetup {
	// Use fixed ports to avoid conflicts
	// All nodes must share the same multicast port for discovery
	apiPort := 58000 + idx*10
	discoveryPort := 48000 + idx*10
	syncPort := 49000 + idx*10
	multicastPort := 47200 // Same for all nodes

	cfg := &config.Config{
		ListenAddr:       fmt.Sprintf("127.0.0.1:%d", apiPort),
		CertFile:         certFile,
		KeyFile:          keyFile,
		AuthToken:        "integration-test-token",
		UseEmulator:      true,
		MulticastGroup:   "239.255.0.1",
		MulticastPort:    multicastPort,
		DiscoveryPort:    discoveryPort,
		SyncPort:         syncPort,
		StreamPort:       "127.0.0.1:0", // Use port 0 for auto-assignment
		AdvHost:          "127.0.0.1",
		AnnounceInterval: 200 * time.Millisecond,
		PeerTTL:          30 * time.Second,
		SyncInterval:     300 * time.Millisecond,
		ShutdownTimeout:  10 * time.Second,
	}

	udp, err := transport.NewUDPTransport(cfg.StreamPort)
	if err != nil {
		t.Fatalf("UDP transport: %v", err)
	}
	// Update config with actual assigned port
	if la := udp.LocalAddr(); la != nil {
		cfg.StreamPort = la.String()
	}
	if err != nil {
		t.Fatalf("UDP transport: %v", err)
	}
	localIPs := collectLocalIPs(udp, nil)

	// Ensure TLS certs exist
	if err := ensureTLS(cfg); err != nil {
		t.Fatalf("TLS: %v", err)
	}

	return &nodeSetup{
		idx:      idx,
		cfg:      cfg,
		udp:      udp,
		trans:    udp,
		disc:     nil,
		sync:     nil,
		apiSrv:   nil,
		localIPs: localIPs,
	}
}

func (n *nodeSetup) start() {
	n.disc, _ = discovery.New(n.cfg, "")
	go n.disc.Run()

	n.sync = syncmgr.NewManager(n.cfg, n.disc)
	go n.sync.Run()

	// Wait a bit for discovery to start
	time.Sleep(100 * time.Millisecond)

	// Start HTTP API server
	rt, err := router.New(filepath.Join(os.TempDir(), fmt.Sprintf("routes_%d.json", n.idx)))
	if err != nil {
		panic(fmt.Sprintf("router.New: %v", err))
	}
	apiSrv := api.NewServer(n.cfg, rt, n.disc, n.sync, nil)
	srv := &http.Server{Addr: n.cfg.ListenAddr, Handler: apiSrv.Handler()}
	n.apiSrv = srv
	go func() {
		srv.ListenAndServeTLS(n.cfg.CertFile, n.cfg.KeyFile)
	}()
	time.Sleep(200 * time.Millisecond) // wait for server to start
}

func (n *nodeSetup) cleanup() {
	if n.apiSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n.apiSrv.Shutdown(ctx)
	}
	if n.disc != nil {
		n.disc.Close()
	}
	if n.sync != nil {
		n.sync.Close()
	}
	if n.udp != nil {
		n.udp.Close()
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("allocate udp port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func collectLocalIPs(udp *transport.UDPTransport, disc *discovery.Discovery) []string {
	set := make(map[string]bool)
	if la := udp.LocalAddr(); la != nil {
		set[la.IP.String()] = true
	}
	if disc != nil {
		if own := disc.OwnAddr(); own != "" {
			if host, _, err := net.SplitHostPort(own); err == nil && host != "" {
				set[host] = true
			}
		}
	}
	set["127.0.0.1"] = true
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				set[ipn.IP.String()] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	return out
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func extractPort(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

func ensureTLS(cfg *config.Config) error {
	if fileExists(cfg.CertFile) && fileExists(cfg.KeyFile) {
		return nil
	}
	if fileExists(cfg.CertFile) != fileExists(cfg.KeyFile) {
		return fmt.Errorf("only one of cert/key exists")
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "audio-router-p2p", Organization: []string{"audio-router-p2p"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(filepath.Dir(cfg.CertFile), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.CertFile, certPEM, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.KeyFile, keyPEM, 0600); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func roomStateString(s group.RoomState) string {
	switch s {
	case group.RoomStateCreated:
		return "Created"
	case group.RoomStateElection:
		return "Election"
	case group.RoomStateActive:
		return "Active"
	case group.RoomStateClosing:
		return "Closing"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}
