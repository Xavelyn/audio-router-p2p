package router

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
)

// ChannelLayout represents canonical audio channel topologies.
type ChannelLayout string

const (
	LayoutMono   ChannelLayout = "mono"
	LayoutStereo ChannelLayout = "stereo"
	LayoutQuad   ChannelLayout = "quad"
	Layout51     ChannelLayout = "5.1"
	Layout71     ChannelLayout = "7.1"
	LayoutCustom ChannelLayout = "custom"
)

// Channels returns the expected channel count for standard layouts.
func (l ChannelLayout) Channels() int {
	switch l {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case LayoutQuad:
		return 4
	case Layout51:
		return 6
	case Layout71:
		return 8
	default:
		return 0
	}
}

// ParseChannelLayout maps a string to a valid ChannelLayout.
func ParseChannelLayout(s string) ChannelLayout {
	switch s {
	case "mono":
		return LayoutMono
	case "stereo":
		return LayoutStereo
	case "quad":
		return LayoutQuad
	case "5.1":
		return Layout51
	case "7.1":
		return Layout71
	default:
		return LayoutCustom
	}
}

// MatrixEndpoint identifies an audio endpoint (source or destination) in the matrix.
type MatrixEndpoint struct {
	NodeID   string        `json:"node_id"`
	DeviceID string        `json:"device_id"`
	Layout   ChannelLayout `json:"layout,omitempty"`
	Channels []int         `json:"channels"`
}

// RouteSource and RouteDestination are aliases for MatrixEndpoint.
type RouteSource = MatrixEndpoint
type RouteDestination = MatrixEndpoint

// FormatConversion specifies sample rate, channel count, and layout adaptations between endpoints.
type FormatConversion struct {
	InSampleRate  int           `json:"in_sample_rate"`
	OutSampleRate int           `json:"out_sample_rate"`
	InLayout      ChannelLayout `json:"in_layout,omitempty"`
	OutLayout     ChannelLayout `json:"out_layout,omitempty"`
	InChannels    int           `json:"in_channels"`
	OutChannels   int           `json:"out_channels"`
}

// NeedsResampling returns true if sample rate conversion is required.
func (fc *FormatConversion) NeedsResampling() bool {
	return fc.InSampleRate > 0 && fc.OutSampleRate > 0 && fc.InSampleRate != fc.OutSampleRate
}

// NeedsRemixing returns true if channel count conversion is required.
func (fc *FormatConversion) NeedsRemixing() bool {
	return fc.InChannels > 0 && fc.OutChannels > 0 && fc.InChannels != fc.OutChannels
}

// Validate checks that sample rates and channel counts are positive.
func (fc *FormatConversion) Validate() error {
	if fc.InChannels <= 0 || fc.OutChannels <= 0 {
		return errors.New("channel counts must be positive")
	}
	if fc.InSampleRate <= 0 || fc.OutSampleRate <= 0 {
		return errors.New("sample rates must be positive")
	}
	return nil
}

// MatrixRoute defines an arbitrary N:M matrix routing session between endpoints.
type MatrixRoute struct {
	ID            string           `json:"id"`
	Name          string           `json:"name,omitempty"`
	Source        RouteSource      `json:"source"`
	Destination   RouteDestination `json:"destination"`
	InChannels    int              `json:"in_channels"`
	OutChannels   int              `json:"out_channels"`
	Coefficients  []float64        `json:"coefficients"`
	SlewMS        float64          `json:"slew_ms"`
	Muted         bool             `json:"muted"`
	State         string           `json:"state"`                    // active, slewing, paused, error
	LatencyPreset string           `json:"latency_preset,omitempty"` // low, balanced, reliable

	// ChannelDelaysMS holds the per-channel alignment offset in milliseconds,
	// applied before the gain matrix (see StreamRoute.ChannelDelaysMS).
	ChannelDelaysMS []float64 `json:"channel_delays_ms,omitempty"`
}

// Validate verifies dimensions, coefficient ranges [0.0, 2.0], and channel counts.
func (r *MatrixRoute) Validate() error {
	if r.ID == "" {
		return errors.New("route id is required")
	}
	if r.InChannels <= 0 {
		return fmt.Errorf("in_channels must be positive, got %d", r.InChannels)
	}
	if r.OutChannels <= 0 {
		return fmt.Errorf("out_channels must be positive, got %d", r.OutChannels)
	}
	expectedCoeffs := r.InChannels * r.OutChannels
	if len(r.Coefficients) != expectedCoeffs {
		return fmt.Errorf("coefficients length mismatch: expected %d (%d x %d), got %d",
			expectedCoeffs, r.OutChannels, r.InChannels, len(r.Coefficients))
	}
	for i, c := range r.Coefficients {
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0.0 || c > 2.0 {
			return fmt.Errorf("coefficient at index %d out of bounds [0.0, 2.0]: %f", i, c)
		}
	}
	if r.SlewMS < 0 {
		return fmt.Errorf("slew_ms must be non-negative, got %f", r.SlewMS)
	}
	return nil
}

