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
		roomMgr.SetClusterManager(cm)
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

// TestClusterCrossNodeAudioRouting_FormatAgility_E2E verifies cross-node audio routing with
// dynamic channel remapping (mono <-> stereo) and sample rate conversion (44.1 kHz <-> 48 kHz):
// 1. Node A creates a 44.1 kHz 1-channel (mono) input device.
// 2. Node B creates a 48 kHz 2-channel (stereo) output device.
// 3. Audio flows from Node A (44.1 kHz mono) -> Node B (48 kHz stereo) over UDP.
// 4. Verifies both Left and Right channels on Node B capture the converted 48 kHz stereo signal.
// 5. Tests reverse routing from Node B (48 kHz stereo) -> Node A (44.1 kHz mono) with downmixing.
func TestClusterCrossNodeAudioRouting_FormatAgility_E2E(t *testing.T) {
	nodeA := spawnClusterNode(t, "node-fmt-A")
	defer nodeA.close()

	nodeB := spawnClusterNode(t, "node-fmt-B", nodeA.disc.OwnAddr())
	defer nodeB.close()

	// Wait for 2-node cluster convergence
	t.Log("Waiting for 2-node cluster convergence...")
	deadline := time.Now().Add(6 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		resp, err := nodeA.get("/api/v1/cluster/nodes")
		if err == nil && resp.StatusCode == http.StatusOK {
			var nodeResp cluster.ClusterNodesResponse
			if err := json.NewDecoder(resp.Body).Decode(&nodeResp); err == nil && len(nodeResp.Nodes) == 2 {
				_ = resp.Body.Close()
				converged = true
				break
			}
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !converged {
		t.Fatalf("cluster nodes failed to converge within deadline")
	}

	// 1. Forward direction: 44.1 kHz Mono (Node A) -> 48 kHz Stereo (Node B)
	micReqA := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-nodeA-mono-44k",
		Direction:        "input",
		Channels:         1,
		SampleRate:       44100,
		BufferSizeFrames: 441,
	}
	resp, err := nodeA.post("/api/v1/devices/virtual", micReqA)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node A failed: %v", err)
	}
	var devA v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devA)
	_ = resp.Body.Close()

	spkReqB := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-nodeB-stereo-48k",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err = nodeB.post("/api/v1/devices/virtual", spkReqB)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node B failed: %v", err)
	}
	var devB v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devB)
	_ = resp.Body.Close()

	routeID_AtoB := "route-mono44k-to-stereo48k"
	routeReq_AtoB := v1.CreateRouteRequest{
		ID:   routeID_AtoB,
		Name: "Node A Mono 44.1k to Node B Stereo 48k",
		Source: router.MatrixEndpoint{
			NodeID:   "node-fmt-A",
			DeviceID: devA.URN,
			Channels: []int{0},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   "node-fmt-B",
			DeviceID: devB.URN,
			Channels: []int{0, 1},
		},
		Coefficients: []float64{1.0, 1.0}, // 1x2: duplicate mono input to both stereo channels
		SlewMS:       20.0,
	}

	// Establish egress on Node A and ingress on Node B
	resp, err = nodeA.post("/api/v1/routes", routeReq_AtoB)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node A: POST /api/v1/routes failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = nodeB.post("/api/v1/routes", routeReq_AtoB)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node B: POST /api/v1/routes failed: %v", err)
	}
	_ = resp.Body.Close()

	streamA, okA := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devA.URN)
	streamB, okB := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devB.URN)
	if !okA || !okB || streamA == nil || streamB == nil {
		t.Fatalf("could not obtain VirtualAudioDevice streams for route AtoB")
	}

	// Inject 440 Hz mono sine wave at 44100 Hz into Node A
	framesMono := 441 // 10ms @ 44.1 kHz
	monoChunk := generateSineBlock(44100, 1, framesMono, 440.0, 0.707)

	doneFeedA := make(chan struct{})
	go func() {
		defer close(doneFeedA)
		for i := 0; i < 40; i++ {
			_, _ = streamA.InjectPush(monoChunk)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Verify converted 48 kHz stereo delivery on Node B
	t.Log("Verifying 44.1kHz mono -> 48kHz stereo conversion across nodes...")
	deadline = time.Now().Add(4 * time.Second)
	var capturedStereo []byte
	for time.Now().Before(deadline) {
		capturedStereo = streamB.CapturedBytes()
		// 10ms of 48kHz stereo is 480 * 2 * 2 = 1920 bytes
		if len(capturedStereo) >= 1920*2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	<-doneFeedA

	if len(capturedStereo) < 1920*2 {
		t.Fatalf("expected converted stereo frames at Node B: got %d bytes, want >= %d",
			len(capturedStereo), 1920*2)
	}

	if len(capturedStereo)%4 != 0 {
		t.Fatalf("expected captured buffer to be aligned to 4-byte stereo frames, got len=%d", len(capturedStereo))
	}

	// Verify both Left and Right stereo channels received signal
	maxLeft, maxRight := int16(0), int16(0)
	for i := 0; i+4 <= len(capturedStereo); i += 4 {
		sampleL := int16(binary.LittleEndian.Uint16(capturedStereo[i : i+2]))
		sampleR := int16(binary.LittleEndian.Uint16(capturedStereo[i+2 : i+4]))
		if sampleL < 0 {
			sampleL = -sampleL
		}
		if sampleR < 0 {
			sampleR = -sampleR
		}
		if sampleL > maxLeft {
			maxLeft = sampleL
		}
		if sampleR > maxRight {
			maxRight = sampleR
		}
	}

	if maxLeft < 1000 || maxRight < 1000 {
		t.Fatalf("stereo playout silent or missing channel: maxLeft=%d maxRight=%d (want > 1000)", maxLeft, maxRight)
	}
	t.Logf("SUCCESS: Mono 44.1k -> Stereo 48k converted & delivered (bytes=%d, peakL=%d, peakR=%d)",
		len(capturedStereo), maxLeft, maxRight)

	// Clean up route AtoB
	if resp, err := nodeA.delete("/api/v1/routes/" + routeID_AtoB); err == nil && resp != nil {
		_ = resp.Body.Close()
	}
	if resp, err := nodeB.delete("/api/v1/routes/" + routeID_AtoB); err == nil && resp != nil {
		_ = resp.Body.Close()
	}

	// 2. Reverse direction: 48 kHz Stereo (Node B) -> 44.1 kHz Mono (Node A)
	micReqB := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-nodeB-stereo-in",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err = nodeB.post("/api/v1/devices/virtual", micReqB)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node B stereo in failed: %v", err)
	}
	var devB_in v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devB_in)
	_ = resp.Body.Close()

	spkReqA := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-nodeA-mono-out",
		Direction:        "output",
		Channels:         1,
		SampleRate:       44100,
		BufferSizeFrames: 441,
	}
	resp, err = nodeA.post("/api/v1/devices/virtual", spkReqA)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node A mono out failed: %v", err)
	}
	var devA_out v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devA_out)
	_ = resp.Body.Close()

	routeID_BtoA := "route-stereo48k-to-mono44k"
	routeReq_BtoA := v1.CreateRouteRequest{
		ID:   routeID_BtoA,
		Name: "Node B Stereo 48k to Node A Mono 44.1k",
		Source: router.MatrixEndpoint{
			NodeID:   "node-fmt-B",
			DeviceID: devB_in.URN,
			Channels: []int{0, 1},
		},
		Destination: router.MatrixEndpoint{
			NodeID:   "node-fmt-A",
			DeviceID: devA_out.URN,
			Channels: []int{0},
		},
		Coefficients: []float64{0.5, 0.5}, // 2x1 downmix: average L and R
		SlewMS:       20.0,
	}

	resp, err = nodeB.post("/api/v1/routes", routeReq_BtoA)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node B: POST /api/v1/routes failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = nodeA.post("/api/v1/routes", routeReq_BtoA)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node A: POST /api/v1/routes failed: %v", err)
	}
	_ = resp.Body.Close()

	streamB_in, okB_in := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devB_in.URN)
	streamA_out, okA_out := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devA_out.URN)
	if !okB_in || !okA_out || streamB_in == nil || streamA_out == nil {
		t.Fatalf("could not obtain VirtualAudioDevice streams for route BtoA")
	}

	// Inject 440 Hz stereo sine wave at 48000 Hz into Node B
	framesStereo := 480 // 10ms @ 48 kHz
	stereoChunk := generateSineBlock(48000, 2, framesStereo, 440.0, 0.707)

	doneFeedB := make(chan struct{})
	go func() {
		defer close(doneFeedB)
		for i := 0; i < 40; i++ {
			_, _ = streamB_in.InjectPush(stereoChunk)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Verify converted 44.1 kHz mono delivery on Node A
	t.Log("Verifying 48kHz stereo -> 44.1kHz mono downmixing across nodes...")
	deadline = time.Now().Add(4 * time.Second)
	var capturedMono []byte
	for time.Now().Before(deadline) {
		capturedMono = streamA_out.CapturedBytes()
		// 10ms of 44.1kHz mono is 441 * 1 * 2 = 882 bytes
		if len(capturedMono) >= 882*2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	<-doneFeedB

	if len(capturedMono) < 882*2 {
		t.Fatalf("expected converted mono frames at Node A: got %d bytes, want >= %d",
			len(capturedMono), 882*2)
	}

	if len(capturedMono)%2 != 0 {
		t.Fatalf("expected captured buffer to be aligned to 2-byte mono frames, got len=%d", len(capturedMono))
	}

	maxMono := int16(0)
	for i := 0; i+2 <= len(capturedMono); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(capturedMono[i : i+2]))
		if sample < 0 {
			sample = -sample
		}
		if sample > maxMono {
			maxMono = sample
		}
	}

	if maxMono < 1000 {
		t.Fatalf("mono playout silent: maxMono=%d (want > 1000)", maxMono)
	}
	t.Logf("SUCCESS: Stereo 48k -> Mono 44.1k converted & delivered (bytes=%d, peakMono=%d)",
		len(capturedMono), maxMono)

	// Teardown
	for _, delCall := range []func() (*http.Response, error){
		func() (*http.Response, error) { return nodeB.delete("/api/v1/routes/" + routeID_BtoA) },
		func() (*http.Response, error) { return nodeA.delete("/api/v1/routes/" + routeID_BtoA) },
		func() (*http.Response, error) { return nodeA.delete("/api/v1/devices/virtual/" + devA.URN) },
		func() (*http.Response, error) { return nodeB.delete("/api/v1/devices/virtual/" + devB.URN) },
		func() (*http.Response, error) { return nodeB.delete("/api/v1/devices/virtual/" + devB_in.URN) },
		func() (*http.Response, error) { return nodeA.delete("/api/v1/devices/virtual/" + devA_out.URN) },
	} {
		if resp, err := delCall(); err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}

	t.Log("Format agility bidirectional test completed successfully.")
}

