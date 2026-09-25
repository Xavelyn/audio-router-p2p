package dsp

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func generateSinePCM(sampleRate, channels int, freq float64, numFrames int) []byte {
	buf := make([]byte, numFrames*channels*2)
	for i := 0; i < numFrames; i++ {
		t := float64(i) / float64(sampleRate)
		val := int16(math.Sin(2*math.Pi*freq*t) * 20000.0)
		for ch := 0; ch < channels; ch++ {
			off := (i*channels + ch) * 2
			binary.LittleEndian.PutUint16(buf[off:], uint16(val))
		}
	}
	return buf
}

func computeRMS(pcm []byte) float64 {
	if len(pcm) < 2 {
		return 0
	}
	sum := 0.0
	numSamples := len(pcm) / 2
	for i := 0; i < numSamples; i++ {
		val := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += val * val
	}
	return math.Sqrt(sum / float64(numSamples))
}

func TestPSOLAConcealer_PitchPreservation(t *testing.T) {
	sampleRate := 48000
	channels := 1
	freq := 250.0 // Expected pitch period = 48000 / 250 = 192 samples
	expectedPeriod := int(math.Round(float64(sampleRate) / freq))

	plc := NewPSOLAConcealer(sampleRate, channels)

	// Feed 80ms of sine wave (3840 samples)
	history := generateSinePCM(sampleRate, channels, freq, 3840)
	plc.AddHistory(history)

	// Conceal 1 frame of 20ms (960 samples)
	out := make([]byte, 960*channels*2)
	n := plc.Conceal(out)
	if n != len(out) {
		t.Fatalf("expected %d bytes written, got %d", len(out), n)
	}

	stats := plc.Stats()
	if stats.ConcealedFramesTotal != 1 {
		t.Fatalf("expected 1 concealed frame, got %d", stats.ConcealedFramesTotal)
	}
	if stats.PitchDetectedFramesTotal != 1 {
		t.Fatalf("expected 1 pitch detected frame, got %d", stats.PitchDetectedFramesTotal)
	}

	lag := plc.lastPitchLag
	if math.Abs(float64(lag-expectedPeriod)) > 2 {
		t.Fatalf("expected pitch lag around %d, got %d", expectedPeriod, lag)
	}

	// Compare concealed output with continued sine wave
	ref := make([]byte, 960*channels*2)
	for i := 0; i < 960; i++ {
		tRef := float64(3840+i) / float64(sampleRate)
		val := int16(math.Sin(2*math.Pi*freq*tRef) * 20000.0)
		binary.LittleEndian.PutUint16(ref[i*2:], uint16(val))
	}

	// Compute correlation between synthetic and reference
	var dot, normSynth, normRef float64
	for i := 0; i < 960; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(out[i*2:])))
		r := float64(int16(binary.LittleEndian.Uint16(ref[i*2:])))
		dot += s * r
		normSynth += s * s
		normRef += r * r
	}
	corr := dot / math.Sqrt(normSynth*normRef)
	if corr < 0.85 {
		t.Fatalf("correlation between concealed audio and reference sine is too low: %f (expected >= 0.85)", corr)
	}
}

func TestPSOLAConcealer_EnergyDecayAndMute(t *testing.T) {
	sampleRate := 48000
	channels := 2
	plc := NewPSOLAConcealer(sampleRate, channels)

	// Feed 80ms of 400Hz audio
	history := generateSinePCM(sampleRate, channels, 400.0, 3840)
	plc.AddHistory(history)

	frameLen := 960 * channels * 2
	frames := make([][]byte, 6)
	rms := make([]float64, 6)

	for i := 0; i < 6; i++ {
		frames[i] = make([]byte, frameLen)
		plc.Conceal(frames[i])
		rms[i] = computeRMS(frames[i])
	}

	// Verify monotonic energy decay for frames 0..3
	for i := 0; i < 4; i++ {
		if rms[i] <= rms[i+1] {
			t.Fatalf("expected RMS[%d] (%f) > RMS[%d] (%f)", i, rms[i], i+1, rms[i+1])
		}
	}

	// Frame 4 (index 4 = frame 5) should be total mute (silence)
	if rms[4] != 0.0 {
		t.Fatalf("expected frame 5 to be completely muted, got RMS = %f", rms[4])
	}
	if rms[5] != 0.0 {
		t.Fatalf("expected frame 6 to be completely muted, got RMS = %f", rms[5])
	}

	stats := plc.Stats()
	if stats.SilenceDecayEventsTotal != 1 {
		t.Fatalf("expected 1 silence decay event, got %d", stats.SilenceDecayEventsTotal)
	}
}

