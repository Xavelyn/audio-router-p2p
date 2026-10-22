package dsp

import (
	"encoding/binary"
	"math"
	"time"
)

// ProbeTone describes a calibration probe: a sine burst of known frequency,
// amplitude, and length injected into a route's input. The 4.2 kHz default
// sits outside the speech band, its amplitude is well above the DTX silence
// threshold but far from clipping, and 600 ms gives the detector several
// hundred cycles to lock.
type ProbeTone struct {
	FreqHz    float64
	Amplitude int16
	Duration  time.Duration
}

// DefaultProbeTone returns the standard calibration probe.
func DefaultProbeTone() ProbeTone {
	return ProbeTone{FreqHz: 4200, Amplitude: 16000, Duration: 600 * time.Millisecond}
}

// ProbeGenerator adds a probe tone to incoming PCM frames, starting at phase
// zero on the first sample it processes. The caller stamps the wall-clock
// time of the first Process call (started == true) as the probe's origin, so
// the detector can match the tone against an absolute phase law.
type ProbeGenerator struct {
	rate      int
	tone      ProbeTone
	selected  []int
	cacheMask []bool
	samplesIn int
	left      int
}

// NewProbeGenerator creates a generator for the given sample rate and tone.
// channels selects which channels carry the tone; nil selects all of them.
func NewProbeGenerator(sampleRate int, tone ProbeTone, channels []int) *ProbeGenerator {
	g := &ProbeGenerator{
		rate: sampleRate,
		tone: tone,
		left: int(tone.Duration.Seconds() * float64(sampleRate)),
	}
	for _, c := range channels {
		if c >= 0 {
			g.selected = append(g.selected, c)
		}
	}
	return g
}

// Process adds the tone to the selected channels of pcm (little-endian
// int16, in place). It returns started=true on the first call and done=true
// once the full burst has been emitted.
func (g *ProbeGenerator) Process(pcm []byte, channels int) (started, done bool) {
	if g.left <= 0 {
		return false, false
	}
	started = g.samplesIn == 0
	samples := len(pcm) / (2 * channels)
	if samples == 0 {
		return started, false
	}
	if g.cacheMask == nil || len(g.cacheMask) != channels {
		g.cacheMask = make([]bool, channels)
		if g.selected == nil {
			for c := range g.cacheMask {
				g.cacheMask[c] = true
			}
		} else {
			for _, c := range g.selected {
				if c < channels {
					g.cacheMask[c] = true
				}
			}
		}
	}
	m := g.cacheMask
	inc := 2 * math.Pi * g.tone.FreqHz / float64(g.rate)
	var ci, si float64
	ci, si = math.Cos(0), math.Sin(0)
	cosInc, sinInc := math.Cos(inc), math.Sin(inc)
	for i := 0; i < samples && g.left > 0; i++ {
		amp := float64(g.tone.Amplitude) * si
		for c, on := range m {
			if !on {
				continue
			}
			off := (i*channels + c) * 2
			v := int(int16(binary.LittleEndian.Uint16(pcm[off:off+2]))) + int(amp)
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(pcm[off:off+2], uint16(int16(v)))
		}
		ci, si = ci*cosInc-si*sinInc, si*cosInc+ci*sinInc
		g.samplesIn++
		g.left--
	}
	return started, g.left <= 0
}

// ProbeHit reports one channel's measured delay: the time between the probe
// origin and the playout of the first tone sample on that channel.
type ProbeHit struct {
	Channel int
	DelayMS float64
}

// detectThreshold is the normalized cross-correlation required to accept a
// detection. The probe is a several-hundred-cycle burst, so the peak at the
// true delay is near 1.0 in quiet conditions; lower signal-to-noise mixes
// (speech under the probe) reduce it. A one-sample misalignment scores at
// most ~0.85, so the argmax stays sample-accurate above this gate.
const detectThreshold = 0.8

// minLockSamples is the minimum tone samples present in a frame for its
// boundary (onset or offset) candidates to be trusted.
const minLockSamples = 160

// ProbeDetector finds an injected probe tone in a stream of playout frames
// and measures, per channel, the delay between the probe's origin t0 and the
// tone's arrival, with sample accuracy. It is calibrated for a single
// daemon: t0 and the frame timestamps must come from the same clock.
type ProbeDetector struct {
	rate     int
	tone     ProbeTone
	t0       time.Time
	maxDelay time.Duration
	toneLen  int
	channels int
	chanMask []bool
	done     []bool
}

// NewProbeDetector creates a detector for a probe that starts (on the
// sender) at t0. maxDelay bounds the search window. channels selects which
// channels are detected; nil selects all.
func NewProbeDetector(sampleRate int, tone ProbeTone, t0 time.Time, maxDelay time.Duration, channels []int) *ProbeDetector {
	if maxDelay <= 0 {
		maxDelay = 2 * time.Second
	}
	d := &ProbeDetector{
		rate:     sampleRate,
		tone:     tone,
		t0:       t0,
		maxDelay: maxDelay,
		toneLen:  int(tone.Duration.Seconds() * float64(sampleRate)),
	}
	if channels == nil {
		return d
	}
	d.channels = len(channels)
	d.chanMask = make([]bool, len(channels))
	d.done = make([]bool, len(channels))
	for _, c := range channels {
		if c >= 0 && c < len(channels) {
			d.chanMask[c] = true
			d.done[c] = false
		} else {
			d.done[c] = true
		}
	}
	return d
}

