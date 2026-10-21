package telemetry

import (
	"math"
	"testing"
)

func TestCalculateLossCoefficient(t *testing.T) {
	tests := []struct {
		name        string
		lossPercent float64
		expected    float64
	}{
		{"zero loss", 0.0, 1.0},
		{"below 0.5%", 0.4, 1.0},
		{"at 0.5%", 0.5, 1.0},
		{"at 1.0%", 1.0, 1.0 - 0.08*(1.0-0.5)},
		{"at 5.0%", 5.0, 0.64},
		{"at 10.0%", 10.0, 0.64 - 0.035*(10.0-5.0)},
		{"at 20.0%", 20.0, 0.115},
		{"at 25.0%", 25.0, 0.115 - 0.01*(25.0-20.0)},
		{"at 35.0%", 35.0, 0.0},
		{"extreme 100%", 100.0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateLossCoefficient(tt.lossPercent)
			if math.Abs(got-tt.expected) > 1e-6 {
				t.Errorf("lossPercent=%f: got %f, want %f", tt.lossPercent, got, tt.expected)
			}
		})
	}
}

func TestCalculateJitterCoefficient(t *testing.T) {
	tests := []struct {
		name         string
		underruns10s uint64
		jitterMS     float64
		expected     float64
	}{
		{"perfect conditions", 0, 0, 1.0},
		{"jitter 20ms", 0, 20.0, 0.8},
		{"jitter 100ms max penalty", 0, 100.0, 0.5},
		{"underruns 1", 1, 0, 0.85},
		{"underruns 2", 2, 0, 0.70},
		{"underruns 4 max penalty", 4, 0, 0.5},
		{"both jitter and underruns maxed", 4, 100.0, 0.0},
		{"negative jitterMS clamped", 0, -10.0, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateJitterCoefficient(tt.underruns10s, tt.jitterMS)
			if math.Abs(got-tt.expected) > 1e-6 {
				t.Errorf("underruns=%d, jitter=%f: got %f, want %f", tt.underruns10s, tt.jitterMS, got, tt.expected)
			}
		})
	}
}

func TestCalculateClockCoefficient(t *testing.T) {
	tests := []struct {
		name     string
		offsetUS float64
		driftPPM float64
		expected float64
	}{
		{"locked and low drift", 10.0, 5.0, 1.0},
		{"offset exactly 500us", 500.0, 20.0, 1.0},
		{"offset 5000us (penalty log10(10)=1.0 clamped to 0.35)", 5000.0, 10.0, 0.65},
		{"drift 60ppm (deduction 0.15)", 100.0, 60.0, 0.85},
		{"offset 5000us and drift 60ppm", 5000.0, 60.0, 0.50},
		{"negative offset treated symmetrically", -500.0, -10.0, 1.0},
		{"negative offset large", -5000.0, 10.0, 0.65},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateClockCoefficient(tt.offsetUS, tt.driftPPM)
			if math.Abs(got-tt.expected) > 1e-6 {
				t.Errorf("offset=%f, drift=%f: got %f, want %f", tt.offsetUS, tt.driftPPM, got, tt.expected)
			}
		})
	}
}

func TestCalculateSWIMCoefficient(t *testing.T) {
	tests := []struct {
		name             string
		state            string
		directPingFailed bool
		expected         float64
	}{
		{"alive direct", "alive", false, 1.0},
		{"alive indirect", "alive", true, 0.85},
		{"suspect", "suspect", false, 0.4},
		{"dead", "dead", false, 0.0},
		{"left", "left", false, 0.0},
		{"case insensitivity", "ALIVE", false, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateSWIMCoefficient(tt.state, tt.directPingFailed)
			if math.Abs(got-tt.expected) > 1e-6 {
				t.Errorf("state=%s, directPingFailed=%v: got %f, want %f", tt.state, tt.directPingFailed, got, tt.expected)
			}
		})
	}
}

func TestCalculatePeerHealthScore(t *testing.T) {
	// Perfect link
	score := CalculatePeerHealthScore(0.0, 0, 0.0, 50.0, 5.0, "alive", false)
	if math.Abs(score-100.0) > 1e-6 {
		t.Errorf("expected 100.0 for perfect link, got %f", score)
	}

	// Degraded link with loss
	scoreLoss := CalculatePeerHealthScore(5.0, 0, 0.0, 50.0, 5.0, "alive", false)
	if math.Abs(scoreLoss-64.0) > 1e-6 {
		t.Errorf("expected 64.0 for 5%% loss, got %f", scoreLoss)
	}

	// Suspect node
	scoreSuspect := CalculatePeerHealthScore(0.0, 0, 0.0, 50.0, 5.0, "suspect", false)
	if math.Abs(scoreSuspect-40.0) > 1e-6 {
		t.Errorf("expected 40.0 for suspect node, got %f", scoreSuspect)
	}
}

func TestCalculateNodeHealthScore(t *testing.T) {
	// Zero peers -> 100.0
	if s := CalculateNodeHealthScore(nil, 0); s != 100.0 {
		t.Errorf("expected 100 for 0 peers, got %f", s)
	}

	// Single peer with score 80
	s1 := CalculateNodeHealthScore([]float64{80.0}, 0)
	if math.Abs(s1-80.0) > 1e-6 {
		t.Errorf("expected 80.0 for single peer, got %f", s1)
	}

	// Harmonic mean of 100 and 50:
	// 2 / (1/100 + 1/50) = 2 / (0.01 + 0.02) = 2 / 0.03 = 66.6666...
	s2 := CalculateNodeHealthScore([]float64{100.0, 50.0}, 0)
	expectedHMean := 200.0 / 3.0
	if math.Abs(s2-expectedHMean) > 1e-6 {
		t.Errorf("expected %f, got %f", expectedHMean, s2)
	}

	// Local hardware underrun penalty
	sPenalty := CalculateNodeHealthScore([]float64{100.0}, 2)
	// P_local = 1.0 - min(0.5, 2*0.1) = 0.8
	if math.Abs(sPenalty-80.0) > 1e-6 {
		t.Errorf("expected 80.0 with 2 local underruns, got %f", sPenalty)
	}
}

func TestCalculateGrade(t *testing.T) {
	if CalculateGrade(95.0) != GradeExcellent {
		t.Errorf("expected EXCELLENT for 95")
	}
	if CalculateGrade(80.0) != GradeGood {
		t.Errorf("expected GOOD for 80")
	}
	if CalculateGrade(60.0) != GradeFair {
		t.Errorf("expected FAIR for 60")
	}
	if CalculateGrade(30.0) != GradePoor {
		t.Errorf("expected POOR for 30")
	}
	if CalculateGrade(10.0) != GradeCritical {
		t.Errorf("expected CRITICAL for 10")
	}
}
