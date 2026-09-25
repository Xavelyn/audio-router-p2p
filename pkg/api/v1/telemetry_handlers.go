package v1

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/websocket"
)

// handleTelemetryStreamStats handles GET /api/v1/telemetry/stream-stats.
func (s *V1Server) handleTelemetryStreamStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	result := make(map[string]StreamStats)
	if s.streams != nil {
		rawStats := s.streams.Stats()
		for id, st := range rawStats {
			peers := make(map[string]JitterStats, len(st.Peers))
			for pAddr, jb := range st.Peers {
				peers[pAddr] = JitterStats{
					CurrentDepthFrames: jb.BufferedFrames,
					TargetDepthFrames:  jb.CapacityFrames,
					Underruns:          jb.Underruns,
					Overruns:           jb.Overruns,
					JitterMS:           float64(jb.TargetDelay.Milliseconds()),
				}
			}

			result[id] = StreamStats{
				StreamID:          st.StreamID,
				PacketsSent:       st.PacketsSent,
				PacketsReceived:   st.PacketsReceived,
				Underruns:         st.Underruns,
				Overruns:          st.Overruns,
				LatePackets:       st.LatePackets,
				EarlyPackets:      st.EarlyPackets,
				DTXState:          st.DTXState,
				PacketsSuppressed: st.PacketsSuppressed,
				RMSdBFS:           st.RMSdBFS,
				PeakdBFS:          st.PeakdBFS,
				FEC: FECStats{
					GroupSizeK:                 st.FEC.GroupSizeK,
					ParityPacketsSentTotal:     st.FEC.ParityPacketsSentTotal,
					ParityPacketsReceivedTotal: st.FEC.ParityPacketsReceivedTotal,
					PacketsRecoveredTotal:      st.FEC.PacketsRecoveredTotal,
					UnrecoverableGroupsTotal:   st.FEC.UnrecoverableGroupsTotal,
				},
				PLC: PLCStats{
					ConcealedFramesTotal:         st.PLC.ConcealedFramesTotal,
					PitchDetectedFramesTotal:     st.PLC.PitchDetectedFramesTotal,
					NoiseExtrapolatedFramesTotal: st.PLC.NoiseExtrapolatedFramesTotal,
					SilenceDecayEventsTotal:      st.PLC.SilenceDecayEventsTotal,
				},
				ASRC: ASRCStats{
					NominalRateHz:           st.ASRC.NominalRateHz,
					SmoothedWatermarkFrames: st.ASRC.SmoothedWatermarkFrames,
					TargetWatermarkFrames:   st.ASRC.TargetWatermarkFrames,
					DriftPPM:                st.ASRC.DriftPPM,
					ResampleRatio:           st.ASRC.ResampleRatio,
					SamplesAdjustedTotal:    st.ASRC.SamplesAdjustedTotal,
				},
				Peers: peers,
			}
		}
	}

	s.WriteJSON(w, http.StatusOK, result)
}

// handleTelemetryMetersSSE handles GET /api/v1/telemetry/meters (Server-Sent Events).
func (s *V1Server) handleTelemetryMetersSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.WriteError(w, http.StatusInternalServerError, "ERR_STREAM_UNSUPPORTED", "streaming unsupported")
		return
	}

	// Parse optional rate query param (10-60 Hz, default 30)
	rateHz := 30
	if rateStr := r.URL.Query().Get("rate"); rateStr != "" {
		if parsed, err := strconv.Atoi(rateStr); err == nil {
			if parsed >= 10 && parsed <= 60 {
				rateHz = parsed
			}
		}
	}
	_ = rateHz // Rate determines hub subscription pacing

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := s.hub.Subscribe(4)
	defer s.hub.Unsubscribe(sub)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.Ch:
			if !ok {
				return
			}
			_, err := fmt.Fprintf(w, "event: meter\ndata: %s\n\n", string(msg))
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleTelemetryWebSocket handles GET /api/v1/telemetry/ws.
func (s *V1Server) handleTelemetryWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.Authenticate(r) {
		s.WriteError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "missing or invalid credentials")
		return
	}

	handler := websocket.Handler(s.serveWebSocket)
	handler.ServeHTTP(w, r)
}

func (s *V1Server) serveWebSocket(ws *websocket.Conn) {
	defer ws.Close()

	sub := s.hub.Subscribe(4)
	defer s.hub.Unsubscribe(sub)

	doneCh := make(chan struct{})

	// Goroutine 1: Fan-out telemetry frames to WebSocket client
	go func() {
		defer close(doneCh)
		for {
			select {
			case <-doneCh:
				return
			case msg, ok := <-sub.Ch:
				if !ok {
					return
				}
				_ = ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if err := websocket.Message.Send(ws, string(msg)); err != nil {
					return
				}
			}
		}
	}()

	// Goroutine 2: Read control messages from client (crosspoint, patch, mute)
	for {
		var rawMsg string
		if err := websocket.Message.Receive(ws, &rawMsg); err != nil {
			if err == io.EOF {
				break
			}
			break
		}

		var cmd WSControlMessage
		if err := json.Unmarshal([]byte(rawMsg), &cmd); err != nil {
			resp := map[string]string{"error": "invalid json payload"}
			_ = websocket.JSON.Send(ws, resp)
			continue
		}

		s.processWSCommand(ws, cmd)
	}
}

func (s *V1Server) processWSCommand(ws *websocket.Conn, cmd WSControlMessage) {
	if s.router == nil {
		_ = websocket.JSON.Send(ws, map[string]string{"error": "router not configured"})
		return
	}

	switch cmd.Type {
	case "crosspoint":
		slew := cmd.SlewMS
		if slew <= 0 {
			slew = 20.0
		}
		mr, err := s.router.UpdateCrosspoint(cmd.RouteID, cmd.OutChannel, cmd.InChannel, cmd.Gain, slew)
		if err != nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		_ = websocket.JSON.Send(ws, map[string]any{"status": "ok", "route": mr})

	case "patch_route":
		slew := cmd.SlewMS
		var slewPtr *float64
		if slew > 0 {
			slewPtr = &slew
		}
		mr, err := s.router.PatchMatrix(cmd.RouteID, nil, cmd.Coefficients, slewPtr, cmd.Muted)
		if err != nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		_ = websocket.JSON.Send(ws, map[string]any{"status": "ok", "route": mr})

	case "mute":
		if cmd.Muted == nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": "missing muted value"})
			return
		}
		mr, err := s.router.PatchMatrix(cmd.RouteID, nil, nil, nil, cmd.Muted)
		if err != nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		_ = websocket.JSON.Send(ws, map[string]any{"status": "ok", "route": mr})

	case "ping":
		_ = websocket.JSON.Send(ws, map[string]string{"type": "pong"})

	default:
		_ = websocket.JSON.Send(ws, map[string]string{"error": fmt.Sprintf("unknown command type: %s", cmd.Type)})
	}
}
