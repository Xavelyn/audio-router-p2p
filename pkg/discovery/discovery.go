package discovery

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/audio-router-p2p/internal/config"
)

const announceType = "announce"

// Peer is a node discovered on the network.
type Peer struct {
	ID         string    `json:"id"`
	Addr       string    `json:"addr"`       // unicast discovery/control address host:port
	SyncAddr   string    `json:"syncAddr"`   // clock-sync address host:port
	StreamAddr string    `json:"streamAddr"` // audio stream address host:port
	LastSeen   time.Time `json:"lastSeen"`
}

// announcement is the (de)serialized wire format of a discovery message.
type announcement struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Addr       string `json:"addr"`
	SyncAddr   string `json:"syncAddr"`
	StreamAddr string `json:"streamAddr"`
	Token      string `json:"token,omitempty"`
	TS         int64  `json:"ts"`
}

// Discovery discovers peers over UDP multicast and maintains a TTL-expiring
// table keyed by node ID.
type Discovery struct {
	cfg    *config.Config
	nodeID string

	mu        sync.RWMutex
	peers     map[string]*Peer
	ownAddr   string
	ownSync   string
	ownStream string
	recvOnce  sync.Once

	annSend *net.UDPConn // writes announcements to the multicast group
	control *net.UDPConn // receives unicast announcements on DiscoveryPort
	mcast   *net.UDPConn // receives multicast announcements (nil if unsupported)
	group   *net.UDPAddr
	dialed  bool // annSend was created with DialUDP (connected socket)

	started   atomic.Bool
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New creates a Discovery. nodeID may be empty, in which case one is generated.
func New(cfg *config.Config, nodeID string) (*Discovery, error) {
	if nodeID == "" {
		gen, err := generateNodeID()
		if err != nil {
			return nil, err
		}
		nodeID = gen
	}
	d := &Discovery{
		cfg:    cfg,
		nodeID: nodeID,
		peers:  make(map[string]*Peer),
		stopCh: make(chan struct{}),
	}
	return d, nil
}

func generateNodeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NodeID returns this node's ID.
func (d *Discovery) NodeID() string { return d.nodeID }

// Peers returns a snapshot sorted by ID.
func (d *Discovery) Peers() []Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Peer, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PeerAddrs returns the discovery control addresses of all live peers.
func (d *Discovery) PeerAddrs() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p.Addr)
	}
	return out
}

// PeerByID returns a peer by its ID, or nil if not found.
func (d *Discovery) PeerByID(id string) *Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if p, ok := d.peers[id]; ok {
		return p
	}
	return nil
}

// PeerByAddr returns a peer by its stream address, or nil if not found.
func (d *Discovery) PeerByAddr(addr string) *Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, p := range d.peers {
		if p.StreamAddr == addr || p.Addr == addr {
			return p
		}
	}
	return nil
}

// AddPeer manually adds a peer to the discovery table (for testing purposes).
func (d *Discovery) AddPeer(peer *Peer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peers[peer.ID] = peer
}

// OwnAddr is this node's advertised discovery control address.
func (d *Discovery) OwnAddr() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ownAddr
}

// OwnSyncAddr is this node's advertised clock-sync address.
func (d *Discovery) OwnSyncAddr() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ownSync
}

// OwnStreamAddr is this node's advertised audio stream address.
func (d *Discovery) OwnStreamAddr() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ownStream
}

// Run starts the announcement loop and receive listeners. It blocks until
// Close is called.
func (d *Discovery) Run() {
	if d.started.Swap(true) {
		return
	}
	d.setup()

	if d.mcast != nil {
		d.wg.Add(1)
		go d.recvLoop(d.mcast, "multicast")
	}
	if d.control != nil {
		d.wg.Add(1)
		go d.recvLoop(d.control, "unicast")
	}

	annTicker := time.NewTicker(d.cfg.AnnounceInterval)
	defer annTicker.Stop()
	sweepTicker := time.NewTicker(d.cfg.AnnounceInterval)
	defer sweepTicker.Stop()

	// Announce immediately so peers learn about this node quickly.
	d.sendAnnounceNow()

	for {
		select {
		case <-annTicker.C:
			d.sendAnnounceNow()
		case <-sweepTicker.C:
			d.prune()
		case <-d.stopCh:
			d.closeSockets()
			d.wg.Wait()
			return
		}
	}
}

func (d *Discovery) setup() {
	group := &net.UDPAddr{IP: net.ParseIP(d.cfg.MulticastGroup), Port: d.cfg.MulticastPort}
	d.group = group

	// Join the multicast group on the first suitable interface.
	for _, ifc := range candidateInterfaces() {
		c, err := net.ListenMulticastUDP("udp4", ifc, group)
		if err != nil {
			continue
		}
		d.mcast = c
		if ip := interfaceIPv4(ifc.Name); ip != "" {
			// Binding the send socket to the interface address forces multicast
			// delivery out of that interface without needing IP_MULTICAST_IF.
			if s, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: 0}); err == nil {
				d.annSend = s
			}
		}
		break
	}
	if d.annSend == nil {
		// Fall back to a routed socket; the kernel picks the interface.
		if s, err := net.DialUDP("udp4", nil, group); err == nil {
			d.annSend = s
			d.dialed = true
		}
	}
	if d.mcast == nil || d.annSend == nil {
		log.Printf("discovery: multicast unavailable; announcements will be unicast-only")
	}

	// Use SO_REUSEADDR and port 0 for auto-assignment
	if d.cfg.DiscoveryPort >= 0 {
		lc := net.ListenConfig{
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				})
			},
		}
		if d.control != nil {
			d.control.Close()
		}
		if c, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", d.cfg.DiscoveryPort)); err == nil {
			d.control = c.(*net.UDPConn)
		} else {
			log.Printf("discovery: cannot bind unicast control port %d: %v", d.cfg.DiscoveryPort, err)
		}
	}

	advertisedIP := d.advertisedIP()
	d.mu.Lock()
	d.ownAddr = net.JoinHostPort(advertisedIP, strconv.Itoa(d.cfg.DiscoveryPort))
	d.ownSync = net.JoinHostPort(advertisedIP, strconv.Itoa(d.cfg.SyncPort))
	// Extract port from StreamPort (format: "host:port" or ":port")
	_, streamPort, _ := net.SplitHostPort(d.cfg.StreamPort)
	d.ownStream = net.JoinHostPort(advertisedIP, streamPort)
	d.mu.Unlock()
}

