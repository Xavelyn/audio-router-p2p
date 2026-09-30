package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/telemetry"
)

// LocalNodeProvider provides telemetry and device control for the local daemon.
type LocalNodeProvider interface {
	NodeID() string
	NodeName() string
	APIAddr() string
	UptimeSeconds() float64
	ListDevices(kind string) ([]AudioDeviceSummary, error)
	UpdateDeviceState(id string, vol *float64, muted *bool) (AudioDeviceSummary, bool)
	HealthSnapshot() *telemetry.NetworkHealthSnapshot
	ActiveStreams() int
}

// DiscoveryProvider provides mesh topology and gossip state.
type DiscoveryProvider interface {
	NodeID() string
	Peers() []discovery.Peer
	PeerByID(id string) *discovery.Peer
	PeerState(id string) string
}

// Config specifies operating parameters for the cluster manager.
type Config struct {
	ScrapeInterval          time.Duration // Default: 2.0s
	ScrapeTimeout           time.Duration // Default: 1.5s
	MaxWorkers              int           // Default: 8
	AuthToken               string
	CircuitBreakerThreshold int           // Default: 3
	CircuitBreakerCooldown  time.Duration // Default: 10s
	ClusterID               string
}

// Manager aggregates cluster-wide telemetry and proxies remote device mutations.
type Manager struct {
	cfg        Config
	local      LocalNodeProvider
	disc       DiscoveryProvider
	httpClient *http.Client

	// Lock-free read plane for O(1) instant UI/API queries
	snapshot atomic.Pointer[ClusterSnapshot]

	// Circuit breaker state per peer ID
	peerMu       sync.RWMutex
	peerFailures map[string]int
	circuitOpen  map[string]time.Time

	// Lifecycle management
	started  atomic.Bool
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc
	ctx      context.Context
	wg       sync.WaitGroup
}

// NewManager creates a thread-safe ClusterManager.
func NewManager(cfg Config, local LocalNodeProvider, disc DiscoveryProvider, customClient ...*http.Client) *Manager {
	if cfg.ScrapeInterval <= 0 {
		cfg.ScrapeInterval = 2 * time.Second
	}
	if cfg.ScrapeTimeout <= 0 {
		cfg.ScrapeTimeout = 1500 * time.Millisecond
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 8
	}
	if cfg.CircuitBreakerThreshold <= 0 {
		cfg.CircuitBreakerThreshold = 3
	}
	if cfg.CircuitBreakerCooldown <= 0 {
		cfg.CircuitBreakerCooldown = 10 * time.Second
	}

	var client *http.Client
	if len(customClient) > 0 && customClient[0] != nil {
		client = customClient[0]
	} else {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // Allow self-signed certificates inside mesh
			},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		}
		client = &http.Client{
			Transport: tr,
			Timeout:   cfg.ScrapeTimeout + 500*time.Millisecond,
		}
	}

	m := &Manager{
		cfg:          cfg,
		local:        local,
		disc:         disc,
		httpClient:   client,
		peerFailures: make(map[string]int),
		circuitOpen:  make(map[string]time.Time),
		stopCh:       make(chan struct{}),
	}

	// Initialize with an empty snapshot containing local node info
	m.snapshot.Store(m.compileInitialSnapshot())
	return m
}

// Start begins background periodic scraping of cluster peers.
func (m *Manager) Start(ctx context.Context) {
	if m.started.Swap(true) {
		return
	}

	m.ctx, m.cancel = context.WithCancel(ctx)

	// Perform initial scrape immediately
	m.ScrapeCluster(m.ctx)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.cfg.ScrapeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-m.stopCh:
				return
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.ScrapeCluster(m.ctx)
			}
		}
	}()
}

// Stop halts background scraping routines safely.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		if m.cancel != nil {
			m.cancel()
		}
		m.wg.Wait()
	})
}

