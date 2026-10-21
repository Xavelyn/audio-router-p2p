package group

import (
	"bytes"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("allocate UDP port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestGroupStreamerNilRoom(t *testing.T) {
	_, err := NewGroupStreamer(GroupStreamerConfig{})
	if err == nil {
		t.Fatal("expected error on nil room")
	}
}

func TestGroupStreamerStreaming(t *testing.T) {
	port1 := freeUDPPort(t)
	port2 := freeUDPPort(t)
	addr1 := net.JoinHostPort("127.0.0.1", strconv.Itoa(port1))
	addr2 := net.JoinHostPort("127.0.0.1", strconv.Itoa(port2))

	trans1, err := transport.NewUDPTransport(addr1)
	if err != nil {
		t.Fatalf("trans1: %v", err)
	}
	defer trans1.Close()

	trans2, err := transport.NewUDPTransport(addr2)
	if err != nil {
		t.Fatalf("trans2: %v", err)
	}
	defer trans2.Close()

	roomID := RoomID("group-audio-room")

	// Node 1 is leader because "node-1" < "node-2"
	room1 := NewRoom(RoomConfig{
		ID:          roomID,
		LocalPeerID: "node-1",
		Transport:   trans1,
	})
	room1.addMember("node-1", &RoomMember{PeerID: "node-1", StreamAddr: addr1})
	room1.addMember("node-2", &RoomMember{PeerID: "node-2", StreamAddr: addr2})
	room1.recomputeLeader()

	room2 := NewRoom(RoomConfig{
		ID:          roomID,
		LocalPeerID: "node-2",
		Transport:   trans2,
	})
	room2.addMember("node-1", &RoomMember{PeerID: "node-1", StreamAddr: addr1})
	room2.addMember("node-2", &RoomMember{PeerID: "node-2", StreamAddr: addr2})
	room2.recomputeLeader()

	if !room1.IsLeader() {
		t.Fatalf("expected room1 to be leader")
	}
	if room2.IsLeader() {
		t.Fatalf("expected room2 to be follower")
	}

	devIn := audio.NewVirtualAudioDevice()
	defer devIn.Close()

	devOut := audio.NewVirtualAudioDevice()
	defer devOut.Close()

	// Leader with input
	gsLeader, err := NewGroupStreamer(GroupStreamerConfig{
		Room:         room1,
		Input:        devIn,
		SampleRate:   48000,
		Channels:     2,
		StartLatency: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGroupStreamer leader: %v", err)
	}

	// Follower with output
	gsFollower, err := NewGroupStreamer(GroupStreamerConfig{
		Room:         room2,
		Output:       devOut,
		SampleRate:   48000,
		Channels:     2,
		StartLatency: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGroupStreamer follower: %v", err)
	}

	if err := gsFollower.Start(); err != nil {
		t.Fatalf("start follower: %v", err)
	}
	defer gsFollower.Stop()

	if err := gsLeader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	defer gsLeader.Stop()

	if gsLeader.State() != GroupPlaybackPlaying {
		t.Fatalf("expected leader state playing, got %v", gsLeader.State())
	}
	if gsFollower.State() != GroupPlaybackPlaying {
		t.Fatalf("expected follower state playing, got %v", gsFollower.State())
	}

	// Inject 40 packets of test audio into devIn
	packetBytes := (48000 * 20 / 1000) * 2 * 2 // 3840 bytes
	testData := bytes.Repeat([]byte{0x7F, 0x12, 0x34, 0x56}, packetBytes/4)

	go func() {
		for i := 0; i < 40; i++ {
			if _, err := devIn.InjectPush(testData); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Wait for audio to arrive at devOut
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if devOut.Buffered() >= packetBytes*5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	captured := devOut.CapturedBytes()
	if len(captured) < packetBytes*5 {
		t.Fatalf("expected at least %d captured bytes, got %d", packetBytes*5, len(captured))
	}

	// Check that non-zero audio was received and contains test sequence
	snippet := testData[:32]
	if !bytes.Contains(captured, snippet) {
		t.Fatal("follower output did not receive injected leader audio")
	}

	statsLeader := gsLeader.Stats()
	if statsLeader.PacketsSent == 0 {
		t.Fatalf("expected leader PacketsSent > 0, got %d", statsLeader.PacketsSent)
	}
	if statsLeader.MemberCount != 1 {
		t.Fatalf("expected MemberCount 1, got %d", statsLeader.MemberCount)
	}
}
