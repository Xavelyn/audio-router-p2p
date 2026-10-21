package streamer

import (
	"errors"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"encoding/binary"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

type failingTransport struct {
	err error
}

func (f failingTransport) Addr() string                      { return "127.0.0.1:0" }
func (f failingTransport) LocalAddr() *net.UDPAddr           { return &net.UDPAddr{IP: net.ParseIP("127.0.0.1")} }
func (f failingTransport) Send(to string, data []byte) error { return f.err }
func (f failingTransport) Receive(h func([]byte, *net.UDPAddr)) func() {
	return func() {}
}
func (f failingTransport) Wait()        {}
func (f failingTransport) Closed() bool { return false }
func (f failingTransport) Close() error { return nil }

// startToneInput feeds a 440 Hz sine wave (loud enough for DTX to stay
// active) into a virtual device at the given sample rate, returning a stop
// function.
func startToneInput(t *testing.T, dev *audio.VirtualAudioDevice, sampleRate int, done *atomic.Bool) {
	t.Helper()
	const chunkFrames = 96
	period := float64(sampleRate) / 440.0
	block := make([]byte, chunkFrames*2)
	phase := 0.0
	feed := func() {
		for i := 0; i < chunkFrames; i++ {
			s := int16(8000 * math.Sin(2*math.Pi*phase/period))
			binary.LittleEndian.PutUint16(block[i*2:], uint16(s))
			phase += 1
			if phase >= period {
				phase -= period
			}
		}
		if _, err := dev.InjectPush(block); err != nil {
			return
		}
	}
	go func() {
		for {
			if done.Load() {
				return
			}
			feed()
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

// TestRouteStatusTracksSendActivity verifies the "why isn't this playing"
// snapshot sees capture activity and per-destination send liveness. The
// destination is a real loopback listener: some VMs reject UDP sends to
// TEST-NET addresses, which would make every send fail.
func TestRouteStatusTracksSendActivity(t *testing.T) {
	udp, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	sink, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.Receive(func([]byte, *net.UDPAddr) {})

	cfg := DefaultStreamConfig(48000, 1)
	s, err := NewStreamer(udp, nil, cfg, "sender")
	if err != nil {
		t.Fatal(err)
	}
	in := audio.NewVirtualAudioDevice()
	defer in.Close()
	var done atomic.Bool
	defer done.Store(true)
	startToneInput(t, in, 48000, &done)

	if err := s.Start(in, []string{sink.Addr()}, nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.RouteStatus()
		if !st.LastInput.IsZero() && len(st.Peers) == 1 && !st.Peers[0].LastSend.IsZero() {
			if st.Peers[0].PacketsSent == 0 {
				t.Fatal("peer has LastSend but zero packets sent")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("send activity not recorded: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRouteStatusRecordsSendErrors verifies a failing transport shows up as a
// send error in the route status instead of silently dropping audio.
func TestRouteStatusRecordsSendErrors(t *testing.T) {
	cfg := DefaultStreamConfig(48000, 1)
	s, err := NewStreamer(failingTransport{err: errors.New("boom: no route to host")}, nil, cfg, "sender")
	if err != nil {
		t.Fatal(err)
	}
	in := audio.NewVirtualAudioDevice()
	defer in.Close()
	var done atomic.Bool
	defer done.Store(true)
	startToneInput(t, in, 48000, &done)

	if err := s.Start(in, []string{"192.0.2.1:45678"}, nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.RouteStatus()
		if len(st.Peers) == 1 && st.Peers[0].LastSendError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("send error not recorded: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