// SetCrosspoint updates an individual crosspoint gain with default 20ms slewing.
func (r *MatrixRoute) SetCrosspoint(outCh, inCh int, gain float32) error {
	if outCh < 0 || outCh >= r.OutChannels || inCh < 0 || inCh >= r.InChannels {
		return fmt.Errorf("crosspoint out of range: out=%d, in=%d", outCh, inCh)
	}
	if math.IsNaN(float64(gain)) || math.IsInf(float64(gain), 0) || gain < 0.0 || gain > 2.0 {
		return fmt.Errorf("gain out of bounds [0.0, 2.0]: %f", gain)
	}
	r.Coefficients[outCh*r.InChannels+inCh] = float64(gain)
	r.SlewMS = 20.0
	r.State = "slewing"
	return nil
}

// SetLiveGain updates the full coefficient matrix and transitions smoothly over transitionMs milliseconds.
func (r *MatrixRoute) SetLiveGain(matrix GainMatrix, transitionMs int) error {
	if err := matrix.Validate(); err != nil {
		return err
	}
	if matrix.InChannels != r.InChannels || matrix.OutChannels != r.OutChannels {
		return fmt.Errorf("matrix dimensions mismatch: expected %dx%d, got %dx%d",
			r.OutChannels, r.InChannels, matrix.OutChannels, matrix.InChannels)
	}
	if transitionMs <= 0 {
		transitionMs = 20
	}
	r.Coefficients = make([]float64, len(matrix.Coefficients))
	copy(r.Coefficients, matrix.Coefficients)
	r.SlewMS = float64(transitionMs)
	r.State = "slewing"
	return nil
}

// GainMatrix holds an N x M row-major gain coefficient matrix.
type GainMatrix struct {
	InChannels   int       `json:"in_channels"`
	OutChannels  int       `json:"out_channels"`
	Coefficients []float64 `json:"coefficients"`
}

// NewGainMatrix creates and validates a new GainMatrix.
func NewGainMatrix(inChannels, outChannels int, coefficients []float64) (*GainMatrix, error) {
	gm := &GainMatrix{
		InChannels:   inChannels,
		OutChannels:  outChannels,
		Coefficients: coefficients,
	}
	if err := gm.Validate(); err != nil {
		return nil, err
	}
	return gm, nil
}

// NewIdentityGainMatrix creates an N x N identity matrix.
func NewIdentityGainMatrix(channels int) *GainMatrix {
	if channels <= 0 {
		channels = 2
	}
	coeffs := make([]float64, channels*channels)
	for i := 0; i < channels; i++ {
		coeffs[i*channels+i] = 1.0
	}
	return &GainMatrix{
		InChannels:   channels,
		OutChannels:  channels,
		Coefficients: coeffs,
	}
}

// NewDefaultDownmixMatrix creates standard ITU/SMPTE or energy-normalized downmix/upmix matrices.
func NewDefaultDownmixMatrix(inChannels, outChannels int) *GainMatrix {
	if inChannels <= 0 {
		inChannels = 2
	}
	if outChannels <= 0 {
		outChannels = 2
	}
	coeffs := make([]float64, outChannels*inChannels)

	if inChannels == outChannels {
		for i := 0; i < inChannels; i++ {
			coeffs[i*inChannels+i] = 1.0
		}
	} else if inChannels == 1 && outChannels == 2 {
		// Mono -> Stereo
		coeffs[0] = 1.0
		coeffs[1] = 1.0
	} else if inChannels == 2 && outChannels == 1 {
		// Stereo -> Mono (0.5 L + 0.5 R)
		coeffs[0] = 0.5
		coeffs[1] = 0.5
	} else if inChannels == 4 && outChannels == 2 {
		// Quad -> Stereo
		const kNorm = 1.0 / (1.0 + 0.70710678)
		const sNorm = kNorm * 0.70710678
		// L = kNorm*FL + sNorm*SL; R = kNorm*FR + sNorm*SR
		coeffs[0*4+0] = kNorm
		coeffs[0*4+2] = sNorm
		coeffs[1*4+1] = kNorm
		coeffs[1*4+3] = sNorm
	} else if inChannels == 2 && outChannels == 4 {
		// Stereo -> Quad
		const invSqrt2 = 0.70710678
		coeffs[0*2+0] = 1.0
		coeffs[1*2+1] = 1.0
		coeffs[2*2+0] = invSqrt2
		coeffs[3*2+1] = invSqrt2
	} else {
		// General normalized fallback
		scale := 1.0 / math.Sqrt(float64(inChannels))
		if scale > 1.0 {
			scale = 1.0
		}
		for o := 0; o < outChannels; o++ {
			inIdx := o % inChannels
			coeffs[o*inChannels+inIdx] = scale
		}
	}

	return &GainMatrix{
		InChannels:   inChannels,
		OutChannels:  outChannels,
		Coefficients: coeffs,
	}
}

