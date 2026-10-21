package telemetry

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SilenceFloorDBFS defines the theoretical dynamic range floor for 16-bit linear PCM.
const SilenceFloorDBFS = -96.0

// ChannelMeter captures real-time level metrics for an individual audio channel.
type ChannelMeter struct {
	Channel  int     `json:"channel"`
	RMSdBFS  float64 `json:"rms_dbfs"`
	PeakdBFS float64 `json:"peak_dbfs"`
	Clipped  bool    `json:"clipped"`
}

// StreamMeterFrame captures real-time meter telemetry for a stream across all channels.
type StreamMeterFrame struct {
	StreamID    string         `json:"stream_id"`
	TimestampNS int64          `json:"timestamp_ns"`
	DTXState    string         `json:"dtx_state"`
	Channels    []ChannelMeter `json:"channels"`
}

// MeterFrame is an alias for StreamMeterFrame.
type MeterFrame = StreamMeterFrame

// AtomicChannelTap records samples lock-free on the real-time audio thread.
type AtomicChannelTap struct {
	sampleCnt atomic.Uint32
	peakVal   atomic.Uint32
	sumSq     atomic.Uint64
	clipped   atomic.Bool
}

// RecordSample records an interleaved 16-bit audio sample without locks or heap allocations.
func (t *AtomicChannelTap) RecordSample(s int16) {
	val := int32(s)
	if val < 0 {
		val = -val
	}
	uval := uint32(val)
	if uval >= 32767 {
		t.clipped.Store(true)
		uval = 32767
	}
	// Lock-free CAS loop to update peak
	for {
		curr := t.peakVal.Load()
		if uval <= curr {
			break
		}
		if t.peakVal.CompareAndSwap(curr, uval) {
			break
		}
	}
	t.sumSq.Add(uint64(val * val))
	t.sampleCnt.Add(1)
}

// Snapshot atomically extracts and resets accumulated loudness metrics.
func (t *AtomicChannelTap) Snapshot() (rmsDBFS, peakDBFS float64, clipped bool) {
	cnt := t.sampleCnt.Swap(0)
	peak := t.peakVal.Swap(0)
	sumSq := t.sumSq.Swap(0)
	clipped = t.clipped.Swap(false)

	if cnt == 0 {
		return SilenceFloorDBFS, SilenceFloorDBFS, clipped
	}

	rms := math.Sqrt(float64(sumSq) / float64(cnt))
	if rms <= 1e-6 {
		rmsDBFS = SilenceFloorDBFS
	} else {
		rmsDBFS = 20.0 * math.Log10(rms/32768.0)
		if rmsDBFS < SilenceFloorDBFS {
			rmsDBFS = SilenceFloorDBFS
		}
	}

	if peak == 0 {
		peakDBFS = SilenceFloorDBFS
	} else {
		peakDBFS = 20.0 * math.Log10(float64(peak)/32768.0)
		if peakDBFS < SilenceFloorDBFS {
			peakDBFS = SilenceFloorDBFS
		}
	}

	return rmsDBFS, peakDBFS, clipped
}

// StreamTap manages a set of lock-free channel taps for a specific stream.
type StreamTap struct {
	streamID string
	channels []*AtomicChannelTap
	dtxState atomic.Pointer[string]
}

// NewStreamTap creates a tap with a designated number of audio channels.
func NewStreamTap(streamID string, channels int) *StreamTap {
	if channels <= 0 {
		channels = 2
	}
	taps := make([]*AtomicChannelTap, channels)
	for i := 0; i < channels; i++ {
		taps[i] = &AtomicChannelTap{}
	}
	st := &StreamTap{
		streamID: streamID,
		channels: taps,
	}
	defDTX := "Active"
	st.dtxState.Store(&defDTX)
	return st
}

// StreamID returns the stream identifier.
func (st *StreamTap) StreamID() string {
	return st.streamID
}

// RecordBlock updates all channel taps from interleaved 16-bit LE PCM data.
// Executes with zero allocations and zero mutex locks.
func (st *StreamTap) RecordBlock(pcm []byte, channels int) {
	numSamples := len(pcm) / 2
	for i := 0; i < numSamples; i++ {
		ch := i % channels
		if ch < len(st.channels) {
			sample := int16(binary.LittleEndian.Uint16(pcm[i*2 : (i+1)*2]))
			st.channels[ch].RecordSample(sample)
		}
	}
}

