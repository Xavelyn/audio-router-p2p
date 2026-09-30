package discovery

import (
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/telemetry"
	"github.com/hashicorp/memberlist"
)

const (
	beaconMagic        = "ARP2P"
	maxConcurrentJoins = 8
)

// Ensure Discovery implements all required Memberlist delegate interfaces at compile time.
var (
	_ memberlist.Delegate         = (*Discovery)(nil)
	_ memberlist.AliveDelegate    = (*Discovery)(nil)
	_ memberlist.EventDelegate    = (*Discovery)(nil)
	_ memberlist.ConflictDelegate = (*Discovery)(nil)
)

// Peer is a node discovered on the network.
type Peer struct {
	ID         string    `json:"id"`
	Addr       string    `json:"addr"`                 // unicast discovery/control address host:port
	APIAddr    string    `json:"apiAddr,omitempty"`    // HTTP/HTTPS control plane base URL
	SyncAddr   string    `json:"syncAddr"`             // clock-sync address host:port
	StreamAddr string    `json:"streamAddr"`           // audio stream address host:port
	LastSeen   time.Time `json:"lastSeen"`
}

// nodeMetadata is the compact JSON wire format serialized into Memberlist NodeMeta.
type nodeMetadata struct {
	ID         string `json:"id"`
	Addr       string `json:"addr,omitempty"`
	APIAddr    string `json:"apiAddr,omitempty"`
	SyncAddr   string `json:"syncAddr"`
	StreamAddr string `json:"streamAddr"`
	AuthToken  string `json:"authToken,omitempty"`
	Token      string `json:"token,omitempty"`
}

func (m *nodeMetadata) getAuthToken() string {
	if m.AuthToken != "" {
		return m.AuthToken
	}
	return m.Token
}

