package streamer

import (
	"bytes"
	"math"
	"net"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

// Test that when a destination address matches a local address, the streamer
// bypasses the network and writes directly to the provided output stream.
func TestStreamerLocalBypass(t *testing.T) {
	in, err := audio.OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	defer in.Close()
	out, err := audio.OpenOutput(1, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenOutput error: %v", err)
	}
	defer out.Close()

	s, err := NewStreamer(nil, []string{"127.0.0.1"}, DefaultStreamConfig(44100, 1), "test-node")
	if err != nil {
		t.Fatalf("NewStreamer error: %v", err)
	}

	dests := []string{"127.0.0.1:12345"}
	if err := s.Start(in, dests, out); err != nil {
		t.Fatalf("Streamer start error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for audio.OutputBufferSize(out) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("expected data in output buffer, got 0")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.Stop()
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
}

// Test that the streamer correctly packetizes audio and sends it over UDP.
func TestStreamerNetworkSend(t *testing.T) {
	udp, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp transport create: %v", err)
	}
	defer udp.Close()

	pktCh := make(chan []byte, 1)
	udp.Receive(func(data []byte, _ *net.UDPAddr) {
		select {
		case pktCh <- data:
		default:
		}
	})

	in, err := audio.OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	defer in.Close()

	// No local IPs, so the destination is treated as remote.
	s, err := NewStreamer(udp, []string{}, DefaultStreamConfig(44100, 1), "test-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	dest := []string{udp.Addr()}
	if err := s.Start(in, dest, nil); err != nil {
		t.Fatalf("Streamer start: %v", err)
	}

	var data []byte
	select {
	case data = <-pktCh:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive UDP packet")
	}

	hdr, payload, err := transport.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if hdr.SampleRate != 44100 || hdr.Channels != 1 {
		t.Fatalf("unexpected format in header: sr=%d ch=%d", hdr.SampleRate, hdr.Channels)
	}
	if len(payload) == 0 {
		t.Fatalf("empty payload")
	}

	s.Stop()
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
}

// TestStreamerSharedTransport verifies that multiple streamers (routes) can
// share a single transport socket, and that stopping one route does not tear
// down the shared socket or disrupt the other route. This is the regression
// test for the old behavior where Stop() closed the shared transport.
func TestStreamerSharedTransport(t *testing.T) {
	udp, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp transport create: %v", err)
	}
	defer udp.Close()

	in1, err := audio.OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput 1 error: %v", err)
	}
	defer in1.Close()
	in2, err := audio.OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput 2 error: %v", err)
	}
	defer in2.Close()

	s1, err := NewStreamer(udp, nil, DefaultStreamConfig(44100, 1), "node")
	if err != nil {
		t.Fatalf("NewStreamer s1: %v", err)
	}
	s2, err := NewStreamer(udp, nil, DefaultStreamConfig(44100, 1), "node")
	if err != nil {
		t.Fatalf("NewStreamer s2: %v", err)
	}

	// Observer handler: filter self-sent packets by stream ID so we can tell
	// each route's traffic apart. Registered before any streamer starts so no
	// early packets are missed.
	got1 := make(chan struct{}, 256)
	got2 := make(chan struct{}, 256)
	udp.Receive(func(data []byte, _ *net.UDPAddr) {
		hdr, _, err := transport.Unmarshal(data)
		if err != nil {
			return
		}
		switch hdr.StreamID {
		case s1.StreamID():
			got1 <- struct{}{}
		case s2.StreamID():
			got2 <- struct{}{}
		}
	})

	dest := []string{udp.Addr()}
	if err := s1.Start(in1, dest, nil); err != nil {
		t.Fatalf("s1 start: %v", err)
	}
	if err := s2.Start(in2, dest, nil); err != nil {
		t.Fatalf("s2 start: %v", err)
	}

	waitFor := func(name string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no packet observed", name)
		}
	}

	// Both routes deliver concurrently over the one shared socket.
	waitFor("route1", got1)
	waitFor("route2", got2)

	// Stop route 1: must not close the shared transport.
	s1.Stop()
	if err := s1.Wait(); err != nil {
		t.Fatalf("s1 wait: %v", err)
	}
	if udp.Closed() {
		t.Fatal("shared transport was closed by stopping one route")
	}

	// Drain any in-flight route-2 packets, then confirm route 2 keeps streaming.
	for len(got2) > 0 {
		<-got2
	}
	waitFor("route2-after-stop", got2)

	s2.Stop()
	if err := s2.Wait(); err != nil {
		t.Fatalf("s2 wait: %v", err)
	}
	if udp.Closed() {
		t.Fatal("shared transport was closed by stopping the last route")
	}
}
func TestStreamerRemoteStillServed(t *testing.T) {
	// Bind on all interfaces so a 127.0.0.2-addressed remote still arrives.
	udp, err := transport.NewUDPTransport(":0")
	if err != nil {
		t.Fatalf("udp transport create: %v", err)
	}
	defer udp.Close()

	pktCh := make(chan []byte, 1)
	udp.Receive(func(data []byte, _ *net.UDPAddr) {
		select {
		case pktCh <- data:
		default:
		}
	})

	in, err := audio.OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	defer in.Close()
	out, err := audio.OpenOutput(1, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenOutput error: %v", err)
	}
	defer out.Close()

	s, err := NewStreamer(udp, []string{"127.0.0.1"}, DefaultStreamConfig(44100, 1), "test-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	// 127.0.0.1 is treated as local (bypass); 127.0.0.2 is loopback-routed but
	// not in the local set, so it must still go over the network to udp.
	_, port, err := net.SplitHostPort(udp.Addr())
	if err != nil {
		t.Fatalf("parse udp addr: %v", err)
	}
	dests := []string{"127.0.0.1:9", "127.0.0.2:" + port}
	if err := s.Start(in, dests, out); err != nil {
		t.Fatalf("Streamer start: %v", err)
	}

	select {
	case data := <-pktCh:
		if hdr, _, err := transport.Unmarshal(data); err != nil || hdr.SampleRate != 44100 {
			t.Fatalf("bad remote packet: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote peer did not receive packets")
	}

	s.Stop()
	_ = s.Wait()
}

// TestStreamerStopsOnEOF verifies the loop terminates when the input is closed.
func TestStreamerStopsOnEOF(t *testing.T) {
	in, err := audio.OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	out, err := audio.OpenOutput(1, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenOutput error: %v", err)
	}

	s, err := NewStreamer(nil, []string{"127.0.0.1"}, DefaultStreamConfig(44100, 1), "test-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}
	if err := s.Start(in, []string{"127.0.0.1:12345"}, out); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_ = in.Close()
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamer did not stop after input EOF")
	}
	_ = out.Close()
}

// TestJitterBuffer tests the jitter buffer independently
func TestJitterBuffer(t *testing.T) {
	jb := NewJitterBuffer(2, 44100, 1, 10)
	defer jb.Close()

	// Push some frames
	baseTime := time.Now().UnixNano()
	for i := uint32(0); i < 5; i++ {
		data := []byte{byte(i), byte(i >> 8)}
		if _, err := jb.Push(i, baseTime+int64(i*20000000), data); err != nil { // 20ms apart
			t.Fatalf("Push error: %v", err)
		}
	}

	// Pop them back - use Pop which blocks until playout time
	for i := uint32(0); i < 5; i++ {
		data, seq, err := jb.Pop()
		if err != nil {
			t.Fatalf("Pop error: %v", err)
		}
		if seq != i {
			t.Fatalf("sequence mismatch: got %d want %d", seq, i)
		}
		want := []byte{byte(i), byte(i >> 8)}
		if !bytes.Equal(data, want) {
			t.Fatalf("content mismatch for seq %d: got %v want %v", seq, data, want)
		}
	}

	// Buffer is now empty; PopNonBlock must report ErrBufferEmpty until the
	// next packet is due.
	if _, _, err := jb.PopNonBlock(); err != ErrBufferEmpty {
		t.Fatalf("expected ErrBufferEmpty, got %v", err)
	}
}

// TestJitterBufferOutOfOrder verifies that packets arriving out of order are
// played back in sequence order.
func TestJitterBufferOutOfOrder(t *testing.T) {
	jb := NewJitterBuffer(2, 44100, 1, 10)
	defer jb.Close()

	baseTime := time.Now().UnixNano()
	// 0 arrives first (anchors the stream), then 2, then 1 (out of order).
	for _, seq := range []uint32{0, 2, 1} {
		data := []byte{byte(seq), 0xFF}
		if _, err := jb.Push(seq, baseTime, data); err != nil {
			t.Fatalf("Push(%d) error: %v", seq, err)
		}
	}

	for want := uint32(0); want < 3; want++ {
		data, seq, err := jb.Pop()
		if err != nil {
			t.Fatalf("Pop error: %v", err)
		}
		if seq != want {
			t.Fatalf("sequence mismatch: got %d want %d", seq, want)
		}
		if !bytes.Equal(data, []byte{byte(want), 0xFF}) {
			t.Fatalf("content mismatch for seq %d: got %v", want, data)
		}
	}
}

// TestJitterBufferGapFill verifies that a missing sequence number is emitted as
// silence once its playout time passes, and later out-of-order arrival does not
// disturb the playout cursor.
func TestJitterBufferGapFill(t *testing.T) {
	jb := NewJitterBuffer(2, 44100, 1, 10)
	defer jb.Close()

	baseTime := time.Now().UnixNano()
	// seq 0 and seq 2 arrive; seq 1 is lost on the network.
	if _, err := jb.Push(0, baseTime, []byte{0xAA, 0xAA}); err != nil {
		t.Fatalf("Push(0) error: %v", err)
	}
	if _, err := jb.Push(2, baseTime, []byte{0xCC, 0xCC}); err != nil {
		t.Fatalf("Push(2) error: %v", err)
	}

	// seq 0 is available immediately.
	data, seq, err := jb.Pop()
	if err != nil {
		t.Fatalf("Pop(0) error: %v", err)
	}
	if seq != 0 || !bytes.Equal(data, []byte{0xAA, 0xAA}) {
		t.Fatalf("unexpected seq 0 pop: seq=%d data=%v", seq, data)
	}

	// seq 1 is missing: Pop blocks until its playout time, then emits silence.
	data, seq, err = jb.Pop()
	if err != nil {
		t.Fatalf("Pop(1) error: %v", err)
	}
	if seq != 1 {
		t.Fatalf("sequence mismatch: got %d want 1", seq)
	}
	for _, b := range data {
		if b != 0 {
			t.Fatalf("expected silence for seq 1, got %v", data)
		}
	}
	if jb.underruns != 1 {
		t.Fatalf("expected 1 underrun, got %d", jb.underruns)
	}

	// seq 2 plays out next.
	data, seq, err = jb.Pop()
	if err != nil {
		t.Fatalf("Pop(2) error: %v", err)
	}
	if seq != 2 || !bytes.Equal(data, []byte{0xCC, 0xCC}) {
		t.Fatalf("unexpected seq 2 pop: seq=%d data=%v", seq, data)
	}
}

// TestJitterBufferWraparound verifies the ring indexing survives uint32
// sequence number wraparound.
func TestJitterBufferWraparound(t *testing.T) {
	jb := NewJitterBuffer(2, 44100, 1, 10)
	defer jb.Close()

	baseTime := time.Now().UnixNano()
	for _, seq := range []uint32{math.MaxUint32 - 1, math.MaxUint32, 0, 1} {
		data := []byte{byte(seq), byte(seq >> 8)}
		if _, err := jb.Push(seq, baseTime, data); err != nil {
			t.Fatalf("Push(%d) error: %v", seq, err)
		}
	}

	want := []uint32{math.MaxUint32 - 1, math.MaxUint32, 0, 1}
	for _, w := range want {
		data, seq, err := jb.Pop()
		if err != nil {
			t.Fatalf("Pop error: %v", err)
		}
		if seq != w {
			t.Fatalf("sequence mismatch: got %d want %d", seq, w)
		}
		if !bytes.Equal(data, []byte{byte(w), byte(w >> 8)}) {
			t.Fatalf("content mismatch for seq %d: got %v", w, data)
		}
	}
}

func TestJitterBuffer_LatePackets(t *testing.T) {
	jb := NewJitterBuffer(2, 44100, 1, 10)
	defer jb.Close()

	baseTime := time.Now().UnixNano()
	// Push seq 10 to anchor
	if _, err := jb.Push(10, baseTime, []byte{0x10, 0x00}); err != nil {
		t.Fatalf("Push(10) error: %v", err)
	}

	// Pop seq 10, advancing nextSeq to 11
	data, seq, err := jb.Pop()
	if err != nil || seq != 10 {
		t.Fatalf("Pop(10) failed: seq=%d err=%v", seq, err)
	}
	if !bytes.Equal(data, []byte{0x10, 0x00}) {
		t.Fatalf("data mismatch")
	}

	// Now push seq 9 (distFromNext = 9 - 11 = -2 < 0): must be treated as late packet
	if _, err := jb.Push(9, baseTime, []byte{0x09, 0x00}); err != nil {
		t.Fatalf("Push(9) error: %v", err)
	}

	stats := jb.Stats()
	if stats.LatePackets != 1 {
		t.Fatalf("expected 1 late packet, got %d", stats.LatePackets)
	}

	// Buffer must not pop seq 9
	if _, _, err := jb.PopNonBlock(); err != ErrBufferEmpty {
		t.Fatalf("expected ErrBufferEmpty, got %v", err)
	}
}

func BenchmarkJitterBuffer_Push(b *testing.B) {
	jb := NewJitterBuffer(960*4, 48000, 2, 200)
	defer jb.Close()

	payload := make([]byte, 960*4)
	ts := time.Now().UnixNano()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		jb.Push(uint32(i), ts, payload)
	}
}

// TestResampler tests the resampler
func TestResampler(t *testing.T) {
	// 44100 -> 48000
	r := NewResampler(44100, 48000, 1)
	if r == nil {
		t.Fatal("expected resampler")
	}

	// Generate 100ms of 440Hz sine at 44100
	inFrames := 4410
	inData := make([]byte, inFrames*2)
	for i := 0; i < inFrames; i++ {
		val := int16(32767 * math.Sin(2*math.Pi*440*float64(i)/44100))
		inData[i*2] = byte(val)
		inData[i*2+1] = byte(val >> 8)
	}

	outData, consumed := r.Resample(inData)
	if consumed != inFrames {
		t.Fatalf("consumed %d frames, expected %d", consumed, inFrames)
	}
	// Expect ~4800 frames (48000 * 0.1)
	if len(outData)/2 < 4700 || len(outData)/2 > 4900 {
		t.Fatalf("unexpected output frames: %d", len(outData)/2)
	}
}

func TestDeriveStreamID(t *testing.T) {
	id1 := DeriveStreamID("secret-key", "room-123")
	id2 := DeriveStreamID("secret-key", "room-123")
	id3 := DeriveStreamID("different-secret", "room-123")
	id4 := DeriveStreamID("", "room-123")

	var empty [16]byte
	if id1 == empty {
		t.Fatal("DeriveStreamID returned empty stream ID")
	}
	if id1 != id2 {
		t.Fatal("DeriveStreamID is not deterministic for same inputs")
	}
	if id1 == id3 {
		t.Fatal("DeriveStreamID should differ for different secrets")
	}
	if id4 == empty {
		t.Fatal("DeriveStreamID without secret should return valid SHA256 prefix")
	}
}

func TestStreamerSetStreamIDAndStats(t *testing.T) {
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, DefaultStreamConfig(44100, 1), "test-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	customID := DeriveStreamID("test-secret", "custom-stream")
	s.SetStreamID(customID)
	if s.StreamID() != customID {
		t.Fatalf("StreamID mismatch: got %v want %v", s.StreamID(), customID)
	}

	stats := s.Stats()
	if stats.StreamID == "" {
		t.Fatal("Stats should report non-empty StreamID")
	}
}

func TestStreamLinkStats_AtomicCollection(t *testing.T) {
	s, err := NewStreamer(nil, []string{"127.0.0.1"}, DefaultStreamConfig(44100, 1), "test-node")
	if err != nil {
		t.Fatalf("NewStreamer: %v", err)
	}

	peerAddr := "192.168.1.100:45400"
	jb := NewJitterBuffer(256, 44100, 1, 100)
	defer jb.Close()

	s.peersMu.Lock()
	p := &RemotePeer{
		Addr:      peerAddr,
		PeerID:    "node-remote-1",
		JitterBuf: jb,
	}
	p.packetsSent.Store(120)
	p.packetsRecv.Store(118)
	p.packetsLost.Store(2)
	p.fecRecovered.Store(1)
	s.peers[peerAddr] = p
	s.peersMu.Unlock()

	linkStats := s.StreamLinkStats()
	if len(linkStats) != 1 {
		t.Fatalf("expected 1 stream link stat, got %d", len(linkStats))
	}

	ls := linkStats[0]
	if ls.PeerID != "node-remote-1" {
		t.Errorf("expected peer ID node-remote-1, got %s", ls.PeerID)
	}
	if ls.PacketsSent != 120 || ls.PacketsReceived != 118 || ls.PacketsLost != 2 {
		t.Errorf("unexpected packet counts: %+v", ls)
	}
	if ls.FECRecovered != 1 {
		t.Errorf("expected 1 fec recovered, got %d", ls.FECRecovered)
	}
}
