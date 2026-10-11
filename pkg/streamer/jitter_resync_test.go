package streamer

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

// TestJitterBuffer_StarvationRecovery verifies ADR 0033:
// Stream active audio -> simulate pause / underruns where PopNonBlock advances nextSeq ->
// resume audio with older sequence -> assert packet is accepted, no drop, audio plays out immediately.
func TestJitterBuffer_StarvationRecovery(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	// Short target delay so playout deadlines occur quickly in tests
	jb.minDelay = 20 * time.Millisecond
	jb.targetDelay = 20 * time.Millisecond

	baseTime := time.Now().UnixNano()

	// 1. Stream active audio: push seq 10 and 11
	if _, err := jb.PushWithFlags(10, baseTime, []byte{1, 2, 3, 4}, transport.FlagActive); err != nil {
		t.Fatalf("Push(10) error: %v", err)
	}
	if _, err := jb.PushWithFlags(11, baseTime, []byte{5, 6, 7, 8}, transport.FlagActive); err != nil {
		t.Fatalf("Push(11) error: %v", err)
	}

	// Wait for playout readiness and pop seq 10 and 11
	time.Sleep(25 * time.Millisecond)

	data, seq, err := jb.PopNonBlock()
	if err != nil || seq != 10 {
		t.Fatalf("expected pop seq 10, got seq=%d err=%v", seq, err)
	}
	if !bytes.Equal(data, []byte{1, 2, 3, 4}) {
		t.Fatalf("unexpected data for seq 10: %v", data)
	}

	data, seq, err = jb.PopNonBlock()
	if err != nil || seq != 11 {
		t.Fatalf("expected pop seq 11, got seq=%d err=%v", seq, err)
	}
	if !bytes.Equal(data, []byte{5, 6, 7, 8}) {
		t.Fatalf("unexpected data for seq 11: %v", data)
	}

	// Buffer is now starved (buffered == 0), next expected is seq 12
	// 2. Simulate pause / starvation: wait for playout clock ticks and call PopNonBlock
	time.Sleep(45 * time.Millisecond)
	silence1, sSeq1, err := jb.PopNonBlock()
	if err != nil || sSeq1 != 12 {
		t.Fatalf("expected underrun advance at seq 12, got seq=%d err=%v", sSeq1, err)
	}
	if len(silence1) != 4 {
		t.Fatalf("expected 4 bytes silence, got %d", len(silence1))
	}

	time.Sleep(25 * time.Millisecond)
	silence2, sSeq2, err := jb.PopNonBlock()
	if err != nil || sSeq2 != 13 {
		t.Fatalf("expected underrun advance at seq 13, got seq=%d err=%v", sSeq2, err)
	}
	if len(silence2) != 4 {
		t.Fatalf("expected 4 bytes silence, got %d", len(silence2))
	}

	// Playout cursor has run away to nextSeq = 14 with consecutiveUnderruns == 2
	jb.mu.Lock()
	nextSeq := jb.nextSeq
	consec := jb.consecutiveUnderruns
	jb.mu.Unlock()
	if nextSeq != 14 {
		t.Fatalf("expected nextSeq 14, got %d", nextSeq)
	}
	if consec < 2 {
		t.Fatalf("expected consecutiveUnderruns >= 2, got %d", consec)
	}

	// 3. Sender resumes audio at pause sequence S_resume = 12 (distFromNext = 12 - 14 = -2 < 0)
	now := time.Now().UnixNano()
	resumePayload := []byte{9, 10, 11, 12}
	dropped, err := jb.PushWithFlags(12, now, resumePayload, transport.FlagActive)
	if err != nil {
		t.Fatalf("PushWithFlags(12) error on resumption: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 dropped frames, got %d", dropped)
	}

	stats := jb.Stats()
	if stats.LatePackets != 0 {
		t.Fatalf("expected 0 late packets (should not drop on starvation recovery), got %d", stats.LatePackets)
	}

	// 4. Verify re-anchor aligned nextSeq to 12 and audio is ready to play out
	time.Sleep(25 * time.Millisecond)
	popData, popSeq, err := jb.PopNonBlock()
	if err != nil {
		t.Fatalf("PopNonBlock error after resumption: %v", err)
	}
	if popSeq != 12 {
		t.Fatalf("expected popSeq 12, got %d", popSeq)
	}
	if !bytes.Equal(popData, resumePayload) {
		t.Fatalf("expected resumed payload %v, got %v", resumePayload, popData)
	}
}

