package streamer

import (
	"encoding/binary"
	"math"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

// freePort returns an available UDP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// TestIntraNodeRouting_BypassAndLoopback tests routing within a single node:
// 1. Direct local bypass (when destination matches localIPs)
// 2. Local UDP loopback routing (real socket receive and playout on 127.0.0.1)
func TestIntraNodeRouting_BypassAndLoopback(t *testing.T) {
	// Subtest 1: Intra-node local bypass (zero network overhead, direct playout)
	t.Run("DirectLocalBypass", func(t *testing.T) {
		const (
			rate       = 48000
			channels   = 2
			packetTime = 20
			numPackets = 30
		)
		localIPs := []string{"127.0.0.1"}

		cfg := DefaultStreamConfig(rate, channels)
		cfg.PacketTime = packetTime

		inDev := audio.NewVirtualAudioDevice()
		defer inDev.Close()
		outDev := audio.NewVirtualAudioDevice()
		defer outDev.Close()

		st, err := NewStreamer(nil, localIPs, cfg, "local-node")
		if err != nil {
			t.Fatalf("NewStreamer: %v", err)
		}
		defer st.Stop()

		// Destination is 127.0.0.1:45678, which is in localIPs -> triggers intra-node bypass
		if err := st.Start(inDev, []string{"127.0.0.1:45678"}, outDev); err != nil {
			t.Fatalf("Start: %v", err)
		}

		testPackets := generateSineWave(rate, channels, numPackets, packetTime, 440.0)
		for _, pkt := range testPackets {
			if _, err := inDev.InjectPush(pkt); err != nil {
				t.Fatalf("InjectPush: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}

		deadline := time.Now().Add(2 * time.Second)
		minExpected := (rate * packetTime / 1000) * channels * 2 * (numPackets - 5)
		for time.Now().Before(deadline) {
			if len(outDev.CapturedBytes()) >= minExpected {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		captured := outDev.CapturedBytes()
		if len(captured) < minExpected {
			t.Fatalf("intra-node direct bypass: expected >= %d bytes, got %d", minExpected, len(captured))
		}
	})

	// Subtest 2: Intra-node UDP loopback (socket send -> socket receive -> jitter buffer -> playout)
	t.Run("UDPLoopbackReceive", func(t *testing.T) {
		const (
			rate       = 48000
			channels   = 2
			packetTime = 20
			numPackets = 30
		)
		port := freePort(t)
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

		trans, err := transport.NewUDPTransport(addr)
		if err != nil {
			t.Fatalf("NewUDPTransport: %v", err)
		}
		defer trans.Close()

		cfg := DefaultStreamConfig(rate, channels)
		cfg.PacketTime = packetTime

		inDev := audio.NewVirtualAudioDevice()
		defer inDev.Close()
		outDev := audio.NewVirtualAudioDevice()
		defer outDev.Close()

		// Pass empty localIPs so it does NOT bypass; it exercises the true UDP receive path!
		st, err := NewStreamer(trans, []string{}, cfg, "loopback-node")
		if err != nil {
			t.Fatalf("NewStreamer: %v", err)
		}
		defer st.Stop()

		streamID := DeriveStreamID("loopback-secret", "intra-node-test")
		st.SetStreamID(streamID)

		if err := st.Start(inDev, []string{addr}, outDev); err != nil {
			t.Fatalf("Start: %v", err)
		}

		testPackets := generateSineWave(rate, channels, numPackets, packetTime, 440.0)
		for _, pkt := range testPackets {
			if _, err := inDev.InjectPush(pkt); err != nil {
				t.Fatalf("InjectPush: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}

		deadline := time.Now().Add(3 * time.Second)
		minExpected := (rate * packetTime / 1000) * channels * 2 * (numPackets / 3)
		for time.Now().Before(deadline) {
			if len(outDev.CapturedBytes()) >= minExpected {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		captured := outDev.CapturedBytes()
		if len(captured) < minExpected {
			t.Fatalf("intra-node UDP loopback: expected >= %d bytes, got %d", minExpected, len(captured))
		}
	})
}

// TestNodeToNode_BidirectionalMatrixRouting tests concurrent bidirectional audio streams:
// Node A sends audio stream to Node B while Node B sends audio stream to Node A simultaneously.
func TestNodeToNode_BidirectionalMatrixRouting(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		numPackets = 40
	)

	portA := freePort(t)
	portB := freePort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, err := transport.NewUDPTransport(addrA)
	if err != nil {
		t.Fatalf("transA: %v", err)
	}
	defer transA.Close()

	transB, err := transport.NewUDPTransport(addrB)
	if err != nil {
		t.Fatalf("transB: %v", err)
	}
	defer transB.Close()

	sidAB := DeriveStreamID("matrix-secret", "stream-A-to-B")
	sidBA := DeriveStreamID("matrix-secret", "stream-B-to-A")

	cfgA := DefaultStreamConfig(rate, channels)
	cfgA.PacketTime = packetTime
	cfgB := DefaultStreamConfig(rate, channels)
	cfgB.PacketTime = packetTime

	inA, outA := audio.NewVirtualAudioDevice(), audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer outA.Close()

	inB, outB := audio.NewVirtualAudioDevice(), audio.NewVirtualAudioDevice()
	defer inB.Close()
	defer outB.Close()

	stA, err := NewStreamer(transA, []string{}, cfgA, "nodeA")
	if err != nil {
		t.Fatalf("NewStreamer A: %v", err)
	}
	stA.SetStreamID(sidAB)
	defer stA.Stop()

	stB, err := NewStreamer(transB, []string{}, cfgB, "nodeB")
	if err != nil {
		t.Fatalf("NewStreamer B: %v", err)
	}
	stB.SetStreamID(sidBA)
	defer stB.Stop()

	// Each directional route has its own session ID and matching receiver on
	// the shared socket; knowing a sender's address cannot bypass demux.
	recvA, err := NewStreamer(transA, nil, cfgA, "nodeA")
	if err != nil {
		t.Fatalf("NewStreamer receiver A: %v", err)
	}
	recvA.SetStreamID(sidBA)
	defer recvA.Stop()
	recvB, err := NewStreamer(transB, nil, cfgB, "nodeB")
	if err != nil {
		t.Fatalf("NewStreamer receiver B: %v", err)
	}
	recvB.SetStreamID(sidAB)
	defer recvB.Stop()
	if err := recvA.Start(nil, nil, outA); err != nil {
		t.Fatalf("Start recvA: %v", err)
	}
	if err := recvB.Start(nil, nil, outB); err != nil {
		t.Fatalf("Start recvB: %v", err)
	}

	// Start independent send-only directional routes.
	if err := stA.Start(inA, []string{addrB}, nil); err != nil {
		t.Fatalf("Start stA: %v", err)
	}
	if err := stB.Start(inB, []string{addrA}, nil); err != nil {
		t.Fatalf("Start stB: %v", err)
	}

	// Generate different frequencies: 440 Hz for A->B, 880 Hz for B->A
	pktsA := generateSineWave(rate, channels, numPackets, packetTime, 440.0)
	pktsB := generateSineWave(rate, channels, numPackets, packetTime, 880.0)

	// Inject concurrently
	go func() {
		for _, pkt := range pktsA {
			_, _ = inA.InjectPush(pkt)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	go func() {
		for _, pkt := range pktsB {
			_, _ = inB.InjectPush(pkt)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	minExpected := (rate * packetTime / 1000) * channels * 2 * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)

	for time.Now().Before(deadline) {
		if len(outA.CapturedBytes()) >= minExpected && len(outB.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	capA := outA.CapturedBytes()
	capB := outB.CapturedBytes()

	if len(capA) < minExpected {
		t.Fatalf("Node A did not receive expected audio from Node B (got %d, want >= %d)", len(capA), minExpected)
	}
	if len(capB) < minExpected {
		t.Fatalf("Node B did not receive expected audio from Node A (got %d, want >= %d)", len(capB), minExpected)
	}
}

// TestMatrixRouting_OneToManyBroadcast tests 1-to-N fanout routing:
// Node A transmits a single audio input stream to Node B and Node C simultaneously.
func TestMatrixRouting_OneToManyBroadcast(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		numPackets = 40
	)

	portA := freePort(t)
	portB := freePort(t)
	portC := freePort(t)

	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))
	addrC := net.JoinHostPort("127.0.0.1", strconv.Itoa(portC))

	transA, _ := transport.NewUDPTransport(addrA)
	transB, _ := transport.NewUDPTransport(addrB)
	transC, _ := transport.NewUDPTransport(addrC)
	defer transA.Close()
	defer transB.Close()
	defer transC.Close()

	streamID := DeriveStreamID("broadcast-secret", "room-broadcast-1")

	cfg := DefaultStreamConfig(rate, channels)
	cfg.PacketTime = packetTime

	inA := audio.NewVirtualAudioDevice()
	outB := audio.NewVirtualAudioDevice()
	outC := audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer outB.Close()
	defer outC.Close()

	stA, _ := NewStreamer(transA, []string{}, cfg, "nodeA")
	stA.SetStreamID(streamID)
	defer stA.Stop()

	stB, _ := NewStreamer(transB, []string{}, cfg, "nodeB")
	stB.SetStreamID(streamID)
	defer stB.Stop()

	stC, _ := NewStreamer(transC, []string{}, cfg, "nodeC")
	stC.SetStreamID(streamID)
	defer stC.Stop()

	// Receivers start first
	_ = stB.Start(nil, []string{}, outB)
	_ = stC.Start(nil, []string{}, outC)

	// Sender starts with multiple destinations: [addrB, addrC]
	if err := stA.Start(inA, []string{addrB, addrC}, nil); err != nil {
		t.Fatalf("Start stA fanout: %v", err)
	}

	pkts := generateSineWave(rate, channels, numPackets, packetTime, 440.0)
	go func() {
		for _, pkt := range pkts {
			_, _ = inA.InjectPush(pkt)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	minExpected := (rate * packetTime / 1000) * channels * 2 * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(outB.CapturedBytes()) >= minExpected && len(outC.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if len(outB.CapturedBytes()) < minExpected {
		t.Fatalf("Node B failed to receive broadcast audio (got %d bytes)", len(outB.CapturedBytes()))
	}
	if len(outC.CapturedBytes()) < minExpected {
		t.Fatalf("Node C failed to receive broadcast audio (got %d bytes)", len(outC.CapturedBytes()))
	}
}

// TestMatrixRouting_ManyToOneFanIn tests N-to-1 audio multiplexing:
// Node A and Node B both stream audio over the network to Node C's output sink.
func TestMatrixRouting_ManyToOneFanIn(t *testing.T) {
	const (
		rate       = 48000
		channels   = 2
		packetTime = 20
		numPackets = 30
	)

	portA := freePort(t)
	portB := freePort(t)
	portC := freePort(t)

	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))
	addrC := net.JoinHostPort("127.0.0.1", strconv.Itoa(portC))

	transA, _ := transport.NewUDPTransport(addrA)
	transB, _ := transport.NewUDPTransport(addrB)
	transC, _ := transport.NewUDPTransport(addrC)
	defer transA.Close()
	defer transB.Close()
	defer transC.Close()

	sidA := DeriveStreamID("fanin-secret", "stream-source-A")
	sidB := DeriveStreamID("fanin-secret", "stream-source-B")

	cfg := DefaultStreamConfig(rate, channels)
	cfg.PacketTime = packetTime

	inA := audio.NewVirtualAudioDevice()
	inB := audio.NewVirtualAudioDevice()
	outC := audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer inB.Close()
	defer outC.Close()

	stA, _ := NewStreamer(transA, []string{}, cfg, "nodeA")
	stA.SetStreamID(sidA)
	defer stA.Stop()

	stB, _ := NewStreamer(transB, []string{}, cfg, "nodeB")
	stB.SetStreamID(sidB)
	defer stB.Stop()

	// Node C is in promiscuous/receiver mode for both peers
	stC, _ := NewStreamer(transC, []string{}, cfg, "nodeC")
	stC.SetStreamID([16]byte{}) // promiscuous receiver
	defer stC.Stop()

	_ = stC.Start(nil, []string{}, outC)
	_ = stA.Start(inA, []string{addrC}, nil)
	_ = stB.Start(inB, []string{addrC}, nil)

	pktsA := generateSineWave(rate, channels, numPackets, packetTime, 440.0)
	pktsB := generateSineWave(rate, channels, numPackets, packetTime, 660.0)

	go func() {
		for _, p := range pktsA {
			_, _ = inA.InjectPush(p)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() {
		for _, p := range pktsB {
			_, _ = inB.InjectPush(p)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	minExpected := (rate * packetTime / 1000) * channels * 2 * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(outC.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if len(outC.CapturedBytes()) < minExpected {
		t.Fatalf("Node C did not capture fan-in audio (got %d bytes, want >= %d)", len(outC.CapturedBytes()), minExpected)
	}
}

// TestMultiChannel_StereoTo51Upmix_E2E tests 2-channel Stereo upmixed to 6-channel 5.1 Surround across network transports.
func TestMultiChannel_StereoTo51Upmix_E2E(t *testing.T) {
	const (
		rate       = 48000
		inCh       = 2 // Stereo
		outCh      = 6 // 5.1 Surround
		packetTime = 20
		numPackets = 30
	)

	portA := freePort(t)
	portB := freePort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, _ := transport.NewUDPTransport(addrA)
	transB, _ := transport.NewUDPTransport(addrB)
	defer transA.Close()
	defer transB.Close()

	sid := DeriveStreamID("matrix-secret", "stereo-to-51")

	cfgA := DefaultStreamConfig(rate, inCh)
	cfgA.PacketTime = packetTime

	cfgB := DefaultStreamConfig(rate, outCh)
	cfgB.PacketTime = packetTime

	inA := audio.NewVirtualAudioDevice()
	outB := audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer outB.Close()

	stA, _ := NewStreamer(transA, []string{}, cfgA, "nodeA")
	stA.SetStreamID(sid)
	defer stA.Stop()

	stB, _ := NewStreamer(transB, []string{}, cfgB, "nodeB")
	stB.SetStreamID(sid)
	defer stB.Stop()

	_ = stB.Start(nil, []string{}, outB)
	_ = stA.Start(inA, []string{addrB}, nil)

	pkts := generateSineWave(rate, inCh, numPackets, packetTime, 440.0)
	go func() {
		for _, p := range pkts {
			_, _ = inA.InjectPush(p)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	expectedFrameBytes := outCh * 2 // 12 bytes per 5.1 frame
	minExpected := (rate * packetTime / 1000) * expectedFrameBytes * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)

	for time.Now().Before(deadline) {
		if len(outB.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	capB := outB.CapturedBytes()
	if len(capB) < minExpected {
		t.Fatalf("Node B 5.1 upmix: expected >= %d bytes, got %d", minExpected, len(capB))
	}
	if len(capB)%expectedFrameBytes != 0 {
		t.Fatalf("Node B 5.1 upmix: byte length %d is not a multiple of 12 (6-channel frame size)", len(capB))
	}

	// Verify non-silent samples exist across FL (0), FR (1), and FC (2)
	hasCenter := false
	numFrames := len(capB) / expectedFrameBytes
	for f := 0; f < numFrames; f++ {
		fc := int16(binary.LittleEndian.Uint16(capB[f*expectedFrameBytes+4:]))
		if fc != 0 {
			hasCenter = true
			break
		}
	}
	if !hasCenter {
		t.Fatal("5.1 upmix: Center channel consists entirely of silence")
	}
}

// TestMultiChannel_StereoToQuadUpmix_E2E tests 2-channel Stereo upmixed to 4-channel Quadraphonic across network transports.
func TestMultiChannel_StereoToQuadUpmix_E2E(t *testing.T) {
	const (
		rate       = 48000
		inCh       = 2 // Stereo
		outCh      = 4 // Quadraphonic
		packetTime = 20
		numPackets = 30
	)

	portA := freePort(t)
	portB := freePort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, _ := transport.NewUDPTransport(addrA)
	transB, _ := transport.NewUDPTransport(addrB)
	defer transA.Close()
	defer transB.Close()

	sid := DeriveStreamID("matrix-secret", "stereo-to-quad")

	cfgA := DefaultStreamConfig(rate, inCh)
	cfgA.PacketTime = packetTime
	cfgB := DefaultStreamConfig(rate, outCh)
	cfgB.PacketTime = packetTime

	inA := audio.NewVirtualAudioDevice()
	outB := audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer outB.Close()

	stA, _ := NewStreamer(transA, []string{}, cfgA, "nodeA")
	stA.SetStreamID(sid)
	defer stA.Stop()

	stB, _ := NewStreamer(transB, []string{}, cfgB, "nodeB")
	stB.SetStreamID(sid)
	defer stB.Stop()

	_ = stB.Start(nil, []string{}, outB)
	_ = stA.Start(inA, []string{addrB}, nil)

	pkts := generateSineWave(rate, inCh, numPackets, packetTime, 440.0)
	go func() {
		for _, p := range pkts {
			_, _ = inA.InjectPush(p)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	expectedFrameBytes := outCh * 2 // 8 bytes per Quad frame
	minExpected := (rate * packetTime / 1000) * expectedFrameBytes * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)

	for time.Now().Before(deadline) {
		if len(outB.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	capB := outB.CapturedBytes()
	if len(capB) < minExpected {
		t.Fatalf("Node B Quad upmix: expected >= %d bytes, got %d", minExpected, len(capB))
	}
	if len(capB)%expectedFrameBytes != 0 {
		t.Fatalf("Node B Quad upmix: length %d not a multiple of 8", len(capB))
	}
}

// TestMultiChannel_QuadToStereoDownmix_E2E tests 4-channel Quadraphonic downmixed to 2-channel Stereo.
func TestMultiChannel_QuadToStereoDownmix_E2E(t *testing.T) {
	const (
		rate       = 48000
		inCh       = 4 // Quadraphonic
		outCh      = 2 // Stereo
		packetTime = 20
		numPackets = 30
	)

	portA := freePort(t)
	portB := freePort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, _ := transport.NewUDPTransport(addrA)
	transB, _ := transport.NewUDPTransport(addrB)
	defer transA.Close()
	defer transB.Close()

	sid := DeriveStreamID("matrix-secret", "quad-to-stereo")

	cfgA := DefaultStreamConfig(rate, inCh)
	cfgA.PacketTime = packetTime
	cfgB := DefaultStreamConfig(rate, outCh)
	cfgB.PacketTime = packetTime

	inA := audio.NewVirtualAudioDevice()
	outB := audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer outB.Close()

	stA, _ := NewStreamer(transA, []string{}, cfgA, "nodeA")
	stA.SetStreamID(sid)
	defer stA.Stop()

	stB, _ := NewStreamer(transB, []string{}, cfgB, "nodeB")
	stB.SetStreamID(sid)
	defer stB.Stop()

	_ = stB.Start(nil, []string{}, outB)
	_ = stA.Start(inA, []string{addrB}, nil)

	pkts := generateSineWave(rate, inCh, numPackets, packetTime, 440.0)
	go func() {
		for _, p := range pkts {
			_, _ = inA.InjectPush(p)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	expectedFrameBytes := outCh * 2 // 4 bytes per Stereo frame
	minExpected := (rate * packetTime / 1000) * expectedFrameBytes * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)

	for time.Now().Before(deadline) {
		if len(outB.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	capB := outB.CapturedBytes()
	if len(capB) < minExpected {
		t.Fatalf("Node B Quad downmix: expected >= %d bytes, got %d", minExpected, len(capB))
	}
	if len(capB)%expectedFrameBytes != 0 {
		t.Fatalf("Node B Quad downmix: length %d not a multiple of 4", len(capB))
	}
}

// TestMultiChannel_71ToStereoDownmix_E2E tests 8-channel 7.1 Surround downmixed to 2-channel Stereo.
func TestMultiChannel_71ToStereoDownmix_E2E(t *testing.T) {
	const (
		rate       = 48000
		inCh       = 8 // 7.1 Surround
		outCh      = 2 // Stereo
		packetTime = 20
		numPackets = 30
	)

	portA := freePort(t)
	portB := freePort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, _ := transport.NewUDPTransport(addrA)
	transB, _ := transport.NewUDPTransport(addrB)
	defer transA.Close()
	defer transB.Close()

	sid := DeriveStreamID("matrix-secret", "71-to-stereo")

	cfgA := DefaultStreamConfig(rate, inCh)
	cfgA.PacketTime = packetTime
	cfgB := DefaultStreamConfig(rate, outCh)
	cfgB.PacketTime = packetTime

	inA := audio.NewVirtualAudioDevice()
	outB := audio.NewVirtualAudioDevice()
	defer inA.Close()
	defer outB.Close()

	stA, _ := NewStreamer(transA, []string{}, cfgA, "nodeA")
	stA.SetStreamID(sid)
	defer stA.Stop()

	stB, _ := NewStreamer(transB, []string{}, cfgB, "nodeB")
	stB.SetStreamID(sid)
	defer stB.Stop()

	_ = stB.Start(nil, []string{}, outB)
	_ = stA.Start(inA, []string{addrB}, nil)

	// Generate 8-channel test tone
	pkts := generateMultiChannelTone(rate, inCh, numPackets, packetTime, 440.0)
	go func() {
		for _, p := range pkts {
			_, _ = inA.InjectPush(p)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	expectedFrameBytes := outCh * 2 // 4 bytes per Stereo frame
	minExpected := (rate * packetTime / 1000) * expectedFrameBytes * (numPackets / 3)
	deadline := time.Now().Add(4 * time.Second)

	for time.Now().Before(deadline) {
		if len(outB.CapturedBytes()) >= minExpected {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	capB := outB.CapturedBytes()
	if len(capB) < minExpected {
		t.Fatalf("Node B 7.1 downmix: expected >= %d bytes, got %d", minExpected, len(capB))
	}
	if len(capB)%expectedFrameBytes != 0 {
		t.Fatalf("Node B 7.1 downmix: length %d not a multiple of 4", len(capB))
	}
}

func generateMultiChannelTone(sampleRate, channels, numPackets, packetTimeMs int, freq float64) [][]byte {
	framesPerPacket := sampleRate * packetTimeMs / 1000
	packetBytes := framesPerPacket * channels * 2
	packets := make([][]byte, numPackets)

	phase := 0.0
	phaseInc := 2.0 * math.Pi * freq / float64(sampleRate)

	for p := 0; p < numPackets; p++ {
		buf := make([]byte, packetBytes)
		for f := 0; f < framesPerPacket; f++ {
			sampleVal := int16(16384.0 * math.Sin(phase))
			phase += phaseInc
			for c := 0; c < channels; c++ {
				off := (f*channels + c) * 2
				binary.LittleEndian.PutUint16(buf[off:], uint16(sampleVal))
			}
		}
		packets[p] = buf
	}
	return packets
}
