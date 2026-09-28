package discovery

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/hashicorp/memberlist"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	beaconMagic        = "ARP2P"
	maxConcurrentJoins = 8
)

var virtualIfaceRegex = regexp.MustCompile(`^(docker\d+|veth.*|virbr\d+|incusbr\d+|lxdbr\d+|br-[a-f0-9]+|br\d+|cni\d+|u?tun\d+|tap\d+|tailscale\d+|wg\d+|vboxnet\d+|vmnet\d+|awdl\d+|llw\d+)`)

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
	Addr       string    `json:"addr"`       // unicast discovery/control address host:port
	SyncAddr   string    `json:"syncAddr"`   // clock-sync address host:port
	StreamAddr string    `json:"streamAddr"` // audio stream address host:port
	LastSeen   time.Time `json:"lastSeen"`
}

// nodeMetadata is the compact JSON wire format serialized into Memberlist NodeMeta.
type nodeMetadata struct {
	ID         string `json:"id"`
	Addr       string `json:"addr,omitempty"`
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

	mu        sync.RWMutex
	peers     map[string]*Peer
	ownAddr   string
	ownSync   string
	ownStream string
	boundPort int

	mlistMu sync.RWMutex
	mlist   *memberlist.Memberlist

	sockMu          sync.RWMutex
	candidateIfaces []*net.Interface
	bcastAddrs      []*net.UDPAddr
	globalBcast     *net.UDPAddr
	annSend         *net.UDPConn
	annSendPConn    *ipv4.PacketConn
	mcast           *net.UDPConn
	group           *net.UDPAddr
	dialed          bool

	joinMu          sync.Mutex
	lastJoinAttempt map[string]time.Time
	joinSem         chan struct{}

	started   atomic.Bool
	stopCh    chan struct{}
	closeOnce sync.Once
	closeMu   sync.RWMutex
	wg        sync.WaitGroup
}

// New creates a Discovery instance and synchronously initializes sockets,
// determines candidate interfaces, and creates the HashiCorp Memberlist cluster.
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

func isVirtualInterface(name string) bool {
	return virtualIfaceRegex.MatchString(name)
}

func findCandidateInterfaces() (candidates []*net.Interface, bcastIPs []net.IP) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}

	var physical []*net.Interface
	var virtual []*net.Interface
	bcastMap := make(map[string]net.IP)
	physBcastMap := make(map[string]net.IP)

	for i := range ifaces {
		ifc := &ifaces[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		hasValidIPv4 := false
		var ifcBcasts []net.IP
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() || ip4.IsLinkLocalUnicast() {
				continue
			}
			hasValidIPv4 = true
			if bcast := SubnetBroadcast(ip4, ipn.Mask); bcast != nil {
				ifcBcasts = append(ifcBcasts, bcast)
			}
		}
		if !hasValidIPv4 {
			continue
		}

		if isVirtualInterface(ifc.Name) {
			virtual = append(virtual, ifc)
			for _, b := range ifcBcasts {
				bcastMap[b.String()] = b
			}
		} else {
			physical = append(physical, ifc)
			for _, b := range ifcBcasts {
				physBcastMap[b.String()] = b
			}
		}
	}

	if len(physical) > 0 {
		candidates = physical
		for _, b := range physBcastMap {
			bcastIPs = append(bcastIPs, b)
		}
	} else if len(virtual) > 0 {
		log.Printf("[DISCOVERY:WARN] no physical interfaces found, falling back to virtual/container interfaces")
		candidates = virtual
		for _, b := range bcastMap {
			bcastIPs = append(bcastIPs, b)
		}
	}

	sort.Slice(bcastIPs, func(i, j int) bool {
		return bcastIPs[i].String() < bcastIPs[j].String()
	})

	return candidates, bcastIPs
}

func listenConfigDualMode() net.ListenConfig {
	return net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
				_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			})
		},
	}
}

func listenConfigBroadcast() net.ListenConfig {
	return net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			})
		},
	}
}

