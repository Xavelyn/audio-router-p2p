package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
	"github.com/audio-router-p2p/pkg/transport"
)

type testDaemon struct {
	cfg        *config.Config
	disc       *discovery.Discovery
	syncMgr    *syncmgr.Manager
	trans      streamer.Transport
	streams    *streamManager
	roomMgr    *group.DefaultRoomManager
	rt         *router.Router
	apiServer  *api.Server
	httpServer *http.Server
	apiURL     string
	inDev      *audio.VirtualAudioDevice
	outDev     *audio.VirtualAudioDevice
	hubCtx     context.Context
	hubCancel  context.CancelFunc
}

func setupDaemon(t *testing.T, id string) *testDaemon {
	t.Helper()
	tmpDir := t.TempDir()

	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")
	routesFile := filepath.Join(tmpDir, "routes.json")

	cfg := &config.Config{
		AuthToken:        "secret-daemon-token",
		CertFile:         certFile,
		KeyFile:          keyFile,
		RoutesFile:       routesFile,
		UseEmulator:      true,
		DiscoveryPort:    0,
		MulticastPort:    0,
		SyncPort:         0,
		StreamPort:       "127.0.0.1:0",
		ListenAddr:       "127.0.0.1:0",
		AnnounceInterval: 1 * time.Second,
		PeerTTL:          4 * time.Second,
		SyncInterval:     1 * time.Second,
		ShutdownTimeout:  2 * time.Second,
	}

	if err := ensureTLS(cfg); err != nil {
		t.Fatalf("ensureTLS: %v", err)
	}

	disc, err := discovery.New(cfg, id)
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewUDPTransport: %v", err)
	}

	sm := syncmgr.NewManager(cfg, disc)
	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)

	inDev := audio.NewVirtualAudioDevice()
	outDev := audio.NewVirtualAudioDevice()
	vp := group.NewVirtualDeviceProvider(inDev, outDev)

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans, vp)

	apiSrv := api.NewServer(cfg, rt, disc, sm, streamMgr, roomMgr)

	hubCtx, hubCancel := context.WithCancel(context.Background())
	apiSrv.Hub().Start(hubCtx)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{},
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	tlsConfig.Certificates = append(tlsConfig.Certificates, cert)

	tlsListener := tls.NewListener(ln, tlsConfig)
	httpServer := &http.Server{Handler: apiSrv.Handler()}

	go func() {
		_ = httpServer.Serve(tlsListener)
	}()

	apiURL := fmt.Sprintf("https://%s", ln.Addr().String())

	return &testDaemon{
		cfg:        cfg,
		disc:       disc,
		syncMgr:    sm,
		trans:      trans,
		streams:    streamMgr,
		roomMgr:    roomMgr,
		rt:         rt,
		apiServer:  apiSrv,
		httpServer: httpServer,
		apiURL:     apiURL,
		inDev:      inDev,
		outDev:     outDev,
		hubCtx:     hubCtx,
		hubCancel:  hubCancel,
	}
}

func (d *testDaemon) close() {
	d.hubCancel()
	d.apiServer.Hub().Stop()
	_ = d.httpServer.Close()
	_ = d.roomMgr.Close()
	d.streams.StopAll()
	d.disc.Close()
	d.syncMgr.Close()
	_ = d.trans.Close()
	d.inDev.Close()
	d.outDev.Close()
}

func (d *testDaemon) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}
}

