package streamer

import (
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestAudioPipelineThroughNetworkSimulator verifies the degraded-network
// path end to end: 30% loss and 5-25 ms jitter on the sender's egress must
// still deliver audible audio at the receiver, while the receiver observes
// the loss (fewer received packets than sent).
func TestAudioPipelineThroughNetworkSimulator(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 150
	)

	transB, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transB: %v", err)
	}
	defer transB.Close()

	udpA, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transA: %v", err)
	}
	simA := transport.NewNetworkSimulator(udpA, transport.SimProfile{
		Enabled:   true,
		LossPct:   30,
		LatencyMS: 5,
		JitterMS:  20,
	})
	defer simA.Close()

	streamID := DeriveStreamID("sim-e2e-secret", "sim-session")
	cfg := DefaultStreamConfig(sampleRate, channels)

	devA := audio.NewVirtualAudioDevice()
	defer devA.Close()
	devB := audio.NewVirtualAudioDevice()
	defer devB.Close()

	streamerA, err := NewStreamer(simA, []string{}, cfg, "nodeA")
	if err != nil {
		t.Fatalf("create streamerA: %v", err)
	}
	streamerA.SetStreamID(streamID)

	streamerB, err := NewStreamer(transB, []string{}, cfg, "nodeB")
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

	testPackets := generateSineWave(sampleRate, channels, numPackets, packetTimeMs, 440.0)
	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Wait for a substantial fraction of the audio to be captured at B.
	// With 30% loss and PLC concealment the byte count still grows steadily.
	deadline := time.Now().Add(10 * time.Second)
	minExpectedBytes := len(testPackets[0]) * (numPackets / 3)
	for time.Now().Before(deadline) {
		if devB.Buffered() >= minExpectedBytes {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytes {
		t.Fatalf("expected at least %d captured bytes through the degraded link, got %d",
			minExpectedBytes, len(captured))
	}
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

	statsA := streamerA.Stats()
	statsB := streamerB.Stats()
	if statsB.PacketsReceived == 0 {
		t.Fatalf("streamerB should have recorded received packets: %+v", statsB)
	}
	if statsB.PacketsReceived >= statsA.PacketsSent {
		t.Fatalf("30%% loss should be visible at the receiver: sent %d, received %d",
			statsA.PacketsSent, statsB.PacketsReceived)
	}
}
