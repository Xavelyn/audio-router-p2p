package v1

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/audio-router-p2p/pkg/discovery"
)

// handleSystemStatus handles GET /api/v1/system/status.
func (s *V1Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	activeRoutes := 0
	if s.router != nil {
		activeRoutes = len(s.router.ListMatrix())
	}

	peerCount := 0
	if s.discovery != nil {
		peerCount = len(s.discovery.Peers())
	}

	syncLocked := false
	if s.syncMgr != nil {
		syncInfo := s.syncMgr.Offsets()
		syncLocked = len(syncInfo) > 0
	}

	status := SystemStatus{
		Status:          "ok",
		Version:         "1.0.0",
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
			for _, p := range discPeers {
				peers = append(peers, PeerNode{
					ID:       p.ID,
					Addr:     p.Addr,
					Status:   "connected",
					RTTMS:    0.0,
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
		if req.Addr == "" {
			s.WriteError(w, http.StatusBadRequest, "ERR_MISSING_ADDR", "peer address is required")
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
			// Record peer discovery if available
			s.discovery.AddPeer(&discovery.Peer{
				ID:       peer.ID,
				Addr:     req.Addr,
				LastSeen: time.Now(),
			})
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
