package main

import (
	"fmt"
	"math"
	"sync"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/audio/dsp"
	"github.com/audio-router-p2p/pkg/router"
)

// maxChannelDelayMS bounds a per-channel alignment offset (1000 ms).
const maxChannelDelayMS = 1000.0

// routeMatrix wraps a route's PCM stream without owning the underlying device.
// SlewGainMatrix serializes processing and live updates internally.
type routeMatrix struct {
	device   audio.AudioStream
	mixer    *router.SlewGainMatrix
	delay    *dsp.DelayLine
	channels int
	mu       sync.Mutex
	buf      []byte
	dBuf     []byte
}

func newRouteMatrix(device audio.AudioStream, route router.StreamRoute) (*routeMatrix, error) {
	ch := route.Channels
	if route.InChannels != 0 && route.InChannels != ch || route.OutChannels != 0 && route.OutChannels != ch {
		return nil, fmt.Errorf("route %s: channel-count conversion is not supported by live PCM routes", route.ID)
	}
	weights := routeWeights(route)
	mixer, err := router.NewSlewGainMatrixWithSampleRate(ch, ch, route.SampleRate, weights)
	if err != nil {
		return nil, err
	}
	rm := &routeMatrix{
		device:   device,
		mixer:    mixer,
		delay:    dsp.NewDelayLine(route.SampleRate, ch, route.SampleRate),
		channels: ch,
	}
	if len(route.ChannelDelaysMS) > 0 {
		if _, err := rm.SetChannelDelays(route.ChannelDelaysMS); err != nil {
			return nil, err
		}
	}
	return rm, nil
}

func routeWeights(route router.StreamRoute) []float32 {
	ch := route.Channels
	weights := make([]float32, ch*ch)
	if route.Muted {
		return weights
	}
	if len(route.Coefficients) == len(weights) {
		for i, v := range route.Coefficients {
			weights[i] = float32(v)
		}
	} else {
		for i := 0; i < ch; i++ {
			weights[i*ch+i] = 1
		}
	}
	return weights
}

func (m *routeMatrix) Update(route router.MatrixRoute) error {
	if route.InChannels != m.channels || route.OutChannels != m.channels {
		return fmt.Errorf("route %s: live channel-count changes require restarting the route", route.ID)
	}
	if err := route.Validate(); err != nil {
		return err
	}
	weights := make([]float32, m.channels*m.channels)
	if !route.Muted {
		for i, v := range route.Coefficients {
			weights[i] = float32(v)
		}
	}
	samples := int(math.Ceil(route.SlewMS * float64(m.mixer.SampleRate()) / 1000))
	return m.mixer.SetTarget(weights, samples)
}

// SetChannelDelays live-updates the per-channel alignment offset in
// milliseconds (each in [0, 1000]). The delay is applied before the gain
// matrix, so it delays the route's audio before it is mixed. It returns the
// offsets actually applied, quantized to whole sample periods.
func (m *routeMatrix) SetChannelDelays(ms []float64) ([]float64, error) {
	if len(ms) != m.channels {
		return nil, fmt.Errorf("route: want %d per-channel offsets, got %d", m.channels, len(ms))
	}
	samples := make([]int, m.channels)
	for i, v := range ms {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > maxChannelDelayMS {
			return nil, fmt.Errorf("route: channel %d offset %.3f ms out of range [0, %g]", i, v, maxChannelDelayMS)
		}
		samples[i] = int(v * float64(m.mixer.SampleRate()) / 1000)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.delay.SetDelaysSamples(samples); err != nil {
		return nil, err
	}
	return m.delay.DelaysMS(), nil
}

// ChannelDelaysMS returns the current per-channel alignment offset in ms.
func (m *routeMatrix) ChannelDelaysMS() []float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delay.DelaysMS()
}

func (m *routeMatrix) Read(p []byte) (int, error) {
	n, err := m.device.Read(p)
	if n == 0 || err != nil {
		return n, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) < n {
		m.buf = make([]byte, n)
		m.dBuf = make([]byte, n)
	}
	if _, err := m.delay.Process(p[:n], m.dBuf[:n]); err != nil {
		return n, err
	}
	_, mixErr := m.mixer.Process(m.dBuf[:n], m.buf[:n])
	if mixErr == nil {
		copy(p[:n], m.buf[:n])
	}
	return n, mixErr
}

func (m *routeMatrix) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) < len(p) {
		m.buf = make([]byte, len(p))
		m.dBuf = make([]byte, len(p))
	}
	if _, err := m.delay.Process(p, m.dBuf[:len(p)]); err != nil {
		return 0, err
	}
	if _, err := m.mixer.Process(m.dBuf[:len(p)], m.buf[:len(p)]); err != nil {
		return 0, err
	}
	return m.device.Write(m.buf[:len(p)])
}

func (m *routeMatrix) Close() error { return m.device.Close() }

// Counters delegates to the wrapped device so diagnostics can see the
// hardware counters through the matrix.
func (m *routeMatrix) Counters() audio.DeviceCounters { return audio.CountersOf(m.device) }

var _ audio.AudioStream = (*routeMatrix)(nil)
