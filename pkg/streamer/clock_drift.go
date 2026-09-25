package streamer

import (
	"math"
	"sync"
)

// Default ClockDriftController tuning parameters per ADR 0004.
const (
	DefaultDriftAlpha    = 0.002   // EWMA filter smoothing factor (tau ~ 10s at 50 pps)
	DefaultDriftKp       = 1.0e-5  // Proportional controller gain
	DefaultDriftKi       = 1.0e-7  // Integral controller gain
	DefaultMaxDriftPPM   = 500.0   // +/- 500 ppm (+/- 0.05%) acoustic safety clamp
	DefaultDriftClamp    = DefaultMaxDriftPPM * 1.0e-6 // +/- 0.0005
)

// ClockDriftConfig holds configuration parameters for clock drift compensation.
type ClockDriftConfig struct {
	Alpha           float64 // EWMA filter smoothing factor
	Kp              float64 // Proportional gain
	Ki              float64 // Integral gain
	MaxDriftPPM     float64 // Maximum allowed drift offset in PPM
	TargetWatermark float64 // Target jitter buffer watermark (in frames)
	FrameDuration   float64 // Frame duration in seconds (default 0.02 for 20ms)
}

// DefaultClockDriftConfig returns standard ADR 0004 settings.
func DefaultClockDriftConfig(targetWatermark float64) ClockDriftConfig {
	if targetWatermark <= 0 {
		targetWatermark = 3.0 // default ~3 frames target depth
	}
	return ClockDriftConfig{
		Alpha:           DefaultDriftAlpha,
		Kp:              DefaultDriftKp,
		Ki:              DefaultDriftKi,
		MaxDriftPPM:     DefaultMaxDriftPPM,
		TargetWatermark: targetWatermark,
		FrameDuration:   0.02,
	}
}

// ClockDriftController dynamically tracks jitter buffer watermark divergence and
// computes the fractional playout resampling ratio correction to neutralize
// crystal oscillator drift.
//
// Concurrency: Thread-safe via internal mutex.
type ClockDriftController struct {
	mu sync.Mutex

	cfg ClockDriftConfig

	smoothedWatermark float64
	integral          float64
	deltaR            float64
	ratio             float64
	initialized       bool
}

// NewClockDriftController creates a new ClockDriftController.
func NewClockDriftController(cfg ClockDriftConfig) *ClockDriftController {
	if cfg.Alpha <= 0 || cfg.Alpha > 1.0 {
		cfg.Alpha = DefaultDriftAlpha
	}
	if cfg.Kp <= 0 {
		cfg.Kp = DefaultDriftKp
	}
	if cfg.Ki <= 0 {
		cfg.Ki = DefaultDriftKi
	}
	if cfg.MaxDriftPPM <= 0 {
		cfg.MaxDriftPPM = DefaultMaxDriftPPM
	}
	if cfg.TargetWatermark <= 0 {
		cfg.TargetWatermark = 3.0
	}
	if cfg.FrameDuration <= 0 {
		cfg.FrameDuration = 0.02
	}

	return &ClockDriftController{
		cfg:               cfg,
		smoothedWatermark: cfg.TargetWatermark,
		ratio:             1.0,
		initialized:       true,
	}
}

// Update feeds an instantaneous jitter buffer depth sample (in frames) into the
// EWMA low-pass filter and executes one step of the PI controller.
// Returns the updated resample ratio r = 1.0 + delta_r.
func (c *ClockDriftController) Update(depth float64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if math.IsNaN(depth) || math.IsInf(depth, 0) {
		return c.ratio
	}

	// Stage 1: EWMA Low-Pass Watermark Filter
	if !c.initialized {
		c.smoothedWatermark = depth
		c.initialized = true
	} else {
		c.smoothedWatermark = (1.0-c.cfg.Alpha)*c.smoothedWatermark + c.cfg.Alpha*depth
	}

	// Stage 2: PI Loop Controller
	// Error e_t = W_t - W_target.
	err := c.smoothedWatermark - c.cfg.TargetWatermark

	// Integrator update with dt and anti-windup clamping
	maxClamp := c.cfg.MaxDriftPPM * 1.0e-6
	maxIntegral := maxClamp / c.cfg.Ki

	dt := c.cfg.FrameDuration
	if dt <= 0 {
		dt = 0.02
	}
	c.integral += err * dt
	if c.integral > maxIntegral {
		c.integral = maxIntegral
	} else if c.integral < -maxIntegral {
		c.integral = -maxIntegral
	}

	// Compute PI output
	rawDelta := c.cfg.Kp*err + c.cfg.Ki*c.integral

	// Hard clamp to +/- MaxDriftPPM (+/- 500 ppm)
	if rawDelta > maxClamp {
		rawDelta = maxClamp
	} else if rawDelta < -maxClamp {
		rawDelta = -maxClamp
	}

	c.deltaR = rawDelta
	c.ratio = 1.0 + c.deltaR
	return c.ratio
}

// Ratio returns the current target resampling ratio.
func (c *ClockDriftController) Ratio() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		return 1.0
	}
	return c.ratio
}

// DriftPPM returns the current estimated clock drift in parts-per-million (PPM).
func (c *ClockDriftController) DriftPPM() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deltaR * 1.0e6
}

// SmoothedWatermark returns the current EWMA-smoothed buffer depth in frames.
func (c *ClockDriftController) SmoothedWatermark() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.smoothedWatermark
}

// TargetWatermark returns the nominal target buffer depth in frames.
func (c *ClockDriftController) TargetWatermark() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.TargetWatermark
}

// SetTarget updates the target buffer depth watermark.
func (c *ClockDriftController) SetTarget(target float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if target > 0 {
		c.cfg.TargetWatermark = target
	}
}

// Reset clears the filter and integrator state.
func (c *ClockDriftController) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initialized = false
	c.smoothedWatermark = 0
	c.integral = 0
	c.deltaR = 0
	c.ratio = 1.0
}
