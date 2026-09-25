package dsp

import (
	"encoding/binary"
	"math"
)

// SilenceFloorDBFS defines the theoretical dynamic range floor for 16-bit linear PCM.
const SilenceFloorDBFS = -96.0

// LoudnessMetrics encapsulates signal energy and true peak amplitude metrics
// for an audio frame across all interleaved channels.
type LoudnessMetrics struct {
	RMS      float64 `json:"rms"`
	RMSdBFS  float64 `json:"rms_dbfs"`
	Peak     int16   `json:"peak"`
	PeakdBFS float64 `json:"peak_dbfs"`
}

// AnalyzeLoudness evaluates the Root-Mean-Square (RMS) energy, dBFS relative to full scale,
// and true absolute peak amplitude of 16-bit little-endian PCM audio.
// If the buffer is empty, channels <= 0, or signal is completely silent (RMS == 0),
// dBFS values are clamped to SilenceFloorDBFS (-96.0 dBFS).
func AnalyzeLoudness(pcm []byte, channels int) LoudnessMetrics {
	totalSamples := len(pcm) / 2
	if totalSamples == 0 || channels <= 0 {
		return LoudnessMetrics{
			RMS:      0.0,
			RMSdBFS:  SilenceFloorDBFS,
			Peak:     0,
			PeakdBFS: SilenceFloorDBFS,
		}
	}

	var sumSq float64
	var maxAbs int32

	for i := 0; i < totalSamples; i++ {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2 : (i+1)*2]))
		val := int32(sample)
		absVal := val
		if absVal < 0 {
			absVal = -absVal
		}
		if absVal > 32767 {
			absVal = 32767
		}
		if absVal > maxAbs {
			maxAbs = absVal
		}

		sF := float64(val)
		sumSq += sF * sF
	}

	rms := math.Sqrt(sumSq / float64(totalSamples))

	var rmsDBFS float64
	if rms == 0.0 {
		rmsDBFS = SilenceFloorDBFS
	} else {
		// 20 * log10(max(RMS, 10^-6) / 32768.0)
		effRMS := rms
		if effRMS < 1e-6 {
			effRMS = 1e-6
		}
		rmsDBFS = 20.0 * math.Log10(effRMS/32768.0)
		if rmsDBFS < SilenceFloorDBFS {
			rmsDBFS = SilenceFloorDBFS
		}
	}

	peak := int16(maxAbs)
	var peakDBFS float64
	if peak == 0 {
		peakDBFS = SilenceFloorDBFS
	} else {
		peakDBFS = 20.0 * math.Log10(float64(peak)/32768.0)
		if peakDBFS < SilenceFloorDBFS {
			peakDBFS = SilenceFloorDBFS
		}
	}

	return LoudnessMetrics{
		RMS:      rms,
		RMSdBFS:  rmsDBFS,
		Peak:     peak,
		PeakdBFS: peakDBFS,
	}
}
