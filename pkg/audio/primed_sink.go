package audio

import (
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// blocksPerBuffer is the default hardware quantum (256 frames = ~5.33ms @ 48kHz).
const blocksPerBuffer = 256

// PortAudioStreamWriter abstracts the blocking write and lifecycle of a PortAudio output stream.
type PortAudioStreamWriter interface {
	Start() error
	Write() error
	Stop() error
	Abort() error
	Close() error
}

// SinkState represents the lifecycle phase of the PrimedBufferedSink.
type SinkState int32

const (
	StateUnprimed SinkState = iota
	StatePriming
	StateStreaming
	StateDraining
	StateClosed
)

func (s SinkState) String() string {
	switch s {
	case StateUnprimed:
		return "unprimed"
	case StatePriming:
		return "priming"
	case StateStreaming:
		return "streaming"
	case StateDraining:
		return "draining"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// spscRingBuffer is a pre-allocated bounded ring buffer designed for single-producer single-consumer
// audio pipelines. It drops oldest frames on overrun and synthesizes comfort silence when empty.
type spscRingBuffer struct {
	mu        sync.Mutex
	data      []byte
	start     int // read cursor (oldest byte)
	count     int // number of valid bytes buffered
	capacity  int // total capacity in bytes
	frameSize int // channels * 2
}

func newSPSCRingBuffer(capacity, frameSize int) *spscRingBuffer {
	if frameSize < 1 {
		frameSize = 1
	}
	capBytes := capacity - capacity%frameSize
	if capBytes < frameSize {
		capBytes = frameSize
	}
	return &spscRingBuffer{
		data:      make([]byte, capBytes),
		capacity:  capBytes,
		frameSize: frameSize,
	}
}

// push enqueues p into the ring buffer. If capacity is exceeded, oldest frames
// are discarded to guarantee strictly bounded latency.
func (r *spscRingBuffer) push(p []byte) (int, int) {
	if len(p) == 0 {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	origLen := len(p)
	dropped := 0
	if len(p) >= r.capacity {
		dropped = r.count + (len(p) - r.capacity)
		p = p[len(p)-r.capacity:]
		rem := len(p) % r.frameSize
		if rem != 0 {
			p = p[rem:]
		}
		r.start = 0
		r.count = 0
	}

	overflow := (r.count + len(p)) - r.capacity
	if overflow > 0 {
		dropFrames := (overflow + r.frameSize - 1) / r.frameSize
		dropBytes := dropFrames * r.frameSize
		if dropBytes > r.count {
			dropBytes = r.count
		}
		r.start = (r.start + dropBytes) % r.capacity
		r.count -= dropBytes
		dropped += dropBytes
	}

	pos := (r.start + r.count) % r.capacity
	first := r.capacity - pos
	if len(p) <= first {
		copy(r.data[pos:pos+len(p)], p)
	} else {
		copy(r.data[pos:], p[:first])
		copy(r.data[:len(p)-first], p[first:])
	}
	r.count += len(p)

	return origLen, dropped
}

// popBlock reads up to len(dst) samples (converted to int16 LE) from the ring buffer.
// If count >= neededBytes, it fills dst with valid audio and returns true (hasData).
// If 0 < count < neededBytes, it fills partial audio, zeroes the remainder, and returns true.
// If count == 0, it zeroes dst and returns false (comfort silence injected).
func (r *spscRingBuffer) popBlock(dst []int16) bool {
	neededBytes := len(dst) * 2
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		for i := range dst {
			dst[i] = 0
		}
		return false
	}

	n := neededBytes
	hasPartial := false
	if n > r.count {
		n = r.count
		hasPartial = true
	}
	n -= n % r.frameSize
	if n == 0 {
		for i := range dst {
			dst[i] = 0
		}
		if r.count < r.frameSize {
			r.count = 0
			r.start = 0
		}
		return false
	}

	samples := n / 2
	firstBytes := r.capacity - r.start
	if firstBytes >= n {
		convertBytesToInt16LE(r.data[r.start:r.start+n], dst[:samples])
	} else {
		firstSamples := firstBytes / 2
		convertBytesToInt16LE(r.data[r.start:r.start+firstBytes], dst[:firstSamples])
		remBytes := n - firstBytes
		remSamples := samples - firstSamples
		convertBytesToInt16LE(r.data[:remBytes], dst[firstSamples:firstSamples+remSamples])
	}

	r.start = (r.start + n) % r.capacity
	r.count -= n

	if hasPartial {
		for i := samples; i < len(dst); i++ {
			dst[i] = 0
		}
		if r.count == 0 {
			r.start = 0
		}
	}

	return true
}

func (r *spscRingBuffer) buffered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func convertBytesToInt16LE(src []byte, dst []int16) {
	for i := range dst {
		dst[i] = int16(uint16(src[i*2]) | (uint16(src[i*2+1]) << 8))
	}
}

// PrimedSinkOption configures PrimedBufferedSink settings.
type PrimedSinkOption func(*PrimedBufferedSink)

// WithPreRollWatermark sets the buffer duration before PortAudio DAC activation.
func WithPreRollWatermark(d time.Duration) PrimedSinkOption {
	return func(s *PrimedBufferedSink) {
		if d > 0 {
			s.watermarkDuration = d
		}
	}
}

// WithCapacity sets the maximum buffer capacity duration before oldest frames are dropped.
func WithCapacity(d time.Duration) PrimedSinkOption {
	return func(s *PrimedBufferedSink) {
		if d > 0 {
			s.capacityDuration = d
		}
	}
}

// WithFramesPerBuffer sets the block quantum in frames (default: 256).
func WithFramesPerBuffer(n int) PrimedSinkOption {
	return func(s *PrimedBufferedSink) {
		if n > 0 {
			s.framesPerBuffer = n
		}
	}
}

// WithOnClose sets an optional cleanup callback invoked on Close().
func WithOnClose(fn func()) PrimedSinkOption {
	return func(s *PrimedBufferedSink) {
		s.onClose = fn
	}
}

// PrimedBufferedSink wraps a physical PortAudio output stream with an asynchronous
// primed playout queue. It eliminates ALSA XRuns by deferring DAC start until a pre-roll
// watermark is buffered, injecting comfort silence during packet starvation, and
// maintaining strictly bounded latency via overrun clamping.
type PrimedBufferedSink struct {
	stream            PortAudioStreamWriter
	buf               []int16 // pre-allocated buffer bound to PortAudio stream
	sampleRate        int
	channels          int
	frameSize         int
	framesPerBuffer   int
	watermarkDuration time.Duration
	capacityDuration  time.Duration
	watermarkBytes    int
	capacityBytes     int

	ring *spscRingBuffer

	state atomic.Int32 // SinkState

	startCh     chan struct{}
	primeOnce   sync.Once
	stopCh      chan struct{}
	pumpDoneCh  chan struct{}
	closeOnce   sync.Once
	cleanupOnce sync.Once
	onClose     func()

	// Observability & Telemetry
	comfortSilenceBlocks atomic.Uint64
	overrunDroppedFrames atomic.Uint64
	lastUnderflow        atomic.Int64

	writeMu sync.Mutex
	spill   []byte
}

// NewPrimedBufferedSink creates a PrimedBufferedSink wrapping stream.
func NewPrimedBufferedSink(stream PortAudioStreamWriter, buf []int16, sampleRate, channels int, opts ...PrimedSinkOption) *PrimedBufferedSink {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if channels <= 0 {
		channels = 2
	}
	s := &PrimedBufferedSink{
		stream:            stream,
		sampleRate:        sampleRate,
		channels:          channels,
		frameSize:         channels * 2,
		framesPerBuffer:   blocksPerBuffer,
		watermarkDuration: 60 * time.Millisecond,
		capacityDuration:  200 * time.Millisecond,
		startCh:           make(chan struct{}),
		stopCh:            make(chan struct{}),
		pumpDoneCh:        make(chan struct{}),
	}
	s.spill = make([]byte, 0, s.frameSize)
	s.state.Store(int32(StateUnprimed))

	for _, opt := range opts {
		opt(s)
	}

	if s.framesPerBuffer <= 0 {
		s.framesPerBuffer = blocksPerBuffer
	}

	blockBytes := s.framesPerBuffer * s.frameSize

	watermarkFrames := int((int64(s.sampleRate) * int64(s.watermarkDuration)) / int64(time.Second))
	if watermarkFrames < s.framesPerBuffer {
		watermarkFrames = s.framesPerBuffer
	}
	s.watermarkBytes = watermarkFrames * s.frameSize

	capacityFrames := int((int64(s.sampleRate) * int64(s.capacityDuration)) / int64(time.Second))
	minCap := s.watermarkBytes + blockBytes*2
	s.capacityBytes = capacityFrames * s.frameSize
	if s.capacityBytes < minCap {
		s.capacityBytes = minCap
	}

	s.ring = newSPSCRingBuffer(s.capacityBytes, s.frameSize)

	if len(buf) >= s.framesPerBuffer*s.channels {
		s.buf = buf
	} else {
		s.buf = make([]int16, s.framesPerBuffer*s.channels)
	}

	go s.pumpWorker()

	return s
}

// pumpWorker runs in a dedicated goroutine, transferring blocks to PortAudio.
func (s *PrimedBufferedSink) pumpWorker() {
	defer close(s.pumpDoneCh)

	// Phase 1: Park until pre-roll watermark is reached or sink is closed
	select {
	case <-s.stopCh:
		s.state.Store(int32(StateClosed))
		return
	case <-s.startCh:
	}

	if s.isClosed() {
		return
	}

	// Phase 2: Lazy DAC activation with primed cushion
	if !s.state.CompareAndSwap(int32(StateUnprimed), int32(StatePriming)) {
		return
	}
	if s.stream != nil {
		if err := s.stream.Start(); err != nil {
			log.Printf("[AUDIO:IO] PrimedBufferedSink: failed to start PortAudio stream: %v", err)
			s.state.Store(int32(StateClosed))
			return
		}
	}
	if !s.state.CompareAndSwap(int32(StatePriming), int32(StateStreaming)) {
		if s.stream != nil {
			_ = s.stream.Abort()
		}
		return
	}

	// Phase 3: Steady-state playout loop
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		hasData := s.ring.popBlock(s.buf)
		if !hasData {
			s.comfortSilenceBlocks.Add(1)
		}

		if s.stream != nil {
			if err := s.stream.Write(); err != nil {
				if s.isClosed() {
					return
				}
				if isOutputUnderflowError(err) {
					now := time.Now().UnixNano()
					last := s.lastUnderflow.Load()
					if now-last > int64(time.Second) && s.lastUnderflow.CompareAndSwap(last, now) {
						log.Printf("[AUDIO:IO] Output buffer underflowed (XRun)")
					}
				} else {
					log.Printf("[AUDIO:IO] PrimedBufferedSink: stream write error: %v", err)
					s.state.Store(int32(StateClosed))
					return
				}
			}
		} else {
			blockDur := time.Duration(s.framesPerBuffer) * time.Second / time.Duration(s.sampleRate)
			if blockDur < time.Millisecond {
				blockDur = time.Millisecond
			}
			select {
			case <-s.stopCh:
				return
			case <-time.After(blockDur):
			}
		}
	}
}

// Write enqueues PCM audio frames into the decoupled ring buffer.
func (s *PrimedBufferedSink) Write(p []byte) (int, error) {
	if s.isClosed() {
		return 0, net.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.isClosed() {
		return 0, net.ErrClosed
	}

	var data []byte
	if len(s.spill) > 0 {
		data = append(s.spill, p...)
		s.spill = s.spill[:0]
	} else {
		data = p
	}

	rem := len(data) % s.frameSize
	if rem != 0 {
		validLen := len(data) - rem
		s.spill = append(s.spill[:0], data[validLen:]...)
		data = data[:validLen]
	}

	if len(data) > 0 {
		_, dropped := s.ring.push(data)
		if dropped > 0 {
			s.overrunDroppedFrames.Add(uint64(dropped / s.frameSize))
		}
	}

	if SinkState(s.state.Load()) == StateUnprimed {
		if s.ring.buffered() >= s.watermarkBytes {
			s.primeOnce.Do(func() {
				close(s.startCh)
			})
		}
	}

	return len(p), nil
}

// Read implements AudioStream. Output sinks do not produce capture frames.
func (s *PrimedBufferedSink) Read(p []byte) (int, error) {
	if s.isClosed() {
		return 0, io.EOF
	}
	return 0, nil
}

// Buffered returns the number of valid PCM bytes currently queued in the ring buffer.
func (s *PrimedBufferedSink) Buffered() int {
	s.writeMu.Lock()
	spillLen := len(s.spill)
	s.writeMu.Unlock()
	return s.ring.buffered() + spillLen
}

// State returns the current lifecycle state of the sink.
func (s *PrimedBufferedSink) State() SinkState {
	return SinkState(s.state.Load())
}

// ComfortSilenceBlocks returns the cumulative count of comfort silence blocks injected.
func (s *PrimedBufferedSink) ComfortSilenceBlocks() uint64 {
	return s.comfortSilenceBlocks.Load()
}

// OverrunDroppedFrames returns the cumulative count of audio frames dropped to clamp latency.
func (s *PrimedBufferedSink) OverrunDroppedFrames() uint64 {
	return s.overrunDroppedFrames.Load()
}

func (s *PrimedBufferedSink) isClosed() bool {
	return SinkState(s.state.Load()) == StateClosed
}

// Close cleanly terminates the pump worker, aborts physical playback, closes PortAudio,
// and invokes any registered close hooks.
func (s *PrimedBufferedSink) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		prevState := SinkState(s.state.Swap(int32(StateClosed)))
		close(s.stopCh)

		if s.stream != nil {
			if prevState == StateStreaming || prevState == StatePriming {
				_ = s.stream.Abort()
			}
		}
	})

	<-s.pumpDoneCh

	s.cleanupOnce.Do(func() {
		if s.stream != nil {
			suppressStderr(func() {
				closeErr = s.stream.Close()
			})
		}
		if s.onClose != nil {
			s.onClose()
		}
	})

	return closeErr
}
