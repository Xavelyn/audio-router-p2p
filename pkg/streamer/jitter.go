package streamer

import (
	"errors"
	"math"
	"sync"
	"time"
)

// JitterBufferInterface defines the interface for jitter buffers.
type JitterBufferInterface interface {
	Push(seq uint32, timestamp int64, data []byte) (int, error)
	PopNonBlock() ([]byte, uint32, error)
	Stats() JitterBufferStats
	Close() error
}

var (
	ErrBufferEmpty = errors.New("jitter buffer empty")
	ErrBufferFull  = errors.New("jitter buffer full")
	ErrClosed      = errors.New("jitter buffer closed")
)

// JitterBuffer implements an adaptive jitter buffer for audio playout.
// It smooths out network jitter by buffering packets and playing them out
// at a steady rate. The target delay adapts based on observed network conditions.
type JitterBuffer struct {
	mu           sync.Mutex
	frames       []*jbFrame
	capacity     int
	frameSize    int
	sampleRate   int
	channels     int

	// Playout state
	playoutTime   time.Time
	originTime    time.Time
	originSeq     uint32
	seqInitialized bool

	// Adaptive delay
	targetDelay    time.Duration
	minDelay       time.Duration
	maxDelay       time.Duration
	ewmaDelay      time.Duration
	ewmaDeviation  time.Duration
	alpha          float64 // EWMA smoothing factor

	// Stats
	underruns      uint64
	overruns       uint64
	latePackets    uint64
	earlyPackets   uint64

	closed bool
}

type jbFrame struct {
	seq       uint32
	timestamp time.Time // sender timestamp
	data      []byte
	received  time.Time
}

func NewJitterBuffer(frameSize, sampleRate, channels int, capacityFrames int) *JitterBuffer {
	if capacityFrames <= 0 {
		capacityFrames = 200 // ~4 seconds at 20ms packets
	}
	minDelay := time.Duration(20) * time.Millisecond
	maxDelay := time.Duration(200) * time.Millisecond
	return &JitterBuffer{
		frames:      make([]*jbFrame, capacityFrames),
		capacity:    capacityFrames,
		frameSize:   frameSize,
		sampleRate:  sampleRate,
		channels:    channels,
		minDelay:    minDelay,
		maxDelay:    maxDelay,
		targetDelay: minDelay,
		ewmaDelay:   minDelay,
		alpha:       0.125,
	}
}

// Push adds a frame to the jitter buffer.
// seq: packet sequence number
// timestamp: sender's timestamp (nanoseconds since epoch)
// data: audio frame data
// Returns the number of frames dropped due to overflow.
func (jb *JitterBuffer) Push(seq uint32, timestamp int64, data []byte) (int, error) {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if jb.closed {
		return 0, ErrClosed
	}
	if len(data) != jb.frameSize {
		return 0, errors.New("invalid frame size")
	}

	now := time.Now()
	ts := time.Unix(0, timestamp)

	// Initialize origin on first packet
	if !jb.seqInitialized {
		jb.originSeq = seq
		jb.originTime = ts
		jb.playoutTime = now.Add(jb.targetDelay)
		jb.seqInitialized = true
	}

	// Calculate expected playout time for this sequence
	expectedPlayout := jb.playoutTime.Add(time.Duration(seq-jb.originSeq) * jb.frameDuration())

	// Check if packet is too late (already past playout time)
	late := now.After(expectedPlayout.Add(jb.targetDelay))
	if late {
		jb.latePackets++
	}

	// Find slot
	idx := int((seq - jb.originSeq) % uint32(jb.capacity))
	if idx < 0 {
		idx += jb.capacity
	}

	dropped := 0
	if jb.frames[idx] != nil {
		// Overwrite existing frame (overrun)
		dropped = 1
		jb.overruns++
	}
	jb.frames[idx] = &jbFrame{
		seq:      seq,
		timestamp: ts,
		data:     append([]byte(nil), data...),
		received: now,
	}

	// Adaptive delay adjustment based on arrival time vs expected
	if jb.seqInitialized && !late {
		jb.adaptDelay(now, expectedPlayout)
	}

	return dropped, nil
}

