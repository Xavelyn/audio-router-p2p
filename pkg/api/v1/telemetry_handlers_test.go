package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	netTelemetry "github.com/audio-router-p2p/pkg/telemetry"
	"golang.org/x/net/websocket"
)

type mockStreamerStatsManager struct {
	stats map[string]streamer.StreamerStats
}

func (m *mockStreamerStatsManager) StartRoute(route router.StreamRoute) error {
	return nil
}

func (m *mockStreamerStatsManager) StopRoute(routeID string) error {
	return nil
}

func (m *mockStreamerStatsManager) Stats() map[string]streamer.StreamerStats {
	return m.stats
}

func TestV1TelemetryStreamStats(t *testing.T) {
	s, mux := newTestV1Server(t)
	s.streams = &mockStreamerStatsManager{
		stats: map[string]streamer.StreamerStats{
			"route-1": {
				StreamID:        "stream-999",
				PacketsSent:     500,
				PacketsReceived: 495,
				Underruns:       1,
				Overruns:        0,
				RMSdBFS:         -18.5,
				PeakdBFS:        -6.2,
				FEC: streamer.FECStats{
					GroupSizeK:                 4,
					ParityPacketsSentTotal:     125,
					ParityPacketsReceivedTotal: 124,
					PacketsRecoveredTotal:      2,
				},
				PLC: streamer.PLCStats{
					ConcealedFramesTotal: 3,
				},
				ASRC: streamer.ASRCStats{
					NominalRateHz: 48000,
					DriftPPM:      -1.5,
				},
				Peers: map[string]streamer.JitterBufferStats{
					"192.168.1.50:7001": {
						BufferedFrames: 3,
						CapacityFrames: 10,
						Underruns:      0,
						Overruns:       0,
						TargetDelay:    20 * time.Millisecond,
					},
				},
			},
		},
	}

	w := doV1Request(mux, http.MethodGet, "/api/v1/telemetry/stream-stats", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var stats map[string]StreamStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}

	st, ok := stats["route-1"]
	if !ok {
		t.Fatal("expected route-1 in stream stats")
	}
	if st.StreamID != "stream-999" || st.PacketsSent != 500 {
		t.Errorf("stats content mismatch: %+v", st)
	}
	if st.FEC.PacketsRecoveredTotal != 2 {
		t.Errorf("fec packets recovered = %d; want 2", st.FEC.PacketsRecoveredTotal)
	}
	if peer, hasPeer := st.Peers["192.168.1.50:7001"]; !hasPeer || peer.CurrentDepthFrames != 3 {
		t.Errorf("peer jitter stats mismatch: %+v", peer)
	}
}

func TestV1TelemetryMetersSSE(t *testing.T) {
	s, mux := newTestV1Server(t)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/telemetry/meters?rate=30", nil)
	req.Header.Set("Authorization", "Bearer test-v1-token")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	// Broadcast test frame via hub
	testFrame := []byte(`{"stream_id":"test-sse-stream","channels":[{"channel":0,"rms_dbfs":-12.0,"peak_dbfs":-3.0,"clipped":false}]}`)
	s.Hub().Broadcast(testFrame)

	// Read first SSE chunk
	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read SSE frame: %v", err)
	}

	readStr := string(buf[:n])
	if !strings.Contains(readStr, "event: meter") || !strings.Contains(readStr, "test-sse-stream") {
		t.Errorf("unexpected SSE data: %s", readStr)
	}

	// Cancel client context
	cancel()
}