func (d *Discovery) setup() error {
	d.closeSockets()

	d.mlistMu.Lock()
	if d.mlist != nil {
		_ = d.mlist.Shutdown()
		d.mlist = nil
	}
	d.mlistMu.Unlock()

	candidates, bcastIPs := findCandidateInterfaces()

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
	var annSendPConn *ipv4.PacketConn
	dialed := false

	mcastIP := net.ParseIP(d.cfg.MulticastGroup)
	if mcastIP != nil && mcastIP.IsMulticast() && d.cfg.MulticastPort > 0 {
		group = &net.UDPAddr{IP: mcastIP, Port: d.cfg.MulticastPort}

		// Wildcard multicast & broadcast listener on MulticastPort
		lcMcast := listenConfigDualMode()
		if pc, err := lcMcast.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", d.cfg.MulticastPort)); err == nil {
			mcastConn = pc.(*net.UDPConn)
			pconn := ipv4.NewPacketConn(mcastConn)
			if len(candidates) > 0 {
				for _, ifc := range candidates {
					if err := pconn.JoinGroup(ifc, group); err != nil {
						log.Printf("[DISCOVERY:WARN] join multicast group on %s: %v", ifc.Name, err)
					}
				}
			} else {
				if err := pconn.JoinGroup(nil, group); err != nil {
					log.Printf("[DISCOVERY:WARN] join multicast group default: %v", err)
				}
			}
			if lo, err := net.InterfaceByName("lo"); err == nil {
				_ = pconn.JoinGroup(lo, group)
			}
		} else {
			log.Printf("[DISCOVERY:WARN] cannot bind wildcard dual-mode socket on :%d: %v", d.cfg.MulticastPort, err)
		}
	}

	// Outbound send socket configured with SO_BROADCAST
	if d.cfg.MulticastPort > 0 {
		lcSend := listenConfigBroadcast()
		if pc, err := lcSend.ListenPacket(context.Background(), "udp4", ":0"); err == nil {
			annSendConn = pc.(*net.UDPConn)
			annSendPConn = ipv4.NewPacketConn(annSendConn)
			_ = annSendPConn.SetMulticastLoopback(true)
		} else {
			log.Printf("[DISCOVERY:WARN] cannot bind outbound broadcast socket: %v", err)
			if c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0}); err == nil {
				annSendConn = c
				annSendPConn = ipv4.NewPacketConn(annSendConn)
			} else if group != nil {
				if s, err := net.DialUDP("udp4", nil, group); err == nil {
					annSendConn = s
					dialed = true
				}
			}
		}
	}

	d.sockMu.Lock()
	d.group = group
	d.candidateIfaces = candidates
	d.bcastAddrs = bcastAddrs
	d.globalBcast = globalBcast
	d.mcast = mcastConn
	d.annSend = annSendConn
	d.annSendPConn = annSendPConn
	d.dialed = dialed
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

	// Pre-populate ownSync and ownStream so that NodeMeta() has them available during Memberlist init
	d.mu.Lock()
	d.ownSync = net.JoinHostPort(advertisedIP, strconv.Itoa(syncPort))
	d.ownStream = net.JoinHostPort(advertisedIP, streamPort)
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
	if bindPort > 0 {
		mcfg.AdvertisePort = bindPort
	}
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

	ifaceNames := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ifaceNames = append(ifaceNames, c.Name)
	}
	bcastStrs := make([]string, 0, len(bcastAddrs))
	for _, b := range bcastAddrs {
		bcastStrs = append(bcastStrs, b.String())
	}
	log.Printf("[DISCOVERY:BIND] memberlist=%s advertised=%s multicast_group=%s:%d ifaces=%v bcast_targets=%v",
		d.ownAddr, advertisedIP, d.cfg.MulticastGroup, d.cfg.MulticastPort, ifaceNames, bcastStrs)

	return nil
}

