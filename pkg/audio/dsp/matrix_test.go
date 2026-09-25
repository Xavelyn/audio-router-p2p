package dsp

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestClampInt16(t *testing.T) {
	tests := []struct {
		in   float32
		want int16
	}{
		{0.0, 0},
		{0.4, 0},
		{0.6, 1},
		{-0.4, 0},
		{-0.6, -1},
		{32767.0, 32767},
		{32767.4, 32767},
		{32767.9, 32767},
		{32768.0, 32767},
		{100000.0, 32767},
		{-32768.0, -32768},
		{-32768.4, -32768},
		{-32769.0, -32768},
		{-100000.0, -32768},
	}

	for _, tt := range tests {
		got := clampInt16(tt.in)
		if got != tt.want {
			t.Errorf("clampInt16(%f) = %d; want %d", tt.in, got, tt.want)
		}
	}
}

func TestPresetsCreation(t *testing.T) {
	pairs := [][2]int{
		{1, 2}, {2, 1},
		{2, 4}, {4, 2},
		{2, 6}, {6, 2},
		{2, 8}, {8, 2},
		{3, 5}, {5, 3}, // arbitrary fallback
		{2, 2}, {6, 6}, // identity
	}

	for _, p := range pairs {
		m, err := NewChannelMatrix(p[0], p[1])
		if err != nil {
			t.Fatalf("NewChannelMatrix(%d, %d) failed: %v", p[0], p[1], err)
		}
		if m.InChannels() != p[0] || m.OutChannels() != p[1] {
			t.Fatalf("channel mismatch: got %d->%d, want %d->%d", m.InChannels(), m.OutChannels(), p[0], p[1])
		}
	}
}

// TestClippingImmunity verifies that full-scale coherent audio (+32767 on all channels)
// does not cause integer overflow or wrap-around clipping when downmixed.
func TestClippingImmunity(t *testing.T) {
	downmixConfigs := [][2]int{
		{2, 1},
		{4, 2},
		{6, 2},
		{8, 2},
		{5, 2},
		{7, 3},
	}

	const frames = 100
	const maxVal = int16(32767)

	for _, cfg := range downmixConfigs {
		inCh, outCh := cfg[0], cfg[1]
		m, err := NewChannelMatrix(inCh, outCh)
		if err != nil {
			t.Fatalf("NewChannelMatrix(%d, %d) failed: %v", inCh, outCh, err)
		}

		inPCM := make([]byte, frames*inCh*2)
		for f := 0; f < frames; f++ {
			for c := 0; c < inCh; c++ {
				offset := (f*inCh + c) * 2
				binary.LittleEndian.PutUint16(inPCM[offset:], uint16(maxVal))
			}
		}

		outPCM := make([]byte, frames*outCh*2)
		n, err := m.Transform(inPCM, outPCM)
		if err != nil {
			t.Fatalf("Transform error for %d->%d: %v", inCh, outCh, err)
		}
		if n != len(outPCM) {
			t.Fatalf("expected %d bytes written, got %d", len(outPCM), n)
		}

		// Verify every output sample is positive and <= 32767 (never wrapped to negative)
		for f := 0; f < frames; f++ {
			for c := 0; c < outCh; c++ {
				offset := (f*outCh + c) * 2
				sample := int16(binary.LittleEndian.Uint16(outPCM[offset:]))
				if sample < 0 {
					t.Fatalf("cfg %d->%d: integer wrap-around detected! sample=%d at frame %d, ch %d",
						inCh, outCh, sample, f, c)
				}
				if sample > 32767 {
					t.Fatalf("cfg %d->%d: sample exceeded 32767: %d", inCh, outCh, sample)
				}
			}
		}
	}
}

