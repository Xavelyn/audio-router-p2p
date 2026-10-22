package dsp

import (
	"encoding/binary"
	"testing"
)

func patternSamples(n int, seed int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16((i*7 + seed) % 1000)
	}
	return out
}

func TestDelayLineExactDelay(t *testing.T) {
	const rate = 48000
	const channels = 2
	const total = 6000
	dl := NewDelayLine(rate, channels, rate)

	var delays []int
	if err := dl.SetDelaysSamples([]int{960, 2880}); err != nil { // 20 ms, 60 ms
		t.Fatal(err)
	}
	delays = dl.DelaysSamples()
	if delays[0] != 960 || delays[1] != 2880 {
		t.Fatalf("delays not set: %v", delays)
	}

	in := monoPCM(patternSamples(total, 0))
	// Build interleaved input from two pattern streams.
	inL := patternSamples(total, 0)
	inR := patternSamples(total, 5)
	inI := stereoPCM(inL, inR)
	_ = in
	out := make([]byte, len(inI))
	if _, err := dl.Process(inI, out); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < total; i++ {
		for c, delay := range []int{960, 2880} {
			got := int16(binary.LittleEndian.Uint16(out[(i*channels+c)*2:]))
			if i < delay {
				if got != 0 {
					t.Fatalf("warm-up sample %d ch %d: got %d, want 0", i, c, got)
				}
			} else if want := int16(binary.LittleEndian.Uint16(inI[((i-delay)*channels+c)*2:])); got != want {
				t.Fatalf("sample %d ch %d: got %d, want %d", i, c, got, want)
			}
		}
	}
}

func TestDelayLineLiveChange(t *testing.T) {
	const rate = 48000
	const channels = 1
	dl := NewDelayLine(rate, channels, rate)
	if err := dl.SetDelaysSamples([]int{100}); err != nil {
		t.Fatal(err)
	}

	const total = 4800
	inL := patternSamples(total, 3)
	in := monoPCM(inL)
	out := make([]byte, len(in))
	frame := 960

	// First segment with a 100-sample delay.
	if _, err := dl.Process(in[:frame*2], out[:frame*2]); err != nil {
		t.Fatal(err)
	}
	if err := dl.SetDelaysSamples([]int{300}); err != nil {
		t.Fatal(err)
	}
	// Second segment with a 300-sample delay.
	if _, err := dl.Process(in[frame*2:], out[frame*2:]); err != nil {
		t.Fatal(err)
	}
	// After the second segment the whole tail must reflect the 300-sample
	// delay against the original input.
	for i := total - 1000; i < total; i++ {
		got := int16(binary.LittleEndian.Uint16(out[(i*channels)*2:]))
		want := inL[i-300]
		if got != want {
			t.Fatalf("sample %d: got %d, want %d", i, got, want)
		}
	}
}

func TestDelayLineValidationAndClamp(t *testing.T) {
	dl := NewDelayLine(48000, 2, 1000)
	if err := dl.SetDelaysSamples([]int{500}); err == nil {
		t.Fatal("wrong-length delays must be rejected")
	}
	if err := dl.SetDelaysSamples([]int{-5, 5000}); err != nil {
		t.Fatal(err)
	}
	d := dl.DelaysSamples()
	if d[0] != 0 || d[1] != 1000 {
		t.Fatalf("delays not clamped: %v", d)
	}
}