// Close implements io.Closer and cleans up resources.
func (m *Manager) Close() error {
	m.Stop()
	if tr, ok := m.httpClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// GetSnapshot returns the current atomic ClusterSnapshot in lock-free O(1) time.
func (m *Manager) GetSnapshot() *ClusterSnapshot {
	snap := m.snapshot.Load()
	if snap == nil {
		return m.compileInitialSnapshot()
	}
	return snap
}

func (m *Manager) localNodeID() string {
	if m.local != nil {
		return m.local.NodeID()
	}
	if m.disc != nil {
		return m.disc.NodeID()
	}
	return "local"
}

func (m *Manager) localNodeName() string {
	if m.local != nil && m.local.NodeName() != "" {
		return m.local.NodeName()
	}
	return m.localNodeID()
}

func (m *Manager) compileInitialSnapshot() *ClusterSnapshot {
	localID := m.localNodeID()
	localName := m.localNodeName()

	var localDevices []AudioDeviceSummary
	if m.local != nil {
		devs, err := m.local.ListDevices("all")
		if err == nil {
			localDevices = devs
		}
	}

	var localHealthScore float64 = 100.0
	var localHealthGrade string = string(telemetry.GradeExcellent)
	var localSync telemetry.ClockSyncHealth
	var localActiveStreams int
	var localUptime float64
	var localLatencies map[string]float64

	if m.local != nil {
		if snap := m.local.HealthSnapshot(); snap != nil {
			localHealthScore = snap.HealthScore
			localHealthGrade = string(snap.HealthGrade)
			localSync = snap.Sync
			localLatencies = snap.LatencyMatrix
		}
		localActiveStreams = m.local.ActiveStreams()
		localUptime = m.local.UptimeSeconds()
	}

	localSummary := ClusterNodeSummary{
		ID:            localID,
		NodeID:        localID,
		Name:          localName,
		APIAddr:       m.localAPIAddr(),
		IsLocal:       true,
		SWIMState:     "alive",
		HealthScore:   localHealthScore,
		HealthGrade:   localHealthGrade,
		ClockSync:     localSync,
		Devices:       localDevices,
		ActiveStreams: localActiveStreams,
		UptimeSeconds: localUptime,
		LastSeen:      time.Now(),
	}

	nodes := map[string]ClusterNodeSummary{
		localID: localSummary,
	}

	latMatrix := make(map[string]map[string]float64)
	if localLatencies != nil {
		latMatrix[localID] = localLatencies
	}

	return &ClusterSnapshot{
		GeneratedAt:    time.Now(),
		LocalNodeID:    localID,
		ClusterHealth:  localHealthScore,
		TotalNodes:     1,
		ActiveNodes:    1,
		Nodes:          nodes,
		LatencyMatrix:  latMatrix,
		ClusterDevices: localDevices,
	}
}

func (m *Manager) localAPIAddr() string {
	if m.local != nil {
		return m.local.APIAddr()
	}
	return ""
}

// ScrapeCluster initiates parallel scraping across all discovered peers and publishes
// a newly compiled atomic ClusterSnapshot.
func (m *Manager) ScrapeCluster(ctx context.Context) *ClusterSnapshot {
	if ctx.Err() != nil {
		return m.GetSnapshot()
	}

	localID := m.localNodeID()
	localName := m.localNodeName()

	// 1. Gather local telemetry & devices
	var localDevices []AudioDeviceSummary
	if m.local != nil {
		devs, err := m.local.ListDevices("all")
		if err == nil {
			localDevices = devs
		}
	}

	var localHealthScore float64 = 100.0
	var localHealthGrade string = string(telemetry.GradeExcellent)
	var localSync telemetry.ClockSyncHealth
	var localActiveStreams int
	var localUptime float64
	var localLatencies map[string]float64

	if m.local != nil {
		if snap := m.local.HealthSnapshot(); snap != nil {
			localHealthScore = snap.HealthScore
			localHealthGrade = string(snap.HealthGrade)
			localSync = snap.Sync
			localLatencies = snap.LatencyMatrix
		}
		localActiveStreams = m.local.ActiveStreams()
		localUptime = m.local.UptimeSeconds()
	}

	localSummary := ClusterNodeSummary{
		ID:            localID,
		NodeID:        localID,
		Name:          localName,
		APIAddr:       m.localAPIAddr(),
		IsLocal:       true,
		SWIMState:     "alive",
		HealthScore:   localHealthScore,
		HealthGrade:   localHealthGrade,
		ClockSync:     localSync,
		Devices:       localDevices,
		ActiveStreams: localActiveStreams,
		UptimeSeconds: localUptime,
		LastSeen:      time.Now(),
	}

	// 2. Discover peers
	var peers []discovery.Peer
	if m.disc != nil {
		peers = m.disc.Peers()
	}

	// Prune circuit breaker state for departed peers
	m.pruneCircuitBreakers(peers)

	// 3. Parallel bounded worker pool
	sem := make(chan struct{}, m.cfg.MaxWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	peerSummaries := make([]ClusterNodeSummary, 0, len(peers))
	peerLatencyMaps := make(map[string]map[string]float64)

peerLoop:
	for _, p := range peers {
		if p.ID == localID {
			continue
		}

		select {
		case <-ctx.Done():
			break peerLoop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(peer discovery.Peer) {
			defer func() {
				<-sem
				wg.Done()
			}()

			scrapeCtx, cancel := context.WithTimeout(ctx, m.cfg.ScrapeTimeout)
			defer cancel()

			summary, latencies := m.scrapePeer(scrapeCtx, peer)

			mu.Lock()
			peerSummaries = append(peerSummaries, summary)
			if len(latencies) > 0 {
				peerLatencyMaps[peer.ID] = latencies
			}
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	// 4. Compile ClusterSnapshot
	nodes := make(map[string]ClusterNodeSummary, len(peerSummaries)+1)
	nodes[localID] = localSummary

	allDevices := make([]AudioDeviceSummary, 0, len(localDevices))
	allDevices = append(allDevices, localDevices...)

	activeNodes := 0
	if localSummary.HealthScore > 0 && localSummary.ErrorReason == "" {
		activeNodes = 1
	}

	for _, ps := range peerSummaries {
		nodes[ps.ID] = ps
		if ps.HealthScore > 0 && ps.SWIMState == "alive" && ps.ErrorReason == "" {
			activeNodes++
		}
		allDevices = append(allDevices, ps.Devices...)
	}

	// Build latency matrix
	latMatrix := make(map[string]map[string]float64)
	if localLatencies != nil {
		latMatrix[localID] = localLatencies
	}
	for peerID, lats := range peerLatencyMaps {
		latMatrix[peerID] = lats
	}

	// Compute harmonic mean cluster health score
	var sumInv float64
	var scoredCount int
	for _, node := range nodes {
		if node.HealthScore > 0 {
			score := node.HealthScore
			if score < 1.0 {
				score = 1.0
			}
			sumInv += 1.0 / score
			scoredCount++
		}
	}
	var clusterHealth float64
	if scoredCount > 0 && sumInv > 0 {
		clusterHealth = math.Round((float64(scoredCount)/sumInv)*10) / 10
	}

	snap := &ClusterSnapshot{
		GeneratedAt:    time.Now(),
		LocalNodeID:    localID,
		ClusterHealth:  clusterHealth,
		TotalNodes:     len(nodes),
		ActiveNodes:    activeNodes,
		Nodes:          nodes,
		LatencyMatrix:  latMatrix,
		ClusterDevices: allDevices,
	}

	m.snapshot.Store(snap)
	return snap
}

func (m *Manager) scrapePeer(ctx context.Context, peer discovery.Peer) (ClusterNodeSummary, map[string]float64) {
	swimState := "alive"
	if m.disc != nil {
		swimState = m.disc.PeerState(peer.ID)
	}

	// If SWIM state is dead, bypass HTTP calls entirely
	if swimState == "dead" {
		return ClusterNodeSummary{
			ID:          peer.ID,
			NodeID:      peer.ID,
			Name:        peer.ID,
			APIAddr:     peer.APIAddr,
			IsLocal:     false,
			SWIMState:   swimState,
			HealthScore: 0.0,
			HealthGrade: string(telemetry.GradeCritical),
			LastSeen:    peer.LastSeen,
			ErrorReason: "SWIM failure detector reported dead",
		}, nil
	}

	// Check circuit breaker
	if m.isCircuitOpen(peer.ID) {
		return ClusterNodeSummary{
			ID:          peer.ID,
			NodeID:      peer.ID,
			Name:        peer.ID,
			APIAddr:     peer.APIAddr,
			IsLocal:     false,
			SWIMState:   swimState,
			HealthScore: 0.0,
			HealthGrade: string(telemetry.GradeCritical),
			LastSeen:    peer.LastSeen,
			ErrorReason: "circuit breaker open (consecutive failures)",
		}, nil
	}

	if peer.APIAddr == "" {
		return ClusterNodeSummary{
			ID:          peer.ID,
			NodeID:      peer.ID,
			Name:        peer.ID,
			APIAddr:     "",
			IsLocal:     false,
			SWIMState:   swimState,
			HealthScore: 0.0,
			HealthGrade: string(telemetry.GradeCritical),
			LastSeen:    peer.LastSeen,
			ErrorReason: "peer advertised no APIAddr",
		}, nil
	}

	// 1. Scrape GET /api/v1/network/health
	healthURL := strings.TrimRight(peer.APIAddr, "/") + "/api/v1/network/health"
	hReq, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, err), nil
	}
	if m.cfg.AuthToken != "" {
		hReq.Header.Set("Authorization", "Bearer "+m.cfg.AuthToken)
	}

	hResp, err := m.httpClient.Do(hReq)
	if err != nil {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, err), nil
	}
	defer hResp.Body.Close()

	if hResp.StatusCode != http.StatusOK {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, fmt.Errorf("health endpoint returned %d", hResp.StatusCode)), nil
	}

	var netSnap telemetry.NetworkHealthSnapshot
	if err := json.NewDecoder(hResp.Body).Decode(&netSnap); err != nil {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, fmt.Errorf("decode health: %w", err)), nil
	}

	// 2. Scrape GET /api/v1/devices
	devicesURL := strings.TrimRight(peer.APIAddr, "/") + "/api/v1/devices"
	dReq, err := http.NewRequestWithContext(ctx, http.MethodGet, devicesURL, nil)
	if err != nil {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, err), nil
	}
	if m.cfg.AuthToken != "" {
		dReq.Header.Set("Authorization", "Bearer "+m.cfg.AuthToken)
	}

	dResp, err := m.httpClient.Do(dReq)
	if err != nil {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, err), nil
	}
	defer dResp.Body.Close()

	if dResp.StatusCode != http.StatusOK {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, fmt.Errorf("devices endpoint returned %d", dResp.StatusCode)), nil
	}

	var rawDevices []struct {
		ID                string   `json:"id"`
		DeviceID          string   `json:"device_id"`
		URN               string   `json:"urn"`
		Name              string   `json:"name"`
		Direction         string   `json:"direction"`
		MaxInput          int      `json:"max_input"`
		MaxOutput         int      `json:"max_output"`
		DefaultSampleRate float64  `json:"default_sample_rate"`
		IsVirtual         bool     `json:"is_virtual"`
		Volume            float64  `json:"volume"`
		Muted             bool     `json:"muted"`
		SupportedLayouts  []string `json:"supported_layouts"`
		State             string   `json:"state"`
	}
	if err := json.NewDecoder(dResp.Body).Decode(&rawDevices); err != nil {
		m.recordFailure(peer.ID)
		return m.failedSummary(peer, swimState, fmt.Errorf("decode devices: %w", err)), nil
	}

	// Success: reset failures and clear circuit
	m.recordSuccess(peer.ID)

	peerDevices := make([]AudioDeviceSummary, len(rawDevices))
	for i, d := range rawDevices {
		devID := d.URN
		if devID == "" {
			devID = d.ID
		}
		if devID == "" {
			devID = d.DeviceID
		}
		direction := d.Direction
		if direction == "" {
			if d.MaxInput > 0 && d.MaxOutput > 0 {
				direction = "duplex"
			} else if d.MaxInput > 0 {
				direction = "input"
			} else if d.MaxOutput > 0 {
				direction = "output"
			} else {
				direction = "duplex"
			}
		}
		state := d.State
		if state == "" {
			state = "active"
		}

		peerDevices[i] = AudioDeviceSummary{
			ID:                devID,
			DeviceID:          devID,
			URN:               devID,
			NodeID:            peer.ID,
			NodeName:          peer.ID,
			Name:              d.Name,
			Direction:         direction,
			MaxInput:          d.MaxInput,
			MaxOutput:         d.MaxOutput,
			DefaultSampleRate: d.DefaultSampleRate,
			IsVirtual:         d.IsVirtual,
			Volume:            d.Volume,
			Muted:             d.Muted,
			SupportedLayouts:  d.SupportedLayouts,
			State:             state,
		}
	}

	summary := ClusterNodeSummary{
		ID:            peer.ID,
		NodeID:        peer.ID,
		Name:          peer.ID,
		APIAddr:       peer.APIAddr,
		IsLocal:       false,
		SWIMState:     swimState,
		HealthScore:   netSnap.HealthScore,
		HealthGrade:   string(netSnap.HealthGrade),
		ClockSync:     netSnap.Sync,
		Devices:       peerDevices,
		ActiveStreams: len(netSnap.Streams),
		LastSeen:      peer.LastSeen,
	}

	return summary, netSnap.LatencyMatrix
}

