package audio

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"

	"github.com/audio-router-p2p/pkg/audio/dsp"
)

const (
	defaultResampleFrames = 4096
	maxUpsampleMultiplier = 8
)

// resampledAudioStream wraps an underlying AudioStream with a cubic Hermite
// ASRCResampler to adapt mismatched sample rates with zero heap allocations on the hot path.
type resampledAudioStream struct {
	mu         sync.Mutex
	underlying AudioStream
	resampler  *dsp.ASRCResampler
	channels   int
	frameSize  int
	readStep   float64
	writeStep  float64
	closed     bool

	// Preallocated buffers for Read path
	inBuf      []byte
	outBuf     []byte
	spill      []byte
	spillHead  int
	spillTail  int
	inSpill    [16]byte
	inSpillLen int

	// Preallocated buffers for Write path
	writeOutBuf   []byte
	writeSpill    [16]byte
	writeSpillLen int
}

// NewResampleInputStream wraps an underlying input AudioStream running at hwRate
// and exposes it as an AudioStream at requestedRate.
func NewResampleInputStream(underlying AudioStream, hwRate, requestedRate, channels int) (AudioStream, error) {
	if underlying == nil {
		return nil, errors.New("underlying stream cannot be nil")
	}
	if hwRate <= 0 || requestedRate <= 0 {
		return nil, fmt.Errorf("invalid sample rates: hwRate=%d requestedRate=%d", hwRate, requestedRate)
	}
	if channels < 1 || channels > 8 {
		return nil, fmt.Errorf("unsupported channel count %d (must be 1 to 8)", channels)
	}

	frameSize := channels * 2
	step := float64(hwRate) / float64(requestedRate)
	inCap := defaultResampleFrames * frameSize
	outCap := defaultResampleFrames * maxUpsampleMultiplier * frameSize

	return &resampledAudioStream{
		underlying:  underlying,
		resampler:   dsp.NewASRCResampler(channels),
		channels:    channels,
		frameSize:   frameSize,
		readStep:    step,
		inBuf:       make([]byte, inCap),
		outBuf:      make([]byte, outCap),
		spill:       make([]byte, outCap),
		writeOutBuf: make([]byte, outCap),
	}, nil
}

// NewResampleOutputStream wraps an underlying output AudioStream running at hwRate
// and allows callers to write PCM audio at requestedRate.
func NewResampleOutputStream(underlying AudioStream, requestedRate, hwRate, channels int) (AudioStream, error) {
	if underlying == nil {
		return nil, errors.New("underlying stream cannot be nil")
	}
	if hwRate <= 0 || requestedRate <= 0 {
		return nil, fmt.Errorf("invalid sample rates: requestedRate=%d hwRate=%d", requestedRate, hwRate)
	}
	if channels < 1 || channels > 8 {
		return nil, fmt.Errorf("unsupported channel count %d (must be 1 to 8)", channels)
	}

	frameSize := channels * 2
	step := float64(requestedRate) / float64(hwRate)
	inCap := defaultResampleFrames * frameSize
	outCap := defaultResampleFrames * maxUpsampleMultiplier * frameSize

	return &resampledAudioStream{
		underlying:  underlying,
		resampler:   dsp.NewASRCResampler(channels),
		channels:    channels,
		frameSize:   frameSize,
		writeStep:   step,
		inBuf:       make([]byte, inCap),
		outBuf:      make([]byte, outCap),
		spill:       make([]byte, outCap),
		writeOutBuf: make([]byte, outCap),
	}, nil
}

// Read reads from the underlying stream at hwRate, resamples it to requestedRate,
// and fills dst. Residual frames from previous resampling operations are drained first.
// The hot path executes with zero dynamic heap allocations.
func (s *resampledAudioStream) Read(dst []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(dst) == 0 {
		return 0, nil
	}

	// 1. Drain residual spill bytes from previous resample processing
	if s.spillHead < s.spillTail {
		n := copy(dst, s.spill[s.spillHead:s.spillTail])
		s.spillHead += n
		if s.spillHead == s.spillTail {
			s.spillHead = 0
			s.spillTail = 0
		}
		return n, nil
	}

	if s.closed || s.underlying == nil {
		return 0, io.EOF
	}

	if s.readStep <= 0 {
		return s.underlying.Read(dst)
	}

	// 2. Compute how many hardware input frames are needed
	frameSize := s.frameSize
	outFramesNeeded := len(dst) / frameSize
	if outFramesNeeded < 1 {
		outFramesNeeded = 1
	}

	inFramesNeeded := int(math.Ceil(float64(outFramesNeeded)*s.readStep)) + 8
	if inFramesNeeded < 256 {
		inFramesNeeded = 256
	}
	maxInFrames := len(s.inBuf) / frameSize
	if inFramesNeeded > maxInFrames {
		inFramesNeeded = maxInFrames
	}
	inBytesToRead := inFramesNeeded * frameSize

	// Prepend leftover unaligned frame bytes if present
	startOffset := 0
	if s.inSpillLen > 0 {
		copy(s.inBuf[0:], s.inSpill[:s.inSpillLen])
		startOffset = s.inSpillLen
		s.inSpillLen = 0
	}

	readTarget := inBytesToRead
	if readTarget <= startOffset {
		readTarget = startOffset + frameSize
		if readTarget > len(s.inBuf) {
			readTarget = len(s.inBuf)
		}
	}

	n, err := s.underlying.Read(s.inBuf[startOffset:readTarget])
	total := startOffset + n
	if total <= 0 {
		return 0, err
	}

	validBytes := total - (total % frameSize)
	if total%frameSize != 0 {
		s.inSpillLen = copy(s.inSpill[:], s.inBuf[validBytes:total])
	}

	if validBytes == 0 {
		if err != nil {
			return 0, err
		}
		return 0, nil
	}

	outBytes := s.resampler.Process(s.inBuf[:validBytes], s.outBuf, s.readStep)
	if outBytes == 0 {
		if err != nil {
			return 0, err
		}
		return 0, nil
	}

	copied := copy(dst, s.outBuf[:outBytes])
	if copied < outBytes {
		s.spillHead = 0
		s.spillTail = copy(s.spill, s.outBuf[copied:outBytes])
	}

	return copied, nil
}

