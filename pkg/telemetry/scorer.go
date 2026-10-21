package telemetry

import (
	"math"
	"strings"
)

// CalculateLossCoefficient computes C_loss per ADR 0010.
// lossPercent is in the range [0.0, 100.0].
func CalculateLossCoefficient(lossPercent float64) float64 {
	if math.IsNaN(lossPercent) || math.IsInf(lossPercent, 0) || lossPercent <= 0.5 {
		return 1.0
	}
	if lossPercent <= 5.0 {
		c := 1.0 - 0.08*(lossPercent-0.5)
		return clamp(c, 0.0, 1.0)
	}
	if lossPercent <= 20.0 {
		c := 0.64 - 0.035*(lossPercent-5.0)
		return clamp(c, 0.0, 1.0)
	}
	c := math.Max(0.0, 0.115-0.01*(lossPercent-20.0))
	return clamp(c, 0.0, 1.0)
}

// CalculateJitterCoefficient computes C_jitter per ADR 0010 based on
// underruns in a 10s sliding window and jitter target delay in milliseconds.
func CalculateJitterCoefficient(underruns10s uint64, jitterMS float64) float64 {
	if math.IsNaN(jitterMS) || math.IsInf(jitterMS, 0) || jitterMS < 0 {
		jitterMS = 0
	}
	underrunPenalty := math.Min(0.5, float64(underruns10s)*0.15)
	jitterPenalty := math.Min(0.5, jitterMS/100.0)
	c := 1.0 - underrunPenalty - jitterPenalty
	return clamp(c, 0.0, 1.0)
}

// CalculateClockCoefficient computes C_clock per ADR 0010 based on
// clock offset in microseconds (offsetUS) and drift in PPM (driftPPM).
func CalculateClockCoefficient(offsetUS float64, driftPPM float64) float64 {
	if math.IsNaN(offsetUS) || math.IsInf(offsetUS, 0) {
		offsetUS = 0
	}
	if math.IsNaN(driftPPM) || math.IsInf(driftPPM, 0) {
		driftPPM = 0
	}

	absOffset := math.Abs(offsetUS)
	absDrift := math.Abs(driftPPM)

	offsetPenalty := 0.0
	if absOffset > 500.0 {
		penalty := math.Log10(absOffset / 500.0)
		if penalty < 0 {
			penalty = 0
		}
		offsetPenalty = math.Min(0.35, penalty)
	}

	driftPenalty := 0.0
	if absDrift > 50.0 {
		driftPenalty = 0.15
	}

	c := 1.0 - offsetPenalty - driftPenalty
	return clamp(c, 0.0, 1.0)
}

// CalculateSWIMCoefficient computes C_swim per ADR 0010 based on
// memberlist SWIM failure detector state and direct ping reachability.
func CalculateSWIMCoefficient(swimState string, directPingFailed bool) float64 {
	state := strings.ToLower(strings.TrimSpace(swimState))
	switch state {
	case "suspect":
		return 0.4
	case "dead", "left":
		return 0.0
	case "alive", "":
		if directPingFailed {
			return 0.85
		}
		return 1.0
	default:
		if directPingFailed {
			return 0.85
		}
		return 1.0
	}
}

// CalculatePeerHealthScore evaluates the composite 0-100 peer health score per ADR 0010:
// H_peer = max(0, min(100, 100 * C_loss * C_jitter * C_clock * C_swim))
func CalculatePeerHealthScore(
	lossPercent float64,
	underruns10s uint64,
	jitterMS float64,
	offsetUS float64,
	driftPPM float64,
	swimState string,
	directPingFailed bool,
) float64 {
	cLoss := CalculateLossCoefficient(lossPercent)
	cJitter := CalculateJitterCoefficient(underruns10s, jitterMS)
	cClock := CalculateClockCoefficient(offsetUS, driftPPM)
	cSWIM := CalculateSWIMCoefficient(swimState, directPingFailed)

	raw := 100.0 * cLoss * cJitter * cClock * cSWIM
	return clamp(raw, 0.0, 100.0)
}

// CalculateNodeHealthScore computes the aggregate node health score per ADR 0010.
// If N_peers == 0, returns 100.0.
// Otherwise evaluates harmonic mean of peer scores multiplied by local hardware penalty.
func CalculateNodeHealthScore(peerScores []float64, localHWUnderruns10s uint64) float64 {
	if len(peerScores) == 0 {
		return 100.0
	}

	var sumInv float64
	validPeers := 0
	for _, score := range peerScores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			score = 0.0
		}
		effectiveScore := math.Max(1.0, score)
		sumInv += 1.0 / effectiveScore
		validPeers++
	}

	if validPeers == 0 || sumInv <= 0 || math.IsNaN(sumInv) {
		return 100.0
	}

	hMean := float64(validPeers) / sumInv
	pLocal := 1.0 - math.Min(0.5, float64(localHWUnderruns10s)*0.1)

	return clamp(hMean*pLocal, 0.0, 100.0)
}

// CalculateGrade maps a 0-100 health score to its operational NodeHealthGrade.
func CalculateGrade(score float64) NodeHealthGrade {
	if math.IsNaN(score) || score < 25.0 {
		return GradeCritical
	}
	switch {
	case score >= 90.0:
		return GradeExcellent
	case score >= 75.0:
		return GradeGood
	case score >= 50.0:
		return GradeFair
	case score >= 25.0:
		return GradePoor
	default:
		return GradeCritical
	}
}

// clamp bounds val to [min, max] and strictly neutralizes IEEE 754 NaN/Inf.
func clamp(val, min, max float64) float64 {
	if math.IsNaN(val) {
		return min
	}
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}