func (m *Manager) failedSummary(peer discovery.Peer, swimState string, err error) ClusterNodeSummary {
	return ClusterNodeSummary{
		ID:          peer.ID,
		NodeID:      peer.ID,
		Name:        peer.ID,
		APIAddr:     peer.APIAddr,
		IsLocal:     false,
		SWIMState:   swimState,
		HealthScore: 0.0,
		HealthGrade: string(telemetry.GradeCritical),
		LastSeen:    peer.LastSeen,
		ErrorReason: err.Error(),
	}
}

// isCircuitOpen checks if the circuit breaker is active for peerID.
func (m *Manager) isCircuitOpen(peerID string) bool {
	m.peerMu.RLock()
	defer m.peerMu.RUnlock()

	failures := m.peerFailures[peerID]
	if failures < m.cfg.CircuitBreakerThreshold {
		return false
	}
	openUntil, ok := m.circuitOpen[peerID]
	if !ok {
		return false
	}
	return time.Now().Before(openUntil)
}

func (m *Manager) recordFailure(peerID string) {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()

	m.peerFailures[peerID]++
	if m.peerFailures[peerID] >= m.cfg.CircuitBreakerThreshold {
		m.circuitOpen[peerID] = time.Now().Add(m.cfg.CircuitBreakerCooldown)
	}
}

