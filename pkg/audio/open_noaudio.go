//go:build noaudio

package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

func isOutputUnderflowError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "underflow")
}

// OpenInput returns a synthetic sine-wave source. The real-device path is not
// available in the noaudio build.
func OpenInput(deviceID int, sampleRate, channels int, useEmulator bool, devNames ...string) (AudioStream, error) {
	if !useEmulator {
		return nil, fmt.Errorf("hardware audio devices not supported in noaudio build (deviceID=%d)", deviceID)
	}
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	if channels < 1 {
		channels = 1
	}
	return newEmulatorInput(deviceID, sampleRate, channels), nil
}

// OpenOutput returns an in-memory sink in the noaudio build.
func OpenOutput(deviceID int, sampleRate, channels int, useEmulator bool, devNames ...string) (AudioStream, error) {
	if !useEmulator {
		return nil, fmt.Errorf("hardware audio devices not supported in noaudio build (deviceID=%d)", deviceID)
	}
	return newEmulatorOutput(deviceID), nil
}

func isPortAudioStreamDirection(s AudioStream) (isInput, isOutput bool) {
	if rs, ok := s.(interface{ Underlying() AudioStream }); ok {
		if inner := rs.Underlying(); inner != nil {
			s = inner
		}
	}
	switch s.(type) {
	case *PrimedBufferedSink:
		return false, true
	}
	return false, false
}

func isOutputUnderflowError(err error) bool {
	return false
}


func buildRateCandidates(requested, defaultRate int, activeRates ...int) []int {
	raw := make([]int, 0, len(activeRates)+5)
	for _, ar := range activeRates {
		if ar > 0 {
			raw = append(raw, ar)
		}
	}
	raw = append(raw, requested, defaultRate, 48000, 44100, 96000)
	candidates := make([]int, 0, len(raw))
	seen := make(map[int]bool, len(raw))
	for _, r := range raw {
		if r > 0 && !seen[r] {
			seen[r] = true
			candidates = append(candidates, r)
		}
	}
	return candidates
}

type portAudioStreamReader interface {
	Read() error
}

type testPAInput struct {
	stream portAudioStreamReader
	buf    []int16
	spill  []byte
	closed bool
}

func (p *testPAInput) Read(dst []byte) (int, error) {
	if len(p.spill) > 0 {
		n := copy(dst, p.spill)
		p.spill = p.spill[n:]
		return n, nil
	}
	if p.closed {
		return 0, io.EOF
	}
	if err := p.stream.Read(); err != nil {
		return 0, err
	}
	raw := make([]byte, len(p.buf)*2)
	for i, v := range p.buf {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	n := copy(dst, raw)
	if n < len(raw) {
		p.spill = append(p.spill[:0], raw[n:]...)
	}
	return n, nil
}

func (p *testPAInput) Write(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (p *testPAInput) Close() error {
	p.closed = true
	return nil
}

// NewPortaudioInputForTest instantiates an AudioStream test mock in noaudio builds.
func NewPortaudioInputForTest(stream portAudioStreamReader, buf []int16) AudioStream {
	return &testPAInput{stream: stream, buf: buf}
}


