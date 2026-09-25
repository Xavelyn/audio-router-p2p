package streamer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/audio-router-p2p/pkg/audio/dsp"
)

const defaultScratchBytes = 65536 // 64 KB pre-allocated scratch capacity

// IngressAdapter converts incoming audio packets of arbitrary sample rates and
// channel layouts to a fixed target format (outRate, outChannels) in 16-bit PCM.
// It uses pre-allocated scratch buffers for zero-allocation steady-state processing
// and maintains fractional phase across packet boundaries to prevent clicks.
type IngressAdapter struct {
	mu          sync.Mutex
	outRate     int
	outChannels int
	phase       float64

	matrices map[int]dsp.ChannelMatrix // cached matrix per inChannels
	chanBuf  []byte
	outBuf   []byte
}

// NewIngressAdapter creates an IngressAdapter targeting outRate and outChannels.
func NewIngressAdapter(outRate, outChannels int) *IngressAdapter {
	if outRate <= 0 {
		outRate = 48000
	}
	if outChannels <= 0 {
		outChannels = 2
	}
	return &IngressAdapter{
		outRate:     outRate,
		outChannels: outChannels,
		matrices:    make(map[int]dsp.ChannelMatrix),
		chanBuf:     make([]byte, defaultScratchBytes),
		outBuf:      make([]byte, defaultScratchBytes),
	}
}

// Process transforms in (16-bit little-endian PCM) from (inRate, inChannels) into
// (outRate, outChannels). It performs channel matrix mixing followed by linear
// resampling with continuous phase memory.
func (a *IngressAdapter) Process(in []byte, inRate, inChannels int) ([]byte, error) {
	if len(in) == 0 {
		return a.outBuf[:0], nil
	}
	if inRate <= 0 {
		return nil, errors.New("format_adapter: invalid input sample rate")
	}
	if inChannels <= 0 {
		return nil, errors.New("format_adapter: invalid input channel count")
	}
	if a.outChannels <= 0 {
		return nil, errors.New("format_adapter: invalid output channel count")
	}

	bytesPerSample := 2
	inFrameBytes := inChannels * bytesPerSample
	if len(in)%inFrameBytes != 0 {
		return nil, errors.New("format_adapter: input buffer length is not a multiple of frame size")
	}
	inFrames := len(in) / inFrameBytes

	a.mu.Lock()
	defer a.mu.Unlock()

	// Step 1: Channel Matrix Mixing
	var remapSrc []byte
	if inChannels == a.outChannels {
		remapSrc = in
	} else {
		matrix, ok := a.matrices[inChannels]
		if !ok {
			var err error
			matrix, err = dsp.NewChannelMatrix(inChannels, a.outChannels)
			if err != nil {
				return nil, fmt.Errorf("format_adapter: create channel matrix (%d->%d): %w", inChannels, a.outChannels, err)
			}
			a.matrices[inChannels] = matrix
		}

		needed := inFrames * a.outChannels * bytesPerSample
		if len(a.chanBuf) < needed {
			a.chanBuf = make([]byte, needed*2)
		}
		remapSrc = a.chanBuf[:needed]
		if _, err := matrix.Transform(in, remapSrc); err != nil {
			return nil, fmt.Errorf("format_adapter: matrix transform: %w", err)
		}
	}

	// Step 2: Linear Resampling
	if inRate == a.outRate {
		// No resampling needed; pass through remapped audio
		outBytes := inFrames * a.outChannels * bytesPerSample
		if len(a.outBuf) < outBytes {
			a.outBuf = make([]byte, outBytes*2)
		}
		copy(a.outBuf[:outBytes], remapSrc[:outBytes])
		a.phase = 0.0
		return a.outBuf[:outBytes], nil
	}

	// Calculate output frames
	outFrames := int(math.Round(float64(inFrames) * float64(a.outRate) / float64(inRate)))
	if outFrames <= 0 {
		return a.outBuf[:0], nil
	}

	outBytes := outFrames * a.outChannels * bytesPerSample
	if len(a.outBuf) < outBytes {
		a.outBuf = make([]byte, outBytes*2)
	}
	outSlice := a.outBuf[:outBytes]

	step := float64(inRate) / float64(a.outRate)
	pos := a.phase

	sampleAt := func(frameIdx, ch int) int16 {
		if frameIdx < inFrames {
			off := (frameIdx*a.outChannels + ch) * 2
			return int16(binary.LittleEndian.Uint16(remapSrc[off:]))
		}
		if inFrames >= 2 {
			lastOff := ((inFrames-1)*a.outChannels + ch) * 2
			prevOff := ((inFrames-2)*a.outChannels + ch) * 2
			sLast := int32(int16(binary.LittleEndian.Uint16(remapSrc[lastOff:])))
			sPrev := int32(int16(binary.LittleEndian.Uint16(remapSrc[prevOff:])))
			extrap := sLast + (sLast - sPrev)
			if extrap > 32767 {
				extrap = 32767
			} else if extrap < -32768 {
				extrap = -32768
			}
			return int16(extrap)
		}
		if inFrames == 1 {
			off := ch * 2
			return int16(binary.LittleEndian.Uint16(remapSrc[off:]))
		}
		return 0
	}

	for m := 0; m < outFrames; m++ {
		k := int(pos)
		frac := pos - float64(k)

		for c := 0; c < a.outChannels; c++ {
			s0 := float64(sampleAt(k, c))
			s1 := float64(sampleAt(k+1, c))
			interp := s0*(1.0-frac) + s1*frac
			if interp > 32767.0 {
				interp = 32767.0
			} else if interp < -32768.0 {
				interp = -32768.0
			}
			val := int16(math.Round(interp))
			off := (m*a.outChannels + c) * 2
			binary.LittleEndian.PutUint16(outSlice[off:], uint16(val))
		}
		pos += step
	}

	// Carry over fractional phase
	a.phase = pos - float64(inFrames)
	if a.phase < 0 {
		a.phase = 0
	}

	return outSlice, nil
}
