package streamer

import "time"

// LatencyPreset selects a tuned playout profile for a room's audio routes:
// how aggressively the jitter buffer starts playout, how far it may adapt
// under jitter, and whether the stream is protected with FEC. All presets
// keep the 20 ms packet time so both ends of a route (and any output bus
// they share) always agree on frame sizes.
type LatencyPreset string

const (
	// LatencyPresetLow favors minimum latency: playout starts at 10 ms and
	// the adaptive delay is capped at 50 ms, with no FEC. Best on a good
	// local network.
	LatencyPresetLow LatencyPreset = "low"
	// LatencyPresetBalanced is the default: 20 ms initial delay, up to 200
	// ms adaptive, no FEC.
	LatencyPresetBalanced LatencyPreset = "balanced"
	// LatencyPresetReliable trades extra latency for loss resilience: 40 ms
	// initial delay, up to 400 ms adaptive, an 8 s jitter buffer, and RFC
	// 5109 FEC (K=4). Best on lossy or wide-area links.
	LatencyPresetReliable LatencyPreset = "reliable"
)

// PresetTiming is the playout timing a latency preset prescribes.
type PresetTiming struct {
	MinDelay     time.Duration // playout starts here
	MaxDelay     time.Duration // adaptive delay never exceeds this
	JitterFrames int           // jitter buffer capacity, in packet frames
	FECEnabled   bool
	FECGroupSize int
}

// Valid reports whether the preset name is accepted. The empty name is
// accepted and treated as balanced (no preset requested).
func (p LatencyPreset) Valid() bool {
	switch p {
	case "", LatencyPresetLow, LatencyPresetBalanced, LatencyPresetReliable:
		return true
	}
	return false
}

// ValidPresets returns the accepted preset names, in display order.
func ValidPresets() []string {
	return []string{
		string(LatencyPresetLow),
		string(LatencyPresetBalanced),
		string(LatencyPresetReliable),
	}
}

// Timing returns the playout timing the preset prescribes. Balanced returns
// the network defaults; unknown names behave like balanced.
func (p LatencyPreset) Timing() PresetTiming {
	switch p {
	case LatencyPresetLow:
		return PresetTiming{
			MinDelay:     10 * time.Millisecond,
			MaxDelay:     50 * time.Millisecond,
			JitterFrames: 20,
			FECEnabled:   false,
			FECGroupSize: 4,
		}
	case LatencyPresetReliable:
		return PresetTiming{
			MinDelay:     40 * time.Millisecond,
			MaxDelay:     400 * time.Millisecond,
			JitterFrames: 400,
			FECEnabled:   true,
			FECGroupSize: 4,
		}
	default: // balanced and unknown
		return PresetTiming{
			MinDelay:     20 * time.Millisecond,
			MaxDelay:     200 * time.Millisecond,
			JitterFrames: 200,
			FECEnabled:   false,
			FECGroupSize: 4,
		}
	}
}

// ApplyLatencyPreset returns cfg with the preset's timing parameters
// applied. Balanced and unknown presets leave cfg unchanged, so this is
// safe to call unconditionally with a possibly-empty route preset.
func ApplyLatencyPreset(cfg StreamConfig, p LatencyPreset) StreamConfig {
	t := p.Timing()
	if p == LatencyPresetBalanced {
		return cfg
	}
	cfg.MinDelay = t.MinDelay
	cfg.MaxDelay = t.MaxDelay
	cfg.JitterBufSize = t.JitterFrames
	cfg.FEC.Enabled = t.FECEnabled
	if t.FECGroupSize > 0 {
		cfg.FEC.GroupSize = t.FECGroupSize
	}
	return cfg
}
