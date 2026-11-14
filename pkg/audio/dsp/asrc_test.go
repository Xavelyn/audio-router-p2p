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

func TestASRCResampler_SmallPacketHistoryContinuity(t *testing.T) {
	channels := 1
	resampler := NewASRCResampler(channels)
	frameBytes := channels * 2

	// Feed 5 frames one by one (1-frame packets)
	values := []int16{1000, 2000, 3000, 4000, 5000}
	outBuf := make([]byte, 16)
	for _, val := range values {
		inBuf := make([]byte, frameBytes)
		binary.LittleEndian.PutUint16(inBuf, uint16(val))
		resampler.Process(inBuf, outBuf, 1.0)
	}

	// At this point, history must contain the last 3 frames: [3000, 4000, 5000]
	// If the old zero-padding bug were present, history[0] and history[1] would be 0!
	expectedHist := []int16{3000, 4000, 5000}
	for i, exp := range expectedHist {
		got := resampler.history[i]
		if got != exp {
			t.Errorf("history[%d] = %d, want %d (zero-padding wiped history!)", i, got, exp)
		}
	}

	// Now feed a 2-frame packet: [6000, 7000]
	inBuf2 := make([]byte, 2*frameBytes)
	binary.LittleEndian.PutUint16(inBuf2[0:], 6000)
	binary.LittleEndian.PutUint16(inBuf2[2:], 7000)
	resampler.Process(inBuf2, outBuf, 1.0)

	// History should now be [5000, 6000, 7000]
	expectedHist2 := []int16{5000, 6000, 7000}
	for i, exp := range expectedHist2 {
		got := resampler.history[i]
		if got != exp {
			t.Errorf("after 2-frame packet: history[%d] = %d, want %d", i, got, exp)
		}
	}

	// Verify audio continuity: compare stream processed in 1-2 frame chunks vs contiguous
	fullInput := generateSinePCM(48000, 2, 440.0, 100)
	rContig := NewASRCResampler(2)
	outContig := make([]byte, len(fullInput)*2)
	nContig := rContig.Process(fullInput, outContig, 1.0001)

	rSmall := NewASRCResampler(2)
	var outSmall []byte
	chunkSizes := []int{1, 2, 1, 3, 1, 2}
	off := 0
	csIdx := 0
	frameBytes2 := 4
	for off < len(fullInput) {
		frames := chunkSizes[csIdx%len(chunkSizes)]
		csIdx++
		byteLen := frames * frameBytes2
		if off+byteLen > len(fullInput) {
			byteLen = len(fullInput) - off
		}
		inChunk := fullInput[off : off+byteLen]
		chunkOut := make([]byte, 32)
		n := rSmall.Process(inChunk, chunkOut, 1.0001)
		outSmall = append(outSmall, chunkOut[:n]...)
		off += byteLen
	}

	minLen := nContig
	if len(outSmall) < minLen {
		minLen = len(outSmall)
	}
	maxDiff := 0
	for i := 0; i < minLen/2; i++ {
		s1 := int(int16(binary.LittleEndian.Uint16(outContig[i*2:])))
		s2 := int(int16(binary.LittleEndian.Uint16(outSmall[i*2:])))
		diff := int(math.Abs(float64(s1 - s2)))
		if diff > maxDiff {
			maxDiff = diff
		}
	}
	if maxDiff > 100 {
		t.Fatalf("small-packet processing resulted in audio discontinuity, max diff = %d", maxDiff)
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
