package audio

import (
	"io"
	"sync"
	"time"
)

// VirtualAudioDevice implements AudioStream and BufferedStream for testing.
// It allows injecting PCM data for consumption via Read, capturing PCM data
// emitted via Write, and optional real-time pacing simulation.
type VirtualAudioDevice struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inBuf    []byte
	captured []byte
	closed   bool

	pacing     bool
	sampleRate int
	channels   int
}

var (
	_ AudioStream    = (*VirtualAudioDevice)(nil)
	_ BufferedStream = (*VirtualAudioDevice)(nil)
)

// NewVirtualAudioDevice creates an initialized VirtualAudioDevice.
func NewVirtualAudioDevice() *VirtualAudioDevice {
	v := &VirtualAudioDevice{}
	v.cond = sync.NewCond(&v.mu)
	return v
}

// InjectPush appends PCM bytes to the input buffer for the reader to consume.
// It is thread-safe and unblocks any waiting Read calls.
func (v *VirtualAudioDevice) InjectPush(data []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return 0, io.ErrClosedPipe
	}

	v.inBuf = append(v.inBuf, data...)
	v.cond.Broadcast()
	return len(data), nil
}

// Read copies PCM frames into p. If the input buffer is empty, it blocks
// until data is injected via InjectPush or the device is closed.
// When closed and drained, it returns io.EOF.
func (v *VirtualAudioDevice) Read(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for len(v.inBuf) == 0 && !v.closed {
		v.cond.Wait()
	}

	if len(v.inBuf) == 0 && v.closed {
		return 0, io.EOF
	}

	n := copy(p, v.inBuf)
	v.inBuf = v.inBuf[n:]

	pacing := v.pacing
	sr := v.sampleRate
	ch := v.channels

	if pacing && sr > 0 && ch > 0 && n > 0 {
		bytesPerSec := sr * ch * 2
		duration := time.Duration(float64(n) / float64(bytesPerSec) * float64(time.Second))
		v.mu.Unlock()
		time.Sleep(duration)
		v.mu.Lock()
	}

	return n, nil
}

// Write captures PCM frames written to the device.
func (v *VirtualAudioDevice) Write(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return 0, io.ErrClosedPipe
	}

	v.captured = append(v.captured, p...)
	return len(p), nil
}

// CapturedBytes returns a copy of all PCM bytes written to the device.
func (v *VirtualAudioDevice) CapturedBytes() []byte {
	v.mu.Lock()
	defer v.mu.Unlock()

	out := make([]byte, len(v.captured))
	copy(out, v.captured)
	return out
}

// Reset clears both the input and captured output buffers.
func (v *VirtualAudioDevice) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.inBuf = nil
	v.captured = nil
}

// SetPacing configures real-time simulation pacing on Read operations.
func (v *VirtualAudioDevice) SetPacing(enabled bool, sampleRate int, channels int) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.pacing = enabled
	v.sampleRate = sampleRate
	v.channels = channels
}

// Buffered returns the number of bytes currently buffered in the device.
// If captured output is present, it returns captured bytes; otherwise input bytes.
func (v *VirtualAudioDevice) Buffered() int {
	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.captured) > 0 {
		return len(v.captured)
	}
	return len(v.inBuf)
}

// Close closes the virtual audio device, releasing any blocked readers.
func (v *VirtualAudioDevice) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return nil
	}
	v.closed = true
	v.cond.Broadcast()
	return nil
}
