package main

import (
	"errors"
	"fmt"
	"time"

	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/audio/dsp"
)

// maxCalibrationSearch bounds the detector's delay search window. The probe
// round trip on a healthy loopback route is well under 200 ms; 2 s leaves
// room for heavily saturated jitter buffers while keeping the search cheap.
const maxCalibrationSearch = 2 * time.Second

// probeRunTimeout adds margin to the probe duration for the tone to travel
// the pipeline and for the detector to see its boundary frames.
const probeRunTimeout = 3 * time.Second

// CalibrateRoute runs one probe round on a running route: it injects the
// probe tone into the route's input after the gain stage, detects it at the
// route's playout write, and returns the per-channel pipeline delays. The
// boolean result reports whether the route is running (false → 404).
//
// The probe bypasses the route's gain, mute, and alignment offsets, so the
// measured delays are the raw pipeline delays. With Apply, the computed
// offsets (max minus per-channel delay) are stored as the route alignment,
// which is stable under re-apply: the probe keeps measuring the same raw
// delays, so the same offsets are recomputed.
func (m *streamManager) CalibrateRoute(routeID string, req v1.CalibrateRequest) (v1.CalibrateResult, bool, error) {
	m.mu.Lock()
	as, ok := m.routes[routeID]
	m.mu.Unlock()
	if !ok {
		return v1.CalibrateResult{}, false, nil
	}
	if as.injector == nil || as.detector == nil {
		return v1.CalibrateResult{}, true, v1.ErrRouteNotCalibratable
	}
	if !as.session.begin() {
		return v1.CalibrateResult{}, true, errors.New("calibration already in progress on this route")
	}
	defer as.session.end()

	// Build the tone from the defaults with request overrides.
	tone := dsp.DefaultProbeTone()
	if req.FreqHz != 0 {
		tone.FreqHz = req.FreqHz
	}
	if req.Amplitude != 0 {
		tone.Amplitude = req.Amplitude
	}
	if req.DurationMS != 0 {
		tone.Duration = time.Duration(req.DurationMS) * time.Millisecond
	}

	// Arm detection before injection so no tone frame is missed; the
	// detector builds itself once the injector stamps the origin.
	as.detector.Arm(tone)
	as.injector.SetProbe(tone)

	deadline := time.Now().Add(tone.Duration + probeRunTimeout)
	var hits []dsp.ProbeHit
	var done bool
	for {
		hits, done = as.session.result()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	channels := as.route.Channels
	if channels <= 0 {
		channels = 2
	}
	delays := make([]float64, channels)
	detected := 0
	for _, h := range hits {
		if h.Channel >= 0 && h.Channel < channels {
			if delays[h.Channel] == 0 {
				delays[h.Channel] = h.DelayMS
				detected++
			}
		}
	}
	allDetected := detected == channels

	result := v1.CalibrateResult{
		RouteID:         routeID,
		Detected:        allDetected,
		ChannelDelaysMS: delays,
		FreqHz:          tone.FreqHz,
		DurationMS:      int(tone.Duration / time.Millisecond),
	}
	if allDetected {
		maxMS := 0.0
		for _, d := range delays {
			result.DelayMS += d
			if d > maxMS {
				maxMS = d
			}
		}
		result.DelayMS /= float64(channels)
		if req.Apply {
			offsets := make([]float64, channels)
			for c, d := range delays {
				offsets[c] = maxMS - d
			}
			if applied, err := as.matrix.SetChannelDelays(offsets); err != nil {
				return result, true, fmt.Errorf("store alignment: %w", err)
			} else {
				offsets = applied
			}
			m.persistDelays(as, offsets)
			result.OffsetsMS = offsets
			result.Applied = true
		}
	} else {
		result.Note = fmt.Sprintf("probe detected on %d of %d channels", detected, channels)
	}
	if !result.Applied && as.matrix != nil {
		result.OffsetsMS = as.matrix.ChannelDelaysMS()
	}
	return result, true, nil
}

// persistDelays stores the applied offsets on the route and writes the
// route table so the alignment survives a restart. If the route was stopped
// or restarted while the calibration ran, the stale state is not persisted.
func (m *streamManager) persistDelays(as *activeStream, offsets []float64) {
	m.mu.Lock()
	rt := m.rt
	active := m.routes[as.route.ID] == as
	if active {
		as.route.ChannelDelaysMS = append([]float64(nil), offsets...)
	}
	m.mu.Unlock()
	if rt != nil && active {
		_ = rt.Add(as.route)
	}
}

// SetRouteAlignment live-updates a running route's per-channel alignment
// offsets (milliseconds, each in [0, 1000]) and persists them on the route.
// The boolean result reports whether the route is running (false → 404).
func (m *streamManager) SetRouteAlignment(routeID string, offsetsMS []float64) ([]float64, bool, error) {
	m.mu.Lock()
	as, ok := m.routes[routeID]
	m.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	if as.matrix == nil {
		return nil, true, v1.ErrRouteNotCalibratable
	}
	applied, err := as.matrix.SetChannelDelays(offsetsMS)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %v", v1.ErrInvalidAlignment, err)
	}
	m.persistDelays(as, applied)
	return applied, true, nil
}

// GetRouteAlignment reports a route's per-channel alignment offsets: the
// live values when the route is running, otherwise the persisted route
// values. The boolean result reports whether the route is known.
func (m *streamManager) GetRouteAlignment(routeID string) ([]float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	as, ok := m.routes[routeID]
	if !ok {
		if m.rt != nil {
			if route, found := m.rt.Get(routeID); found {
				return append([]float64(nil), route.ChannelDelaysMS...), true
			}
		}
		return nil, false
	}
	if as.matrix != nil {
		return as.matrix.ChannelDelaysMS(), true
	}
	return append([]float64(nil), as.route.ChannelDelaysMS...), true
}
