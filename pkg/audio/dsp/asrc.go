package dsp

import (
	"encoding/binary"
	"math"
	"sync/atomic"
)

// ASRCResampler implements an Asynchronous Sample Rate Converter using
// 4-point 3rd-order Catmull-Rom (Hermite) cubic interpolation with
// continuous fractional phase carryover across packet boundaries.
//
// Process executes with zero dynamic heap allocations.
type ASRCResampler struct {
	channels   int
	phase      float64
	history    [3 * 8]int16 // up to 8 channels, 3 frames of history
	hasHistory bool
	samplesAdj atomic.Uint64
}

// NewASRCResampler creates a resampler for the given number of channels (1 to 8).
func NewASRCResampler(channels int) *ASRCResampler {
	if channels <= 0 {
		channels = 2
	}
	if channels > 8 {
		channels = 8
	}
	return &ASRCResampler{
		channels: channels,
	}
}

// Reset clears the phase and history state.
func (r *ASRCResampler) Reset() {
	r.phase = 0
	r.hasHistory = false
	r.samplesAdj.Store(0)
	for i := range r.history {
		r.history[i] = 0
	}
}

// Phase returns the current fractional phase in [0, 1).
func (r *ASRCResampler) Phase() float64 {
	return r.phase
}

// SetPhase explicitly sets the resampler phase.
func (r *ASRCResampler) SetPhase(phase float64) {
	r.phase = phase
}

// SamplesAdjusted returns the total number of resampled output frames produced.
func (r *ASRCResampler) SamplesAdjusted() uint64 {
	return r.samplesAdj.Load()
}

// Process resamples inPCM into outPCM at the given step ratio (inRate / outRate).
// Both inPCM and outPCM are interleaved 16-bit little-endian PCM audio.
// Returns the number of bytes written to outPCM.
// Hot path execution contains zero heap allocations.
func (r *ASRCResampler) Process(inPCM, outPCM []byte, step float64) int {
	if math.IsNaN(step) || math.IsInf(step, 0) || step <= 0 {
		step = 1.0
	}
	if math.IsNaN(r.phase) || math.IsInf(r.phase, 0) || r.phase < 0 {
		r.phase = 0
	}

	frameBytes := r.channels * 2
	inFrames := len(inPCM) / frameBytes
	if inFrames == 0 {
		return 0
	}

	maxOutFrames := len(outPCM) / frameBytes
	if maxOutFrames == 0 {
		return 0
	}

	pos := r.phase
	outFrames := 0

	// Helper to access sample at relative frame index idx for channel ch
	sampleAt := func(idx, ch int) float64 {
		if idx >= 0 && idx < inFrames {
			off := (idx*r.channels + ch) * 2
			return float64(int16(binary.LittleEndian.Uint16(inPCM[off:])))
		}
		if idx < 0 {
			if r.hasHistory {
				// History: frame -1 is at [2*channels+ch], -2 at [1*channels+ch], -3 at [0*channels+ch]
				histIdx := 3 + idx
				if histIdx >= 0 && histIdx < 3 {
					return float64(r.history[histIdx*r.channels+ch])
				}
			}
			// Extrapolate from frame 0
			off := ch * 2
			return float64(int16(binary.LittleEndian.Uint16(inPCM[off:])))
		}
		// idx >= inFrames: extrapolate linearly from last two frames
		if inFrames >= 2 {
			offLast := ((inFrames-1)*r.channels + ch) * 2
			offPrev := ((inFrames-2)*r.channels + ch) * 2
			sLast := float64(int16(binary.LittleEndian.Uint16(inPCM[offLast:])))
			sPrev := float64(int16(binary.LittleEndian.Uint16(inPCM[offPrev:])))
			return sLast + float64(idx-(inFrames-1))*(sLast-sPrev)
		}
		off := ((inFrames-1)*r.channels + ch) * 2
		return float64(int16(binary.LittleEndian.Uint16(inPCM[off:])))
	}

	for outFrames < maxOutFrames && pos < float64(inFrames) {
		k := int(math.Floor(pos))
		x := pos - float64(k)
		x2 := x * x
		x3 := x2 * x

		for ch := 0; ch < r.channels; ch++ {
			s0 := sampleAt(k-1, ch)
			s1 := sampleAt(k, ch)
			s2 := sampleAt(k+1, ch)
			s3 := sampleAt(k+2, ch)

			// 4-point 3rd-order Catmull-Rom cubic Hermite spline:
			// c0 = s1
			// c1 = 0.5 * (s2 - s0)
			// c2 = s0 - 2.5 * s1 + 2.0 * s2 - 0.5 * s3
			// c3 = 0.5 * (s3 - s0) + 1.5 * (s1 - s2)
			c0 := s1
			c1 := 0.5 * (s2 - s0)
			c2 := s0 - 2.5*s1 + 2.0*s2 - 0.5*s3
			c3 := 0.5*(s3-s0) + 1.5*(s1-s2)

			interp := c3*x3 + c2*x2 + c1*x + c0
			if interp > 32767.0 {
				interp = 32767.0
			} else if interp < -32768.0 {
				interp = -32768.0
			}

			val := int16(interp)
			off := (outFrames*r.channels + ch) * 2
			binary.LittleEndian.PutUint16(outPCM[off:], uint16(val))
		}

		outFrames++
		pos += step
	}

	// Carry over fractional phase
	r.phase = pos - float64(inFrames)
	if math.IsNaN(r.phase) || math.IsInf(r.phase, 0) || r.phase < 0 {
		r.phase = 0
	}

	// Store last up to 3 frames of inPCM into history for next packet
	for h := 0; h < 3; h++ {
		srcFrame := inFrames - 3 + h
		for ch := 0; ch < r.channels; ch++ {
			if srcFrame >= 0 {
				off := (srcFrame*r.channels + ch) * 2
				r.history[h*r.channels+ch] = int16(binary.LittleEndian.Uint16(inPCM[off:]))
			} else {
				r.history[h*r.channels+ch] = 0
			}
		}
	}
	r.hasHistory = true
	r.samplesAdj.Add(uint64(outFrames))

	return outFrames * frameBytes
}
