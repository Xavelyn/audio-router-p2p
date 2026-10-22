package dsp

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func monoPCM(samples []int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

func stereoPCM(l, r []int16) []byte {
	if len(l) != len(r) {
		panic("stereoPCM: length mismatch")
	}
	b := make([]byte, len(l)*4)
	for i := range l {
		binary.LittleEndian.PutUint16(b[i*4:], uint16(l[i]))
		binary.LittleEndian.PutUint16(b[i*4+2:], uint16(r[i]))
	}
	return b
}

func TestProbeGeneratorProducesSine(t *testing.T) {
	const rate = 48000
	tone := ProbeTone{FreqHz: 4200, Amplitude: 16000, Duration: 100 * time.Millisecond}
	gen := NewProbeGenerator(rate, tone, nil)

	const frame = 1600
	wantTotal := int(tone.Duration.Seconds() * float64(rate)) // 4800
	if wantTotal%frame != 0 {
		t.Fatalf("test setup: %d not a multiple of %d", wantTotal, frame)
	}

	emitted := 0
	startedSeen := false
	doneSeen := false
	for f := 0; f < wantTotal/frame+1; f++ {
		buf := make([]byte, frame*2)
		started, done := gen.Process(buf, 1)
		if started {
			startedSeen = true
		}
		for i := 0; i < frame; i++ {
			if emitted < wantTotal {
				want := int16(float64(tone.Amplitude) * math.Sin(2*math.Pi*tone.FreqHz*float64(emitted)/float64(rate)))
				got := int16(binary.LittleEndian.Uint16(buf[i*2:]))
				if d := int(got) - int(want); d > 1 || d < -1 {
					t.Fatalf("sample %d: got %d, want %d", emitted, got, want)
				}
				emitted++
			} else {
				if v := int16(binary.LittleEndian.Uint16(buf[i*2:])); v != 0 {
					t.Fatalf("sample %d after burst: got %d, want 0", i, v)
				}
			}
		}
		if done {
			doneSeen = true
			break
		}
	}
	if !startedSeen {
		t.Error("generator never reported started")
	}
	if !doneSeen {
		t.Error("generator never reported done")
	}
	// After completion Process is inert.
	buf := make([]byte, frame*2)
	for i := range buf {
		buf[i] = 0x11
	}
	if started, done := gen.Process(buf, 1); started || done {
		t.Errorf("completed generator: started=%v done=%v", started, done)
	}
	if buf[0] != 0x11 {
		t.Error("completed generator modified the frame")
	}
}

func TestProbeDetectorMeasuresDelay(t *testing.T) {
	const rate = 48000
	const delay = 3123 // 65.0625 ms, deliberately not a frame multiple
	const frame = 960

	tone := ProbeTone{FreqHz: 4200, Amplitude: 16000, Duration: 100 * time.Millisecond}
	toneLen := int(tone.Duration.Seconds() * float64(rate))
	t0 := time.Now()

	// Simulated pipeline: silence until the tone onset, then the tone
	// (sample n plays at t0 + (n+delay)/rate).
	total := delay + toneLen + frame
	out := make([]int16, total)
	for n := delay; n < delay+toneLen; n++ {
		phase := 2 * math.Pi * tone.FreqHz * float64(n-delay) / float64(rate)
		out[n] = int16(float64(tone.Amplitude) * math.Sin(phase))
	}

	det := NewProbeDetector(rate, tone, t0, 2*time.Second, nil)
	var hits []ProbeHit
	done := false
	for m := 0; m+frame <= total; m += frame {
		// Output position m plays at t0 + m/rate, independent of the delay
		// (the delay is which source sample sits at each position).
		fStart := t0.Add(time.Duration(float64(m) * float64(time.Second) / float64(rate)))
		var fh []ProbeHit
		fh, done = det.Process(monoPCM(out[m:m+frame]), 1, fStart)
		hits = append(hits, fh...)
		if done {
			break
		}
	}
	if !done {
		t.Fatalf("detector never completed, hits so far: %+v", hits)
	}
	if len(hits) != 1 || hits[0].Channel != 0 {
		t.Fatalf("expected exactly one mono hit, got %+v", hits)
	}
	wantMS := float64(delay) / float64(rate) * 1000
	if d := hits[0].DelayMS - wantMS; d > 0.05 || d < -0.05 {
		t.Errorf("measured delay %f ms, want %f ms (±0.05)", hits[0].DelayMS, wantMS)
	}
}

func TestProbeDetectorPerChannelDelays(t *testing.T) {
	const rate = 48000
	const delayL = 3000 // 62.5 ms
	const delayR = 3500 // 72.9167 ms
	const frame = 960

	tone := ProbeTone{FreqHz: 4200, Amplitude: 16000, Duration: 100 * time.Millisecond}
	toneLen := int(tone.Duration.Seconds() * float64(rate))
	t0 := time.Now()

	total := delayR + toneLen + frame
	l := make([]int16, total)
	r := make([]int16, total)
	for n := 0; n < toneLen; n++ {
		v := int16(float64(tone.Amplitude) * math.Sin(2*math.Pi*tone.FreqHz*float64(n)/float64(rate)))
		l[delayL+n] = v
		r[delayR+n] = v
	}

	det := NewProbeDetector(rate, tone, t0, 2*time.Second, nil)
	byCh := map[int]float64{}
	done := false
	for m := 0; m+frame <= total; m += frame {
		// Both channels play on the same wall clock: position m plays at
		// t0 + m/rate; each channel's delay is which source sample sits at
		// each position.
		fStart := t0.Add(time.Duration(float64(m) * float64(time.Second) / float64(rate)))
		hits, d := det.Process(stereoPCM(l[m:m+frame], r[m:m+frame]), 2, fStart)
		for _, h := range hits {
			byCh[h.Channel] = h.DelayMS
		}
		done = d
		if done {
			break
		}
	}
	if !done {
		t.Fatalf("detector never completed: %+v", byCh)
	}
	for ch, delay := range map[int]int{0: delayL, 1: delayR} {
		wantMS := float64(delay) / float64(rate) * 1000
		got, ok := byCh[ch]
		if !ok {
			t.Fatalf("no hit for channel %d: %+v", ch, byCh)
		}
		if d := got - wantMS; d > 0.05 || d < -0.05 {
			t.Errorf("channel %d: measured %f ms, want %f ms", ch, got, wantMS)
		}
	}
}

func TestProbeDetectorIgnoresSilence(t *testing.T) {
	const rate = 48000
	const frame = 960
	tone := DefaultProbeTone()
	t0 := time.Now()

	det := NewProbeDetector(rate, tone, t0, 2*time.Second, nil)
	buf := make([]byte, frame*2)
	for i := 0; i < 50; i++ {
		hits, done := det.Process(buf, 2, t0.Add(time.Duration(i)*time.Duration(float64(frame)*float64(time.Second)/float64(rate))))
		if len(hits) > 0 {
			t.Fatalf("false detection in silence: %+v", hits)
		}
		if done {
			t.Fatal("silence must not complete detection")
		}
	}
}
