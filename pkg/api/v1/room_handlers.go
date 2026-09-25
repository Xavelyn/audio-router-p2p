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

// handleRoomsSubpath handles /api/v1/rooms/{id}, /api/v1/rooms/{id}/join, and /api/v1/rooms/{id}/leave.
func (s *V1Server) handleRoomsSubpath(w http.ResponseWriter, r *http.Request) {
	if s.roomMgr == nil {
		s.WriteError(w, http.StatusNotFound, "ERR_NO_ROOM_MANAGER", "room manager not configured")
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/rooms/")
	if sub == "" {
		s.handleRooms(w, r)
		return
	}

	// Route 1: POST /api/v1/rooms/{id}/join
	if strings.HasSuffix(sub, "/join") {
		roomID := strings.TrimSuffix(sub, "/join")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.roomMgr.JoinRoom(roomID); err != nil {
			s.WriteError(w, http.StatusNotFound, "ERR_JOIN_FAILED", err.Error())
			return
		}
		// Return room details after join
		room := s.findRoom(roomID)
		s.WriteJSON(w, http.StatusOK, room)
		return
	}

	// Route 2: POST /api/v1/rooms/{id}/leave
	if strings.HasSuffix(sub, "/leave") {
		roomID := strings.TrimSuffix(sub, "/leave")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.roomMgr.LeaveRoom(roomID); err != nil {
			s.WriteError(w, http.StatusNotFound, "ERR_LEAVE_FAILED", err.Error())
			return
		}
		room := s.findRoom(roomID)
		s.WriteJSON(w, http.StatusOK, room)
		return
	}

	// Route 3: GET or DELETE /api/v1/rooms/{id}
	roomID := sub
	switch r.Method {
	case http.MethodGet:
		room := s.findRoom(roomID)
		if room.ID == "" {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "room not found")
			return
		}
		s.WriteJSON(w, http.StatusOK, room)

	case http.MethodDelete:
		// Disband/leave room
		if err := s.roomMgr.LeaveRoom(roomID); err != nil {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
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
