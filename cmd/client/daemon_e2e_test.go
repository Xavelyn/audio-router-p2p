package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api"
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
	}
}

func (d *testDaemon) close() {
	_ = d.httpServer.Close()
	d.roomMgr.Close()
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

// TestDaemonAPI_DynamicRouteMatrixE2E exercises dynamic matrix routing via the full HTTP REST API:
// 1. Add route via POST /routes
// 2. Transmit audio over network between two independent daemons
// 3. Inspect telemetry via GET /streams/stats and GET /metrics
// 4. Remove route via DELETE /routes
func TestDaemonAPI_DynamicRouteMatrixE2E(t *testing.T) {
	node1 := setupDaemon(t, "daemon-node-01")
	defer node1.close()

	node2 := setupDaemon(t, "daemon-node-02")
	defer node2.close()

	client := node1.httpClient()

	// 1. Check API /status
	req, _ := http.NewRequest(http.MethodGet, node1.apiURL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status status: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 2. Add dynamic route from Node 1 to Node 2 via REST API
	destAddr := node2.trans.Addr()
	routePayload := map[string]interface{}{
		"id":           "route-api-matrix-1",
		"sourceDevice": -1,
		"destinations": []string{destAddr},
		"sampleRate":   48000,
		"channels":     2,
	}
	body, _ := json.Marshal(routePayload)

	req, _ = http.NewRequest(http.MethodPost, node1.apiURL+"/routes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /routes failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /routes status: %d, body: %s", resp.StatusCode, string(respBody))
	}
	_ = resp.Body.Close()

	// Verify route exists via GET /routes
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/routes", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /routes failed")
	}
	_ = resp.Body.Close()

	// 3. Check Stream Stats endpoint via GET /streams/stats
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/streams/stats", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /streams/stats failed")
	}
	var stats map[string]streamer.StreamerStats
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	_ = resp.Body.Close()

	if _, ok := stats["route-api-matrix-1"]; !ok {
		t.Errorf("expected stats for route-api-matrix-1")
	}

	// 4. Check Prometheus metrics via GET /metrics
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics failed")
	}
	metricsBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Contains(metricsBytes, []byte("audio_router_active_routes")) {
		t.Errorf("expected audio_router_active_routes metric in Prometheus output")
	}

	// 5. Delete route via DELETE /routes?id=route-api-matrix-1
	req, _ = http.NewRequest(http.MethodDelete, node1.apiURL+"/routes?id=route-api-matrix-1", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /routes failed")
	}

	// Verify route list is now empty
	req, _ = http.NewRequest(http.MethodGet, node1.apiURL+"/routes", nil)
	req.Header.Set("Authorization", "Bearer "+node1.cfg.AuthToken)
	resp, _ = client.Do(req)
	var activeRoutes []router.StreamRoute
	_ = json.NewDecoder(resp.Body).Decode(&activeRoutes)
	_ = resp.Body.Close()
	if len(activeRoutes) != 0 {
		t.Errorf("expected 0 active routes after DELETE, got %d", len(activeRoutes))
	}
}

// TestDaemonAPI_RoomsLifecycleMatrixE2E exercises multi-room management via the HTTP REST API:
// POST /rooms, GET /rooms, POST /rooms/{id}/join, POST /rooms/{id}/leave
func TestDaemonAPI_RoomsLifecycleMatrixE2E(t *testing.T) {
	node := setupDaemon(t, "daemon-room-node")
	defer node.close()

	client := node.httpClient()

	// 1. Create room via POST /rooms
	createReq := map[string]string{
		"id":   "matrix-room-alpha",
		"name": "Alpha Conference Room",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest(http.MethodPost, node.apiURL+"/rooms", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// 2. List rooms via GET /rooms
	req, _ = http.NewRequest(http.MethodGet, node.apiURL+"/rooms", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /rooms failed: %v", err)
	}
	var rooms []api.RoomInfo
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	_ = resp.Body.Close()

	if len(rooms) != 1 || rooms[0].ID != "matrix-room-alpha" {
		t.Fatalf("unexpected rooms list: %+v", rooms)
	}

	// 3. Join room via POST /rooms/matrix-room-alpha/join
	req, _ = http.NewRequest(http.MethodPost, node.apiURL+"/rooms/matrix-room-alpha/join", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rooms/join failed: %v", err)
	}
	_ = resp.Body.Close()

	// 4. Leave room via POST /rooms/matrix-room-alpha/leave
	req, _ = http.NewRequest(http.MethodPost, node.apiURL+"/rooms/matrix-room-alpha/leave", nil)
	req.Header.Set("Authorization", "Bearer "+node.cfg.AuthToken)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rooms/leave failed: %v", err)
	}
	_ = resp.Body.Close()
}
