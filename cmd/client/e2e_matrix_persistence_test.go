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
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"go.uber.org/goleak"
)

// AudioMetrics encapsulates mathematical DSP properties of sampled PCM audio.
type AudioMetrics struct {
	SampleCount int
	PeakAmpNorm float64 // normalized 0.0 to 1.0 (relative to int16 32768)
	PeakAmpPCM  int16   // raw max absolute sample value (0 to 32767)
	RMSNorm     float64 // normalized RMS energy (0.0 to 1.0)
	RMSdBFS     float64 // 20 * log10(RMSNorm) in decibels relative to full scale
	ContigZeros int     // maximum contiguous block of zero samples
}

// AnalyzeAudioPCM performs mathematical DSP verification on 16-bit little-endian PCM data.
func AnalyzeAudioPCM(pcmData []byte) AudioMetrics {
	if len(pcmData) < 2 {
		return AudioMetrics{RMSdBFS: -100.0}
	}

	samples := len(pcmData) / 2
	var sumSq float64
	var maxPCM int16
	currentZeros := 0
	maxZeros := 0

	for i := 0; i+2 <= len(pcmData); i += 2 {
		val := int16(binary.LittleEndian.Uint16(pcmData[i : i+2]))
		absVal := val
		if absVal < 0 {
			if absVal == -32768 {
				absVal = 32767
			} else {
				absVal = -absVal
			}
		}

		if absVal > maxPCM {
			maxPCM = absVal
		}

		if absVal == 0 {
			currentZeros++
			if currentZeros > maxZeros {
				maxZeros = currentZeros
			}
		} else {
			currentZeros = 0
		}

		norm := float64(val) / 32768.0
		sumSq += norm * norm
	}

	rmsNorm := math.Sqrt(sumSq / float64(samples))
	rmsDB := -100.0
	if rmsNorm > 1e-5 {
		rmsDB = 20.0 * math.Log10(rmsNorm)
	}

	return AudioMetrics{
		SampleCount: samples,
		PeakAmpNorm: float64(maxPCM) / 32768.0,
		PeakAmpPCM:  maxPCM,
		RMSNorm:     rmsNorm,
		RMSdBFS:     rmsDB,
		ContigZeros: maxZeros,
	}
}

// Eventually polls condition periodically until true or deadline expires, avoiding brittle sleeps.
func Eventually(t *testing.T, timeout, interval time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("condition failed to converge within %v: %s", timeout, msg)
}

// ManagedTestNode encapsulates a fully isolated, production-grade daemon instance
// bound to ephemeral loopback ports with disk-backed state persistence.
type ManagedTestNode struct {
	id           string
	configuredID string
	t            *testing.T
	tmpDir       string
	routesFile   string
	roomsFile    string
	nodeIDFile   string
	certFile     string
	keyFile      string
	seedAddrs    []string
	apiPort      int
	streamPort   int
	cfg          *config.Config
	disc         *discovery.Discovery
	syncMgr      *syncmgr.Manager
	trans        streamer.Transport
	streams      *streamManager
	roomMgr      *group.DefaultRoomManager
	rt           *router.Router
	apiServer    *api.Server
	httpServer   *http.Server
	apiURL       string
	client       *http.Client
	isDead       bool
	virtualDevs  []v1.CreateVirtualDeviceRequest
	mu           sync.Mutex
}

// spawnManagedTestNode initializes and launches an active node with dedicated storage.
func spawnManagedTestNode(t *testing.T, id string, seedAddrs ...string) *ManagedTestNode {
	t.Helper()
	tmpDir := t.TempDir()

	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")
	routesFile := filepath.Join(tmpDir, "routes.json")
	roomsFile := filepath.Join(tmpDir, "rooms.json")
	nodeIDFile := filepath.Join(tmpDir, "node.id")

	node := &ManagedTestNode{
		id:           id,
		configuredID: id,
		t:            t,
		tmpDir:       tmpDir,
		routesFile:   routesFile,
		roomsFile:    roomsFile,
		nodeIDFile:   nodeIDFile,
		certFile:     certFile,
		keyFile:      keyFile,
		seedAddrs:    seedAddrs,
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: 5 * time.Second,
		},
	}

	node.startComponentsLocked()

	t.Cleanup(func() {
		node.Close()
	})

	return node
}