func TestClusterCrossNodeAudioRouting_RoomSynthesis_E2E(t *testing.T) {
	nodeA := spawnClusterNode(t, "node-room-A")
	defer nodeA.close()

	nodeB := spawnClusterNode(t, "node-room-B", nodeA.disc.OwnAddr())
	defer nodeB.close()

	// Wait for convergence
	t.Log("Waiting for 2-node cluster convergence...")
	deadline := time.Now().Add(5 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		snapA := nodeA.apiServer.ClusterManager().GetSnapshot()
		snapB := nodeB.apiServer.ClusterManager().GetSnapshot()
		if snapA != nil && snapB != nil && snapA.ActiveNodes >= 2 && snapB.ActiveNodes >= 2 {
			converged = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !converged {
		t.Fatalf("cluster failed to converge within 5s")
	}

	// 1. Provision Input on Node A and Output on Node B
	micReqA := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-room-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err := nodeA.post("/api/v1/devices/virtual", micReqA)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node A failed: %v", err)
	}
	var devMic v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devMic)
	_ = resp.Body.Close()

	spkReqB := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-room-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	}
	resp, err = nodeB.post("/api/v1/devices/virtual", spkReqB)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node B failed: %v", err)
	}
	var devSpk v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devSpk)
	_ = resp.Body.Close()

	// Force scrape on both cluster managers so device inventory is populated
	nodeA.apiServer.ClusterManager().ScrapeCluster(context.Background())
	nodeB.apiServer.ClusterManager().ScrapeCluster(context.Background())

	// 2. Create Room on Node A
	roomReq := map[string]string{
		"id":   "room-cross-synth",
		"name": "Cross Synthesis Room",
	}
	resp, err = nodeA.post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// 3. Bind Input (on Node A) and Output (on Node B) with node_id
	bindInReq := v1.RoomDeviceBindingRequest{
		DeviceID: devMic.URN,
		NodeID:   "node-room-A",
	}
	resp, err = nodeA.post("/api/v1/rooms/room-cross-synth/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/.../inputs failed: %v", err)
	}
	_ = resp.Body.Close()

	bindOutReq := v1.RoomDeviceBindingRequest{
		DeviceID: devSpk.URN,
		NodeID:   "node-room-B",
	}
	resp, err = nodeA.post("/api/v1/rooms/room-cross-synth/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/.../outputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// 4. Wait for CRDT convergence on Node B
	t.Log("Waiting for room-cross-synth to converge on Node B...")
	deadline = time.Now().Add(5 * time.Second)
	roomBReady := false
	for time.Now().Before(deadline) {
		rResp, rErr := nodeB.get("/api/v1/rooms/room-cross-synth")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			_ = rResp.Body.Close()
			if len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0 {
				roomBReady = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !roomBReady {
		t.Fatalf("room-cross-synth did not converge on Node B within 5s")
	}

	// 5. Verify Virtual Stream handles exist and stream audio
	micStream, okMic := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devMic.URN)
	spkStream, okSpk := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devSpk.URN)
	if !okMic || !okSpk || micStream == nil || spkStream == nil {
		t.Fatalf("could not obtain VirtualAudioDevice streams for room cross synth")
	}

	// Inject 440 Hz stereo sine wave on Node A
	frames := 480
	sineBlock := generateSineBlock(48000, 2, frames, 440.0, 0.707)

	doneFeed := make(chan struct{})
	go func() {
		defer close(doneFeed)
		for i := 0; i < 40; i++ {
			_, _ = micStream.InjectPush(sineBlock)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// 6. Verify delivery on Node B
	t.Log("Verifying room synthesized cross-node audio delivery to Node B speaker...")
	deadline = time.Now().Add(5 * time.Second)
	var captured []byte
	for time.Now().Before(deadline) {
		captured = spkStream.CapturedBytes()
		if len(captured) >= len(sineBlock)*2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	<-doneFeed

	if len(captured) < len(sineBlock)*2 {
		t.Fatalf("expected audio delivered to Node B speaker via synthesized room route: got %d bytes, want >= %d",
			len(captured), len(sineBlock)*2)
	}

	maxAmp := int16(0)
	for i := 0; i+2 <= len(captured); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(captured[i : i+2]))
		if sample < 0 {
			sample = -sample
		}
		if sample > maxAmp {
			maxAmp = sample
		}
	}

	if maxAmp < 1000 {
		t.Fatalf("room synthesized audio silent on Node B: maxAmp=%d (want > 1000)", maxAmp)
	}
	t.Logf("SUCCESS: Room synthesized cross-node audio delivered to Node B (bytes=%d, peakAmp=%d / 32767)",
		len(captured), maxAmp)
}
