package group

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/transport"
)

func generateGroupSineWave(sampleRate, channels, numPackets, packetTimeMs int, freq float64) [][]byte {
	samplesPerPacket := (sampleRate * packetTimeMs) / 1000
	packets := make([][]byte, numPackets)

	t := 0.0
	step := 2.0 * math.Pi * freq / float64(sampleRate)

	for p := 0; p < numPackets; p++ {
		buf := make([]byte, samplesPerPacket*channels*2)
		for s := 0; s < samplesPerPacket; s++ {
			sampleVal := int16(25000.0 * math.Sin(t))
			t += step
			for c := 0; c < channels; c++ {
				offset := (s*channels + c) * 2
				binary.LittleEndian.PutUint16(buf[offset:], uint16(sampleVal))
			}
		}
		packets[p] = buf
	}
	return packets
}

// TestMultiNodeGroupAudioStreamingE2E validates deterministic room orchestration:
// 1. 3 nodes (Node A: node-01, Node B: node-02, Node C: node-03) with UDP transports and discovery.
// 2. Node A becomes leader deterministically (lowest PeerID).
// 3. Audio injected into Node A streams to Node B and Node C over UDP.
// 4. Followers Node B and Node C receive and play out audio.
// 5. Failover: Node A stops, Node B becomes leader deterministically.
func TestMultiNodeGroupAudioStreamingE2E(t *testing.T) {
	mcastPort := freeUDPPort(t)
	mcastGroup := "239.255.0.1"
	roomID := "e2e-group-room"

	setupNode := func(nodeID string) (
		*config.Config,
		*transport.UDPTransport,
		*discovery.Discovery,
		*audio.VirtualAudioDevice,
		*audio.VirtualAudioDevice,
		*DefaultRoomManager,
	) {
		trans, err := transport.NewUDPTransport("127.0.0.1:0")
		if err != nil {
			t.Fatalf("%s: transport: %v", nodeID, err)
		}

		cfg := &config.Config{
			MulticastGroup:   mcastGroup,
			MulticastPort:    mcastPort,
			DiscoveryPort:    0,
			StreamPort:       trans.Addr(),
			AdvHost:          "127.0.0.1",
			AuthToken:        "e2e-token",
			AnnounceInterval: 100 * time.Millisecond,
			PeerTTL:          1 * time.Second,
			ShutdownTimeout:  1 * time.Second,
		}

		disc, err := discovery.New(cfg, nodeID)
		if err != nil {
			t.Fatalf("%s: discovery: %v", nodeID, err)
		}
		go disc.Run()

		devIn := audio.NewVirtualAudioDevice()
		devOut := audio.NewVirtualAudioDevice()
		vp := NewVirtualDeviceProvider(devIn, devOut)

		mgr := NewDefaultRoomManager(cfg, disc, nil, trans, vp)
		return cfg, trans, disc, devIn, devOut, mgr
	}

	// 1. Create Node A, Node B, Node C
	_, transA, discA, devInA, devOutA, mgrA := setupNode("node-01")
	defer func() {
		_ = mgrA.Close()
		discA.Close()
		_ = transA.Close()
		devInA.Close()
		devOutA.Close()
	}()

	_, transB, discB, devInB, devOutB, mgrB := setupNode("node-02")
	defer func() {
		_ = mgrB.Close()
		discB.Close()
		_ = transB.Close()
		devInB.Close()
		devOutB.Close()
	}()

	_, transC, discC, devInC, devOutC, mgrC := setupNode("node-03")
	defer func() {
		_ = mgrC.Close()
		discC.Close()
		_ = transC.Close()
		devInC.Close()
		devOutC.Close()
	}()

	// Wait for peer discovery across all three nodes
	discDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(discDeadline) {
		if len(discA.Peers()) >= 2 && len(discB.Peers()) >= 2 && len(discC.Peers()) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(discA.Peers()) < 2 || len(discB.Peers()) < 2 || len(discC.Peers()) < 2 {
		t.Fatalf("discovery incomplete: peersA=%d, peersB=%d, peersC=%d",
			len(discA.Peers()), len(discB.Peers()), len(discC.Peers()))
	}

	// 2. Create and Join Room across all nodes
	_, err := mgrA.CreateRoom(roomID, "E2E Group Room")
	if err != nil {
		t.Fatalf("mgrA CreateRoom: %v", err)
	}
	_, err = mgrB.CreateRoom(roomID, "E2E Group Room")
	if err != nil {
		t.Fatalf("mgrB CreateRoom: %v", err)
	}
	_, err = mgrC.CreateRoom(roomID, "E2E Group Room")
	if err != nil {
		t.Fatalf("mgrC CreateRoom: %v", err)
	}

	if err := mgrA.JoinRoom(roomID); err != nil {
		t.Fatalf("mgrA JoinRoom: %v", err)
	}
	if err := mgrB.JoinRoom(roomID); err != nil {
		t.Fatalf("mgrB JoinRoom: %v", err)
	}
	if err := mgrC.JoinRoom(roomID); err != nil {
		t.Fatalf("mgrC JoinRoom: %v", err)
	}

	// Wait for leadership to converge on Node A ("node-01" < "node-02" < "node-03")
	leaderDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(leaderDeadline) {
		rA, _ := mgrA.GetRoom(roomID)
		rB, _ := mgrB.GetRoom(roomID)
		rC, _ := mgrC.GetRoom(roomID)
		if rA != nil && rB != nil && rC != nil {
			if rA.Leader() == "node-01" && rB.Leader() == "node-01" && rC.Leader() == "node-01" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	rA, _ := mgrA.GetRoom(roomID)
	rB, _ := mgrB.GetRoom(roomID)
	rC, _ := mgrC.GetRoom(roomID)
	if rA.Leader() != "node-01" || rB.Leader() != "node-01" || rC.Leader() != "node-01" {
		t.Fatalf("expected leader node-01, got A=%q, B=%q, C=%q",
			rA.Leader(), rB.Leader(), rC.Leader())
	}

	// 3. Inject PCM audio packets into Leader Node A
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 40
	)
	testPackets := generateGroupSineWave(sampleRate, channels, numPackets, packetTimeMs, 440.0)
	packetBytes := len(testPackets[0])

	go func() {
		for _, pkt := range testPackets {
			if _, err := devInA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// 4. Verify followers Node B and Node C receive and play out audio
	playoutDeadline := time.Now().Add(5 * time.Second)
	minExpectedBytes := packetBytes * 3
	for time.Now().Before(playoutDeadline) {
		if len(devOutB.CapturedBytes()) >= minExpectedBytes && len(devOutC.CapturedBytes()) >= minExpectedBytes {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	capturedB := devOutB.CapturedBytes()
	capturedC := devOutC.CapturedBytes()

	if len(capturedB) < minExpectedBytes {
		t.Fatalf("follower Node B: expected at least %d bytes, got %d", minExpectedBytes, len(capturedB))
	}
	if len(capturedC) < minExpectedBytes {
		t.Fatalf("follower Node C: expected at least %d bytes, got %d", minExpectedBytes, len(capturedC))
	}

	// Verify non-silent audio on both followers
	hasNonZero := func(data []byte) bool {
		for _, b := range data {
			if b != 0 {
				return true
			}
		}
		return false
	}
	if !hasNonZero(capturedB) {
		t.Fatal("follower Node B received only silence")
	}
	if !hasNonZero(capturedC) {
		t.Fatal("follower Node C received only silence")
	}

	// 5. Failover test: Stop Node A, verify Node B becomes leader deterministically
	t.Log("Initiating Node A shutdown for failover test...")
	_ = mgrA.LeaveRoom(roomID)
	discA.Close()
	_ = transA.Close()

	// Wait for Node B and Node C to detect leader failure and elect Node B
	failoverDeadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(failoverDeadline) {
		if rB.Leader() == "node-02" && rC.Leader() == "node-02" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if rB.Leader() != "node-02" {
		t.Fatalf("failover failed: Node B leader is %q (expected node-02)", rB.Leader())
	}
	if rC.Leader() != "node-02" {
		t.Fatalf("failover failed: Node C leader is %q (expected node-02)", rC.Leader())
	}
	if !rB.IsLeader() {
		t.Fatal("expected Node B to report IsLeader() == true")
	}
}