func TestV1TelemetryWebSocket(t *testing.T) {
	s, mux := newTestV1Server(t)

	// Add test route to router for live control testing
	_ = s.router.AddMatrix(router.MatrixRoute{
		ID:           "ws-route-1",
		Name:         "WS Route",
		Source:       router.MatrixEndpoint{NodeID: "local", DeviceID: "0", Channels: []int{0, 1}},
		Destination:  router.MatrixEndpoint{NodeID: "local", DeviceID: "1", Channels: []int{0, 1}},
		InChannels:   2,
		OutChannels:  2,
		Coefficients: []float64{1.0, 0.0, 0.0, 1.0},
		SlewMS:       20.0,
		State:        "active",
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsURL := fmt.Sprintf("ws://%s/api/v1/telemetry/ws?token=test-v1-token", ts.Listener.Addr().String())

	ws, err := websocket.Dial(wsURL, "", "http://localhost")
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Broadcast meter message from hub
	testMsg := `{"stream_id":"ws-stream","channels":[]}`
	time.Sleep(20 * time.Millisecond)
	s.Hub().Broadcast([]byte(testMsg))

	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var receivedMsg string
	if err := websocket.Message.Receive(ws, &receivedMsg); err != nil {
		t.Fatalf("WS receive initial frame failed: %v", err)
	}
	if !strings.Contains(receivedMsg, "ws-stream") {
		t.Errorf("expected ws-stream in received frame, got: %s", receivedMsg)
	}

	// Send live crosspoint gain command
	cmd := WSControlMessage{
		Type:       "crosspoint",
		RouteID:    "ws-route-1",
		OutChannel: 0,
		InChannel:  1,
		Gain:       0.5,
		SlewMS:     20.0,
	}
	_ = ws.SetDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Send(ws, cmd); err != nil {
		t.Fatalf("WS send command failed: %v", err)
	}

	// Read response
	var resp map[string]any
	if err := websocket.JSON.Receive(ws, &resp); err != nil {
		t.Fatalf("WS receive ack failed: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %+v", resp)
	}

	// Verify route updated in router
	mr, found := s.router.GetMatrix("ws-route-1")
	if !found {
		t.Fatal("route not found after ws command")
	}
	if mr.Coefficients[1] != 0.5 {
		t.Errorf("crosspoint gain = %f; want 0.5", mr.Coefficients[1])
	}

	// Send live crosspoint gain command with nested crosspoint object (frontend web/src/lib/api/ws.ts format)
	nestedCmd := WSControlMessage{
		Type:    "crosspoint",
		RouteID: "ws-route-1",
		Crosspoint: &CrosspointGainRequest{
			OutChannel: 0,
			InChannel:  1,
			Gain:       0.75,
			SlewMS:     15.0,
		},
	}
	_ = ws.SetDeadline(time.Now().Add(2 * time.Second))
	if err := websocket.JSON.Send(ws, nestedCmd); err != nil {
		t.Fatalf("WS send nested command failed: %v", err)
	}

	var nestedResp map[string]any
	if err := websocket.JSON.Receive(ws, &nestedResp); err != nil {
		t.Fatalf("WS receive nested ack failed: %v", err)
	}

	if nestedResp["status"] != "ok" {
		t.Errorf("expected status ok for nested crosspoint, got %+v", nestedResp)
	}

	mr2, found := s.router.GetMatrix("ws-route-1")
	if !found {
		t.Fatal("route not found after nested ws command")
	}
	if mr2.Coefficients[1] != 0.75 {
		t.Errorf("nested crosspoint gain = %f; want 0.75", mr2.Coefficients[1])
	}
}

// v1FakeRouteSource is a controlled fake RouteArtifactSource for handler tests.
type v1FakeRouteSource struct {
	mu      sync.Mutex
	samples map[string]netTelemetry.RouteArtifactSample
}

func (f *v1FakeRouteSource) RouteArtifactSamples() []netTelemetry.RouteArtifactSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]netTelemetry.RouteArtifactSample, 0, len(f.samples))
	for _, s := range f.samples {
		out = append(out, s)
	}
	return out
}

func (f *v1FakeRouteSource) set(routeID string, mutate func(*netTelemetry.RouteArtifactSample)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.samples[routeID]
	s.RouteID = routeID
	mutate(&s)
	f.samples[routeID] = s
}

// v1FakeDeviceSource is a controlled fake DeviceArtifactSource for handler tests.
type v1FakeDeviceSource struct {
	mu      sync.Mutex
	samples []netTelemetry.DeviceArtifactSample
}

func (f *v1FakeDeviceSource) DeviceArtifactSamples() []netTelemetry.DeviceArtifactSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]netTelemetry.DeviceArtifactSample, len(f.samples))
	copy(out, f.samples)
	return out
}

func (f *v1FakeDeviceSource) set(s netTelemetry.DeviceArtifactSample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, cur := range f.samples {
		if cur.URN == s.URN && cur.Direction == s.Direction {
			f.samples[i] = s
			return
		}
	}
	f.samples = append(f.samples, s)
}

