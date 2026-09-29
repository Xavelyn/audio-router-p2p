package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/sync"
	netTelemetry "github.com/audio-router-p2p/pkg/telemetry"
)

type mockStreamManager struct {
	started  map[string]router.StreamRoute
	stopped  []string
	startErr error
	stopErr  error
}

func (m *mockStreamManager) Stats() map[string]streamer.StreamerStats {
	return map[string]streamer.StreamerStats{
		"route-1": {
			StreamID:        "abcd1234abcd1234",
			PacketsSent:     100,
			PacketsReceived: 95,
			FEC: streamer.FECStats{
				PacketsRecoveredTotal: 3,
			},
			PLC: streamer.PLCStats{
				ConcealedFramesTotal: 1,
			},
			Peers: map[string]streamer.JitterBufferStats{
				"192.168.1.50:7001": {
					BufferedFrames: 2,
					CapacityFrames: 10,
					TargetDelay:    20 * time.Millisecond,
					Underruns:      0,
					Overruns:       0,
				},
			},
		},
	}
}

func (m *mockStreamManager) StartRoute(route router.StreamRoute) error {
	if m.startErr != nil {
		return m.startErr
	}
	if m.started == nil {
		m.started = make(map[string]router.StreamRoute)
	}
	m.started[route.ID] = route
	return nil
}

func (m *mockStreamManager) StopRoute(routeID string) error {
	if m.stopErr != nil {
		return m.stopErr
	}
	m.stopped = append(m.stopped, routeID)
	return nil
}

type testMockRoomManager struct {
	rooms    map[string]RoomInfo
	joinErr  error
	leaveErr error
}

func newTestMockRoomManager() *testMockRoomManager {
	return &testMockRoomManager{rooms: make(map[string]RoomInfo)}
}

func (m *testMockRoomManager) ListRooms() []RoomInfo {
	res := make([]RoomInfo, 0, len(m.rooms))
	for _, r := range m.rooms {
		res = append(res, r)
	}
	return res
}

func (m *testMockRoomManager) CreateRoom(id, name string) (RoomInfo, error) {
	if id == "" {
		id = "gen-" + name
	}
	info := RoomInfo{ID: id, Name: name, Leader: "node-1", State: "active"}
	m.rooms[id] = info
	return info, nil
}

func (m *testMockRoomManager) JoinRoom(id string) error {
	if m.joinErr != nil {
		return m.joinErr
	}
	r, ok := m.rooms[id]
	if !ok {
		return fmt.Errorf("room %s not found", id)
	}
	r.Members = append(r.Members, "local-peer")
	m.rooms[id] = r
	return nil
}

func (m *testMockRoomManager) LeaveRoom(id string) error {
	if m.leaveErr != nil {
		return m.leaveErr
	}
	delete(m.rooms, id)
	return nil
}

func (m *testMockRoomManager) AddInput(roomID, deviceID string) error {
	r, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	r.Inputs = append(r.Inputs, deviceID)
	m.rooms[roomID] = r
	return nil
}

func (m *testMockRoomManager) RemoveInput(roomID, deviceID string) error {
	r, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
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
		m.rooms[roomID] = r
	}
	return nil
}

func (m *testMockRoomManager) AddOutput(roomID, deviceID string) error {
	r, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	r.Outputs = append(r.Outputs, deviceID)
	m.rooms[roomID] = r
	return nil
}

func (m *testMockRoomManager) RemoveOutput(roomID, deviceID string) error {
	r, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
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
		m.rooms[roomID] = r
	}
	return nil
}

func (m *testMockRoomManager) SetRoomSync(roomID string, synced bool) error {
	r, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	r.Synced = synced
	m.rooms[roomID] = r
	return nil
}

func (m *testMockRoomManager) SetRoomBindings(roomID string, inputs, outputs []string, synced bool) error {
	r, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	r.Inputs = append([]string(nil), inputs...)
	r.Outputs = append([]string(nil), outputs...)
	r.Synced = synced
	m.rooms[roomID] = r
	return nil
}

