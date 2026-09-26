package router

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestChannelLayout(t *testing.T) {
	tests := []struct {
		layout   ChannelLayout
		str      string
		expected int
	}{
		{LayoutMono, "mono", 1},
		{LayoutStereo, "stereo", 2},
		{LayoutQuad, "quad", 4},
		{Layout51, "5.1", 6},
		{Layout71, "7.1", 8},
		{LayoutCustom, "unknown", 0},
	}

	for _, tt := range tests {
		if got := tt.layout.Channels(); got != tt.expected {
			t.Errorf("Channels() for %s = %d; want %d", tt.layout, got, tt.expected)
		}
		if got := ParseChannelLayout(tt.str); got != tt.layout {
			t.Errorf("ParseChannelLayout(%q) = %s; want %s", tt.str, got, tt.layout)
		}
	}
}

func TestClampInt16(t *testing.T) {
	cases := []struct {
		in   float32
		want int16
	}{
		{0.0, 0},
		{100.4, 100},
		{100.5, 101},
		{-100.4, -100},
		{-100.5, -101},
		{32767.0, 32767},
		{32767.4, 32767},
		{40000.0, 32767},
		{1e6, 32767},
		{-32768.0, -32768},
		{-32768.4, -32768},
		{-50000.0, -32768},
		{-1e6, -32768},
	}

	for _, c := range cases {
		got := ClampInt16(c.in)
		if got != c.want {
			t.Errorf("ClampInt16(%f) = %d; want %d", c.in, got, c.want)
		}
	}
}

