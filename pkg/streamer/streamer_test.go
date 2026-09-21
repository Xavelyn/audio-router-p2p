package streamer

import (
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

// TestStreamerRemoteStillServed verifies a mixed destination list (one local,
// one remote) still delivers to the remote peer (regression for the old
// all-or-nothing bypass).
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
		if len(data) != 2 {
			t.Fatalf("frame size mismatch: got %d", len(data))
		}
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