// Init synchronously initializes discovery sockets, interfaces, and Memberlist (alias for setup).
func (d *Discovery) Init() error {
	return d.setup()
}

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
			if ip := ipn.IP.To4(); ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return ip.String()
			}
		}
	}
	return ""
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

	d.sockMu.RLock()
	candidates := d.candidateIfaces
	d.sockMu.RUnlock()

	if len(candidates) > 0 {
		for _, ifc := range candidates {
			if ip := interfaceIPv4(ifc.Name); ip != "" {
				return ip
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
	if d.cfg.AuthToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(d.cfg.AuthToken)) != 1 {
		log.Printf("[DISCOVERY:WARN] unauthorized node rejected from %s (%s): token mismatch", peer.Name, peer.Address())
		return errors.New("auth token mismatch")
	}

	// Refresh peer LastSeen timestamp so room manager does not prematurely time out members
	now := time.Now()
	d.mu.Lock()
	if p, ok := d.peers[peer.Name]; ok {
		p.LastSeen = now
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
	var syncAddr, streamAddr string
	if meta.SyncAddr != "" && ValidAddr(meta.SyncAddr) {
		syncAddr = meta.SyncAddr
	}
	if meta.StreamAddr != "" && ValidAddr(meta.StreamAddr) {
		streamAddr = meta.StreamAddr
	}

	addr := node.Address()
	p := &Peer{
		ID:         peerID,
		Addr:       addr,
		SyncAddr:   syncAddr,
		StreamAddr: streamAddr,
		LastSeen:   time.Now(),
	}
	d.mu.Lock()
	d.peers[peerID] = p
	d.mu.Unlock()
	log.Printf("[MEMBERLIST:JOIN] Node=%s Addr=%s StreamAddr=%s SyncAddr=%s", peerID, addr, streamAddr, syncAddr)
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
		if meta.SyncAddr != "" && ValidAddr(meta.SyncAddr) {
			p.SyncAddr = meta.SyncAddr
		}
		if meta.StreamAddr != "" && ValidAddr(meta.StreamAddr) {
			p.StreamAddr = meta.StreamAddr
		}
		p.LastSeen = now
	} else {
		var syncAddr, streamAddr string
		if meta.SyncAddr != "" && ValidAddr(meta.SyncAddr) {
			syncAddr = meta.SyncAddr
		}
		if meta.StreamAddr != "" && ValidAddr(meta.StreamAddr) {
			streamAddr = meta.StreamAddr
		}
		d.peers[peerID] = &Peer{
			ID:         peerID,
			Addr:       addr,
			SyncAddr:   syncAddr,
			StreamAddr: streamAddr,
			LastSeen:   now,
		}
	}
	d.mu.Unlock()
	log.Printf("[MEMBERLIST:UPDATE] Node=%s Addr=%s StreamAddr=%s SyncAddr=%s", peerID, addr, meta.StreamAddr, meta.SyncAddr)
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
	if d.isStopping() {
		return
	}
	if d.started.Swap(true) {
		return
	}
	d.wg.Add(1)
	defer d.wg.Done()

	d.mlistMu.RLock()
	mlist := d.mlist
	d.mlistMu.RUnlock()
	if mlist == nil {
		if err := d.setup(); err != nil {
			log.Printf("[DISCOVERY:WARN] setup failed: %v", err)
			return
		}
		d.mlistMu.RLock()
		mlist = d.mlist
		d.mlistMu.RUnlock()
	}

	// 1. Join static seed peers from cfg.StaticPeers
	if len(d.cfg.StaticPeers) > 0 && mlist != nil {
		var seedAddrs []string
		for _, sp := range d.cfg.StaticPeers {
			if sp.Addr != "" {
				seedAddrs = append(seedAddrs, sp.Addr)
			}
		}
		if len(seedAddrs) > 0 {
			numJoined, err := mlist.Join(seedAddrs)
			if err != nil {
				log.Printf("[DISCOVERY:WARN] failed to join static seed peers: %v", err)
			} else {
				log.Printf("[DISCOVERY:JOIN] joined %d/%d static seed peers", numJoined, len(seedAddrs))
			}
		}
	}

	// 2. Start LAN beacon listener if MulticastPort is configured
	d.sockMu.RLock()
	mcast := d.mcast
	d.sockMu.RUnlock()

	if mcast != nil {
		d.wg.Add(1)
		go d.beaconRecvLoop(mcast)
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
	if d.cfg.AuthToken == "" || subtle.ConstantTimeCompare([]byte(b.Token), []byte(d.cfg.AuthToken)) != 1 {
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
	annSendPConn := d.annSendPConn
	dialed := d.dialed
	group := d.group
	candidates := d.candidateIfaces
	bcastAddrs := d.bcastAddrs
	globalBcast := d.globalBcast
	d.sockMu.RUnlock()

	if annSend == nil {
		return
	}

	if dialed {
		_, _ = annSend.Write(payload)
		return
	}

	// 1. Multicast egress
	if group != nil {
		if annSendPConn != nil && len(candidates) > 0 {
			for _, ifc := range candidates {
				_ = annSendPConn.SetMulticastInterface(ifc)
				if _, err := annSend.WriteToUDP(payload, group); err != nil {
					if errors.Is(err, net.ErrClosed) || d.isStopping() {
						return
					}
					log.Printf("[DISCOVERY:WARN] multicast beacon to %s on %s: %v", group, ifc.Name, err)
				}
			}
		} else {
			if _, err := annSend.WriteToUDP(payload, group); err != nil {
				if errors.Is(err, net.ErrClosed) || d.isStopping() {
					return
				}
				log.Printf("[DISCOVERY:WARN] multicast beacon to %s: %v", group, err)
			}
		}
		// Also send to loopback if available
		if lo, err := net.InterfaceByName("lo"); err == nil && annSendPConn != nil {
			_ = annSendPConn.SetMulticastInterface(lo)
			_, _ = annSend.WriteToUDP(payload, group)
		}
	}

	// 2. Directed subnet broadcast egress
	for _, baddr := range bcastAddrs {
		if _, err := annSend.WriteToUDP(payload, baddr); err != nil {
			if errors.Is(err, net.ErrClosed) || d.isStopping() {
				return
			}
			log.Printf("[DISCOVERY:WARN] subnet broadcast to %s: %v", baddr, err)
		}
	}

	// 3. Limited global broadcast fallback
	if globalBcast != nil {
		if _, err := annSend.WriteToUDP(payload, globalBcast); err != nil {
			if errors.Is(err, net.ErrClosed) || d.isStopping() {
				return
			}
			log.Printf("[DISCOVERY:WARN] global broadcast: %v", err)
		}
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
	d.annSendPConn = nil
	d.mcast = nil
	d.sockMu.Unlock()

	for _, conn := range conns {
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
