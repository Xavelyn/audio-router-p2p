package streamer

import (
	"bytes"
	"encoding/binary"
	"math"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/audio/dsp"
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

// generateSineWave generates 16-bit PCM mono or stereo sine wave data.
func generateSineWave(sampleRate, channels, numPackets, packetTimeMs int, freq float64) [][]byte {
	samplesPerPacket := (sampleRate * packetTimeMs) / 1000
	packets := make([][]byte, numPackets)

	t := 0.0
	step := 2.0 * math.Pi * freq / float64(sampleRate)

	for p := 0; p < numPackets; p++ {
		buf := make([]byte, samplesPerPacket*channels*2)
		for s := 0; s < samplesPerPacket; s++ {
			sampleVal := int16(30000.0 * math.Sin(t))
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

// TestAudioPipelineE2E_UDP verifies end-to-end streaming over UDP:
// Input VirtualAudioDevice -> Streamer A -> UDPTransport -> Streamer B -> JitterBuffer -> Playout -> Output VirtualAudioDevice.
func TestAudioPipelineE2E_UDP(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 60
	)

	portA := freeUDPPort(t)
	portB := freeUDPPort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, err := transport.NewUDPTransport(addrA)
	if err != nil {
		t.Fatalf("create transA: %v", err)
	}
	defer transA.Close()

	transB, err := transport.NewUDPTransport(addrB)
	if err != nil {
		t.Fatalf("create transB: %v", err)
	}
	defer transB.Close()

	streamID := DeriveStreamID("e2e-udp-secret", "e2e-session")

	cfg := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
	}

	devA := audio.NewVirtualAudioDevice()
	defer devA.Close()

	devB := audio.NewVirtualAudioDevice()
	defer devB.Close()

	streamerA, err := NewStreamer(transA, []string{}, cfg, "nodeA")
	if err != nil {
		t.Fatalf("create streamerA: %v", err)
	}
	streamerA.SetStreamID(streamID)

	streamerB, err := NewStreamer(transB, []string{}, cfg, "nodeB")
	if err != nil {
		t.Fatalf("create streamerB: %v", err)
	}
	streamerB.SetStreamID(streamID)

	// Start Receiver B first
	if err := streamerB.Start(nil, []string{}, devB); err != nil {
		t.Fatalf("start streamerB: %v", err)
	}
	defer streamerB.Stop()

	// Start Sender A pointing to Receiver B
	if err := streamerA.Start(devA, []string{transB.Addr()}, nil); err != nil {
		t.Fatalf("start streamerA: %v", err)
	}
	defer streamerA.Stop()

	// Generate and inject test audio packets
	testPackets := generateSineWave(sampleRate, channels, numPackets, packetTimeMs, 440.0)
	packetBytes := len(testPackets[0])

	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Wait for audio to arrive at devB
	deadline := time.Now().Add(5 * time.Second)
	minExpectedBytes := packetBytes * (numPackets / 3) // At least 1/3 of the audio received
	for time.Now().Before(deadline) {
		if devB.Buffered() >= minExpectedBytes {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytes {
		t.Fatalf("expected at least %d captured bytes, got %d", minExpectedBytes, len(captured))
	}

	// Verify that non-zero audio content arrived (not just silence)
	hasNonZero := false
	for _, b := range captured {
		if b != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Fatal("captured audio consists entirely of silence")
	}

	// Check that a characteristic chunk of our generated sine wave is in the captured stream
	targetSnippet := testPackets[5][:64] // A 64-byte snippet from packet 5
	if !bytes.Contains(captured, targetSnippet) {
		t.Fatal("captured audio does not contain injected test audio snippet")
	}

	statsB := streamerB.Stats()
	if statsB.PacketsReceived == 0 {
		t.Fatalf("streamerB should have recorded received packets: %+v", statsB)
	}
}

// TestAudioPipelineE2E_DTLS verifies end-to-end streaming over DTLS:
// Input VirtualAudioDevice -> Streamer A -> DTLSTransport -> Streamer B -> JitterBuffer -> Playout -> Output VirtualAudioDevice.
func TestAudioPipelineE2E_DTLS(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 60
	)

	serverPort := freeUDPPort(t)
	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort))

	psk := []byte("audio-router-e2e-dtls-psk")
	handshakeTimeout := 5 * time.Second

	// Node B is the DTLS server/listener
	dtlsB, err := transport.NewDTLSTransport(transport.DTLSConfig{
		ListenAddr:       serverAddr,
		PSK:              psk,
		IsServer:         true,
		HandshakeTimeout: handshakeTimeout,
	})
	if err != nil {
		t.Fatalf("create dtlsB server: %v", err)
	}
	defer dtlsB.Close()

	// Node A is the DTLS client connecting to serverAddr
	dtlsA, err := transport.NewDTLSTransport(transport.DTLSConfig{
		ListenAddr:       "127.0.0.1:0",
		RemoteAddr:       serverAddr,
		PSK:              psk,
		IsServer:         false,
		HandshakeTimeout: handshakeTimeout,
	})
	if err != nil {
		t.Fatalf("create dtlsA client: %v", err)
	}
	defer dtlsA.Close()

	streamID := DeriveStreamID("e2e-dtls-secret", "e2e-secure-session")

	cfg := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
	}

	devA := audio.NewVirtualAudioDevice()
	defer devA.Close()

	devB := audio.NewVirtualAudioDevice()
	defer devB.Close()

	streamerA, err := NewStreamer(dtlsA, []string{}, cfg, "nodeA")
	if err != nil {
		t.Fatalf("create streamerA: %v", err)
	}
	streamerA.SetStreamID(streamID)

	streamerB, err := NewStreamer(dtlsB, []string{}, cfg, "nodeB")
	if err != nil {
		t.Fatalf("create streamerB: %v", err)
	}
	streamerB.SetStreamID(streamID)

	// Start Receiver B
	if err := streamerB.Start(nil, []string{}, devB); err != nil {
		t.Fatalf("start streamerB: %v", err)
	}
	defer streamerB.Stop()

	// Start Sender A pointing to Receiver B
	if err := streamerA.Start(devA, []string{serverAddr}, nil); err != nil {
		t.Fatalf("start streamerA: %v", err)
	}
	defer streamerA.Stop()

	// Generate and inject test audio packets
	testPackets := generateSineWave(sampleRate, channels, numPackets, packetTimeMs, 880.0)
	packetBytes := len(testPackets[0])

	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Wait for audio to arrive at devB
	deadline := time.Now().Add(7 * time.Second)
	minExpectedBytes := packetBytes * (numPackets / 3)
	for time.Now().Before(deadline) {
		if devB.Buffered() >= minExpectedBytes {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytes {
		t.Fatalf("expected at least %d captured bytes over DTLS, got %d", minExpectedBytes, len(captured))
	}

	// Verify non-zero audio content arrived
	hasNonZero := false
	for _, b := range captured {
		if b != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Fatal("captured audio over DTLS consists entirely of silence")
	}

	// Verify characteristic snippet
	targetSnippet := testPackets[5][:64]
	if !bytes.Contains(captured, targetSnippet) {
		t.Fatal("captured audio over DTLS does not contain injected test audio snippet")
	}

	statsB := streamerB.Stats()
	if statsB.PacketsReceived == 0 {
		t.Fatalf("streamerB should have recorded received packets over DTLS: %+v", statsB)
	}
}

// TestDTXSilenceSuppressionE2E verifies that DTX suppresses transmission during unvoiced intervals:
// Node A transmits 50 packets of active audio followed by 100 packets of silence.
// Verify network transmission drops by >90% during silence, SID packets arrive, and Node B
// has 0 underruns during the silence period.
func TestDTXSilenceSuppressionE2E(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
	)

	portA := freeUDPPort(t)
	portB := freeUDPPort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	transA, err := transport.NewUDPTransport(addrA)
	if err != nil {
		t.Fatalf("create transA: %v", err)
	}
	defer transA.Close()

	transB, err := transport.NewUDPTransport(addrB)
	if err != nil {
		t.Fatalf("create transB: %v", err)
	}
	defer transB.Close()

	streamID := DeriveStreamID("dtx-secret", "e2e-dtx-silence-session")

	dtxCfg := dsp.DTXConfig{
		Enabled:          true,
		SilenceThreshold: -50.0,
		HangoverDuration: 60 * time.Millisecond,  // 3 frames hangover to enter silence cleanly
		SIDInterval:      500 * time.Millisecond, // 25 frames
		FrameDuration:    20 * time.Millisecond,
	}

	cfgA := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
		DTX:           dtxCfg,
	}

	cfgB := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
		DTX:           dtxCfg,
	}

	devA := audio.NewVirtualAudioDevice()
	defer devA.Close()

	devB := audio.NewVirtualAudioDevice()
	defer devB.Close()

	streamerA, err := NewStreamer(transA, []string{}, cfgA, "nodeA")
	if err != nil {
		t.Fatalf("create streamerA: %v", err)
	}
	streamerA.SetStreamID(streamID)

	streamerB, err := NewStreamer(transB, []string{}, cfgB, "nodeB")
	if err != nil {
		t.Fatalf("create streamerB: %v", err)
	}
	streamerB.SetStreamID(streamID)

	if err := streamerB.Start(nil, []string{}, devB); err != nil {
		t.Fatalf("start streamerB: %v", err)
	}
	defer streamerB.Stop()

	if err := streamerA.Start(devA, []string{transB.Addr()}, nil); err != nil {
		t.Fatalf("start streamerA: %v", err)
	}
	defer streamerA.Stop()

	packetBytes := (sampleRate * packetTimeMs / 1000) * channels * 2

	// Phase 1: Transmit 50 active audio packets (sine wave at -6 dBFS)
	activePackets := generateSineWave(sampleRate, channels, 50, packetTimeMs, 440.0)
	for _, pkt := range activePackets {
		if _, err := devA.InjectPush(pkt); err != nil {
			t.Fatalf("inject active packet: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for active audio to reach streamerB
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if streamerB.Stats().PacketsReceived >= 20 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sentBeforeSilence := streamerA.Stats().PacketsSent
	underrunsBeforeSilence := streamerB.Stats().Underruns

	// Phase 2: Transmit 100 silence packets (zeroed PCM)
	silentPacket := make([]byte, packetBytes)
	for i := 0; i < 100; i++ {
		if _, err := devA.InjectPush(silentPacket); err != nil {
			t.Fatalf("inject silent packet: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for all 100 silence packets to be processed by streamerA
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if streamerA.Stats().PacketsSuppressed >= 90 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	statsA := streamerA.Stats()
	statsB := streamerB.Stats()

	// Verify packets suppressed: out of 100 silence packets, with 3 frames hangover and 25-frame SID,
	// at least 90 packets must be suppressed (>90% reduction in transmission)
	packetsSentDuringSilence := statsA.PacketsSent - sentBeforeSilence
	suppressionRatio := float64(statsA.PacketsSuppressed) / 100.0

	if statsA.PacketsSuppressed < 90 {
		t.Fatalf("expected at least 90 suppressed packets (>90%% drop), got %d (sent during silence: %d, ratio: %.2f)",
			statsA.PacketsSuppressed, packetsSentDuringSilence, suppressionRatio)
	}

	// Verify DTX State on sender
	if statsA.DTXState != "Silence" {
		t.Fatalf("expected sender DTX state 'Silence', got '%s'", statsA.DTXState)
	}

	// Verify receiver had 0 underruns during silence period
	underrunsDuringSilence := statsB.Underruns - underrunsBeforeSilence
	if underrunsDuringSilence != 0 {
		t.Fatalf("expected 0 underruns during DTX silence period, got %d", underrunsDuringSilence)
	}

	// Verify receiver JitterBuffer is marked in DTX mode (received SID packet)
	foundPeerDTX := false
	for _, peerStats := range statsB.Peers {
		if peerStats.DTXMode {
			foundPeerDTX = true
			break
		}
	}
	if !foundPeerDTX {
		t.Fatalf("expected peer jitter buffer on Node B to be in DTX mode after SID arrival: %+v", statsB.Peers)
	}
}
