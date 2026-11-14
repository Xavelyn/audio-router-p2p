package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/audio-router-p2p/pkg/api/telemetry"
	"github.com/audio-router-p2p/pkg/streamer"
	netTelemetry "github.com/audio-router-p2p/pkg/telemetry"
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

		// ADR 0034: load the artifact snapshot exactly once. When the
		// aggregator is unwired (tests, partial startup) it stays nil and the
		// response is byte-compatible with today's (health/devices omitted).
		var snap *netTelemetry.ArtifactSnapshot
		if s.artifactAggregator != nil {
			snap = s.artifactAggregator.GetArtifactSnapshot()
		}

		for id, st := range rawStats {
			peers := make(map[string]JitterStats, len(st.Peers))
			for pAddr, jb := range st.Peers {
				peers[pAddr] = JitterStats{
					CurrentDepthFrames: jb.BufferedFrames,
					TargetDepthFrames:  jb.CapacityFrames,
					Underruns:          jb.Underruns,
					Overruns:           jb.Overruns,
					JitterMS:           float64(jb.TargetDelay.Milliseconds()),
					LatePackets:        jb.LatePackets,
					EarlyPackets:       jb.EarlyPackets,
				}
			}

			entry := StreamStats{
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

			if snap != nil {
				if rh, ok := snap.Routes[id]; ok {
					health := rh // copy: the snapshot is immutable but keep ownership local
					entry.Health = &health
				}
				if impact := buildRouteDeviceImpact(st, snap); impact != nil {
					entry.Devices = impact
				}
			}

			result[id] = entry
		}
	}

	s.WriteJSON(w, http.StatusOK, result)
}

// buildRouteDeviceImpact overlays the artifact health of the route's source
// and destination devices (ADR 0034 attribution discipline: device XRuns are
// shared, never folded into the route score). Returns nil when neither
// device is present in the snapshot.
func buildRouteDeviceImpact(st streamer.StreamerStats, snap *netTelemetry.ArtifactSnapshot) *RouteDeviceImpact {
	var impact *RouteDeviceImpact
	if urn := st.SourceDeviceURN; urn != "" {
		if dh, ok := snap.Devices[urn]; ok {
			if impact == nil {
				impact = &RouteDeviceImpact{}
			}
			health := dh
			impact.Input = &health
		}
	}
	if urn := st.DestDeviceURN; urn != "" {
		if dh, ok := snap.Devices[urn]; ok {
			if impact == nil {
				impact = &RouteDeviceImpact{}
			}
			health := dh
			impact.Output = &health
		}
	}
	return impact
}

// handleTelemetryDeviceArtifacts handles GET /api/v1/telemetry/device-artifacts
// (ADR 0034): the URN-keyed map of per-device artifact health. Returns an
// empty object when the aggregator is unwired.
func (s *V1Server) handleTelemetryDeviceArtifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	resp := make(map[string]DeviceHealth)
	if s.artifactAggregator != nil {
		snap := s.artifactAggregator.GetArtifactSnapshot()
		for urn, dh := range snap.Devices {
			resp[urn] = dh
		}
	}

	s.WriteJSON(w, http.StatusOK, resp)
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

	localOnly := r.URL.Query().Get("local_only") == "true" || r.Header.Get("X-Cluster-Relay") == "true"
	var sub *telemetry.Subscriber
	if localOnly {
		sub = s.hub.SubscribeLocalOnly(4)
	} else {
		sub = s.hub.Subscribe(4)
	}
	defer s.hub.Unsubscribe(sub)

	ctx := r.Context()

	// Periodic SSE keepalive comment. The hub stays silent when no taps are
	// registered, so without this idle connections can be reaped by intermediaries
	// and peers cannot detect dead links promptly.
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
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

	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(stopCh)
			_ = ws.Close()
		})
	}
	defer stop()

	// Goroutine 1: Fan-out telemetry frames to WebSocket client
	go func() {
		defer stop()
		for {
			select {
			case <-stopCh:
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
		outCh := cmd.OutChannel
		inCh := cmd.InChannel
		gain := cmd.Gain
		slew := cmd.SlewMS

		if cmd.Crosspoint != nil {
			outCh = cmd.Crosspoint.OutChannel
			inCh = cmd.Crosspoint.InChannel
			gain = cmd.Crosspoint.Gain
			if cmd.Crosspoint.SlewMS > 0 {
				slew = cmd.Crosspoint.SlewMS
			}
		}

		if slew <= 0 {
			slew = 20.0
		}
		old, _ := s.router.GetMatrix(cmd.RouteID)
		mr, err := s.router.UpdateCrosspoint(cmd.RouteID, outCh, inCh, gain, slew)
		if err != nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		if err := s.updateLiveMatrix(mr); err != nil {
			s.restoreMatrix(old)
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
		old, _ := s.router.GetMatrix(cmd.RouteID)
		mr, err := s.router.PatchMatrix(cmd.RouteID, nil, cmd.Coefficients, slewPtr, cmd.Muted)
		if err != nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		if err := s.updateLiveMatrix(mr); err != nil {
			s.restoreMatrix(old)
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		_ = websocket.JSON.Send(ws, map[string]any{"status": "ok", "route": mr})

	case "mute":
		if cmd.Muted == nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": "missing muted value"})
			return
		}
		old, _ := s.router.GetMatrix(cmd.RouteID)
		mr, err := s.router.PatchMatrix(cmd.RouteID, nil, nil, nil, cmd.Muted)
		if err != nil {
			_ = websocket.JSON.Send(ws, map[string]string{"error": err.Error()})
			return
		}
		if err := s.updateLiveMatrix(mr); err != nil {
			s.restoreMatrix(old)
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
