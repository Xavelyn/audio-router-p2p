package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/sync"
)

// v1RoomManagerAdapter adapts api.RoomManager to v1.RoomManager dynamically.
type v1RoomManagerAdapter struct {
	s *Server
}

func (a *v1RoomManagerAdapter) ListRooms() []v1.RoomInfo {
	rm := a.s.getRoomManager()
	if rm == nil {
		return nil
	}
	rooms := rm.ListRooms()
	res := make([]v1.RoomInfo, len(rooms))
	for i, r := range rooms {
		res[i] = v1.RoomInfo{
			ID:      r.ID,
			Name:    r.Name,
			Leader:  r.Leader,
			State:   r.State,
			Members: r.Members,
		}
	}
	return res
}

func (a *v1RoomManagerAdapter) CreateRoom(id, name string) (v1.RoomInfo, error) {
	rm := a.s.getRoomManager()
	if rm == nil {
		return v1.RoomInfo{}, fmt.Errorf("room manager not configured")
	}
	r, err := rm.CreateRoom(id, name)
	if err != nil {
		return v1.RoomInfo{}, err
	}
	return v1.RoomInfo{
		ID:      r.ID,
		Name:    r.Name,
		Leader:  r.Leader,
		State:   r.State,
		Members: r.Members,
	}, nil
}

func (a *v1RoomManagerAdapter) JoinRoom(id string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.JoinRoom(id)
}

func (a *v1RoomManagerAdapter) LeaveRoom(id string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.LeaveRoom(id)
}

// registerLegacyRoutes binds all backward-compatible unversioned endpoints.
func (s *Server) registerLegacyRoutes() {
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/devices", s.handleDevices)
	s.mux.HandleFunc("/peers", s.handlePeers)
	s.mux.HandleFunc("/sync", s.handleSync)
	s.mux.HandleFunc("/routes", s.handleRoutes)
	s.mux.HandleFunc("/streams/stats", s.handleStreamStats)
	s.mux.HandleFunc("/rooms", s.handleRooms)
	s.mux.HandleFunc("/rooms/", s.handleRoomsSubpath)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var syncInfo map[string]sync.PeerOffset
	if s.syncMgr != nil {
		syncInfo = s.syncMgr.Offsets()
	}
	active := s.rt.List()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "ok",
		"nodeID":      s.disc.NodeID(),
		"addr":        s.disc.OwnAddr(),
		"peerCount":   len(s.disc.Peers()),
		"routeCount":  len(active),
		"syncPeers":   len(syncInfo),
		"uptimeNanos": time.Now().UnixNano(),
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	devs, err := audio.ListDevices()
	if err != nil {
		log.Printf("api: list devices: %v", err)
		devs = []audio.Device{}
	}
	writeJSON(w, http.StatusOK, devs)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	peers := s.disc.Peers()
	s.peerCnt.Set(float64(len(peers)))
	writeJSON(w, http.StatusOK, peers)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var offsets map[string]sync.PeerOffset
	if s.syncMgr != nil {
		offsets = s.syncMgr.Offsets()
	}
	writeJSON(w, http.StatusOK, offsets)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes := s.rt.List()
		s.routesActive.Set(float64(len(routes)))
		writeJSON(w, http.StatusOK, routes)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var sr router.StreamRoute
		if err := json.NewDecoder(r.Body).Decode(&sr); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if sr.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "route id is required"})
			return
		}
		if err := s.rt.Add(sr); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if s.streams != nil {
			if err := s.streams.StartRoute(sr); err != nil {
				_ = s.rt.Delete(sr.ID)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusCreated, sr)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
			return
		}
		if s.streams != nil {
			if err := s.streams.StopRoute(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := s.rt.Delete(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStreamStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.streams == nil {
		writeJSON(w, http.StatusOK, map[string]streamer.StreamerStats{})
		return
	}
	writeJSON(w, http.StatusOK, s.streams.Stats())
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	roomMgr := s.getRoomManager()
	switch r.Method {
	case http.MethodGet:
		if roomMgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "room manager not configured"})
			return
		}
		rooms := roomMgr.ListRooms()
		if rooms == nil {
			rooms = []RoomInfo{}
		}
		writeJSON(w, http.StatusOK, rooms)

	case http.MethodPost:
		if roomMgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "room manager not configured"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		room, err := roomMgr.CreateRoom(req.ID, req.Name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, room)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRoomsSubpath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	roomMgr := s.getRoomManager()
	if roomMgr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "room manager not configured"})
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/rooms/")
	var isJoin, isLeave bool
	var roomID string

	if sub == "join" || strings.HasSuffix(sub, "/join") {
		isJoin = true
		roomID = strings.TrimSuffix(sub, "/join")
		if roomID == "join" {
			roomID = ""
		}
	} else if sub == "leave" || strings.HasSuffix(sub, "/leave") {
		isLeave = true
		roomID = strings.TrimSuffix(sub, "/leave")
		if roomID == "leave" {
			roomID = ""
		}
	} else {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if roomID == "" {
		roomID = r.URL.Query().Get("id")
	}
	if roomID == "" && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			ID     string `json:"id"`
			RoomID string `json:"roomId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID != "" {
			roomID = req.ID
		} else {
			roomID = req.RoomID
		}
	}

	if roomID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "room id is required"})
		return
	}

	if isJoin {
		if err := roomMgr.JoinRoom(roomID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "joined", "id": roomID})
		return
	}

	if isLeave {
		if err := roomMgr.LeaveRoom(roomID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "left", "id": roomID})
		return
	}
}
