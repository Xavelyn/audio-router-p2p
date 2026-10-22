package streamer

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

func TestLatencyPresetValidity(t *testing.T) {
	valid := []string{"", "low", "balanced", "reliable"}
	for _, p := range valid {
		if !LatencyPreset(p).Valid() {
			t.Errorf("preset %q should be valid", p)
		}
	}
	for _, p := range []string{"ultra", "LOW", "Low", "high", "low "} {
		if LatencyPreset(p).Valid() {
			t.Errorf("preset %q should be invalid", p)
		}
	}
	if got := ValidPresets(); len(got) != 3 {
		t.Errorf("expected 3 valid presets, got %v", got)
	}
}

func TestApplyLatencyPreset(t *testing.T) {
	base := DefaultStreamConfig(48000, 2)
	base.PacketTime = 20

	// low: fast start, tight adaptive cap, no FEC.
	low := ApplyLatencyPreset(base, LatencyPresetLow)
	if low.MinDelay != 10*time.Millisecond || low.MaxDelay != 50*time.Millisecond {
		t.Errorf("low delays wrong: %s..%s", low.MinDelay, low.MaxDelay)
	}
	if low.JitterBufSize != 20 {
		t.Errorf("low jitter buffer: %d frames", low.JitterBufSize)
	}
	if low.FEC.Enabled {
		t.Error("low must not enable FEC")
	}
	if low.PacketTime != 20 {
		t.Errorf("presets must not change the packet time, got %d ms", low.PacketTime)
	}

	// balanced: leaves the config untouched.
	same := ApplyLatencyPreset(base, LatencyPresetBalanced)
	if same != base {
		t.Errorf("balanced must not modify the config: %+v vs %+v", same, base)
	}

	// reliable: deep buffer, wide adaptive range, FEC on.
	rel := ApplyLatencyPreset(base, LatencyPresetReliable)
	if rel.MinDelay != 40*time.Millisecond || rel.MaxDelay != 400*time.Millisecond {
		t.Errorf("reliable delays wrong: %s..%s", rel.MinDelay, rel.MaxDelay)
	}
	if rel.JitterBufSize != 400 {
		t.Errorf("reliable jitter buffer: %d frames", rel.JitterBufSize)
	}
	if !rel.FEC.Enabled || rel.FEC.GroupSize != 4 {
		t.Errorf("reliable must enable FEC K=4: %+v", rel.FEC)
	}

	// unknown and empty presets keep the config untouched.
	for _, p := range []LatencyPreset{"", "bogus"} {
		got := ApplyLatencyPreset(base, p)
		if got != base {
			t.Errorf("preset %q must not modify the config", p)
		}
	}
}

func TestJitterBufferSetDelayLimits(t *testing.T) {
	jb := NewJitterBuffer(3840, 48000, 2, 100)
	defer jb.Close()

	jb.SetDelayLimits(10*time.Millisecond, 50*time.Millisecond)
	// targetDelay starts at the constructor min (20ms) and must clamp into
	// the new range; here it stays put because 20ms is inside [10, 50].
	if jb.targetDelay != 20*time.Millisecond {
		t.Errorf("target delay should stay in range, got %s", jb.targetDelay)
	}

	// A range entirely below the current target clamps the target down.
	jb.SetDelayLimits(5*time.Millisecond, 15*time.Millisecond)
	if jb.targetDelay != 15*time.Millisecond {
		t.Errorf("target delay should clamp to max, got %s", jb.targetDelay)
	}

	// Inverted bounds are normalized (max raised to min).
	jb.SetDelayLimits(100*time.Millisecond, 10*time.Millisecond)
	if jb.maxDelay != 100*time.Millisecond {
		t.Errorf("max should be raised to min, got %s", jb.maxDelay)
	}
}

// TestFECParityToNonFECReceiver verifies that a receiver with FEC disabled
// tolerates parity packets from an FEC-enabled sender (e.g. the two ends of
// a link running different latency presets): no panic, media still plays.
func TestFECParityToNonFECReceiver(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 12
	)

	portA := freeUDPPort(t)
	portB := freeUDPPort(t)

	rawTransA, err := transport.NewUDPTransport(net.JoinHostPort("127.0.0.1", strconv.Itoa(portA)))
	if err != nil {
		t.Fatalf("create rawTransA: %v", err)
	}
	defer rawTransA.Close()

	rawTransB, err := transport.NewUDPTransport(net.JoinHostPort("127.0.0.1", strconv.Itoa(portB)))
	if err != nil {
		t.Fatalf("create rawTransB: %v", err)
	}
	defer rawTransB.Close()

	streamID := DeriveStreamID("preset-mismatch-secret", "fec-to-nonfec")

	// Sender runs the reliable preset (FEC on); receiver runs balanced.
	cfgA := ApplyLatencyPreset(DefaultStreamConfig(sampleRate, channels), LatencyPresetReliable)
	cfgB := DefaultStreamConfig(sampleRate, channels)

	devA := audio.NewVirtualAudioDevice()
	defer devA.Close()
	devB := audio.NewVirtualAudioDevice()
	defer devB.Close()

	streamerA, err := NewStreamer(rawTransA, []string{}, cfgA, "nodeA")
	if err != nil {
		t.Fatalf("create streamerA: %v", err)
	}
	streamerA.SetStreamID(streamID)

	streamerB, err := NewStreamer(rawTransB, []string{}, cfgB, "nodeB")
	if err != nil {
		t.Fatalf("create streamerB: %v", err)
	}
	streamerB.SetStreamID(streamID)

	if err := streamerB.Start(nil, []string{}, devB); err != nil {
		t.Fatalf("start streamerB: %v", err)
	}
	defer streamerB.Stop()

	if err := streamerA.Start(devA, []string{rawTransB.Addr()}, nil); err != nil {
		t.Fatalf("start streamerA: %v", err)
	}
	defer streamerA.Stop()

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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if devB.Buffered() >= packetBytes*(numPackets/2) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The receiver must have ingested the media (parity dropped, not fatal).
	if stats := streamerB.Stats(); stats.PacketsReceived == 0 {
		t.Fatalf("FEC-off receiver should still receive media: %+v", stats)
	}
}