// beaconMessage is the decoupled UDP bootstrap datagram emitted for zero-conf LAN discovery.
type beaconMessage struct {
	Magic string `json:"magic"`
	ID    string `json:"id"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Discovery manages peer discovery and cluster membership via HashiCorp Memberlist
// SWIM gossip with TCP fallback, augmented with a decoupled zero-conf UDP LAN beacon.
type Discovery struct {
	cfg    *config.Config
	nodeID string

	mu         sync.RWMutex
	peers      map[string]*Peer
	ownAddr    string
	ownAPIAddr string
	ownSync    string
	ownStream  string
	boundPort  int

	mlistMu sync.RWMutex
	mlist   *memberlist.Memberlist

	sockMu      sync.RWMutex
	bcastAddrs  []*net.UDPAddr
	globalBcast *net.UDPAddr
	annSend     *net.UDPConn
	mcast       *net.UDPConn
	group       *net.UDPAddr

	joinMu          sync.Mutex
	lastJoinAttempt map[string]time.Time
	joinSem         chan struct{}

	beaconsSent       atomic.Uint64
	beaconsRecv       atomic.Uint64
	tokenRejections   atomic.Uint64
	seedJoinAttempts  atomic.Uint64
	seedJoinSuccesses atomic.Uint64

	started   atomic.Bool
	stopCh    chan struct{}
	closeOnce sync.Once
	closeMu   sync.RWMutex
	wg        sync.WaitGroup
}

// New creates a Discovery instance and synchronously initializes sockets,
// discovers local broadcast targets, and creates the HashiCorp Memberlist cluster.
func New(cfg *config.Config, nodeID string) (*Discovery, error) {
	if nodeID == "" {
		gen, err := generateNodeID()
		if err != nil {
			return nil, err
		}
		nodeID = gen
	}
	d := &Discovery{
		cfg:             cfg,
		nodeID:          nodeID,
		peers:           make(map[string]*Peer),
		lastJoinAttempt: make(map[string]time.Time),
		joinSem:         make(chan struct{}, maxConcurrentJoins),
		stopCh:          make(chan struct{}),
	}
	if err := d.setup(); err != nil {
		return nil, err
	}
	return d, nil
}

func generateNodeID() (string, error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NodeID returns this node's unique cluster ID.
func (d *Discovery) NodeID() string { return d.nodeID }

// Peers returns a snapshot of discovered live peers sorted by ID.
func (d *Discovery) Peers() []Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Peer, 0, len(d.peers))
	for _, p := range d.peers {
		if p != nil {
			out = append(out, *p)
		}
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
		if p != nil {
			out = append(out, p.Addr)
		}
	}
	return out
}

// PeerByID returns a safe copy of a peer by its ID, or nil if not found.
func (d *Discovery) PeerByID(id string) *Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if p, ok := d.peers[id]; ok && p != nil {
		cp := *p
		return &cp
	}
	return nil
}

// PeerByAddr returns a safe copy of a peer by its stream address or discovery address, or nil if not found.
func (d *Discovery) PeerByAddr(addr string) *Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, p := range d.peers {
		if p != nil && (p.StreamAddr == addr || p.Addr == addr) {
			cp := *p
			return &cp
		}
	}
	return nil
}

// AddPeer manually adds a peer to the discovery table (for testing/mock injection).
func (d *Discovery) AddPeer(peer *Peer) {
	if peer == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := *peer
	d.peers[peer.ID] = &cp
}

// Telemetry returns cluster health, discovery, and bootstrap metrics safely for telemetry consumers.
func (d *Discovery) Telemetry() telemetry.DiscoveryHealth {
	var active, suspect, dead int
	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()
	if mlist != nil {
		for _, m := range mlist.Members() {
			switch m.State {
			case memberlist.StateAlive:
				active++
			case memberlist.StateSuspect:
				suspect++
			case memberlist.StateDead:
				dead++
			}
		}
	} else {
		active = 1 // local node default
	}

	lanActive := false
	if d.cfg != nil && d.cfg.MulticastPort > 0 {
		d.sockMu.RLock()
		lanActive = d.annSend != nil || d.mcast != nil
		d.sockMu.RUnlock()
	}

	seedActive := false
	if d.cfg != nil && len(d.cfg.StaticPeers) > 0 {
		seedActive = true
	}

	return telemetry.DiscoveryHealth{
		ActiveMembers:     active,
		SuspectMembers:    suspect,
		DeadMembers:       dead,
		LANBeaconActive:   lanActive,
		BeaconsSent:       d.beaconsSent.Load(),
		BeaconsReceived:   d.beaconsRecv.Load(),
		TokenRejections:   d.tokenRejections.Load(),
		SeedRetryActive:   seedActive,
		SeedJoinAttempts:  d.seedJoinAttempts.Load(),
		SeedJoinSuccesses: d.seedJoinSuccesses.Load(),
	}
}

// DiscoveredPeers returns an aggregated view of known peers with their SWIM state.
func (d *Discovery) DiscoveredPeers() []telemetry.DiscoveredPeer {
	peers := d.Peers()
	result := make([]telemetry.DiscoveredPeer, len(peers))

	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()

	stateMap := make(map[string]string)
	if mlist != nil {
		for _, m := range mlist.Members() {
			switch m.State {
			case memberlist.StateAlive:
				stateMap[m.Name] = "alive"
			case memberlist.StateSuspect:
				stateMap[m.Name] = "suspect"
			case memberlist.StateDead:
				stateMap[m.Name] = "dead"
			}
		}
	}

	for i, p := range peers {
		state := stateMap[p.ID]
		if state == "" {
			state = "alive"
		}
		result[i] = telemetry.DiscoveredPeer{
			ID:         p.ID,
			Addr:       p.Addr,
			APIAddr:    p.APIAddr,
			SyncAddr:   p.SyncAddr,
			StreamAddr: p.StreamAddr,
			LastSeen:   p.LastSeen,
			SWIMState:  state,
		}
	}
	return result
}

// PeerState queries the SWIM failure detector state for a given peer ID.
func (d *Discovery) PeerState(peerID string) string {
	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()
	if mlist == nil {
		return "alive"
	}
	for _, m := range mlist.Members() {
		if m.Name == peerID {
			switch m.State {
			case memberlist.StateAlive:
				return "alive"
			case memberlist.StateSuspect:
				return "suspect"
			case memberlist.StateDead:
				return "dead"
			}
		}
	}
	return "alive"
}

// APIAddr returns this node's advertised HTTP/HTTPS control plane base URL.
func (d *Discovery) APIAddr() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ownAPIAddr
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

// SubnetBroadcast calculates the directed subnet broadcast address: ip | ^mask.
func SubnetBroadcast(ip net.IP, mask net.IPMask) net.IP {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	mask4 := mask
	if len(mask4) == 16 {
		mask4 = mask4[12:16]
	}
	if len(mask4) != 4 {
		return nil
	}
	bcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		bcast[i] = ip4[i] | ^mask4[i]
	}
	return bcast
}

func findBroadcastAddresses() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var bcastIPs []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
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
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() || ip4.IsLinkLocalUnicast() {
				continue
			}
			if bcast := SubnetBroadcast(ip4, ipn.Mask); bcast != nil {
				key := bcast.String()
				if _, exists := seen[key]; !exists {
					seen[key] = struct{}{}
					bcastIPs = append(bcastIPs, bcast)
				}
			}
		}
	}
	sort.Slice(bcastIPs, func(i, j int) bool {
		return bcastIPs[i].String() < bcastIPs[j].String()
	})
	return bcastIPs
}

func (d *Discovery) setup() error {
	if d.isStopping() {
		return errors.New("discovery is closed")
	}

	d.closeSockets()

	d.mlistMu.Lock()
	if d.mlist != nil {
		_ = d.mlist.Shutdown()
		d.mlist = nil
	}
	d.mlistMu.Unlock()

	bcastIPs := findBroadcastAddresses()

	var bcastAddrs []*net.UDPAddr
	var globalBcast *net.UDPAddr
	if d.cfg.MulticastPort > 0 {
		for _, bcast := range bcastIPs {
			bcastAddrs = append(bcastAddrs, &net.UDPAddr{IP: bcast, Port: d.cfg.MulticastPort})
		}
		globalBcast = &net.UDPAddr{IP: net.IPv4bcast, Port: d.cfg.MulticastPort}
	}

	var group *net.UDPAddr
	var mcastConn, annSendConn *net.UDPConn

	if d.cfg.MulticastPort > 0 {
		mcastIP := net.ParseIP(d.cfg.MulticastGroup)
		if mcastIP != nil && mcastIP.IsMulticast() {
			group = &net.UDPAddr{IP: mcastIP, Port: d.cfg.MulticastPort}
			// Inbound beacon socket: standard net.ListenMulticastUDP
			if mc, err := net.ListenMulticastUDP("udp4", nil, group); err == nil {
				mcastConn = mc
			} else {
				log.Printf("[DISCOVERY:WARN] listen multicast UDP failed: %v, falling back to ListenUDP", err)
			}
		}
		// Fallback to standard net.ListenUDP on MulticastPort
		if mcastConn == nil {
			if uc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: d.cfg.MulticastPort}); err == nil {
				mcastConn = uc
			} else {
				log.Printf("[DISCOVERY:WARN] listen inbound beacon UDP on port %d failed: %v", d.cfg.MulticastPort, err)
			}
		}

		// Outbound beacon socket: standard net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
		if sc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0}); err == nil {
			annSendConn = sc
		} else {
			log.Printf("[DISCOVERY:WARN] cannot bind outbound beacon socket: %v", err)
		}
	}

	d.sockMu.Lock()
	d.group = group
	d.bcastAddrs = bcastAddrs
	d.globalBcast = globalBcast
	d.mcast = mcastConn
	d.annSend = annSendConn
	d.sockMu.Unlock()

	advertisedIP := d.advertisedIP()

	syncPort := d.cfg.SyncPort
	if syncPort <= 0 {
		syncPort = 45402
	}

	streamPort := d.cfg.StreamPort
	if streamPort == "" {
		streamPort = "45678"
	} else {
		_, p, err := net.SplitHostPort(streamPort)
		if err == nil && p != "" {
			streamPort = p
		}
	}

	// Determine APIAddr scheme, host, and port
	scheme := "https"
	if d.cfg != nil && d.cfg.NoTLS {
		scheme = "http"
	}

	apiHost := advertisedIP
	apiPort := "8443"
	if d.cfg != nil && d.cfg.ListenAddr != "" {
		h, p, err := net.SplitHostPort(d.cfg.ListenAddr)
		if err == nil {
			if p != "" {
				apiPort = p
			}
			if h != "" && h != "0.0.0.0" && (d.cfg.AdvHost == "" || d.cfg.AdvHost == "0.0.0.0") {
				apiHost = h
			}
		} else {
			trimmed := strings.TrimPrefix(d.cfg.ListenAddr, ":")
			if _, err := strconv.Atoi(trimmed); err == nil {
				apiPort = trimmed
			}
		}
	}
	apiAddr := fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(apiHost, apiPort))

	// Pre-populate ownSync, ownStream, and ownAPIAddr so that NodeMeta() has them available during Memberlist init
	d.mu.Lock()
	d.ownSync = net.JoinHostPort(advertisedIP, strconv.Itoa(syncPort))
	d.ownStream = net.JoinHostPort(advertisedIP, streamPort)
	d.ownAPIAddr = apiAddr
	d.mu.Unlock()

	// Memberlist configuration based on DefaultLANConfig
	mcfg := memberlist.DefaultLANConfig()
	mcfg.Name = d.nodeID
	mcfg.BindAddr = "0.0.0.0"
	bindPort := d.cfg.DiscoveryPort
	if bindPort < 0 {
		bindPort = 0
	}
	mcfg.BindPort = bindPort
	mcfg.AdvertiseAddr = advertisedIP
	mcfg.AdvertisePort = bindPort
	mcfg.Delegate = d
	mcfg.Alive = d
	mcfg.Events = d
	mcfg.Conflict = d
	mcfg.DeadNodeReclaimTime = 1 * time.Millisecond
	mcfg.LogOutput = io.Discard // Suppress verbose memberlist internal logging

	mlist, err := memberlist.Create(mcfg)
	if err != nil && bindPort > 0 {
		log.Printf("[DISCOVERY:WARN] cannot bind discovery port %d: %v, falling back to ephemeral port", bindPort, err)
		mcfg.BindPort = 0
		mcfg.AdvertisePort = 0
		mlist, err = memberlist.Create(mcfg)
	}
	if err != nil {
		return fmt.Errorf("create memberlist: %w", err)
	}

	actualBoundPort := int(mlist.LocalNode().Port)

	d.mu.Lock()
	d.boundPort = actualBoundPort
	d.ownAddr = net.JoinHostPort(advertisedIP, strconv.Itoa(actualBoundPort))
	d.mu.Unlock()

	// Re-synchronize local node metadata so ownAddr is populated in Memberlist
	_ = mlist.UpdateNode(0)

	d.mlistMu.Lock()
	d.mlist = mlist
	d.mlistMu.Unlock()

	bcastStrs := make([]string, 0, len(bcastAddrs))
	for _, b := range bcastAddrs {
		bcastStrs = append(bcastStrs, b.String())
	}
	log.Printf("[DISCOVERY:BIND] memberlist=%s advertised=%s multicast_group=%s:%d bcast_targets=%v",
		d.ownAddr, advertisedIP, d.cfg.MulticastGroup, d.cfg.MulticastPort, bcastStrs)

	return nil
}

// Init synchronously initializes discovery sockets, interfaces, and Memberlist (alias for setup).
func (d *Discovery) Init() error {
	if d.isStopping() {
		return errors.New("discovery is closed")
	}
	return d.setup()
}

func (d *Discovery) advertisedIP() string {
	if d.cfg.AdvHost != "" {
		if ip := net.ParseIP(d.cfg.AdvHost); ip != nil {
			return ip.String()
		}
		if ips, err := net.LookupIP(d.cfg.AdvHost); err == nil {
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
		return d.cfg.AdvHost
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
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
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
			if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() {
				return ip.String()
			}
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Memberlist Delegates Implementation
// -----------------------------------------------------------------------------

// NodeMeta returns this node's serialized routing metadata.
func (d *Discovery) NodeMeta(limit int) []byte {
	d.mu.RLock()
	meta := nodeMetadata{
		ID:         d.nodeID,
		Addr:       d.ownAddr,
		APIAddr:    d.ownAPIAddr,
		SyncAddr:   d.ownSync,
		StreamAddr: d.ownStream,
		AuthToken:  d.cfg.AuthToken,
	}
	d.mu.RUnlock()

	b, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	if limit > 0 && len(b) > limit {
		log.Printf("[DISCOVERY:WARN] metadata size %d exceeds limit %d", len(b), limit)
		return nil
	}
	return b
}

// NotifyMsg receives user-data broadcast gossip frames.
func (d *Discovery) NotifyMsg([]byte) {}

// GetBroadcasts returns pending user broadcast messages to piggyback onto outgoing SWIM pings.
func (d *Discovery) GetBroadcasts(overhead, limit int) [][]byte {
	return nil
}

// LocalState is invoked for TCP push/pull state transfers.
func (d *Discovery) LocalState(join bool) []byte {
	return nil
}

// MergeRemoteState is invoked after a TCP push/pull state transfer.
func (d *Discovery) MergeRemoteState(buf []byte, join bool) {}

// NotifyAlive validates an incoming node alive message using constant-time token comparison.
func (d *Discovery) NotifyAlive(peer *memberlist.Node) error {
	if peer == nil {
		return errors.New("nil peer")
	}
	if peer.Name == d.nodeID {
		return nil
	}
	if len(peer.Meta) == 0 {
		return errors.New("empty node metadata")
	}
	var meta nodeMetadata
	if err := json.Unmarshal(peer.Meta, &meta); err != nil {
		return fmt.Errorf("unmarshal metadata: %w", err)
	}
	token := meta.getAuthToken()
	if subtle.ConstantTimeCompare([]byte(token), []byte(d.cfg.AuthToken)) != 1 {
		d.tokenRejections.Add(1)
		log.Printf("[DISCOVERY:WARN] unauthorized node rejected from %s (%s): token mismatch", peer.Name, peer.Address())
		return errors.New("auth token mismatch")
	}

	// Refresh peer LastSeen timestamp so room manager does not prematurely time out members
	now := time.Now()
	d.mu.Lock()
	if p, ok := d.peers[peer.Name]; ok {
		p.LastSeen = now
		if meta.APIAddr != "" && ValidAPIAddr(meta.APIAddr) {
			p.APIAddr = meta.APIAddr
		}
	}
	d.mu.Unlock()

	return nil
}

// NotifyJoin is invoked when a validated peer joins the cluster.
func (d *Discovery) NotifyJoin(node *memberlist.Node) {
	if node == nil || node.Name == d.nodeID {
		return
	}
	var meta nodeMetadata
	if len(node.Meta) > 0 {
		_ = json.Unmarshal(node.Meta, &meta)
	}

	// The cluster membership name is the authoritative peer ID.
	peerID := node.Name
	if meta.ID != "" && meta.ID != node.Name {
		log.Printf("[DISCOVERY:WARN] node %s advertised mismatched meta.ID %s; using node.Name", node.Name, meta.ID)
	}

	// Validate endpoint addresses
	var syncAddr, streamAddr, apiAddr string
	if meta.SyncAddr != "" && ValidAddr(meta.SyncAddr) {
		syncAddr = meta.SyncAddr
	}
	if meta.StreamAddr != "" && ValidAddr(meta.StreamAddr) {
		streamAddr = meta.StreamAddr
	}
	if meta.APIAddr != "" && ValidAPIAddr(meta.APIAddr) {
		apiAddr = meta.APIAddr
	}

	addr := node.Address()
	p := &Peer{
		ID:         peerID,
		Addr:       addr,
		APIAddr:    apiAddr,
		SyncAddr:   syncAddr,
		StreamAddr: streamAddr,
		LastSeen:   time.Now(),
	}
	d.mu.Lock()
	d.peers[peerID] = p
	d.mu.Unlock()
	log.Printf("[MEMBERLIST:JOIN] Node=%s Addr=%s APIAddr=%s StreamAddr=%s SyncAddr=%s", peerID, addr, meta.APIAddr, streamAddr, syncAddr)
}

// NotifyLeave is invoked when a peer leaves the cluster or fails failure detection.
func (d *Discovery) NotifyLeave(node *memberlist.Node) {
	if node == nil || node.Name == d.nodeID {
		return
	}
	d.mu.Lock()
	delete(d.peers, node.Name)
	nodeAddr := node.Address()
	for id, p := range d.peers {
		if id == node.Name || (p != nil && p.Addr == nodeAddr) {
			delete(d.peers, id)
		}
	}
	d.mu.Unlock()
	log.Printf("[MEMBERLIST:LEAVE] Node=%s Addr=%s", node.Name, node.Address())
}

// NotifyUpdate is invoked when peer metadata or addresses are updated.
func (d *Discovery) NotifyUpdate(node *memberlist.Node) {
	if node == nil || node.Name == d.nodeID {
		return
	}
	var meta nodeMetadata
	if len(node.Meta) > 0 {
		_ = json.Unmarshal(node.Meta, &meta)
	}
	peerID := node.Name
	addr := node.Address()
	now := time.Now()

	d.mu.Lock()
	if p, ok := d.peers[peerID]; ok {
		p.Addr = addr
		if meta.APIAddr != "" && ValidAPIAddr(meta.APIAddr) {
			p.APIAddr = meta.APIAddr
		}
		if meta.SyncAddr != "" && ValidAddr(meta.SyncAddr) {
			p.SyncAddr = meta.SyncAddr
		}
		if meta.StreamAddr != "" && ValidAddr(meta.StreamAddr) {
			p.StreamAddr = meta.StreamAddr
		}
		p.LastSeen = now
	} else {
		var syncAddr, streamAddr, apiAddr string
		if meta.SyncAddr != "" && ValidAddr(meta.SyncAddr) {
			syncAddr = meta.SyncAddr
		}
		if meta.StreamAddr != "" && ValidAddr(meta.StreamAddr) {
			streamAddr = meta.StreamAddr
		}
		if meta.APIAddr != "" && ValidAPIAddr(meta.APIAddr) {
			apiAddr = meta.APIAddr
		}
		d.peers[peerID] = &Peer{
			ID:         peerID,
			Addr:       addr,
			APIAddr:    apiAddr,
			SyncAddr:   syncAddr,
			StreamAddr: streamAddr,
			LastSeen:   now,
		}
	}
	d.mu.Unlock()
	log.Printf("[MEMBERLIST:UPDATE] Node=%s Addr=%s APIAddr=%s StreamAddr=%s SyncAddr=%s", peerID, addr, meta.APIAddr, meta.StreamAddr, meta.SyncAddr)
}

// NotifyConflict is invoked when a node ID conflict is detected.
func (d *Discovery) NotifyConflict(existing, other *memberlist.Node) {
	if existing == nil || other == nil {
		return
	}
	log.Printf("[MEMBERLIST:CONFLICT] Node=%s ExistingAddr=%s OtherAddr=%s", existing.Name, existing.Address(), other.Address())
}

// -----------------------------------------------------------------------------
// Lifecycle & Background Routines
// -----------------------------------------------------------------------------

// Run starts the seed peer join sequence, LAN beacon listener, and beacon emitter.
// It blocks until Close() is called.
func (d *Discovery) Run() {
	d.closeMu.RLock()
	if d.isStopping() || d.started.Swap(true) {
		d.closeMu.RUnlock()
		return
	}
	d.wg.Add(1)
	d.closeMu.RUnlock()
	defer d.wg.Done()

	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()
	if mlist == nil {
		log.Printf("[DISCOVERY:WARN] memberlist not initialized; aborting Run")
		return
	}

	// 1. Join static seed peers from cfg.StaticPeers with background retry
	if len(d.cfg.StaticPeers) > 0 {
		var seedAddrs []string
		for _, sp := range d.cfg.StaticPeers {
			if sp.Addr != "" {
				seedAddrs = append(seedAddrs, sp.Addr)
			}
		}
		if len(seedAddrs) > 0 {
			d.closeMu.RLock()
			if !d.isStopping() {
				d.wg.Add(1)
				go d.seedJoinLoop(seedAddrs)
			}
			d.closeMu.RUnlock()
		}
	}

	// 2. Start LAN beacon listener if MulticastPort is configured
	d.sockMu.RLock()
	mcast := d.mcast
	d.sockMu.RUnlock()

	if mcast != nil {
		d.closeMu.RLock()
		if !d.isStopping() {
			d.wg.Add(1)
			go d.beaconRecvLoop(mcast)
		}
		d.closeMu.RUnlock()
	}

	// 3. Periodic beacon emitter & sweep ticker
	interval := d.cfg.AnnounceInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	beaconTicker := time.NewTicker(interval)
	defer beaconTicker.Stop()

	sweepInterval := d.cfg.PeerTTL
	if sweepInterval <= 0 || sweepInterval > interval {
		sweepInterval = interval
	}
	sweepTicker := time.NewTicker(sweepInterval)
	defer sweepTicker.Stop()

	// Send initial beacon immediately
	d.sendBeaconNow()

	for {
		select {
		case <-beaconTicker.C:
			d.sendBeaconNow()
		case <-sweepTicker.C:
			d.refreshActiveMembers()
			d.prune()
		case <-d.stopCh:
			return
		}
	}
}

func (d *Discovery) seedJoinLoop(seedAddrs []string) {
	defer d.wg.Done()

	// Initial immediate attempt
	d.attemptSeedJoin(seedAddrs)

	retryTicker := time.NewTicker(3 * time.Second)
	defer retryTicker.Stop()

	for {
		select {
		case <-retryTicker.C:
			d.attemptSeedJoin(seedAddrs)
		case <-d.stopCh:
			return
		}
	}
}

func (d *Discovery) attemptSeedJoin(seedAddrs []string) {
	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()
	if mlist == nil || d.isStopping() {
		return
	}

	// Check which seeds are not yet in our peer list
	var unjoined []string
	d.mu.RLock()
	for _, addr := range seedAddrs {
		found := false
		for _, p := range d.peers {
			if p != nil && p.Addr == addr {
				found = true
				break
			}
		}
		if !found {
			unjoined = append(unjoined, addr)
		}
	}
	d.mu.RUnlock()

	if len(unjoined) == 0 {
		return
	}

	d.seedJoinAttempts.Add(1)
	numJoined, err := mlist.Join(unjoined)
	if err != nil {
		log.Printf("[DISCOVERY:WARN] seed join attempt to %v failed: %v", unjoined, err)
	} else if numJoined > 0 {
		d.seedJoinSuccesses.Add(uint64(numJoined))
		log.Printf("[DISCOVERY:JOIN] joined %d/%d seed peers", numJoined, len(unjoined))
	}
}

func (d *Discovery) refreshActiveMembers() {
	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()
	if mlist == nil {
		return
	}

	now := time.Now()
	members := mlist.Members()
	d.mu.Lock()
	for _, m := range members {
		if m != nil && m.Name != d.nodeID {
			if p, ok := d.peers[m.Name]; ok {
				p.LastSeen = now
			}
		}
	}
	d.mu.Unlock()
}

func (d *Discovery) beaconRecvLoop(conn *net.UDPConn) {
	defer d.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, srcAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || d.isStopping() {
				return
			}
			log.Printf("[DISCOVERY:WARN] beacon receive: %v", err)
			return
		}
		var b beaconMessage
		if err := json.Unmarshal(buf[:n], &b); err != nil {
			continue
		}
		d.handleBeacon(b, srcAddr)
	}
}

func (d *Discovery) handleBeacon(b beaconMessage, srcAddr *net.UDPAddr) {
	if b.Magic != beaconMagic || b.ID == "" || b.ID == d.nodeID {
		return
	}
	d.beaconsRecv.Add(1)
	if subtle.ConstantTimeCompare([]byte(b.Token), []byte(d.cfg.AuthToken)) != 1 {
		d.tokenRejections.Add(1)
		log.Printf("[DISCOVERY:WARN] rejected beacon from node %s: token mismatch", b.ID)
		return
	}

	peerAddr := b.Addr
	if peerAddr == "" || !ValidAddr(peerAddr) {
		if b.Port > 0 && srcAddr != nil {
			peerAddr = net.JoinHostPort(srcAddr.IP.String(), strconv.Itoa(b.Port))
		}
	} else if srcAddr != nil {
		host, port, err := net.SplitHostPort(peerAddr)
		if err == nil && (host == "" || host == "0.0.0.0") {
			peerAddr = net.JoinHostPort(srcAddr.IP.String(), port)
		}
	}

	if !ValidAddr(peerAddr) {
		return
	}

	now := time.Now()
	d.mu.Lock()
	if p, known := d.peers[b.ID]; known {
		p.LastSeen = now
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	d.joinMu.Lock()
	last, attempted := d.lastJoinAttempt[b.ID]
	if attempted && time.Since(last) < 2*time.Second {
		d.joinMu.Unlock()
		return
	}
	d.lastJoinAttempt[b.ID] = now
	d.joinMu.Unlock()

	// Rate-limit concurrent background join attempts
	select {
	case d.joinSem <- struct{}{}:
	default:
		// Queue saturated; drop reactive join and await next periodic cycle
		return
	}

	d.closeMu.RLock()
	if d.isStopping() {
		d.closeMu.RUnlock()
		<-d.joinSem
		return
	}
	d.wg.Add(1)
	d.closeMu.RUnlock()

	go func(target string) {
		defer func() {
			<-d.joinSem
			d.wg.Done()
		}()

		if d.isStopping() {
			return
		}
		d.mlistMu.RLock()
		mlist := d.mlist
		d.mlistMu.RUnlock()
		if mlist == nil {
			return
		}
		if _, err := mlist.Join([]string{target}); err != nil {
			log.Printf("[DISCOVERY:WARN] join beacon peer %s: %v", target, err)
		}
	}(peerAddr)
}

func (d *Discovery) sendBeaconNow() {
	if d.isStopping() {
		return
	}

	d.mu.RLock()
	b := beaconMessage{
		Magic: beaconMagic,
		ID:    d.nodeID,
		Addr:  d.ownAddr,
		Port:  d.boundPort,
		Token: d.cfg.AuthToken,
	}
	d.mu.RUnlock()

	payload, err := json.Marshal(b)
	if err != nil {
		log.Printf("[DISCOVERY:WARN] marshal beacon: %v", err)
		return
	}

	d.sockMu.RLock()
	annSend := d.annSend
	group := d.group
	bcastAddrs := d.bcastAddrs
	globalBcast := d.globalBcast
	d.sockMu.RUnlock()

	if annSend == nil {
		return
	}

	sentCount := 0
	// 1. Multicast beacon egress
	if group != nil {
		if _, err := annSend.WriteToUDP(payload, group); err != nil {
			if errors.Is(err, net.ErrClosed) || d.isStopping() {
				return
			}
			log.Printf("[DISCOVERY:WARN] multicast beacon to %s: %v", group, err)
		} else {
			sentCount++
		}
	}

	// 2. Directed subnet broadcast egress
	for _, baddr := range bcastAddrs {
		if _, err := annSend.WriteToUDP(payload, baddr); err != nil {
			if errors.Is(err, net.ErrClosed) || d.isStopping() {
				return
			}
			log.Printf("[DISCOVERY:WARN] subnet broadcast to %s: %v", baddr, err)
		} else {
			sentCount++
		}
	}

	// 3. Limited global broadcast fallback
	if globalBcast != nil {
		if _, err := annSend.WriteToUDP(payload, globalBcast); err != nil {
			if errors.Is(err, net.ErrClosed) || d.isStopping() {
				return
			}
			log.Printf("[DISCOVERY:WARN] global broadcast: %v", err)
		} else {
			sentCount++
		}
	}

	if sentCount > 0 {
		d.beaconsSent.Add(1)
	}
}

// prune removes peers that have not been refreshed within PeerTTL and cleans up join throttles.
func (d *Discovery) prune() {
	if d.cfg.PeerTTL > 0 {
		cutoff := time.Now().Add(-d.cfg.PeerTTL)
		d.mu.Lock()
		for id, p := range d.peers {
			if p.LastSeen.Before(cutoff) {
				delete(d.peers, id)
			}
		}
		d.mu.Unlock()
	}

	// Evict join throttle cache entries older than 10 seconds
	throttleCutoff := time.Now().Add(-10 * time.Second)
	d.joinMu.Lock()
	for id, t := range d.lastJoinAttempt {
		if t.Before(throttleCutoff) {
			delete(d.lastJoinAttempt, id)
		}
	}
	d.joinMu.Unlock()
}

func (d *Discovery) isStopping() bool {
	select {
	case <-d.stopCh:
		return true
	default:
		return false
	}
}

// Close gracefully stops Memberlist and shuts down discovery sockets.
func (d *Discovery) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.closeMu.Lock()
		close(d.stopCh)
		d.closeMu.Unlock()

		d.mlistMu.Lock()
		mlist := d.mlist
		d.mlist = nil
		d.mlistMu.Unlock()

		if mlist != nil {
			_ = mlist.Leave(1 * time.Second)
			err = mlist.Shutdown()
		}

		d.closeSockets()
		d.wg.Wait()
	})
	return err
}

func (d *Discovery) closeSockets() {
	d.sockMu.Lock()
	conns := []*net.UDPConn{d.annSend, d.mcast}
	d.annSend = nil
	d.mcast = nil
	d.sockMu.Unlock()

	for _, conn := range conns {
		if conn != nil {
			_ = conn.Close()
		}
	}
}

// ValidAddr reports whether addr parses as a host:port pair with a valid port number.
func ValidAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p <= 65535
}

// ValidAPIAddr reports whether apiAddr is a valid http or https URL with a non-empty host.
// It explicitly denies userinfo (credentials in URL) and non-HTTP schemes to prevent SSRF.
func ValidAPIAddr(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil || u.Host == "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
		port = ""
	}
	if host == "" {
		return false
	}
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p <= 0 || p > 65535 {
			return false
		}
	}
	return true
}
