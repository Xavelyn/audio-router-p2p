package telemetry

import (
	"math"
	"testing"
)

func TestArtifactSeverityFor_Boundaries(t *testing.T) {
	cases := []struct {
		score float64
		want  ArtifactSeverity
	}{
		{100, SeverityNone},
		{90, SeverityNone},      // >= 90 boundary
		{89.999, SeverityMinor}, // just below
		{75, SeverityMinor},
		{60, SeverityMinor},     // >= 60 boundary
		{59.999, SeverityMajor}, // just below
		{45, SeverityMajor},
		{30, SeverityMajor},        // >= 30 boundary
		{29.999, SeverityCritical}, // just below
		{0, SeverityCritical},
		{-1, SeverityCritical}, // clamped down
		{150, SeverityNone},    // clamped up
		{math.NaN(), SeverityCritical},
	}
	for _, c := range cases {
		if got := ArtifactSeverityFor(c.score); got != c.want {
			t.Errorf("ArtifactSeverityFor(%v) = %s; want %s", c.score, got, c.want)
		}
	}
}

func TestArtifactCoefficientTables_Boundaries(t *testing.T) {
	type caseRow struct {
		v    uint64
		want float64
	}
	tables := []struct {
		name string
		fn   func(uint64) float64
		rows []caseRow
	}{
		{"C_underrun", cUnderrun, []caseRow{
			{0, 1.00}, {1, 0.80}, {2, 0.80}, {3, 0.60}, {7, 0.60},
			{8, 0.40}, {15, 0.40}, {16, 0.25}, {30, 0.25}, {31, 0.10}, {1000, 0.10},
		}},
		{"C_overrun", cOverrun, []caseRow{
			{0, 1.00}, {1, 0.90}, {2, 0.90}, {3, 0.75}, {7, 0.75}, {8, 0.55}, {15, 0.55}, {16, 0.30}, {1000, 0.30},
		}},
		{"C_ring", cRing, []caseRow{
			{0, 1.00}, {1, 0.85}, {2, 0.65}, {4, 0.65}, {5, 0.45}, {9, 0.45}, {10, 0.25}, {100, 0.25},
		}},
		{"C_xrun", cXrun, []caseRow{
			{0, 1.00}, {1, 0.70}, {2, 0.50}, {3, 0.50}, {4, 0.30}, {7, 0.30}, {8, 0.15}, {100, 0.15},
		}},
		{"C_fecUnrec", cFECUnrec, []caseRow{
			{0, 1.00}, {1, 0.90}, {2, 0.90}, {3, 0.75}, {5, 0.75}, {6, 0.55}, {100, 0.55},
		}},
		{"C_plc", cPLC, []caseRow{
			{0, 1.00}, {9, 1.00}, {10, 0.85}, {49, 0.85}, {50, 0.65}, {249, 0.65}, {250, 0.45}, {10000, 0.45},
		}},
	}
	for _, tbl := range tables {
		for _, row := range tbl.rows {
			if got := tbl.fn(row.v); math.Abs(got-row.want) > 1e-12 {
				t.Errorf("%s(%d) = %v; want %v", tbl.name, row.v, got, row.want)
			}
		}
	}
}

// TestCalculateRouteArtifactScore_MonotonicNonIncreasing verifies the score
// never increases as any single artifact input grows.
func TestCalculateRouteArtifactScore_MonotonicNonIncreasing(t *testing.T) {
	dims := []struct {
		name string
		set  func(v uint64) func() float64
	}{
		{"underruns", func(v uint64) func() float64 {
			return func() float64 { return CalculateRouteArtifactScore(v, 0, 0, 0, 0, 0) }
		}},
		{"overruns", func(v uint64) func() float64 {
			return func() float64 { return CalculateRouteArtifactScore(0, v, 0, 0, 0, 0) }
		}},
		{"ringDrops", func(v uint64) func() float64 {
			return func() float64 { return CalculateRouteArtifactScore(0, 0, v, 0, 0, 0) }
		}},
		{"fecUnrecoverable", func(v uint64) func() float64 {
			return func() float64 { return CalculateRouteArtifactScore(0, 0, 0, v, 0, 0) }
		}},
		{"plcConcealed", func(v uint64) func() float64 {
			return func() float64 { return CalculateRouteArtifactScore(0, 0, 0, 0, 0, v) }
		}},
	}
	for _, dim := range dims {
		prev := 101.0
		for v := uint64(0); v <= 60; v++ {
			got := dim.set(v)()
			if got > prev+1e-9 {
				t.Errorf("%s: score increased from %v to %v at v=%d", dim.name, prev, got, v)
			}
			prev = got
		}
	}

	// Loss ratio dimension (float sweep).
	prev := 101.0
	for i := 0; i <= 100; i++ {
		got := CalculateRouteArtifactScore(0, 0, 0, 0, float64(i)/100.0, 0)
		if got > prev+1e-9 {
			t.Errorf("lossRatio: score increased from %v to %v at ratio=%v", prev, got, float64(i)/100.0)
		}
		prev = got
	}
}