func testConfig() *config.Config {
	return &config.Config{
		ListenAddr:       ":8443",
		CertFile:         "cert.pem",
		KeyFile:          "key.pem",
		AuthToken:        "test-token",
		UseEmulator:      true,
		MulticastGroup:   "239.255.0.1",
		MulticastPort:    45400,
		DiscoveryPort:    45401,
		SyncPort:         45402,
		StreamPort:       ":45678",
		AnnounceInterval: 3 * time.Second,
		PeerTTL:          10 * time.Second,
		SyncInterval:     2 * time.Second,
	}
}

func testRouter(t *testing.T) *router.Router {
	r, err := router.New("/tmp/test_routes_" + t.Name() + ".json")
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

func testDiscovery(t *testing.T) *discovery.Discovery {
	cfg := testConfig()
	d, err := discovery.New(cfg, "test-node")
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	return d
}

func testSyncMgr(t *testing.T) *sync.Manager {
	cfg := testConfig()
	disc := testDiscovery(t)
	sm := sync.NewManager(cfg, disc)
	return sm
}

func newTestServer(t *testing.T, streams StreamManager) *Server {
	cfg := testConfig()
	rt := testRouter(t)
	disc := testDiscovery(t)
	sm := testSyncMgr(t)

	return NewServer(cfg, rt, disc, sm, streams)
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("json encode: %v", err)
		}
		req = httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestServerAuthenticationModalities(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// 1. Missing token -> 401
	w := doRequest(t, handler, http.MethodGet, "/status", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", w.Code)
	}

	// 2. Wrong token -> 401
	w = doRequest(t, handler, http.MethodGet, "/status", nil, "wrong-token")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", w.Code)
	}

	// 3. Bearer header -> 200
	w = doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with Bearer token, got %d", w.Code)
	}

	// 4. X-Auth-Token header -> 200
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("X-Auth-Token", "test-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with X-Auth-Token, got %d", w.Code)
	}

	// 5. Cookie auth: audio_session -> 200
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.AddCookie(&http.Cookie{Name: "audio_session", Value: "test-token"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with audio_session cookie, got %d", w.Code)
	}

	// 6. Query parameter token fallback: ?token=... -> 200
	req = httptest.NewRequest(http.MethodGet, "/status?token=test-token", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with ?token= query param, got %d", w.Code)
	}
}

func TestServerStatusAlias(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// GET /status alias forwards to /api/v1/system/status
	w := doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /status, got %d: %s", w.Code, w.Body.String())
	}

	var status v1.SystemStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decode SystemStatus: %v", err)
	}
	if status.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", status.Status)
	}

	// Non-GET method on /status returns 405
	w = doRequest(t, handler, http.MethodPost, "/status", nil, "test-token")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 on POST /status, got %d", w.Code)
	}
}

func TestServerSystemEndpoints(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// GET /api/v1/system/status
	w := doRequest(t, handler, http.MethodGet, "/api/v1/system/status", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/v1/system/status, got %d", w.Code)
	}
	var stat v1.SystemStatus
	if err := json.NewDecoder(w.Body).Decode(&stat); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if stat.Status != "ok" {
		t.Errorf("expected ok status, got %s", stat.Status)
	}

	// GET /api/v1/system/peers
	w = doRequest(t, handler, http.MethodGet, "/api/v1/system/peers", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/v1/system/peers, got %d", w.Code)
	}

	// POST /api/v1/system/peers
	connectReq := v1.ConnectPeerRequest{Addr: "192.168.1.55:7001"}
	w = doRequest(t, handler, http.MethodPost, "/api/v1/system/peers", connectReq, "test-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 from POST /api/v1/system/peers, got %d: %s", w.Code, w.Body.String())
	}

	// GET /api/v1/system/clock-sync
	w = doRequest(t, handler, http.MethodGet, "/api/v1/system/clock-sync", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/v1/system/clock-sync, got %d", w.Code)
	}
}

