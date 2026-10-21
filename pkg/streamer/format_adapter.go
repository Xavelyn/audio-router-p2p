package streamer

import (
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
	asrc     *dsp.ASRCResampler
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
		asrc:        dsp.NewASRCResampler(outChannels),
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

	// Step 2: Dynamic ASRC Resampling using Catmull-Rom cubic interpolation
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

	step := float64(inRate) / float64(a.outRate)
	outFrames := int(math.Round(float64(inFrames) / step))
	if outFrames <= 0 {
		return a.outBuf[:0], nil
	}

	outBytes := outFrames * a.outChannels * bytesPerSample
	if len(a.outBuf) < outBytes {
		a.outBuf = make([]byte, outBytes*2)
	}

	if a.asrc == nil {
		a.asrc = dsp.NewASRCResampler(a.outChannels)
	}

	n := a.asrc.Process(remapSrc, a.outBuf[:outBytes], step)
	return a.outBuf[:n], nil
}