func TestCalculateRouteArtifactScore_AllZero(t *testing.T) {
	if got := CalculateRouteArtifactScore(0, 0, 0, 0, 0, 0); math.Abs(got-100) > 1e-9 {
		t.Errorf("all-zero score = %v; want 100", got)
	}
	if got := ArtifactSeverityFor(100); got != SeverityNone {
		t.Errorf("all-zero severity = %s; want NONE", got)
	}
}

// TestCalculateRouteArtifactScore_ADRSanityCheckpoints verifies the worked
// examples from ADR 0034 §3.
func TestCalculateRouteArtifactScore_ADRSanityCheckpoints(t *testing.T) {
	cases := []struct {
		name                           string
		und, over, ring, fecUnrec, plc uint64
		lossRatio                      float64
		wantScore                      float64
		wantSeverity                   ArtifactSeverity
	}{
		{"clean path", 0, 0, 0, 0, 0, 0, 100, SeverityNone},
		{"one 20ms glitch (1 underrun)", 1, 0, 0, 0, 0, 0, 80, SeverityMinor},
		{"5 underruns + 10 concealed frames", 5, 0, 0, 0, 10, 0, 51, SeverityMajor},
		{"sustained underrun storm (31 events)", 31, 0, 0, 0, 0, 0, 10, SeverityCritical},
	}
	for _, c := range cases {
		got := CalculateRouteArtifactScore(c.und, c.over, c.ring, c.fecUnrec, c.lossRatio, c.plc)
		if math.Abs(got-c.wantScore) > 1e-9 {
			t.Errorf("%s: score = %v; want %v", c.name, got, c.wantScore)
		}
		if gotSev := ArtifactSeverityFor(got); gotSev != c.wantSeverity {
			t.Errorf("%s: severity = %s; want %s", c.name, gotSev, c.wantSeverity)
		}
	}
}

// TestCalculateRouteArtifactScore_LossMinSemantics verifies C_loss =
// min(C_fecUnrec, C_lossRatio) picks the weaker of the two.
func TestCalculateRouteArtifactScore_LossMinSemantics(t *testing.T) {
	// 2% loss: C_lossRatio = 1 - 0.08*(2-0.5) = 0.88; C_fecUnrec(1) = 0.90.
	// min = 0.88 (loss ratio is weaker).
	got := CalculateRouteArtifactScore(0, 0, 0, 1, 0.02, 0)
	want := 100.0 * 0.88
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("loss-ratio-weaker score = %v; want %v", got, want)
	}

	// No loss: C_lossRatio = 1.0; C_fecUnrec(6) = 0.55. min = 0.55.
	got = CalculateRouteArtifactScore(0, 0, 0, 6, 0, 0)
	if math.Abs(got-55) > 1e-9 {
		t.Errorf("fec-weaker score = %v; want 55", got)
	}
}

// TestCalculateRouteArtifactScore_NaNGuard verifies NaN/Inf loss ratios are
// guarded: NaN and +Inf take the worst coefficient (score 0), -Inf clamps to
// no penalty (score 100).
func TestCalculateRouteArtifactScore_NaNGuard(t *testing.T) {
	if got := CalculateRouteArtifactScore(0, 0, 0, 0, math.NaN(), 0); got != 0 {
		t.Errorf("NaN loss ratio score = %v; want 0 (worst)", got)
	}
	if got := CalculateRouteArtifactScore(0, 0, 0, 0, math.Inf(1), 0); got != 0 {
		t.Errorf("+Inf loss ratio score = %v; want 0 (worst)", got)
	}
	if got := CalculateRouteArtifactScore(0, 0, 0, 0, math.Inf(-1), 0); got != 100 {
		t.Errorf("-Inf loss ratio score = %v; want 100 (clamped, no penalty)", got)
	}
	// 100% finite loss also bottoms out the coefficient.
	if got := CalculateRouteArtifactScore(0, 0, 0, 0, 1.0, 0); got != 0 {
		t.Errorf("100%% loss score = %v; want 0", got)
	}
}

// TestCalculateDeviceArtifactScore covers direction handling: a missing
// direction contributes C = 1.0.
func TestCalculateDeviceArtifactScore(t *testing.T) {
	cases := []struct {
		name    string
		xrunIn  uint64
		xrunOut uint64
		hasIn   bool
		hasOut  bool
		want    float64
	}{
		{"both zero", 0, 0, true, true, 100},
		{"input-only device, clean", 0, 0, true, false, 100},
		{"output-only device, clean", 0, 0, false, true, 100},
		{"no directions at all", 5, 5, false, false, 100},
		{"input xrun only", 1, 0, true, false, 70},
		{"output xrun only", 0, 1, false, true, 70},
		{"both directions 1 xrun", 1, 1, true, true, 49},
		{"ADR: 8+ xruns both directions", 8, 8, true, true, 2.25},
	}
	for _, c := range cases {
		got := CalculateDeviceArtifactScore(c.xrunIn, c.xrunOut, c.hasIn, c.hasOut)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: score = %v; want %v", c.name, got, c.want)
		}
	}
	if got := ArtifactSeverityFor(CalculateDeviceArtifactScore(8, 8, true, true)); got != SeverityCritical {
		t.Errorf("8+8 xruns severity = %s; want CRITICAL", got)
	}
}