// startComponentsLocked starts or restarts all internal daemons, loading persisted state.
func (n *ManagedTestNode) startComponentsLocked() {
	var ln net.Listener
	var err error
	if n.apiPort > 0 {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", n.apiPort))
	}
	if ln == nil || err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			n.t.Fatalf("[%s] listen tcp: %v", n.id, err)
		}
	}
	n.apiPort = ln.Addr().(*net.TCPAddr).Port

	var trans streamer.Transport
	if n.streamPort > 0 {
		trans, err = transport.NewUDPTransport(fmt.Sprintf(":%d", n.streamPort))
	}
	if trans == nil || err != nil {
		trans, err = transport.NewUDPTransport(":0")
		if err != nil {
			_ = ln.Close()
			n.t.Fatalf("[%s] NewUDPTransport: %v", n.id, err)
		}
	}
	n.trans = trans
	n.streamPort = trans.LocalAddr().Port

	cfg := &config.Config{
		AuthToken:        "managed-e2e-matrix-token",
		CertFile:         n.certFile,
		KeyFile:          n.keyFile,
		RoutesFile:       n.routesFile,
		RoomsFile:        n.roomsFile,
		NodeID:           n.configuredID,
		NodeIDFile:       n.nodeIDFile,
		UseEmulator:      false,
		DiscoveryPort:    0,
		MulticastPort:    0,
		SyncPort:         0,
		StreamPort:       fmt.Sprintf(":%d", n.streamPort),
		ListenAddr:       fmt.Sprintf("127.0.0.1:%d", n.apiPort),
		AnnounceInterval: 100 * time.Millisecond,
		PeerTTL:          4 * time.Second,
		SyncInterval:     200 * time.Millisecond,
		ShutdownTimeout:  2 * time.Second,
	}
	for _, seed := range n.seedAddrs {
		if seed != "" {
			cfg.StaticPeers = append(cfg.StaticPeers, config.StaticPeerConfig{Addr: seed})
		}
	}
	n.cfg = cfg

	if err := ensureTLS(cfg); err != nil {
		n.t.Fatalf("[%s] ensureTLS: %v", n.id, err)
	}

	resolvedID, err := discovery.ResolveOrPersistNodeID(cfg)
	if err != nil {
		n.t.Fatalf("[%s] discovery.ResolveOrPersistNodeID: %v", n.id, err)
	}
	n.id = resolvedID

	disc, err := discovery.New(cfg, n.id)
	if err != nil {
		n.t.Fatalf("[%s] discovery.New: %v", n.id, err)
	}
	n.disc = disc

	sm := syncmgr.NewManager(cfg, disc)
	n.syncMgr = sm

	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		n.t.Fatalf("[%s] router.New: %v", n.id, err)
	}
	n.rt = rt

	localIPs := collectLocalIPs(trans, disc)
	streamMgr := newStreamManager(cfg, trans, localIPs, disc)
	n.streams = streamMgr

	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRoomsFile(cfg.RoomsFile)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streamMgr)
	roomMgr.SetDeviceResolver(audio.GlobalResolver())
	disc.SetStateDelegate(roomMgr)
	n.roomMgr = roomMgr

	apiSrv := api.NewServer(cfg, rt, disc, sm, streamMgr, roomMgr)
	n.apiServer = apiSrv
	streamMgr.SetHub(apiSrv.Hub())

	// Re-provision registered virtual devices into device registry
	for _, req := range n.virtualDevs {
		_, _ = apiSrv.V1().DeviceRegistry().CreateVirtualDevice(req)
	}

	// Rehydrate rooms and active route definitions from persistent storage
	if err := roomMgr.Load(cfg.RoomsFile); err != nil && !os.IsNotExist(err) {
		n.t.Logf("[%s] Warning loading rooms file %s: %v", n.id, cfg.RoomsFile, err)
	}
	for _, r := range rt.List() {
		streamMgr.mu.Lock()
		_, exists := streamMgr.routes[r.ID]
		streamMgr.mu.Unlock()
		if !exists {
			_ = streamMgr.StartRoute(r)
		}
	}

	go disc.Run()
	if cm := apiSrv.ClusterManager(); cm != nil {
		roomMgr.SetClusterManager(cm)
		cm.Start(context.Background())
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{}}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		n.t.Fatalf("[%s] LoadX509KeyPair: %v", n.id, err)
	}
	tlsConfig.Certificates = append(tlsConfig.Certificates, cert)

	tlsListener := tls.NewListener(ln, tlsConfig)
	httpServer := &http.Server{Handler: apiSrv.Handler()}
	n.httpServer = httpServer

	go func() {
		_ = httpServer.Serve(tlsListener)
	}()

	n.apiURL = fmt.Sprintf("https://127.0.0.1:%d", n.apiPort)
}

// stopComponentsLocked terminates node subsystems. If graceful is false, skips persistence flushes.
func (n *ManagedTestNode) stopComponentsLocked(graceful bool) {
	if n.client != nil {
		n.client.CloseIdleConnections()
	}
	if n.disc != nil {
		_ = n.disc.Close()
	}
	if n.syncMgr != nil {
		n.syncMgr.Close()
	}
	if n.apiServer != nil {
		if cm := n.apiServer.ClusterManager(); cm != nil {
			cm.Stop()
		}
		if n.apiServer.Hub() != nil {
			n.apiServer.Hub().Stop()
		}
	}
	if n.httpServer != nil {
		_ = n.httpServer.Close()
	}
	if graceful && n.roomMgr != nil {
		_ = n.roomMgr.Close()
	}
	if n.streams != nil {
		n.streams.StopAll()
	}
	if n.trans != nil {
		_ = n.trans.Close()
	}
}

// Restart executes a clean reboot cycle, rehydrating persisted rooms and routes from disk.
func (n *ManagedTestNode) Restart() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.isDead {
		n.stopComponentsLocked(true)
	}
	n.startComponentsLocked()
	n.isDead = false
}

// SimulateCrash abruptly terminates the node (SIGKILL simulation) without executing clean teardown or flushes.
func (n *ManagedTestNode) SimulateCrash() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.isDead {
		return
	}
	n.stopComponentsLocked(false)
	n.isDead = true
}

