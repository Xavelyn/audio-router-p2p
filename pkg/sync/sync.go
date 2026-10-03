package sync

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/telemetry"
)

const (
	defaultSyncInterval = 1 * time.Second
	defaultPeerTTL      = 5 * time.Second
	recvBufferSize      = 2048
)

// PeerOffset is the estimated clock offset and link telemetry for a peer.
type PeerOffset struct {
	Offset     time.Duration `json:"offset"`
	RTT        time.Duration `json:"rtt"`
	DriftPPM   float64       `json:"drift_ppm"`
	LastUpdate time.Time     `json:"lastUpdate"`
}

// PeerSource is the minimal view of the discovery service that the sync manager
// needs. *discovery.Discovery satisfies it.
type PeerSource interface {
	NodeID() string
	Peers() []discovery.Peer
}

// Manager performs NTP-style clock synchronisation against discovered peers.
type Manager struct {
	cfg          *config.Config
	disc         PeerSource
	syncInterval time.Duration
	peerTTL      time.Duration

	mu      sync.RWMutex
	conn    *net.UDPConn
	started bool
	stopCh  chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	offsets map[string]PeerOffset
	pending map[uint64]time.Time // nonce -> T1 send time

	packetsSent atomic.Uint64
	packetsRecv atomic.Uint64
	packetsLost atomic.Uint64
	isMaster    atomic.Bool
}

// NewManager creates a clock-sync manager for the given discovery service and configuration.
// If intervals are zero or invalid, sensible defaults are automatically established.
func NewManager(cfg *config.Config, disc PeerSource) *Manager {
	interval := defaultSyncInterval
	ttl := defaultPeerTTL
	if cfg != nil {
		if cfg.SyncInterval > 0 {
			interval = cfg.SyncInterval
		}
		if cfg.PeerTTL > 0 {
			ttl = cfg.PeerTTL
		}
	}

	return &Manager{
		cfg:          cfg,
		disc:         disc,
		syncInterval: interval,
		peerTTL:      ttl,
		offsets:      make(map[string]PeerOffset),
		pending:      make(map[uint64]time.Time),
		stopCh:       make(chan struct{}),
	}
}

// Run starts the clock synchronization engine and blocks until Close is called.
func (m *Manager) Run() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true

	// Bind UDP listener if a port was configured
	if m.cfg != nil && m.cfg.SyncPort > 0 {
		if c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: m.cfg.SyncPort}); err == nil {
			m.conn = c
			m.wg.Add(1)
			go m.recvLoop(c)
		} else {
			log.Printf("sync: cannot bind UDP port %d: %v", m.cfg.SyncPort, err)
		}
	}
	m.mu.Unlock()

	// Track the main worker loop on m.wg so Close() cleanly awaits total shutdown
	m.wg.Add(1)
	defer m.wg.Done()

	ticker := time.NewTicker(m.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.pingPeers()
			m.sweepPending()
		case <-m.stopCh:
			return
		}
	}
}

// recvLoop continuously reads and decodes incoming synchronization datagrams.
func (m *Manager) recvLoop(conn *net.UDPConn) {
	defer m.wg.Done()
	buf := make([]byte, recvBufferSize)

	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			m.mu.RLock()
			stopping := m.conn == nil
			m.mu.RUnlock()
			if stopping {
				return
			}
			log.Printf("sync: receive error: %v", err)
			continue
		}

		msg, err := decodeMessage(buf[:n])
		if err != nil {
			continue
		}

		m.handle(addr, msg)
	}
}

// peerIDForSyncAddr resolves a sync datagram source address to the ID of a
// currently-discovered peer. The local node is excluded.
func (m *Manager) peerIDForSyncAddr(addr *net.UDPAddr) (string, bool) {
	if addr == nil || m.disc == nil {
		return "", false
	}
	return resolvePeerID(m.disc.Peers(), m.disc.NodeID(), addr)
}

// handle processes an incoming request or response message.
func (m *Manager) handle(addr *net.UDPAddr, msg *message) {
	if msg == nil || addr == nil {
		return
	}

	switch msg.Type {
	case msgRequest:
		m.handleRequest(addr, msg)
	case msgResponse:
		m.handleResponse(addr, msg)
	}
}

// handleRequest responds to incoming sync pings from authorized cluster peers.
func (m *Manager) handleRequest(addr *net.UDPAddr, msg *message) {
	// Restrict sync to peers that discovery currently knows about.
	if _, ok := m.peerIDForSyncAddr(addr); !ok {
		return
	}

	now := time.Now().UnixNano()
	resp := message{
		Type:  msgResponse,
		ID:    m.disc.NodeID(),
		Nonce: msg.Nonce,
		T1:    msg.T1,
		T2:    now, // server receive time of the request
		T3:    now, // server send time of the response
	}

	payload, err := encodeMessage(&resp)
	if err != nil {
		return
	}

	m.mu.RLock()
	conn := m.conn
	m.mu.RUnlock()

	if conn != nil {
		_, _ = conn.WriteToUDP(payload, addr)
	}
}