func TestServerDevicesCRUDAndVirtual(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// 1. GET /api/v1/devices
	w := doRequest(t, handler, http.MethodGet, "/api/v1/devices", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/v1/devices, got %d", w.Code)
	}

	// 2. POST /api/v1/devices/virtual
	createVirt := v1.CreateVirtualDeviceRequest{
		Name:        "Virtual-Loopback-Test",
		Direction:   "duplex",
		Channels:    2,
		SampleRate:  48000,
		BufferSizeFrames: 480,
	}
	w = doRequest(t, handler, http.MethodPost, "/api/v1/devices/virtual", createVirt, "test-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 from POST /api/v1/devices/virtual, got %d: %s", w.Code, w.Body.String())
	}
	var createdDev v1.AudioDevice
	if err := json.NewDecoder(w.Body).Decode(&createdDev); err != nil {
		t.Fatalf("decode created device: %v", err)
	}
	if !createdDev.IsVirtual || createdDev.Name != "Virtual-Loopback-Test" {
		t.Errorf("unexpected virtual device: %+v", createdDev)
	}

	// 3. GET /api/v1/devices/{deviceId}
	w = doRequest(t, handler, http.MethodGet, "/api/v1/devices/"+createdDev.ID, nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/v1/devices/{id}, got %d", w.Code)
	}

	// 4. PATCH /api/v1/devices/{deviceId}
	vol := 0.8
	muted := true
	patchReq := v1.UpdateDeviceRequest{Volume: &vol, Muted: &muted}
	w = doRequest(t, handler, http.MethodPatch, "/api/v1/devices/"+createdDev.ID, patchReq, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from PATCH /api/v1/devices/{id}, got %d: %s", w.Code, w.Body.String())
	}
	var updatedDev v1.AudioDevice
	if err := json.NewDecoder(w.Body).Decode(&updatedDev); err != nil {
		t.Fatalf("decode patched device: %v", err)
	}
	if updatedDev.Volume != 0.8 || !updatedDev.Muted {
		t.Errorf("unexpected volume/muted state: %+v", updatedDev)
	}

	// 5. DELETE /api/v1/devices/virtual/{deviceId}
	w = doRequest(t, handler, http.MethodDelete, "/api/v1/devices/virtual/"+createdDev.ID, nil, "test-token")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 from DELETE /api/v1/devices/virtual/{id}, got %d", w.Code)
	}

	// Verify device is gone
	w = doRequest(t, handler, http.MethodGet, "/api/v1/devices/"+createdDev.ID, nil, "test-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after deletion, got %d", w.Code)
	}
}

func TestServerMatrixRoutingCrosspointAndSlew(t *testing.T) {
	msm := &mockStreamManager{}
	s := newTestServer(t, msm)
	handler := s.Handler()

	// 1. POST /api/v1/routes (Create route)
	createReq := v1.CreateRouteRequest{
		ID:   "matrix-route-test-1",
		Name: "Monitor Output Feed",
		Source: router.MatrixEndpoint{
			NodeID:   "local",
			DeviceID: "0",
			Layout:   router.LayoutStereo,
			Channels: []int{0, 1},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   "local",
			DeviceID: "1",
			Layout:   router.LayoutStereo,
			Channels: []int{0, 1},
		},
		Coefficients: []float64{1.0, 0.0, 0.0, 1.0},
		SlewMS:       20.0,
	}
	w := doRequest(t, handler, http.MethodPost, "/api/v1/routes", createReq, "test-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 from POST /api/v1/routes, got %d: %s", w.Code, w.Body.String())
	}
	var mr router.MatrixRoute
	if err := json.NewDecoder(w.Body).Decode(&mr); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if mr.ID != "matrix-route-test-1" {
		t.Errorf("route ID mismatch: %s", mr.ID)
	}

	// 2. GET /api/v1/routes/{id}
	w = doRequest(t, handler, http.MethodGet, "/api/v1/routes/matrix-route-test-1", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/v1/routes/{id}, got %d", w.Code)
	}

	// 3. POST /api/v1/routes/{id}/crosspoint (Dynamic crosspoint modification with 20ms slew)
	cpReq := v1.CrosspointGainRequest{
		OutChannel: 0,
		InChannel:  1,
		Gain:       0.7071,
		SlewMS:     20.0,
	}
	w = doRequest(t, handler, http.MethodPost, "/api/v1/routes/matrix-route-test-1/crosspoint", cpReq, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from crosspoint update, got %d: %s", w.Code, w.Body.String())
	}
	var cpRoute router.MatrixRoute
	if err := json.NewDecoder(w.Body).Decode(&cpRoute); err != nil {
		t.Fatalf("decode crosspoint route: %v", err)
	}
	if cpRoute.Coefficients[1] != 0.7071 || cpRoute.State != "slewing" {
		t.Errorf("crosspoint update unexpected: coeff=%f, state=%s", cpRoute.Coefficients[1], cpRoute.State)
	}

	// 4. PATCH /api/v1/routes/{id} (Live gain slew update)
	newCoeffs := []float64{0.5, 0.5, 0.5, 0.5}
	slewTime := 15.0
	patchReq := v1.PatchRouteRequest{
		Coefficients: newCoeffs,
		SlewMS:       &slewTime,
	}
	w = doRequest(t, handler, http.MethodPatch, "/api/v1/routes/matrix-route-test-1", patchReq, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from PATCH /api/v1/routes/{id}, got %d: %s", w.Code, w.Body.String())
	}

	// 5. DELETE /api/v1/routes/{id}
	w = doRequest(t, handler, http.MethodDelete, "/api/v1/routes/matrix-route-test-1", nil, "test-token")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 from DELETE /api/v1/routes/{id}, got %d", w.Code)
	}

	// Verify route is deleted
	w = doRequest(t, handler, http.MethodGet, "/api/v1/routes/matrix-route-test-1", nil, "test-token")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after route deletion, got %d", w.Code)
	}
}