// SetDTXState records voice activity detection state.
func (st *StreamTap) SetDTXState(state string) {
	st.dtxState.Store(&state)
}

// Snapshot reads and resets the tap into a StreamMeterFrame.
func (st *StreamTap) Snapshot(timestampNS int64) StreamMeterFrame {
	dtx := "Active"
	if ptr := st.dtxState.Load(); ptr != nil {
		dtx = *ptr
	}
	chans := make([]ChannelMeter, len(st.channels))
	for i, tap := range st.channels {
		rms, peak, clipped := tap.Snapshot()
		chans[i] = ChannelMeter{
			Channel:  i,
			RMSdBFS:  math.Round(rms*100) / 100,
			PeakdBFS: math.Round(peak*100) / 100,
			Clipped:  clipped,
		}
	}
	return StreamMeterFrame{
		StreamID:    st.streamID,
		TimestampNS: timestampNS,
		DTXState:    dtx,
		Channels:    chans,
	}
}

// Subscriber represents an SSE or WebSocket listener with bounded buffer.
type Subscriber struct {
	ID            string
	Ch            chan []byte
	DroppedFrames atomic.Uint64
	closed        atomic.Bool
}

// Client is an alias for Subscriber.
type Client = Subscriber

// TelemetryHub manages high-frequency meter broadcasting.
type TelemetryHub struct {
	mu          sync.RWMutex
	rateHz      int
	subscribers map[string]*Subscriber
	taps        map[string]*StreamTap
	nextSubID   atomic.Uint64
	stopCh      chan struct{}
	running     atomic.Bool
	pool        sync.Pool
}

// NewTelemetryHub constructs a TelemetryHub with configurable rate (20-50 Hz, default 30 Hz).
func NewTelemetryHub(rateHz int) *TelemetryHub {
	if rateHz <= 0 {
		rateHz = 30
	} else if rateHz < 20 {
		rateHz = 20
	} else if rateHz > 50 {
		rateHz = 50
	}

	h := &TelemetryHub{
		rateHz:      rateHz,
		subscribers: make(map[string]*Subscriber),
		taps:        make(map[string]*StreamTap),
		stopCh:      make(chan struct{}),
	}
	h.pool = sync.Pool{
		New: func() any {
			b := make([]byte, 0, 1024)
			return &b
		},
	}
	return h
}

// RateHz returns the broadcast rate in Hertz.
func (h *TelemetryHub) RateHz() int {
	return h.rateHz
}

// RegisterTap adds or replaces an atomic stream tap.
func (h *TelemetryHub) RegisterTap(streamID string, channels int) *StreamTap {
	h.mu.Lock()
	defer h.mu.Unlock()
	tap := NewStreamTap(streamID, channels)
	h.taps[streamID] = tap
	return tap
}

// UnregisterTap removes a stream tap.
func (h *TelemetryHub) UnregisterTap(streamID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.taps, streamID)
}

// GetTap retrieves an existing stream tap.
func (h *TelemetryHub) GetTap(streamID string) (*StreamTap, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	tap, ok := h.taps[streamID]
	return tap, ok
}

// Subscribe registers a new subscriber with a bounded channel (default capacity 4).
func (h *TelemetryHub) Subscribe(bufCap ...int) *Subscriber {
	capSize := 4
	if len(bufCap) > 0 && bufCap[0] > 0 {
		capSize = bufCap[0]
	}
	subID := fmt.Sprintf("sub-%d", h.nextSubID.Add(1))
	sub := &Subscriber{
		ID: subID,
		Ch: make(chan []byte, capSize),
	}

	h.mu.Lock()
	h.subscribers[subID] = sub
	h.mu.Unlock()

	return sub
}

// Unsubscribe removes a subscriber and closes its channel.
func (h *TelemetryHub) Unsubscribe(sub *Subscriber) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	delete(h.subscribers, sub.ID)
	h.mu.Unlock()

	if sub.closed.CompareAndSwap(false, true) {
		close(sub.Ch)
	}
}

