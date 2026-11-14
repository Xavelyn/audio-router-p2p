package streamer

import (
	"testing"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
)

func TestResolvePeerDestination(t *testing.T) {
	cfg, err := config.LoadFromArgs([]string{"-no-tls", "-auth-token", "testtoken"})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.StreamPort = ":45678"
	cfg.DiscoveryPort = 0
	disc, err := discovery.New(cfg, "node-local")
	if err != nil {
		t.Fatalf("discovery init: %v", err)
	}
	defer disc.Close()

	// 1. Route with valid PeerAddr already containing host:port
	sr1 := router.StreamRoute{
		ID:       "rt-1",
		PeerID:   "peer-1",
		PeerAddr: "192.168.1.50:45678",
	}
	addr, err := ResolvePeerDestination(sr1, disc, "45678")
	if err != nil || addr != "192.168.1.50:45678" {
		t.Fatalf("expected 192.168.1.50:45678, got %s (err: %v)", addr, err)
	}

	// 2. Local route
	srLocal := router.StreamRoute{
		ID:     "rt-local",
		PeerID: "local",
	}
	_, err = ResolvePeerDestination(srLocal, disc, "45678")
	if err == nil {
		t.Fatalf("expected error for local route")
	}

	// 3. Unresolved remote peer (not in discovery)
	srUnknown := router.StreamRoute{
		ID:     "rt-unknown",
		PeerID: "peer-unknown",
	}
	_, err = ResolvePeerDestination(srUnknown, disc, "45678")
	if err == nil {
		t.Fatalf("expected error for unknown peer")
	}

	// 4. Peer with Addr but no StreamAddr and empty defaultPort fallback
	disc.AddPeer(&discovery.Peer{
		ID:   "peer-nostreamport",
		Addr: "192.168.10.20:45401",
	})
	srFallback := router.StreamRoute{
		ID:     "rt-fallback",
		PeerID: "peer-nostreamport",
	}
	addr, err = ResolvePeerDestination(srFallback, disc, "")
	if err != nil {
		t.Fatalf("expected resolution with default port fallback, got: %v", err)
	}
	if addr != "192.168.10.20:45678" {
		t.Fatalf("expected 192.168.10.20:45678, got %s", addr)
	}
}
