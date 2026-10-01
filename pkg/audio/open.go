//go:build !noaudio

package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

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

// portAudioStreamReader abstracts the blocking read and lifecycle from a PortAudio stream.
type portAudioStreamReader interface {
	Read() error
	Stop() error
	Close() error
}

// portaudioInput wraps a blocking PortAudio input stream and exposes it as a
// byte-oriented AudioStream (little-endian int16 PCM).
type portaudioInput struct {
	mu           sync.Mutex
	stream       portAudioStreamReader
	buf          []int16
	rawBuf       []byte
	spill        []byte
	closed       bool
	lastOverflow atomic.Int64
	isRealPA     bool
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
	latency := dev.DefaultLowInputLatency
	if dev.DefaultHighInputLatency > latency {
		latency = dev.DefaultHighInputLatency
	}
	if latency < 40*time.Millisecond {
		latency = 40 * time.Millisecond
	}
	log.Printf("[AUDIO:IO] Opening PortAudio input: device=%q channels=%d sampleRate=%d latency=%v", dev.Name, channels, sampleRate, latency)
	buf := make([]int16, blocksPerBuffer*channels)
	st, err := withSuppressedStderr(func() (*portaudio.Stream, error) {
		s, oErr := portaudio.OpenStream(portaudio.StreamParameters{
			Input: portaudio.StreamDeviceParameters{
				Device:   dev,
				Channels: channels,
				Latency:  latency,
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
		log.Printf("[AUDIO:IO] ERROR opening PortAudio input device %q: %v", dev.Name, err)
		termPortAudio()
		return nil, err
	}
	log.Printf("[AUDIO:IO] PortAudio input online: device=%q channels=%d sampleRate=%d", dev.Name, channels, sampleRate)
	return &portaudioInput{
		stream:   st,
		buf:      buf,
		rawBuf:   make([]byte, len(buf)*2),
		spill:    make([]byte, 0, len(buf)*2),
		isRealPA: true,
	}, nil
}

// NewPortaudioInputForTest instantiates a portaudioInput with a custom stream reader for unit testing.
func NewPortaudioInputForTest(stream portAudioStreamReader, buf []int16) AudioStream {
	return &portaudioInput{
		stream:   stream,
		buf:      buf,
		rawBuf:   make([]byte, len(buf)*2),
		spill:    make([]byte, 0, len(buf)*2),
		isRealPA: false,
	}
}

func (p *portaudioInput) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. Drain any remaining spillover bytes from a previous read first
	if len(p.spill) > 0 {
		n := copy(dst, p.spill)
		p.spill = p.spill[n:]
		return n, nil
	}

	if p.closed {
		return 0, io.EOF
	}

	// 2. Read next buffer from hardware / stream reader
	if err := p.stream.Read(); err != nil {
		if errors.Is(err, portaudio.InputOverflowed) {
			now := time.Now().UnixNano()
			last := p.lastOverflow.Load()
			if now-last > int64(time.Second) && p.lastOverflow.CompareAndSwap(last, now) {
				log.Printf("[AUDIO:IO] Input buffer overflowed (XRun)")
			}
		} else {
			log.Printf("[AUDIO:IO] Input stream read error: %v", err)
			return 0, err
		}
	}

	// 3. Serialize int16 buffer to little-endian bytes without extra allocations
	need := len(p.buf) * 2
	if len(p.rawBuf) < need {
		p.rawBuf = make([]byte, need)
	}
	for i, s := range p.buf {
		binary.LittleEndian.PutUint16(p.rawBuf[i*2:], uint16(s))
	}

	// 4. Fill dst with as many bytes as possible and preserve the remainder in spill
	n := copy(dst, p.rawBuf[:need])
	if n < need {
		p.spill = append(p.spill[:0], p.rawBuf[n:need]...)
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
	var err error
	if p.stream != nil {
		_ = p.stream.Stop()
		suppressStderr(func() {
			err = p.stream.Close()
		})
	}
	if p.isRealPA {
		termPortAudio()
	}
	return err
}

// portaudioOutput wraps a blocking PortAudio output stream exposed as a
// byte-oriented AudioStream. Complete buffer-sized writes are played back in
// real time; smaller writes are parked until a full buffer accumulates.
type portaudioOutput struct {
	mu            sync.Mutex
	stream        *portaudio.Stream
	buf           []int16
	frameSize     int
	blockBytes    int
	spill         []byte
	closed        bool
	lastUnderflow atomic.Int64
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
	latency := dev.DefaultLowOutputLatency
	if dev.DefaultHighOutputLatency > latency {
		latency = dev.DefaultHighOutputLatency
	}
	if latency < 40*time.Millisecond {
		latency = 40 * time.Millisecond
	}
	log.Printf("[AUDIO:IO] Opening PortAudio output: device=%q channels=%d sampleRate=%d latency=%v", dev.Name, channels, sampleRate, latency)
	buf := make([]int16, blocksPerBuffer*channels)
	st, err := withSuppressedStderr(func() (*portaudio.Stream, error) {
		s, oErr := portaudio.OpenStream(portaudio.StreamParameters{
			Output: portaudio.StreamDeviceParameters{
				Device:   dev,
				Channels: channels,
				Latency:  latency,
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
		log.Printf("[AUDIO:IO] ERROR opening PortAudio output device %q: %v", dev.Name, err)
		termPortAudio()
		return nil, err
	}
	log.Printf("[AUDIO:IO] PortAudio output online: device=%q channels=%d sampleRate=%d", dev.Name, channels, sampleRate)
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
		if err := o.stream.Write(); err != nil {
			if errors.Is(err, portaudio.OutputUnderflowed) {
				now := time.Now().UnixNano()
				last := o.lastUnderflow.Load()
				if now-last > int64(time.Second) && o.lastUnderflow.CompareAndSwap(last, now) {
					log.Printf("[AUDIO:IO] Output buffer underflowed (XRun)")
				}
			} else {
				log.Printf("[AUDIO:IO] Output stream write error: %v", err)
				o.spill = append(o.spill[:0], data[off:end]...)
				return off, err
			}
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