// Pop retrieves the next frame for playout.
// Returns the frame data and the sequence number.
// Blocks until a frame is available or the buffer is closed.
func (jb *JitterBuffer) Pop() ([]byte, uint32, error) {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if !jb.seqInitialized {
		return nil, 0, ErrBufferEmpty
	}

	for {
		idx := int((jb.originSeq - jb.originSeq) % uint32(jb.capacity))
		if idx < 0 {
			idx += jb.capacity
		}

		frame := jb.frames[idx]
		if frame != nil {
			jb.frames[idx] = nil
			jb.originSeq++
			return frame.data, frame.seq, nil
		}

		// Check if it's time to play out silence (underrun)
		expectedPlayout := jb.playoutTime.Add(time.Duration(jb.originSeq-jb.originSeq) * jb.frameDuration())
		if time.Now().After(expectedPlayout) {
			jb.underruns++
			// Generate silence frame
			silence := make([]byte, jb.frameSize)
			jb.originSeq++
			return silence, jb.originSeq - 1, nil
		}

		// Wait a bit
		waitTime := expectedPlayout.Sub(time.Now())
		if waitTime < 0 {
			waitTime = 1 * time.Millisecond
		}
		jb.mu.Unlock()
		time.Sleep(waitTime)
		jb.mu.Lock()
	}
}

// PopNonBlock tries to pop a frame without blocking.
// Returns ErrBufferEmpty if no frame is ready for playout.
func (jb *JitterBuffer) PopNonBlock() ([]byte, uint32, error) {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if !jb.seqInitialized {
		return nil, 0, ErrBufferEmpty
	}

	idx := int((jb.originSeq - jb.originSeq) % uint32(jb.capacity))
	if idx < 0 {
		idx += jb.capacity
	}

	frame := jb.frames[idx]
	if frame == nil {
		expectedPlayout := jb.playoutTime.Add(time.Duration(jb.originSeq-jb.originSeq) * jb.frameDuration())
		if time.Now().After(expectedPlayout) {
			jb.underruns++
			silence := make([]byte, jb.frameSize)
			jb.originSeq++
			return silence, jb.originSeq - 1, nil
		}
		return nil, 0, ErrBufferEmpty
	}

	jb.frames[idx] = nil
	jb.originSeq++
	return frame.data, frame.seq, nil
}

func (jb *JitterBuffer) frameDuration() time.Duration {
	// frames per second = sampleRate / (frameSize / channels / 2)
	// but we know frameSize = channels * 2 (int16 per channel)
	// so frames per buffer = frameSize / (channels * 2)
	// Actually for 20ms packets: frames = sampleRate * 0.02
	framesPerPacket := jb.sampleRate / 50 // 20ms = 1/50 sec
	if framesPerPacket <= 0 {
		framesPerPacket = 1
	}
	return time.Duration(float64(time.Second) / float64(framesPerPacket))
}

func (jb *JitterBuffer) adaptDelay(arrival, expected time.Time) {
	// Calculate network delay variation
	delay := arrival.Sub(expected)
	if delay < 0 {
		delay = 0
	}

	// EWMA of delay and deviation
	if jb.ewmaDelay == 0 {
		jb.ewmaDelay = delay
		jb.ewmaDeviation = delay / 2
	} else {
		jb.ewmaDelay = time.Duration(float64(jb.ewmaDelay)*(1-jb.alpha) + float64(delay)*jb.alpha)
		dev := time.Duration(math.Abs(float64(delay - jb.ewmaDelay)))
		jb.ewmaDeviation = time.Duration(float64(jb.ewmaDeviation)*(1-jb.alpha) + float64(dev)*jb.alpha)
	}

	// Target delay = ewmaDelay + 4*deviation (covers ~95% of variation)
	newTarget := jb.ewmaDelay + 4*jb.ewmaDeviation
	if newTarget < jb.minDelay {
		newTarget = jb.minDelay
	}
	if newTarget > jb.maxDelay {
		newTarget = jb.maxDelay
	}
	jb.targetDelay = newTarget
}

