package streamer

import (
	"encoding/binary"
	"math"
	"testing"
)

func putInt16(b []byte, v int16) {
	binary.LittleEndian.PutUint16(b, uint16(v))
}

func TestIngressAdapter_ChannelRemapping(t *testing.T) {
	// 1. Mono to Stereo
	t.Run("MonoToStereo", func(t *testing.T) {
		adapter := NewIngressAdapter(48000, 2)
		// 3 mono samples: 1000, -2000, 3000
		in := make([]byte, 6)
		putInt16(in[0:], 1000)
		putInt16(in[2:], -2000)
		putInt16(in[4:], 3000)

		out, err := adapter.Process(in, 48000, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != 12 { // 3 frames * 2 channels * 2 bytes
			t.Fatalf("expected 12 bytes, got %d", len(out))
		}

		// Verify duplicate channels L and R
		for i, expected := range []int16{1000, -2000, 3000} {
			l := int16(binary.LittleEndian.Uint16(out[i*4:]))
			r := int16(binary.LittleEndian.Uint16(out[i*4+2:]))
			if l != expected || r != expected {
				t.Fatalf("frame %d: expected L=R=%d, got L=%d, R=%d", i, expected, l, r)
			}
		}
	})

	// 2. Stereo to Mono
	t.Run("StereoToMono", func(t *testing.T) {
		adapter := NewIngressAdapter(48000, 1)
		// 2 stereo frames: (1000, 3000) -> 2000, (-1000, -3000) -> -2000
		in := make([]byte, 8)
		putInt16(in[0:], 1000)
		putInt16(in[2:], 3000)
		putInt16(in[4:], -1000)
		putInt16(in[6:], -3000)

		out, err := adapter.Process(in, 48000, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != 4 { // 2 frames * 1 channel * 2 bytes
			t.Fatalf("expected 4 bytes, got %d", len(out))
		}

		s0 := int16(binary.LittleEndian.Uint16(out[0:]))
		s1 := int16(binary.LittleEndian.Uint16(out[2:]))
		if s0 != 2000 || s1 != -2000 {
			t.Fatalf("expected samples [2000, -2000], got [%d, %d]", s0, s1)
		}
	})

	// 3. Passthrough
	t.Run("Passthrough", func(t *testing.T) {
		adapter := NewIngressAdapter(48000, 2)
		in := make([]byte, 8)
		binary.LittleEndian.PutUint16(in[0:], 100)
		binary.LittleEndian.PutUint16(in[2:], 200)
		binary.LittleEndian.PutUint16(in[4:], 300)
		binary.LittleEndian.PutUint16(in[6:], 400)

		out, err := adapter.Process(in, 48000, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != len(in) {
			t.Fatalf("expected %d bytes, got %d", len(in), len(out))
		}
		for i := range in {
			if out[i] != in[i] {
				t.Fatalf("byte %d mismatch: got %x want %x", i, out[i], in[i])
			}
		}
	})
}

func TestIngressAdapter_ResamplingRates(t *testing.T) {
	rates := []struct {
		name       string
		inRate     int
		inChannels int
		outRate    int
		outChan    int
		inFrames   int
		wantFrames int
	}{
		{
			name:       "44100_to_48000_stereo",
			inRate:     44100,
			inChannels: 2,
			outRate:    48000,
			outChan:    2,
			inFrames:   882, // 20ms at 44.1kHz
			wantFrames: 960, // 20ms at 48kHz
		},
		{
			name:       "48000_to_44100_stereo",
			inRate:     48000,
			inChannels: 2,
			outRate:    44100,
			outChan:    2,
			inFrames:   960, // 20ms at 48kHz
			wantFrames: 882, // 20ms at 44.1kHz
		},
		{
			name:       "16000_to_48000_mono_to_stereo",
			inRate:     16000,
			inChannels: 1,
			outRate:    48000,
			outChan:    2,
			inFrames:   320, // 20ms at 16kHz
			wantFrames: 960, // 20ms at 48kHz
		},
	}

	for _, tc := range rates {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewIngressAdapter(tc.outRate, tc.outChan)
			inBytes := tc.inFrames * tc.inChannels * 2
			in := make([]byte, inBytes)
			// Fill with DC offset 10000
			for f := 0; f < tc.inFrames; f++ {
				for c := 0; c < tc.inChannels; c++ {
					binary.LittleEndian.PutUint16(in[(f*tc.inChannels+c)*2:], uint16(int16(10000)))
				}
			}

			out, err := adapter.Process(in, tc.inRate, tc.inChannels)
			if err != nil {
				t.Fatalf("process error: %v", err)
			}

			expectedOutBytes := tc.wantFrames * tc.outChan * 2
			if len(out) != expectedOutBytes {
				t.Fatalf("expected %d bytes (%d frames), got %d bytes (%d frames)",
					expectedOutBytes, tc.wantFrames, len(out), len(out)/(tc.outChan*2))
			}

			// Constant DC offset should be preserved through linear resampling
			for f := 0; f < tc.wantFrames; f++ {
				for c := 0; c < tc.outChan; c++ {
					val := int16(binary.LittleEndian.Uint16(out[(f*tc.outChan+c)*2:]))
					if math.Abs(float64(val-10000)) > 2 {
						t.Fatalf("frame %d ch %d: expected ~10000, got %d", f, c, val)
					}
				}
			}
		})
	}
}

func TestIngressAdapter_ZeroAllocations(t *testing.T) {
	adapter := NewIngressAdapter(48000, 2)
	// 44100 Hz mono 20ms packet (882 samples * 2 bytes = 1764 bytes)
	in := make([]byte, 882*2)
	for i := 0; i < 882; i++ {
		binary.LittleEndian.PutUint16(in[i*2:], uint16(int16(i*10)))
	}

	// Warm-up to ensure any one-off setup has executed
	_, err := adapter.Process(in, 44100, 1)
	if err != nil {
		t.Fatalf("warm-up process failed: %v", err)
	}

	allocs := testing.AllocsPerRun(1000, func() {
		_, err := adapter.Process(in, 44100, 1)
		if err != nil {
			t.Fatalf("process error: %v", err)
		}
	})

	if allocs != 0 {
		t.Fatalf("expected 0 steady-state allocations, got %f", allocs)
	}
}

func TestIngressAdapter_PhaseContinuity(t *testing.T) {
	// Test continuous 440 Hz sine wave across 5 successive packets
	// converted from 44100 Hz mono to 48000 Hz stereo.
	adapter := NewIngressAdapter(48000, 2)

	const (
		inRate      = 44100
		inChannels  = 1
		outRate     = 48000
		freq        = 440.0
		inFramesPkt = 882 // 20ms
		numPackets  = 5
	)

	var fullOutput []int16
	tVal := 0.0
	step := 2.0 * math.Pi * freq / float64(inRate)

	for p := 0; p < numPackets; p++ {
		pkt := make([]byte, inFramesPkt*2)
		for i := 0; i < inFramesPkt; i++ {
			sample := int16(25000.0 * math.Sin(tVal))
			tVal += step
			binary.LittleEndian.PutUint16(pkt[i*2:], uint16(sample))
		}

		out, err := adapter.Process(pkt, inRate, inChannels)
		if err != nil {
			t.Fatalf("packet %d error: %v", p, err)
		}

		outFrames := len(out) / 4 // 2 channels * 2 bytes
		if outFrames != 960 {
			t.Fatalf("packet %d: expected 960 frames, got %d", p, outFrames)
		}

		for i := 0; i < outFrames; i++ {
			// Read Left channel
			s := int16(binary.LittleEndian.Uint16(out[i*4:]))
			fullOutput = append(fullOutput, s)
		}
	}

	// In a continuous 440 Hz sine wave at 48 kHz, max delta between adjacent samples is:
	// d/dt (25000 * sin(2*pi*440*t)) * dt = 25000 * (2*pi*440 / 48000) ~ 1440.
	// We verify that at every packet boundary (i = 960, 1920, 2880, 3840), the delta
	// does not experience a discontinuity or phase jump.
	maxDelta := 0
	for i := 1; i < len(fullOutput); i++ {
		delta := int(math.Abs(float64(fullOutput[i] - fullOutput[i-1])))
		if delta > maxDelta {
			maxDelta = delta
		}
	}

	// Boundary transitions must be well below click/phase-jump threshold
	for b := 1; b < numPackets; b++ {
		boundaryIdx := b * 960
		boundaryDelta := int(math.Abs(float64(fullOutput[boundaryIdx] - fullOutput[boundaryIdx-1])))
		if boundaryDelta > 1600 {
			t.Fatalf("packet boundary %d has discontinuity: delta=%d (exceeds tolerance 1600)",
				b, boundaryDelta)
		}
	}

	if maxDelta > 1600 {
		t.Fatalf("unexpected high delta in output stream: %d", maxDelta)
	}
}

func TestIngressAdapter_MultiChannelSurround(t *testing.T) {
	// 1. 5.1 Surround (44.1kHz) to Stereo (48kHz)
	t.Run("5.1_44100_to_Stereo_48000", func(t *testing.T) {
		adapter := NewIngressAdapter(48000, 2)
		const inRate = 44100
		const inFrames = 882 // 20ms
		const inChannels = 6 // 5.1
		inBytes := inFrames * inChannels * 2
		in := make([]byte, inBytes)

		// Fill Center channel (channel index 2) with constant amplitude
		for f := 0; f < inFrames; f++ {
			offset := (f*inChannels + 2) * 2
			putInt16(in[offset:], 15000)
		}

		out, err := adapter.Process(in, inRate, inChannels)
		if err != nil {
			t.Fatalf("process error: %v", err)
		}

		expectedOutFrames := 960 // 20ms at 48kHz
		expectedBytes := expectedOutFrames * 2 * 2
		if len(out) != expectedBytes {
			t.Fatalf("expected %d bytes, got %d", expectedBytes, len(out))
		}

		// Since input was Center channel only, Left and Right must be symmetric
		for f := 0; f < expectedOutFrames; f++ {
			l := int16(binary.LittleEndian.Uint16(out[f*4:]))
			r := int16(binary.LittleEndian.Uint16(out[f*4+2:]))
			if l != r {
				t.Fatalf("frame %d: expected symmetric downmix L=R, got L=%d R=%d", f, l, r)
			}
			if l == 0 {
				t.Fatalf("frame %d: expected non-zero downmixed audio, got 0", f)
			}
		}
	})

	// 2. 7.1 Surround to Stereo
	t.Run("7.1_to_Stereo", func(t *testing.T) {
		adapter := NewIngressAdapter(48000, 2)
		const frames = 100
		in := make([]byte, frames*8*2)
		// Set FL=10000, FR=10000
		for f := 0; f < frames; f++ {
			putInt16(in[(f*8+0)*2:], 10000)
			putInt16(in[(f*8+1)*2:], 10000)
		}

		out, err := adapter.Process(in, 48000, 8)
		if err != nil {
			t.Fatalf("process error: %v", err)
		}
		if len(out) != frames*2*2 {
			t.Fatalf("expected %d bytes, got %d", frames*2*2, len(out))
		}
	})

	// 3. Stereo to 5.1 Upmix
	t.Run("Stereo_to_5.1", func(t *testing.T) {
		adapter := NewIngressAdapter(48000, 6)
		const frames = 100
		in := make([]byte, frames*2*2)
		for f := 0; f < frames; f++ {
			putInt16(in[(f*2+0)*2:], 20000)
			putInt16(in[(f*2+1)*2:], 20000)
		}

		out, err := adapter.Process(in, 48000, 2)
		if err != nil {
			t.Fatalf("process error: %v", err)
		}
		if len(out) != frames*6*2 {
			t.Fatalf("expected %d bytes, got %d", frames*6*2, len(out))
		}
	})
}

func TestIngressAdapter_ValidationErrors(t *testing.T) {
	adapter := NewIngressAdapter(48000, 2)

	// Zero-length input
	out, err := adapter.Process([]byte{}, 44100, 1)
	if err != nil || len(out) != 0 {
		t.Fatalf("expected empty slice and nil error, got len=%d err=%v", len(out), err)
	}

	// Invalid rate
	if _, err := adapter.Process([]byte{0, 1}, 0, 1); err == nil {
		t.Fatal("expected error on rate <= 0")
	}

	// Invalid channel count
	if _, err := adapter.Process([]byte{0, 1}, 44100, 0); err == nil {
		t.Fatal("expected error on channels <= 0")
	}

	// Invalid buffer length (not a multiple of frame size: 1 channel needs 2 bytes, 3 bytes is invalid)
	if _, err := adapter.Process([]byte{0, 1, 2}, 44100, 1); err == nil {
		t.Fatal("expected error on odd buffer length")
	}
}