// Validate checks GainMatrix dimensions and bounds.
func (gm *GainMatrix) Validate() error {
	if gm.InChannels <= 0 {
		return fmt.Errorf("in_channels must be positive, got %d", gm.InChannels)
	}
	if gm.OutChannels <= 0 {
		return fmt.Errorf("out_channels must be positive, got %d", gm.OutChannels)
	}
	expected := gm.InChannels * gm.OutChannels
	if len(gm.Coefficients) != expected {
		return fmt.Errorf("coefficients length mismatch: expected %d, got %d", expected, len(gm.Coefficients))
	}
	for i, c := range gm.Coefficients {
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0.0 || c > 2.0 {
			return fmt.Errorf("coefficient at %d out of bounds [0.0, 2.0]: %f", i, c)
		}
	}
	return nil
}

// At returns the gain coefficient at (outCh, inCh).
func (gm *GainMatrix) At(outCh, inCh int) float64 {
	if outCh < 0 || outCh >= gm.OutChannels || inCh < 0 || inCh >= gm.InChannels {
		return 0.0
	}
	return gm.Coefficients[outCh*gm.InChannels+inCh]
}

// Set updates the gain coefficient at (outCh, inCh).
func (gm *GainMatrix) Set(outCh, inCh int, val float64) error {
	if outCh < 0 || outCh >= gm.OutChannels || inCh < 0 || inCh >= gm.InChannels {
		return fmt.Errorf("crosspoint out of range: out=%d, in=%d", outCh, inCh)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) || val < 0.0 || val > 2.0 {
		return fmt.Errorf("coefficient out of bounds [0.0, 2.0]: %f", val)
	}
	gm.Coefficients[outCh*gm.InChannels+inCh] = val
	return nil
}

// Float32Coefficients returns a slice of float32 coefficients.
func (gm *GainMatrix) Float32Coefficients() []float32 {
	res := make([]float32, len(gm.Coefficients))
	for i, v := range gm.Coefficients {
		res[i] = float32(v)
	}
	return res
}

// ClampInt16 performs branchless/bounded saturation clamping to signed 16-bit PCM integer
// boundaries [-32768, 32767] with half-up rounding.
func ClampInt16(v float32) int16 {
	if v > 32767.0 {
		return 32767
	}
	if v < -32768.0 {
		return -32768
	}
	if v >= 0 {
		return int16(v + 0.5)
	}
	return int16(v - 0.5)
}

// Evaluate applies the static GainMatrix to inPCM (interleaved 16-bit LE PCM)
// writing into outPCM with zero steady-state heap allocations.
func (gm *GainMatrix) Evaluate(inPCM, outPCM []byte) (int, error) {
	if len(inPCM) == 0 {
		return 0, nil
	}
	inFrameBytes := gm.InChannels * 2
	outFrameBytes := gm.OutChannels * 2

	if len(inPCM)%inFrameBytes != 0 {
		return 0, fmt.Errorf("matrix: input length %d is not a multiple of frame size %d", len(inPCM), inFrameBytes)
	}
	numFrames := len(inPCM) / inFrameBytes
	requiredOut := numFrames * outFrameBytes
	if len(outPCM) < requiredOut {
		return 0, fmt.Errorf("matrix: output buffer too small (need %d, have %d)", requiredOut, len(outPCM))
	}

	inCh := gm.InChannels
	outCh := gm.OutChannels
	coeffs := gm.Coefficients

	for f := 0; f < numFrames; f++ {
		inOffset := f * inFrameBytes
		outOffset := f * outFrameBytes

		for o := 0; o < outCh; o++ {
			var acc float32
			wBase := o * inCh
			for i := 0; i < inCh; i++ {
				w := float32(coeffs[wBase+i])
				if w == 0 {
					continue
				}
				sOff := inOffset + i*2
				sample := int16(binary.LittleEndian.Uint16(inPCM[sOff : sOff+2]))
				acc += w * float32(sample)
			}
			clamped := ClampInt16(acc)
			dOff := outOffset + o*2
			binary.LittleEndian.PutUint16(outPCM[dOff:dOff+2], uint16(clamped))
		}
	}

	return requiredOut, nil
}

