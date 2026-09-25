package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
)

type mockV1RoomManager struct {
	mu    sync.Mutex
	rooms map[string]RoomInfo
}

func newMockV1RoomManager() *mockV1RoomManager {
	return &mockV1RoomManager{
		rooms: make(map[string]RoomInfo),
	}
}

func (m *mockV1RoomManager) ListRooms() []RoomInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]RoomInfo, 0, len(m.rooms))
	for _, r := range m.rooms {
		res = append(res, r)
	}
	return res
}

func (m *mockV1RoomManager) CreateRoom(id, name string) (RoomInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := RoomInfo{
		ID:      id,
		Name:    name,
		State:   "active",
		Members: []string{"local"},
	}
	m.rooms[id] = r
	return r, nil
}

func (m *mockV1RoomManager) JoinRoom(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	if !ok {
		return errors.New("room not found")
	}
	r.Members = append(r.Members, "joined-node")
	m.rooms[id] = r
	return nil
}

func (m *mockV1RoomManager) LeaveRoom(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rooms[id]; !ok {
		return errors.New("room not found")
	}
	delete(m.rooms, id)
	return nil
}

func TestV1RoomsCRUD(t *testing.T) {
	s, mux := newTestV1Server(t)
	mockRM := newMockV1RoomManager()
	s.roomMgr = mockRM

	// 1. Initial list rooms
	w := doV1Request(mux, http.MethodGet, "/api/v1/rooms", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var rooms []RoomInfo
	if err := json.Unmarshal(w.Body.Bytes(), &rooms); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(rooms) != 0 {
		t.Errorf("expected 0 rooms, got %d", len(rooms))
	}

	// 2. Create room
	createReq := CreateRoomRequest{
		ID:   "studio-room-a",
		Name: "Live Stage A",
	}
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms", createReq, "Bearer test-v1-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created RoomInfo
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.ID != "studio-room-a" {
		t.Errorf("room ID mismatch: %s", created.ID)
	}

	// 3. GET /api/v1/rooms/{id}
	w = doV1Request(mux, http.MethodGet, "/api/v1/rooms/studio-room-a", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// 4. POST /api/v1/rooms/{id}/join
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/studio-room-a/join", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for join, got %d: %s", w.Code, w.Body.String())
	}

	// 5. POST /api/v1/rooms/{id}/leave
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/studio-room-a/leave", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for leave, got %d: %s", w.Code, w.Body.String())
	}

	// Verify 404 after room removed via leave
	w = doV1Request(mux, http.MethodGet, "/api/v1/rooms/studio-room-a", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after leave/deletion, got %d", w.Code)
	}
}