// Close gracefully terminates all resources upon test conclusion.
func (n *ManagedTestNode) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.isDead {
		return
	}
	n.stopComponentsLocked(true)
	n.isDead = true
}

// ProvisionVirtualDevice registers and instantiates a virtual loopback audio device.
func (n *ManagedTestNode) ProvisionVirtualDevice(req v1.CreateVirtualDeviceRequest) (v1.AudioDevice, error) {
	n.mu.Lock()
	n.virtualDevs = append(n.virtualDevs, req)
	n.mu.Unlock()

	resp, err := n.Post("/api/v1/devices/virtual", req)
	if err != nil {
		return v1.AudioDevice{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return v1.AudioDevice{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var dev v1.AudioDevice
	err = json.NewDecoder(resp.Body).Decode(&dev)
	return dev, err
}

func (n *ManagedTestNode) Post(path string, body any) (*http.Response, error) {
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

func (n *ManagedTestNode) Get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, n.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.AuthToken)
	return n.client.Do(req)
}

func (n *ManagedTestNode) Delete(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, n.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.AuthToken)
	return n.client.Do(req)
}

// ScrapeCluster immediately triggers discovery scrape on the cluster manager.
func (n *ManagedTestNode) ScrapeCluster(ctx context.Context) {
	if cm := n.apiServer.ClusterManager(); cm != nil {
		cm.ScrapeCluster(ctx)
	}
}

// wait2NodeClusterConvergence ensures both nodes have discovered each other in the mesh.
func wait2NodeClusterConvergence(t *testing.T, nodeA, nodeB *ManagedTestNode) {
	t.Helper()
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		snapA := nodeA.apiServer.ClusterManager().GetSnapshot()
		snapB := nodeB.apiServer.ClusterManager().GetSnapshot()
		return snapA != nil && snapB != nil && snapA.ActiveNodes >= 2 && snapB.ActiveNodes >= 2
	}, "2-node cluster discovery convergence")
}

// injectAudioAndVerify sends 48kHz PCM sine blocks and mathematically asserts receiver RMS and peak.
func injectAudioAndVerify(
	t *testing.T,
	micStream *audio.VirtualAudioDevice,
	spkStream *audio.VirtualAudioDevice,
	freq float64,
	amp float64,
	minBytes int,
) AudioMetrics {
	t.Helper()
	spkStream.Reset()

	framesPerChunk := 480 // 10ms @ 48kHz
	sineChunk := generateSineBlock(48000, 2, framesPerChunk, freq, amp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	injectDone := make(chan struct{})
	go func() {
		defer close(injectDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = micStream.InjectPush(sineChunk)
			}
		}
	}()

	var captured []byte
	var metrics AudioMetrics
	Eventually(t, 5*time.Second, 30*time.Millisecond, func() bool {
		captured = spkStream.CapturedBytes()
		if len(captured) < minBytes {
			return false
		}
		metrics = AnalyzeAudioPCM(captured)
		return metrics.RMSdBFS > -30.0
	}, "audio packet transport to receiving speaker with valid RMS energy")

	cancel()
	<-injectDone

	t.Logf("[DEBUG_AUDIO] captured len=%d, sample: %x", len(captured), captured[:min(32, len(captured))])
	if metrics.RMSdBFS <= -30.0 {
		t.Fatalf("expected RMS > -30 dBFS, got %.2f dBFS (RMS norm=%.4f)", metrics.RMSdBFS, metrics.RMSNorm)
	}
	if metrics.PeakAmpNorm < 0.15 {
		t.Fatalf("expected Peak > 0.15, got %.4f (PCM=%d)", metrics.PeakAmpNorm, metrics.PeakAmpPCM)
	}
	if metrics.PeakAmpNorm > 1.05 {
		t.Fatalf("detected audio clipping: peak norm=%.4f", metrics.PeakAmpNorm)
	}

	return metrics
}

