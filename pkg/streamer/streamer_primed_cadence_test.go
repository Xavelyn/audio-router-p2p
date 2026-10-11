package streamer

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

type capturingAudioStream struct {
	mu     sync.Mutex
	writes [][]byte
}

func (c *capturingAudioStream) Read(p []byte) (int, error) {
	return 0, nil
}

func (c *capturingAudioStream) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := make([]byte, len(p))
	copy(copied, p)
	c.writes = append(c.writes, copied)
	return len(p), nil
}

func (c *capturingAudioStream) Close() error {
	return nil
}

func (c *capturingAudioStream) WriteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

func (c *capturingAudioStream) GetWrites() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	copy(out, c.writes)
	return out
}

func TestStreamerPlayout_CadenceInvariantOnUnderrun(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	sid := DeriveStreamID("secret", "cadence-test")
	s.SetStreamID(sid)

	out := &capturingAudioStream{}
	peerAddr := "10.0.0.1:45678"
	jb := NewJitterBuffer(s.packetBytes(), 48000, 2, 50)
	peer := &RemotePeer{
		Addr:      peerAddr,
		StreamID:  sid,
		Output:    out,
		JitterBuf: jb,
	}

	s.peersMu.Lock()
	s.peers[peerAddr] = peer
	s.peersMu.Unlock()

	// Playout 10 consecutive ticks with empty jitter buffer (chronic starvation/underrun)
	const numTicks = 10
	for i := 0; i < numTicks; i++ {
		s.playout()
	}

	if out.WriteCount() != numTicks {
		t.Fatalf("cadence broken: expected %d writes to output device, got %d", numTicks, out.WriteCount())
	}

	writes := out.GetWrites()
	expectedFrameBytes := s.packetBytes()
	for i, w := range writes {
		if len(w) != expectedFrameBytes {
			t.Fatalf("write %d length mismatch: expected %d, got %d", i, expectedFrameBytes, len(w))
		}
		// Invariant: underrun must feed pre-allocated comfort silence (all zeros)
		for j, b := range w {
			if b != 0 {
				t.Fatalf("write %d sample %d non-zero: got %d, expected silence", i, j, b)
			}
		}
	}

	if peer.TotalUnderruns() != numTicks {
		t.Fatalf("expected %d total underruns, got %d", numTicks, peer.TotalUnderruns())
	}
	if peer.ConsecutiveUnderruns() != numTicks {
		t.Fatalf("expected %d consecutive underruns, got %d", numTicks, peer.ConsecutiveUnderruns())
	}
}

func TestStreamerPlayout_CadenceZeroAllocOnUnderrun(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	sid := DeriveStreamID("secret", "zeroalloc-cadence")
	s.SetStreamID(sid)

	out := &noopAudioStream{}
	peerAddr := "10.0.0.1:45678"
	jb := NewJitterBuffer(s.packetBytes(), 48000, 2, 50)
	peer := &RemotePeer{
		Addr:      peerAddr,
		StreamID:  sid,
		Output:    out,
		JitterBuf: jb,
	}

	s.peersMu.Lock()
	s.peers[peerAddr] = peer
	s.peersMu.Unlock()

	// Warmup
	s.playout()

	// Keep frame count away from frame%50 logging
	peer.playoutFrames.Store(1)

	allocs := testing.AllocsPerRun(50, func() {
		s.playout()
	})

	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op during underrun comfort silence playout, got %v", allocs)
	}
}

func TestStreamerPlayout_TransitionFromSilenceToAudio(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 2)
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, cfg, "local-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	sid := DeriveStreamID("secret", "silence-to-audio")
	s.SetStreamID(sid)

	out := &capturingAudioStream{}
	peerAddr := "10.0.0.1:45678"
	jb := NewJitterBuffer(s.packetBytes(), 48000, 2, 50)
	peer := &RemotePeer{
		Addr:      peerAddr,
		StreamID:  sid,
		Output:    out,
		JitterBuf: jb,
	}

	s.peersMu.Lock()
	s.peers[peerAddr] = peer
	s.peersMu.Unlock()

	// 1. Tick during underrun -> writes silence
	s.playout()
	if out.WriteCount() != 1 {
		t.Fatalf("expected 1 write, got %d", out.WriteCount())
	}
	if peer.ConsecutiveUnderruns() != 1 {
		t.Fatalf("expected 1 consecutive underrun, got %d", peer.ConsecutiveUnderruns())
	}

	// 2. Incoming audio arrives
	realAudio := bytes.Repeat([]byte{0x42}, s.packetBytes())
	jb.Push(0, int64(time.Now().UnixNano()), realAudio)

	// Playout should pop real audio and reset consecutive underruns to 0
	s.playout()
	if out.WriteCount() != 2 {
		t.Fatalf("expected 2 writes, got %d", out.WriteCount())
	}
	if peer.ConsecutiveUnderruns() != 0 {
		t.Fatalf("expected consecutive underruns reset to 0, got %d", peer.ConsecutiveUnderruns())
	}

	writes := out.GetWrites()
	if !bytes.Equal(writes[1], realAudio) {
		t.Fatalf("expected write 2 to be real audio, got mismatch")
	}
}