// handleResponse handles arriving sync pong responses, measuring skew, RTT, and clock drift.
func (m *Manager) handleResponse(addr *net.UDPAddr, msg *message) {
	t4 := time.Now().UnixNano()
	nowTime := time.Now()

	// Verify sender identity against discovery
	peerID, ok := m.peerIDForSyncAddr(addr)
	if !ok {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	t1, ok := m.pending[msg.Nonce]
	if !ok {
		return
	}
	delete(m.pending, msg.Nonce)
	m.packetsRecv.Add(1)

	offset, rtt := ComputeOffsetAndRTT(t1.UnixNano(), msg.T2, msg.T3, t4)

	prev, hasPrev := m.offsets[peerID]
	dt := nowTime.Sub(prev.LastUpdate)
	driftPPM := ComputeDriftPPM(prev.Offset, offset, prev.DriftPPM, dt, hasPrev)

	m.offsets[peerID] = PeerOffset{
		Offset:     offset,
		RTT:        rtt,
		DriftPPM:   driftPPM,
		LastUpdate: nowTime,
	}
}

// pingPeers transmits a time-sync request to every discovered cluster peer.
func (m *Manager) pingPeers() {
	if m.disc == nil {
		return
	}

	peers := m.disc.Peers()
	localID := m.disc.NodeID()

	for _, p := range peers {
		if p.ID == localID || p.SyncAddr == "" {
			continue
		}

		nonce, err := randomNonce()
		if err != nil {
			continue
		}

		req := message{
			Type:  msgRequest,
			ID:    localID,
			Nonce: nonce,
			T1:    time.Now().UnixNano(),
		}

		payload, err := encodeMessage(&req)
		if err != nil {
			continue
		}

		// Resolve destination socket outside of mutex lock
		addr, err := net.ResolveUDPAddr("udp4", p.SyncAddr)
		if err != nil {
			continue
		}

		m.mu.Lock()
		if m.conn != nil {
			if _, err := m.conn.WriteToUDP(payload, addr); err == nil {
				m.pending[nonce] = time.Unix(0, req.T1)
				m.packetsSent.Add(1)
			}
		}
		m.mu.Unlock()
	}
}

// sweepPending drops sync requests older than peerTTL that never received a response.
func (m *Manager) sweepPending() {
	cutoff := time.Now().Add(-m.peerTTL)

	m.mu.Lock()
	defer m.mu.Unlock()

	for nonce, sent := range m.pending {
		if sent.Before(cutoff) {
			delete(m.pending, nonce)
			m.packetsLost.Add(1)
		}
	}
}

// Offsets returns a thread-safe snapshot of all known peer clock offsets.
func (m *Manager) Offsets() map[string]PeerOffset {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]PeerOffset, len(m.offsets))
	for k, v := range m.offsets {
		out[k] = v
	}
	return out
}

// IsMaster reports whether this node is designated as the clock synchronization master.
func (m *Manager) IsMaster() bool {
	return m.isMaster.Load()
}

// SetMaster designates this node as the clock synchronization master.
func (m *Manager) SetMaster(v bool) {
	m.isMaster.Store(v)
}

// Telemetry returns aggregated clock sync health metrics safely.
func (m *Manager) Telemetry() telemetry.ClockSyncHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var totalOffsetUS, totalDriftPPM, totalRTTUS float64
	count := 0
	for _, po := range m.offsets {
		totalOffsetUS += float64(po.Offset.Microseconds())
		totalDriftPPM += po.DriftPPM
		totalRTTUS += float64(po.RTT.Microseconds())
		count++
	}

	var avgOffsetUS, avgDriftPPM, avgRTTUS float64
	if count > 0 {
		avgOffsetUS = totalOffsetUS / float64(count)
		avgDriftPPM = totalDriftPPM / float64(count)
		avgRTTUS = totalRTTUS / float64(count)
	}

	return telemetry.ClockSyncHealth{
		OffsetUS:    avgOffsetUS,
		DriftPPM:    avgDriftPPM,
		SyncRTTUS:   avgRTTUS,
		PacketsSent: m.packetsSent.Load(),
		PacketsLost: m.packetsLost.Load(),
		IsMaster:    m.isMaster.Load(),
	}
}

// PeerOffsets returns peer sync telemetry formatted for the telemetry aggregator.
func (m *Manager) PeerOffsets() map[string]telemetry.PeerSyncOffset {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]telemetry.PeerSyncOffset, len(m.offsets))
	for k, v := range m.offsets {
		out[k] = telemetry.PeerSyncOffset{
			OffsetUS:   float64(v.Offset.Microseconds()),
			DriftPPM:   v.DriftPPM,
			RTTUS:      float64(v.RTT.Microseconds()),
			LastUpdate: v.LastUpdate,
		}
	}
	return out
}

// Close stops the manager, closes its UDP socket, and awaits termination of all background routines.
func (m *Manager) Close() {
	m.once.Do(func() {
		close(m.stopCh)

		m.mu.Lock()
		if m.conn != nil {
			_ = m.conn.Close()
			m.conn = nil
		}
		m.mu.Unlock()

		m.wg.Wait()
	})
}