// SubscriberCount returns the active subscriber count.
func (h *TelemetryHub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// Broadcast dispatches a meter message to all subscribers using non-blocking drop-on-lag fan-out.
func (h *TelemetryHub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.subscribers {
		select {
		case client.Ch <- msg:
			// Frame delivered
		default:
			// Slow consumer: drop frame to prevent backpressure
			client.DroppedFrames.Add(1)
		}
	}
}

// Start begins the ticker-driven broadcast loop.
func (h *TelemetryHub) Start(ctx context.Context) {
	if !h.running.CompareAndSwap(false, true) {
		return
	}

	interval := time.Second / time.Duration(h.rateHz)
	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				h.running.Store(false)
				return
			case <-h.stopCh:
				h.running.Store(false)
				return
			case <-ticker.C:
				h.tick()
			}
		}
	}()
}

// Stop shuts down the hub loop.
func (h *TelemetryHub) Stop() {
	if h.running.CompareAndSwap(true, false) {
		close(h.stopCh)
	}
}

// tick gathers snapshots from all active taps and broadcasts JSON payloads.
func (h *TelemetryHub) tick() {
	h.mu.RLock()
	if len(h.subscribers) == 0 || len(h.taps) == 0 {
		h.mu.RUnlock()
		return
	}
	// Take snapshot pointers under read lock
	taps := make([]*StreamTap, 0, len(h.taps))
	for _, tap := range h.taps {
		taps = append(taps, tap)
	}
	h.mu.RUnlock()

	now := time.Now().UnixNano()
	for _, tap := range taps {
		frame := tap.Snapshot(now)
		bufPtr := h.pool.Get().(*[]byte)
		*bufPtr = (*bufPtr)[:0]
		*bufPtr = AppendMeterFrameJSON(*bufPtr, &frame)

		// Create immutable byte slice for fan-out
		msg := make([]byte, len(*bufPtr))
		copy(msg, *bufPtr)
		h.pool.Put(bufPtr)

		h.Broadcast(msg)
	}
}

// SerializeFrame serializes a StreamMeterFrame into JSON using the hub's sync.Pool.
// Returns a byte slice formatted with zero heap allocations during steady-state.
func (h *TelemetryHub) SerializeFrame(frame *StreamMeterFrame, dst *[]byte) []byte {
	return AppendMeterFrameJSON(*dst, frame)
}

// GetBuffer gets a reusable byte slice from the pool.
func (h *TelemetryHub) GetBuffer() *[]byte {
	bufPtr := h.pool.Get().(*[]byte)
	*bufPtr = (*bufPtr)[:0]
	return bufPtr
}

// PutBuffer returns a byte slice to the pool.
func (h *TelemetryHub) PutBuffer(b *[]byte) {
	h.pool.Put(b)
}

// AppendMeterFrameJSON encodes a StreamMeterFrame into JSON directly into dst without allocations.
func AppendMeterFrameJSON(dst []byte, frame *StreamMeterFrame) []byte {
	dst = append(dst, `{"stream_id":"`...)
	dst = append(dst, frame.StreamID...)
	dst = append(dst, `","timestamp_ns":`...)
	dst = strconv.AppendInt(dst, frame.TimestampNS, 10)
	dst = append(dst, `,"dtx_state":"`...)
	dst = append(dst, frame.DTXState...)
	dst = append(dst, `","channels":[`...)
	for i := range frame.Channels {
		if i > 0 {
			dst = append(dst, ',')
		}
		ch := &frame.Channels[i]
		dst = append(dst, `{"channel":`...)
		dst = strconv.AppendInt(dst, int64(ch.Channel), 10)
		dst = append(dst, `,"rms_dbfs":`...)
		dst = strconv.AppendFloat(dst, ch.RMSdBFS, 'f', 2, 64)
		dst = append(dst, `,"peak_dbfs":`...)
		dst = strconv.AppendFloat(dst, ch.PeakdBFS, 'f', 2, 64)
		if ch.Clipped {
			dst = append(dst, `,"clipped":true}`...)
		} else {
			dst = append(dst, `,"clipped":false}`...)
		}
	}
	dst = append(dst, `]}`...)
	return dst
}
