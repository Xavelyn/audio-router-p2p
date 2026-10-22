package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/audio/dsp"
)

// probeSession carries the probe origin and the per-channel results between
// the injector (send side), the detector (playout side) and the calibration
// caller, all within one daemon (same clock).
type probeSession struct {
	t0Nano atomic.Int64
	mu     sync.Mutex
	hits   []dsp.ProbeHit
	done   bool
	busy   bool
}

// begin reserves the session for a calibration run, clearing its state.
// It reports false when a run is already in progress.
func (s *probeSession) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	s.t0Nano.Store(0)
	s.hits = nil
	s.done = false
	return true
}

func (s *probeSession) end() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

func (s *probeSession) setOrigin(t time.Time) { s.t0Nano.Store(t.UnixNano()) }

func (s *probeSession) origin() time.Time {
	if n := s.t0Nano.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (s *probeSession) addHits(hits []dsp.ProbeHit, complete bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, hits...)
	if complete {
		s.done = true
	}
}

func (s *probeSession) result() ([]dsp.ProbeHit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dsp.ProbeHit(nil), s.hits...), s.done
}

// probeInjector wraps a route input and adds the armed probe tone to each
// read frame. The generator's first processed frame stamps the session
// origin, which the detector uses as the timing reference. The tone is
// injected after the gain matrix, so it bypasses the route's gain and mute.
type probeInjector struct {
	device   audio.AudioStream
	rate     int
	channels int
	session  *probeSession
	mu       sync.Mutex
	gen      *dsp.ProbeGenerator
}

var _ audio.AudioStream = (*probeInjector)(nil)

func newProbeInjector(device audio.AudioStream, sampleRate, channels int, session *probeSession) *probeInjector {
	return &probeInjector{device: device, rate: sampleRate, channels: channels, session: session}
}

// SetProbe arms the next probe burst on this route's input.
func (p *probeInjector) SetProbe(tone dsp.ProbeTone) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen = dsp.NewProbeGenerator(p.rate, tone, nil)
}

func (p *probeInjector) Read(b []byte) (int, error) {
	n, err := p.device.Read(b)
	if n > 0 {
		p.mu.Lock()
		gen := p.gen
		p.mu.Unlock()
		if gen != nil {
			started, done := gen.Process(b[:n], p.channels)
			if started {
				p.session.setOrigin(time.Now())
			}
			if done {
				p.mu.Lock()
				p.gen = nil
				p.mu.Unlock()
			}
		}
	}
	return n, err
}

func (p *probeInjector) Write(b []byte) (int, error) { return p.device.Write(b) }
func (p *probeInjector) Close() error                { return p.device.Close() }

// Counters delegates to the wrapped device so diagnostics can see the
// hardware counters through the injector.
func (p *probeInjector) Counters() audio.DeviceCounters { return audio.CountersOf(p.device) }

// probeDetector wraps a route output (playout write) and detects the probe
// tone, timing its arrival against the session origin. Frames are timed at
// the playout write, so the measured delay excludes the output bus tick and
// the hardware device latency.
type probeDetector struct {
	device   audio.AudioStream
	rate     int
	channels int
	session  *probeSession
	mu       sync.Mutex
	armed    dsp.ProbeTone
	hasTone  bool
	det      *dsp.ProbeDetector
}

var _ audio.AudioStream = (*probeDetector)(nil)

func newProbeDetector(device audio.AudioStream, sampleRate, channels int, session *probeSession) *probeDetector {
	return &probeDetector{device: device, rate: sampleRate, channels: channels, session: session}
}

// Arm starts detection of the given probe tone. The DSP detector is built
// lazily once the session origin is known (the injector stamps it on the
// first processed frame).
func (p *probeDetector) Arm(tone dsp.ProbeTone) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = tone
	p.hasTone = true
	p.det = nil
}

func (p *probeDetector) Write(b []byte) (int, error) {
	p.mu.Lock()
	det := p.det
	if det == nil && p.hasTone {
		if t0 := p.session.origin(); !t0.IsZero() {
			det = dsp.NewProbeDetector(p.rate, p.armed, t0, maxCalibrationSearch, nil)
			p.det = det
		}
	}
	p.mu.Unlock()
	if det != nil {
		if hits, done := det.Process(b, p.channels, time.Now()); len(hits) > 0 || done {
			p.session.addHits(hits, done)
			if done {
				p.mu.Lock()
				p.det = nil
				p.hasTone = false
				p.mu.Unlock()
			}
		}
	}
	return p.device.Write(b)
}

func (p *probeDetector) Read(b []byte) (int, error) { return p.device.Read(b) }
func (p *probeDetector) Close() error               { return p.device.Close() }

// Counters delegates to the wrapped device so diagnostics can see the
// hardware counters through the detector.
func (p *probeDetector) Counters() audio.DeviceCounters { return audio.CountersOf(p.device) }