func TestGainMatrix_Validation(t *testing.T) {
	// Valid identity
	id := NewIdentityGainMatrix(2)
	if err := id.Validate(); err != nil {
		t.Fatalf("identity matrix validation failed: %v", err)
	}
	if id.At(0, 0) != 1.0 || id.At(1, 1) != 1.0 || id.At(0, 1) != 0.0 {
		t.Errorf("unexpected identity matrix coefficients: %v", id.Coefficients)
	}

	// Downmix tests
	dm := NewDefaultDownmixMatrix(2, 1)
	if err := dm.Validate(); err != nil {
		t.Fatalf("stereo-to-mono downmix validation failed: %v", err)
	}
	if dm.At(0, 0) != 0.5 || dm.At(0, 1) != 0.5 {
		t.Errorf("unexpected stereo-to-mono weights: %v", dm.Coefficients)
	}

	// Invalid cases
	invalidCases := []struct {
		name   string
		inCh   int
		outCh  int
		coeffs []float64
	}{
		{"zero in_channels", 0, 2, []float64{1, 1}},
		{"zero out_channels", 2, 0, []float64{1, 1}},
		{"length mismatch", 2, 2, []float64{1, 0, 0}},
		{"negative coeff", 2, 1, []float64{-0.1, 1.0}},
		{"overflow coeff > 2.0", 2, 1, []float64{2.01, 1.0}},
		{"NaN coeff", 1, 1, []float64{math.NaN()}},
		{"Inf coeff", 1, 1, []float64{math.Inf(1)}},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			gm := &GainMatrix{
				InChannels:   tc.inCh,
				OutChannels:  tc.outCh,
				Coefficients: tc.coeffs,
			}
			if err := gm.Validate(); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestMatrixRoute_Validation(t *testing.T) {
	valid := MatrixRoute{
		ID:          "route-master",
		Name:        "Master Out",
		Source:      MatrixEndpoint{NodeID: "local", DeviceID: "0", Channels: []int{0, 1}},
		Destination: MatrixEndpoint{NodeID: "local", DeviceID: "1", Channels: []int{0, 1}},
		InChannels:  2,
		OutChannels: 2,
		Coefficients: []float64{
			1.0, 0.0,
			0.0, 1.0,
		},
		SlewMS: 20.0,
		State:  "active",
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("valid route failed validation: %v", err)
	}

	// Missing ID
	inv := valid
	inv.ID = ""
	if err := inv.Validate(); err == nil {
		t.Error("expected error for empty ID")
	}

	// Negative SlewMS
	inv = valid
	inv.SlewMS = -1.0
	if err := inv.Validate(); err == nil {
		t.Error("expected error for negative slew_ms")
	}

	// Mismatched coefficients
	inv = valid
	inv.Coefficients = []float64{1.0}
	if err := inv.Validate(); err == nil {
		t.Error("expected error for mismatched coefficients length")
	}
}

func TestGainMatrix_Evaluate_ZeroAlloc(t *testing.T) {
	gm, err := NewGainMatrix(2, 2, []float64{0.7071, 0.7071, 0.5, 0.5})
	if err != nil {
		t.Fatalf("NewGainMatrix failed: %v", err)
	}

	numFrames := 480 // 10ms @ 48kHz
	inPCM := make([]byte, numFrames*2*2)
	outPCM := make([]byte, numFrames*2*2)

	// Fill inPCM with non-zero test samples
	for i := 0; i < numFrames*2; i++ {
		binary.LittleEndian.PutUint16(inPCM[i*2:(i+1)*2], uint16(1000))
	}

	// Run once to warm
	n, err := gm.Evaluate(inPCM, outPCM)
	if err != nil || n != len(outPCM) {
		t.Fatalf("Evaluate failed: n=%d, err=%v", n, err)
	}

	// Verify zero allocations in steady state
	allocs := testing.AllocsPerRun(1000, func() {
		_, _ = gm.Evaluate(inPCM, outPCM)
	})
	if allocs != 0 {
		t.Errorf("expected 0 allocs/op during Evaluate, got %f", allocs)
	}
}

func TestSlewGainMatrix_LinearSlew_960Samples(t *testing.T) {
	// 1-in 1-out matrix starting at 0.0 gain, targeting 1.0 gain over 960 samples @ 48kHz (20ms)
	sgm, err := NewSlewGainMatrix(1, 1, []float32{0.0})
	if err != nil {
		t.Fatalf("NewSlewGainMatrix failed: %v", err)
	}

	slewSamples := 960
	if err := sgm.SetTarget([]float32{1.0}, slewSamples); err != nil {
		t.Fatalf("SetTarget failed: %v", err)
	}

	if !sgm.IsSlewing() {
		t.Fatal("expected IsSlewing() to be true")
	}

	// Feed 960 samples with DC amplitude of 10000
	inPCM := make([]byte, slewSamples*2)
	outPCM := make([]byte, slewSamples*2)
	const testAmp = 10000
	for i := 0; i < slewSamples; i++ {
		binary.LittleEndian.PutUint16(inPCM[i*2:(i+1)*2], uint16(testAmp))
	}

	n, err := sgm.Process(inPCM, outPCM)
	if err != nil || n != len(outPCM) {
		t.Fatalf("Process failed: n=%d, err=%v", n, err)
	}

	// Verify linear ramp without clicks
	var prevAmp int16 = -1
	for i := 0; i < slewSamples; i++ {
		samp := int16(binary.LittleEndian.Uint16(outPCM[i*2 : (i+1)*2]))
		if samp < prevAmp {
			t.Fatalf("sample at index %d (%d) is not monotonically increasing from %d", i, samp, prevAmp)
		}
		prevAmp = samp
	}

	// At sample index 959 (sample 960), it should be approximately testAmp
	lastSample := int16(binary.LittleEndian.Uint16(outPCM[(slewSamples-1)*2 : slewSamples*2]))
	if math.Abs(float64(lastSample-testAmp)) > 1.0 {
		t.Errorf("last slewed sample = %d; want %d", lastSample, testAmp)
	}

	// State should now have transitioned from slewing to steady state
	if sgm.IsSlewing() {
		t.Errorf("expected IsSlewing() to be false after 960 samples")
	}

	// Current coefficients should have snapped to target
	coeffs := sgm.CurrentCoefficients()
	if coeffs[0] != 1.0 {
		t.Errorf("expected coefficient to snap to 1.0, got %f", coeffs[0])
	}
}

func TestSlewGainMatrix_MultiBlock(t *testing.T) {
	// 20ms slew (960 samples) processed across two 480-sample blocks
	sgm, err := NewSlewGainMatrix(1, 1, []float32{1.0})
	if err != nil {
		t.Fatalf("NewSlewGainMatrix: %v", err)
	}

	// Mute: slew from 1.0 to 0.0 over 960 samples
	if err := sgm.SetTarget([]float32{0.0}, 960); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	blockSamples := 480
	inPCM := make([]byte, blockSamples*2)
	outPCM1 := make([]byte, blockSamples*2)
	outPCM2 := make([]byte, blockSamples*2)
	for i := 0; i < blockSamples; i++ {
		binary.LittleEndian.PutUint16(inPCM[i*2:(i+1)*2], uint16(10000))
	}

	// Block 1: samples 0..479
	if _, err := sgm.Process(inPCM, outPCM1); err != nil {
		t.Fatalf("block 1 process: %v", err)
	}
	if !sgm.IsSlewing() {
		t.Fatal("expected still slewing after block 1")
	}

	// Block 2: samples 480..959
	if _, err := sgm.Process(inPCM, outPCM2); err != nil {
		t.Fatalf("block 2 process: %v", err)
	}
	if sgm.IsSlewing() {
		t.Fatal("expected slewing finished after block 2")
	}

	// End of block 2 should reach 0 (silence)
	lastSamp := int16(binary.LittleEndian.Uint16(outPCM2[(blockSamples-1)*2 : blockSamples*2]))
	if lastSamp != 0 {
		t.Errorf("last sample after mute slew = %d; want 0", lastSamp)
	}
}

func TestSlewGainMatrix_ZeroAlloc(t *testing.T) {
	sgm, err := NewSlewGainMatrix(2, 2, []float32{1.0, 0.0, 0.0, 1.0})
	if err != nil {
		t.Fatalf("NewSlewGainMatrix: %v", err)
	}

	numFrames := 480
	inPCM := make([]byte, numFrames*2*2)
	outPCM := make([]byte, numFrames*2*2)

	// Warm up
	_, _ = sgm.Process(inPCM, outPCM)

	allocs := testing.AllocsPerRun(1000, func() {
		_, _ = sgm.Process(inPCM, outPCM)
	})
	if allocs != 0 {
		t.Errorf("expected 0 allocs/op during steady-state Process, got %f", allocs)
	}

	// Now check during active slewing
	_ = sgm.SetTarget([]float32{0.5, 0.5, 0.5, 0.5}, 1000000)
	allocsSlewing := testing.AllocsPerRun(1000, func() {
		_, _ = sgm.Process(inPCM, outPCM)
	})
	if allocsSlewing != 0 {
		t.Errorf("expected 0 allocs/op during slewing Process, got %f", allocsSlewing)
	}
}

func TestSlewGainMatrix_ProceduralHelper(t *testing.T) {
	inPCM := make([]byte, 480*2*2)
	outPCM := make([]byte, 480*2*2)
	for i := 0; i < 480*2; i++ {
		binary.LittleEndian.PutUint16(inPCM[i*2:(i+1)*2], uint16(5000))
	}
	curr := []float32{1.0, 0.0, 0.0, 1.0}
	target := []float32{0.0, 1.0, 1.0, 0.0}

	n, err := ApplySlewGainMatrix(curr, target, 480, inPCM, outPCM, 2, 2)
	if err != nil || n != len(outPCM) {
		t.Fatalf("SlewGainMatrix helper failed: n=%d, err=%v", n, err)
	}
}

func TestFormatConversion(t *testing.T) {
	fc := FormatConversion{
		InSampleRate:  44100,
		OutSampleRate: 48000,
		InChannels:    1,
		OutChannels:   2,
		InLayout:      LayoutMono,
		OutLayout:     LayoutStereo,
	}

	if err := fc.Validate(); err != nil {
		t.Fatalf("fc.Validate() failed: %v", err)
	}
	if !fc.NeedsResampling() {
		t.Error("expected NeedsResampling to be true")
	}
	if !fc.NeedsRemixing() {
		t.Error("expected NeedsRemixing to be true")
	}

	fcSame := FormatConversion{
		InSampleRate:  48000,
		OutSampleRate: 48000,
		InChannels:    2,
		OutChannels:   2,
	}
	if fcSame.NeedsResampling() {
		t.Error("expected NeedsResampling to be false")
	}
	if fcSame.NeedsRemixing() {
		t.Error("expected NeedsRemixing to be false")
	}

	// Invalid cases
	invalidFC := FormatConversion{InSampleRate: 0, OutSampleRate: 48000, InChannels: 2, OutChannels: 2}
	if err := invalidFC.Validate(); err == nil {
		t.Error("expected error for zero in_sample_rate")
	}
	invalidFC2 := FormatConversion{InSampleRate: 48000, OutSampleRate: 48000, InChannels: 0, OutChannels: 2}
	if err := invalidFC2.Validate(); err == nil {
		t.Error("expected error for zero in_channels")
	}
}

func TestSlewGainMatrix_SetCrosspoint(t *testing.T) {
	// 2x2 identity matrix, 48kHz sample rate
	sgm, err := NewSlewGainMatrix(2, 2, []float32{1.0, 0.0, 0.0, 1.0})
	if err != nil {
		t.Fatalf("NewSlewGainMatrix failed: %v", err)
	}

	// Set crosspoint (out=0, in=1) from 0.0 to 1.0 with 20ms slew (960 samples @ 48kHz)
	if err := sgm.SetCrosspoint(0, 1, 1.0); err != nil {
		t.Fatalf("SetCrosspoint failed: %v", err)
	}
	if !sgm.IsSlewing() {
		t.Fatal("expected IsSlewing() to be true")
	}

	// Total slew is 960 samples
	slewSamples := 960
	inPCM := make([]byte, slewSamples*2*2)
	outPCM := make([]byte, slewSamples*2*2)

	// Feed in[0] = 0, in[1] = 10000
	for i := 0; i < slewSamples; i++ {
		// in[0] = 0
		binary.LittleEndian.PutUint16(inPCM[i*4:i*4+2], 0)
		// in[1] = 10000
		binary.LittleEndian.PutUint16(inPCM[i*4+2:i*4+4], 10000)
	}

	n, err := sgm.Process(inPCM, outPCM)
	if err != nil || n != len(outPCM) {
		t.Fatalf("Process failed: %v", err)
	}

	if sgm.IsSlewing() {
		t.Error("expected IsSlewing() to be false after 960 samples")
	}

	// out[0] should have ramped from 0 up to 10000
	var prevOut0 int16 = -1
	for i := 0; i < slewSamples; i++ {
		samp0 := int16(binary.LittleEndian.Uint16(outPCM[i*4 : i*4+2]))
		if samp0 < prevOut0 {
			t.Fatalf("out[0] at %d (%d) is not monotonically increasing from %d", i, samp0, prevOut0)
		}
		prevOut0 = samp0
	}

	lastSamp0 := int16(binary.LittleEndian.Uint16(outPCM[(slewSamples-1)*4 : (slewSamples-1)*4+2]))
	if math.Abs(float64(lastSamp0-10000)) > 1.0 {
		t.Errorf("last slewed sample = %d; want 10000", lastSamp0)
	}

	// Test bounds validation
	if err := sgm.SetCrosspoint(2, 0, 1.0); err == nil {
		t.Error("expected error for outCh >= outChannels")
	}
	if err := sgm.SetCrosspoint(0, 2, 1.0); err == nil {
		t.Error("expected error for inCh >= inChannels")
	}
	if err := sgm.SetCrosspoint(0, 0, 2.5); err == nil {
		t.Error("expected error for gain > 2.0")
	}
}

func TestSlewGainMatrix_SetLiveGain(t *testing.T) {
	sgm, err := NewSlewGainMatrix(2, 2, []float32{1.0, 0.0, 0.0, 1.0})
	if err != nil {
		t.Fatalf("NewSlewGainMatrix failed: %v", err)
	}

	newMatrix, err := NewGainMatrix(2, 2, []float64{0.5, 0.5, 0.5, 0.5})
	if err != nil {
		t.Fatalf("NewGainMatrix failed: %v", err)
	}

	// Slew over 10ms (480 samples @ 48kHz)
	if err := sgm.SetLiveGain(*newMatrix, 10); err != nil {
		t.Fatalf("SetLiveGain failed: %v", err)
	}
	if !sgm.IsSlewing() {
		t.Fatal("expected IsSlewing() to be true")
	}

	blockSamples := 480
	inPCM := make([]byte, blockSamples*2*2)
	outPCM := make([]byte, blockSamples*2*2)
	for i := 0; i < blockSamples; i++ {
		binary.LittleEndian.PutUint16(inPCM[i*4:i*4+2], 10000)
		binary.LittleEndian.PutUint16(inPCM[i*4+2:i*4+4], 10000)
	}

	if _, err := sgm.Process(inPCM, outPCM); err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if sgm.IsSlewing() {
		t.Fatal("expected slewing finished after 480 samples")
	}

	// At end of 10ms, out[0] = 0.5*10000 + 0.5*10000 = 10000
	lastSamp := int16(binary.LittleEndian.Uint16(outPCM[(blockSamples-1)*4 : (blockSamples-1)*4+2]))
	if math.Abs(float64(lastSamp-10000)) > 2.0 {
		t.Errorf("lastSamp = %d; want 10000", lastSamp)
	}

	// Mismatched dimensions error
	badMatrix, _ := NewGainMatrix(1, 1, []float64{1.0})
	if err := sgm.SetLiveGain(*badMatrix, 10); err == nil {
		t.Error("expected error for dimension mismatch")
	}
}

func TestMatrixRoute_DynamicModification(t *testing.T) {
	mr := MatrixRoute{
		ID:           "test-dyn-route",
		InChannels:   2,
		OutChannels:  2,
		Coefficients: []float64{1.0, 0.0, 0.0, 1.0},
		SlewMS:       0,
		State:        "active",
	}

	if err := mr.SetCrosspoint(0, 1, 0.7071); err != nil {
		t.Fatalf("mr.SetCrosspoint failed: %v", err)
	}
	if mr.Coefficients[1] != float64(float32(0.7071)) {
		t.Errorf("crosspoint coefficient = %f; want 0.7071", mr.Coefficients[1])
	}
	if mr.State != "slewing" || mr.SlewMS != 20.0 {
		t.Errorf("unexpected state/slew: state=%s, slew=%f", mr.State, mr.SlewMS)
	}

	gm, _ := NewGainMatrix(2, 2, []float64{0.5, 0.5, 0.5, 0.5})
	if err := mr.SetLiveGain(*gm, 30); err != nil {
		t.Fatalf("mr.SetLiveGain failed: %v", err)
	}
	if mr.SlewMS != 30.0 || mr.Coefficients[0] != 0.5 {
		t.Errorf("SetLiveGain result unexpected: %+v", mr)
	}
}