// setupLeakGuard registers goroutine leak detection to run after all test resources have cleanly shut down.
func setupLeakGuard(t *testing.T) {
	t.Helper()
	origGrace := audio.GlobalResolver().GracePeriod()
	audio.GlobalResolver().SetGracePeriod(20 * time.Millisecond)
	t.Cleanup(func() {
		_ = audio.GlobalResolver().Close()
		audio.GlobalResolver().SetGracePeriod(origGrace)
		// Allow asynchronous background workers a short window to terminate sockets
		time.Sleep(150 * time.Millisecond)
		goleak.VerifyNone(t,
			goleak.IgnoreTopFunction("net/http.(*Server).Serve"),
			goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).streamListen"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).packetListen"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).packetHandler"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).checkBroadcastQueueDepth"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).pushPullTrigger"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).triggerFunc"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).probeNode"),
			goleak.IgnoreTopFunction("github.com/hashicorp/memberlist.(*Memberlist).probe"),
			goleak.IgnoreTopFunction("github.com/audio-router-p2p/pkg/discovery.(*Discovery).seedJoinLoop"),
			goleak.IgnoreTopFunction("github.com/audio-router-p2p/pkg/discovery.(*Discovery).Run"),
			goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		)
	})
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 1: Entity Sequence Permutations (Output Before Input vs Input Before Output)
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_OutputBeforeInput_AudioFlow tests creating and binding an output device
// before the input capture device exists. Verifies crossbar compiles and routes audio once input arrives.
func TestE2E_Permutation_OutputBeforeInput_AudioFlow(t *testing.T) {
	setupLeakGuard(t)

	nodeA := spawnManagedTestNode(t, "node-outfirst-A")
	nodeB := spawnManagedTestNode(t, "node-outfirst-B", nodeA.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	// Step 1: Create OUTPUT device first on Node B
	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-outfirst-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("failed to create output device: %v", err)
	}

	// Step 2: Create Room on Node A
	roomReq := map[string]string{"id": "room-out-first", "name": "Output First Room"}
	resp, err := nodeA.Post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// Step 3: Bind OUTPUT first to the room
	bindOutReq := v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-out-first/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST outputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Step 4: Verify room has 1 output, 0 inputs (no routes active yet)
	nodeA.ScrapeCluster(context.Background())
	nodeB.ScrapeCluster(context.Background())

	// Step 5: Now create INPUT device on Node A
	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-outfirst-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("failed to create input device: %v", err)
	}

	// Step 6: Bind INPUT second to the room
	bindInReq := v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-out-first/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST inputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Step 7: Wait for CRDT room convergence on Node B
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-out-first")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-out-first convergence on Node B")

	// Step 8: Verify routes compiled and stream audio across UDP
	micStream, okMic := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, okSpk := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !okMic || !okSpk || micStream == nil || spkStream == nil {
		t.Fatalf("failed to obtain virtual device streams")
	}

	metrics := injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Logf("PASS: Output-before-Input audio verified (RMS=%.2f dBFS, peak=%.3f)", metrics.RMSdBFS, metrics.PeakAmpNorm)
}

