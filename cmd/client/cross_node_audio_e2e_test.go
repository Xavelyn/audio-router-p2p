package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
	"github.com/audio-router-p2p/pkg/transport"
)

type testClusterNode struct {
	id         string
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
	client     *http.Client
}

func spawnClusterNode(t *testing.T, id string, seedAddrs ...string) *testClusterNode {
	t.Helper()
	tmpDir := t.TempDir()

	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")
	routesFile := filepath.Join(tmpDir, "routes.json")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("[%s] listen tcp: %v", id, err)
	}
	apiPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	trans, err := transport.NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("[%s] NewUDPTransport: %v", id, err)
	}
	streamPort := trans.LocalAddr().Port

	cfg := &config.Config{
		AuthToken:        "cluster-e2e-token",
		CertFile:         certFile,
		KeyFile:          keyFile,
		RoutesFile:       routesFile,
		UseEmulator:      false,
		DiscoveryPort:    0,
		MulticastPort:    0,
		SyncPort:         0,
		StreamPort:       fmt.Sprintf("127.0.0.1:%d", streamPort),
		ListenAddr:       fmt.Sprintf("127.0.0.1:%s", apiPort),
		AnnounceInterval: 150 * time.Millisecond,
		PeerTTL:          6 * time.Second,
		SyncInterval:     400 * time.Millisecond,
		ShutdownTimeout:  2 * time.Second,
	}
	for _, seed := range seedAddrs {
		if seed != "" {
			cfg.StaticPeers = append(cfg.StaticPeers, config.StaticPeerConfig{Addr: seed})
		}
	}

	if err := ensureTLS(cfg); err != nil {
		t.Fatalf("[%s] ensureTLS: %v", id, err)
	}

	disc, err := discovery.New(cfg, id)
	if err != nil {
		t.Fatalf("[%s] discovery.New: %v", id, err)
	}

	sm := syncmgr.NewManager(cfg, disc)
	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		t.Fatalf("[%s] router.New: %v", id, err)
	}

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streamMgr)
	disc.SetStateDelegate(roomMgr)

	apiSrv := api.NewServer(cfg, rt, disc, sm, streamMgr, roomMgr)
	streamMgr.SetHub(apiSrv.Hub())

	go disc.Run()
	if cm := apiSrv.ClusterManager(); cm != nil {
		cm.Start(context.Background())
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{},
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatalf("[%s] LoadX509KeyPair: %v", id, err)
	}
	tlsConfig.Certificates = append(tlsConfig.Certificates, cert)

	tlsListener := tls.NewListener(ln, tlsConfig)
	httpServer := &http.Server{Handler: apiSrv.Handler()}

	go func() {
		_ = httpServer.Serve(tlsListener)
	}()

	apiURL := fmt.Sprintf("https://127.0.0.1:%s", apiPort)
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}

	return &testClusterNode{
		id:         id,
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
		client:     httpClient,
	}
}

func (n *testClusterNode) close() {
	if cm := n.apiServer.ClusterManager(); cm != nil {
		cm.Stop()
	}
	n.apiServer.Hub().Stop()
	_ = n.httpServer.Close()
	_ = n.roomMgr.Close()
	n.streams.StopAll()
	_ = n.disc.Close()
	n.syncMgr.Close()
	_ = n.trans.Close()
}

func (n *testClusterNode) get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, n.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.AuthToken)
	return n.client.Do(req)
}

func (n *testClusterNode) post(path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequest(http.MethodPost, n.apiURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	return n.client.Do(req)
}

func (n *testClusterNode) delete(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, n.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.AuthToken)
	return n.client.Do(req)
}

