package sync

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/discovery"
)

// message is the wire format for clock-sync pings/pongs (all times in ns).
type message struct {
	Type  string `json:"type"` // "sync-request" | "sync-response"
	ID    string `json:"id"`   // sender node id
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

func (m *Manager) handle(addr *net.UDPAddr, msg *message) {
	switch msg.Type {
	case msgRequest:
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
		m.offsets[msg.ID] = PeerOffset{
			Offset:     time.Duration(offset),
			RTT:        time.Duration(rtt),
			LastUpdate: time.Now(),
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
