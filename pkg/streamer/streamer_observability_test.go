package streamer

import (
	"bytes"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

type failingAudioStream struct {
	audio.AudioStream
}

func (f *failingAudioStream) Write(p []byte) (int, error) {
	return 0, errors.New("simulated hardware write fault")
}

func TestStreamerRateLimitingHelper(t *testing.T) {
	var last atomic.Int64
	interval := int64(50 * time.Millisecond)

	// First evaluation must immediately trigger
	if !AtomicShouldLogRateLimited(&last, interval) {
		t.Fatal("expected first call to trigger immediately")
	}

	// Repeated evaluations within interval must be suppressed
	for i := 0; i < 10; i++ {
		if AtomicShouldLogRateLimited(&last, interval) {
			t.Fatalf("call %d was not suppressed", i)
		}
	}

	// After elapsed interval, exactly one CAS succeeds
	time.Sleep(60 * time.Millisecond)
	if !AtomicShouldLogRateLimited(&last, interval) {
		t.Fatal("expected call after interval to trigger")
	}
	if AtomicShouldLogRateLimited(&last, interval) {
		t.Fatal("subsequent call should be suppressed again")
	}
}

func TestStreamerDemuxDropObservability(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer failed: %v", err)
	}

	expectedSID := DeriveStreamID("secret", "route-1")
	s.SetStreamID(expectedSID)

	dummyAddr, _ := net.ResolveUDPAddr("udp", "10.1.11.11:58432")

	// 1. Unmarshal error drop
	corruptPacket := []byte{0x00, 0x01, 0x02} // truncated, invalid packet
	s.handlePacket(corruptPacket, dummyAddr)

	if s.DemuxDropsUnmarshal() != 1 {
		t.Errorf("expected DemuxDropsUnmarshal == 1, got %d", s.DemuxDropsUnmarshal())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `[STREAM:DROP] reason="unmarshal_error"`) {
		t.Errorf("expected unmarshal_error log, got: %s", logs)
	}

	// 2. StreamID mismatch drop
	differentSID := DeriveStreamID("secret", "route-different")
	hdr := transport.PacketHeader{
		Seq:        10,
		Timestamp:  uint64(time.Now().UnixNano()),
		SampleRate: 48000,
		Channels:   2,
		Flags:      transport.FlagActive,
		StreamID:   differentSID,
	}
	pcm := make([]byte, 1920)
	mismatchedPkt := transport.Marshal(hdr, pcm)

	s.handlePacket(mismatchedPkt, dummyAddr)
	if s.DemuxDropsStreamID() != 1 {
		t.Errorf("expected DemuxDropsStreamID == 1, got %d", s.DemuxDropsStreamID())
	}
	logs = logBuf.String()
	if !strings.Contains(logs, `[STREAM:DROP] reason="stream_id_mismatch"`) {
		t.Errorf("expected stream_id_mismatch log, got: %s", logs)
	}

	// 3. nil local output drop when dynamic peer arrives
	// When streamID matches but streamer has nil output:
	hdr.StreamID = expectedSID
	matchedPkt := transport.Marshal(hdr, pcm)
	s.handlePacket(matchedPkt, dummyAddr)
	if s.DemuxDropsNilOutput() != 1 {
		t.Errorf("expected DemuxDropsNilOutput == 1, got %d", s.DemuxDropsNilOutput())
	}

	logs = logBuf.String()
	if !strings.Contains(logs, `[STREAM:DROP] reason="nil_local_output"`) {
		t.Errorf("expected nil_local_output log, got: %s", logs)
	}
}

