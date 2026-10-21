//go:build noaudio

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestRouteProcessHelper runs only when launched by TestTwoProcessAudioRoute.
// Each subprocess has its own UDP socket, discovery identity and audio streams.
func TestRouteProcessHelper(t *testing.T) {
	role := os.Getenv("AUDIO_ROUTE_TEST_ROLE")
	if role == "" {
		return
	}
	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer trans.Close()
	cfg := &config.Config{
		AuthToken: "two-process-secret", UseEmulator: true,
		AnnounceInterval: time.Second, PeerTTL: 3 * time.Second, SyncInterval: time.Second,
	}
	disc, err := discovery.New(cfg, role)
	if err != nil {
		t.Fatal(err)
	}
	defer disc.Close()
	m := newStreamManager(cfg, trans, []string{"127.0.0.1"}, disc)
	defer m.StopAll()

	// 48 kHz stereo exercises the packet-size policy (5ms, not the old
	// fragmenting 20ms packet) across a real process boundary.
	route := router.StreamRoute{ID: "two-process-route", SampleRate: 48000, Channels: 2}
	if role == "receiver" {
		route.Direction = "receive"
		route.DestDevice = 1
	} else if role == "sender" {
		route.SourceDevice = 0
		route.Destinations = []string{os.Getenv("AUDIO_ROUTE_TEST_DEST")}
	} else {
		t.Fatalf("invalid test role %q", role)
	}
	if err := m.StartRoute(route); err != nil {
		t.Fatal(err)
	}
	if role == "receiver" {
		fmt.Println("READY " + trans.Addr())
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if audio.OutputBufferSize(m.routes[route.ID].matrix.device) > 0 && m.Stats()[route.ID].PacketsReceived > 0 {
				if captured, ok := m.routes[route.ID].matrix.device.(interface{ ReadAll() []byte }); ok {
					for _, sample := range captured.ReadAll() {
						if sample != 0 {
							fmt.Println("AUDIO")
							return
						}
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("receiver did not play non-silent UDP audio")
	}
	fmt.Println("SENDING")
	time.Sleep(3 * time.Second)
}

func TestTwoProcessAudioRoute(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(role, dest string) (*exec.Cmd, <-chan string) {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestRouteProcessHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "AUDIO_ROUTE_TEST_ROLE="+role, "AUDIO_ROUTE_TEST_DEST="+dest)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		lines := make(chan string, 32)
		go func() {
			defer close(lines)
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()
		return cmd, lines
	}
	waitFor := func(lines <-chan string, prefix string) string {
		t.Helper()
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		var output []string
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before %q: %s", prefix, strings.Join(output, "\n"))
				}
				output = append(output, line)
				if strings.HasPrefix(line, prefix) {
					return strings.TrimPrefix(line, prefix)
				}
			case <-deadline.C:
				t.Fatalf("timed out waiting for %q: %s", prefix, strings.Join(output, "\n"))
			}
		}
	}
	_, receiver := start("receiver", "")
	addr := waitFor(receiver, "READY ")
	_, sender := start("sender", addr)
	waitFor(sender, "SENDING")
	waitFor(receiver, "AUDIO")
}
