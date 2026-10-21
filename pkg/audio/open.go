//go:build !noaudio

package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/gordonklaus/portaudio"
)

const blocksPerBuffer = 256

var paInitCount atomic.Int32

func initPortAudio() error {
	return withSuppressedStderrErr(func() error {
		if paInitCount.Add(1) == 1 {
			if err := portaudio.Initialize(); err != nil {
				paInitCount.Add(-1)
				return err
			}
		}
		return nil
	})
}

func termPortAudio() {
	if paInitCount.Add(-1) == 0 {
		suppressStderr(func() {
			_ = portaudio.Terminate()
		})
	}
}

// portaudioInput wraps a blocking PortAudio input stream and exposes it as a
// byte-oriented AudioStream (little-endian int16 PCM).
type portaudioInput struct {
	mu     sync.Mutex
	stream *portaudio.Stream
	buf    []int16
	closed bool
}

func resolveInputDevice(deviceID int) (*portaudio.DeviceInfo, error) {
	return withSuppressedStderr(func() (*portaudio.DeviceInfo, error) {
		if deviceID < 0 {
			return portaudio.DefaultInputDevice()
		}
		devs, err := portaudio.Devices()
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if d.Index == deviceID {
				if d.MaxInputChannels < 1 {
					return nil, fmt.Errorf("device %q has no input channels", d.Name)
				}
				return d, nil
			}
		}
		return nil, fmt.Errorf("input device %d not found", deviceID)
	})
}

// OpenInput returns an AudioStream for the given device ID. With useEmulator it
// returns a synthetic sine-wave source; otherwise it opens a real PortAudio
// device.
func OpenInput(deviceID int, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	if useEmulator {
		return newEmulatorInput(deviceID, sampleRate, channels), nil
	}
	if sampleRate <= 0 {
		return nil, fmt.Errorf("invalid sample rate %d", sampleRate)
	}
	if channels < 1 {
		return nil, fmt.Errorf("invalid channel count %d", channels)
	}
	if err := initPortAudio(); err != nil {
		return nil, err
	}
	dev, err := resolveInputDevice(deviceID)
	if err != nil {
		termPortAudio()
		return nil, err
	}
	if channels > dev.MaxInputChannels {
		channels = dev.MaxInputChannels
	}
	buf := make([]int16, blocksPerBuffer*channels)
	st, err := withSuppressedStderr(func() (*portaudio.Stream, error) {
		s, oErr := portaudio.OpenStream(portaudio.StreamParameters{
			Input: portaudio.StreamDeviceParameters{
				Device:   dev,
				Channels: channels,
				Latency:  dev.DefaultLowInputLatency,
			},
			SampleRate:      float64(sampleRate),
			FramesPerBuffer: blocksPerBuffer,
		}, &buf)
		if oErr != nil {
			return nil, oErr
		}
		if sErr := s.Start(); sErr != nil {
			s.Close()
			return nil, sErr
		}
		return s, nil
	})
	if err != nil {
		termPortAudio()
		return nil, err
	}
	return &portaudioInput{stream: st, buf: buf}, nil
}

func (p *portaudioInput) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.EOF
	}
	if err := p.stream.Read(); err != nil && !errors.Is(err, portaudio.InputOverflowed) {
		return 0, err
	}
	// dst must be able to hold one buffer; otherwise the excess frames are
	// dropped (they have already been consumed from the audio device).
	need := len(p.buf) * 2
	if len(dst) >= need {
		for i, s := range p.buf {
			binary.LittleEndian.PutUint16(dst[i*2:], uint16(s))
		}
		return need, nil
	}
	n := 0
	for i := 0; i < len(p.buf) && n+2 <= len(dst); i++ {
		binary.LittleEndian.PutUint16(dst[n:], uint16(p.buf[i]))
		n += 2
	}
	return n, nil
}

func (p *portaudioInput) Write(b []byte) (int, error) { return 0, nil }

