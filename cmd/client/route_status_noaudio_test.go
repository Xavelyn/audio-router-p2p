//go:build noaudio

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestRouteStatusDiagnostics verifies the "why isn't this playing" status for
// the three interesting cases: a receive route with no source, a send route
// bound to an undiscovered peer, and a healthy local-output route.
func TestRouteStatusDiagnostics(t *testing.T) {
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer trans.Close()
	cfg := &config.Config{
		AuthToken: "status-test", UseEmulator: true,
		AnnounceInterval: time.Second, PeerTTL: 3 * time.Second, SyncInterval: time.Second,
	}
	disc, err := discovery.New(cfg, "status-node")
	if err != nil {
		t.Fatal(err)
	}
	defer disc.Close()
	m := newStreamManager(cfg, trans, []string{"127.0.0.1"}, disc)
	defer m.StopAll()

	// 1) Receive route with no source: not playing, with a useful reason.
	if err := m.StartRoute(router.StreamRoute{ID: "diag-recv", Direction: "receive",
		DestDevice: 0, SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	st, ok := m.RouteStatus("diag-recv")
	if !ok {
		t.Fatal("diag-recv not found")
	}
	if st.Playing || st.Direction != "receive" {
		t.Errorf("unexpected receive status: %+v", st)
	}
	if !containsReason(st.Reasons, "no audio received") {
		t.Errorf("receive reasons should mention missing audio: %+v", st.Reasons)
	}
	if st.Summary == "" {
		t.Error("summary must explain the state")
	}

	// 2) Send route bound to an undiscovered peer: no destination.
	if err := m.StartRoute(router.StreamRoute{ID: "diag-ghost", PeerID: "ghost-peer",
		SourceDevice: 0, SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	stGhost, ok := m.RouteStatus("diag-ghost")
	if !ok {
		t.Fatal("diag-ghost not found")
	}
	if stGhost.Playing {
		t.Errorf("route without a destination must not be playing: %+v", stGhost)
	}
	if !containsReason(stGhost.Reasons, "not discovered") {
		t.Errorf("ghost reasons should mention discovery: %+v", stGhost.Reasons)
	}
	if !containsReason(stGhost.Reasons, "no active send destination") {
		t.Errorf("ghost reasons should mention missing destination: %+v", stGhost.Reasons)
	}

	// 3) Local-output route: healthy and playing through the output bus.
	if err := m.StartRoute(router.StreamRoute{ID: "diag-local", SourceDevice: 0,
		Destinations: []string{trans.Addr()}, DestDevice: 1,
		SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	var stLocal v1.RouteStatus
	playing := false
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, ok := m.RouteStatus("diag-local")
		if ok && s.Playing {
			stLocal, playing = s, true
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local route did not start playing: %+v", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !playing || stLocal.Summary != "playing" {
		t.Errorf("local route should report as playing: %+v", stLocal)
	}

	// 4) Unknown route.
	if _, ok := m.RouteStatus("nope"); ok {
		t.Error("unknown route should not report status")
	}
}

func containsReason(reasons []string, fragment string) bool {
	for _, r := range reasons {
		if strings.Contains(r, fragment) {
			return true
		}
	}
	return false
}