func TestServerRoomsLifecycle(t *testing.T) {
	s := newTestServer(t, nil)
	rm := newTestMockRoomManager()
	s.SetRoomManager(rm)
	handler := s.Handler()

	// 1. GET /api/v1/rooms (initially empty)
	w := doRequest(t, handler, http.MethodGet, "/api/v1/rooms", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/v1/rooms, got %d", w.Code)
	}
	var rooms []v1.RoomInfo
	_ = json.NewDecoder(w.Body).Decode(&rooms)
	if len(rooms) != 0 {
		t.Fatalf("expected 0 rooms initially, got %d", len(rooms))
	}

	// 2. POST /api/v1/rooms (Create room)
	createReq := v1.CreateRoomRequest{ID: "studio-alpha", Name: "Studio Alpha Stage"}
	w = doRequest(t, handler, http.MethodPost, "/api/v1/rooms", createReq, "test-token")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 from POST /api/v1/rooms, got %d: %s", w.Code, w.Body.String())
	}

	// 3. GET /api/v1/rooms/{id}
	w = doRequest(t, handler, http.MethodGet, "/api/v1/rooms/studio-alpha", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/v1/rooms/{id}, got %d", w.Code)
	}

	// 4. POST /api/v1/rooms/{id}/join
	w = doRequest(t, handler, http.MethodPost, "/api/v1/rooms/studio-alpha/join", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from POST /api/v1/rooms/{id}/join, got %d: %s", w.Code, w.Body.String())
	}

	// 5. POST /api/v1/rooms/{id}/leave
	w = doRequest(t, handler, http.MethodPost, "/api/v1/rooms/studio-alpha/leave", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from POST /api/v1/rooms/{id}/leave, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServerTelemetryStreamStatsAndSSE(t *testing.T) {
	msm := &mockStreamManager{}
	s := newTestServer(t, msm)
	handler := s.Handler()

	// 1. GET /api/v1/telemetry/stream-stats
	w := doRequest(t, handler, http.MethodGet, "/api/v1/telemetry/stream-stats", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/v1/telemetry/stream-stats, got %d", w.Code)
	}
	var stats map[string]v1.StreamStats
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats["route-1"].PacketsSent != 100 {
		t.Errorf("expected packets_sent 100, got %d", stats["route-1"].PacketsSent)
	}

	// 2. SSE Meter Stream /api/v1/telemetry/meters
	hub := s.Hub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/telemetry/meters?token=test-token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/telemetry/meters failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from SSE stream, got %d", resp.StatusCode)
	}

	// Broadcast test meter frame from hub
	go func() {
		time.Sleep(20 * time.Millisecond)
		hub.Broadcast([]byte(`{"stream_id":"test-stream","timestamp_ns":123456,"channels":[]}`))
	}()

	scanner := bufio.NewScanner(resp.Body)
	var gotEvent bool
	deadline := time.After(500 * time.Millisecond)
	ch := make(chan string, 1)

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				ch <- line
				return
			}
		}
	}()

	select {
	case line := <-ch:
		if strings.Contains(line, "test-stream") {
			gotEvent = true
		}
	case <-deadline:
		t.Fatal("timed out waiting for SSE meter frame")
	}

	if !gotEvent {
		t.Error("failed to receive broadcasted SSE meter frame")
	}
}

