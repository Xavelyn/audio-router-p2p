package dsp

import (
	"encoding/binary"
	"math"
	"sync"
)

// PLCStats holds packet loss concealment telemetry.
type PLCStats struct {
	ConcealedFramesTotal         uint64 `json:"concealed_frames_total"`
	PitchDetectedFramesTotal     uint64 `json:"pitch_detected_frames_total"`
	NoiseExtrapolatedFramesTotal uint64 `json:"noise_extrapolated_frames_total"`
	SilenceDecayEventsTotal      uint64 `json:"silence_decay_events_total"`
}

// PSOLAConcealer implements Pitch-Synchronous Overlap-Add packet loss concealment
// for linear 16-bit PCM audio.
// Hot path synthesis methods (Conceal, Recover) run with zero dynamic allocations.
type PSOLAConcealer struct {
	mu         sync.Mutex
	sampleRate int
	channels   int

	minLag int // T_min (500 Hz)
	maxLag int // T_max (50 Hz)
	corrN  int // correlation length N (samples)

	// History buffer storing past linear PCM16 samples per channel
	historyCapacity int // total samples per channel
	historySamples  int // currently stored samples per channel
	history         []int16

	// Scratch buffers for zero-allocation processing
	monoHistory []float64

	// Concealment state
	consecutiveFrames int // k counter
	lastPitchLag      int // P_opt
	lastVoiced        bool
	fadeSamples       int // 8ms fade length in samples

	stats PLCStats
}

// NewPSOLAConcealer creates a new PSOLAConcealer for the specified sample rate and channel count.
func NewPSOLAConcealer(sampleRate, channels int) *PSOLAConcealer {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if channels <= 0 {
		channels = 2
	}

	minLag := sampleRate / 500 // 500 Hz (e.g. 96 @ 48kHz)
	if minLag < 2 {
		minLag = 2
	}
	maxLag := sampleRate / 50 // 50 Hz (e.g. 960 @ 48kHz)
	corrN := maxLag

	// History must hold at least maxLag + corrN + 20ms frame = 3 * maxLag
	historyCapacity := maxLag * 4
	if historyCapacity < 3840 {
		historyCapacity = 3840
	}

	fadeSamples := int(0.008 * float64(sampleRate))
	if fadeSamples < 1 {
		fadeSamples = 1
	}

	return &PSOLAConcealer{
		sampleRate:      sampleRate,
		channels:        channels,
		minLag:          minLag,
		maxLag:          maxLag,
		corrN:           corrN,
		historyCapacity: historyCapacity,
		history:         make([]int16, historyCapacity*channels),
		monoHistory:     make([]float64, historyCapacity),
		fadeSamples:     fadeSamples,
	}
}

// AddHistory appends valid 16-bit PCM audio to the history buffer.
// If concealment was active, call Recover instead to perform smooth crossfading.
func (c *PSOLAConcealer) AddHistory(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.consecutiveFrames = 0
	c.appendHistoryBytes(pcm)
}

