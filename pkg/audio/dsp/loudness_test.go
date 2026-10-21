package dsp

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestAnalyzeLoudness_SilenceFloor(t *testing.T) {
	// 1. Completely zeroed buffer
	pcm := make([]byte, 1920) // 20ms at 48kHz mono
	metrics := AnalyzeLoudness(pcm, 1)

	if metrics.RMS != 0.0 {
		t.Errorf("expected RMS 0.0, got %f", metrics.RMS)
	}
	if metrics.RMSdBFS != SilenceFloorDBFS {
		t.Errorf("expected RMSdBFS %f, got %f", SilenceFloorDBFS, metrics.RMSdBFS)
	}
	if metrics.Peak != 0 {
		t.Errorf("expected Peak 0, got %d", metrics.Peak)
	}
	if metrics.PeakdBFS != SilenceFloorDBFS {
		t.Errorf("expected PeakdBFS %f, got %f", SilenceFloorDBFS, metrics.PeakdBFS)
	}

	// 2. Empty buffer
	emptyMetrics := AnalyzeLoudness(nil, 2)
	if emptyMetrics.RMSdBFS != SilenceFloorDBFS || emptyMetrics.PeakdBFS != SilenceFloorDBFS {
		t.Errorf("expected silence floor on empty pcm, got %+v", emptyMetrics)
	}
}

func TestAnalyzeLoudness_FullScaleSquareWave(t *testing.T) {
	const samples = 1000
	pcm := make([]byte, samples*2)

	// Full-scale square wave alternating between +32767 and -32767
	for i := 0; i < samples; i++ {
		val := int16(32767)
		if i%2 == 1 {
			val = -32767
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(val))
	}

	metrics := AnalyzeLoudness(pcm, 1)

	// RMS of full-scale square wave is 32767
	if math.Abs(metrics.RMS-32767.0) > 1e-4 {
		t.Errorf("expected RMS ~32767.0, got %f", metrics.RMS)
	}

	// 20 * log10(32767 / 32768) ≈ -0.00026 dBFS (approximately 0.0 dBFS)
	if math.Abs(metrics.RMSdBFS) > 0.01 {
		t.Errorf("expected RMSdBFS ~0 dBFS, got %f", metrics.RMSdBFS)
	}

	if metrics.Peak != 32767 {
		t.Errorf("expected Peak 32767, got %d", metrics.Peak)
	}
	if math.Abs(metrics.PeakdBFS) > 0.01 {
		t.Errorf("expected PeakdBFS ~0 dBFS, got %f", metrics.PeakdBFS)
	}
}

func TestAnalyzeLoudness_SineWave(t *testing.T) {
	const sampleRate = 48000
	const freq = 1000.0
	const durationSec = 0.1
	numSamples := int(sampleRate * durationSec)

	// Target: peak of -6.0 dBFS
	// A = 32768.0 * 10^(-6.0 / 20.0) ≈ 16422.9
	targetPeakDBFS := -6.0
	amp := 32768.0 * math.Pow(10.0, targetPeakDBFS/20.0)

	pcm := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		theta := 2.0 * math.Pi * freq * float64(i) / float64(sampleRate)
		sampleVal := int16(math.Round(amp * math.Sin(theta)))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sampleVal))
	}

	metrics := AnalyzeLoudness(pcm, 1)

	// Peak should be very close to -6.0 dBFS
	if math.Abs(metrics.PeakdBFS-targetPeakDBFS) > 0.1 {
		t.Errorf("expected PeakdBFS ~%f, got %f", targetPeakDBFS, metrics.PeakdBFS)
	}

	// For a pure sine wave, RMS = Peak / sqrt(2) ≈ -6 - 3.0103 = -9.01 dBFS
	expectedRMSdBFS := targetPeakDBFS - 3.0103
	if math.Abs(metrics.RMSdBFS-expectedRMSdBFS) > 0.15 {
		t.Errorf("expected RMSdBFS ~%f, got %f", expectedRMSdBFS, metrics.RMSdBFS)
	}
}

func TestAnalyzeLoudness_NegativeFullScalePeak(t *testing.T) {
	pcm := make([]byte, 2)
	// Sample value -32768 (minimum int16)
	var v int16 = -32768
	binary.LittleEndian.PutUint16(pcm, uint16(v))

	metrics := AnalyzeLoudness(pcm, 1)
	if metrics.Peak != 32767 {
		t.Errorf("expected Peak 32767 for -32768 input, got %d", metrics.Peak)
	}
	if math.Abs(metrics.PeakdBFS) > 0.01 {
		t.Errorf("expected PeakdBFS ~0 dBFS, got %f", metrics.PeakdBFS)
	}
}