// TestDaemonAPI_DynamicRouteMatrixE2E exercises dynamic matrix routing via the OpenAPI 3.1 HTTP REST API:
// 1. Inspect system status via canonical /status (aliased to /api/v1/system/status) and direct /api/v1/system/status
// 2. Provision and inspect in-memory virtual loopback audio device via POST /api/v1/devices/virtual
// 3. Add dynamic matrix route from Node 1 to Node 2 via POST /api/v1/routes
// 4. Inspect route list and route details via GET /api/v1/routes and GET /api/v1/routes/{id}
// 5. Update crosspoint gain and verify 20ms linear gain slewing via POST /api/v1/routes/{id}/crosspoint
// 6. Inspect telemetry stats via GET /api/v1/telemetry/stream-stats
// 7. Subscribe to real-time SSE meter frames via GET /api/v1/telemetry/meters
// 8. Inspect Prometheus metrics via GET /metrics
// 9. Teardown route via DELETE /api/v1/routes/{id} and verify deletion
// 10. Teardown virtual device via DELETE /api/v1/devices/virtual/{id}
func TestDaemonAPI_DynamicRouteMatrixE2E(t *testing.T) {
	node1 := setupDaemon(t, "daemon-node-01")
	defer node1.close()

	node2 := setupDaemon(t, "daemon-node-02")
	defer node2.close()

	client := node1.httpClient()

	// 1. Check API /status forwarding to /api/v1/system/status
	req, _ := http.NewRequest(http.MethodGet, node1.apiURL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status status: %d", resp.StatusCode)
	}
	var status v1.SystemStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	_ = resp.Body.Close()
	if status.Status != "ok" {
		t.Errorf("unexpected status response: %+v", status)
	}

	// Direct check on /api/v1/system/status
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/api/v1/system/status", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/system/status failed")
	}
	_ = resp.Body.Close()

	// 2. Provision virtual device via POST /api/v1/devices/virtual
	createVirtReq := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-e2e-loopback",
		Direction:        "duplex",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	body, _ := json.Marshal(createVirtReq)
	req, _ = http.NewRequest(http.MethodPost, node1.apiURL+"/api/v1/devices/virtual", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/devices/virtual failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual status: %d", resp.StatusCode)
	}
	var createdDev v1.AudioDevice
	if err := json.NewDecoder(resp.Body).Decode(&createdDev); err != nil {
		t.Fatalf("decode created virtual device: %v", err)
	}
	_ = resp.Body.Close()
	if !createdDev.IsVirtual || createdDev.Name != "vdev-e2e-loopback" {
		t.Fatalf("unexpected created virtual device: %+v", createdDev)
	}

	// Verify device exists via GET /api/v1/devices?kind=virtual
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/api/v1/devices?kind=virtual", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/devices?kind=virtual failed")
	}
	var virtDevs []v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&virtDevs)
	_ = resp.Body.Close()
	foundVirt := false
	for _, d := range virtDevs {
		if d.ID == createdDev.ID {
			foundVirt = true
			break
		}
	}
	if !foundVirt {
		t.Fatalf("expected created virtual device %s in list", createdDev.ID)
	}

	// 3. Add dynamic route from Node 1 to Node 2 via POST /api/v1/routes
	destAddr := node2.trans.Addr()
	routePayload := v1.CreateRouteRequest{
		ID:   "route-api-matrix-1",
		Name: "E2E Matrix Stream",
		Source: router.MatrixEndpoint{
			DeviceID: "-1",
			Channels: []int{0, 1},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   destAddr,
			Channels: []int{0, 1},
		},
		Coefficients: []float64{1.0, 0.0, 0.0, 1.0},
		SlewMS:       20.0,
	}
	body, _ = json.Marshal(routePayload)

	req, _ = http.NewRequest(http.MethodPost, node1.apiURL+"/api/v1/routes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/routes failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/routes status: %d, body: %s", resp.StatusCode, string(respBody))
	}
	var createdRoute router.MatrixRoute
	_ = json.NewDecoder(resp.Body).Decode(&createdRoute)
	_ = resp.Body.Close()
	if createdRoute.ID != "route-api-matrix-1" || createdRoute.State != "active" {
		t.Fatalf("unexpected created route: %+v", createdRoute)
	}

	// 4. Verify route exists via GET /api/v1/routes
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/api/v1/routes", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/routes failed")
	}
	var routesList []router.MatrixRoute
	_ = json.NewDecoder(resp.Body).Decode(&routesList)
	_ = resp.Body.Close()
	if len(routesList) == 0 || routesList[0].ID != "route-api-matrix-1" {
		t.Fatalf("expected route in routes list, got %+v", routesList)
	}

	// Verify route details via GET /api/v1/routes/{id}
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/api/v1/routes/route-api-matrix-1", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/routes/route-api-matrix-1 failed")
	}
	var singleRoute router.MatrixRoute
	_ = json.NewDecoder(resp.Body).Decode(&singleRoute)
	_ = resp.Body.Close()
	if singleRoute.ID != "route-api-matrix-1" {
		t.Fatalf("unexpected route details: %+v", singleRoute)
	}

	// 5. Update crosspoint gain and verify gain slewing via POST /api/v1/routes/{id}/crosspoint
	crosspointReq := v1.CrosspointGainRequest{
		OutChannel: 0,
		InChannel:  0,
		Gain:       0.5,
		SlewMS:     20.0,
	}
	body, _ = json.Marshal(crosspointReq)
	req, _ = http.NewRequest(http.MethodPost, node1.apiURL+"/api/v1/routes/route-api-matrix-1/crosspoint", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/routes/{id}/crosspoint failed")
	}
	var updatedRoute router.MatrixRoute
	_ = json.NewDecoder(resp.Body).Decode(&updatedRoute)
	_ = resp.Body.Close()
	if updatedRoute.Coefficients[0] != 0.5 {
		t.Errorf("expected coefficient[0] = 0.5, got %f", updatedRoute.Coefficients[0])
	}
	active := node1.streams.routes[updatedRoute.ID]
	if active == nil || active.matrix == nil {
		t.Fatal("API route has no live matrix processor")
	}
	originalStream := active.st
	deadline := time.Now().Add(2 * time.Second)
	for active.matrix.mixer.CurrentCoefficients()[0] != 0.5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := active.matrix.mixer.CurrentCoefficients()[0]; got != 0.5 {
		t.Fatalf("live crosspoint gain = %f, want 0.5", got)
	}
	muted := true
	patch := v1.PatchRouteRequest{Muted: &muted}
	w, _ := json.Marshal(patch)
	req, _ = http.NewRequest(http.MethodPatch, node1.apiURL+"/api/v1/routes/route-api-matrix-1", bytes.NewReader(w))
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH mute: response=%v error=%v", resp, err)
	}
	_ = resp.Body.Close()
	deadline = time.Now().Add(2 * time.Second)
	for active.matrix.mixer.CurrentCoefficients()[0] != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := active.matrix.mixer.CurrentCoefficients()[0]; got != 0 {
		t.Fatalf("live mute coefficient = %f, want 0", got)
	}
	if node1.streams.routes[updatedRoute.ID].st != originalStream {
		t.Fatal("gain/mute update restarted the stream")
	}

	// 6. Check Stream Stats endpoint via GET /api/v1/telemetry/stream-stats
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/api/v1/telemetry/stream-stats", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/telemetry/stream-stats failed")
	}
	var stats map[string]v1.StreamStats
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	_ = resp.Body.Close()

	if _, ok := stats["route-api-matrix-1"]; !ok {
		t.Errorf("expected stats for route-api-matrix-1")
	}

	// 7. Subscribe to real-time SSE meter frames via GET /api/v1/telemetry/meters
	sseCtx, sseCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer sseCancel()

	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, node1.apiURL+"/api/v1/telemetry/meters", nil)
	sseReq.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	sseReq.Header.Set("Accept", "text/event-stream")

	sseResp, err := client.Do(sseReq)
	if err != nil {
		t.Fatalf("GET /api/v1/telemetry/meters failed: %v", err)
	}
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/telemetry/meters status: %d", sseResp.StatusCode)
	}

	// Broadcast frame to verify SSE streaming
	go func() {
		time.Sleep(50 * time.Millisecond)
		node1.apiServer.Hub().Broadcast([]byte(`{"stream_id":"route-api-matrix-1","timestamp_ns":123456,"channels":[]}`))
	}()

	reader := bufio.NewReader(sseResp.Body)
	receivedMeterEvent := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "event: meter") {
			receivedMeterEvent = true
			break
		}
	}
	if !receivedMeterEvent {
		t.Errorf("expected SSE meter event from /api/v1/telemetry/meters")
	}

	// 8. Check Prometheus metrics via GET /metrics
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/metrics", nil)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics failed")
	}
	metricsBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	metricsStr := string(metricsBytes)
	if !strings.Contains(metricsStr, "audio_router_active_routes") {
		t.Errorf("expected audio_router_active_routes metric in Prometheus output")
	}
	if !strings.Contains(metricsStr, "audio_router_api_requests_total") {
		t.Errorf("expected audio_router_api_requests_total metric in Prometheus output")
	}

	// 9. Delete route via DELETE /api/v1/routes/{id}
	req, _ = http.NewRequest(http.MethodDelete, node1.apiURL+"/api/v1/routes/route-api-matrix-1", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/v1/routes failed: resp=%v", resp)
	}

	// Verify route list is now empty
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/api/v1/routes", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, _ = client.Do(req)
	var remainingRoutes []router.MatrixRoute
	_ = json.NewDecoder(resp.Body).Decode(&remainingRoutes)
	_ = resp.Body.Close()
	if len(remainingRoutes) != 0 {
		t.Errorf("expected 0 active routes after DELETE, got %d", len(remainingRoutes))
	}

	// 10. Delete virtual device via DELETE /api/v1/devices/virtual/{id}
	req, _ = http.NewRequest(http.MethodDelete, node1.apiURL+"/api/v1/devices/virtual/"+createdDev.ID, nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/v1/devices/virtual/{id} failed")
	}
}