// appendHistoryBytes appends PCM16 little-endian bytes to internal history buffer.
// Must be called with c.mu held.
func (c *PSOLAConcealer) appendHistoryBytes(pcm []byte) {
	frameBytes := c.channels * 2
	numSamples := len(pcm) / frameBytes
	if numSamples <= 0 {
		return
	}

	if numSamples >= c.historyCapacity {
		// Replace entire history with tail of pcm
		tailOffset := (numSamples - c.historyCapacity) * frameBytes
		for i := 0; i < c.historyCapacity*c.channels; i++ {
			c.history[i] = int16(binary.LittleEndian.Uint16(pcm[tailOffset+i*2:]))
		}
		c.historySamples = c.historyCapacity
		return
	}

	// If history would overflow, shift existing samples to the left
	if c.historySamples+numSamples > c.historyCapacity {
		excess := (c.historySamples + numSamples) - c.historyCapacity
		copy(c.history, c.history[excess*c.channels:c.historySamples*c.channels])
		c.historySamples -= excess
	}

	// Append new samples
	startIdx := c.historySamples * c.channels
	for i := 0; i < numSamples*c.channels; i++ {
		c.history[startIdx+i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	c.historySamples += numSamples
}

// Conceal synthesizes one missing audio frame into outPCM using PSOLA with
// Hann windowing and exponential energy decay.
// Returns the number of bytes written to outPCM.
func (c *PSOLAConcealer) Conceal(outPCM []byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	frameBytes := c.channels * 2
	numSamples := len(outPCM) / frameBytes
	if numSamples <= 0 {
		return 0
	}

	// If history is insufficient, output silence without engaging waveform concealment
	neededHistory := c.maxLag + c.corrN
	if c.historySamples < neededHistory {
		for i := range outPCM[:numSamples*frameBytes] {
			outPCM[i] = 0
		}
		c.consecutiveFrames = 0
		return numSamples * frameBytes
	}

	c.consecutiveFrames++
	c.stats.ConcealedFramesTotal++

	k := c.consecutiveFrames

	// Frame >= 5 (> 80ms loss): total silence
	if k >= 5 {
		if k == 5 {
			c.stats.SilenceDecayEventsTotal++
		}
		for i := range outPCM[:numSamples*frameBytes] {
			outPCM[i] = 0
		}
		return numSamples * frameBytes
	}

	// Compute gain envelope for frame k
	// k=1: 1.0 -> 0.85
	// k=2: 0.85 -> 0.7225
	// k=3: 0.7225 -> 0.6141
	// k=4: 0.6141 -> 0.0 (linear taper)
	var gStart, gEnd float64
	switch k {
	case 1:
		gStart = 1.0
		gEnd = 0.85
	case 2:
		gStart = 0.85
		gEnd = 0.7225
	case 3:
		gStart = 0.7225
		gEnd = 0.614125
	case 4:
		gStart = 0.614125
		gEnd = 0.0
	default:
		gStart = 0.0
		gEnd = 0.0
	}

	// Compute pitch lag via NCCF on first concealed frame or use cached lag
	pitchLag := c.lastPitchLag
	voiced := c.lastVoiced
	if k == 1 {
		pitchLag, voiced = c.estimatePitchLag()
		c.lastPitchLag = pitchLag
		c.lastVoiced = voiced
	}

	if voiced {
		c.stats.PitchDetectedFramesTotal++
	} else {
		c.stats.NoiseExtrapolatedFramesTotal++
	}

	// Synthesize samples
	H := c.historySamples
	P := pitchLag
	if P < c.minLag {
		P = c.minLag
	}

	invNumSamples := 1.0 / float64(numSamples)
	twoP := 2 * P

	for i := 0; i < numSamples; i++ {
		frac := float64(i) * invNumSamples
		gain := gStart + frac*(gEnd-gStart)

		if voiced {
			// Hann-windowed pitch-synchronous overlap-add
			pIdx := i % P
			wA := 0.5 * (1.0 + math.Cos(math.Pi*float64(pIdx)/float64(P)))
			wB := 0.5 * (1.0 - math.Cos(math.Pi*float64(pIdx)/float64(P)))

			for ch := 0; ch < c.channels; ch++ {
				sA := float64(c.history[(H-P+pIdx)*c.channels+ch])
				sB := float64(c.history[(H-twoP+pIdx)*c.channels+ch])
				synth := (sA*wA + sB*wB) * gain

				if synth > 32767.0 {
					synth = 32767.0
				} else if synth < -32768.0 {
					synth = -32768.0
				}

				val := int16(synth)
				off := (i*c.channels + ch) * 2
				binary.LittleEndian.PutUint16(outPCM[off:], uint16(val))
			}
		} else {
			// Unvoiced / noise extrapolation with sign flipping
			srcIdx := H - 1 - (i % c.maxLag)
			sign := 1.0
			if ((i / 32) & 1) == 1 {
				sign = -1.0
			}
			for ch := 0; ch < c.channels; ch++ {
				s := float64(c.history[srcIdx*c.channels+ch])
				synth := s * sign * gain

				if synth > 32767.0 {
					synth = 32767.0
				} else if synth < -32768.0 {
					synth = -32768.0
				}

				val := int16(synth)
				off := (i*c.channels + ch) * 2
				binary.LittleEndian.PutUint16(outPCM[off:], uint16(val))
			}
		}
	}

	// Update history with the synthesized frame
	c.appendHistoryBytes(outPCM[:numSamples*frameBytes])
	return numSamples * frameBytes
}

// Recover is called when a valid audio frame arrives after concealment was active.
// It executes an 8ms crossfade between the synthetic continuation and the incoming real frame.
// Returns the number of bytes written to outPCM.
func (c *PSOLAConcealer) Recover(realPCM []byte, outPCM []byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	frameBytes := c.channels * 2
	numSamples := len(realPCM) / frameBytes
	if numSamples <= 0 {
		return 0
	}

	outCapSamples := len(outPCM) / frameBytes
	if numSamples > outCapSamples {
		numSamples = outCapSamples
	}

	wasConcealing := c.consecutiveFrames > 0
	c.consecutiveFrames = 0

	if !wasConcealing || c.historySamples < 2*c.maxLag {
		// No concealment was active; pass through real audio directly
		copy(outPCM[:numSamples*frameBytes], realPCM[:numSamples*frameBytes])
		c.appendHistoryBytes(outPCM[:numSamples*frameBytes])
		return numSamples * frameBytes
	}

	fadeLen := c.fadeSamples
	if fadeLen > numSamples {
		fadeLen = numSamples
	}

	H := c.historySamples
	P := c.lastPitchLag
	if P < c.minLag {
		P = c.minLag
	}

	// Crossfade the first fadeLen samples
	invFade := 1.0 / float64(fadeLen)
	for i := 0; i < fadeLen; i++ {
		alpha := float64(i) * invFade // 0.0 -> 1.0
		invAlpha := 1.0 - alpha

		pIdx := i % P
		wA := 0.5 * (1.0 + math.Cos(math.Pi*float64(pIdx)/float64(P)))
		wB := 0.5 * (1.0 - math.Cos(math.Pi*float64(pIdx)/float64(P)))

		idxA := H - P + pIdx
		if idxA < 0 {
			idxA = 0
		}
		idxB := H - 2*P + pIdx
		if idxB < 0 {
			idxB = 0
		}

		for ch := 0; ch < c.channels; ch++ {
			sA := float64(c.history[idxA*c.channels+ch])
			sB := float64(c.history[idxB*c.channels+ch])
			synthSample := sA*wA + sB*wB

			realOff := (i*c.channels + ch) * 2
			realSample := float64(int16(binary.LittleEndian.Uint16(realPCM[realOff:])))

			mixed := invAlpha*synthSample + alpha*realSample
			if mixed > 32767.0 {
				mixed = 32767.0
			} else if mixed < -32768.0 {
				mixed = -32768.0
			}
			outOff := (i*c.channels + ch) * 2
			binary.LittleEndian.PutUint16(outPCM[outOff:], uint16(int16(mixed)))
		}
	}

	// Remainder of frame is purely real audio
	for i := fadeLen; i < numSamples; i++ {
		for ch := 0; ch < c.channels; ch++ {
			off := (i*c.channels + ch) * 2
			val := binary.LittleEndian.Uint16(realPCM[off:])
			binary.LittleEndian.PutUint16(outPCM[off:], val)
		}
	}

	c.appendHistoryBytes(outPCM[:numSamples*frameBytes])
	return numSamples * frameBytes
}

// estimatePitchLag computes the optimal lag via NCCF over [minLag, maxLag].
// Must be called with c.mu held.
func (c *PSOLAConcealer) estimatePitchLag() (int, bool) {
	H := c.historySamples
	N := c.corrN
	invCh := 1.0 / float64(c.channels)

	// Downmix history to mono float64
	for i := 0; i < H; i++ {
		sum := 0.0
		for ch := 0; ch < c.channels; ch++ {
			sum += float64(c.history[i*c.channels+ch])
		}
		c.monoHistory[i] = sum * invCh
	}

	// Compute base energy E0 of the most recent N samples: x[H-N ... H-1]
	startRecent := H - N
	var e0 float64
	for n := 0; n < N; n++ {
		val := c.monoHistory[startRecent+n]
		e0 += val * val
	}

	if e0 < 1.0e-6 {
		return c.minLag, false
	}

	bestLag := c.minLag
	bestR := -1.0

	for lag := c.minLag; lag <= c.maxLag; lag++ {
		startLag := startRecent - lag
		if startLag < 0 {
			break
		}

		var sxy float64
		var eLag float64

		for n := 0; n < N; n++ {
			xRecent := c.monoHistory[startRecent+n]
			xLag := c.monoHistory[startLag+n]
			sxy += xRecent * xLag
			eLag += xLag * xLag
		}

		denom := math.Sqrt(e0 * eLag)
		if denom > 1.0e-6 {
			r := sxy / denom
			if r > bestR {
				bestR = r
				bestLag = lag
			}
		}
	}

	voiced := bestR >= 0.4
	return bestLag, voiced
}

// Reset clears history, concealment counter, and statistics.
func (c *PSOLAConcealer) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.historySamples = 0
	c.consecutiveFrames = 0
	c.lastPitchLag = 0
	c.lastVoiced = false
}

// ConsecutiveConcealed returns the number of consecutive concealed frames.
func (c *PSOLAConcealer) ConsecutiveConcealed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveFrames
}

// Stats returns a copy of PLC metrics.
func (c *PSOLAConcealer) Stats() PLCStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}
