package dsp

import (
	"encoding/binary"
	"fmt"
)

// DelayLine delays each PCM channel by a fixed, live-adjustable number of
// samples. Routes that feed a shared output can use it for alignment: the
// faster routes are delayed until every source arrives at the same instant.
type DelayLine struct {
	rate     int
	channels int
	max      int
	ring     [][]int16 // per-channel history, most recent at pos-1
	delays   []int     // per-channel delay in samples
	pos      int
}

// NewDelayLine creates a delay line for the given format. maxSamples bounds
// the per-channel delay and sets the history depth.
func NewDelayLine(sampleRate, channels, maxSamples int) *DelayLine {
	if maxSamples < 1 {
		maxSamples = 1
	}
	ring := make([][]int16, channels)
	for c := range ring {
		ring[c] = make([]int16, maxSamples)
	}
	return &DelayLine{
		rate:     sampleRate,
		channels: channels,
		max:      maxSamples,
		ring:     ring,
		delays:   make([]int, channels),
	}
}

// SetDelaysSamples sets the per-channel delay live. Values are clamped to
// [0, maxSamples]. A slice of the wrong length is an error.
func (d *DelayLine) SetDelaysSamples(delays []int) error {
	if len(delays) != d.channels {
		return fmt.Errorf("delay line: want %d per-channel delays, got %d", d.channels, len(delays))
	}
	cp := make([]int, len(delays))
	for i, v := range delays {
		if v < 0 {
			v = 0
		}
		if v > d.max {
			v = d.max
		}
		cp[i] = v
	}
	d.delays = cp
	return nil
}

// DelaysSamples returns the current per-channel delays in samples.
func (d *DelayLine) DelaysSamples() []int {
	return append([]int(nil), d.delays...)
}

// DelaysMS returns the current per-channel delays in milliseconds.
func (d *DelayLine) DelaysMS() []float64 {
	ms := make([]float64, d.channels)
	for i, s := range d.delays {
		ms[i] = float64(s) * 1000 / float64(d.rate)
	}
	return ms
}

// Process writes the delayed samples of in to out (little-endian int16, same
// length). Until a channel's history is deep enough for its delay the output
// for that channel is silence (warm-up).
func (d *DelayLine) Process(in, out []byte) (int, error) {
	if len(out) < len(in) {
		return 0, fmt.Errorf("delay line: output buffer too small")
	}
	samples := len(in) / (2 * d.channels)
	for i := 0; i < samples; i++ {
		d.pos++
		p := d.pos % d.max
		for c := 0; c < d.channels; c++ {
			off := (i*d.channels + c) * 2
			v := int16(binary.LittleEndian.Uint16(in[off : off+2]))
			d.ring[c][p] = v
			delay := d.delays[c]
			var o int16
			if d.pos > delay {
				o = d.ring[c][(p-delay+d.max)%d.max]
			}
			binary.LittleEndian.PutUint16(out[off:off+2], uint16(o))
		}
	}
	return len(in), nil
}
