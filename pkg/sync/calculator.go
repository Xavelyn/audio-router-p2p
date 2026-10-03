package sync

import (
	"math"
	"time"
)

const (
	// MinDriftInterval is the minimum elapsed duration between sync samples required
	// to compute an instantaneous frequency drift rate without high numerical noise.
	MinDriftInterval = 50 * time.Millisecond

	// MaxDriftGap is the maximum elapsed duration between sync samples before previous
	// drift rate history is considered stale (e.g., node reconnection or sleep).
	MaxDriftGap = 60 * time.Second

	// MaxPlausibleDriftPPM defines the maximum plausible oscillator frequency drift (±2000 PPM).
	// Hardware crystal oscillators typically drift within ±50 PPM; clamping protects
	// against step changes (NTP steps, VM time shifts) permanently skewing the filter.
	MaxPlausibleDriftPPM = 2000.0

	// DriftEWMASmoothingAlpha is the weight assigned to new instantaneous drift samples.
	DriftEWMASmoothingAlpha = 0.2
)

// ComputeOffsetAndRTT calculates the NTP clock offset and round-trip delay.
//
// Parameters:
//   - t1ns: client send timestamp (nanoseconds, client clock)
//   - t2ns: server receive timestamp (nanoseconds, server clock)
//   - t3ns: server send timestamp (nanoseconds, server clock)
//   - t4ns: client receive timestamp (nanoseconds, client clock)
//
// Math:
//
//	Offset (θ) = ((T2 - T1) + (T3 - T4)) / 2
//	RTT    (δ) = (T4 - T1) - (T3 - T2)
//
// If measured network delay is negative due to asymmetric host clock adjustments
// or high jitter, RTT is safely bounded to zero.
func ComputeOffsetAndRTT(t1ns, t2ns, t3ns, t4ns int64) (offset time.Duration, rtt time.Duration) {
	offNS := ((t2ns - t1ns) + (t3ns - t4ns)) / 2
	rttNS := (t4ns - t1ns) - (t3ns - t2ns)
	if rttNS < 0 {
		rttNS = 0
	}
	return time.Duration(offNS), time.Duration(rttNS)
}

// ComputeDriftPPM updates the estimated clock frequency drift rate in parts-per-million (PPM)
// using an Exponentially Weighted Moving Average (EWMA).
//
// Parameters:
//   - prevOffset: previous estimated clock offset
//   - newOffset: latest estimated clock offset
//   - prevDrift: previous smoothed drift in PPM
//   - dt: elapsed real time since previous offset measurement
//   - hasPrev: whether previous measurement exists for this peer
func ComputeDriftPPM(prevOffset, newOffset time.Duration, prevDrift float64, dt time.Duration, hasPrev bool) float64 {
	if !hasPrev {
		return 0.0
	}
	if dt < MinDriftInterval {
		return prevDrift
	}
	if dt > MaxDriftGap {
		// Peer reconnected or paused; reset filter to avoid stale EWMA blending
		return 0.0
	}

	deltaOffsetSec := (newOffset - prevOffset).Seconds()
	dtSec := dt.Seconds()
	instantDrift := (deltaOffsetSec / dtSec) * 1e6

	// Clamp to plausible physical oscillator boundaries
	if instantDrift > MaxPlausibleDriftPPM {
		instantDrift = MaxPlausibleDriftPPM
	} else if instantDrift < -MaxPlausibleDriftPPM {
		instantDrift = -MaxPlausibleDriftPPM
	}

	if math.IsNaN(instantDrift) || math.IsInf(instantDrift, 0) {
		return prevDrift
	}

	return (1.0-DriftEWMASmoothingAlpha)*prevDrift + DriftEWMASmoothingAlpha*instantDrift
}