func TestPSOLAConcealer_CrossfadeOnRecovery(t *testing.T) {
	sampleRate := 48000
	channels := 1
	plc := NewPSOLAConcealer(sampleRate, channels)

	// Feed 80ms of 300Hz sine
	h1 := generateSinePCM(sampleRate, channels, 300.0, 3840)
	plc.AddHistory(h1)

	// Conceal 1 frame
	concealed := make([]byte, 960*channels*2)
	plc.Conceal(concealed)

	// Next valid frame arrives: 300Hz sine continuation
	realFrame := make([]byte, 960*channels*2)
	for i := 0; i < 960; i++ {
		tSec := float64(3840+960+i) / float64(sampleRate)
		val := int16(math.Sin(2*math.Pi*300.0*tSec) * 20000.0)
		binary.LittleEndian.PutUint16(realFrame[i*2:], uint16(val))
	}

	outRecovered := make([]byte, 960*channels*2)
	n := plc.Recover(realFrame, outRecovered)
	if n != len(outRecovered) {
		t.Fatalf("expected %d bytes from Recover, got %d", len(outRecovered), n)
	}

	// Verify concealment state is reset
	if plc.ConsecutiveConcealed() != 0 {
		t.Fatalf("expected consecutive concealed frames to reset to 0, got %d", plc.ConsecutiveConcealed())
	}

	// Ensure there are no violent step discontinuities across crossfade
	maxDiff := 0
	for i := 0; i < 959; i++ {
		s0 := int(int16(binary.LittleEndian.Uint16(outRecovered[i*2:])))
		s1 := int(int16(binary.LittleEndian.Uint16(outRecovered[(i+1)*2:])))
		diff := int(math.Abs(float64(s1 - s0)))
		if diff > maxDiff {
			maxDiff = diff
		}
	}

	// For a 20000 amplitude 300Hz sine at 48kHz, max step per sample is ~2*pi*300/48000 * 20000 ~ 785
	// Discontinuity would be thousands (e.g. 10000+).
	if maxDiff > 3500 {
		t.Fatalf("maximum sample step across crossfade is too large (%d), indicates click/discontinuity", maxDiff)
	}
}

func TestPSOLAConcealer_ShortHistoryRecovery(t *testing.T) {
	sampleRate := 48000
	channels := 2
	plc := NewPSOLAConcealer(sampleRate, channels)

	// maxLag = sampleRate / 50 = 960 samples. 2 * maxLag = 1920 samples.
	// Feed a short history of 500 samples (< 2 * maxLag).
	shortHistory := generateSinePCM(sampleRate, channels, 400.0, 500)
	plc.AddHistory(shortHistory)

	// Simulate that concealment was active
	concealed := make([]byte, 960*channels*2)
	plc.Conceal(concealed)

	// Real frame arrives
	realFrame := generateSinePCM(sampleRate, channels, 400.0, 960)
	outRecovered := make([]byte, 960*channels*2)

	// Recover must not panic with negative index and must successfully pass through real audio
	n := plc.Recover(realFrame, outRecovered)
	if n != len(outRecovered) {
		t.Fatalf("expected %d bytes, got %d", len(outRecovered), n)
	}
	if !bytes.Equal(outRecovered, realFrame) {
		t.Fatalf("expected passthrough of realFrame for short history, got mismatch")
	}
	if plc.ConsecutiveConcealed() != 0 {
		t.Fatalf("expected consecutive concealed frames to be 0, got %d", plc.ConsecutiveConcealed())
	}

	// Also test history between maxLag and 2*maxLag (e.g. 1200 samples)
	plc2 := NewPSOLAConcealer(sampleRate, channels)
	plc2.AddHistory(generateSinePCM(sampleRate, channels, 400.0, 1200))
	plc2.Conceal(concealed)
	n2 := plc2.Recover(realFrame, outRecovered)
	if n2 != len(outRecovered) {
		t.Fatalf("expected %d bytes, got %d", len(outRecovered), n2)
	}
	if !bytes.Equal(outRecovered, realFrame) {
		t.Fatalf("expected passthrough of realFrame when history < 2*maxLag, got mismatch")
	}
}

func BenchmarkPSOLAConcealer_Conceal(b *testing.B) {
	plc := NewPSOLAConcealer(48000, 2)
	history := generateSinePCM(48000, 2, 250.0, 3840)
	plc.AddHistory(history)

	out := make([]byte, 960*2*2)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		plc.Conceal(out)
	}
}

func BenchmarkPSOLAConcealer_Recover(b *testing.B) {
	plc := NewPSOLAConcealer(48000, 2)
	history := generateSinePCM(48000, 2, 250.0, 3840)
	plc.AddHistory(history)
	out := make([]byte, 960*2*2)
	plc.Conceal(out)

	realPkt := generateSinePCM(48000, 2, 250.0, 960)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		plc.Recover(realPkt, out)
	}
}
