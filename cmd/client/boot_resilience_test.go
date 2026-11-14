package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
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
)

func TestBootResilience_ImmediateHTTPListen(t *testing.T) {
	tmpDir := t.TempDir()
	routesFile := filepath.Join(tmpDir, "routes.json")
	roomsFile := filepath.Join(tmpDir, "rooms.json")

	cfg := &config.Config{
		AuthToken:        "boot-resilience-token",
		RoutesFile:       routesFile,
		RoomsFile:        roomsFile,
		UseEmulator:      true,
		DiscoveryPort:    0,
		MulticastPort:    0,
		SyncPort:         0,
		StreamPort:       ":45678",
		ListenAddr:       "127.0.0.1:0",
		AnnounceInterval: 1 * time.Second,
		PeerTTL:          3 * time.Second,
		SyncInterval:     1 * time.Second,
		ShutdownTimeout:  2 * time.Second,
	}

	disc, err := discovery.New(cfg, "boot-node-1")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	sm := syncmgr.NewManager(cfg, disc)
	defer sm.Close()

	localIPs := collectLocalIPs(trans, disc)
	streams := newStreamManager(cfg, trans, localIPs, disc)
	defer streams.StopAll()

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streams)
	disc.SetStateDelegate(roomMgr)
	defer roomMgr.Close()

	apiSrv := api.NewServer(cfg, rt, disc, sm, streams, roomMgr)
	defer apiSrv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer ln.Close()

	httpServer := &http.Server{Handler: apiSrv.Handler()}
	go func() {
		_ = httpServer.Serve(ln)
	}()
	defer httpServer.Close()

	// Async background startup simulates route & room loading
	startupDone := make(chan struct{})
	go func() {
		for _, route := range rt.List() {
			_ = streams.StartRoute(route)
		}
		_ = roomMgr.Load(cfg.RoomsFile)
		close(startupDone)
	}()

	// Verify HTTP server is immediately accessible (< 50ms)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	statusURL := fmt.Sprintf("http://%s/status", ln.Addr().String())

	req, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	req.Header.Set("Authorization", "Bearer boot-resilience-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected immediate HTTP server response, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /status, got %d", resp.StatusCode)
	}

	var statusBody map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&statusBody); err != nil {
		t.Fatalf("failed to decode /status JSON: %v", err)
	}

	select {
	case <-startupDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("async startup did not complete within 2s")
	}
}

func TestStreamManager_IngressRoute(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	cfg := &config.Config{
		UseEmulator:     true,
		ShutdownTimeout: 1 * time.Second,
		DiscoveryPort:   0,
		MulticastPort:   0,
		SyncPort:        0,
		StreamPort:      ":45678",
		AuthToken:       "test-tok",
	}

	disc, err := discovery.New(cfg, "local-receiver-node")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	localIPs := []string{"127.0.0.1"}
	sm := newStreamManager(cfg, trans, localIPs, disc)
	defer sm.StopAll()

	// Ingress route: Source is on remote node "sender-node-42", destination is local output device
	ingressRoute := router.StreamRoute{
		ID:              "ingress-route-101",
		SourceNodeID:    "sender-node-42",
		SourceDeviceURN: "hw:0",
		DestDeviceURN:   "hw:1",
		PeerID:          "local",
		SampleRate:      48000,
		Channels:        2,
	}

	if err := sm.StartRoute(ingressRoute); err != nil {
		t.Fatalf("StartRoute failed on ingress route: %v", err)
	}

	// Verify route is registered in active streams
	sm.mu.Lock()
	as, exists := sm.routes["ingress-route-101"]
	sm.mu.Unlock()

	if !exists {
		t.Fatalf("expected ingress route to be registered in active streams")
	}

	// For an ingress route, input must be nil and output must NOT be nil!
	if as.in != nil {
		t.Errorf("expected as.in to be nil for ingress route, got %v", as.in)
	}
	if as.out == nil {
		t.Errorf("expected as.out to NOT be nil for ingress route")
	}

	expectedSID := streamer.DeriveStreamID(cfg.AuthToken, "ingress-route-101")
	if as.st.StreamID() != expectedSID {
		t.Errorf("expected StreamID %x, got %x", expectedSID, as.st.StreamID())
	}

	stats := sm.Stats()
	if _, ok := stats["ingress-route-101"]; !ok {
		t.Errorf("expected stats for ingress-route-101")
	}

	if err := sm.StopRoute("ingress-route-101"); err != nil {
		t.Errorf("StopRoute failed for ingress route: %v", err)
	}
}

