package streamer

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio/dsp"
	"github.com/audio-router-p2p/pkg/transport"
)

// JitterBufferInterface defines the interface for jitter buffers.
type JitterBufferInterface interface {
	Push(seq uint32, timestamp int64, data []byte) (int, error)
	PushWithFlags(seq uint32, timestamp int64, data []byte, flags uint8) (int, error)
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
	mu         sync.Mutex
	frames     []jbFrame
	capacity   int
	frameSize  int
	sampleRate int
	channels   int

	// Playout state
	playoutTime    time.Time
	originTime     time.Time
	originSeq      uint32 // sequence number of the anchor (first) packet
	nextSeq        uint32 // sequence number expected at the head of playout
	seqInitialized bool

	// DTX mode
	dtxMode bool

	// PLC and Clock Drift
	plc        *dsp.PSOLAConcealer
	driftCtrl  *ClockDriftController
	concealBuf []byte
	recoverBuf []byte

	// Adaptive delay
	targetDelay   time.Duration
	playoutDelay  time.Duration // delay actually applied to playoutTime
	minDelay      time.Duration
	maxDelay      time.Duration
	ewmaDeviation time.Duration
	alpha         float64 // EWMA smoothing factor
	lastArrival   time.Time
	lastSenderTS  time.Time
	lastJitterSeq uint32
	hasJitterSeq  bool

	// Stats
	underruns    uint64
	overruns     uint64
	latePackets  uint64
	earlyPackets uint64

	// Telemetry atomic accumulators for lock-free interrogation
	atomicUnderruns    atomic.Uint64
	atomicOverruns     atomic.Uint64
	atomicBuffered     atomic.Int32
	atomicPLCConcealed atomic.Uint64
	atomicTargetDelay  atomic.Int64 // nanoseconds

	closed bool
}

type jbFrame struct {
	seq       uint32
	timestamp time.Time // sender timestamp
	data      []byte
	received  time.Time
	valid     bool
}

func NewJitterBuffer(frameSize, sampleRate, channels int, capacityFrames int) *JitterBuffer {
	if capacityFrames <= 0 {
		capacityFrames = 200 // ~4 seconds at 20ms packets
	}
	minDelay := time.Duration(20) * time.Millisecond
	maxDelay := time.Duration(200) * time.Millisecond
	targetWatermark := float64(minDelay) / float64(20*time.Millisecond)
	if targetWatermark < 1.0 {
		targetWatermark = 1.0
	}
	frames := make([]jbFrame, capacityFrames)
	for i := range frames {
		frames[i].data = make([]byte, frameSize)
	}
	return &JitterBuffer{
		frames:       frames,
		capacity:     capacityFrames,
		frameSize:    frameSize,
		sampleRate:   sampleRate,
		channels:     channels,
		minDelay:     minDelay,
		maxDelay:     maxDelay,
		targetDelay:  minDelay,
		playoutDelay: minDelay,
		alpha:        0.125,
		plc:          dsp.NewPSOLAConcealer(sampleRate, channels),
		driftCtrl:    NewClockDriftController(DefaultClockDriftConfig(targetWatermark)),
		concealBuf:   make([]byte, frameSize),
		recoverBuf:   make([]byte, frameSize),
	}
}

// Push adds a frame to the jitter buffer defaulting to FlagActive.
// Returns the number of frames dropped due to overflow.
func (jb *JitterBuffer) Push(seq uint32, timestamp int64, data []byte) (int, error) {
	return jb.PushWithFlags(seq, timestamp, data, transport.FlagActive)
}