// TestDaemonAPI_RoomsLifecycleMatrixE2E exercises multi-room management via the OpenAPI 3.1 HTTP REST API:
// POST /api/v1/rooms, GET /api/v1/rooms, POST /api/v1/rooms/{id}/join, POST /api/v1/rooms/{id}/leave, GET /api/v1/rooms/{id}
func TestDaemonAPI_RoomsLifecycleMatrixE2E(t *testing.T) {
	node := setupDaemon(t, "daemon-room-node")
	defer node.close()

	client := node.httpClient()

	// 1. Create room via POST /api/v1/rooms
	createReq := v1.CreateRoomRequest{
		ID:   "matrix-room-alpha",
		Name: "Alpha Conference Room",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest(http.MethodPost, node.apiURL+"/api/v1/rooms", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	var createdRoom v1.RoomInfo
	_ = json.NewDecoder(resp.Body).Decode(&createdRoom)
	_ = resp.Body.Close()
	if createdRoom.ID != "matrix-room-alpha" {
		t.Fatalf("unexpected created room: %+v", createdRoom)
	}

	// 2. List rooms via GET /api/v1/rooms
	req, _ = http.NewRequest(http.MethodGet, node.apiURL+"/api/v1/rooms", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/rooms failed: %v", err)
	}
	var rooms []v1.RoomInfo
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	_ = resp.Body.Close()

	if len(rooms) != 1 || rooms[0].ID != "matrix-room-alpha" {
		t.Fatalf("unexpected rooms list: %+v", rooms)
	}

	// 3. Join room via POST /api/v1/rooms/matrix-room-alpha/join
	req, _ = http.NewRequest(http.MethodPost, node.apiURL+"/api/v1/rooms/matrix-room-alpha/join", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/join failed: %v", err)
	}
	_ = resp.Body.Close()

	// 4. Leave room via POST /api/v1/rooms/matrix-room-alpha/leave
	req, _ = http.NewRequest(http.MethodPost, node.apiURL+"/api/v1/rooms/matrix-room-alpha/leave", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/leave failed: %v", err)
	}
	_ = resp.Body.Close()

	// 5. Inspect room details via GET /api/v1/rooms/{id}
	req, _ = http.NewRequest(http.MethodGet, node.apiURL+"/api/v1/rooms/matrix-room-alpha", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/rooms/{id} failed: %v", err)
	}
	var room v1.RoomInfo
	_ = json.NewDecoder(resp.Body).Decode(&room)
	_ = resp.Body.Close()
	if room.ID != "matrix-room-alpha" {
		t.Errorf("unexpected room details: %+v", room)
	}
}