// SlewGainMatrix manages real-time linear gain slewing over a sample block duration
// (e.g., 20ms = 960 samples @ 48kHz) to eliminate digital clicks and zipper artifacts.
type SlewGainMatrix struct {
	mu               sync.RWMutex
	sampleRate       int
	inChannels       int
	outChannels      int
	currentWeights   []float32
	targetWeights    []float32
	deltaWeights     []float32
	remainingSamples int
	totalSlewSamples int
	slewing          bool
}

// NewSlewGainMatrix initializes a SlewGainMatrix with initial coefficients at default 48kHz sample rate.
func NewSlewGainMatrix(inChannels, outChannels int, initial []float32) (*SlewGainMatrix, error) {
	return NewSlewGainMatrixWithSampleRate(inChannels, outChannels, 48000, initial)
}

// NewSlewGainMatrixWithSampleRate initializes a SlewGainMatrix with initial coefficients and custom sample rate.
func NewSlewGainMatrixWithSampleRate(inChannels, outChannels, sampleRate int, initial []float32) (*SlewGainMatrix, error) {
	if inChannels <= 0 || outChannels <= 0 {
		return nil, errors.New("channel counts must be positive")
	}
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	expected := inChannels * outChannels
	if len(initial) != expected {
		return nil, fmt.Errorf("initial coefficients length mismatch: expected %d, got %d", expected, len(initial))
	}

	curr := make([]float32, expected)
	target := make([]float32, expected)
	delta := make([]float32, expected)
	copy(curr, initial)
	copy(target, initial)

	return &SlewGainMatrix{
		sampleRate:     sampleRate,
		inChannels:     inChannels,
		outChannels:    outChannels,
		currentWeights: curr,
		targetWeights:  target,
		deltaWeights:   delta,
		slewing:        false,
	}, nil
}

// SampleRate returns the configured sample rate in Hz.
func (s *SlewGainMatrix) SampleRate() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sampleRate
}

// SetSampleRate updates the configured sample rate in Hz.
func (s *SlewGainMatrix) SetSampleRate(rate int) {
	if rate <= 0 {
		rate = 48000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampleRate = rate
}

func (s *SlewGainMatrix) slewSamplesForDuration(ms int) int {
	if ms <= 0 {
		ms = 20
	}
	return (ms * s.sampleRate) / 1000
}

func (s *SlewGainMatrix) initiateSlewLocked(slewSamples int) error {
	expected := s.inChannels * s.outChannels
	if slewSamples <= 0 {
		// Snap immediately
		copy(s.currentWeights, s.targetWeights)
		for i := range s.deltaWeights {
			s.deltaWeights[i] = 0
		}
		s.slewing = false
		s.remainingSamples = 0
		s.totalSlewSamples = 0
		return nil
	}

	s.remainingSamples = slewSamples
	s.totalSlewSamples = slewSamples
	s.slewing = true

	stepScale := 1.0 / float32(slewSamples)
	for i := 0; i < expected; i++ {
		s.deltaWeights[i] = (s.targetWeights[i] - s.currentWeights[i]) * stepScale
	}
	return nil
}

// SetTarget updates the target matrix coefficients and engages linear slewing over slewSamples.
// For 48kHz and 20ms, slewSamples is 960.
func (s *SlewGainMatrix) SetTarget(target []float32, slewSamples int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	expected := s.inChannels * s.outChannels
	if len(target) != expected {
		return fmt.Errorf("target coefficients length mismatch: expected %d, got %d", expected, len(target))
	}
	for i, c := range target {
		if math.IsNaN(float64(c)) || math.IsInf(float64(c), 0) || c < 0.0 || c > 2.0 {
			return fmt.Errorf("target coefficient at index %d out of bounds [0.0, 2.0]: %f", i, c)
		}
	}

	copy(s.targetWeights, target)
	return s.initiateSlewLocked(slewSamples)
}

// SetCrosspoint updates an individual crosspoint (outCh, inCh) target gain and begins 20ms linear slewing.
// Over 48kHz sample rate, 20ms linearly interpolates over 960 samples to prevent acoustic pops.
func (s *SlewGainMatrix) SetCrosspoint(outCh, inCh int, gain float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if outCh < 0 || outCh >= s.outChannels || inCh < 0 || inCh >= s.inChannels {
		return fmt.Errorf("crosspoint out of range: out=%d, in=%d", outCh, inCh)
	}
	if math.IsNaN(float64(gain)) || math.IsInf(float64(gain), 0) || gain < 0.0 || gain > 2.0 {
		return fmt.Errorf("gain out of bounds [0.0, 2.0]: %f", gain)
	}

	idx := outCh*s.inChannels + inCh
	s.targetWeights[idx] = gain
	slewSamples := s.slewSamplesForDuration(20)
	return s.initiateSlewLocked(slewSamples)
}

// SetLiveGain updates the full coefficient matrix and transitions smoothly over transitionMs milliseconds.
func (s *SlewGainMatrix) SetLiveGain(matrix GainMatrix, transitionMs int) error {
	if err := matrix.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if matrix.InChannels != s.inChannels || matrix.OutChannels != s.outChannels {
		return fmt.Errorf("matrix dimensions mismatch: expected %dx%d, got %dx%d",
			s.outChannels, s.inChannels, matrix.OutChannels, matrix.InChannels)
	}

	for i, c := range matrix.Coefficients {
		s.targetWeights[i] = float32(c)
	}

	if transitionMs <= 0 {
		transitionMs = 20
	}
	slewSamples := s.slewSamplesForDuration(transitionMs)
	return s.initiateSlewLocked(slewSamples)
}

// IsSlewing returns whether the matrix is actively interpolating coefficients.
func (s *SlewGainMatrix) IsSlewing() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.slewing
}