// TestChannelSymmetry verifies that for symmetric downmix/upmix matrices (e.g. 5.1 -> Stereo),
// energizing the left surround/front yields identical magnitude in Left as energizing right produces in Right.
func TestChannelSymmetry(t *testing.T) {
	// 5.1 -> 2 (SMPTE: FL, FR, FC, LFE, SL, SR)
	m51to2, err := NewChannelMatrix(Layout51, LayoutStereo)
	if err != nil {
		t.Fatalf("NewChannelMatrix(6, 2) failed: %v", err)
	}

	const testVal = int16(20000)

	// Case A: Left-dominated input: FL=testVal, SL=testVal, all others 0
	inA := make([]byte, 6*2)
	binary.LittleEndian.PutUint16(inA[0*2:], uint16(testVal)) // FL
	binary.LittleEndian.PutUint16(inA[4*2:], uint16(testVal)) // SL
	outA := make([]byte, 2*2)
	if _, err := m51to2.Transform(inA, outA); err != nil {
		t.Fatalf("transform A: %v", err)
	}
	leftA := int16(binary.LittleEndian.Uint16(outA[0:2]))
	rightA := int16(binary.LittleEndian.Uint16(outA[2:4]))

	// Case B: Right-dominated input: FR=testVal, SR=testVal, all others 0
	inB := make([]byte, 6*2)
	binary.LittleEndian.PutUint16(inB[1*2:], uint16(testVal)) // FR
	binary.LittleEndian.PutUint16(inB[5*2:], uint16(testVal)) // SR
	outB := make([]byte, 2*2)
	if _, err := m51to2.Transform(inB, outB); err != nil {
		t.Fatalf("transform B: %v", err)
	}
	leftB := int16(binary.LittleEndian.Uint16(outB[0:2]))
	rightB := int16(binary.LittleEndian.Uint16(outB[2:4]))

	if leftA != rightB {
		t.Fatalf("symmetry violation: leftA (%d) != rightB (%d)", leftA, rightB)
	}
	if rightA != leftB {
		t.Fatalf("symmetry violation: rightA (%d) != leftB (%d)", rightA, leftB)
	}
	if rightA != 0 || leftB != 0 {
		t.Fatalf("channel bleed violation: rightA=%d, leftB=%d, want 0", rightA, leftB)
	}
}

// TestZeroAllocations verifies that Transform executes with 0 heap allocations
// on pre-allocated buffers.
func TestZeroAllocations(t *testing.T) {
	m, err := NewChannelMatrix(Layout51, LayoutStereo)
	if err != nil {
		t.Fatalf("NewChannelMatrix failed: %v", err)
	}

	const numFrames = 960 // 20ms at 48kHz
	inPCM := make([]byte, numFrames*6*2)
	outPCM := make([]byte, numFrames*2*2)

	// Populate some dummy audio
	for i := 0; i < len(inPCM); i += 2 {
		binary.LittleEndian.PutUint16(inPCM[i:], uint16(1234))
	}

	allocs := testing.AllocsPerRun(100, func() {
		_, err := m.Transform(inPCM, outPCM)
		if err != nil {
			t.Fatalf("Transform error: %v", err)
		}
	})

	if allocs != 0 {
		t.Fatalf("Transform allocated %f heap objects per run; want 0", allocs)
	}
}

// TestITU51DownmixCoefficients verifies the mathematical ITU-R BS.775 downmix equation.
func TestITU51DownmixCoefficients(t *testing.T) {
	m, err := NewChannelMatrix(Layout51, LayoutStereo)
	if err != nil {
		t.Fatalf("matrix creation failed: %v", err)
	}

	// For Center channel (index 2):
	// L = k_norm * 0.7071 * FC, R = k_norm * 0.7071 * FC
	const fcVal = int16(20000)
	inFC := make([]byte, 6*2)
	binary.LittleEndian.PutUint16(inFC[2*2:], uint16(fcVal))
	outFC := make([]byte, 2*2)

	if _, err := m.Transform(inFC, outFC); err != nil {
		t.Fatalf("transform error: %v", err)
	}

	l := int16(binary.LittleEndian.Uint16(outFC[0:2]))
	r := int16(binary.LittleEndian.Uint16(outFC[2:4]))

	if l != r {
		t.Fatalf("center downmix asymmetry: L=%d, R=%d", l, r)
	}

	// Expected: 20000 * 0.41421356 * 0.70710678 = 20000 * 0.2928932 = 5857.86 -> 5858
	expected := int16(math.Round(float64(fcVal) * (1.0 / (1.0 + math.Sqrt2)) * (1.0 / math.Sqrt2)))
	diff := int(math.Abs(float64(l - expected)))
	if diff > 1 {
		t.Fatalf("unexpected Center downmix value: got %d, expected ~%d (diff %d)", l, expected, diff)
	}
}
