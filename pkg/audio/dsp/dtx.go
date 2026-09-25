package dsp

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// DTXState represents the discontinuous transmission finite state machine state.
type DTXState int

const (
	DTXStateActive DTXState = iota
	DTXStateHangover
	DTXStateSilence
)

func (s DTXState) String() string {
	switch s {
	case DTXStateActive:
		return "Active"
	case DTXStateHangover:
		return "Hangover"
	case DTXStateSilence:
		return "Silence"
	default:
		return "Unknown"
	}
}

// DTXConfig holds configuration parameters for Discontinuous Transmission (DTX).
type DTXConfig struct {
	Enabled          bool          `json:"enabled"`
	SilenceThreshold float64       `json:"silence_threshold"` // in dBFS, e.g. -50.0
	HangoverDuration time.Duration `json:"hangover_duration"`  // e.g. 400ms
	SIDInterval      time.Duration `json:"sid_interval"`       // e.g. 500ms
	FrameDuration    time.Duration `json:"frame_duration"`     // e.g. 20ms
}

// DefaultDTXConfig returns recommended DTX parameters.
func DefaultDTXConfig() DTXConfig {
	return DTXConfig{
		Enabled:          true,
		SilenceThreshold: -50.0,
		HangoverDuration: 400 * time.Millisecond,
		SIDInterval:      500 * time.Millisecond,
		FrameDuration:    20 * time.Millisecond,
	}
}

// DTXController executes deterministic voice-activity detection (VAD),
// hangover countdown, and Silence Insertion Descriptor (SID) dispatch intervals.
type DTXController struct {
	mu     sync.Mutex
	config DTXConfig

	state             DTXState
	hangoverFrames    int
	remainingHangover int
	sidFrames         int
	sidCounter        int

	lastMetrics LoudnessMetrics
	suppressed  atomic.Uint64
}

// NewDTXController initializes a DTXController with the given configuration.
func NewDTXController(cfg DTXConfig) *DTXController {
	if cfg.FrameDuration <= 0 {
		cfg.FrameDuration = 20 * time.Millisecond
	}
	if cfg.HangoverDuration <= 0 {
		cfg.HangoverDuration = 400 * time.Millisecond
	}
	if cfg.SIDInterval <= 0 {
		cfg.SIDInterval = 500 * time.Millisecond
	}
	if cfg.SilenceThreshold == 0 {
		cfg.SilenceThreshold = -50.0
	}

	hFrames := int(math.Ceil(float64(cfg.HangoverDuration) / float64(cfg.FrameDuration)))
	if hFrames <= 0 {
		hFrames = 1
	}
	sFrames := int(math.Ceil(float64(cfg.SIDInterval) / float64(cfg.FrameDuration)))
	if sFrames <= 0 {
		sFrames = 1
	}

	return &DTXController{
		config:            cfg,
		state:             DTXStateActive,
		hangoverFrames:    hFrames,
		remainingHangover: hFrames,
		sidFrames:         sFrames,
		sidCounter:        0,
		lastMetrics: LoudnessMetrics{
			RMS:      0,
			RMSdBFS:  SilenceFloorDBFS,
			Peak:     0,
			PeakdBFS: SilenceFloorDBFS,
		},
	}
}

// ProcessFrame analyzes the loudness of the PCM frame and steps the DTX state machine.
// Returns shouldSend (true if packet should be transmitted), isSID (true if Silence
// Insertion Descriptor frame), and the calculated LoudnessMetrics.
func (d *DTXController) ProcessFrame(pcm []byte, channels int) (shouldSend bool, isSID bool, metrics LoudnessMetrics) {
	metrics = AnalyzeLoudness(pcm, channels)

	d.mu.Lock()
	defer d.mu.Unlock()

	d.lastMetrics = metrics

	if !d.config.Enabled {
		d.state = DTXStateActive
		return true, false, metrics
	}

	isActive := metrics.RMSdBFS >= d.config.SilenceThreshold

	if isActive {
		// Active audio detected: transition directly to Active state
		d.state = DTXStateActive
		d.remainingHangover = d.hangoverFrames
		d.sidCounter = 0
		return true, false, metrics
	}

	// Below silence threshold
	switch d.state {
	case DTXStateActive, DTXStateHangover:
		d.state = DTXStateHangover
		d.remainingHangover--
		if d.remainingHangover <= 0 {
			// Hangover expired: enter Silence state and emit initial SID descriptor
			d.state = DTXStateSilence
			d.sidCounter = 0
			return true, true, metrics
		}
		// Transmit speech tail / reverberation decay during hangover
		return true, false, metrics

	case DTXStateSilence:
		d.sidCounter++
		if d.sidCounter >= d.sidFrames {
			// Periodic SID keepalive interval elapsed: dispatch SID packet
			d.sidCounter = 0
			return true, true, metrics
		}
		// Suppress unvoiced frame
		d.suppressed.Add(1)
		return false, false, metrics

	default:
		d.state = DTXStateActive
		return true, false, metrics
	}
}

// State returns the current DTX state in a thread-safe manner.
func (d *DTXController) State() DTXState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// LastMetrics returns the most recent loudness analysis metrics.
func (d *DTXController) LastMetrics() LoudnessMetrics {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastMetrics
}

// SuppressedPackets returns the total number of audio packets dropped during silence suppression.
func (d *DTXController) SuppressedPackets() uint64 {
	return d.suppressed.Load()
}