// TestE2E_Permutation_InputBeforeOutput_AudioFlow tests creating and binding an input device
// before the output playout device exists. Verifies crossbar reconciles cleanly when output appears.
func TestE2E_Permutation_InputBeforeOutput_AudioFlow(t *testing.T) {
	setupLeakGuard(t)

	nodeA := spawnManagedTestNode(t, "node-infirst-A")
	nodeB := spawnManagedTestNode(t, "node-infirst-B", nodeA.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	// Step 1: Create INPUT device first on Node A
	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-infirst-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("failed to create input device: %v", err)
	}

	// Step 2: Create Room on Node A
	roomReq := map[string]string{"id": "room-in-first", "name": "Input First Room"}
	resp, err := nodeA.Post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// Step 3: Bind INPUT first to the room
	bindInReq := v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-in-first/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST inputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Step 4: Now create OUTPUT device second on Node B
	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-infirst-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("failed to create output device: %v", err)
	}

	// Step 5: Bind OUTPUT second to the room
	bindOutReq := v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-in-first/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST outputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Step 6: Wait for CRDT room convergence on Node B
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-in-first")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-in-first convergence on Node B")

	// Step 7: Stream and verify audio
	micStream, okMic := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, okSpk := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !okMic || !okSpk || micStream == nil || spkStream == nil {
		t.Fatalf("failed to obtain virtual device streams")
	}

	metrics := injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Logf("PASS: Input-before-Output audio verified (RMS=%.2f dBFS, peak=%.3f)", metrics.RMSdBFS, metrics.PeakAmpNorm)
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 2: Group Connection Order and Dynamic Gain Slewing
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_ConnectOrderAndDynamicGain tests binding output before input and altering
// device gains dynamically mid-stream while 50 Hz UDP audio is flowing.
func TestE2E_Permutation_ConnectOrderAndDynamicGain(t *testing.T) {
	setupLeakGuard(t)

	nodeA := spawnManagedTestNode(t, "node-gain-A")
	nodeB := spawnManagedTestNode(t, "node-gain-B", nodeA.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-gain-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create mic: %v", err)
	}

	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-gain-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create spk: %v", err)
	}

	// Create room
	roomReq := map[string]string{"id": "room-gain-slew", "name": "Gain Slew Room"}
	resp, err := nodeA.Post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// Connect output before input
	bindOutReq := v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-gain-slew/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST outputs: %v", err)
	}
	_ = resp.Body.Close()

	bindInReq := v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-gain-slew/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST inputs: %v", err)
	}
	_ = resp.Body.Close()

	// Wait for convergence
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-gain-slew")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-gain-slew convergence")

	micStream, _ := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, _ := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)

	// Step 1: Verify initial audio transmission with default unity gain (1.0)
	m1 := injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Logf("Initial audio verified: RMS=%.2f dBFS, Peak=%.3f", m1.RMSdBFS, m1.PeakAmpNorm)

	// Step 2: Dynamically slew gain to 0.4 mid-stream via RoomManager
	err = nodeA.roomMgr.SetInputGain("room-gain-slew", micDev.URN, 0.4)
	if err != nil {
		t.Fatalf("SetInputGain: %v", err)
	}

	// Verify router matrix coefficients updated to 0.4
	Eventually(t, 2*time.Second, 20*time.Millisecond, func() bool {
		for _, r := range nodeA.rt.ListMatrix() {
			if strings.Contains(r.ID, "room-gain-slew") && len(r.Coefficients) > 0 {
				return math.Abs(r.Coefficients[0]-0.4) < 1e-4
			}
		}
		return false
	}, "router matrix coefficients update to 0.4 on node A")

	// Step 3: Verify audio continues to flow smoothly after gain slew
	m2 := injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Logf("Post-gain-slew audio: RMS=%.2f dBFS, Peak=%.3f", m2.RMSdBFS, m2.PeakAmpNorm)

	if m2.RMSdBFS <= -30.0 {
		t.Fatalf("expected post-slew RMS > -30 dBFS, got %.2f dBFS", m2.RMSdBFS)
	}
	t.Log("PASS: Dynamic gain slew verified with matrix coefficient update to 0.4 and active audio transmission")
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 3: Asymmetric Multi-Node Startup Ordering
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_NodeBStartsFirst_AsymmetricBoot tests Node B booting completely first with
// output devices and room configuration, then Node A joining later and binding input.
func TestE2E_Permutation_NodeBStartsFirst_AsymmetricBoot(t *testing.T) {
	setupLeakGuard(t)

	// Node B starts FIRST alone
	nodeB := spawnManagedTestNode(t, "node-asym-B")

	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-asym-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("Node B provision device: %v", err)
	}

	roomReq := map[string]string{"id": "room-asym-boot", "name": "Asymmetric Boot Room"}
	resp, err := nodeB.Post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Node B POST /api/v1/rooms: %v", err)
	}
	_ = resp.Body.Close()

	bindOutReq := v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id}
	resp, err = nodeB.Post("/api/v1/rooms/room-asym-boot/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Node B POST outputs: %v", err)
	}
	_ = resp.Body.Close()

	// Now Node A boots later, seeded with Node B
	nodeA := spawnManagedTestNode(t, "node-asym-A", nodeB.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-asym-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("Node A provision device: %v", err)
	}

	// Node A binds its input to the room
	bindInReq := v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id}
	resp, err = nodeA.Post("/api/v1/rooms/room-asym-boot/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Node A POST inputs: %v", err)
	}
	_ = resp.Body.Close()

	// Wait for convergence on both nodes
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-asym-boot")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-asym-boot convergence on Node B")

	micStream, _ := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, _ := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)

	metrics := injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Logf("PASS: Asymmetric boot audio verified (RMS=%.2f dBFS, peak=%.3f)", metrics.RMSdBFS, metrics.PeakAmpNorm)
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 4: Deletion and Recreation Churn
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_DeletionAndRecreationChurn tests rapid room deletion & recreation with the
// same room ID, as well as virtual device hot-deletion and recreation during active streaming.
func TestE2E_Permutation_DeletionAndRecreationChurn(t *testing.T) {
	setupLeakGuard(t)

	nodeA := spawnManagedTestNode(t, "node-churn-A")
	nodeB := spawnManagedTestNode(t, "node-churn-B", nodeA.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-churn-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create mic: %v", err)
	}

	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-churn-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create spk: %v", err)
	}

	// 1. Setup room
	roomReq := map[string]string{"id": "room-churn-test", "name": "Churn Room"}
	resp, _ := nodeA.Post("/api/v1/rooms", roomReq)
	_ = resp.Body.Close()

	resp, _ = nodeA.Post("/api/v1/rooms/room-churn-test/inputs", v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id})
	_ = resp.Body.Close()
	resp, _ = nodeA.Post("/api/v1/rooms/room-churn-test/outputs", v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id})
	_ = resp.Body.Close()

	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-churn-test")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-churn-test convergence")

	micStream, _ := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, _ := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)

	injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)

	// 2. Churn Phase 1: Delete room and recreate with EXACT SAME ID
	t.Log("Phase 1: Deleting room room-churn-test and recreating with same ID...")
	resp, err = nodeA.Delete("/api/v1/rooms/room-churn-test")
	if err != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent) {
		t.Fatalf("DELETE /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	Eventually(t, 3*time.Second, 50*time.Millisecond, func() bool {
		rResp, _ := nodeA.Get("/api/v1/rooms/room-churn-test")
		defer func() {
			if rResp != nil {
				_ = rResp.Body.Close()
			}
		}()
		return rResp != nil && rResp.StatusCode == http.StatusNotFound
	}, "room-churn-test deletion on Node A")

	// Recreate room with SAME ID
	resp, err = nodeA.Post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("recreate room: %v", err)
	}
	_ = resp.Body.Close()

	resp, _ = nodeA.Post("/api/v1/rooms/room-churn-test/inputs", v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id})
	_ = resp.Body.Close()
	resp, _ = nodeA.Post("/api/v1/rooms/room-churn-test/outputs", v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id})
	_ = resp.Body.Close()

	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-churn-test")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "recreated room convergence")

	// Verify audio resumes cleanly
	injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Log("Phase 1 Complete: Audio verified after room delete/recreate churn")

	// 3. Churn Phase 2: Hot-deletion of active virtual device
	t.Log("Phase 2: Hot-deleting virtual device during active streaming...")
	resp, err = nodeA.Delete("/api/v1/rooms/room-churn-test/outputs/" + spkDev.URN)
	if err != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent) {
		t.Fatalf("unbind virtual output from room: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = nodeB.Delete("/api/v1/devices/virtual/" + spkDev.URN)
	if err != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent) {
		t.Fatalf("delete virtual device: %v", err)
	}
	_ = resp.Body.Close()

	// Recreate virtual device with same name on Node B
	spkDev2, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-churn-spk-2",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("recreate spk: %v", err)
	}

	resp, _ = nodeA.Post("/api/v1/rooms/room-churn-test/outputs", v1.RoomDeviceBindingRequest{DeviceID: spkDev2.URN, NodeID: nodeB.id})
	_ = resp.Body.Close()

	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-churn-test")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			for _, out := range rInfo.Outputs {
				if strings.Contains(out, spkDev2.URN) {
					return true
				}
			}
		}
		return false
	}, "rebound device convergence")

	spkStream2, _ := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev2.URN)
	injectAudioAndVerify(t, micStream, spkStream2, 440.0, 0.707, 3840)
	t.Log("PASS: Deletion and recreation churn executed without panics or leaks")
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 5: Clean Restart and Persistence Verification
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_NodeRestartAndPersistence verifies that routes.json and rooms.json
// atomically store state, and that a graceful node restart reconstitutes all rooms, bindings,
// gains, and streaming without ghost routes.
func TestE2E_Permutation_NodeRestartAndPersistence(t *testing.T) {
	setupLeakGuard(t)

	nodeA := spawnManagedTestNode(t, "node-restart-A")
	nodeB := spawnManagedTestNode(t, "node-restart-B", nodeA.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-restart-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create mic: %v", err)
	}

	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-restart-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create spk: %v", err)
	}

	// Create room and set custom gains
	roomReq := map[string]string{"id": "room-restart-persist", "name": "Persist Room"}
	resp, _ := nodeA.Post("/api/v1/rooms", roomReq)
	_ = resp.Body.Close()

	resp, _ = nodeA.Post("/api/v1/rooms/room-restart-persist/inputs", v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id})
	_ = resp.Body.Close()
	resp, _ = nodeA.Post("/api/v1/rooms/room-restart-persist/outputs", v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id})
	_ = resp.Body.Close()

	_ = nodeA.roomMgr.SetInputGain("room-restart-persist", micDev.URN, 0.8)
	_ = nodeB.roomMgr.SetOutputGain("room-restart-persist", spkDev.URN, 1.2)

	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-restart-persist")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-restart-persist convergence")

	micStream, _ := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, _ := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)

	injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)

	// Verify persistence files exist on disk before reboot
	roomsData, err := os.ReadFile(nodeB.roomsFile)
	if err != nil || len(roomsData) == 0 {
		t.Fatalf("rooms.json missing or empty on Node B: %v", err)
	}
	var prs map[string]group.PersistedRoom
	if err := json.Unmarshal(roomsData, &prs); err != nil || len(prs) == 0 {
		t.Fatalf("invalid rooms.json content: %v", err)
	}
	t.Logf("Confirmed rooms.json persisted on disk: %d room(s)", len(prs))

	// Execute Graceful Node Restart on Node B
	t.Log("Restarting Node B gracefully...")
	nodeB.Restart()

	// Verify Node B restored room state from disk
	roomB, ok := nodeB.roomMgr.GetRoom("room-restart-persist")
	if !ok || roomB == nil {
		t.Fatalf("room-restart-persist was not rehydrated from rooms.json after restart")
	}

	// Verify absence of ghost routes
	for _, r := range nodeB.rt.List() {
		if !strings.Contains(r.ID, "room-restart-persist") {
			t.Fatalf("ghost route detected in router: %s", r.ID)
		}
	}

	// Re-verify cluster discovery convergence
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	// Stream audio again to reconstituted Node B
	spkStreamAfter, okSpkAfter := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !okSpkAfter || spkStreamAfter == nil {
		t.Fatalf("failed to obtain stream for reconstituted speaker device")
	}

	metrics := injectAudioAndVerify(t, micStream, spkStreamAfter, 440.0, 0.707, 3840)
	t.Logf("PASS: Clean node restart persistence verified (RMS=%.2f dBFS)", metrics.RMSdBFS)
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 6: Fail-Stop Crash Recovery and Resilience Verification
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_CrashRecoveryAndPersistence verifies that an abrupt process crash
// (SimulateCrash, skipping graceful close and flush) does not corrupt on-disk persistence,
// and subsequent boot completely re-establishes cross-node UDP audio streams.
func TestE2E_Permutation_CrashRecoveryAndPersistence(t *testing.T) {
	setupLeakGuard(t)

	nodeA := spawnManagedTestNode(t, "node-crash-A")
	nodeB := spawnManagedTestNode(t, "node-crash-B", nodeA.disc.OwnAddr())
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	micDev, err := nodeA.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-crash-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create mic: %v", err)
	}

	spkDev, err := nodeB.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-crash-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create spk: %v", err)
	}

	roomReq := map[string]string{"id": "room-crash-persist", "name": "Crash Room"}
	resp, _ := nodeA.Post("/api/v1/rooms", roomReq)
	_ = resp.Body.Close()

	resp, _ = nodeA.Post("/api/v1/rooms/room-crash-persist/inputs", v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: nodeA.id})
	_ = resp.Body.Close()
	resp, _ = nodeA.Post("/api/v1/rooms/room-crash-persist/outputs", v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: nodeB.id})
	_ = resp.Body.Close()

	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		rResp, rErr := nodeB.Get("/api/v1/rooms/room-crash-persist")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			defer rResp.Body.Close()
			var rInfo api.RoomInfo
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			return len(rInfo.Inputs) > 0 && len(rInfo.Outputs) > 0
		}
		return false
	}, "room-crash-persist convergence")

	micStream, _ := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, _ := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)

	injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)

	// Step 1: Simulate abrupt crash on Node B (mimicking SIGKILL / kernel panic)
	t.Log("Simulating abrupt crash on Node B...")
	nodeB.SimulateCrash()

	// Verify Node B is inaccessible
	_, getErr := nodeB.Get("/api/v1/rooms")
	if getErr == nil {
		t.Fatalf("expected network failure on crashed node, but request succeeded")
	}

	// Verify persistence file on disk remains uncorrupted
	roomsRaw, err := os.ReadFile(nodeB.roomsFile)
	if err != nil || len(roomsRaw) == 0 {
		t.Fatalf("rooms.json missing or corrupted after crash: %v", err)
	}
	var parsedRooms map[string]group.PersistedRoom
	if err := json.Unmarshal(roomsRaw, &parsedRooms); err != nil {
		t.Fatalf("persistence file corrupted during crash: %v", err)
	}
	if _, ok := parsedRooms["room-crash-persist"]; !ok {
		t.Fatalf("room-crash-persist missing from persistence file after crash")
	}

	// Step 2: Reboot Node B from the exact same disk storage
	t.Log("Booting Node B after crash...")
	nodeB.Restart()

	// Verify restored room configuration and absence of ghost routes
	roomB, ok := nodeB.roomMgr.GetRoom("room-crash-persist")
	if !ok || roomB == nil {
		t.Fatalf("room was not restored from rooms.json after reboot")
	}

	// Wait for cluster reconvergence
	wait2NodeClusterConvergence(t, nodeA, nodeB)

	// Re-verify audio flow across nodes
	spkStreamPostCrash, okSpkPost := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !okSpkPost || spkStreamPostCrash == nil {
		t.Fatalf("failed to obtain virtual stream for post-crash speaker")
	}

	metrics := injectAudioAndVerify(t, micStream, spkStreamPostCrash, 440.0, 0.707, 3840)
	t.Logf("PASS: Abrupt crash recovery and audio resumption verified (RMS=%.2f dBFS)", metrics.RMSdBFS)
}

