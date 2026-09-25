package dsp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Channel layout standard channel counts.
const (
	LayoutMono   = 1
	LayoutStereo = 2
	LayoutQuad   = 4
	Layout51     = 6
	Layout71     = 8
)

// ChannelMatrix defines the interface for multi-channel audio matrix mixing.
type ChannelMatrix interface {
	InChannels() int
	OutChannels() int
	Transform(inPCM, outPCM []byte) (int, error)
}

// MatrixMixer implements ChannelMatrix using a floating-point coefficient matrix
// with branchless/bounded clamping to prevent integer overflow.
type MatrixMixer struct {
	inChannels  int
	outChannels int
	flatWeights []float32 // row-major flat matrix of size outChannels * inChannels
}

// InChannels returns the number of input channels expected by this matrix.
func (m *MatrixMixer) InChannels() int {
	return m.inChannels
}

// OutChannels returns the number of output channels produced by this matrix.
func (m *MatrixMixer) OutChannels() int {
	return m.outChannels
}

// clampInt16 rounds and clamps a float32 sample value to the valid 16-bit signed
// PCM integer range [-32768, 32767] with zero possibility of integer wrap-around.
func clampInt16(v float32) int16 {
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

// Transform mixes inPCM (16-bit little-endian signed PCM with InChannels interleaved)
// into outPCM (16-bit little-endian signed PCM with OutChannels interleaved).
// It performs zero heap allocations if outPCM has sufficient capacity.
// Returns the number of bytes written to outPCM.
func (m *MatrixMixer) Transform(inPCM, outPCM []byte) (int, error) {
	if len(inPCM) == 0 {
		return 0, nil
	}
	bytesPerSample := 2
	inFrameBytes := m.inChannels * bytesPerSample
	outFrameBytes := m.outChannels * bytesPerSample

	if len(inPCM)%inFrameBytes != 0 {
		return 0, fmt.Errorf("matrix: input length %d is not a multiple of frame size %d", len(inPCM), inFrameBytes)
	}

	numFrames := len(inPCM) / inFrameBytes
	requiredOut := numFrames * outFrameBytes
	if len(outPCM) < requiredOut {
		return 0, fmt.Errorf("matrix: output buffer too small (need %d, have %d)", requiredOut, len(outPCM))
	}

	inCh := m.inChannels
	outCh := m.outChannels
	weights := m.flatWeights

	for f := 0; f < numFrames; f++ {
		inOffset := f * inFrameBytes
		outOffset := f * outFrameBytes

		for o := 0; o < outCh; o++ {
			var acc float32
			wBase := o * inCh
			for i := 0; i < inCh; i++ {
				w := weights[wBase+i]
				if w == 0 {
					continue
				}
				sampleOffset := inOffset + i*2
				sample := int16(binary.LittleEndian.Uint16(inPCM[sampleOffset : sampleOffset+2]))
				acc += w * float32(sample)
			}
			clamped := clampInt16(acc)
			destOffset := outOffset + o*2
			binary.LittleEndian.PutUint16(outPCM[destOffset:destOffset+2], uint16(clamped))
		}
	}

	return requiredOut, nil
}

// NewCustomMatrix creates a MatrixMixer with explicit user-provided weights.
// weights must have dimension [outChannels][inChannels].
func NewCustomMatrix(inChannels, outChannels int, weights [][]float32) (*MatrixMixer, error) {
	if inChannels <= 0 || outChannels <= 0 {
		return nil, errors.New("matrix: channel counts must be greater than zero")
	}
	if len(weights) != outChannels {
		return nil, fmt.Errorf("matrix: weights row count %d does not match outChannels %d", len(weights), outChannels)
	}
	flat := make([]float32, outChannels*inChannels)
	for o := 0; o < outChannels; o++ {
		if len(weights[o]) != inChannels {
			return nil, fmt.Errorf("matrix: weights col count %d does not match inChannels %d at row %d", len(weights[o]), inChannels, o)
		}
		copy(flat[o*inChannels:(o+1)*inChannels], weights[o])
	}
	return &MatrixMixer{
		inChannels:  inChannels,
		outChannels: outChannels,
		flatWeights: flat,
	}, nil
}

// NewChannelMatrix creates a ChannelMatrix implementing standard ITU-R BS.775
// and SMPTE channel layout transformations, or an energy-normalized fallback
// for arbitrary N <-> M channel configurations.
func NewChannelMatrix(inChannels, outChannels int) (ChannelMatrix, error) {
	if inChannels <= 0 || outChannels <= 0 {
		return nil, errors.New("matrix: channel counts must be greater than zero")
	}

	// 1. Identity Matrix (N == M)
	if inChannels == outChannels {
		weights := make([][]float32, outChannels)
		for o := 0; o < outChannels; o++ {
			weights[o] = make([]float32, inChannels)
			weights[o][o] = 1.0
		}
		return NewCustomMatrix(inChannels, outChannels, weights)
	}

	// 2. Mono <-> Stereo (1 <-> 2)
	if inChannels == LayoutMono && outChannels == LayoutStereo {
		// Mono -> Stereo: duplicate mono to Left and Right
		return NewCustomMatrix(1, 2, [][]float32{
			{1.0}, // L
			{1.0}, // R
		})
	}
	if inChannels == LayoutStereo && outChannels == LayoutMono {
		// Stereo -> Mono: average L and R
		return NewCustomMatrix(2, 1, [][]float32{
			{0.5, 0.5}, // M = 0.5*L + 0.5*R
		})
	}

	// 3. Stereo <-> Quad (2 <-> 4)
	// Quad layout: [FL, FR, SL, SR]
	if inChannels == LayoutStereo && outChannels == LayoutQuad {
		// Stereo -> Quad: FL=L, FR=R, SL=0.7071*L, SR=0.7071*R
		const invSqrt2 = float32(0.70710678)
		return NewCustomMatrix(2, 4, [][]float32{
			{1.0, 0.0},      // FL
			{0.0, 1.0},      // FR
			{invSqrt2, 0.0}, // SL
			{0.0, invSqrt2}, // SR
		})
	}
	if inChannels == LayoutQuad && outChannels == LayoutStereo {
		// Quad -> Stereo:
		// L = k_norm * (FL + 0.7071*SL), R = k_norm * (FR + 0.7071*SR)
		// k_norm = 1.0 / (1.0 + 0.70710678) ≈ 0.58578644
		const kNorm = float32(1.0 / (1.0 + 0.70710678))
		const sNorm = kNorm * float32(0.70710678)
		return NewCustomMatrix(4, 2, [][]float32{
			{kNorm, 0.0, sNorm, 0.0}, // L
			{0.0, kNorm, 0.0, sNorm}, // R
		})
	}

	// 4. Stereo <-> 5.1 Surround (2 <-> 6)
	// 5.1 layout: [FL, FR, FC, LFE, SL, SR]
	if inChannels == LayoutStereo && outChannels == Layout51 {
		// Stereo -> 5.1 Upmix as defined in ADR 0003:
		// FL  = 1.0 * L + 0.0 * R
		// FR  = 0.0 * L + 1.0 * R
		// FC  = 0.5 * L + 0.5 * R
		// LFE = 0.0 * L + 0.0 * R
		// SL  = 0.7071 * L - 0.7071 * R
		// SR  = -0.7071 * L + 0.7071 * R
		const invSqrt2 = float32(0.70710678)
		return NewCustomMatrix(2, 6, [][]float32{
			{1.0, 0.0},           // FL
			{0.0, 1.0},           // FR
			{0.5, 0.5},           // FC
			{0.0, 0.0},           // LFE
			{invSqrt2, -invSqrt2}, // SL
			{-invSqrt2, invSqrt2}, // SR
		})
	}
	if inChannels == Layout51 && outChannels == LayoutStereo {
		// 5.1 -> Stereo Downmix as defined in ITU-R BS.775:
		// L = k_norm * (FL + 0.7071*FC + 0.7071*SL)
		// R = k_norm * (FR + 0.7071*FC + 0.7071*SR)
		// LFE omitted (0.0).
		// k_norm = 1.0 / (1.0 + sqrt(2)) ≈ 0.41421356
		const kNorm = float32(1.0 / (1.0 + math.Sqrt2))
		const cNorm = kNorm * float32(1.0/math.Sqrt2)
		return NewCustomMatrix(6, 2, [][]float32{
			{kNorm, 0.0, cNorm, 0.0, cNorm, 0.0}, // L
			{0.0, kNorm, cNorm, 0.0, 0.0, cNorm}, // R
		})
	}

	// 5. Stereo <-> 7.1 Surround (2 <-> 8)
	// 7.1 layout: [FL, FR, FC, LFE, SL, SR, BL, BR]
	if inChannels == LayoutStereo && outChannels == Layout71 {
		const invSqrt2 = float32(0.70710678)
		return NewCustomMatrix(2, 8, [][]float32{
			{1.0, 0.0},      // FL
			{0.0, 1.0},      // FR
			{0.5, 0.5},      // FC
			{0.0, 0.0},      // LFE
			{invSqrt2, 0.0}, // SL
			{0.0, invSqrt2}, // SR
			{0.5, 0.0},      // BL
			{0.0, 0.5},      // BR
		})
	}
	if inChannels == Layout71 && outChannels == LayoutStereo {
		// 7.1 -> Stereo Downmix:
		// L = k_norm * (FL + 0.7071*FC + 0.7071*SL + 0.7071*BL)
		// R = k_norm * (FR + 0.7071*FC + 0.7071*SR + 0.7071*BR)
		// k_norm = 1.0 / (1.0 + 3*0.70710678) ≈ 0.32037724
		const invSqrt2 = float32(0.70710678)
		const kNorm = float32(1.0 / (1.0 + 3.0*float64(invSqrt2)))
		const att = kNorm * invSqrt2
		return NewCustomMatrix(8, 2, [][]float32{
			{kNorm, 0.0, att, 0.0, att, 0.0, att, 0.0}, // L
			{0.0, kNorm, att, 0.0, 0.0, att, 0.0, att}, // R
		})
	}

	// 6. Arbitrary N <-> M Fallback Matrix
	// Guarantees row sums <= 1.0 to prevent clipping on full-scale inputs.
	weights := make([][]float32, outChannels)
	for o := 0; o < outChannels; o++ {
		weights[o] = make([]float32, inChannels)
	}

	if inChannels == 1 {
		// Mono -> M: distribute mono to all outputs with unity gain
		for o := 0; o < outChannels; o++ {
			weights[o][0] = 1.0
		}
		return NewCustomMatrix(inChannels, outChannels, weights)
	}

	if outChannels == 1 {
		// N -> Mono: average all inputs with equal weight
		w := float32(1.0 / float64(inChannels))
		for i := 0; i < inChannels; i++ {
			weights[0][i] = w
		}
		return NewCustomMatrix(inChannels, outChannels, weights)
	}

	// General N <-> M:
	// Map inputs to outputs cyclically (input i maps to output i % outChannels).
	// Accumulate counts per output channel to normalize weights.
	counts := make([]int, outChannels)
	for i := 0; i < inChannels; i++ {
		target := i % outChannels
		counts[target]++
	}
	for i := 0; i < inChannels; i++ {
		target := i % outChannels
		weights[target][i] = float32(1.0 / float64(counts[target]))
	}

	return NewCustomMatrix(inChannels, outChannels, weights)
}
