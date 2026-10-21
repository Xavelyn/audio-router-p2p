package audio

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors for ghost audio streams.
var ErrStreamClosed = errors.New("audio stream closed")

// Ensure GhostAudioStream satisfies AudioStream and BufferedStream.
var (
	_ AudioStream    = (*GhostAudioStream)(nil)
	_ BufferedStream = (*GhostAudioStream)(nil)
)

// GhostAudioStream generates real-time paced silence for disconnected or quarantined devices.
// It keeps room mixers, WebRTC streams, and matrix pipelines running without stalls or buffer underflow.
type GhostAudioStream struct {
	sampleRate int
	channels   int
	frameSize  int

	paced     atomic.Bool
	closed    atomic.Bool
	closeCh   chan struct{}
	closeOnce sync.Once

	// Precision pacing state
	mu        sync.Mutex
	lastRead  time.Time
	lastWrite time.Time
}

// NewGhostAudioStream creates a clock-paced silence generator for quarantined audio endpoints.
func NewGhostAudioStream(sampleRate, channels int) *GhostAudioStream {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if channels <= 0 {
		channels = 2
	}
	g := &GhostAudioStream{
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  channels * 2, // 16-bit PCM = 2 bytes per sample
		closeCh:    make(chan struct{}),
		lastRead:   time.Now(),
		lastWrite:  time.Now(),
	}
	g.paced.Store(true)
	return g
}

// SetPaced toggles real-time monotonic sleep pacing safely across goroutines.
func (g *GhostAudioStream) SetPaced(paced bool) {
	g.paced.Store(paced)
}

// Read fills p with calibrated zero-samples (silence), paced to nominal audio clock time.
func (g *GhostAudioStream) Read(p []byte) (int, error) {
	if g.closed.Load() {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	if g.paced.Load() && g.sampleRate > 0 && g.frameSize > 0 {
		frames := len(p) / g.frameSize
		if frames == 0 {
			// Ensure sub-frame requests still pace appropriately to avoid CPU spinning
			frames = 1
		}
		nominalDuration := time.Duration(float64(frames) / float64(g.sampleRate) * float64(time.Second))

		g.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(g.lastRead)
		sleepDur := nominalDuration - elapsed
		if sleepDur > 0 {
			g.mu.Unlock()
			timer := time.NewTimer(sleepDur)
			select {
			case <-g.closeCh:
				timer.Stop()
				return 0, io.EOF
			case <-timer.C:
			}
			g.mu.Lock()
			g.lastRead = time.Now()
		} else {
			// Falling behind clock; advance lastRead to current time
			g.lastRead = now
		}
		g.mu.Unlock()
	}

	clear(p)
	return len(p), nil
}

// Write absorbs PCM frames and safely discards them, preventing buffer build-up while pacing consumption.
func (g *GhostAudioStream) Write(p []byte) (int, error) {
	if g.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}

	if g.paced.Load() && g.sampleRate > 0 && g.frameSize > 0 {
		frames := len(p) / g.frameSize
		if frames == 0 {
			frames = 1
		}
		nominalDuration := time.Duration(float64(frames) / float64(g.sampleRate) * float64(time.Second))

		g.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(g.lastWrite)
		sleepDur := nominalDuration - elapsed
		if sleepDur > 0 {
			g.mu.Unlock()
			timer := time.NewTimer(sleepDur)
			select {
			case <-g.closeCh:
				timer.Stop()
				return 0, io.ErrClosedPipe
			case <-timer.C:
			}
			g.mu.Lock()
			g.lastWrite = time.Now()
		} else {
			g.lastWrite = now
		}
		g.mu.Unlock()
	}

	return len(p), nil
}

// Buffered returns the buffer depth, which is always 0 for the ghost generator.
func (g *GhostAudioStream) Buffered() int {
	return 0
}

// Close terminates the ghost stream, unblocking any pending Read/Write calls immediately.
func (g *GhostAudioStream) Close() error {
	g.closed.Store(true)
	g.closeOnce.Do(func() {
		close(g.closeCh)
	})
	return nil
}