func candidateInterfaces() []*net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []*net.Interface
	for i := range ifaces {
		f := ifaces[i].Flags
		if f&net.FlagUp != 0 && f&net.FlagMulticast != 0 {
			out = append(out, &ifaces[i])
		}
	}
	return out
}

// interfaceIPv4 returns the first non-unspecified IPv4 address of an interface.
func interfaceIPv4(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip := ipn.IP.To4(); ip != nil && !ip.IsUnspecified() {
				return ip.String()
			}
		}
	}
	return ""
}

func (d *Discovery) advertisedIP() string {
	if d.cfg.AdvHost != "" {
		return d.cfg.AdvHost
	}
	if d.annSend != nil {
		if la, ok := d.annSend.LocalAddr().(*net.UDPAddr); ok {
			if ip := la.IP; ip != nil && !ip.IsUnspecified() {
				return ip.String()
			}
		}
	}
	if ip := firstNonLoopbackIPv4(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

func firstNonLoopbackIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				return ip.String()
			}
		}
	}
	return ""
}

// sendAnnounceNow sends this node's announcement to the multicast group and to
// every known peer (unicast fallback that also bootstraps loopback setups).
func (d *Discovery) sendAnnounceNow() {
	d.mu.RLock()
	msg := announcement{
		Type:       announceType,
		ID:         d.nodeID,
		Addr:       d.ownAddr,
		SyncAddr:   d.ownSync,
		StreamAddr: d.ownStream,
		Token:      d.cfg.AuthToken,
		TS:         time.Now().UnixNano(),
	}
	peerAddrs := make([]string, 0, len(d.peers))
	for _, p := range d.peers {
		peerAddrs = append(peerAddrs, p.Addr)
	}
	d.mu.RUnlock()

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("discovery: marshal announcement: %v", err)
		return
	}
	if d.annSend != nil {
		if d.dialed {
			_, werr := d.annSend.Write(payload)
			if werr != nil && !d.isStopping() {
				log.Printf("discovery: multicast announce: %v", werr)
			}
		} else if _, werr := d.annSend.WriteToUDP(payload, d.group); werr != nil && !d.isStopping() {
			log.Printf("discovery: multicast announce: %v", werr)
		}
	}
	for _, addr := range peerAddrs {
		d.sendUnicast(addr, payload)
	}
}

func (d *Discovery) sendUnicast(addr string, payload []byte) {
	conn, err := net.Dial("udp4", addr)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil && !d.isStopping() {
		log.Printf("discovery: unicast announce to %s: %v", addr, err)
	}
}

// recvLoop reads datagrams from conn and records valid announcements.
func (d *Discovery) recvLoop(conn *net.UDPConn, name string) {
	defer d.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if d.isStopping() {
				return
			}
			log.Printf("discovery: %s receive: %v", name, err)
			continue
		}
		var msg announcement
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}
		d.handleAnnouncement(msg)
	}
}

func (d *Discovery) handleAnnouncement(msg announcement) {
	if msg.Type != announceType || msg.ID == "" || msg.ID == d.nodeID {
		return
	}
	if subtle.ConstantTimeCompare([]byte(msg.Token), []byte(d.cfg.AuthToken)) != 1 {
		return
	}
	if msg.Addr == "" && msg.SyncAddr == "" && msg.StreamAddr == "" {
		return
	}
	now := time.Now()
	d.mu.Lock()
	p, ok := d.peers[msg.ID]
	if !ok {
		p = &Peer{ID: msg.ID}
		d.peers[msg.ID] = p
	}
	p.Addr = msg.Addr
	p.SyncAddr = msg.SyncAddr
	p.StreamAddr = msg.StreamAddr
	p.LastSeen = now
	d.mu.Unlock()
}

// prune removes peers that have not been refreshed within PeerTTL.
func (d *Discovery) prune() {
	cutoff := time.Now().Add(-d.cfg.PeerTTL)
	d.mu.Lock()
	for id, p := range d.peers {
		if p.LastSeen.Before(cutoff) {
			delete(d.peers, id)
		}
	}
	d.mu.Unlock()
}

func (d *Discovery) isStopping() bool {
	select {
	case <-d.stopCh:
		return true
	default:
		return false
	}
}

// Close stops discovery and releases its sockets.
func (d *Discovery) Close() {
	d.closeOnce.Do(func() {
		close(d.stopCh)
		d.closeSockets()
		d.wg.Wait()
	})
}

func (d *Discovery) closeSockets() {
	for _, conn := range []*net.UDPConn{d.annSend, d.control, d.mcast} {
		if conn != nil {
			_ = conn.Close()
		}
	}
}

// ValidAddr reports whether addr parses as a host:port pair.
func ValidAddr(addr string) bool {
	_, _, err := net.SplitHostPort(addr)
	return err == nil
}