// TestJitterBuffer_SIDZeroBytePayload verifies ADR 0033:
// Verify PushWithFlags(seq, ts, nil, transport.FlagSID) is accepted and sets dtxMode = true.
func TestJitterBuffer_SIDZeroBytePayload(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	ts := time.Now().UnixNano()

	// 1. Push 0-byte nil payload with FlagSID
	dropped, err := jb.PushWithFlags(100, ts, nil, transport.FlagSID)
	if err != nil {
		t.Fatalf("PushWithFlags with nil SID payload failed: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 drops, got %d", dropped)
	}

	stats := jb.Stats()
	if !stats.DTXMode {
		t.Fatalf("expected DTXMode to be true after FlagSID, got false")
	}

	// 2. Push 0-byte slice with FlagSID
	dropped, err = jb.PushWithFlags(101, ts, []byte{}, transport.FlagSID)
	if err != nil {
		t.Fatalf("PushWithFlags with empty slice SID payload failed: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 drops, got %d", dropped)
	}

	// 3. Push full frame size with FlagSID (also valid)
	dropped, err = jb.PushWithFlags(102, ts, []byte{0, 0, 0, 0}, transport.FlagSID)
	if err != nil {
		t.Fatalf("PushWithFlags with full frame SID payload failed: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 drops, got %d", dropped)
	}

	// 4. Non-SID frames must strictly reject empty or invalid size payloads
	if _, err := jb.PushWithFlags(103, ts, nil, transport.FlagActive); !errors.Is(err, ErrInvalidFrameSize) {
		t.Fatalf("expected ErrInvalidFrameSize pushing nil payload with FlagActive, got %v", err)
	}
	if _, err := jb.PushWithFlags(104, ts, []byte{1, 2}, transport.FlagActive); !errors.Is(err, ErrInvalidFrameSize) {
		t.Fatalf("expected ErrInvalidFrameSize pushing 2-byte payload for 4-byte frame with FlagActive, got %v", err)
	}

	// 5. Exit DTX silence suppression mode with FlagActive
	if _, err := jb.PushWithFlags(105, ts, []byte{10, 20, 30, 40}, transport.FlagActive); err != nil {
		t.Fatalf("PushWithFlags with FlagActive failed: %v", err)
	}
	stats = jb.Stats()
	if stats.DTXMode {
		t.Fatalf("expected DTXMode to be false after FlagActive, got true")
	}
}

// TestStreamer_441kHzSender_ZeroByteSID_Into_48kHzStreamer verifies ADR 0033:
// 44.1kHz sender with 0-byte SID into 48kHz streamer: verify handlePacket handles it without size error and activates dtxMode.
func TestStreamer_441kHzSender_ZeroByteSID_Into_48kHzStreamer(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 2)
	streamer, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "test-node-48k")
	if err != nil {
		t.Fatalf("NewStreamer error: %v", err)
	}

	streamID := DeriveStreamID("test-secret", "test-room-sid")
	streamer.SetStreamID(streamID)

	outDev := audio.NewVirtualAudioDevice()
	defer outDev.Close()

	if err := streamer.Start(nil, []string{}, outDev); err != nil {
		t.Fatalf("streamer.Start error: %v", err)
	}
	defer streamer.Stop()

	// Construct a packet originating from a 44,100 Hz sender with a 0-byte payload carrying FlagSID
	hdr := transport.PacketHeader{
		Seq:        100,
		Timestamp:  uint64(time.Now().UnixNano()),
		SampleRate: 44100,
		Channels:   2,
		Flags:      transport.FlagSID,
		StreamID:   streamID,
	}
	wireBytes := transport.Marshal(hdr, nil)

	senderAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321}
	streamer.handlePacket(wireBytes, senderAddr)

	streamer.peersMu.RLock()
	peer, exists := streamer.peers[senderAddr.String()]
	streamer.peersMu.RUnlock()

	if !exists || peer == nil {
		t.Fatalf("peer %s was not registered", senderAddr.String())
	}
	if peer.JitterBuf == nil {
		t.Fatal("peer JitterBuf is nil")
	}

	stats := peer.JitterBuf.Stats()
	if !stats.DTXMode {
		t.Fatalf("expected peer JitterBuf.Stats().DTXMode == true, got false")
	}
}

