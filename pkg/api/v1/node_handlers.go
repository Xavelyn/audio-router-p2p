package v1

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

// handleSystemStatus handles GET /api/v1/system/status.
func (s *V1Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	nodeID := ""
	if s.discovery != nil {
		nodeID = s.discovery.NodeID()
	} else if s.cfg != nil {
		nodeID = s.cfg.NodeID
	}

	activeRoutes := 0
	if s.streams != nil {
		activeRoutes = len(s.streams.Stats())
	}
	if activeRoutes == 0 && s.router != nil {
		activeRoutes = len(s.router.ListMatrix())
	}

	peerCount := 0
	if s.discovery != nil {
		peerCount = len(s.discovery.Peers())
	}

	syncLocked := false
	if s.syncMgr != nil {
		syncInfo := s.syncMgr.Offsets()
		for _, off := range syncInfo {
			if math.Abs(float64(off.Offset)) < float64(10*time.Millisecond) {
				syncLocked = true
				break
			}
		}
	}

	status := SystemStatus{
		Status:          "ok",
		Version:         "1.0.0",
		NodeID:          nodeID,
		NodeName:        nodeID,
		UptimeSeconds:   time.Since(s.startTime).Seconds(),
		ActiveRoutes:    activeRoutes,
		DiscoveredPeers: peerCount,
		ClockSyncLocked: syncLocked,
	}

	s.WriteJSON(w, http.StatusOK, status)
}

// handleSystemPeers handles GET and POST on /api/v1/system/peers.
func (s *V1Server) handleSystemPeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var peers []PeerNode
		if s.discovery != nil {
			discPeers := s.discovery.Peers()
			peers = make([]PeerNode, 0, len(discPeers))
			var offsets map[string]float64
			if s.syncMgr != nil {
				raw := s.syncMgr.Offsets()
				offsets = make(map[string]float64, len(raw))
				for id, off := range raw {
					offsets[id] = float64(off.RTT.Microseconds()) / 1000.0
				}
			}
			for _, p := range discPeers {
				state := s.discovery.PeerState(p.ID)
				status := state
				if state == "alive" || state == "unknown" {
					status = "connected"
				}
				var rttMS float64
				if offsets != nil {
					rttMS = offsets[p.ID]
				}
				peers = append(peers, PeerNode{
					ID:       p.ID,
					Addr:     p.Addr,
					Status:   status,
					RTTMS:    rttMS,
					LastSeen: p.LastSeen.Format(time.RFC3339),
				})
			}
		} else {
			peers = []PeerNode{}
		}
		s.WriteJSON(w, http.StatusOK, peers)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req ConnectPeerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		req.Addr = strings.TrimSpace(req.Addr)
		if req.Addr == "" {
			s.WriteError(w, http.StatusBadRequest, "ERR_MISSING_ADDR", "peer address is required")
			return
		}
		if host, port, err := net.SplitHostPort(req.Addr); err != nil || host == "" || port == "" {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_ADDR", "invalid peer host:port address")
			return
		}

		peer := PeerNode{
			ID:       "peer-" + req.Addr,
			Addr:     req.Addr,
			Status:   "connected",
			RTTMS:    1.0,
			LastSeen: time.Now().Format(time.RFC3339),
		}

		if s.discovery != nil {
			p, err := s.discovery.ConnectPeer(req.Addr)
			if err != nil {
				s.WriteError(w, http.StatusBadGateway, "ERR_PEER_CONNECT", err.Error())
				return
			}
			if p != nil {
				peer.ID = p.ID
				peer.Addr = p.Addr
				if !p.LastSeen.IsZero() {
					peer.LastSeen = p.LastSeen.Format(time.RFC3339)
				}
			}
		}

		s.WriteJSON(w, http.StatusCreated, peer)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleSystemClockSync handles GET /api/v1/system/clock-sync.
func (s *V1Server) handleSystemClockSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var offsetNS int64
	driftPPM := 0.0
	locked := false
	source := "internal"

	if s.syncMgr != nil {
		offsets := s.syncMgr.Offsets()
		if len(offsets) > 0 {
			locked = true
			source = "ptp"
			for _, off := range offsets {
				offsetNS = off.Offset.Nanoseconds()
				break
			}
		}
	}

	resp := ClockSyncStatus{
		Locked:   locked,
		OffsetNS: offsetNS,
		DriftPPM: driftPPM,
		Source:   source,
	}

	s.WriteJSON(w, http.StatusOK, resp)
}