func (m *Manager) recordSuccess(peerID string) {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()

	m.peerFailures[peerID] = 0
	delete(m.circuitOpen, peerID)
}

func (m *Manager) pruneCircuitBreakers(activePeers []discovery.Peer) {
	activeSet := make(map[string]struct{}, len(activePeers))
	for _, p := range activePeers {
		activeSet[p.ID] = struct{}{}
	}

	now := time.Now()
	m.peerMu.Lock()
	defer m.peerMu.Unlock()

	for id := range m.peerFailures {
		if _, active := activeSet[id]; !active {
			delete(m.peerFailures, id)
		}
	}
	for id, openUntil := range m.circuitOpen {
		if _, active := activeSet[id]; !active || now.After(openUntil.Add(m.cfg.CircuitBreakerCooldown)) {
			delete(m.circuitOpen, id)
		}
	}
}

// MutateRemoteDevice proxies device state mutations (volume, mute) to a remote node or
// dispatches directly to the local device registry if nodeID targets the local node.
func (m *Manager) MutateRemoteDevice(ctx context.Context, nodeID string, deviceID string, req DeviceMutationRequest) (*DeviceMutationResponse, error) {
	if req.Volume != nil {
		if math.IsNaN(*req.Volume) || math.IsInf(*req.Volume, 0) || *req.Volume < 0.0 || *req.Volume > 2.0 {
			return nil, fmt.Errorf("%w: volume must be between 0.0 and 2.0", ErrInvalidParameter)
		}
	}

	localID := m.localNodeID()
	if nodeID == "" || nodeID == localID {
		if m.local == nil {
			return nil, ErrRemoteNodeUnavailable
		}
		dev, found := m.local.UpdateDeviceState(deviceID, req.Volume, req.Muted)
		if !found {
			return nil, ErrDeviceNotFound
		}
		return &DeviceMutationResponse{
			ID:        deviceID,
			DeviceID:  deviceID,
			NodeID:    localID,
			Volume:    dev.Volume,
			Muted:     dev.Muted,
			UpdatedAt: time.Now(),
		}, nil
	}

	// Target is remote node
	if m.disc == nil {
		return nil, ErrNodeNotFound
	}
	peer := m.disc.PeerByID(nodeID)
	if peer == nil {
		return nil, ErrNodeNotFound
	}

	if peer.APIAddr == "" {
		return nil, fmt.Errorf("%w: remote node has no APIAddr", ErrRemoteNodeUnavailable)
	}

	if m.disc.PeerState(nodeID) == "dead" || m.isCircuitOpen(nodeID) {
		return nil, fmt.Errorf("%w: node %s is dead or circuit breaker is open", ErrRemoteNodeUnavailable, nodeID)
	}

	localDevID := deviceID
	prefix := fmt.Sprintf("urn:audio:node:%s:dev:", nodeID)
	if strings.HasPrefix(localDevID, prefix) {
		localDevID = strings.TrimPrefix(localDevID, prefix)
	}

	targetURL := strings.TrimRight(peer.APIAddr, "/") + "/api/v1/devices/" + url.PathEscape(localDevID)
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidParameter, err)
	}

	// Enforce bounded timeout for remote mutation call
	callTimeout := m.cfg.ScrapeTimeout * 2
	if callTimeout <= 0 {
		callTimeout = 3 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPatch, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create proxy request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if m.cfg.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+m.cfg.AuthToken)
	}

	resp, err := m.httpClient.Do(httpReq)
	if err != nil {
		m.recordFailure(nodeID)
		return nil, fmt.Errorf("%w: %v", ErrRemoteNodeUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrDeviceNotFound
	}
	if resp.StatusCode >= 500 {
		m.recordFailure(nodeID)
		return nil, fmt.Errorf("%w: remote peer returned status %d", ErrRemoteNodeUnavailable, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("remote device mutation returned status %d", resp.StatusCode)
	}

	m.recordSuccess(nodeID)

	var devResult struct {
		ID       string  `json:"id"`
		DeviceID string  `json:"device_id"`
		Volume   float64 `json:"volume"`
		Muted    bool    `json:"muted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&devResult); err != nil {
		vol := 1.0
		if req.Volume != nil {
			vol = *req.Volume
		}
		muted := false
		if req.Muted != nil {
			muted = *req.Muted
		}
		return &DeviceMutationResponse{
			ID:        deviceID,
			DeviceID:  deviceID,
			NodeID:    nodeID,
			Volume:    vol,
			Muted:     muted,
			UpdatedAt: time.Now(),
		}, nil
	}

	return &DeviceMutationResponse{
		ID:        deviceID,
		DeviceID:  deviceID,
		NodeID:    nodeID,
		Volume:    devResult.Volume,
		Muted:     devResult.Muted,
		UpdatedAt: time.Now(),
	}, nil
}