// TestClusterCrossNodeAudioRouting_E2E verifies the entire system beginning from the HTTP REST API:
// 1. Spawns 3 cluster nodes (Node 1, Node 2, Node 3) with TLS and dynamic UDP audio transports.
// 2. Asserts cluster membership and topology convergence across all 3 nodes via GET /api/v1/cluster/nodes.
// 3. Provisions virtual audio devices (mic input on Node 1, speaker output on Node 2 and Node 3) via POST /api/v1/devices/virtual.
// 4. Verifies cluster-wide device visibility and aggregation via GET /api/v1/cluster/devices.
// 5. Creates and syncs a distributed group (room) across all 3 nodes via POST /api/v1/rooms and GET /api/v1/cluster/rooms.
// 6. Configures cross-node matrix routing from Node 1 input -> Node 2 output via POST /api/v1/routes.
// 7. Injects real audio (PCM sine wave samples) at Node 1 and asserts bitstream delivery and sound capture at Node 2.
// 8. Performs dynamic crosspoint gain modification via POST /api/v1/routes/{id}/crosspoint.
// 9. Cleans up routes and virtual devices via DELETE /api/v1/routes/{id} and DELETE /api/v1/devices/virtual/{id}.
func TestClusterCrossNodeAudioRouting_E2E(t *testing.T) {
	// 1. Spawn 3 cluster nodes
	node1 := spawnClusterNode(t, "node-01")
	defer node1.close()

	node2 := spawnClusterNode(t, "node-02", node1.disc.OwnAddr())
	defer node2.close()

	node3 := spawnClusterNode(t, "node-03", node1.disc.OwnAddr())
	defer node3.close()

	// 2. Verify cluster membership convergence via GET /api/v1/cluster/nodes on Node 1
	t.Log("Waiting for 3-node cluster convergence...")
	deadline := time.Now().Add(6 * time.Second)
	var clusterNodes []cluster.ClusterNodeSummary
	for time.Now().Before(deadline) {
		resp, err := node1.get("/api/v1/cluster/nodes")
		if err == nil && resp.StatusCode == http.StatusOK {
			var nodeResp cluster.ClusterNodesResponse
			if err := json.NewDecoder(resp.Body).Decode(&nodeResp); err == nil && len(nodeResp.Nodes) == 3 {
				_ = resp.Body.Close()
				clusterNodes = nodeResp.Nodes
				break
			}
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(clusterNodes) != 3 {
		t.Fatalf("expected 3 nodes in cluster, got %d: %+v", len(clusterNodes), clusterNodes)
	}
	t.Logf("Cluster converged: %d nodes active (%s, %s, %s)",
		len(clusterNodes), clusterNodes[0].ID, clusterNodes[1].ID, clusterNodes[2].ID)

	// 3. Provision virtual audio devices via POST /api/v1/devices/virtual
	// Node 1: Capture Mic (input)
	micReq := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-node1-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err := node1.post("/api/v1/devices/virtual", micReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node 1: POST /api/v1/devices/virtual failed: err=%v status=%v", err, resp.StatusCode)
	}
	var micDev v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&micDev)
	_ = resp.Body.Close()

	// Node 2: Playback Speaker (output)
	spkReq := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-node2-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err = node2.post("/api/v1/devices/virtual", spkReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node 2: POST /api/v1/devices/virtual failed: err=%v status=%v", err, resp.StatusCode)
	}
	var spkDev v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&spkDev)
	_ = resp.Body.Close()

	// Node 3: Stage Monitor (output)
	monReq := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-node3-mon",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err = node3.post("/api/v1/devices/virtual", monReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node 3: POST /api/v1/devices/virtual failed: err=%v status=%v", err, resp.StatusCode)
	}
	var monDev v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&monDev)
	_ = resp.Body.Close()

	t.Logf("Virtual devices provisioned: Mic=%s Spk=%s Mon=%s", micDev.URN, spkDev.URN, monDev.URN)

	// 4. Verify aggregated device list across the cluster via GET /api/v1/cluster/devices
	deadline = time.Now().Add(4 * time.Second)
	foundClusterDevices := false
	for time.Now().Before(deadline) {
		resp, err = node1.get("/api/v1/cluster/devices")
		if err == nil && resp.StatusCode == http.StatusOK {
			var devResp cluster.ClusterDevicesResponse
			if err := json.NewDecoder(resp.Body).Decode(&devResp); err == nil {
				_ = resp.Body.Close()
				hasMic, hasSpk, hasMon := false, false, false
				for _, d := range devResp.Devices {
					if d.ID == micDev.URN || d.DeviceID == micDev.URN {
						hasMic = true
					}
					if d.ID == spkDev.URN || d.DeviceID == spkDev.URN {
						hasSpk = true
					}
					if d.ID == monDev.URN || d.DeviceID == monDev.URN {
						hasMon = true
					}
				}
				if hasMic && hasSpk && hasMon {
					foundClusterDevices = true
					break
				}
			} else {
				_ = resp.Body.Close()
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !foundClusterDevices {
		t.Fatalf("cluster devices did not aggregate across all 3 nodes within deadline")
	}
	t.Log("Verified: all input and output devices across all 3 nodes visible via cluster API")

	// 5. Distributed Group (Room) Synchronization via API
	// Create Room on Node 1
	createRoomReq := map[string]string{
		"id":   "room-studio-stage",
		"name": "Studio Stage Room",
	}
	resp, err = node1.post("/api/v1/rooms", createRoomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// Add input device on Node 1
	addInputReq := map[string]string{"device_id": micDev.URN}
	resp, err = node1.post("/api/v1/rooms/room-studio-stage/inputs", addInputReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/room-studio-stage/inputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Add output device on Node 1
	addOutputReq := map[string]string{"device_id": spkDev.URN}
	resp, err = node1.post("/api/v1/rooms/room-studio-stage/outputs", addOutputReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/room-studio-stage/outputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Verify CRDT room synchronization arrived at Node 2 and Node 3
	deadline = time.Now().Add(4 * time.Second)
	roomSynced := false
	for time.Now().Before(deadline) {
		resp2, err2 := node2.get("/api/v1/cluster/rooms")
		resp3, err3 := node3.get("/api/v1/cluster/rooms")
		if err2 == nil && err3 == nil && resp2.StatusCode == http.StatusOK && resp3.StatusCode == http.StatusOK {
			var rooms2, rooms3 []group.RoomRecord
			_ = json.NewDecoder(resp2.Body).Decode(&rooms2)
			_ = json.NewDecoder(resp3.Body).Decode(&rooms3)
			_ = resp2.Body.Close()
			_ = resp3.Body.Close()

			hasR2, hasR3 := false, false
			for _, r := range rooms2 {
				if r.ID == "room-studio-stage" && r.Name == "Studio Stage Room" {
					hasR2 = true
				}
			}
			for _, r := range rooms3 {
				if r.ID == "room-studio-stage" && r.Name == "Studio Stage Room" {
					hasR3 = true
				}
			}
			if hasR2 && hasR3 {
				roomSynced = true
				break
			}
		} else {
			if resp2 != nil {
				_ = resp2.Body.Close()
			}
			if resp3 != nil {
				_ = resp3.Body.Close()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !roomSynced {
		t.Fatalf("expected room-studio-stage to converge via CRDT across Node 2 and Node 3")
	}
	t.Log("Verified: distributed room synchronized across all nodes via CRDT")

	// 6. Cross-Node Matrix Audio Routing (Node 1 -> Node 2)
	routeID := "route-cross-1-to-2"
	createRouteReq := v1.CreateRouteRequest{
		ID:   routeID,
		Name: "Node1 Mic to Node2 Speaker",
		Source: router.MatrixEndpoint{
			NodeID:   "node-01",
			DeviceID: micDev.URN,
			Channels: []int{0, 1},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   "node-02",
			DeviceID: spkDev.URN,
			Channels: []int{0, 1},
		},
		Coefficients: []float64{1.0, 0.0, 0.0, 1.0},
		SlewMS:       20.0,
	}

	// Post egress route to Node 1
	resp, err = node1.post("/api/v1/routes", createRouteReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node 1: POST /api/v1/routes failed: err=%v status=%v", err, resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Post ingress route to Node 2
	resp, err = node2.post("/api/v1/routes", createRouteReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node 2: POST /api/v1/routes failed: err=%v status=%v", err, resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 7. Real Audio Transmission (Sine Wave PCM) across nodes
	micStream, ok := node1.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	if !ok || micStream == nil {
		t.Fatalf("could not obtain VirtualAudioDevice stream for %s", micDev.URN)
	}
	spkStream, ok := node2.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !ok || spkStream == nil {
		t.Fatalf("could not obtain VirtualAudioDevice stream for %s", spkDev.URN)
	}

	// Generate 440 Hz stereo PCM sine wave (16-bit little-endian)
	framesPerChunk := 480 // 10ms @ 48000 Hz
	sineChunk := generateSineBlock(48000, 2, framesPerChunk, 440.0, 0.707)

	doneFeed := make(chan struct{})
	go func() {
		defer close(doneFeed)
		for i := 0; i < 40; i++ {
			_, _ = micStream.InjectPush(sineChunk)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Verify audio delivery at Node 2 speaker stream
	t.Log("Verifying cross-node audio delivery to Node 2 speaker...")
	deadline = time.Now().Add(4 * time.Second)
	var captured []byte
	for time.Now().Before(deadline) {
		captured = spkStream.CapturedBytes()
		if len(captured) >= len(sineChunk)*2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	<-doneFeed

	if len(captured) < len(sineChunk)*2 {
		t.Fatalf("expected cross-node audio to arrive at Node 2 output: got %d bytes, want >= %d",
			len(captured), len(sineChunk)*2)
	}

	// Assert non-silence and calculate peak PCM amplitude from captured bytes
	maxAmp := int16(0)
	for i := 0; i+2 <= len(captured); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(captured[i:]))
		if sample < 0 {
			sample = -sample
		}
		if sample > maxAmp {
			maxAmp = sample
		}
	}

	if maxAmp < 1000 {
		t.Fatalf("captured audio is silent or near zero: max amplitude = %d (want > 1000)", maxAmp)
	}
	t.Logf("SUCCESS: cross-node audio received at Node 2 (captured %d bytes, peak PCM amp = %d / 32767)",
		len(captured), maxAmp)

	// 8. Dynamic crosspoint gain modification via POST /api/v1/routes/{id}/crosspoint
	crosspointReq := v1.CrosspointGainRequest{
		InChannel:  0,
		OutChannel: 0,
		Gain:       0.5,
		SlewMS:     20.0,
	}
	resp, err = node1.post(fmt.Sprintf("/api/v1/routes/%s/crosspoint", routeID), crosspointReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/routes/%s/crosspoint failed: %v", routeID, err)
	}
	var updatedRoute router.MatrixRoute
	_ = json.NewDecoder(resp.Body).Decode(&updatedRoute)
	_ = resp.Body.Close()

	if math.Abs(updatedRoute.Coefficients[0]-0.5) > 1e-4 {
		t.Errorf("expected updated gain coefficient 0.5, got %f", updatedRoute.Coefficients[0])
	}
	t.Log("Verified: dynamic crosspoint gain update succeeded via API")

	// 9. Teardown via DELETE API
	resp, err = node1.delete("/api/v1/routes/" + routeID)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Errorf("Node 1: DELETE route failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = node2.delete("/api/v1/routes/" + routeID)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Errorf("Node 2: DELETE route failed: %v", err)
	}
	_ = resp.Body.Close()

	// Delete virtual devices
	resp, err = node1.delete("/api/v1/devices/virtual/" + micDev.URN)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Errorf("Node 1: DELETE virtual device failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = node2.delete("/api/v1/devices/virtual/" + spkDev.URN)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Errorf("Node 2: DELETE virtual device failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = node3.delete("/api/v1/devices/virtual/" + monDev.URN)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Errorf("Node 3: DELETE virtual device failed: %v", err)
	}
	_ = resp.Body.Close()

	t.Log("All e2e API operations completed and verified successfully.")
}
