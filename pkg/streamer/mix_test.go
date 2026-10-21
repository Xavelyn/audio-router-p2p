package streamer

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/audio-router-p2p/pkg/audio"
)

type queuedFrame struct{ data []byte }

func (q *queuedFrame) Push(uint32, int64, []byte) (int, error)                 { return 0, nil }
func (q *queuedFrame) PushWithFlags(uint32, int64, []byte, uint8) (int, error) { return 0, nil }
func (q *queuedFrame) PopNonBlock() ([]byte, uint32, error) {
	if q.data == nil {
		return nil, 0, ErrBufferEmpty
	}
	data := q.data
	q.data = nil
	return data, 0, nil
}
func (q *queuedFrame) Stats() JitterBufferStats { return JitterBufferStats{} }
func (q *queuedFrame) Close() error             { return nil }

type countSink struct {
	*audio.VirtualAudioDevice
	writes int
}

func (s *countSink) Write(data []byte) (int, error) {
	s.writes++
	return s.VirtualAudioDevice.Write(data)
}

func pcmSamples(samples ...int16) []byte {
	data := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
	}
	return data
}

func TestPlayoutMixesPeersInOneSaturatingWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []int16
		want []int16
	}{
		{"sum and cancel", []int16{10000, -10000}, []int16{20000, 10000}, []int16{30000, 0}},
		{"clip positive and negative", []int16{30000, -30000}, []int16{30000, -30000}, []int16{32767, -32768}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &countSink{VirtualAudioDevice: audio.NewVirtualAudioDevice()}
			defer out.Close()
			s, err := NewStreamer(nil, nil, DefaultStreamConfig(48000, 1), "receiver")
			if err != nil {
				t.Fatal(err)
			}
			s.output = out
			s.peers["a"] = &RemotePeer{Output: out, JitterBuf: &queuedFrame{data: pcmSamples(tc.a...)}}
			s.peers["b"] = &RemotePeer{Output: out, JitterBuf: &queuedFrame{data: pcmSamples(tc.b...)}}
			s.playout()
			if out.writes != 1 {
				t.Fatalf("writes = %d, want 1", out.writes)
			}
			got := out.CapturedBytes()
			if len(got) != len(tc.want)*2 {
				t.Fatalf("bytes = %d", len(got))
			}
			for i, want := range tc.want {
				if sample := int16(binary.LittleEndian.Uint16(got[i*2:])); sample != want {
					t.Fatalf("sample %d = %d, want %d", i, sample, want)
				}
			}
			s.playout()
			if out.writes != 1 {
				t.Fatal("empty tick wrote output")
			}
		})
	}
}

func TestPlayoutClampsAfterAllPeers(t *testing.T) {
	out := &countSink{VirtualAudioDevice: audio.NewVirtualAudioDevice()}
	defer out.Close()
	s, err := NewStreamer(nil, nil, DefaultStreamConfig(48000, 1), "receiver")
	if err != nil {
		t.Fatal(err)
	}
	s.output = out
	// Intermediate clamping would yield 32767 - 30000 = 2767 rather than 30000.
	for name, sample := range map[string]int16{"a": 30000, "b": 30000, "c": -30000} {
		s.peers[name] = &RemotePeer{Output: out, JitterBuf: &queuedFrame{data: pcmSamples(sample)}}
	}
	s.playout()
	if out.writes != 1 {
		t.Fatalf("writes = %d", out.writes)
	}
	if got := int16(binary.LittleEndian.Uint16(out.CapturedBytes())); got != 30000 {
		t.Fatalf("sample = %d, want 30000", got)
	}
}

func TestPlayoutSinglePeerPassthrough(t *testing.T) {
	out := &countSink{VirtualAudioDevice: audio.NewVirtualAudioDevice()}
	defer out.Close()
	s, err := NewStreamer(nil, nil, DefaultStreamConfig(48000, 1), "receiver")
	if err != nil {
		t.Fatal(err)
	}
	s.output = out
	want := pcmSamples(-12345, 23456, 0)
	s.peers["only"] = &RemotePeer{Output: out, JitterBuf: &queuedFrame{data: want}}
	s.playout()
	if out.writes != 1 {
		t.Fatalf("writes = %d, want 1", out.writes)
	}
	if got := out.CapturedBytes(); !bytes.Equal(got, want) {
		t.Fatalf("passthrough samples = %v, want %v", got, want)
	}
}
