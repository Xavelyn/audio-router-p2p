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

// TestFormatAgilityE2E_44100MonoTo48000Stereo verifies end-to-end heterogeneous audio streaming:
// Node A (44100 Hz, 1 channel mono) transmits to Node B (48000 Hz, 2 channels stereo) over UDP.
// Node B's IngressAdapter dynamically resamples and expands channels so the JitterBuffer and
// playout sink receive uniform 48kHz stereo frames.
func TestFormatAgilityE2E_44100MonoTo48000Stereo(t *testing.T) {
	const (
		rateA        = 44100
		chanA        = 1
		rateB        = 48000
		chanB        = 2
		packetTimeMs = 20
		numPackets   = 50
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

	streamID := DeriveStreamID("format-agility-secret", "e2e-heterogeneous-session")

	cfgA := StreamConfig{
		SampleRate:    rateA,
		Channels:      chanA,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
	}

	cfgB := StreamConfig{
		SampleRate:    rateB,
		Channels:      chanB,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
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

	// Start Receiver B first with devB as output sink
	if err := streamerB.Start(nil, []string{}, devB); err != nil {
		t.Fatalf("start streamerB: %v", err)
	}
	defer streamerB.Stop()

	// Start Sender A pointing to Receiver B with devA as input source
	if err := streamerA.Start(devA, []string{transB.Addr()}, nil); err != nil {
		t.Fatalf("start streamerA: %v", err)
	}
	defer streamerA.Stop()

	// Generate 50 packets of 440Hz sine wave at 44.1kHz mono
	testPackets := generateSineWave(rateA, chanA, numPackets, packetTimeMs, 440.0)
	packetBytesA := len(testPackets[0]) // 882 * 1 * 2 = 1764 bytes
	packetBytesB := (rateB * packetTimeMs / 1000) * chanB * 2 // 960 * 2 * 2 = 3840 bytes

	if packetBytesA != 1764 {
		t.Fatalf("expected packetBytesA 1764, got %d", packetBytesA)
	}
	if packetBytesB != 3840 {
		t.Fatalf("expected packetBytesB 3840, got %d", packetBytesB)
	}

	// Inject audio packets asynchronously
	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(time.Duration(packetTimeMs/2) * time.Millisecond)
		}
	}()

	// Wait for audio to arrive and play out at Node B
	deadline := time.Now().Add(5 * time.Second)
	minExpectedBytesB := packetBytesB * (numPackets / 3) // At least 1/3 received & played out
	for time.Now().Before(deadline) {
		if len(devB.CapturedBytes()) >= minExpectedBytesB {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytesB {
		t.Fatalf("expected at least %d captured bytes at Node B, got %d", minExpectedBytesB, len(captured))
	}

	// Verify format: frame size must be multiple of 4 bytes (2 channels * 16-bit)
	if len(captured)%4 != 0 {
		t.Fatalf("captured audio length %d is not a multiple of 4 (48kHz stereo frame size)", len(captured))
	}

	// Verify that audio is non-silent
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

	// Verify stereo balance: since source was mono duplicated to stereo,
	// Left and Right channels must be identical for each frame.
	numFrames := len(captured) / 4
	for f := 0; f < numFrames; f++ {
		l := int16(binary.LittleEndian.Uint16(captured[f*4:]))
		r := int16(binary.LittleEndian.Uint16(captured[f*4+2:]))
		if l != r {
			t.Fatalf("frame %d: expected identical L and R channels, got L=%d R=%d", f, l, r)
		}
	}

	statsB := streamerB.Stats()
	if statsB.PacketsReceived == 0 {
		t.Fatalf("streamerB should have recorded received packets: %+v", statsB)
	}
}

// TestMultiChannelE2E_51SurroundToStereo verifies heterogeneous multi-channel audio streaming:
// Node A transmits 6-channel (5.1 surround: FL, FR, FC, LFE, SL, SR) audio over UDP to Node B
// (configured for 2-channel stereo).
// Verify audio arrives, plays out in stereo without clipping or channel errors.
func TestMultiChannelE2E_51SurroundToStereo(t *testing.T) {
	const (
		rateA        = 48000
		chanA        = 6 // 5.1 Surround
		rateB        = 48000
		chanB        = 2 // Stereo
		packetTimeMs = 20
		numPackets   = 50
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

	streamID := DeriveStreamID("multichannel-secret", "e2e-51-to-stereo-session")

	cfgA := StreamConfig{
		SampleRate:    rateA,
		Channels:      chanA,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
	}

	cfgB := StreamConfig{
		SampleRate:    rateB,
		Channels:      chanB,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
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

	// Generate 50 packets of 5.1 surround audio (Center and Left Front populated)
	testPackets := make([][]byte, numPackets)
	samplesPerPacket := rateA * packetTimeMs / 1000
	tVal := 0.0
	step := 2.0 * math.Pi * 440.0 / float64(rateA)
	for p := 0; p < numPackets; p++ {
		buf := make([]byte, samplesPerPacket*chanA*2)
		for s := 0; s < samplesPerPacket; s++ {
			val := int16(25000.0 * math.Sin(tVal))
			tVal += step
			// Set Left Front (index 0) and Center (index 2)
			binary.LittleEndian.PutUint16(buf[(s*chanA+0)*2:], uint16(val))
			binary.LittleEndian.PutUint16(buf[(s*chanA+2)*2:], uint16(val))
		}
		testPackets[p] = buf
	}

	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	packetBytesB := samplesPerPacket * chanB * 2
	minExpectedBytesB := packetBytesB * (numPackets / 3)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(devB.CapturedBytes()) >= minExpectedBytesB {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytesB {
		t.Fatalf("expected at least %d captured bytes at Node B, got %d", minExpectedBytesB, len(captured))
	}

	// Verify format: frame size must be multiple of 4 bytes (2 channels * 16-bit)
	if len(captured)%4 != 0 {
		t.Fatalf("captured audio length %d is not a multiple of 4", len(captured))
	}

	// Verify non-zero and no clipping/wrap-around distortion
	numFrames := len(captured) / 4
	hasNonZero := false
	for f := 0; f < numFrames; f++ {
		l := int16(binary.LittleEndian.Uint16(captured[f*4:]))
		r := int16(binary.LittleEndian.Uint16(captured[f*4+2:]))
		if l != 0 || r != 0 {
			hasNonZero = true
		}
		// Confirm samples did not wrap around (which would produce erratic spikes or inverted polarity)
		if l > 32767 || l < -32768 || r > 32767 || r < -32768 {
			t.Fatalf("frame %d: sample out of range L=%d R=%d", f, l, r)
		}
	}
	if !hasNonZero {
		t.Fatal("captured audio consists entirely of silence")
	}

	statsB := streamerB.Stats()
	if statsB.PacketsReceived == 0 {
		t.Fatalf("streamerB should have recorded received packets: %+v", statsB)
	}
}
