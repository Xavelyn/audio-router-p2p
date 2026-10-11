//go:build !noaudio

package audio

import (
	"encoding/binary"
	"io"
	"testing"
)

type mockPAStream struct {
	buf       []int16
	sampleSeq int16
	channels  int
}

func (m *mockPAStream) Read() error {
	frames := len(m.buf) / m.channels
	for i := 0; i < frames; i++ {
		for c := 0; c < m.channels; c++ {
			m.buf[i*m.channels+c] = m.sampleSeq
		}
		m.sampleSeq++
	}
	return nil
}

func (m *mockPAStream) Stop() error  { return nil }
func (m *mockPAStream) Close() error { return nil }

func TestPortAudioInput_SubBufferRemainderPreserved(t *testing.T) {
	const (
		channels        = 2
		blocksPerBuf    = 256
		packetFrames    = 960 // 20ms @ 48kHz
		numReads        = 20  // total 19200 frames
		bytesPerFrame   = channels * 2
		packetByteCount = packetFrames * bytesPerFrame // 3840 bytes
	)

	buf := make([]int16, blocksPerBuf*channels)
	mock := &mockPAStream{
		buf:      buf,
		channels: channels,
	}

	in := NewPortaudioInputForTest(mock, buf)
	defer in.Close()

	packet := make([]byte, packetByteCount)
	var allFrames []int16

	for r := 0; r < numReads; r++ {
		offset := 0
		for offset < packetByteCount {
			n, err := in.Read(packet[offset:])
			if err != nil {
				t.Fatalf("read %d: %v", r, err)
			}
			if n == 0 {
				t.Fatalf("read %d returned 0 bytes", r)
			}
			offset += n
		}

		for i := 0; i < packetFrames; i++ {
			sample := int16(binary.LittleEndian.Uint16(packet[i*bytesPerFrame:]))
			allFrames = append(allFrames, sample)
		}
	}

	if len(allFrames) != numReads*packetFrames {
		t.Fatalf("expected %d frames, got %d", numReads*packetFrames, len(allFrames))
	}

	// Verify sample sequence continuity without any drops or phase jumps
	droppedFrames := 0
	for i := 1; i < len(allFrames); i++ {
		expected := allFrames[i-1] + 1
		actual := allFrames[i]
		if actual != expected {
			droppedFrames++
			if droppedFrames <= 10 {
				t.Errorf("frame %d: expected %d, got %d (gap=%d)", i, expected, actual, actual-expected)
			}
		}
	}

	if droppedFrames > 0 {
		t.Fatalf("total dropped/discontinuous frames: %d (audio would be robotic/distorted)", droppedFrames)
	}
}

func TestPortAudioInput_SpillDrainBeforeEOF(t *testing.T) {
	buf := make([]int16, 256*2)
	mock := &mockPAStream{
		buf:      buf,
		channels: 2,
	}

	in := NewPortaudioInputForTest(mock, buf)

	// Read 500 bytes (less than 1024 bytes need)
	part := make([]byte, 500)
	n, err := in.Read(part)
	if err != nil || n != 500 {
		t.Fatalf("expected 500 bytes, got %d (err: %v)", n, err)
	}

	// Now close the stream
	_ = in.Close()

	// Subsequent read should drain remaining 524 bytes from spill before returning EOF
	rem := make([]byte, 1024)
	n, err = in.Read(rem)
	if err != nil {
		t.Fatalf("expected spill drain, got err: %v", err)
	}
	if n != 524 {
		t.Fatalf("expected 524 spill bytes, got %d", n)
	}

	// Next read should return EOF
	n, err = in.Read(rem)
	if err != io.EOF {
		t.Fatalf("expected EOF after spill drained, got n=%d err=%v", n, err)
	}
}

func TestBuildRateCandidates_Prioritization(t *testing.T) {
	// 1. Without activeRates: requested rate should come first
	rates := buildRateCandidates(48000, 44100)
	if len(rates) == 0 || rates[0] != 48000 {
		t.Fatalf("expected 48000 at index 0, got %v", rates)
	}

	// 2. With activeRates (e.g. procfs detected 44100 locked hardware rate):
	// Active rate MUST be index 0
	rates = buildRateCandidates(48000, 48000, 44100)
	if len(rates) == 0 || rates[0] != 44100 {
		t.Fatalf("expected active rate 44100 at index 0, got %v", rates)
	}

	// Verify deduplication
	seen := make(map[int]int)
	for _, r := range rates {
		seen[r]++
		if seen[r] > 1 {
			t.Errorf("duplicate rate %d in %v", r, rates)
		}
	}
}

func TestExtractAlsaCardAndDevice(t *testing.T) {
	tests := []struct {
		name     string
		fallback []string
		wantC    int
		wantD    int
		wantOk   bool
	}{
		{
			name:   "hw:Loopback,1",
			wantC:  0,
			wantD:  0,
			wantOk: false, // "Loopback" is not integer card number in hw:
		},
		{
			name:   "hw:0,1",
			wantC:  0,
			wantD:  1,
			wantOk: true,
		},
		{
			name:   "plughw:2,0",
			wantC:  2,
			wantD:  0,
			wantOk: true,
		},
		{
			name:   "ALSA: HDA Intel PCH (hw:0,0)",
			wantC:  0,
			wantD:  0,
			wantOk: true,
		},
		{
			name:     "Invalid name with URN fallback",
			fallback: []string{"urn:audio:hw:alsa:c=1:d=2"},
			wantC:    1,
			wantD:    2,
			wantOk:   true,
		},
		{
			name:     "Invalid name with card-only fallback",
			fallback: []string{"hw:3"},
			wantC:    3,
			wantD:    0,
			wantOk:   true,
		},
	}

	for _, tc := range tests {
		c, d, ok := extractAlsaCardAndDevice(tc.name, tc.fallback...)
		if ok != tc.wantOk {
			t.Errorf("[%s] expected ok=%t, got %t", tc.name, tc.wantOk, ok)
			continue
		}
		if ok {
			if c != tc.wantC || d != tc.wantD {
				t.Errorf("[%s] expected card=%d dev=%d, got card=%d dev=%d", tc.name, tc.wantC, tc.wantD, c, d)
			}
		}
	}
}