// TestV1TelemetryStreamStats_WithArtifactAggregator verifies that when the
// artifact aggregator is wired, stream-stats carries a `health` object
// consistent with the injected samples and a `devices` overlay resolved from
// the route's source/destination URNs (ADR 0034).
func TestV1TelemetryStreamStats_WithArtifactAggregator(t *testing.T) {
	s, mux := newTestV1Server(t)
	s.streams = &mockStreamerStatsManager{
		stats: map[string]streamer.StreamerStats{
			"route-1": {
				StreamID:        "stream-999",
				PacketsSent:     500,
				PacketsReceived: 495,
				Underruns:       1,
				SourceDeviceURN: "alsa://hw:0,0",
				DestDeviceURN:   "alsa://hw:1,0",
				Peers: map[string]streamer.JitterBufferStats{
					"192.168.1.50:7001": {
						BufferedFrames: 3,
						CapacityFrames: 10,
						Underruns:      1,
						LatePackets:    2,
						TargetDelay:    20 * time.Millisecond,
					},
				},
			},
		},
	}

	// Two synchronous ticks so the 10 s windows have a baseline and a delta.
	routes := &v1FakeRouteSource{samples: map[string]netTelemetry.RouteArtifactSample{}}
	routes.set("route-1", func(rs *netTelemetry.RouteArtifactSample) { rs.JitterUnderruns = 0 })
	devices := &v1FakeDeviceSource{samples: []netTelemetry.DeviceArtifactSample{
		{URN: "alsa://hw:0,0", Direction: audio.DirectionInput, HWXRuns: 0},
		{URN: "alsa://hw:1,0", Direction: audio.DirectionOutput, HWXRuns: 12},
	}}
	agg := netTelemetry.NewArtifactAggregator(routes, devices, time.Second, 10*time.Second)
	_ = agg.CollectSnapshot()
	routes.set("route-1", func(rs *netTelemetry.RouteArtifactSample) {
		rs.JitterUnderruns = 1 // 0 -> 1 in window
		rs.FECRecovered = 2    // 0 -> 2 in window
	})
	_ = agg.CollectSnapshot()

	s.SetArtifactAggregator(agg)

	w := doV1Request(mux, http.MethodGet, "/api/v1/telemetry/stream-stats", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var stats map[string]StreamStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	st, ok := stats["route-1"]
	if !ok {
		t.Fatal("expected route-1 in stream stats")
	}

	if st.Health == nil {
		t.Fatal("expected health to be present when aggregator is wired")
	}
	if st.Health.Underruns != 1 {
		t.Errorf("health.underruns = %d; want 1 (windowed)", st.Health.Underruns)
	}
	if st.Health.FECRecovered != 2 {
		t.Errorf("health.fec_recovered = %d; want 2 (windowed)", st.Health.FECRecovered)
	}
	// 1 in-window underrun: 100 * C_underrun(1) = 80 / MINOR.
	if st.Health.Score != 80 {
		t.Errorf("health.score = %v; want 80", st.Health.Score)
	}
	if st.Health.Severity != ArtifactSeverityMinor {
		t.Errorf("health.severity = %s; want MINOR", st.Health.Severity)
	}
	if st.Health.WindowS != 10 {
		t.Errorf("health.window_s = %d; want 10", st.Health.WindowS)
	}

	if st.Devices == nil {
		t.Fatal("expected devices overlay when aggregator is wired")
	}
	if st.Devices.Input == nil || st.Devices.Input.URN != "alsa://hw:0,0" {
		t.Errorf("devices.input = %+v; want urn alsa://hw:0,0", st.Devices.Input)
	}
	if st.Devices.Output == nil || st.Devices.Output.URN != "alsa://hw:1,0" {
		t.Errorf("devices.output = %+v; want urn alsa://hw:1,0", st.Devices.Output)
	}
	if st.Devices.Output.Output == nil {
		t.Fatal("devices.output.output direction object missing")
	}
	if st.Devices.Output.Output.XRunTotal != 12 {
		t.Errorf("devices.output.output.xrun_total = %d; want 12", st.Devices.Output.Output.XRunTotal)
	}
	// No XRuns in-window on a constant counter; score stays clean.
	if st.Devices.Output.Score != 100 || st.Devices.Output.Severity != ArtifactSeverityNone {
		t.Errorf("devices.output score/severity = %v/%s; want 100/NONE", st.Devices.Output.Score, st.Devices.Output.Severity)
	}

	// JitterStats late_packets filled from the streamer's per-peer stats.
	if peer, hasPeer := st.Peers["192.168.1.50:7001"]; !hasPeer || peer.LatePackets != 2 {
		t.Errorf("peer late_packets = %+v; want 2", peer)
	}
}

// TestV1TelemetryStreamStats_WithoutAggregator_BackwardCompatible is the
// backward-compat regression guard: with the artifact aggregator unwired, the
// response must be byte-compatible with today's for aggregator-derived fields
// (no `health`, no `devices`). Peer-level late/early packets are plain
// additive fields per ADR 0034 §4: always present, zero when absent.
func TestV1TelemetryStreamStats_WithoutAggregator_BackwardCompatible(t *testing.T) {
	s, mux := newTestV1Server(t)
	s.streams = &mockStreamerStatsManager{
		stats: map[string]streamer.StreamerStats{
			"route-1": {
				StreamID:        "stream-999",
				PacketsSent:     500,
				PacketsReceived: 495,
				Underruns:       1,
				SourceDeviceURN: "alsa://hw:0,0",
				DestDeviceURN:   "alsa://hw:1,0",
				Peers: map[string]streamer.JitterBufferStats{
					"192.168.1.50:7001": {
						BufferedFrames: 3,
						CapacityFrames: 10,
						Underruns:      1,
						TargetDelay:    20 * time.Millisecond,
					},
				},
			},
		},
	}
	// s.artifactAggregator intentionally left nil.

	w := doV1Request(mux, http.MethodGet, "/api/v1/telemetry/stream-stats", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var raw map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	for id, entry := range raw {
		if _, ok := entry["health"]; ok {
			t.Errorf("route %s carries health although the aggregator is unwired", id)
		}
		if _, ok := entry["devices"]; ok {
			t.Errorf("route %s carries devices although the aggregator is unwired", id)
		}
		if peers, ok := entry["peers"].(map[string]any); ok {
			for pAddr, p := range peers {
				pm, _ := p.(map[string]any)
				// ADR 0034 §4: plain additive fields — present even when zero
				// (only health/devices are omitted when the aggregator is unwired).
				if v, ok := pm["late_packets"].(float64); !ok || v != 0 {
					t.Errorf("peer %s late_packets = %v; want 0 (present, plain field)", pAddr, pm["late_packets"])
				}
				if v, ok := pm["early_packets"].(float64); !ok || v != 0 {
					t.Errorf("peer %s early_packets = %v; want 0 (present, plain field)", pAddr, pm["early_packets"])
				}
			}
		}
	}

	// Pre-existing payload is intact.
	var stats map[string]StreamStats
	_ = json.Unmarshal(w.Body.Bytes(), &stats)
	if st := stats["route-1"]; st.StreamID != "stream-999" || st.PacketsSent != 500 || st.Underruns != 1 {
		t.Errorf("pre-existing payload mismatch: %+v", st)
	}
}

// TestV1TelemetryDeviceArtifacts covers the new URN-keyed endpoint: auth is
// enforced, unwired returns an empty object, and wired returns the snapshot's
// device map with the ADR 0034 shape.
func TestV1TelemetryDeviceArtifacts(t *testing.T) {
	s, mux := newTestV1Server(t)
	const path = "/api/v1/telemetry/device-artifacts"

	// (c) Auth enforced.
	if w := doV1Request(mux, http.MethodGet, path, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without credentials, got %d", w.Code)
	}
	if w := doV1Request(mux, http.MethodGet, path, nil, "Bearer wrong-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with invalid credentials, got %d", w.Code)
	}
	if w := doV1Request(mux, http.MethodPost, path, nil, "Bearer test-v1-token"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", w.Code)
	}

	// Unwired aggregator: empty object.
	w := doV1Request(mux, http.MethodGet, path, nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 unwired, got %d", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "{}" {
		t.Fatalf("unwired device-artifacts = %q; want {}", body)
	}

	// Wired aggregator: correct shape.
	routes := &v1FakeRouteSource{samples: map[string]netTelemetry.RouteArtifactSample{}}
	devices := &v1FakeDeviceSource{samples: []netTelemetry.DeviceArtifactSample{
		{URN: "alsa://hw:0,0", Direction: audio.DirectionInput, HWXRuns: 0},
		{URN: "alsa://hw:1,0", Direction: audio.DirectionOutput, HWXRuns: 12},
	}}
	agg := netTelemetry.NewArtifactAggregator(routes, devices, time.Second, 10*time.Second)
	_ = agg.CollectSnapshot()
	s.SetArtifactAggregator(agg)

	w = doV1Request(mux, http.MethodGet, path, nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 wired, got %d", w.Code)
	}
	var devMap map[string]DeviceHealth
	if err := json.Unmarshal(w.Body.Bytes(), &devMap); err != nil {
		t.Fatalf("unmarshal device-artifacts: %v", err)
	}
	if len(devMap) != 2 {
		t.Fatalf("expected 2 devices, got %d: %s", len(devMap), w.Body.String())
	}
	if in := devMap["alsa://hw:0,0"]; in.URN != "alsa://hw:0,0" || in.Input == nil || in.Output != nil {
		t.Errorf("input device shape wrong: %+v", in)
	}
	if out := devMap["alsa://hw:1,0"]; out.URN != "alsa://hw:1,0" || out.Output == nil || out.Output.XRunTotal != 12 {
		t.Errorf("output device shape wrong: %+v", out)
	}
	body := w.Body.String()
	for _, key := range []string{`"urn"`, `"ghost"`, `"score"`, `"severity"`, `"input"`, `"output"`, `"xrun_window"`, `"xrun_total"`} {
		if !strings.Contains(body, key) {
			t.Errorf("device-artifacts response missing key %s: %s", key, body)
		}
	}
}