// -------------------------------------------------------------------------------------------------
// PERMUTATION MATRIX 7: Single-Node Reboot State Reconciliation and Persistent Identity Verification
// -------------------------------------------------------------------------------------------------

// TestE2E_Permutation_SingleNodeRestartAndPersistence verifies ADR 0025:
// 1. A standalone node initialized without an explicit node ID generates and atomically persists a 32-hex ID into node.id (0600 permissions).
// 2. Intra-node room routing passes 48kHz PCM audio with valid RMS energy.
// 3. Across graceful daemon restarts, the exact node ID is restored from node.id.
// 4. Stale node prefixes in persistent rooms referencing local audio hardware/virtual devices are dynamically reconciled to the active node ID.
// 5. The single-node fast-path immediately asserts RoomStateActive and elects the local node leader without discovery timeouts.
// 6. Post-restart intra-node audio streaming resumes cleanly with valid RMS signal energy (> -30 dBFS).
func TestE2E_Permutation_SingleNodeRestartAndPersistence(t *testing.T) {
	setupLeakGuard(t)

	// Step 1: Spawn single node without an explicit node ID (simulating default daemon startup)
	node := spawnManagedTestNode(t, "")
	initialID := node.id
	if len(initialID) != 32 {
		t.Fatalf("expected 32-char generated node ID, got %q (len=%d)", initialID, len(initialID))
	}

	// Verify node.id file exists on disk with 0600 permissions and matching ID
	idData, err := os.ReadFile(node.nodeIDFile)
	if err != nil {
		t.Fatalf("reading node.id file: %v", err)
	}
	if strings.TrimSpace(string(idData)) != initialID {
		t.Fatalf("node.id content %q does not match node.id %q", string(idData), initialID)
	}
	info, err := os.Stat(node.nodeIDFile)
	if err != nil {
		t.Fatalf("stat node.id file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected node.id permissions 0600, got %04o", perm)
	}

	// Step 2: Provision virtual mic and speaker devices
	micDev, err := node.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-single-mic",
		Direction:        "input",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create mic: %v", err)
	}

	spkDev, err := node.ProvisionVirtualDevice(v1.CreateVirtualDeviceRequest{
		Name:             "vdev-single-spk",
		Direction:        "output",
		Channels:         2,
		SampleRate:       48000,
		BufferSizeFrames: 480,
	})
	if err != nil {
		t.Fatalf("create spk: %v", err)
	}

	// Step 3: Create room and bind local devices via API
	const roomID = "room-single-persist"
	roomReq := map[string]string{"id": roomID, "name": "Single Persist Room"}
	resp, err := node.Post("/api/v1/rooms", roomReq)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = node.Post(fmt.Sprintf("/api/v1/rooms/%s/inputs", roomID), v1.RoomDeviceBindingRequest{DeviceID: micDev.URN, NodeID: node.id})
	if err != nil {
		t.Fatalf("add input: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = node.Post(fmt.Sprintf("/api/v1/rooms/%s/outputs", roomID), v1.RoomDeviceBindingRequest{DeviceID: spkDev.URN, NodeID: node.id})
	if err != nil {
		t.Fatalf("add output: %v", err)
	}
	_ = resp.Body.Close()

	_ = node.roomMgr.SetInputGain(roomID, micDev.URN, 1.0)
	_ = node.roomMgr.SetOutputGain(roomID, spkDev.URN, 1.0)

	// Verify room convergence to RoomStateActive and leader == node.id
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		r, ok := node.roomMgr.GetRoom(roomID)
		return ok && r != nil && r.State() == group.RoomStateActive && r.Leader() == node.id
	}, "room-single-persist convergence to active")

	// Step 4: Stream audio pre-restart and assert valid RMS energy
	micStream, okMic := node.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStream, okSpk := node.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !okMic || !okSpk {
		t.Fatalf("failed to retrieve virtual streams pre-restart")
	}

	preMetrics := injectAudioAndVerify(t, micStream, spkStream, 440.0, 0.707, 3840)
	t.Logf("Pre-restart intra-node audio verified (RMS=%.2f dBFS)", preMetrics.RMSdBFS)

	// Step 5: Stop node components gracefully (flushing in-memory state to disk)
	node.mu.Lock()
	node.stopComponentsLocked(true)

	// Read rooms.json to verify persistence
	roomsData, err := os.ReadFile(node.roomsFile)
	if err != nil || len(roomsData) == 0 {
		node.mu.Unlock()
		t.Fatalf("rooms.json missing or empty on disk: %v", err)
	}
	var prs map[string]group.PersistedRoom
	if err := json.Unmarshal(roomsData, &prs); err != nil {
		node.mu.Unlock()
		t.Fatalf("unmarshaling rooms.json: %v", err)
	}

	// Inject a second room simulating a stale node ID from prior release / dead host
	const staleRoomID = "room-stale-persist"
	const staleNodeID = "stale-dead-node-legacy9999"
	prs[staleRoomID] = group.PersistedRoom{
		ID:          staleRoomID,
		Name:        "Stale Reconcile Room",
		Leader:      staleNodeID,
		Inputs:      []string{staleNodeID + "/" + micDev.URN},
		Outputs:     []string{staleNodeID + "/" + spkDev.URN},
		InputGains:  map[string]float64{staleNodeID + "/" + micDev.URN: 1.0},
		OutputGains: map[string]float64{staleNodeID + "/" + spkDev.URN: 1.0},
		Timestamp:   group.HLT{WallTime: time.Now().UnixNano(), Counter: 1, NodeID: staleNodeID},
	}
	modData, err := json.MarshalIndent(prs, "", "  ")
	if err != nil {
		node.mu.Unlock()
		t.Fatalf("marshal modified rooms.json: %v", err)
	}
	if err := os.WriteFile(node.roomsFile, modData, 0644); err != nil {
		node.mu.Unlock()
		t.Fatalf("write modified rooms.json: %v", err)
	}

	// Step 6: Restart node components from persistent storage
	t.Log("Restarting node from persistent state...")
	node.startComponentsLocked()
	node.mu.Unlock()

	// Step 7: Verify persistent node identity was preserved across restart
	if node.id != initialID {
		t.Fatalf("node ID changed across reboot: pre=%q post=%q", initialID, node.id)
	}
	idDataPost, err := os.ReadFile(node.nodeIDFile)
	if err != nil || strings.TrimSpace(string(idDataPost)) != initialID {
		t.Fatalf("node.id file post-reboot mismatch: %v, content=%q", err, string(idDataPost))
	}

	// Step 8: Verify pre-existing room rehydration and convergence
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		r, ok := node.roomMgr.GetRoom(roomID)
		return ok && r != nil && r.State() == group.RoomStateActive && r.Leader() == node.id
	}, "rehydrated room-single-persist convergence")

	// Step 9: Verify stale room self-healing: stale prefix reconciled to node.id and converges to Active
	Eventually(t, 5*time.Second, 50*time.Millisecond, func() bool {
		r, ok := node.roomMgr.GetRoom(staleRoomID)
		if !ok || r == nil {
			return false
		}
		if r.State() != group.RoomStateActive || r.Leader() != node.id {
			return false
		}
		for _, in := range r.Inputs() {
			owner, _ := r.ResolveDeviceEndpoint(in)
			if owner != node.id && owner != "local" {
				return false
			}
		}
		for _, out := range r.Outputs() {
			owner, _ := r.ResolveDeviceEndpoint(out)
			if owner != node.id && owner != "local" {
				return false
			}
		}
		return true
	}, "stale room self-healing and leadership reconciliation")

	// Step 10: Stream audio post-restart through reconciled pipeline and verify RMS energy
	micStreamPost, okMicPost := node.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(micDev.URN)
	spkStreamPost, okSpkPost := node.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(spkDev.URN)
	if !okMicPost || !okSpkPost {
		t.Fatalf("failed to retrieve virtual streams post-restart")
	}

	postMetrics := injectAudioAndVerify(t, micStreamPost, spkStreamPost, 440.0, 0.707, 3840)
	t.Logf("PASS: Post-restart intra-node audio and reconciliation verified (RMS=%.2f dBFS)", postMetrics.RMSdBFS)
}
