package v1

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/audio-router-p2p/pkg/router"
)

// Live matrix updates are optional for stream-manager implementations used by
// API consumers and tests. The daemon implements this without restarting I/O.
type liveMatrixUpdater interface {
	UpdateMatrix(router.MatrixRoute) error
}

func (s *V1Server) updateLiveMatrix(mr router.MatrixRoute) error {
	if updater, ok := s.streams.(liveMatrixUpdater); ok {
		return updater.UpdateMatrix(mr)
	}
	return nil
}

func (s *V1Server) restoreMatrix(mr router.MatrixRoute) {
	if err := s.router.AddMatrix(mr); err != nil {
		// Keep the original failure visible to the caller, but record rollback
		// failures so a persisted/live mismatch does not go unnoticed.
		log.Printf("route %s: matrix rollback failed: %v", mr.ID, err)
	}
}

// handleRoutes handles GET /api/v1/routes and POST /api/v1/routes.
func (s *V1Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		s.WriteError(w, http.StatusInternalServerError, "ERR_ROUTER_NIL", "router store not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		routes := s.router.ListMatrix()
		s.WriteJSON(w, http.StatusOK, routes)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CreateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}

		if req.ID == "" {
			req.ID = fmt.Sprintf("route-%d", time.Now().UnixNano())
		}

		// Conflict check
		if _, exists := s.router.GetMatrix(req.ID); exists {
			s.WriteError(w, http.StatusConflict, "ERR_ROUTE_COLLISION", fmt.Sprintf("route %s already exists", req.ID))
			return
		}

		// Validate device capabilities: source must have input channels, destination must have output channels
		if s.devReg != nil {
			if req.Source.DeviceID != "" {
				if dev, ok := s.devReg.GetDevice(req.Source.DeviceID); ok {
					if dev.MaxInput <= 0 {
						s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_DEVICE_DIRECTION",
							fmt.Sprintf("source device %q (id %s) has no input channels; outputs cannot be used as sources", dev.Name, dev.ID))
						return
					}
				}
			}
			if req.Destination.NodeID == "" || req.Destination.NodeID == "local" {
				if req.Destination.DeviceID != "" {
					if dev, ok := s.devReg.GetDevice(req.Destination.DeviceID); ok {
						if dev.MaxOutput <= 0 {
							s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_DEVICE_DIRECTION",
								fmt.Sprintf("destination device %q (id %s) has no output channels; inputs cannot be used as destinations", dev.Name, dev.ID))
							return
						}
					}
				}
			}
		}

		inCh := len(req.Source.Channels)
		if inCh == 0 {
			inCh = 2
			req.Source.Channels = []int{0, 1}
		}
		outCh := len(req.Destination.Channels)
		if outCh == 0 {
			outCh = 2
			req.Destination.Channels = []int{0, 1}
		}

		coeffs := req.Coefficients
		if len(coeffs) == 0 {
			coeffs = router.NewDefaultDownmixMatrix(inCh, outCh).Coefficients
		}

		slewMS := req.SlewMS
		if slewMS <= 0 {
			slewMS = 20.0
		}

		mr := router.MatrixRoute{
			ID:           req.ID,
			Name:         req.Name,
			Source:       req.Source,
			Destination:  req.Destination,
			InChannels:   inCh,
			OutChannels:  outCh,
			Coefficients: coeffs,
			SlewMS:       slewMS,
			Muted:        false,
			State:        "active",
		}

		if err := mr.Validate(); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_ROUTE", err.Error())
			return
		}

		if err := s.router.AddMatrix(mr); err != nil {
			s.WriteError(w, http.StatusInternalServerError, "ERR_SAVE_ROUTE", err.Error())
			return
		}

		if s.streams != nil {
			sr := router.MatrixRouteToStreamRoute(mr)
			if err := s.streams.StartRoute(sr); err != nil {
				_ = s.router.DeleteMatrix(mr.ID)
				s.WriteError(w, http.StatusBadRequest, "ERR_STREAM_START", err.Error())
				return
			}
		}

		s.WriteJSON(w, http.StatusCreated, mr)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleRoutesSubpath handles operations targeting /api/v1/routes/{id} and /api/v1/routes/{id}/crosspoint.