func TestServerMetricsPrometheus(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	_ = doRequest(t, handler, http.MethodGet, "/status", nil, "test-token")
	w := doRequest(t, handler, http.MethodGet, "/metrics", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /metrics, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "audio_router_api_requests_total") {
		t.Errorf("expected audio_router_api_requests_total in metrics, got: %s", body)
	}
}

func TestServerMaxBytesLimit(t *testing.T) {
	s := newTestServer(t, nil)
	handler := s.Handler()

	// Body exceeding 1MB limit
	hugeBody := bytes.Repeat([]byte("a"), 2*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/peers", bytes.NewReader(hugeBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// MaxBytesReader returns 400 Bad Request on decoding oversized payload
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized payload, got %d", w.Code)
	}
}

func TestServerPanicRecovery(t *testing.T) {
	s := newTestServer(t, nil)
	s.mux.HandleFunc("/panic-test", func(w http.ResponseWriter, r *http.Request) {
		panic("test panic recovery")
	})
	handler := s.Handler()

	w := doRequest(t, handler, http.MethodGet, "/panic-test", nil, "test-token")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on panic, got %d", w.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "internal error" {
		t.Errorf("unexpected error payload: %+v", resp)
	}
}

func TestConstantTimeCompare(t *testing.T) {
	token := []byte("test-token")
	if subtle.ConstantTimeCompare(token, token) != 1 {
		t.Error("constant time compare failed for equal")
	}
	if subtle.ConstantTimeCompare(token, []byte("wrong")) == 1 {
		t.Error("constant time compare should fail for different")
	}
}

func TestServerNetworkHealthAndMetrics(t *testing.T) {
	msm := &mockStreamManager{}
	s := newTestServer(t, msm)
	handler := s.Handler()

	// 1. Unauthenticated request to /api/v1/network/health -> 401
	w := doRequest(t, handler, http.MethodGet, "/api/v1/network/health", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized from /api/v1/network/health, got %d", w.Code)
	}

	// 2. Authenticated GET /api/v1/network/health -> 200
	w = doRequest(t, handler, http.MethodGet, "/api/v1/network/health", nil, "test-token")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/v1/network/health, got %d: %s", w.Code, w.Body.String())
	}

	var snap netTelemetry.NetworkHealthSnapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("decode network health snapshot: %v", err)
	}
	if snap.HealthScore < 0.0 || snap.HealthScore > 100.0 {
		t.Errorf("health score out of bounds: %f", snap.HealthScore)
	}
	if snap.HealthGrade == "" {
		t.Errorf("empty health grade")
	}
	if snap.Streams == nil {
		t.Errorf("streams map is nil")
	}
	if st, ok := snap.Streams["abcd1234abcd1234"]; !ok {
		t.Errorf("expected abcd1234abcd1234 in streams, got %+v", snap.Streams)
	} else if st.PacketsSent != 100 {
		t.Errorf("packets sent = %d; want 100", st.PacketsSent)
	}

	// 3. GET /metrics -> Prometheus scraping with ADR 0010 metric families
	w = doRequest(t, handler, http.MethodGet, "/metrics", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /metrics, got %d", w.Code)
	}
	body := w.Body.String()

	expectedMetrics := []string{
		"audio_router_network_node_health_score",
		"audio_router_network_cluster_nodes",
		"audio_router_network_lan_beacons_total",
		"audio_router_network_token_rejections_total",
		"audio_router_network_seed_joins_total",
		"audio_router_network_clock_offset_microseconds",
		"audio_router_network_clock_drift_ppm",
		"audio_router_network_clock_is_master",
		"audio_router_stream_fec_recovered_packets_total",
		"audio_router_stream_plc_concealed_frames_total",
	}

	for _, metricName := range expectedMetrics {
		if !strings.Contains(body, metricName) {
			t.Errorf("expected %s in /metrics response, got:\n%s", metricName, body)
		}
	}
}
