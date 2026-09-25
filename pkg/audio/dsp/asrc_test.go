package dsp

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestASRCResampler_PassThroughAccuracy(t *testing.T) {
	channels := 2
	resampler := NewASRCResampler(channels)

	numFrames := 960
	inPCM := generateSinePCM(48000, channels, 440.0, numFrames)
	outPCM := make([]byte, len(inPCM))

	n := resampler.Process(inPCM, outPCM, 1.0)
	if n != len(outPCM) {
		t.Fatalf("expected %d bytes, got %d", len(outPCM), n)
	}

	// For step 1.0 starting at phase 0, Catmull-Rom evaluates at integer positions x=0,
	// where s(0) == s1. So output must match input almost exactly!
	maxDiff := 0
	for i := 0; i < len(inPCM)/2; i++ {
		sIn := int(int16(binary.LittleEndian.Uint16(inPCM[i*2:])))
		sOut := int(int16(binary.LittleEndian.Uint16(outPCM[i*2:])))
		diff := int(math.Abs(float64(sIn - sOut)))
		if diff > maxDiff {
			maxDiff = diff
		}
	}

	if maxDiff > 1 {
		t.Fatalf("expected near-zero difference on 1.0 pass-through, got max diff = %d", maxDiff)
	}
}

func TestASRCResampler_PhaseContinuityAcrossPackets(t *testing.T) {
	channels := 2
	freq := 400.0
	sampleRate := 48000
	step := 1.0003 // +300 ppm drift

	totalFrames := 1920
	fullInput := generateSinePCM(sampleRate, channels, freq, totalFrames)

	// Resample contiguous in one shot
	rContiguous := NewASRCResampler(channels)
	outContiguous := make([]byte, totalFrames*channels*2+100)
	nContiguous := rContiguous.Process(fullInput, outContiguous, step)

	// Resample chunked into two 960-frame packets
	rChunked := NewASRCResampler(channels)
	pkt1 := fullInput[:960*channels*2]
	pkt2 := fullInput[960*channels*2:]

	outChunked1 := make([]byte, 1000*channels*2)
	outChunked2 := make([]byte, 1000*channels*2)

	n1 := rChunked.Process(pkt1, outChunked1, step)
	n2 := rChunked.Process(pkt2, outChunked2, step)

	joined := append(outChunked1[:n1], outChunked2[:n2]...)

	// Compare joined output with contiguous output
	compareBytes := nContiguous
	if len(joined) < compareBytes {
		compareBytes = len(joined)
	}

	maxDiff := 0
	for i := 0; i < compareBytes/2; i++ {
		s1 := int(int16(binary.LittleEndian.Uint16(outContiguous[i*2:])))
		s2 := int(int16(binary.LittleEndian.Uint16(joined[i*2:])))
		diff := int(math.Abs(float64(s1 - s2)))
		if diff > maxDiff {
			maxDiff = diff
		}
	}

	// Maximum error across the boundary should be tiny (Catmull-Rom Hermite interpolation with history)
	if maxDiff > 50 { // < 0.25% of 20000 amplitude
		t.Fatalf("expected seamless phase continuity across packets, max diff was %d", maxDiff)
	}
}

func TestASRCResampler_NaNAndInfSanitization(t *testing.T) {
	channels := 2
	resampler := NewASRCResampler(channels)
	inPCM := generateSinePCM(48000, channels, 440.0, 960)
	outPCM := make([]byte, len(inPCM))

	// Test NaN step
	n := resampler.Process(inPCM, outPCM, math.NaN())
	if n != len(outPCM) {
		t.Fatalf("expected valid output with NaN step, got %d", n)
	}

	// Test +Inf and -Inf step
	n = resampler.Process(inPCM, outPCM, math.Inf(1))
	if n != len(outPCM) {
		t.Fatalf("expected valid output with +Inf step, got %d", n)
	}
	n = resampler.Process(inPCM, outPCM, math.Inf(-1))
	if n != len(outPCM) {
		t.Fatalf("expected valid output with -Inf step, got %d", n)
	}

	// Test NaN phase
	resampler.SetPhase(math.NaN())
	n = resampler.Process(inPCM, outPCM, 1.0)
	if n != len(outPCM) {
		t.Fatalf("expected valid output with NaN phase, got %d", n)
	}
	if math.IsNaN(resampler.Phase()) || math.IsInf(resampler.Phase(), 0) {
		t.Fatalf("phase was not sanitized, got %f", resampler.Phase())
	}

	// Test Inf phase
	resampler.SetPhase(math.Inf(1))
	n = resampler.Process(inPCM, outPCM, 1.0)
	if n != len(outPCM) {
		t.Fatalf("expected valid output with Inf phase, got %d", n)
	}
	if math.IsNaN(resampler.Phase()) || math.IsInf(resampler.Phase(), 0) {
		t.Fatalf("phase was not sanitized, got %f", resampler.Phase())
	}
}

func BenchmarkASRCResampler_Process(b *testing.B) {
	channels := 2
	resampler := NewASRCResampler(channels)

	inPCM := generateSinePCM(48000, channels, 440.0, 960)
	outPCM := make([]byte, 960*channels*2+32)
	step := 1.00005 // +50 ppm

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		resampler.Process(inPCM, outPCM, step)
	}
}