func (s *V1Server) handleRoutesSubpath(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		s.WriteError(w, http.StatusInternalServerError, "ERR_ROUTER_NIL", "router store not configured")
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/routes/")
	if sub == "" {
		s.handleRoutes(w, r)
		return
	}

	// Check if this is a crosspoint update: /api/v1/routes/{id}/crosspoint
	if strings.HasSuffix(sub, "/crosspoint") {
		routeID := strings.TrimSuffix(sub, "/crosspoint")
		if r.Method != http.MethodPost && r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CrosspointGainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		slewMS := req.SlewMS
		if slewMS <= 0 {
			slewMS = 20.0
		}
		old, _ := s.router.GetMatrix(routeID)
		mr, err := s.router.UpdateCrosspoint(routeID, req.OutChannel, req.InChannel, req.Gain, slewMS)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
			} else {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_CROSSPOINT", err.Error())
			}
			return
		}
		if err := s.updateLiveMatrix(mr); err != nil {
			s.restoreMatrix(old)
			s.WriteError(w, http.StatusConflict, "ERR_LIVE_ROUTE", err.Error())
			return
		}
		s.WriteJSON(w, http.StatusOK, mr)
		return
	}

	routeID := sub
	switch r.Method {
	case http.MethodGet:
		mr, found := s.router.GetMatrix(routeID)
		if !found {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "route not found")
			return
		}
		s.WriteJSON(w, http.StatusOK, mr)

	case http.MethodPut:
		// Complete replacement
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CreateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}

		old, found := s.router.GetMatrix(routeID)
		if !found {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "route not found")
			return
		}

		inCh := len(req.Source.Channels)
		if inCh == 0 {
			inCh = 2
			req.Source.Channels = []int{0, 1}
		}
		outCh := len(req.Destination.Channels)
		if outCh == 0 {
			outCh = 2
			req.Destination.Channels = []int{0, 1}
		}
		coeffs := req.Coefficients
		if len(coeffs) == 0 {
			coeffs = router.NewDefaultDownmixMatrix(inCh, outCh).Coefficients
		}
		slewMS := req.SlewMS
		if slewMS <= 0 {
			slewMS = 20.0
		}

		mr := router.MatrixRoute{
			ID:           routeID,
			Name:         req.Name,
			Source:       req.Source,
			Destination:  req.Destination,
			InChannels:   inCh,
			OutChannels:  outCh,
			Coefficients: coeffs,
			SlewMS:       slewMS,
			Muted:        false,
			State:        "active",
		}
		if err := mr.Validate(); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_ROUTE", err.Error())
			return
		}
		if err := s.router.AddMatrix(mr); err != nil {
			s.WriteError(w, http.StatusInternalServerError, "ERR_SAVE_ROUTE", err.Error())
			return
		}
		if s.streams != nil {
			if err := s.streams.StartRoute(router.MatrixRouteToStreamRoute(mr)); err != nil {
				_ = s.router.AddMatrix(old)
				s.WriteError(w, http.StatusConflict, "ERR_STREAM_START", err.Error())
				return
			}
		}
		s.WriteJSON(w, http.StatusOK, mr)

	case http.MethodPatch:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req PatchRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		old, _ := s.router.GetMatrix(routeID)
		mr, err := s.router.PatchMatrix(routeID, req.Name, req.Coefficients, req.SlewMS, req.Muted)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
			} else {
				s.WriteError(w, http.StatusBadRequest, "ERR_PATCH_ROUTE", err.Error())
			}
			return
		}
		if err := s.updateLiveMatrix(mr); err != nil {
			s.restoreMatrix(old)
			s.WriteError(w, http.StatusConflict, "ERR_LIVE_ROUTE", err.Error())
			return
		}
		s.WriteJSON(w, http.StatusOK, mr)

	case http.MethodDelete:
		if _, found := s.router.GetMatrix(routeID); !found {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "route not found")
			return
		}
		if s.streams != nil {
			_ = s.streams.StopRoute(routeID)
		}
		if err := s.router.DeleteMatrix(routeID); err != nil {
			s.WriteError(w, http.StatusInternalServerError, "ERR_DELETE_ROUTE", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