func (p *portaudioInput) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	_ = p.stream.Stop()
	var err error
	suppressStderr(func() {
		err = p.stream.Close()
	})
	termPortAudio()
	return err
}

// portaudioOutput wraps a blocking PortAudio output stream exposed as a
// byte-oriented AudioStream. Complete buffer-sized writes are played back in
// real time; smaller writes are parked until a full buffer accumulates.
type portaudioOutput struct {
	mu         sync.Mutex
	stream     *portaudio.Stream
	buf        []int16
	frameSize  int
	blockBytes int
	spill      []byte
	closed     bool
}

func resolveOutputDevice(deviceID int) (*portaudio.DeviceInfo, error) {
	return withSuppressedStderr(func() (*portaudio.DeviceInfo, error) {
		if deviceID < 0 {
			return portaudio.DefaultOutputDevice()
		}
		devs, err := portaudio.Devices()
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if d.Index == deviceID {
				if d.MaxOutputChannels < 1 {
					return nil, fmt.Errorf("device %q has no output channels", d.Name)
				}
				return d, nil
			}
		}
		return nil, fmt.Errorf("output device %d not found", deviceID)
	})
}

// OpenOutput returns an AudioStream for the given device ID. With useEmulator
// it returns an in-memory buffer; otherwise it opens a real PortAudio device.
func OpenOutput(deviceID int, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	if useEmulator {
		return newEmulatorOutput(deviceID), nil
	}
	if sampleRate <= 0 || channels < 1 {
		return nil, fmt.Errorf("invalid sample rate %d / channels %d", sampleRate, channels)
	}
	if err := initPortAudio(); err != nil {
		return nil, err
	}
	dev, err := resolveOutputDevice(deviceID)
	if err != nil {
		termPortAudio()
		return nil, err
	}
	if channels > dev.MaxOutputChannels {
		channels = dev.MaxOutputChannels
	}
	buf := make([]int16, blocksPerBuffer*channels)
	st, err := withSuppressedStderr(func() (*portaudio.Stream, error) {
		s, oErr := portaudio.OpenStream(portaudio.StreamParameters{
			Output: portaudio.StreamDeviceParameters{
				Device:   dev,
				Channels: channels,
				Latency:  dev.DefaultLowOutputLatency,
			},
			SampleRate:      float64(sampleRate),
			FramesPerBuffer: blocksPerBuffer,
		}, &buf)
		if oErr != nil {
			return nil, oErr
		}
		if sErr := s.Start(); sErr != nil {
			s.Close()
			return nil, sErr
		}
		return s, nil
	})
	if err != nil {
		termPortAudio()
		return nil, err
	}
	return &portaudioOutput{
		stream:     st,
		buf:        buf,
		frameSize:  channels * 2,
		blockBytes: len(buf) * 2,
	}, nil
}

func (o *portaudioOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, net.ErrClosed
	}
	data := append(o.spill, p...)
	end := len(data) - len(data)%o.frameSize
	if end < o.blockBytes {
		o.spill = data[:end:end]
		return len(p), nil
	}
	off := 0
	for off+o.blockBytes <= end {
		base := off
		for i := range o.buf {
			o.buf[i] = int16(binary.LittleEndian.Uint16(data[base+i*2:]))
		}
		if err := o.stream.Write(); err != nil && !errors.Is(err, portaudio.OutputUnderflowed) {
			o.spill = append(o.spill[:0], data[off:end]...)
			return off, err
		}
		off += o.blockBytes
	}
	o.spill = append(o.spill[:0], data[off:end]...)
	return len(p), nil
}

func (o *portaudioOutput) Read(b []byte) (int, error) { return 0, nil }

func (o *portaudioOutput) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	_ = o.stream.Abort()
	var err error
	suppressStderr(func() {
		err = o.stream.Close()
	})
	termPortAudio()
	return err
}