// PushWithFlags adds a frame to the jitter buffer with wire control flags.
// When a packet carries FlagSID, it updates the playout sequence anchor and
// marks the buffer as operating in DTX silence suppression mode.
func (jb *JitterBuffer) PushWithFlags(seq uint32, timestamp int64, data []byte, flags uint8) (int, error) {
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

	isSID := (flags & transport.FlagSID) != 0

	// Handle DTX mode transitions and sequence anchoring
	if isSID {
		jb.dtxMode = true
		jb.originSeq = seq
		jb.nextSeq = seq
		jb.originTime = ts
		jb.playoutTime = now.Add(jb.targetDelay)
		jb.playoutDelay = jb.targetDelay
		jb.hasJitterSeq = false
		jb.seqInitialized = true
	} else if (flags & transport.FlagActive) != 0 {
		if jb.dtxMode {
			// Transition out of DTX silence mode back to active speech
			jb.dtxMode = false
			jb.originSeq = seq
			jb.nextSeq = seq
			jb.originTime = ts
			jb.playoutTime = now.Add(jb.targetDelay)
			jb.playoutDelay = jb.targetDelay
			jb.hasJitterSeq = false
			jb.seqInitialized = true
		}
	}

	// Initialize origin on first packet
	if !jb.seqInitialized {
		jb.originSeq = seq
		jb.nextSeq = seq
		jb.originTime = ts
		jb.playoutTime = now.Add(jb.targetDelay)
		jb.playoutDelay = jb.targetDelay
		jb.seqInitialized = true
	}

	// Signed RFC 1982 serial number distance from next expected sequence
	distFromNext := int32(seq - jb.nextSeq)
	if distFromNext < 0 {
		jb.latePackets++
		return 0, nil
	}

	// Calculate expected playout time for this sequence using signed distance from origin
	distFromOrigin := int32(seq - jb.originSeq)
	expectedPlayout := jb.playoutTime.Add(time.Duration(distFromOrigin) * jb.frameDuration())

	// Check if packet is too late (already past playout time)
	late := now.After(expectedPlayout)
	if late {
		jb.latePackets++
	}

	// Find slot: sequence numbers map directly into the ring buffer and wrap
	// naturally via unsigned modulo, which keeps buffered frames at stable
	// slots regardless of how many packets play out in between.
	idx := int(seq % uint32(jb.capacity))
	f := &jb.frames[idx]

	dropped := 0
	if f.valid {
		// Overwrite existing frame (overrun)
		dropped = 1
		jb.overruns++
		jb.atomicOverruns.Add(1)
	} else {
		jb.atomicBuffered.Add(1)
	}
	f.seq = seq
	f.timestamp = ts
	copy(f.data, data)
	f.received = now
	f.valid = true

	// Sender/receiver clocks need not agree. Use differences between packet
	// transit times to estimate jitter, rather than absolute wall clock delay.
	if !late && !isSID && flags&transport.FlagRedundant == 0 {
		jb.adaptDelay(now, ts, seq)
	}

	// Update clock drift controller with current buffer depth
	if jb.driftCtrl != nil && !isSID {
		buffered := 0
		for i := range jb.frames {
			if jb.frames[i].valid {
				buffered++
			}
		}
		jb.driftCtrl.Update(float64(buffered))
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
		if jb.closed {
			return nil, 0, ErrClosed
		}

		idx := int(jb.nextSeq % uint32(jb.capacity))
		frame := &jb.frames[idx]
		distFromOrigin := int32(jb.nextSeq - jb.originSeq)
		expectedPlayout := jb.playoutTime.Add(time.Duration(distFromOrigin) * jb.frameDuration())
		if !time.Now().Before(expectedPlayout) && frame.valid && frame.seq == jb.nextSeq {
			frame.valid = false
			jb.atomicBuffered.Add(-1)
			jb.nextSeq++
			data := frame.data
			if jb.plc != nil {
				if jb.plc.ConsecutiveConcealed() > 0 {
					n := jb.plc.Recover(frame.data, jb.recoverBuf)
					data = append([]byte(nil), jb.recoverBuf[:n]...)
				} else {
					jb.plc.AddHistory(frame.data)
				}
			}
			return data, frame.seq, nil
		}

		// Check if it's time to play out silence or run PLC (underrun or DTX comfort noise)
		if !time.Now().Before(expectedPlayout) {
			if !jb.dtxMode {
				jb.underruns++
				jb.atomicUnderruns.Add(1)
				if jb.plc != nil {
					jb.atomicPLCConcealed.Add(1)
					n := jb.plc.Conceal(jb.concealBuf)
					concealed := append([]byte(nil), jb.concealBuf[:n]...)
					jb.nextSeq++
					return concealed, jb.nextSeq - 1, nil
				}
			}
			// Generate silence frame
			silence := make([]byte, jb.frameSize)
			jb.nextSeq++
			return silence, jb.nextSeq - 1, nil
		}

		// Block until the next frame is due, then re-check for its arrival.
		waitTime := expectedPlayout.Sub(time.Now())
		if waitTime < time.Millisecond {
			waitTime = time.Millisecond
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

	idx := int(jb.nextSeq % uint32(jb.capacity))
	frame := &jb.frames[idx]
	distFromOrigin := int32(jb.nextSeq - jb.originSeq)
	expectedPlayout := jb.playoutTime.Add(time.Duration(distFromOrigin) * jb.frameDuration())
	if time.Now().Before(expectedPlayout) {
		return nil, 0, ErrBufferEmpty
	}
	if !frame.valid || frame.seq != jb.nextSeq {
		if !time.Now().Before(expectedPlayout) {
			if !jb.dtxMode {
				jb.underruns++
				jb.atomicUnderruns.Add(1)
				if jb.plc != nil {
					jb.atomicPLCConcealed.Add(1)
					n := jb.plc.Conceal(jb.concealBuf)
					concealed := append([]byte(nil), jb.concealBuf[:n]...)
					jb.nextSeq++
					return concealed, jb.nextSeq - 1, nil
				}
			}
			silence := make([]byte, jb.frameSize)
			jb.nextSeq++
			return silence, jb.nextSeq - 1, nil
		}
		return nil, 0, ErrBufferEmpty
	}

	frame.valid = false
	jb.atomicBuffered.Add(-1)
	jb.nextSeq++
	data := frame.data
	if jb.plc != nil {
		if jb.plc.ConsecutiveConcealed() > 0 {
			n := jb.plc.Recover(frame.data, jb.recoverBuf)
			data = append([]byte(nil), jb.recoverBuf[:n]...)
		} else {
			jb.plc.AddHistory(frame.data)
		}
	}
	return data, frame.seq, nil
}

func (jb *JitterBuffer) frameDuration() time.Duration {
	// A ring slot is one PCM packet; its duration follows its frame size,
	// not a fixed 20ms assumption. The sender's packet size is determined by
	// StreamConfig.PacketTime and sample rate.
	if jb.sampleRate <= 0 || jb.channels <= 0 {
		return 20 * time.Millisecond
	}
	framesPerPacket := jb.frameSize / (jb.channels * 2)
	if framesPerPacket <= 0 {
		return 20 * time.Millisecond
	}
	return time.Duration(framesPerPacket) * time.Second / time.Duration(jb.sampleRate)
}

func (jb *JitterBuffer) adaptDelay(arrival, senderTS time.Time, seq uint32) {
	if jb.hasJitterSeq && int32(seq-jb.lastJitterSeq) <= 0 {
		return // reordered packets cannot form a valid transit-time pair
	}
	if jb.hasJitterSeq {
		variation := arrival.Sub(jb.lastArrival) - senderTS.Sub(jb.lastSenderTS)
		if variation < 0 {
			variation = -variation
		}
		// Ignore implausible discontinuities (e.g. source clock reset).
		if variation <= jb.maxDelay {
			jb.ewmaDeviation = time.Duration(float64(jb.ewmaDeviation)*(1-jb.alpha) + float64(variation)*jb.alpha)
		}
	}
	jb.lastArrival, jb.lastSenderTS, jb.lastJitterSeq, jb.hasJitterSeq = arrival, senderTS, seq, true

	// Base latency plus a margin for observed transit variation.
	newTarget := jb.minDelay + 4*jb.ewmaDeviation
	if newTarget < jb.minDelay {
		newTarget = jb.minDelay
	}
	if newTarget > jb.maxDelay {
		newTarget = jb.maxDelay
	}
	jb.targetDelay = newTarget
	jb.atomicTargetDelay.Store(int64(newTarget))
	// Do not jump the playout schedule: a sudden delay change would cause a
	// skip or duplicate a packet. Move at most 1ms per received packet.
	step := time.Millisecond
	delta := newTarget - jb.playoutDelay
	if delta > step {
		delta = step
	} else if delta < -step {
		delta = -step
	}
	jb.playoutTime = jb.playoutTime.Add(delta)
	jb.playoutDelay += delta
}

// AtomicLinkStats returns instantaneous jitter buffer metrics without locking the audio thread.
func (jb *JitterBuffer) AtomicLinkStats() (buffered int, targetDelayMS float64, underruns, overruns, plcConcealed uint64) {
	b := jb.atomicBuffered.Load()
	if b < 0 {
		b = 0
	}
	targetNS := jb.atomicTargetDelay.Load()
	if targetNS == 0 {
		targetNS = int64(jb.minDelay)
	}
	return int(b), float64(targetNS) / 1e6, jb.atomicUnderruns.Load(), jb.atomicOverruns.Load(), jb.atomicPLCConcealed.Load()
}

// Stats returns current buffer statistics.
func (jb *JitterBuffer) Stats() JitterBufferStats {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	buffered := 0
	for i := range jb.frames {
		if jb.frames[i].valid {
			buffered++
		}
	}
	stats := JitterBufferStats{
		BufferedFrames: buffered,
		CapacityFrames: jb.capacity,
		TargetDelay:    jb.targetDelay,
		MinDelay:       jb.minDelay,
		MaxDelay:       jb.maxDelay,
		Underruns:      jb.underruns,
		Overruns:       jb.overruns,
		LatePackets:    jb.latePackets,
		EarlyPackets:   jb.earlyPackets,
		DTXMode:        jb.dtxMode,
	}

	if jb.driftCtrl != nil {
		stats.DriftPPM = jb.driftCtrl.DriftPPM()
		stats.ResampleRatio = jb.driftCtrl.Ratio()
		stats.SmoothedWatermark = jb.driftCtrl.SmoothedWatermark()
	}
	if jb.plc != nil {
		stats.PLC = jb.plc.Stats()
	}

	return stats
}

// PLC returns the underlying packet loss concealer.
func (jb *JitterBuffer) PLC() *dsp.PSOLAConcealer {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	return jb.plc
}

// DriftController returns the clock drift controller.
func (jb *JitterBuffer) DriftController() *ClockDriftController {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	return jb.driftCtrl
}

type JitterBufferStats struct {
	BufferedFrames    int           `json:"bufferedFrames"`
	CapacityFrames    int           `json:"capacityFrames"`
	TargetDelay       time.Duration `json:"targetDelay"`
	MinDelay          time.Duration `json:"minDelay"`
	MaxDelay          time.Duration `json:"maxDelay"`
	Underruns         uint64        `json:"underruns"`
	Overruns          uint64        `json:"overruns"`
	LatePackets       uint64        `json:"latePackets"`
	EarlyPackets      uint64        `json:"earlyPackets"`
	DTXMode           bool          `json:"dtxMode,omitempty"`
	DriftPPM          float64       `json:"driftPPM,omitempty"`
	ResampleRatio     float64       `json:"resampleRatio,omitempty"`
	SmoothedWatermark float64       `json:"smoothedWatermark,omitempty"`
	PLC               dsp.PLCStats  `json:"plc,omitempty"`
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
	inRate     int
	outRate    int
	channels   int
	pos        float64
	step       float64
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
		// Passthrough; frame size is unknown without a receiver, so report
		// zero consumed frames to avoid a nil dereference.
		return inData, 0
	}

	frameSize := r.channels * 2
	inFrames := len(inData) / frameSize
	if inFrames == 0 {
		return nil, 0
	}

	// Estimate output frames
	outFrames := int(float64(inFrames)/r.step) + 2
	outData := make([]byte, outFrames*frameSize)

	// Convert input to float
	inFloat := make([]float64, inFrames*r.channels)
	for i := 0; i < inFrames*r.channels; i++ {
		inFloat[i] = float64(int16(inData[i*2])|int16(inData[i*2+1])<<8) / 32768.0
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