func (d *ProbeDetector) eachChannel(channels int, fn func(c int)) {
	if d.chanMask == nil {
		for c := 0; c < channels; c++ {
			fn(c)
		}
		return
	}
	for c, on := range d.chanMask {
		if on && c < channels {
			fn(c)
		}
	}
}

func (d *ProbeDetector) allDone(channels int) bool {
	if d.chanMask == nil {
		// All channels must be seen; track them lazily.
		if len(d.done) != channels {
			d.done = make([]bool, channels)
		}
		for _, ok := range d.done {
			if !ok {
				return false
			}
		}
		return true
	}
	for c, on := range d.chanMask {
		if on && !d.done[c] {
			return false
		}
	}
	return true
}

// Process examines one playout frame. frameStart is the wall-clock time the
// frame begins playing. It returns the channel hits found in this frame and
// done=true once every selected channel has been detected.
//
// The search is boundary-anchored: only frames containing the tone's onset
// or offset are evaluated, which both keeps the cost tiny and resolves the
// period ambiguity of a bare sine (the window edge is unique).
func (d *ProbeDetector) Process(pcm []byte, channels int, frameStart time.Time) ([]ProbeHit, bool) {
	if d.allDone(channels) {
		return nil, true
	}
	samples := len(pcm) / (2 * channels)
	if samples == 0 {
		return nil, d.allDone(channels)
	}
	// Frame start relative to the probe origin, in samples.
	rel := frameStart.Sub(d.t0).Seconds() * float64(d.rate)
	maxDelayS := d.maxDelay.Seconds() * float64(d.rate)
	// Per-channel frame energy: it gates the candidate search on silence
	// frames, and it is the NCC denominator, so the tone's silent flanks are
	// what anchors a candidate to the true boundary (a window aligned mid-tone
	// cannot explain the energy outside it).
	energy := make([]float64, channels)
	for c := 0; c < channels; c++ {
		for i := 0; i < samples; i++ {
			off := (i*channels + c) * 2
			x := float64(int16(binary.LittleEndian.Uint16(pcm[off : off+2])))
			energy[c] += x * x
		}
	}
	hits := []ProbeHit{}
	d.eachChannel(channels, func(c int) {
		if d.done[c] || energy[c] <= 0 {
			return
		}
		bestNCC, bestW := -1.0, 0
		// Onset candidates: the tone starts at frame sample w.
		for w := 0; w+minLockSamples <= samples; w++ {
			delay := rel + float64(w)
			if delay < 0 || delay > maxDelayS {
				continue
			}
			if ncc := d.correlate(pcm, channels, c, w, energy[c]); ncc > bestNCC {
				bestNCC, bestW = ncc, w
			}
		}
		// Offset candidates: the tone started before this frame and ends in
		// it (window start w < 0, window end w+toneLen inside the frame).
		wStart := minLockSamples - d.toneLen
		wEnd := samples - d.toneLen
		if wEnd > 0 {
			wEnd = 0
		}
		for w := wStart; w < wEnd; w++ {
			delay := rel + float64(w)
			if delay < 0 || delay > maxDelayS {
				continue
			}
			if ncc := d.correlate(pcm, channels, c, w, energy[c]); ncc > bestNCC {
				bestNCC, bestW = ncc, w
			}
		}
		if bestNCC >= detectThreshold {
			d.done[c] = true
			hits = append(hits, ProbeHit{Channel: c, DelayMS: (rel + float64(bestW)) / float64(d.rate) * 1000})
		}
	})
	return hits, d.allDone(channels)
}

// correlate is the normalized cross-correlation of channel c's frame against
// the probe reference, whose window occupies frame positions [w, w+toneLen)
// with phase 0 at w (w may be negative: the tone started before the frame).
// frameEnergy is the channel's energy over the whole frame, so the window
// must explain all of it to score near 1.0.
func (d *ProbeDetector) correlate(pcm []byte, channels, c, w int, frameEnergy float64) float64 {
	if frameEnergy <= 0 {
		return 0
	}
	samples := len(pcm) / (2 * channels)
	i0 := w
	if i0 < 0 {
		i0 = 0
	}
	i1 := w + d.toneLen
	if i1 > samples {
		i1 = samples
	}
	l := i1 - i0
	if l <= 0 {
		return 0
	}
	inc := 2 * math.Pi * d.tone.FreqHz / float64(d.rate)
	cosInc, sinInc := math.Cos(inc), math.Sin(inc)
	ci, si := math.Cos(float64(i0-w)*inc), math.Sin(float64(i0-w)*inc)
	var num, denR float64
	for i := 0; i < l; i++ {
		off := ((i0+i)*channels + c) * 2
		x := float64(int16(binary.LittleEndian.Uint16(pcm[off : off+2])))
		ref := float64(d.tone.Amplitude) * si
		num += x * ref
		denR += ref * ref
		ci, si = ci*cosInc-si*sinInc, si*cosInc+ci*sinInc
	}
	if denR <= 0 {
		return 0
	}
	return num / math.Sqrt(frameEnergy*denR)
}
