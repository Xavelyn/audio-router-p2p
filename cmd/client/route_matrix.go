package main

import (
	"fmt"
	"math"
	"sync"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/router"
)

// routeMatrix wraps a route's PCM stream without owning the underlying device.
// SlewGainMatrix serializes processing and live updates internally.
type routeMatrix struct {
	device   audio.AudioStream
	mixer    *router.SlewGainMatrix
	channels int
	mu       sync.Mutex
	buf      []byte
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
	return &routeMatrix{device: device, mixer: mixer, channels: ch}, nil
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

func (m *routeMatrix) Read(p []byte) (int, error) {
	n, err := m.device.Read(p)
	if n == 0 || err != nil {
		return n, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) < n {
		m.buf = make([]byte, n)
	}
	_, mixErr := m.mixer.Process(p[:n], m.buf[:n])
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
	}
	if _, err := m.mixer.Process(p, m.buf[:len(p)]); err != nil {
		return 0, err
	}
	return m.device.Write(m.buf[:len(p)])
}

func (m *routeMatrix) Close() error { return m.device.Close() }

var _ audio.AudioStream = (*routeMatrix)(nil)