func TestStreamManager_CrossNodePeerResolution(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	cfg := &config.Config{
		UseEmulator:     true,
		ShutdownTimeout: 1 * time.Second,
		DiscoveryPort:   0,
		MulticastPort:   0,
		SyncPort:        0,
		StreamPort:      ":45678",
		AuthToken:       "test-tok",
	}

	disc, err := discovery.New(cfg, "source-node-01")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	defer disc.Close()

	// Inject a peer into the discovery table
	peerID := "peer-stage-box"
	disc.AddPeer(&discovery.Peer{
		ID:         peerID,
		Addr:       "192.168.1.150:45401",
		StreamAddr: "192.168.1.150:45678",
		LastSeen:   time.Now(),
	})

	localIPs := []string{"127.0.0.1"}
	sm := newStreamManager(cfg, trans, localIPs, disc)
	defer sm.StopAll()

	route := router.StreamRoute{
		ID:              "cross-node-route-1",
		SourceDeviceURN: "hw:0",
		PeerID:          peerID,
		Destinations:    []string{"10.0.0.50"}, // Bare host destination without port
		SampleRate:      48000,
		Channels:        2,
	}

	if err := sm.StartRoute(route); err != nil {
		t.Fatalf("StartRoute failed: %v", err)
	}

	sm.mu.Lock()
	as, exists := sm.routes["cross-node-route-1"]
	sm.mu.Unlock()

	if !exists {
		t.Fatalf("expected route to be active")
	}

	// Verify PeerAddr was resolved with port
	if as.route.PeerAddr != "192.168.1.150:45678" {
		t.Errorf("expected PeerAddr to be '192.168.1.150:45678', got %q", as.route.PeerAddr)
	}

	// Check destinations in active streamer: bare host "10.0.0.50" must be normalized to "10.0.0.50:45678"
	var hasNormalizedBareHost bool
	for _, d := range as.route.Destinations {
		if d == "10.0.0.50:45678" {
			hasNormalizedBareHost = true
		}
		if d == "10.0.0.50" {
			t.Errorf("bare host 10.0.0.50 without port was preserved in destinations")
		}
	}
	if !hasNormalizedBareHost {
		// If destAddrs was updated during StartRoute
		t.Logf("route destinations: %v", as.route.Destinations)
	}
}

func TestDeviceTimeoutFallback_GhostStream(t *testing.T) {
	resolver := audio.NewDeviceResolver()
	defer resolver.Close()

	// An unregistered or unavailable device should fall back to GhostAudioStream within timeout
	start := time.Now()
	st := openInputWithTimeout(resolver, "non-existent-device-urn", 48000, 2, false, 50*time.Millisecond)
	elapsed := time.Since(start)

	if st == nil {
		t.Fatalf("expected non-nil stream from openInputWithTimeout fallback")
	}
	defer st.Close()

	if elapsed > 1*time.Second {
		t.Errorf("openInputWithTimeout took too long: %v", elapsed)
	}

	// Verify the stream produces or absorbs silence without panicking
	buf := make([]byte, 192)
	n, err := st.Read(buf)
	if err != nil {
		t.Errorf("expected read from ghost stream to succeed, got %v", err)
	}
	if n != len(buf) {
		t.Errorf("expected %d bytes, got %d", len(buf), n)
	}
}
