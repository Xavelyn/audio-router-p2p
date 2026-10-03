package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/streamer"
)

// TestMultiNodePipeline_StrictUDPRouting verifies:
// 1. Node A and Node B synthesize byte-for-byte identical Route IDs and Stream IDs.
// 2. Sender node (Node A) has out == nil (zero local bypass write).
// 3. Receiver node (Node B) has in == nil (strictly receives via UDP).
// 4. Audio injected on Node A travels exclusively across the UDP network socket to Node B.
func TestMultiNodePipeline_StrictUDPRouting(t *testing.T) {
	nodeA := spawnClusterNode(t, "node-strict-A")
	defer nodeA.close()

	nodeB := spawnClusterNode(t, "node-strict-B", nodeA.disc.OwnAddr())
	defer nodeB.close()

	// Wait for 2-node cluster convergence
	t.Log("Waiting for cluster convergence between Node A and Node B...")
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

	// 1. Provision Virtual Input on Node A and Virtual Output on Node B
	micReqA := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-strict-mic",
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
		Name:             "vdev-strict-spk",
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

	// Force scrape cluster inventory
	nodeA.apiServer.ClusterManager().ScrapeCluster(context.Background())
	nodeB.apiServer.ClusterManager().ScrapeCluster(context.Background())

	// 2. Create Room on Node A
	roomReq := map[string]string{
		"id":   "room-strict-udp",
		"name": "Strict UDP Room",
	}
	resp, err = nodeA.post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// 3. Bind input (Node A) and output (Node B) with explicit node qualifiers
	bindInReq := v1.RoomDeviceBindingRequest{
		DeviceID: devMic.URN,
		NodeID:   "node-strict-A",
	}
	resp, err = nodeA.post("/api/v1/rooms/room-strict-udp/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/.../inputs failed: %v", err)
	}
	_ = resp.Body.Close()

	bindOutReq := v1.RoomDeviceBindingRequest{
		DeviceID: devSpk.URN,
		NodeID:   "node-strict-B",
	}
	resp, err = nodeA.post("/api/v1/rooms/room-strict-udp/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/.../outputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// 4. Wait for CRDT room convergence on Node B
	t.Log("Waiting for room-strict-udp to converge on Node B...")
	deadline = time.Now().Add(5 * time.Second)
	roomBReady := false
	for time.Now().Before(deadline) {
		rResp, rErr := nodeB.get("/api/v1/rooms/room-strict-udp")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			var rInfo map[string]interface{}
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			_ = rResp.Body.Close()
			inList, _ := rInfo["inputs"].([]interface{})
			outList, _ := rInfo["outputs"].([]interface{})
			if len(inList) > 0 && len(outList) > 0 {
				roomBReady = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !roomBReady {
		t.Fatalf("room-strict-udp did not converge on Node B within 5s")
	}

	// 5. Invariant Assertion: Verify Route ID and Stream ID symmetry
	nodeA.streams.mu.Lock()
	var routeA string
	var asA *activeStream
	for rID, as := range nodeA.streams.routes {
		routeA = rID
		asA = as
		break
	}
	nodeA.streams.mu.Unlock()

	nodeB.streams.mu.Lock()
	var routeB string
	var asB *activeStream
	for rID, as := range nodeB.streams.routes {
		routeB = rID
		asB = as
		break
	}
	nodeB.streams.mu.Unlock()

	if routeA == "" || routeB == "" {
		t.Fatalf("missing compiled routes: routeA=%q routeB=%q", routeA, routeB)
	}

	if routeA != routeB {
		t.Fatalf("FAIL: Asymmetric Route ID detected across nodes!\nNode A route: %s\nNode B route: %s", routeA, routeB)
	}

	streamIDA := streamer.DeriveStreamID(nodeA.cfg.AuthToken, routeA)
	streamIDB := streamer.DeriveStreamID(nodeB.cfg.AuthToken, routeB)
	if streamIDA != streamIDB {
		t.Fatalf("FAIL: Stream ID mismatch! streamIDA=%x != streamIDB=%x", streamIDA, streamIDB)
	}
	t.Logf("CONFIRMED: Route ID (%s) and Stream ID (%x) match 100%% symmetrically across nodes", routeA, streamIDA)

	// 6. Invariant Assertion: Sender has NO local output, Receiver has NO local input
	if asA.out != nil {
		t.Fatalf("FAIL: Node A (sender) opened local output! Cross-node audio would bypass UDP!")
	}
	if asA.in == nil {
		t.Fatalf("FAIL: Node A (sender) has nil input stream!")
	}
	if asB.out == nil {
		t.Fatalf("FAIL: Node B (receiver) has nil output stream!")
	}
	if asB.in != nil {
		t.Fatalf("FAIL: Node B (receiver) opened local input on ingress route!")
	}
	t.Log("CONFIRMED: Strict egress/ingress separation. Sender has zero local output bypass.")

	// 7. Inject audio on Node A and verify reception on Node B over UDP
	micStream, okMic := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devMic.URN)
	spkStream, okSpk := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devSpk.URN)
	if !okMic || !okSpk || micStream == nil || spkStream == nil {
		t.Fatalf("could not obtain VirtualAudioDevice streams")
	}

	frames := 480
	sineBlock := generateSineBlock(48000, 2, frames, 440.0, 0.707)

	doneFeed := make(chan struct{})
	go func() {
		defer close(doneFeed)
		for i := 0; i < 50; i++ {
			_, _ = micStream.InjectPush(sineBlock)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	t.Log("Verifying audio delivered exclusively via UDP to Node B speaker...")
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
		t.Fatalf("expected audio delivered to Node B speaker via UDP: got %d bytes, want >= %d",
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
		t.Fatalf("audio on Node B speaker is silent: maxAmp=%d (want > 1000)", maxAmp)
	}
	t.Logf("SUCCESS: Audio successfully routed across nodes via UDP (bytes=%d, peakAmp=%d / 32767)",
		len(captured), maxAmp)
}

// TestMultiNodePipeline_SampleRateMismatch_ALSA44kTo48k reproduces the exact user scenario:
// An input audio source operates at 44.1 kHz (e.g. Raspotify / librespot on ALSA loopback),
// and streams across nodes over UDP to an output sink operating at 48 kHz.
func TestMultiNodePipeline_SampleRateMismatch_ALSA44kTo48k(t *testing.T) {
	nodeA := spawnClusterNode(t, "node-rate-A")
	defer nodeA.close()

	nodeB := spawnClusterNode(t, "node-rate-B", nodeA.disc.OwnAddr())
	defer nodeB.close()

	// Wait for convergence
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

	// Node A: 44.1 kHz Input (simulating ALSA snd-aloop Raspotify device)
	micReqA := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-loopback-44k",
		Direction:        "input",
		Channels:         2,
		SampleRate:       44100,
		BufferSizeFrames: 441,
	}
	resp, err := nodeA.post("/api/v1/devices/virtual", micReqA)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/devices/virtual Node A failed: %v", err)
	}
	var devMic v1.AudioDevice
	_ = json.NewDecoder(resp.Body).Decode(&devMic)
	_ = resp.Body.Close()

	// Node B: 48 kHz Output (standard DAC / speaker)
	spkReqB := v1.CreateVirtualDeviceRequest{
		Name:             "vdev-speaker-48k",
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

	nodeA.apiServer.ClusterManager().ScrapeCluster(context.Background())
	nodeB.apiServer.ClusterManager().ScrapeCluster(context.Background())

	// Create room and bind
	roomReq := map[string]string{
		"id":   "room-rate-mismatch",
		"name": "Rate Mismatch Room (44.1k -> 48k)",
	}
	resp, err = nodeA.post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	bindInReq := v1.RoomDeviceBindingRequest{
		DeviceID: devMic.URN,
		NodeID:   "node-rate-A",
	}
	resp, err = nodeA.post("/api/v1/rooms/room-rate-mismatch/inputs", bindInReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/.../inputs failed: %v", err)
	}
	_ = resp.Body.Close()

	bindOutReq := v1.RoomDeviceBindingRequest{
		DeviceID: devSpk.URN,
		NodeID:   "node-rate-B",
	}
	resp, err = nodeA.post("/api/v1/rooms/room-rate-mismatch/outputs", bindOutReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/rooms/.../outputs failed: %v", err)
	}
	_ = resp.Body.Close()

	// Wait for convergence on Node B
	deadline = time.Now().Add(5 * time.Second)
	roomBReady := false
	for time.Now().Before(deadline) {
		rResp, rErr := nodeB.get("/api/v1/rooms/room-rate-mismatch")
		if rErr == nil && rResp.StatusCode == http.StatusOK {
			var rInfo map[string]interface{}
			_ = json.NewDecoder(rResp.Body).Decode(&rInfo)
			_ = rResp.Body.Close()
			inList, _ := rInfo["inputs"].([]interface{})
			outList, _ := rInfo["outputs"].([]interface{})
			if len(inList) > 0 && len(outList) > 0 {
				roomBReady = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !roomBReady {
		t.Fatalf("room-rate-mismatch did not converge on Node B within 5s")
	}

	micStream, okMic := nodeA.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devMic.URN)
	spkStream, okSpk := nodeB.apiServer.V1().DeviceRegistry().GetVirtualDeviceStream(devSpk.URN)
	if !okMic || !okSpk || micStream == nil || spkStream == nil {
		t.Fatalf("could not obtain stream handles")
	}

	// Feed 44.1 kHz sine wave from Node A
	frames44k := 441
	sineBlock44k := generateSineBlock(44100, 2, frames44k, 440.0, 0.707)

	doneFeed := make(chan struct{})
	go func() {
		defer close(doneFeed)
		for i := 0; i < 50; i++ {
			_, _ = micStream.InjectPush(sineBlock44k)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	t.Log("Verifying 44.1 kHz -> 48 kHz cross-node resampled audio delivery on Node B...")
	deadline = time.Now().Add(5 * time.Second)
	var captured []byte
	for time.Now().Before(deadline) {
		captured = spkStream.CapturedBytes()
		if len(captured) >= len(sineBlock44k)*2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	<-doneFeed

	if len(captured) < len(sineBlock44k)*2 {
		t.Fatalf("expected resampled audio delivered to Node B: got %d bytes, want >= %d",
			len(captured), len(sineBlock44k)*2)
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
		t.Fatalf("resampled audio on Node B is silent: maxAmp=%d", maxAmp)
	}
	t.Logf("SUCCESS: 44.1 kHz -> 48 kHz UDP audio resampled and delivered cleanly (bytes=%d, peakAmp=%d / 32767)",
		len(captured), maxAmp)
}

// TestMultiNodePipeline_LeaderConsensus verifies that both nodes in a multi-node room
// deterministically elect the identical leader (lowest-ID rule) and never fall back to split-brain.
func TestMultiNodePipeline_LeaderConsensus(t *testing.T) {
	nodeA := spawnClusterNode(t, "node-lead-01")
	defer nodeA.close()

	nodeB := spawnClusterNode(t, "node-lead-02", nodeA.disc.OwnAddr())
	defer nodeB.close()

	// Wait for convergence
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapA := nodeA.apiServer.ClusterManager().GetSnapshot()
		snapB := nodeB.apiServer.ClusterManager().GetSnapshot()
		if snapA != nil && snapB != nil && snapA.ActiveNodes >= 2 && snapB.ActiveNodes >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Create room on Node A
	roomReq := map[string]string{
		"id":   "room-consensus-test",
		"name": "Consensus Room",
	}
	resp, err := nodeA.post("/api/v1/rooms", roomReq)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/rooms failed: %v", err)
	}
	_ = resp.Body.Close()

	// Bind input on Node A and output on Node B
	bindIn := v1.RoomDeviceBindingRequest{DeviceID: "urn:audio:hw:mic:dummy", NodeID: "node-lead-01"}
	bindOut := v1.RoomDeviceBindingRequest{DeviceID: "urn:audio:hw:spk:dummy", NodeID: "node-lead-02"}

	resp, err = nodeA.post("/api/v1/rooms/room-consensus-test/inputs", bindIn)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("bind input: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = nodeA.post("/api/v1/rooms/room-consensus-test/outputs", bindOut)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("bind output: %v", err)
	}
	_ = resp.Body.Close()

	// Allow convergence
	time.Sleep(1 * time.Second)

	roomA, okA := nodeA.roomMgr.GetRoom("room-consensus-test")
	roomB, okB := nodeB.roomMgr.GetRoom("room-consensus-test")
	if !okA || !okB {
		t.Fatalf("room missing on nodes: okA=%t okB=%t", okA, okB)
	}

	leaderA := roomA.Leader()
	leaderB := roomB.Leader()

	if leaderA == "" || leaderB == "" {
		t.Fatalf("leader is empty: leaderA=%q leaderB=%q", leaderA, leaderB)
	}

	if leaderA != leaderB {
		t.Fatalf("SPLIT BRAIN: leaderA=%q != leaderB=%q", leaderA, leaderB)
	}

	expectedLeader := "node-lead-01" // lowest ID between "node-lead-01" and "node-lead-02"
	if leaderA != expectedLeader {
		t.Fatalf("leader=%q, want deterministic lowestID=%q", leaderA, expectedLeader)
	}

	t.Logf("SUCCESS: Both nodes agree on leader %s with zero split-brain", leaderA)
}
