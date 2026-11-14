package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/streamer"
)

// AmbiguousEndpointError indicates that an unqualified device reference matches multiple cluster nodes.
type AmbiguousEndpointError struct {
	DeviceID string
	Owners   []string
}

func (e *AmbiguousEndpointError) Error() string {
	return fmt.Sprintf("device URN %q exists on multiple nodes (%v); explicit node_id required", e.DeviceID, e.Owners)
}

// normalizeEndpoint resolves an incoming device reference to canonical "<node_id>/<device_id>".
func (s *V1Server) normalizeEndpoint(deviceID, explicitNodeID string) (string, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return "", fmt.Errorf("device_id is required")
	}

	// 1. Explicit canonical prefix: "<node_id>/<device_id>"
	if slash := strings.Index(deviceID, "/"); slash > 0 {
		return deviceID, nil
	}

	// 2. RFC 8141 node URN format: "urn:audio:node:<nodeID>:dev:<devID>"
	const nodePrefix = "urn:audio:node:"
	if strings.HasPrefix(deviceID, nodePrefix) {
		remainder := strings.TrimPrefix(deviceID, nodePrefix)
		if idx := strings.Index(remainder, ":dev:"); idx > 0 {
			return remainder[:idx] + "/" + remainder[idx+5:], nil
		}
	}

	// 3. Explicit node parameter supplied by client
	if explicitNodeID != "" {
		return explicitNodeID + "/" + deviceID, nil
	}

	localNodeID := "local"
	if s.discovery != nil && s.discovery.NodeID() != "" {
		localNodeID = s.discovery.NodeID()
	}

	// 4. Cluster inventory consensus lookup
	if s.clusterMgr != nil {
		snap := s.clusterMgr.GetSnapshot()
		if snap != nil {
			var owners []string
			seenOwners := make(map[string]bool)
			for _, cd := range snap.ClusterDevices {
				if cd.URN == deviceID || cd.ID == deviceID || cd.DeviceID == deviceID {
					if cd.NodeID != "" && !seenOwners[cd.NodeID] {
						seenOwners[cd.NodeID] = true
						owners = append(owners, cd.NodeID)
					}
				}
			}
			sort.Strings(owners)
			if len(owners) == 1 {
				return owners[0] + "/" + deviceID, nil
			}
			if len(owners) > 1 && explicitNodeID == "" {
				return "", &AmbiguousEndpointError{
					DeviceID: deviceID,
					Owners:   owners,
				}
			}
		}
	}

	// 5. Check if device exists in local resolver (s.devReg or audio.GlobalResolver())
	if s.devReg != nil {
		if _, ok := s.devReg.GetDevice(deviceID); ok {
			return localNodeID + "/" + deviceID, nil
		}
	}
	if gr := audio.GlobalResolver(); gr != nil {
		if _, ok := gr.Resolve(deviceID); ok {
			return localNodeID + "/" + deviceID, nil
		}
	}

	// 6. Fallback to localNodeID + "/" + deviceID
	return localNodeID + "/" + deviceID, nil
}

