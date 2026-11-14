package telemetry

import (
	"math"
)

// Audibility step-function coefficient tables (ADR 0034 §3). Each table maps
// a windowed (10 s) event count to a multiplicative penalty in (0, 1].
// Boundaries are the exact audibility heuristics documented in the ADR.

// cUnderrun is the jitter-underrun coefficient C_underrun.
func cUnderrun(events uint64) float64 {
	switch {
	case events == 0:
		return 1.00
	case events <= 2:
		return 0.80
	case events <= 7:
		return 0.60
	case events <= 15:
		return 0.40
	case events <= 30:
		return 0.25
	default:
		return 0.10
	}
}

// cOverrun is the jitter-overrun coefficient C_overrun.
func cOverrun(events uint64) float64 {
	switch {
	case events == 0:
		return 1.00
	case events <= 2:
		return 0.90
	case events <= 7:
		return 0.75
	case events <= 15:
		return 0.55
	default:
		return 0.30
	}
}

// cRing is the ring-drop coefficient C_ring (capture + playback drop events).
func cRing(events uint64) float64 {
	switch {
	case events == 0:
		return 1.00
	case events == 1:
		return 0.85
	case events <= 4:
		return 0.65
	case events <= 9:
		return 0.45
	default:
		return 0.25
	}
}

// cXrun is the hardware-XRun coefficient C_xrun, per device direction.
func cXrun(xruns uint64) float64 {
	switch {
	case xruns == 0:
		return 1.00
	case xruns == 1:
		return 0.70
	case xruns <= 3:
		return 0.50
	case xruns <= 7:
		return 0.30
	default:
		return 0.15
	}
}

// cFECUnrec is the FEC-unrecoverable coefficient C_fecUnrec.
func cFECUnrec(events uint64) float64 {
	switch {
	case events == 0:
		return 1.00
	case events <= 2:
		return 0.90
	case events <= 5:
		return 0.75
	default:
		return 0.55
	}
}

// cPLC is the PLC-concealment coefficient C_plc (concealed frames).
func cPLC(frames uint64) float64 {
	switch {
	case frames < 10: // 0-9
		return 1.00
	case frames <= 49:
		return 0.85
	case frames <= 249:
		return 0.65
	default:
		return 0.45
	}
}

// guardedLossCoefficient maps a loss ratio in [0, 1] to the ADR 0010
// C_lossRatio, with NaN/Inf guards: NaN and +Inf are impossible measurements
// that indicate a corrupted counter, so they take the worst coefficient (0);
// -Inf / negative values are clamped to no penalty (1.0).
func guardedLossCoefficient(lossRatio float64) float64 {
	switch {
	case math.IsNaN(lossRatio):
		return 0.0
	case math.IsInf(lossRatio, 1):
		return 0.0
	case math.IsInf(lossRatio, -1) || lossRatio < 0:
		return 1.0
	default:
		return CalculateLossCoefficient(lossRatio * 100.0)
	}
}

// CalculateRouteArtifactScore computes the per-route artifact score (ADR
// 0034 §3):
//
//	RouteScore = clamp(100 * C_underrun * C_overrun * C_ring * C_loss * C_plc, 0, 100)
//
// where C_loss = min(C_fecUnrec, C_lossRatio) and C_lossRatio reuses the
// existing ADR 0010 CalculateLossCoefficient applied to the windowed loss
// ratio. All event counts are windowed (10 s) values; lossRatio is the
// windowed lost/(received+lost) fraction in [0, 1].
func CalculateRouteArtifactScore(underruns, overruns, ringDrops, fecUnrecoverable uint64, lossRatio float64, plcConcealed uint64) float64 {
	cLoss := math.Min(cFECUnrec(fecUnrecoverable), guardedLossCoefficient(lossRatio))
	score := 100.0 *
		cUnderrun(underruns) *
		cOverrun(overruns) *
		cRing(ringDrops) *
		cLoss *
		cPLC(plcConcealed)
	return clamp(score, 0.0, 100.0)
}

// CalculateDeviceArtifactScore computes the per-device artifact score (ADR
// 0034 §3):
//
//	DeviceScore = clamp(100 * C_xrun(input) * C_xrun(output), 0, 100)
//
// A missing direction (hasIn / hasOut false) contributes C = 1.0.
func CalculateDeviceArtifactScore(xrunIn, xrunOut uint64, hasIn, hasOut bool) float64 {
	cin := 1.0
	if hasIn {
		cin = cXrun(xrunIn)
	}
	cout := 1.0
	if hasOut {
		cout = cXrun(xrunOut)
	}
	return clamp(100.0*cin*cout, 0.0, 100.0)
}

// ArtifactSeverityFor maps a 0-100 artifact score to its 4-tier grade.
// Out-of-range and NaN inputs are clamped to the worst grade.
func ArtifactSeverityFor(score float64) ArtifactSeverity {
	if math.IsNaN(score) || score < 0 {
		return SeverityCritical
	}
	switch {
	case score >= 90:
		return SeverityNone
	case score >= 60:
		return SeverityMinor
	case score >= 30:
		return SeverityMajor
	default:
		return SeverityCritical
	}
}
