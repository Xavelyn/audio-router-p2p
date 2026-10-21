package streamer

import (
	"math"
	"testing"
)

func TestClockDriftController_PositiveDriftConvergence(t *testing.T) {
	targetWatermark := 3.0
	cfg := DefaultClockDriftConfig(targetWatermark)
	// Tuning for stable and fast simulation convergence
	cfg.Kp = 5.0e-4
	cfg.Ki = 1.0e-5

	ctrl := NewClockDriftController(cfg)

	// Simulated sender clock running +30 ppm faster than receiver
	senderDrift := +30.0 * 1.0e-6 // +0.000030
	depth := targetWatermark

	// Run 100,000 steps (~33 minutes of 50pps audio)
	for step := 0; step < 100000; step++ {
		ratio := ctrl.Update(depth)
		// Playout consumes ratio frames per cycle, while sender produces 1 + drift frames
		delta := (1.0 + senderDrift) - ratio
		depth += delta
	}

	ppm := ctrl.DriftPPM()
	if math.Abs(ppm-30.0) > 2.0 {
		t.Fatalf("expected drift to converge near +30 ppm, got %f ppm", ppm)
	}

	watermark := ctrl.SmoothedWatermark()
	if math.Abs(watermark-targetWatermark) > 0.5 {
		t.Fatalf("expected smoothed watermark near %f, got %f", targetWatermark, watermark)
	}
}

func TestClockDriftController_NegativeDriftConvergence(t *testing.T) {
	targetWatermark := 3.0
	cfg := DefaultClockDriftConfig(targetWatermark)
	cfg.Kp = 5.0e-4
	cfg.Ki = 1.0e-5

	ctrl := NewClockDriftController(cfg)

	// Simulated sender clock running -30 ppm slower than receiver
	senderDrift := -30.0 * 1.0e-6 // -0.000030
	depth := targetWatermark

	for step := 0; step < 100000; step++ {
		ratio := ctrl.Update(depth)
		delta := (1.0 + senderDrift) - ratio
		depth += delta
	}

	ppm := ctrl.DriftPPM()
	if math.Abs(ppm-(-30.0)) > 2.0 {
		t.Fatalf("expected drift to converge near -30 ppm, got %f ppm", ppm)
	}

	watermark := ctrl.SmoothedWatermark()
	if math.Abs(watermark-targetWatermark) > 0.5 {
		t.Fatalf("expected smoothed watermark near %f, got %f", targetWatermark, watermark)
	}
}

func TestClockDriftController_HardClamp(t *testing.T) {
	cfg := DefaultClockDriftConfig(3.0)
	ctrl := NewClockDriftController(cfg)

	// Feed extreme buffer depth (e.g. 50 frames)
	for i := 0; i < 1000; i++ {
		ctrl.Update(50.0)
	}

	ppm := ctrl.DriftPPM()
	if ppm > 500.0 {
		t.Fatalf("expected drift clamped to <= 500 ppm, got %f ppm", ppm)
	}
	if ctrl.Ratio() > 1.0005 {
		t.Fatalf("expected ratio clamped to <= 1.0005, got %f", ctrl.Ratio())
	}

	// Feed zero depth (starvation)
	for i := 0; i < 2000; i++ {
		ctrl.Update(0.0)
	}

	ppm = ctrl.DriftPPM()
	if ppm < -500.0 {
		t.Fatalf("expected drift clamped to >= -500 ppm, got %f ppm", ppm)
	}
	if ctrl.Ratio() < 0.9995 {
		t.Fatalf("expected ratio clamped to >= 0.9995, got %f", ctrl.Ratio())
	}
}

func TestClockDriftController_NaNAndInfSanitization(t *testing.T) {
	cfg := DefaultClockDriftConfig(3.0)
	ctrl := NewClockDriftController(cfg)

	initialRatio := ctrl.Ratio()

	// Update with NaN and Inf
	r1 := ctrl.Update(math.NaN())
	if r1 != initialRatio || math.IsNaN(r1) || math.IsInf(r1, 0) {
		t.Fatalf("expected valid ratio on NaN depth, got %f", r1)
	}

	r2 := ctrl.Update(math.Inf(1))
	if r2 != initialRatio || math.IsNaN(r2) || math.IsInf(r2, 0) {
		t.Fatalf("expected valid ratio on +Inf depth, got %f", r2)
	}

	r3 := ctrl.Update(math.Inf(-1))
	if r3 != initialRatio || math.IsNaN(r3) || math.IsInf(r3, 0) {
		t.Fatalf("expected valid ratio on -Inf depth, got %f", r3)
	}

	if math.IsNaN(ctrl.SmoothedWatermark()) || math.IsInf(ctrl.SmoothedWatermark(), 0) {
		t.Fatalf("smoothed watermark corrupted by NaN/Inf: %f", ctrl.SmoothedWatermark())
	}
	if math.IsNaN(ctrl.DriftPPM()) || math.IsInf(ctrl.DriftPPM(), 0) {
		t.Fatalf("drift PPM corrupted by NaN/Inf: %f", ctrl.DriftPPM())
	}
}