// Write takes incoming audio at requestedRate, resamples via ASRCResampler,
// and writes the converted stream to the underlying stream at hwRate.
// The hot path executes with zero dynamic heap allocations.
func (s *resampledAudioStream) Write(src []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.underlying == nil {
		return 0, net.ErrClosed
	}
	if len(src) == 0 {
		return 0, nil
	}
	if s.writeStep <= 0 {
		return s.underlying.Write(src)
	}

	frameSize := s.frameSize
	offset := 0

	const maxInChunkFrames = 2048
	maxInChunkBytes := maxInChunkFrames * frameSize

	// Stitch residual unaligned frame bytes from earlier write
	if s.writeSpillLen > 0 {
		// Combine residual spill with incoming src data in inBuf to avoid isolated 1-frame ASRC processing
		copy(s.inBuf[0:], s.writeSpill[:s.writeSpillLen])
		toCopy := maxInChunkBytes - s.writeSpillLen
		if toCopy > len(src) {
			toCopy = len(src)
		}
		copy(s.inBuf[s.writeSpillLen:], src[:toCopy])
		total := s.writeSpillLen + toCopy
		offset = toCopy
		s.writeSpillLen = 0

		validBytes := total - (total % frameSize)
		if total%frameSize != 0 {
			s.writeSpillLen = copy(s.writeSpill[:], s.inBuf[validBytes:total])
		}

		if validBytes > 0 {
			outBytes := s.resampler.Process(s.inBuf[:validBytes], s.writeOutBuf, s.writeStep)
			if outBytes > 0 {
				for written := 0; written < outBytes; {
					nw, err := s.underlying.Write(s.writeOutBuf[written:outBytes])
					if err != nil {
						return 0, err
					}
					written += nw
				}
			}
		}
	}

	for offset < len(src) {
		chunkLen := len(src) - offset
		if chunkLen > maxInChunkBytes {
			chunkLen = maxInChunkBytes
		}
		rem := chunkLen % frameSize
		if rem != 0 {
			chunkLen -= rem
		}
		if chunkLen == 0 {
			// Save residual partial frame bytes
			s.writeSpillLen = copy(s.writeSpill[:], src[offset:])
			break
		}

		inChunk := src[offset : offset+chunkLen]
		outBytes := s.resampler.Process(inChunk, s.writeOutBuf, s.writeStep)
		if outBytes > 0 {
			for written := 0; written < outBytes; {
				nw, err := s.underlying.Write(s.writeOutBuf[written:outBytes])
				if err != nil {
					return offset, err
				}
				written += nw
			}
		}
		offset += chunkLen
	}

	return len(src), nil
}

// SetXRunHook forwards the hook to the underlying stream when it implements
// XRunHookSetter (ADR 0034), so the device-lifetime XRun counters stay
// attached to the physical PortAudio stream across the ASRC wrapper. The
// underlying stream is read under the wrapper lock, mirroring how Read /
// Write / Close access it. Non-implementing underlyings (ghost / emulator /
// virtual) are safely skipped via the optional-interface type assertion.
func (s *resampledAudioStream) SetXRunHook(hook func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.underlying != nil {
		if setter, ok := s.underlying.(XRunHookSetter); ok {
			setter.SetXRunHook(hook)
		}
	}
}

// Close closes the resampler and releases the underlying AudioStream.
func (s *resampledAudioStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	underlying := s.underlying
	s.underlying = nil
	s.mu.Unlock()

	if underlying != nil {
		return underlying.Close()
	}
	return nil
}