// TestJitterBuffer_ActiveStreamingOutOfOrderDrop verifies ADR 0033:
// Legitimate out-of-order straggler while buffer has frames is still dropped as late.
func TestJitterBuffer_ActiveStreamingOutOfOrderDrop(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	baseTime := time.Now().UnixNano()

	// Push seq 100 and seq 102 (seq 101 missing, but buffer has valid frames)
	if _, err := jb.Push(100, baseTime, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Push(100) error: %v", err)
	}
	if _, err := jb.Push(102, baseTime, []byte{5, 6, 7, 8}); err != nil {
		t.Fatalf("Push(102) error: %v", err)
	}

	// Buffer currently holds 2 valid frames
	jb.mu.Lock()
	buffered := 0
	for i := range jb.frames {
		if jb.frames[i].valid {
			buffered++
		}
	}
	jb.mu.Unlock()
	if buffered != 2 {
		t.Fatalf("expected 2 buffered frames, got %d", buffered)
	}

	// Now a straggler packet with seq 98 arrives (distFromNext = 98 - 100 = -2 < 0).
	// Because buffered > 0 and within normal bounds, it must be dropped as late!
	dropped, err := jb.Push(98, baseTime, []byte{9, 10, 11, 12})
	if err != nil {
		t.Fatalf("Push(98) error: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected dropped == 0, got %d", dropped)
	}

	stats := jb.Stats()
	if stats.LatePackets != 1 {
		t.Fatalf("expected 1 late packet, got %d", stats.LatePackets)
	}

	// Slot for seq 98 should not be valid
	idx := int(98 % uint32(jb.capacity))
	jb.mu.Lock()
	isValid := jb.frames[idx].valid
	jb.mu.Unlock()
	if isValid {
		t.Fatal("expected slot for dropped late packet 98 to be invalid")
	}
}

// TestJitterBuffer_TimeGapStarvationRecovery verifies ADR 0033:
// When buffered == 0 and time gap since last packet >= 100ms, playhead re-anchors even if consecutiveUnderruns < 2.
func TestJitterBuffer_TimeGapStarvationRecovery(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	jb.minDelay = 20 * time.Millisecond
	jb.targetDelay = 20 * time.Millisecond

	baseTime := time.Now().UnixNano()
	if _, err := jb.Push(10, baseTime, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Push(10) error: %v", err)
	}

	time.Sleep(25 * time.Millisecond)
	if _, _, err := jb.PopNonBlock(); err != nil {
		t.Fatalf("PopNonBlock error: %v", err)
	}

	// Buffer is empty (buffered == 0). Wait 110ms to exceed 100ms inter-packet gap threshold.
	time.Sleep(110 * time.Millisecond)

	// Arriving packet with seq 9 (distFromNext = 9 - 11 = -2 < 0)
	now := time.Now().UnixNano()
	dropped, err := jb.Push(9, now, []byte{5, 6, 7, 8})
	if err != nil {
		t.Fatalf("Push(9) error: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 drops on time gap re-anchor, got %d", dropped)
	}

	stats := jb.Stats()
	if stats.LatePackets != 0 {
		t.Fatalf("expected 0 late packets, got %d", stats.LatePackets)
	}

	jb.mu.Lock()
	nextSeq := jb.nextSeq
	jb.mu.Unlock()
	if nextSeq != 9 {
		t.Fatalf("expected nextSeq to re-anchor to 9, got %d", nextSeq)
	}
}