// handleRooms handles GET /api/v1/rooms and POST /api/v1/rooms.
func (s *V1Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	if s.roomMgr == nil {
		s.WriteError(w, http.StatusNotFound, "ERR_NO_ROOM_MANAGER", "room manager not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		rooms := s.roomMgr.ListRooms()
		if rooms == nil {
			rooms = []RoomInfo{}
		}
		s.WriteJSON(w, http.StatusOK, rooms)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CreateRoomRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		if req.ID == "" || req.Name == "" {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "id and name are required")
			return
		}
		room, err := s.roomMgr.CreateRoom(req.ID, req.Name)
		if err != nil {
			s.WriteError(w, http.StatusInternalServerError, "ERR_CREATE_ROOM", err.Error())
			return
		}
		s.WriteJSON(w, http.StatusCreated, room)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleRoomsSubpath handles /api/v1/rooms/{id} subpaths:
// - GET/DELETE /api/v1/rooms/{id}
// - POST /api/v1/rooms/{id}/join
// - POST /api/v1/rooms/{id}/leave
// - PUT /api/v1/rooms/{id}/bindings
// - PUT /api/v1/rooms/{id}/latency
// - POST /api/v1/rooms/{id}/inputs
// - DELETE /api/v1/rooms/{id}/inputs/{devId}
// - POST /api/v1/rooms/{id}/outputs
// - DELETE /api/v1/rooms/{id}/outputs/{devId}
// - POST /api/v1/rooms/{id}/sync
func (s *V1Server) handleRoomsSubpath(w http.ResponseWriter, r *http.Request) {
	if s.roomMgr == nil {
		s.WriteError(w, http.StatusNotFound, "ERR_NO_ROOM_MANAGER", "room manager not configured")
		return
	}

	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/rooms/"), "/")
	if sub == "" {
		s.handleRooms(w, r)
		return
	}

	parts := strings.Split(sub, "/")

	// 1 segment: /api/v1/rooms/{id}
	if len(parts) == 1 {
		roomID := parts[0]
		switch r.Method {
		case http.MethodGet:
			room := s.findRoom(roomID)
			if room.ID == "" {
				s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "room not found")
				return
			}
			s.WriteJSON(w, http.StatusOK, room)

		case http.MethodDelete:
			room := s.findRoom(roomID)
			if room.ID == "" {
				s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "room not found")
				return
			}
			if err := s.roomMgr.DeleteRoom(roomID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_DELETE_FAILED", err.Error())
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	// 2 segments: /api/v1/rooms/{id}/{action}
	if len(parts) == 2 {
		roomID := parts[0]
		action := parts[1]

		room := s.findRoom(roomID)
		if room.ID == "" {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "room not found")
			return
		}

		switch action {
		case "join":
			w.Header().Set("Allow", http.MethodPost)
			w.Header().Set("Warning", `299 - "POST /api/v1/rooms/{id}/join is deprecated and will be removed in v2.0.0; audio routes are automatically compiled via bindings."`)
			w.Header().Set("Deprecation", "@1759449600")
			w.Header().Set("Sunset", "Fri, 01 Jan 2027 00:00:00 GMT")
			w.Header().Set("Link", `</api/v1/docs/adr/0022>; rel="deprecation"`)
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			log.Printf("[DEPRECATION] Client invoked legacy JoinRoom endpoint: room_id=%s remote_addr=%s. Migrate to declarative Audio Flow routing.", roomID, r.RemoteAddr)
			if err := s.roomMgr.JoinRoom(roomID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_JOIN_FAILED", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "leave":
			w.Header().Set("Allow", http.MethodPost)
			w.Header().Set("Warning", `299 - "POST /api/v1/rooms/{id}/leave is deprecated and will be removed in v2.0.0; audio routes are automatically compiled via bindings."`)
			w.Header().Set("Deprecation", "@1759449600")
			w.Header().Set("Sunset", "Fri, 01 Jan 2027 00:00:00 GMT")
			w.Header().Set("Link", `</api/v1/docs/adr/0022>; rel="deprecation"`)
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			log.Printf("[DEPRECATION] Client invoked legacy LeaveRoom endpoint: room_id=%s remote_addr=%s. Migrate to declarative Audio Flow routing.", roomID, r.RemoteAddr)
			if err := s.roomMgr.LeaveRoom(roomID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_LEAVE_FAILED", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "bindings":
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			var req UpdateRoomBindingsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
				return
			}
			synced := room.Synced
			if req.Synced != nil {
				synced = *req.Synced
			}
			if req.Inputs == nil {
				req.Inputs = []string{}
			}
			if req.Outputs == nil {
				req.Outputs = []string{}
			}

			normInputs := make([]string, len(req.Inputs))
			for i, inDev := range req.Inputs {
				norm, err := s.normalizeEndpoint(inDev, "")
				if err != nil {
					var ambigErr *AmbiguousEndpointError
					if errors.As(err, &ambigErr) || strings.Contains(err.Error(), "exists on multiple nodes") {
						s.WriteError(w, http.StatusUnprocessableEntity, "ERR_AMBIGUOUS_ENDPOINT", err.Error())
						return
					}
					s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", err.Error())
					return
				}
				normInputs[i] = norm
			}

			normOutputs := make([]string, len(req.Outputs))
			for i, outDev := range req.Outputs {
				norm, err := s.normalizeEndpoint(outDev, "")
				if err != nil {
					var ambigErr *AmbiguousEndpointError
					if errors.As(err, &ambigErr) || strings.Contains(err.Error(), "exists on multiple nodes") {
						s.WriteError(w, http.StatusUnprocessableEntity, "ERR_AMBIGUOUS_ENDPOINT", err.Error())
						return
					}
					s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", err.Error())
					return
				}
				normOutputs[i] = norm
			}

			if err := s.roomMgr.SetRoomBindings(roomID, normInputs, normOutputs, synced); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_SET_BINDINGS", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "inputs":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			var req RoomDeviceBindingRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
				return
			}
			if req.DeviceID == "" {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "device_id is required")
				return
			}
			bindID, err := s.normalizeEndpoint(req.DeviceID, req.NodeID)
			if err != nil {
				var ambigErr *AmbiguousEndpointError
				if errors.As(err, &ambigErr) || strings.Contains(err.Error(), "exists on multiple nodes") {
					s.WriteError(w, http.StatusUnprocessableEntity, "ERR_AMBIGUOUS_ENDPOINT", err.Error())
					return
				}
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", err.Error())
				return
			}
			if err := s.roomMgr.AddInput(roomID, bindID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_ADD_INPUT", err.Error())
				return
			}
			if req.Gain != nil {
				if gSetter, ok := s.roomMgr.(interface {
					SetInputGain(string, string, float64) error
				}); ok {
					_ = gSetter.SetInputGain(roomID, bindID, *req.Gain)
				}
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "outputs":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			var req RoomDeviceBindingRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
				return
			}
			if req.DeviceID == "" {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "device_id is required")
				return
			}
			bindID, err := s.normalizeEndpoint(req.DeviceID, req.NodeID)
			if err != nil {
				var ambigErr *AmbiguousEndpointError
				if errors.As(err, &ambigErr) || strings.Contains(err.Error(), "exists on multiple nodes") {
					s.WriteError(w, http.StatusUnprocessableEntity, "ERR_AMBIGUOUS_ENDPOINT", err.Error())
					return
				}
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", err.Error())
				return
			}
			if err := s.roomMgr.AddOutput(roomID, bindID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_ADD_OUTPUT", err.Error())
				return
			}
			if req.Gain != nil {
				if gSetter, ok := s.roomMgr.(interface {
					SetOutputGain(string, string, float64) error
				}); ok {
					_ = gSetter.SetOutputGain(roomID, bindID, *req.Gain)
				}
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "latency":
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Body == nil {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", "missing request body")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			var req UpdateRoomLatencyRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
				return
			}
			if !streamer.LatencyPreset(req.Preset).Valid() || req.Preset == "" {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS",
					fmt.Sprintf("unknown preset %q (want one of %s)", req.Preset,
						strings.Join(streamer.ValidPresets(), ", ")))
				return
			}
			if err := s.roomMgr.SetRoomLatencyPreset(roomID, req.Preset); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_SET_LATENCY", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "sync":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			var req SetRoomSyncRequest
			newSynced := !room.Synced
			if r.Body != nil {
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Synced != nil {
					newSynced = *req.Synced
				}
			}
			if err := s.roomMgr.SetRoomSync(roomID, newSynced); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_SET_SYNC", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		default:
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "endpoint not found")
		}
		return
	}

	// 3+ segments: /api/v1/rooms/{id}/{category}/{devId...}
	if len(parts) >= 3 {
		roomID := parts[0]
		category := parts[1]
		devID := strings.Join(parts[2:], "/")

		room := s.findRoom(roomID)
		if room.ID == "" {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "room not found")
			return
		}

		switch category {
		case "inputs":
			if r.Method != http.MethodDelete {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if devID == "" {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "device id required")
				return
			}
			targetDevID := devID
			hasExact := false
			for _, in := range room.Inputs {
				if in == devID || strings.EqualFold(in, devID) || strings.HasSuffix(strings.ToLower(in), "/"+strings.ToLower(devID)) {
					targetDevID = in
					hasExact = true
					break
				}
			}
			if !hasExact {
				if normDevID, err := s.normalizeEndpoint(devID, ""); err == nil && normDevID != "" {
					targetDevID = normDevID
				}
			}

			if err := s.roomMgr.RemoveInput(roomID, targetDevID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_REMOVE_INPUT", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "outputs":
			if r.Method != http.MethodDelete {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if devID == "" {
				s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "device id required")
				return
			}
			targetDevID := devID
			hasExact := false
			for _, out := range room.Outputs {
				if out == devID || strings.EqualFold(out, devID) || strings.HasSuffix(strings.ToLower(out), "/"+strings.ToLower(devID)) {
					targetDevID = out
					hasExact = true
					break
				}
			}
			if !hasExact {
				if normDevID, err := s.normalizeEndpoint(devID, ""); err == nil && normDevID != "" {
					targetDevID = normDevID
				}
			}

			if err := s.roomMgr.RemoveOutput(roomID, targetDevID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_REMOVE_OUTPUT", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		default:
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "endpoint not found")
		}
		return
	}

	s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "endpoint not found")
}

func (s *V1Server) findRoom(id string) RoomInfo {
	if s.roomMgr == nil {
		return RoomInfo{}
	}
	rooms := s.roomMgr.ListRooms()
	for _, r := range rooms {
		if r.ID == id {
			return r
		}
	}
	return RoomInfo{}
}
