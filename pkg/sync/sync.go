package sync

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/telemetry"
)

// message is the wire format for clock-sync pings/pongs (all times in ns).
type message struct {
	Type  string `json:"type"` // "sync-request" | "sync-response"
	ID    string `json:"id"`   // sender node id (diagnostic only; not trusted for accounting)
	Nonce uint64 `json:"nonce"`
	T1    int64  `json:"t1"` // client send time
	T2    int64  `json:"t2"` // server receive time
	T3    int64  `json:"t3"` // server send time
	T4    int64  `json:"t4"` // client receive time (response only)
}

const (
	msgRequest  = "sync-request"
	msgResponse = "sync-response"
)

// PeerOffset is the estimated clock offset for a peer.
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
	cfg  *config.Config
	disc PeerSource

	mu      sync.Mutex
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

// NewManager creates a clock-sync manager for the given discovery service.
func NewManager(cfg *config.Config, disc PeerSource) *Manager {
	return &Manager{
		cfg:     cfg,
		disc:    disc,
		offsets: make(map[string]PeerOffset),
		pending: make(map[uint64]time.Time),
		stopCh:  make(chan struct{}),
	}
}

// Run starts the sync loop. It blocks until Close is called.
func (m *Manager) Run() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	if m.cfg.SyncPort > 0 {
		if c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: m.cfg.SyncPort}); err == nil {
			m.conn = c
			m.wg.Add(1)
			go m.recvLoop(c)
		} else {
			log.Printf("sync: cannot bind UDP port %d: %v", m.cfg.SyncPort, err)
		}
	}
	m.mu.Unlock()

	ticker := time.NewTicker(m.cfg.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.pingPeers()
			m.sweepPending()
		case <-m.stopCh:
			m.mu.Lock()
			if m.conn != nil {
				_ = m.conn.Close()
			}
			m.mu.Unlock()
			m.wg.Wait()
			return
		}
	}
}

func (m *Manager) recvLoop(conn *net.UDPConn) {
	defer m.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			m.mu.Lock()
			stopping := m.conn == nil
			m.mu.Unlock()
			if stopping {
				return
			}
			log.Printf("sync: receive: %v", err)
			continue
		}
		var msg message
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}
		m.handle(addr, &msg)
	}
}

// syncAddrMatches compares a source socket with an advertised UDP endpoint.
// Discovery normally emits numeric IPv4 addresses, but resolving the advertised
// value also keeps a configured hostname from being rejected merely because the
// UDP source is numeric.
func syncAddrMatches(advertised string, source *net.UDPAddr) bool {
	if source == nil {
		return false
	}
	peerAddr, err := net.ResolveUDPAddr("udp4", advertised)
	if err != nil {
		return false
	}
	return peerAddr.Port == source.Port && peerAddr.IP.Equal(source.IP)
}

// peerIDForSyncAddr resolves a sync datagram source address to the ID of a
// currently-discovered peer. It binds accounting to the address that discovery
// supplied rather than to a sender-claimed ID. The local node is excluded.
//
// This is an admission check, not cryptographic authentication: discovery's
// current token check is LAN-level and is addressed separately in finding 4.1.
func (m *Manager) peerIDForSyncAddr(addr *net.UDPAddr) (string, bool) {
	if addr == nil {
		return "", false
	}
	localID := m.disc.NodeID()
	for _, p := range m.disc.Peers() {
		if p.ID == "" || p.ID == localID || p.SyncAddr == "" {
			continue
		}
		if syncAddrMatches(p.SyncAddr, addr) {
			return p.ID, true
		}
	}
	return "", false
}

func (m *Manager) handle(addr *net.UDPAddr, msg *message) {
	switch msg.Type {
	case msgRequest:
		// Restrict sync to peers that discovery currently knows about. This
		// avoids replying to arbitrary UDP senders; it does not replace
		// message authentication, which remains an open finding 4.1 item.
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
		payload, _ := json.Marshal(resp)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.conn != nil {
			_, _ = m.conn.WriteToUDP(payload, addr)
		}
	case msgResponse:
		t4 := time.Now().UnixNano()
		nowTime := time.Now()
		m.packetsRecv.Add(1)
		// The response arrived from a concrete UDP source; resolve its
		// identity from discovery instead of trusting the sender-claimed
		// msg.ID (which any off-path attacker can set). Do this before m.mu:
		// PeerSource may take the discovery lock.
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
		t1ns := t1.UnixNano()
		// offset = ((t2 - t1) + (t3 - t4)) / 2
		offset := ((msg.T2 - t1ns) + (msg.T3 - t4)) / 2
		rtt := t4 - msg.T3 + msg.T2 - t1ns

		driftPPM := 0.0
		if prev, hasPrev := m.offsets[peerID]; hasPrev {
			dt := nowTime.Sub(prev.LastUpdate).Seconds()
			if dt > 0.05 {
				deltaOffsetSec := (time.Duration(offset) - prev.Offset).Seconds()
				instantDrift := (deltaOffsetSec / dt) * 1e6
				driftPPM = 0.8*prev.DriftPPM + 0.2*instantDrift
			} else {
				driftPPM = prev.DriftPPM
			}
		}

		m.offsets[peerID] = PeerOffset{
			Offset:     time.Duration(offset),
			RTT:        time.Duration(rtt),
			DriftPPM:   driftPPM,
			LastUpdate: nowTime,
		}
	}
}

// pingPeers sends a time-sync request to every discovered peer.
func (m *Manager) pingPeers() {
	for _, p := range m.disc.Peers() {
		if p.SyncAddr == "" {
			continue
		}
		nonce, err := randomNonce()
		if err != nil {
			continue
		}
		req := message{
			Type:  msgRequest,
			ID:    m.disc.NodeID(),
			Nonce: nonce,
			T1:    time.Now().UnixNano(),
		}
		payload, err := json.Marshal(req)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if m.conn == nil {
			m.mu.Unlock()
			return
		}
		addr, err := net.ResolveUDPAddr("udp4", p.SyncAddr)
		if err == nil {
			_, _ = m.conn.WriteToUDP(payload, addr)
			m.pending[nonce] = time.Unix(0, req.T1)
			m.packetsSent.Add(1)
		}
		m.mu.Unlock()
	}
}

// sweepPending drops sync requests older than PeerTTL that never got a reply.
func (m *Manager) sweepPending() {
	cutoff := time.Now().Add(-m.cfg.PeerTTL)
	m.mu.Lock()
	for nonce, sent := range m.pending {
		if sent.Before(cutoff) {
			delete(m.pending, nonce)
			m.packetsLost.Add(1)
		}
	}
	m.mu.Unlock()
}

func randomNonce() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

// Offsets returns a snapshot of all known clock offsets.
func (m *Manager) Offsets() map[string]PeerOffset {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()

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
	m.mu.Lock()
	defer m.mu.Unlock()

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

// Close stops the manager and closes its socket.
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
