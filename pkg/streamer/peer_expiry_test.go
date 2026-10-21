package streamer

import (
	"net"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

func TestReceivePeerExpiresAndRestartsSequence(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 1)
	cfg.PeerIdleTimeout = 50 * time.Millisecond
	s, err := NewStreamer(nil, nil, cfg, "receiver")
	if err != nil {
		t.Fatal(err)
	}
	out := audio.NewVirtualAudioDevice()
	defer out.Close()
	s.output = out
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 41000}
	packet := func(seq uint32) []byte {
		return transport.Marshal(transport.PacketHeader{
			StreamID: s.StreamID(), Seq: seq, Timestamp: uint64(time.Now().UnixNano()),
			SampleRate: 48000, Channels: 1, Flags: transport.FlagActive,
		}, make([]byte, s.packetBytes()))
	}
	s.handlePacket(packet(100), addr)
	s.peersMu.RLock()
	old := s.peers[addr.String()]
	s.peersMu.RUnlock()
	if old == nil || old.Direction != DirRecvOnly {
		t.Fatal("sender not learned")
	}
	s.peersMu.Lock()
	old.lastPacket = time.Now().Add(-time.Second)
	s.peersMu.Unlock()
	// Even before a periodic cleanup, a new packet replaces stale sequence
	// state rather than being rejected as late by the previous jitter buffer.
	s.handlePacket(packet(0), addr)
	s.peersMu.RLock()
	newPeer := s.peers[addr.String()]
	s.peersMu.RUnlock()
	if newPeer == nil || newPeer == old {
		t.Fatal("stale peer not replaced on restart")
	}
	if got := newPeer.JitterBuf.Stats().BufferedFrames; got != 1 {
		t.Fatalf("restarted packet buffered = %d, want 1", got)
	}
	if _, _, err := old.JitterBuf.PopNonBlock(); err != ErrClosed && err != ErrBufferEmpty {
		t.Fatalf("old jitter buffer should be closed: %v", err)
	}
	s.peersMu.Lock()
	newPeer.lastPacket = time.Now().Add(-time.Second)
	s.peersMu.Unlock()
	s.expireIdlePeers(time.Now())
	s.peersMu.RLock()
	_, exists := s.peers[addr.String()]
	s.peersMu.RUnlock()
	if exists {
		t.Fatal("idle peer retained")
	}
}

func TestIdleCleanupPreservesConfiguredDestination(t *testing.T) {
	s, err := NewStreamer(nil, nil, DefaultStreamConfig(48000, 1), "sender")
	if err != nil {
		t.Fatal(err)
	}
	s.peers["127.0.0.2:41000"] = &RemotePeer{
		Addr: "127.0.0.2:41000", Direction: DirSendRecv,
		lastPacket: time.Now().Add(-time.Hour),
	}
	s.expireIdlePeers(time.Now())
	if len(s.peers) != 1 {
		t.Fatal("configured destination was evicted")
	}
}

func TestReceiveLoopExpiresIdlePeer(t *testing.T) {
	udp, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	cfg := DefaultStreamConfig(48000, 1)
	cfg.PeerIdleTimeout = 40 * time.Millisecond
	out := audio.NewVirtualAudioDevice()
	defer out.Close()
	s, err := NewStreamer(udp, nil, cfg, "receiver")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(nil, nil, out); err != nil {
		t.Fatal(err)
	}
	defer func() { s.Stop(); _ = s.Wait() }()
	sender, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	pkt := transport.Marshal(transport.PacketHeader{
		Seq: 5, StreamID: s.StreamID(), Timestamp: uint64(time.Now().UnixNano()),
		SampleRate: 48000, Channels: 1, Flags: transport.FlagActive,
	}, make([]byte, s.packetBytes()))
	if err := sender.Send(udp.Addr(), pkt); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for s.Stats().PacketsReceived == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Stats().PacketsReceived == 0 {
		t.Fatal("packet not received")
	}
	for len(s.StreamLinkStats()) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.StreamLinkStats()) != 0 {
		t.Fatal("receive loop did not evict idle peer")
	}
}