// TestJitterBuffer_HeadDriftStarvationRecovery verifies ADR 0033:
// When buffered == 0 and distFromNext <= -5, playhead re-anchors even without consecutiveUnderruns or time gap.
func TestJitterBuffer_HeadDriftStarvationRecovery(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	baseTime := time.Now().UnixNano()
	if _, err := jb.Push(10, baseTime, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Push(10) error: %v", err)
	}

	time.Sleep(25 * time.Millisecond)
	if _, _, err := jb.PopNonBlock(); err != nil {
		t.Fatalf("PopNonBlock error: %v", err)
	}

	// Simulate playout cursor having drifted by setting nextSeq forward by 6 (distFromNext = 11 - 17 = -6 <= -5)
	jb.mu.Lock()
	jb.nextSeq = 17
	jb.mu.Unlock()

	dropped, err := jb.Push(11, time.Now().UnixNano(), []byte{5, 6, 7, 8})
	if err != nil {
		t.Fatalf("Push(11) error: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 drops, got %d", dropped)
	}

	stats := jb.Stats()
	if stats.LatePackets != 0 {
		t.Fatalf("expected 0 late packets on drift recovery, got %d", stats.LatePackets)
	}

	jb.mu.Lock()
	nextSeq := jb.nextSeq
	jb.mu.Unlock()
	if nextSeq != 11 {
		t.Fatalf("expected nextSeq to re-anchor to 11, got %d", nextSeq)
	}
}

// TestJitterBuffer_DiscontinuityReanchor verifies ADR 0033:
// Large sequence jump (|seq - nextSeq| >= capacity) triggers playhead re-anchor.
func TestJitterBuffer_DiscontinuityReanchor(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	baseTime := time.Now().UnixNano()

	// Initialize buffer at seq 10
	if _, err := jb.Push(10, baseTime, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Push(10) error: %v", err)
	}

	// Sudden sender reboot/jump by +1000 (> capacity of 50)
	jumpSeq := uint32(1010)
	jumpPayload := []byte{5, 6, 7, 8}
	dropped, err := jb.Push(jumpSeq, baseTime, jumpPayload)
	if err != nil {
		t.Fatalf("Push(1010) error: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 drops on re-anchor, got %d", dropped)
	}

	jb.mu.Lock()
	reanchoredSeq := jb.nextSeq
	jb.mu.Unlock()
	if reanchoredSeq != jumpSeq {
		t.Fatalf("expected nextSeq to re-anchor to %d, got %d", jumpSeq, reanchoredSeq)
	}
}

// TestJitterBuffer_AncientStragglerDropDuringActiveStreaming verifies that an ancient
// out-of-order packet (distFromNext <= -capacity) arriving while active frames are buffered
// (buffered > 0) is dropped as a late packet and DOES NOT wipe out the active playout stream.
func TestJitterBuffer_AncientStragglerDropDuringActiveStreaming(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	baseTime := time.Now().UnixNano()

	// Push valid active frames seq 100, 101, 102
	if _, err := jb.Push(100, baseTime, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Push(100) error: %v", err)
	}
	if _, err := jb.Push(101, baseTime, []byte{5, 6, 7, 8}); err != nil {
		t.Fatalf("Push(101) error: %v", err)
	}
	if _, err := jb.Push(102, baseTime, []byte{9, 10, 11, 12}); err != nil {
		t.Fatalf("Push(102) error: %v", err)
	}

	if b := jb.Stats().BufferedFrames; b != 3 {
		t.Fatalf("expected 3 buffered frames, got %d", b)
	}

	// An ancient packet from 60 frames ago arrives (distFromNext = 40 - 100 = -60 <= -capacity of 50).
	// Because buffered > 0, it must be dropped as late and MUST NOT re-anchor or destroy queued frames!
	dropped, err := jb.Push(40, baseTime, []byte{99, 99, 99, 99})
	if err != nil {
		t.Fatalf("Push(40) error: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 dropped, got %d", dropped)
	}

	stats := jb.Stats()
	if stats.LatePackets != 1 {
		t.Fatalf("expected ancient packet to be recorded as LatePacket, got %d", stats.LatePackets)
	}

	// Verify head and buffered frames are intact
	jb.mu.Lock()
	nextSeq := jb.nextSeq
	jb.mu.Unlock()
	if nextSeq != 100 {
		t.Fatalf("expected nextSeq to remain 100, got %d", nextSeq)
	}
	if b := jb.Stats().BufferedFrames; b != 3 {
		t.Fatalf("expected 3 buffered frames intact, got %d", b)
	}
}

