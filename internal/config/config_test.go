package config

import (
	"testing"
	"time"
)

func TestLoadFromArgs_Valid(t *testing.T) {
	args := []string{
		"-auth-token", "my-secret-token",
		"-listen", "127.0.0.1:9000",
		"-use-emulator",
		"-encrypt",
		"-announce-interval", "2s",
		"-peer-ttl", "8s",
		"-sync-interval", "1s",
	}

	cfg, err := LoadFromArgs(args)
	if err != nil {
		t.Fatalf("LoadFromArgs failed: %v", err)
	}

	if cfg.AuthToken != "my-secret-token" {
		t.Errorf("expected AuthToken 'my-secret-token', got '%s'", cfg.AuthToken)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("expected ListenAddr '127.0.0.1:9000', got '%s'", cfg.ListenAddr)
	}
	if !cfg.UseEmulator {
		t.Errorf("expected UseEmulator true")
	}
	if !cfg.Encrypt {
		t.Errorf("expected Encrypt true")
	}
	if cfg.AnnounceInterval != 2*time.Second {
		t.Errorf("expected AnnounceInterval 2s, got %v", cfg.AnnounceInterval)
	}
	if cfg.ElectionPort != 45403 {
		t.Errorf("expected ElectionPort default 45403, got %d", cfg.ElectionPort)
	}
}

func TestLoadFromArgs_RoomFlags(t *testing.T) {
	args := []string{
		"-auth-token", "tok",
		"-room-id", "my-test-room",
		"-room-name", "Conference Hall",
		"-room-auto-join",
	}

	cfg, err := LoadFromArgs(args)
	if err != nil {
		t.Fatalf("LoadFromArgs failed: %v", err)
	}

	if cfg.RoomID != "my-test-room" {
		t.Errorf("expected RoomID 'my-test-room', got '%s'", cfg.RoomID)
	}
	if cfg.RoomName != "Conference Hall" {
		t.Errorf("expected RoomName 'Conference Hall', got '%s'", cfg.RoomName)
	}
	if !cfg.RoomAutoJoin {
		t.Errorf("expected RoomAutoJoin true")
	}
}

func TestLoadFromArgs_MissingAuthToken(t *testing.T) {
	args := []string{
		"-listen", ":8443",
	}

	_, err := LoadFromArgs(args)
	if err == nil {
		t.Fatal("expected error when auth-token is missing, got nil")
	}
}

func TestLoadFromArgs_InvalidIntervals(t *testing.T) {
	args := []string{
		"-auth-token", "tok",
		"-announce-interval", "0s",
	}

	_, err := LoadFromArgs(args)
	if err == nil {
		t.Fatal("expected error for non-positive announce-interval, got nil")
	}
}

func TestLoadFromArgs_StaticPeers(t *testing.T) {
	staticPeers := `[{"id":"peer1","addr":"10.0.0.1:45401","streamAddr":"10.0.0.1:45678","syncAddr":"10.0.0.1:45402"}]`
	args := []string{
		"-auth-token", "tok",
		"-static-peers", staticPeers,
	}

	cfg, err := LoadFromArgs(args)
	if err != nil {
		t.Fatalf("LoadFromArgs with static-peers failed: %v", err)
	}

	if len(cfg.StaticPeers) != 1 {
		t.Fatalf("expected 1 static peer, got %d", len(cfg.StaticPeers))
	}
	if cfg.StaticPeers[0].ID != "peer1" {
		t.Errorf("expected static peer ID 'peer1', got '%s'", cfg.StaticPeers[0].ID)
	}
}

func TestLoadFromArgs_PeersFlag(t *testing.T) {
	args := []string{
		"-auth-token", "tok",
		"-peers", "10.1.10.160:45401, 10.1.10.201",
	}

	cfg, err := LoadFromArgs(args)
	if err != nil {
		t.Fatalf("LoadFromArgs with -peers failed: %v", err)
	}

	if len(cfg.StaticPeers) != 2 {
		t.Fatalf("expected 2 static peers, got %d", len(cfg.StaticPeers))
	}
	if cfg.StaticPeers[0].Addr != "10.1.10.160:45401" {
		t.Errorf("expected peer 0 addr 10.1.10.160:45401, got '%s'", cfg.StaticPeers[0].Addr)
	}
	if cfg.StaticPeers[1].Addr != "10.1.10.201:45401" {
		t.Errorf("expected peer 1 addr 10.1.10.201:45401, got '%s'", cfg.StaticPeers[1].Addr)
	}
}