// CurrentCoefficients returns a snapshot of active coefficients.
func (s *SlewGainMatrix) CurrentCoefficients() []float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]float32, len(s.currentWeights))
	copy(out, s.currentWeights)
	return out
}

// Process transforms inPCM into outPCM, applying per-sample linear slewing if active.
// Performs zero dynamic heap allocations.
func (s *SlewGainMatrix) Process(inPCM, outPCM []byte) (int, error) {
	if len(inPCM) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	inFrameBytes := s.inChannels * 2
	outFrameBytes := s.outChannels * 2

	if len(inPCM)%inFrameBytes != 0 {
		return 0, fmt.Errorf("matrix: input length %d is not a multiple of frame size %d", len(inPCM), inFrameBytes)
	}

	numFrames := len(inPCM) / inFrameBytes
	requiredOut := numFrames * outFrameBytes
	if len(outPCM) < requiredOut {
		return 0, fmt.Errorf("matrix: output buffer too small (need %d, have %d)", requiredOut, len(outPCM))
	}

	inCh := s.inChannels
	outCh := s.outChannels
	totalCoeffs := inCh * outCh

	for f := 0; f < numFrames; f++ {
		// Slew step per sample frame
		if s.slewing {
			s.remainingSamples--
			if s.remainingSamples <= 0 {
				copy(s.currentWeights, s.targetWeights)
				for i := 0; i < totalCoeffs; i++ {
					s.deltaWeights[i] = 0
				}
				s.slewing = false
			} else {
				for i := 0; i < totalCoeffs; i++ {
					s.currentWeights[i] += s.deltaWeights[i]
				}
			}
		}

		inOffset := f * inFrameBytes
		outOffset := f * outFrameBytes

		for o := 0; o < outCh; o++ {
			var acc float32
			wBase := o * inCh
			for i := 0; i < inCh; i++ {
				w := s.currentWeights[wBase+i]
				if w == 0 {
					continue
				}
				sOff := inOffset + i*2
				sample := int16(binary.LittleEndian.Uint16(inPCM[sOff : sOff+2]))
				acc += w * float32(sample)
			}
			clamped := ClampInt16(acc)
			dOff := outOffset + o*2
			binary.LittleEndian.PutUint16(outPCM[dOff:dOff+2], uint16(clamped))
		}
	}

	return requiredOut, nil
}

// ApplySlewGainMatrix procedural helper for one-shot linear gain slewing over inPCM.
func ApplySlewGainMatrix(current, target []float32, slewSamples int, inPCM, outPCM []byte, inChannels, outChannels int) (int, error) {
	sgm, err := NewSlewGainMatrix(inChannels, outChannels, current)
	if err != nil {
		return 0, err
	}
	if err := sgm.SetTarget(target, slewSamples); err != nil {
		return 0, err
	}
	return sgm.Process(inPCM, outPCM)
}