// TestJitterBuffer_SerialWraparoundStarvationRecovery verifies that RFC 1982 serial
// arithmetic handles starvation recovery across uint32 wraparound boundaries seamlessly.
func TestJitterBuffer_SerialWraparoundStarvationRecovery(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	jb.minDelay = 20 * time.Millisecond
	jb.targetDelay = 20 * time.Millisecond

	baseTime := time.Now().UnixNano()

	// 1. Anchor playhead near max uint32: seq = 0xFFFFFFFD (4294967293)
	startSeq := uint32(0xFFFFFFFD)
	if _, err := jb.PushWithFlags(startSeq, baseTime, []byte{1, 2, 3, 4}, transport.FlagActive); err != nil {
		t.Fatalf("Push startSeq failed: %v", err)
	}

	// Pop the frame once ready
	time.Sleep(25 * time.Millisecond)
	data, seq, err := jb.PopNonBlock()
	if err != nil || seq != startSeq {
		t.Fatalf("expected pop seq=%d, got seq=%d err=%v", startSeq, seq, err)
	}
	if !bytes.Equal(data, []byte{1, 2, 3, 4}) {
		t.Fatalf("unexpected data: %v", data)
	}

	// 2. Starve playout across the 32-bit integer boundary:
	// 4294967294 (startSeq+1), 4294967295 (startSeq+2), 0 (startSeq+3), 1 (startSeq+4)
	time.Sleep(25 * time.Millisecond)
	_, _, _ = jb.PopNonBlock() // 4294967294
	time.Sleep(25 * time.Millisecond)
	_, _, _ = jb.PopNonBlock() // 4294967295
	time.Sleep(25 * time.Millisecond)
	_, sSeq3, _ := jb.PopNonBlock() // 0 (wrapped!)
	time.Sleep(25 * time.Millisecond)
	_, sSeq4, _ := jb.PopNonBlock() // 1

	if sSeq3 != 0 || sSeq4 != 1 {
		t.Fatalf("expected wraparound playout seqs 0 and 1, got sSeq3=%d sSeq4=%d", sSeq3, sSeq4)
	}

	jb.mu.Lock()
	nextSeq := jb.nextSeq
	consec := jb.consecutiveUnderruns
	jb.mu.Unlock()

	if nextSeq != 2 {
		t.Fatalf("expected playhead to have wrapped to nextSeq=2, got %d", nextSeq)
	}
	if consec < 2 {
		t.Fatalf("expected consecutiveUnderruns >= 2, got %d", consec)
	}

	// 3. Sender resumes at sequence 4294967294 (startSeq+1).
	// distFromNext = int32(4294967294 - 2) = int32(-4) = -4 < 0.
	resumeSeq := uint32(0xFFFFFFFE)
	dropped, err := jb.PushWithFlags(resumeSeq, time.Now().UnixNano(), []byte{10, 20, 30, 40}, transport.FlagActive)
	if err != nil {
		t.Fatalf("PushWithFlags on wraparound resumption error: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("expected 0 dropped frames, got %d", dropped)
	}

	stats := jb.Stats()
	if stats.LatePackets != 0 {
		t.Fatalf("expected 0 late packets on wraparound starvation recovery, got %d", stats.LatePackets)
	}

	jb.mu.Lock()
	reanchoredSeq := jb.nextSeq
	jb.mu.Unlock()
	if reanchoredSeq != resumeSeq {
		t.Fatalf("expected nextSeq to re-anchor to %d, got %d", resumeSeq, reanchoredSeq)
	}
}

// TestJitterBuffer_ResetClearsDTXMode verifies that Reset() clears dtxMode.
func TestJitterBuffer_ResetClearsDTXMode(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)
	defer jb.Close()

	ts := time.Now().UnixNano()
	if _, err := jb.PushWithFlags(100, ts, nil, transport.FlagSID); err != nil {
		t.Fatalf("PushWithFlags FlagSID failed: %v", err)
	}

	if !jb.Stats().DTXMode {
		t.Fatal("expected DTXMode == true")
	}

	jb.Reset()

	if jb.Stats().DTXMode {
		t.Fatal("expected DTXMode == false after Reset()")
	}
}
