package audio

import (
	"encoding/binary"
	"io"
	"math"
	"sync"
	"time"
)

// AudioStream abstracts a PCM audio source or sink.
type AudioStream interface {
	// Read fills p with little-endian int16 PCM frames. It returns io.EOF once
	// the stream is closed and drained.
	Read(p []byte) (int, error)
	// Write accepts little-endian int16 PCM frames.
	Write(p []byte) (int, error)
	// Close releases the stream.
	Close() error
}

// DeviceCounters holds hardware buffer failure counters for a device stream.
type DeviceCounters struct {
	Overflows  uint64 `json:"overflows"`  // input buffer overflows (capture dropped audio)
	Underflows uint64 `json:"underflows"` // output buffer underflows (playback starved)
}

// CounterStream is implemented by audio streams that can report hardware
// buffer counters (for example real PortAudio device streams).
type CounterStream interface {
	AudioStream
	Counters() DeviceCounters
}

// CountersOf returns the hardware counters of streams that report them, or
// zero counters for streams that do not track any.
func CountersOf(s AudioStream) DeviceCounters {
	if s == nil {
		return DeviceCounters{}
	}
	if c, ok := s.(CounterStream); ok {
		return c.Counters()
	}
	return DeviceCounters{}
}

// ringBuf is a bounded byte ring buffer. It is NOT safe for concurrent use;
// callers must hold their own lock.
type ringBuf struct {
	data      []byte
	start     int // index of the oldest valid byte
	n         int // number of valid bytes
	frameSize int // alignment granularity (channels * 2)
}

func newRingBuf(capacity, frameSize int) *ringBuf {
	if frameSize < 1 {
		frameSize = 1
	}
	capBytes := capacity - capacity%frameSize
	if capBytes < frameSize {
		capBytes = frameSize
	}
	return &ringBuf{data: make([]byte, capBytes), frameSize: frameSize}
}

// write appends p, dropping the oldest data (in whole frames) when full.
func (r *ringBuf) write(p []byte) {
	if len(p) == 0 {
		return
	}
	// A single write larger than the ring can never fit in full: keep the
	// newest cap bytes.
	if len(p) >= r.cap() {
		p = p[len(p)-r.cap():]
	}
	for len(p) > r.cap()-r.n {
		r.dropFrame()
	}
	pos := (r.start + r.n) % r.cap()
	first := r.cap() - pos
	if len(p) <= first {
		copy(r.data[pos:pos+len(p)], p)
	} else {
		copy(r.data[pos:], p[:first])
		copy(r.data[:len(p)-first], p[first:])
	}
	r.n += len(p)
}

// read copies up to len(dst) bytes into dst, aligned to frame boundaries, and
// advances the read position. It returns the number of bytes copied.
func (r *ringBuf) read(dst []byte) int {
	if len(dst) > r.frameSize {
		dst = dst[:len(dst)-len(dst)%r.frameSize]
	}
	n := len(dst)
	if n > r.n {
		n = r.n
	}
	if n == 0 {
		return 0
	}
	first := r.cap() - r.start
	if n <= first {
		copy(dst, r.data[r.start:r.start+n])
	} else {
		copy(dst, r.data[r.start:])
		copy(dst[first:], r.data[:n-first])
	}
	r.start += n
	if r.start >= r.cap() {
		r.start -= r.cap()
	}
	r.n -= n
	if r.n == 0 {
		r.start = 0
	}
	return n
}

// dropFrame removes a single frame (rounded to the next full frame) from the
// oldest side of the buffer.
func (r *ringBuf) dropFrame() {
	n := r.frameSize
	if n < 1 {
		n = 1
	}
	if n > r.n {
		n = r.n
	}
	r.start = (r.start + n) % r.cap()
	r.n -= n
	if r.n == 0 {
		r.start = 0
	}
}

func (r *ringBuf) cap() int { return len(r.data) }

// emulatorInput implements AudioStream by generating a sine wave into a bounded
// ring buffer. It runs a background goroutine (produce) that simulates
// real-time capture.
type emulatorInput struct {
	deviceID   int
	sampleRate int
	channels   int
	frameSize  int
	buf        *ringBuf
	mu         sync.Mutex
	cond       *sync.Cond
	closed     bool
}

