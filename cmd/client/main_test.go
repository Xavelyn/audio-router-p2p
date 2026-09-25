package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
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
		UseEmulator:     true,
		ShutdownTimeout: 1 * time.Second,
		DiscoveryPort:   0,
		MulticastPort:   0,
		SyncPort:        0,
		AnnounceInterval: 1 * time.Second,
		PeerTTL:         3 * time.Second,
		SyncInterval:    1 * time.Second,
		AuthToken:       "test-tok",
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

	// Stop single route
	if err := sm.StopRoute("test-route-1"); err != nil {
		t.Errorf("StopRoute failed: %v", err)
	}

	// Start again and test StopAll
	if err := sm.StartRoute(route); err != nil {
		t.Fatalf("StartRoute 2 failed: %v", err)
	}
	sm.StopAll()
}
