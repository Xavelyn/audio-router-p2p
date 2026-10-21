package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/transport"
)

func TestDestIsLocal(t *testing.T) {
	localIPs := []string{"127.0.0.1", "192.168.1.100", "::1"}

	tests := []struct {
		hostport string
		expected bool
	}{
		{"127.0.0.1:45678", true},
		{"192.168.1.100:8000", true},
		{"[::1]:9000", true},
		{"10.0.0.1:45678", false},
		{"invalid-host", false},
	}

	for _, tt := range tests {
		got := destIsLocal(tt.hostport, localIPs)
		if got != tt.expected {
			t.Errorf("destIsLocal(%s) = %v; want %v", tt.hostport, got, tt.expected)
		}
	}
}

func TestContains(t *testing.T) {
	slice := []string{"apple", "banana", "cherry"}
	if !contains(slice, "banana") {
		t.Errorf("expected slice to contain 'banana'")
	}
	if contains(slice, "orange") {
		t.Errorf("did not expect slice to contain 'orange'")
	}
}

func TestEnsureTLS(t *testing.T) {
	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")

	cfg := &config.Config{
		CertFile: certFile,
		KeyFile:  keyFile,
	}

	// Should generate cert and key
	if err := ensureTLS(cfg); err != nil {
		t.Fatalf("ensureTLS failed: %v", err)
	}

	if !fileExists(certFile) || !fileExists(keyFile) {
		t.Fatal("expected cert and key files to be created")
	}

	// Second invocation should be a no-op
	if err := ensureTLS(cfg); err != nil {
		t.Fatalf("ensureTLS second run failed: %v", err)
	}

	// If only one exists, should return error
	_ = os.Remove(keyFile)
	if err := ensureTLS(cfg); err == nil {
		t.Fatal("expected error when only cert exists, got nil")
	}
}

func TestStreamManager(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}
	defer trans.Close()

	cfg := &config.Config{
		UseEmulator:      true,
		ShutdownTimeout:  1 * time.Second,
		DiscoveryPort:    0,
		MulticastPort:    0,
		SyncPort:         0,
		AnnounceInterval: 1 * time.Second,
		PeerTTL:          3 * time.Second,
		SyncInterval:     1 * time.Second,
		AuthToken:        "test-tok",
	}

	disc, err := discovery.New(cfg, "test-node-sm")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}

	localIPs := []string{"127.0.0.1"}
	sm := newStreamManager(cfg, trans, localIPs, disc)

	route := router.StreamRoute{
		ID:           "test-route-1",
		SourceDevice: -1,
		Destinations: []string{"127.0.0.1:45678"},
		SampleRate:   44100,
		Channels:     2,
	}

	if err := sm.StartRoute(route); err != nil {
		t.Fatalf("StartRoute failed: %v", err)
	}

	stats := sm.Stats()
	if _, ok := stats["test-route-1"]; !ok {
		t.Errorf("expected stats for test-route-1")
	}
	wantID := streamer.DeriveStreamID(cfg.AuthToken, "route:"+route.ID)
	if got := sm.routes[route.ID].st.StreamID(); got != wantID {
		t.Fatalf("route stream ID = %x, want %x", got, wantID)
	}

	// Stop single route
	if err := sm.StopRoute("test-route-1"); err != nil {
		t.Errorf("StopRoute failed: %v", err)
	}

	// Start again and test StopAll
	if err := sm.StartRoute(route); err != nil {
		t.Fatalf("StartRoute 2 failed: %v", err)
	}
	if got := sm.routes[route.ID].st.StreamID(); got != wantID {
		t.Fatalf("restarted route stream ID = %x, want %x", got, wantID)
	}
	sm.StopAll()
}

func TestStreamManagersSendAndReceiveRoute(t *testing.T) {
	senderTransport, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer senderTransport.Close()
	receiverTransport, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer receiverTransport.Close()

	cfg := &config.Config{
		AuthToken: "shared-test-token", UseEmulator: true,
		AnnounceInterval: time.Second, PeerTTL: 3 * time.Second, SyncInterval: time.Second,
	}
	senderDisc, err := discovery.New(cfg, "sender")
	if err != nil {
		t.Fatal(err)
	}
	defer senderDisc.Close()
	receiverDisc, err := discovery.New(cfg, "receiver")
	if err != nil {
		t.Fatal(err)
	}
	defer receiverDisc.Close()
	localIPs := []string{"127.0.0.1"}
	sender := newStreamManager(cfg, senderTransport, localIPs, senderDisc)
	receiver := newStreamManager(cfg, receiverTransport, localIPs, receiverDisc)
	// Only the receiver has an output; it listens before capture starts.
	if err := receiver.StartRoute(router.StreamRoute{
		ID: "shared-route", Direction: "receive", DestDevice: 1,
		SampleRate: 44100, Channels: 1,
	}); err != nil {
		t.Fatal(err)
	}
	defer receiver.StopAll()
	if receiver.routes["shared-route"].in != nil || receiver.routes["shared-route"].out == nil {
		t.Fatal("receive route must open output only")
	}
	if err := sender.StartRoute(router.StreamRoute{
		ID: "shared-route", SourceDevice: 0,
		Destinations: []string{receiverTransport.Addr()},
		SampleRate:   44100, Channels: 1,
	}); err != nil {
		t.Fatal(err)
	}
	defer sender.StopAll()
	if sender.routes["shared-route"].out != nil {
		t.Fatal("remote send route unexpectedly opened local output")
	}
	if sender.routes["shared-route"].st.StreamID() != receiver.routes["shared-route"].st.StreamID() {
		t.Fatal("send and receive routes do not share a stream ID")
	}

	deadline := time.Now().Add(3 * time.Second)
	for audio.OutputBufferSize(receiver.routes["shared-route"].out) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := audio.OutputBufferSize(receiver.routes["shared-route"].out); got == 0 {
		t.Fatal("receiver did not play network audio")
	}
	if got := receiver.Stats()["shared-route"].PacketsReceived; got == 0 {
		t.Fatal("receiver did not count any packets")
	}
}
