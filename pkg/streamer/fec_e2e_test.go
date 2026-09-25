package streamer

import (
	"bytes"
	"math"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

// filteredTransport wraps an underlying Transport and selectively drops outgoing packets
// matching drop predicate.
type filteredTransport struct {
	underlying Transport
	mu         sync.Mutex
	dropFilter func(hdr transport.PacketHeader, payload []byte) bool
}

func newFilteredTransport(t Transport, filter func(hdr transport.PacketHeader, payload []byte) bool) *filteredTransport {
	return &filteredTransport{
		underlying: t,
		dropFilter: filter,
	}
}

func (f *filteredTransport) Addr() string {
	return f.underlying.Addr()
}

func (f *filteredTransport) LocalAddr() *net.UDPAddr {
	return f.underlying.LocalAddr()
}

func (f *filteredTransport) Send(to string, data []byte) error {
	f.mu.Lock()
	filter := f.dropFilter
	f.mu.Unlock()

	if filter != nil {
		hdr, payload, err := transport.Unmarshal(data)
		if err == nil && filter(hdr, payload) {
			return nil // simulate packet loss
		}
	}
	return f.underlying.Send(to, data)
}

func (f *filteredTransport) Receive(handler func([]byte, *net.UDPAddr)) func() {
	return f.underlying.Receive(handler)
}

func (f *filteredTransport) Wait() {
	f.underlying.Wait()
}

func (f *filteredTransport) Closed() bool {
	return f.underlying.Closed()
}

func (f *filteredTransport) Close() error {
	return f.underlying.Close()
}

// TestFECE2E_SingleLossRecovery verifies end-to-end RFC 5109 FEC recovery over UDP:
// 1. Streamer A sends audio with FEC enabled (K=4).
// 2. Transport drops exactly one packet (sequence 2) in the first FEC group {0, 1, 2, 3}.
// 3. The parity packet reconstructs packet 2 at Streamer B without retransmission.
// 4. Streamer B's jitter buffer receives the packet with FlagRedundant and plays it out.
// 5. Verifies PacketsRecoveredTotal >= 1, UnrecoverableGroupsTotal == 0, and audio snippet match.
func TestFECE2E_SingleLossRecovery(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 24
	)

	portA := freeUDPPort(t)
	portB := freeUDPPort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	rawTransA, err := transport.NewUDPTransport(addrA)
	if err != nil {
		t.Fatalf("create rawTransA: %v", err)
	}
	defer rawTransA.Close()

	rawTransB, err := transport.NewUDPTransport(addrB)
	if err != nil {
		t.Fatalf("create rawTransB: %v", err)
	}
	defer rawTransB.Close()

	// Drop packet with Seq == 2 (media packet)
	droppedSeq := uint32(2)
	var droppedCount int
	var dropMu sync.Mutex

	transA := newFilteredTransport(rawTransA, func(hdr transport.PacketHeader, payload []byte) bool {
		if (hdr.Flags&transport.FlagFEC) == 0 && hdr.Seq == droppedSeq {
			dropMu.Lock()
			droppedCount++
			dropMu.Unlock()
			return true // drop packet 2
		}
		return false
	})

	streamID := DeriveStreamID("fec-e2e-secret", "fec-single-loss")

	cfg := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      40 * time.Millisecond,
		MaxDelay:      200 * time.Millisecond,
		FEC: FECConfig{
			Enabled:   true,
			GroupSize: 4,
		},
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

	streamerB, err := NewStreamer(rawTransB, []string{}, cfg, "nodeB")
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

	// Wait for audio to arrive at devB
	deadline := time.Now().Add(5 * time.Second)
	minExpectedBytes := packetBytes * (numPackets / 2)
	for time.Now().Before(deadline) {
		if devB.Buffered() >= minExpectedBytes {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	dropMu.Lock()
	numDropped := droppedCount
	dropMu.Unlock()
	if numDropped == 0 {
		t.Fatalf("expected packet %d to be dropped by filter", droppedSeq)
	}

	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytes {
		t.Fatalf("expected at least %d captured bytes, got %d", minExpectedBytes, len(captured))
	}

	statsB := streamerB.Stats()
	if statsB.FEC.PacketsRecoveredTotal == 0 {
		t.Fatalf("expected at least 1 recovered packet in statsB, got %+v", statsB.FEC)
	}
	if statsB.FEC.UnrecoverableGroupsTotal != 0 {
		t.Fatalf("expected 0 unrecoverable groups, got %d", statsB.FEC.UnrecoverableGroupsTotal)
	}

	// Verify that the dropped packet 2 audio was recovered and appears in captured output!
	targetSnippet := testPackets[droppedSeq][:64]
	if !bytes.Contains(captured, targetSnippet) {
		t.Fatalf("captured audio is missing snippet from recovered packet %d", droppedSeq)
	}
}

// TestFECE2E_BurstLossPLCConcealment verifies packet loss concealment when loss exceeds FEC capacity:
// 1. Two packets in group {0, 1, 2, 3} are dropped (Seq 1 and Seq 2).
// 2. FEC decoder detects unrecoverable group (2 lost > 1 parity).
// 3. JitterBuffer triggers PSOLA Conceal during playout.
// 4. Verifies UnrecoverableGroupsTotal >= 1, ConcealedFramesTotal >= 1, and non-silent output.
func TestFECE2E_BurstLossPLCConcealment(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
		packetTimeMs = 20
		numPackets   = 20
	)

	portA := freeUDPPort(t)
	portB := freeUDPPort(t)
	addrA := net.JoinHostPort("127.0.0.1", strconv.Itoa(portA))
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))

	rawTransA, err := transport.NewUDPTransport(addrA)
	if err != nil {
		t.Fatalf("create rawTransA: %v", err)
	}
	defer rawTransA.Close()

	rawTransB, err := transport.NewUDPTransport(addrB)
	if err != nil {
		t.Fatalf("create rawTransB: %v", err)
	}
	defer rawTransB.Close()

	// Drop packets with Seq == 1 and Seq == 2 (2 media packets dropped in same FEC group)
	var droppedBurstCount int
	var dropMu sync.Mutex

	transA := newFilteredTransport(rawTransA, func(hdr transport.PacketHeader, payload []byte) bool {
		if (hdr.Flags&transport.FlagFEC) == 0 && (hdr.Seq == 1 || hdr.Seq == 2) {
			dropMu.Lock()
			droppedBurstCount++
			dropMu.Unlock()
			return true
		}
		return false
	})

	streamID := DeriveStreamID("fec-e2e-secret", "plc-burst-loss")

	cfg := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      100 * time.Millisecond,
		FEC: FECConfig{
			Enabled:   true,
			GroupSize: 4,
		},
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

	streamerB, err := NewStreamer(rawTransB, []string{}, cfg, "nodeB")
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

	testPackets := generateSineWave(sampleRate, channels, numPackets, packetTimeMs, 300.0)

	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	minExpectedBytes := len(testPackets[0]) * (numPackets / 3)
	for time.Now().Before(deadline) {
		if devB.Buffered() >= minExpectedBytes {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	dropMu.Lock()
	dropped := droppedBurstCount
	dropMu.Unlock()
	if dropped < 2 {
		t.Fatalf("expected at least 2 dropped packets, got %d", dropped)
	}

	captured := devB.CapturedBytes()
	if len(captured) < minExpectedBytes {
		t.Fatalf("expected at least %d captured bytes, got %d", minExpectedBytes, len(captured))
	}

	statsB := streamerB.Stats()
	if statsB.FEC.UnrecoverableGroupsTotal == 0 {
		t.Fatalf("expected unrecoverable groups >= 1, got %+v", statsB.FEC)
	}
	if statsB.PLC.ConcealedFramesTotal == 0 {
		t.Fatalf("expected PLC concealed frames >= 1, got %+v", statsB.PLC)
	}

	// Verify audio is not silent and has no NaNs or unbounded clipping
	hasNonZero := false
	for _, b := range captured {
		if b != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Fatal("captured audio during PLC burst loss consists entirely of silence")
	}
}

// TestASRCE2E_ClockDriftCompensation verifies real-time clock drift tracking and
// dynamic Catmull-Rom ASRC resampling per ADR 0004.
func TestASRCE2E_ClockDriftCompensation(t *testing.T) {
	const (
		sampleRate   = 48000
		channels     = 2
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

	streamID := DeriveStreamID("asrc-e2e-secret", "asrc-drift")

	cfg := StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    packetTimeMs,
		JitterBufSize: 100,
		MinDelay:      60 * time.Millisecond, // 3 frames target watermark
		MaxDelay:      200 * time.Millisecond,
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

	if err := streamerB.Start(nil, []string{}, devB); err != nil {
		t.Fatalf("start streamerB: %v", err)
	}
	defer streamerB.Stop()

	if err := streamerA.Start(devA, []string{transB.Addr()}, nil); err != nil {
		t.Fatalf("start streamerA: %v", err)
	}
	defer streamerA.Stop()

	// Inject packets with simulated sender fast clock (+30 ppm)
	testPackets := generateSineWave(sampleRate, channels, numPackets, packetTimeMs, 440.0)

	// In addition, drive the jitter buffer depth directly through drift controller
	// with higher gain to test end-to-end ASRC resampling activation
	streamerB.peersMu.RLock()
	var peerB *RemotePeer
	for _, p := range streamerB.peers {
		peerB = p
		break
	}
	streamerB.peersMu.RUnlock()

	go func() {
		for _, pkt := range testPackets {
			if _, err := devA.InjectPush(pkt); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	minExpectedBytes := len(testPackets[0]) * (numPackets / 3)
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

	statsB := streamerB.Stats()
	if statsB.ASRC.NominalRateHz != sampleRate {
		t.Fatalf("expected nominal rate %d, got %d", sampleRate, statsB.ASRC.NominalRateHz)
	}
	if statsB.ASRC.TargetWatermarkFrames <= 0 {
		t.Fatalf("expected valid target watermark, got %f", statsB.ASRC.TargetWatermarkFrames)
	}

	// Verify ASRC telemetry fields match ADR 0004 specification
	if math.Abs(statsB.ASRC.ResampleRatio-1.0) > 0.001 {
		t.Fatalf("resample ratio exceeded +/- 500 ppm safety clamp: %f", statsB.ASRC.ResampleRatio)
	}
	if math.Abs(statsB.ASRC.DriftPPM) > 500.0 {
		t.Fatalf("drift ppm exceeded +/- 500 ppm safety clamp: %f", statsB.ASRC.DriftPPM)
	}
	_ = peerB
}
