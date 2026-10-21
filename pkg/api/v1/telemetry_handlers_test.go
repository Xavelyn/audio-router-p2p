package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
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
}
