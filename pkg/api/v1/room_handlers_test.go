package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/audio-router-p2p/pkg/streamer"
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

func (m *mockV1RoomManager) AddInput(roomID, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	for _, id := range r.Inputs {
		if id == deviceID {
			return nil
		}
	}
	r.Inputs = append(r.Inputs, deviceID)
	m.rooms[roomID] = r
	return nil
}

func (m *mockV1RoomManager) RemoveInput(roomID, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	idx := -1
	for i, id := range r.Inputs {
		if id == deviceID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		r.Inputs = append(r.Inputs[:idx], r.Inputs[idx+1:]...)
		if r.InputGains != nil {
			delete(r.InputGains, deviceID)
		}
		m.rooms[roomID] = r
	}
	return nil
}

func (m *mockV1RoomManager) AddOutput(roomID, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	for _, id := range r.Outputs {
		if id == deviceID {
			return nil
		}
	}
	r.Outputs = append(r.Outputs, deviceID)
	m.rooms[roomID] = r
	return nil
}

func (m *mockV1RoomManager) RemoveOutput(roomID, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	idx := -1
	for i, id := range r.Outputs {
		if id == deviceID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		r.Outputs = append(r.Outputs[:idx], r.Outputs[idx+1:]...)
		if r.OutputGains != nil {
			delete(r.OutputGains, deviceID)
		}
		m.rooms[roomID] = r
	}
	return nil
}

func (m *mockV1RoomManager) SetRoomSync(roomID string, synced bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	r.Synced = synced
	m.rooms[roomID] = r
	return nil
}

func (m *mockV1RoomManager) SetRoomBindings(roomID string, inputs, outputs []string, synced bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	r.Inputs = make([]string, len(inputs))
	copy(r.Inputs, inputs)
	r.Outputs = make([]string, len(outputs))
	copy(r.Outputs, outputs)
	r.Synced = synced
	m.rooms[roomID] = r
	return nil
}

func (m *mockV1RoomManager) SetRoomLatencyPreset(roomID string, preset string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	if !streamer.LatencyPreset(preset).Valid() || preset == "" {
		return fmt.Errorf("unknown latency preset %q", preset)
	}
	r.LatencyPreset = preset
	m.rooms[roomID] = r
	return nil
}

func (m *mockV1RoomManager) SetInputGain(roomID, deviceID string, gain float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	if r.InputGains == nil {
		r.InputGains = make(map[string]float64)
	}
	r.InputGains[deviceID] = gain
	m.rooms[roomID] = r
	return nil
}

func (m *mockV1RoomManager) SetOutputGain(roomID, deviceID string, gain float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return errors.New("room not found")
	}
	if r.OutputGains == nil {
		r.OutputGains = make(map[string]float64)
	}
	r.OutputGains[deviceID] = gain
	m.rooms[roomID] = r
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