// Stats returns current buffer statistics.
func (jb *JitterBuffer) Stats() JitterBufferStats {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	buffered := 0
	for _, f := range jb.frames {
		if f != nil {
			buffered++
		}
	}
	return JitterBufferStats{
		BufferedFrames:  buffered,
		CapacityFrames:  jb.capacity,
		TargetDelay:     jb.targetDelay,
		MinDelay:        jb.minDelay,
		MaxDelay:        jb.maxDelay,
		Underruns:       jb.underruns,
		Overruns:        jb.overruns,
		LatePackets:     jb.latePackets,
		EarlyPackets:    jb.earlyPackets,
	}
}

type JitterBufferStats struct {
	BufferedFrames  int
	CapacityFrames  int
	TargetDelay     time.Duration
	MinDelay        time.Duration
	MaxDelay        time.Duration
	Underruns       uint64
	Overruns        uint64
	LatePackets     uint64
	EarlyPackets    uint64
}

func (jb *JitterBuffer) Close() error {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	jb.closed = true
	return nil
}

// Resampler provides linear interpolation resampling for sample rate conversion.
// Not for production quality; use soxr or similar for high-quality resampling.
type Resampler struct {
	inRate    int
	outRate   int
	channels  int
	pos       float64
	step      float64
	lastSample []float64
}

func NewResampler(inRate, outRate, channels int) *Resampler {
	if inRate == outRate {
		return nil
	}
	return &Resampler{
		inRate:     inRate,
		outRate:    outRate,
		channels:   channels,
		step:       float64(inRate) / float64(outRate),
		lastSample: make([]float64, channels),
	}
}

// Resample converts int16 PCM from inRate to outRate.
// inData must be a multiple of frameSize (channels * 2).
// Returns resampled data and number of input frames consumed.
func (r *Resampler) Resample(inData []byte) ([]byte, int) {
	if r == nil {
		return inData, len(inData) / (r.channels * 2)
	}

	frameSize := r.channels * 2
	inFrames := len(inData) / frameSize
	if inFrames == 0 {
		return nil, 0
	}

	// Estimate output frames
	outFrames := int(float64(inFrames) / r.step) + 2
	outData := make([]byte, outFrames*frameSize)

	// Convert input to float
	inFloat := make([]float64, inFrames*r.channels)
	for i := 0; i < inFrames*r.channels; i++ {
		inFloat[i] = float64(int16(inData[i*2]) | int16(inData[i*2+1])<<8) / 32768.0
	}

	outIdx := 0
	for ch := 0; ch < r.channels; ch++ {
		r.pos = 0
		for outIdx < outFrames {
			inIdx := int(r.pos)
			if inIdx >= inFrames-1 {
				break
			}
			frac := r.pos - float64(inIdx)
			sample := inFloat[inIdx*r.channels+ch]*(1-frac) + inFloat[(inIdx+1)*r.channels+ch]*frac
			r.lastSample[ch] = sample
			val := int16(sample * 32767)
			outData[outIdx*frameSize+ch*2] = byte(val)
			outData[outIdx*frameSize+ch*2+1] = byte(val >> 8)
			r.pos += r.step
			outIdx++
		}
	}

	consumed := int(r.pos / r.step)
	if consumed > inFrames {
		consumed = inFrames
	}
	r.pos -= float64(consumed) * r.step

	return outData[:outIdx*frameSize], consumed
}

func (r *Resampler) Reset() {
	if r != nil {
		r.pos = 0
		for i := range r.lastSample {
			r.lastSample[i] = 0
		}
	}
}