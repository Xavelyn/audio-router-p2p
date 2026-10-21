package v1

import (
	"encoding/json"
	"net/http"
	"strings"
)

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
			if err := s.roomMgr.LeaveRoom(roomID); err != nil {
				s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
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
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if err := s.roomMgr.JoinRoom(roomID); err != nil {
				s.WriteError(w, http.StatusNotFound, "ERR_JOIN_FAILED", err.Error())
				return
			}
			updated := s.findRoom(roomID)
			s.WriteJSON(w, http.StatusOK, updated)

		case "leave":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if err := s.roomMgr.LeaveRoom(roomID); err != nil {
				s.WriteError(w, http.StatusNotFound, "ERR_LEAVE_FAILED", err.Error())
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
			if err := s.roomMgr.SetRoomBindings(roomID, req.Inputs, req.Outputs, synced); err != nil {
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
			if err := s.roomMgr.AddInput(roomID, req.DeviceID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_ADD_INPUT", err.Error())
				return
			}
			if req.Gain != nil {
				if gSetter, ok := s.roomMgr.(interface {
					SetInputGain(string, string, float64) error
				}); ok {
					_ = gSetter.SetInputGain(roomID, req.DeviceID, *req.Gain)
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
			if err := s.roomMgr.AddOutput(roomID, req.DeviceID); err != nil {
				s.WriteError(w, http.StatusInternalServerError, "ERR_ADD_OUTPUT", err.Error())
				return
			}
			if req.Gain != nil {
				if gSetter, ok := s.roomMgr.(interface {
					SetOutputGain(string, string, float64) error
				}); ok {
					_ = gSetter.SetOutputGain(roomID, req.DeviceID, *req.Gain)
				}
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

	// 3 segments: /api/v1/rooms/{id}/{category}/{devId}
	if len(parts) == 3 {
		roomID := parts[0]
		category := parts[1]
		devID := parts[2]

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
			if err := s.roomMgr.RemoveInput(roomID, devID); err != nil {
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
			if err := s.roomMgr.RemoveOutput(roomID, devID); err != nil {
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