func TestV1RoomBindingsAndSync(t *testing.T) {
	s, mux := newTestV1Server(t)
	mockRM := newMockV1RoomManager()
	s.roomMgr = mockRM

	// 1. Create a test room
	_, err := mockRM.CreateRoom("room-bus-1", "Bus Room 1")
	if err != nil {
		t.Fatalf("create room: %v", err)
	}

	// 2. POST /api/v1/rooms/{id}/inputs (add input-1 with gain 1.2)
	gainVal := 1.2
	w := doV1Request(mux, http.MethodPost, "/api/v1/rooms/room-bus-1/inputs", RoomDeviceBindingRequest{
		DeviceID: "mic-1",
		Gain:     &gainVal,
	}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for add input, got %d: %s", w.Code, w.Body.String())
	}
	var room RoomInfo
	if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(room.Inputs) != 1 || room.Inputs[0] != "mic-1" {
		t.Fatalf("expected input 'mic-1', got %+v", room.Inputs)
	}

	// Add second input
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/room-bus-1/inputs", RoomDeviceBindingRequest{
		DeviceID: "mic-2",
	}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for add input 2, got %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if len(room.Inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(room.Inputs))
	}

	// 3. POST /api/v1/rooms/{id}/outputs (add spk-1 with gain 0.8)
	outGainVal := 0.8
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/room-bus-1/outputs", RoomDeviceBindingRequest{
		DeviceID: "spk-1",
		Gain:     &outGainVal,
	}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for add output, got %d: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if len(room.Outputs) != 1 || room.Outputs[0] != "spk-1" {
		t.Fatalf("expected output 'spk-1', got %+v", room.Outputs)
	}

	// 4. DELETE /api/v1/rooms/{id}/inputs/{devId} (remove mic-1)
	w = doV1Request(mux, http.MethodDelete, "/api/v1/rooms/room-bus-1/inputs/mic-1", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delete input, got %d: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if len(room.Inputs) != 1 || room.Inputs[0] != "mic-2" {
		t.Fatalf("expected input mic-2 remaining, got %+v", room.Inputs)
	}

	// 5. DELETE /api/v1/rooms/{id}/outputs/{devId} (remove spk-1)
	w = doV1Request(mux, http.MethodDelete, "/api/v1/rooms/room-bus-1/outputs/spk-1", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for delete output, got %d: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if len(room.Outputs) != 0 {
		t.Fatalf("expected 0 outputs remaining, got %+v", room.Outputs)
	}

	// 6. PUT /api/v1/rooms/{id}/bindings (bulk update)
	syncedTrue := true
	w = doV1Request(mux, http.MethodPut, "/api/v1/rooms/room-bus-1/bindings", UpdateRoomBindingsRequest{
		Inputs:  []string{"stream-in-1", "stream-in-2"},
		Outputs: []string{"dac-out-1", "dac-out-2"},
		Synced:  &syncedTrue,
	}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for bulk bindings, got %d: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if len(room.Inputs) != 2 || len(room.Outputs) != 2 || !room.Synced {
		t.Fatalf("unexpected room state after bulk bindings: %+v", room)
	}

	// 7. POST /api/v1/rooms/{id}/sync (toggle or explicit set)
	syncedFalse := false
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/room-bus-1/sync", SetRoomSyncRequest{
		Synced: &syncedFalse,
	}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for set sync false, got %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if room.Synced {
		t.Fatal("expected room to be unsynced")
	}

	// Toggle sync with empty body
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/room-bus-1/sync", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for toggle sync, got %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &room)
	if !room.Synced {
		t.Fatal("expected room to be toggled to synced")
	}
}

func TestV1RoomErrorCases(t *testing.T) {
	s, mux := newTestV1Server(t)
	mockRM := newMockV1RoomManager()
	s.roomMgr = mockRM

	// 1. Non-existent room operations should 404
	w := doV1Request(mux, http.MethodPost, "/api/v1/rooms/non-existent/inputs", RoomDeviceBindingRequest{DeviceID: "mic-1"}, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for input on non-existent room, got %d", w.Code)
	}

	w = doV1Request(mux, http.MethodDelete, "/api/v1/rooms/non-existent/inputs/mic-1", nil, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for delete input on non-existent room, got %d", w.Code)
	}

	w = doV1Request(mux, http.MethodPut, "/api/v1/rooms/non-existent/bindings", UpdateRoomBindingsRequest{}, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for bindings on non-existent room, got %d", w.Code)
	}

	// 2. Missing device_id should 400
	_, _ = mockRM.CreateRoom("valid-room", "Valid Room")
	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/valid-room/inputs", RoomDeviceBindingRequest{DeviceID: ""}, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty device_id, got %d", w.Code)
	}

	w = doV1Request(mux, http.MethodPost, "/api/v1/rooms/valid-room/outputs", RoomDeviceBindingRequest{DeviceID: ""}, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty output device_id, got %d", w.Code)
	}
}

func TestV1RoomLatencyPreset(t *testing.T) {
	s, mux := newTestV1Server(t)
	mockRM := newMockV1RoomManager()
	s.roomMgr = mockRM

	if _, err := mockRM.CreateRoom("lat-room", "Latency Room"); err != nil {
		t.Fatalf("create room: %v", err)
	}

	// 1. PUT a valid preset.
	w := doV1Request(mux, http.MethodPut, "/api/v1/rooms/lat-room/latency",
		UpdateRoomLatencyRequest{Preset: "low"}, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for PUT latency, got %d: %s", w.Code, w.Body.String())
	}
	var room RoomInfo
	if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
		t.Fatalf("unmarshal room: %v", err)
	}
	if room.LatencyPreset != "low" {
		t.Errorf("expected latency_preset low, got %q", room.LatencyPreset)
	}

	// 2. GET reflects the stored preset.
	w = doV1Request(mux, http.MethodGet, "/api/v1/rooms/lat-room", nil, "Bearer test-v1-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET room, got %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
		t.Fatalf("unmarshal room: %v", err)
	}
	if room.LatencyPreset != "low" {
		t.Errorf("GET does not reflect preset: %q", room.LatencyPreset)
	}

	// 3. Unknown preset -> 400.
	w = doV1Request(mux, http.MethodPut, "/api/v1/rooms/lat-room/latency",
		UpdateRoomLatencyRequest{Preset: "ultra"}, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown preset, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Empty preset -> 400.
	w = doV1Request(mux, http.MethodPut, "/api/v1/rooms/lat-room/latency",
		UpdateRoomLatencyRequest{}, "Bearer test-v1-token")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty preset, got %d", w.Code)
	}

	// 5. Non-PUT method -> 405.
	w = doV1Request(mux, http.MethodGet, "/api/v1/rooms/lat-room/latency", nil, "Bearer test-v1-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET latency, got %d", w.Code)
	}

	// 6. Non-existent room -> 404.
	w = doV1Request(mux, http.MethodPut, "/api/v1/rooms/ghost/latency",
		UpdateRoomLatencyRequest{Preset: "low"}, "Bearer test-v1-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown room, got %d", w.Code)
	}

	// 7. All accepted presets are stored.
	for _, p := range []string{"balanced", "reliable", "low"} {
		w = doV1Request(mux, http.MethodPut, "/api/v1/rooms/lat-room/latency",
			UpdateRoomLatencyRequest{Preset: p}, "Bearer test-v1-token")
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for preset %s, got %d: %s", p, w.Code, w.Body.String())
			continue
		}
		if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
			t.Fatalf("unmarshal room: %v", err)
		}
		if room.LatencyPreset != p {
			t.Errorf("expected preset %s stored, got %q", p, room.LatencyPreset)
		}
	}
}