func TestStreamerDynamicPeerRegistrationAndFirstPacketAccept(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer failed: %v", err)
	}

	sid := DeriveStreamID("secret", "route-ingress")
	s.SetStreamID(sid)

	out, err := audio.OpenOutput(0, 48000, 2, true)
	if err != nil {
		t.Fatalf("OpenOutput failed: %v", err)
	}
	defer out.Close()

	if err := s.Start(nil, nil, out); err != nil {
		t.Fatalf("Streamer start failed: %v", err)
	}
	defer s.Stop()

	dummyAddr, _ := net.ResolveUDPAddr("udp", "10.1.11.11:58432")
	hdr := transport.PacketHeader{
		Seq:        1,
		Timestamp:  uint64(time.Now().UnixNano()),
		SampleRate: 48000,
		Channels:   2,
		Flags:      transport.FlagActive,
		StreamID:   sid,
	}
	pcm := make([]byte, s.packetBytes())
	pkt := transport.Marshal(hdr, pcm)

	s.handlePacket(pkt, dummyAddr)

	logs := logBuf.String()
	if !strings.Contains(logs, "[STREAM:RECV] dynamic peer registered: addr=10.1.11.11:58432") {
		t.Errorf("expected dynamic peer registered log, got: %s", logs)
	}
	if !strings.Contains(logs, "[STREAM:RECV] first packet accepted") {
		t.Errorf("expected first packet accepted log, got: %s", logs)
	}
}

func TestStreamerPlayoutObservability(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer failed: %v", err)
	}

	sid := DeriveStreamID("secret", "route-playout")
	s.SetStreamID(sid)

	failOut := &failingAudioStream{}
	peerAddr := "10.1.10.160:45678"
	jb := NewJitterBuffer(s.packetBytes(), 48000, 2, 50)
	peer := &RemotePeer{
		Addr:      peerAddr,
		StreamID:  sid,
		Output:    failOut,
		JitterBuf: jb,
	}

	s.peersMu.Lock()
	s.peers[peerAddr] = peer
	s.peersMu.Unlock()

	// 1. Playout with empty jitter buffer should trigger underrun observability
	s.playout()

	if peer.TotalUnderruns() != 1 {
		t.Errorf("expected 1 underrun, got %d", peer.TotalUnderruns())
	}
	if peer.ConsecutiveUnderruns() != 1 {
		t.Errorf("expected 1 consecutive underrun, got %d", peer.ConsecutiveUnderruns())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[AUDIO:UNDERRUN]") {
		t.Errorf("expected [AUDIO:UNDERRUN] log, got: %s", logs)
	}

	// 2. Push frames and trigger 50-frame playout heartbeat and write error log
	for i := uint32(0); i < 50; i++ {
		pcm := make([]byte, s.packetBytes())
		jb.Push(i, int64(time.Now().UnixNano()), pcm)
		s.playout()
	}

	logs = logBuf.String()
	if !strings.Contains(logs, "[AUDIO:PLAY]") {
		t.Errorf("expected [AUDIO:PLAY] heartbeat log, got: %s", logs)
	}
	if !strings.Contains(logs, "[AUDIO:PLAY] ERROR: output.Write failed: simulated hardware write fault") {
		t.Errorf("expected output.Write error log, got: %s", logs)
	}
}

type noopAudioStream struct{}

func (n *noopAudioStream) Read(p []byte) (int, error)  { return len(p), nil }
func (n *noopAudioStream) Write(p []byte) (int, error) { return len(p), nil }
func (n *noopAudioStream) Close() error                { return nil }

func TestStreamerZeroAllocPlayoutNominalPath(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer failed: %v", err)
	}

	sid := DeriveStreamID("secret", "route-zeroalloc")
	s.SetStreamID(sid)

	out := &noopAudioStream{}
	peerAddr := "10.1.10.160:45678"
	jb := NewJitterBuffer(s.packetBytes(), 48000, 2, 500)
	peer := &RemotePeer{
		Addr:      peerAddr,
		StreamID:  sid,
		Output:    out,
		JitterBuf: jb,
	}

	s.peersMu.Lock()
	s.peers[peerAddr] = peer
	s.peersMu.Unlock()

	// Pre-fill jitter buffer with plenty of packets so it never underruns
	pcm := make([]byte, s.packetBytes())
	for i := uint32(0); i < 200; i++ {
		jb.Push(i, int64(time.Now().UnixNano()), pcm)
	}

	// Warm up
	s.playout()

	// Test allocation count on nominal frame (not frame%50 heartbeat frame)
	peer.playoutFrames.Store(1) // frames 2..49 are nominal
	allocs := testing.AllocsPerRun(20, func() {
		// Ensure jitter buffer has data
		jb.Push(1000, int64(time.Now().UnixNano()), pcm)
		s.playout()
	})

	if allocs > 0 {
		t.Errorf("expected 0 allocs on nominal playout path, got %f allocs/op", allocs)
	}
}