func newEmulatorInput(deviceID int, sampleRate, channels int) *emulatorInput {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	if channels < 1 {
		channels = 1
	}
	e := &emulatorInput{
		deviceID:   deviceID,
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  channels * 2,
	}
	e.buf = newRingBuf(128*1024, e.frameSize)
	e.cond = sync.NewCond(&e.mu)
	go e.produce()
	return e
}

// emuMicLevel is the peak amplitude of the emulated microphone's 440 Hz
// sine. It sits well above the DTX silence threshold so emulated routes
// stay "active", but far below full scale so that calibration probes
// (injected near full scale) remain detectable above the background and
// the sum of the two never clips.
const emuMicLevel = 4000

// produce generates a 440 Hz sine wave as int16 little-endian PCM.
func (e *emulatorInput) produce() {
	const chunkFrames = 256
	period := float64(e.sampleRate) / 440.0
	chunk := make([]byte, 0, chunkFrames*e.frameSize)
	block := make([]byte, chunkFrames*e.frameSize)
	t := 0.0
	sleep := time.Duration(float64(chunkFrames)/float64(e.sampleRate)*1000) * time.Millisecond
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return
		}
		for i := 0; i < chunkFrames; i++ {
			sample := uint16(int16(emuMicLevel * math.Sin(2*math.Pi*t/period)))
			for c := 0; c < e.channels; c++ {
				binary.LittleEndian.PutUint16(block[(i*e.channels+c)*2:], sample)
			}
			t += 1.0
			if t >= period {
				t -= period
			}
		}
		chunk = append(chunk[:0], block...)
		e.buf.write(chunk)
		e.cond.Broadcast()
		e.mu.Unlock()
		time.Sleep(sleep)
	}
}

func (e *emulatorInput) Read(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.buf.n == 0 && !e.closed {
		e.cond.Wait()
	}
	if e.buf.n == 0 {
		return 0, io.EOF
	}
	return e.buf.read(p), nil
}

func (e *emulatorInput) Write(p []byte) (int, error) {
	// Input streams do not support Write.
	return 0, nil
}

func (e *emulatorInput) Close() error {
	e.mu.Lock()
	e.closed = true
	e.cond.Broadcast()
	e.mu.Unlock()
	return nil
}

// emulatorOutput implements AudioStream by buffering written PCM data.
type emulatorOutput struct {
	deviceID   int
	buf        []byte
	mu         sync.Mutex
	closed     bool
	closedCh   chan struct{}
	closedOnce sync.Once
	drainedCh  chan struct{} // closed when buffer is fully drained after close
}

func newEmulatorOutput(deviceID int) *emulatorOutput {
	return &emulatorOutput{deviceID: deviceID, closedCh: make(chan struct{}), drainedCh: make(chan struct{})}
}

func (e *emulatorOutput) Read(p []byte) (int, error) {
	// Output streams do not support Read.
	return 0, nil
}

func (e *emulatorOutput) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0, io.ErrClosedPipe
	}
	e.buf = append(e.buf, p...)
	return len(p), nil
}

// Drain is called by the consumer (Streamer) to consume buffered data.
// Returns the data and drains the internal buffer.
func (e *emulatorOutput) Drain() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) == 0 {
		return nil
	}
	out := make([]byte, len(e.buf))
	copy(out, e.buf)
	e.buf = e.buf[:0]
	// If closed and buffer is now empty, signal drained
	if e.closed {
		e.closedOnce.Do(func() { close(e.drainedCh) })
	}
	return out
}

func (e *emulatorOutput) Buffered() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.buf)
}

func (e *emulatorOutput) ReadAll() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]byte, len(e.buf))
	copy(out, e.buf)
	return out
}

func (e *emulatorOutput) Drained() bool {
	select {
	case <-e.drainedCh:
		return true
	default:
		return false
	}
}

func (e *emulatorOutput) Close() error {
	e.mu.Lock()
	e.closed = true
	e.closedOnce.Do(func() { close(e.closedCh) })
	// If buffer is already empty, signal drained immediately
	if len(e.buf) == 0 {
		e.closedOnce.Do(func() { close(e.drainedCh) })
	}
	e.mu.Unlock()
	return nil
}
