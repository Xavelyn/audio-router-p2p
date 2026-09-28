package discovery

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	randv2 "math/rand/v2"
	"net"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	announceType                   = "announce"
	maxConcurrentReactiveAnnounces = 32
)

var virtualIfaceRegex = regexp.MustCompile(`^(docker\d+|veth.*|virbr\d+|br-[a-f0-9]+|br\d+|cni\d+|u?tun\d+|tap\d+|tailscale\d+|wg\d+|vboxnet\d+|vmnet\d+|awdl\d+|llw\d+)`)

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

// Discovery discovers peers over UDP multicast and broadcast, and maintains a TTL-expiring
// table keyed by node ID.
type Discovery struct {
	cfg    *config.Config
	nodeID string

	mu        sync.RWMutex
	sockMu    sync.RWMutex
	closeMu   sync.RWMutex
	peers     map[string]*Peer
	ownAddr   string
	ownSync   string
	ownStream string

	candidateIfaces []*net.Interface
	bcastAddrs      []*net.UDPAddr
	globalBcast     *net.UDPAddr

	replyMu     sync.Mutex
	lastReplied map[string]time.Time
	reactiveSem chan struct{}

	annSend      *net.UDPConn     // writes announcements (multicast, subnet bcast, global bcast)
	annSendPConn *ipv4.PacketConn // cached packet connection for multicast interface selection
	control      *net.UDPConn     // receives unicast announcements on DiscoveryPort
	mcast        *net.UDPConn     // receives multicast & broadcast announcements on MulticastPort
	group        *net.UDPAddr
	dialed       bool // annSend was created with DialUDP (connected socket)

	started   atomic.Bool
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New creates a Discovery and synchronously binds sockets, determines interfaces,
// and populates advertised addresses before returning.
func New(cfg *config.Config, nodeID string) (*Discovery, error) {
	if nodeID == "" {
		gen, err := generateNodeID()
		if err != nil {
			return nil, err
		}
		nodeID = gen
	}
	d := &Discovery{
		cfg:         cfg,
		nodeID:      nodeID,
		peers:       make(map[string]*Peer),
		lastReplied: make(map[string]time.Time),
		reactiveSem: make(chan struct{}, maxConcurrentReactiveAnnounces),
		stopCh:      make(chan struct{}),
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
	var mcastConn, annSendConn, controlConn *net.UDPConn
	var annSendPConn *ipv4.PacketConn
	dialed := false

	mcastIP := net.ParseIP(d.cfg.MulticastGroup)
	if mcastIP != nil && mcastIP.IsMulticast() && d.cfg.MulticastPort > 0 {
		group = &net.UDPAddr{IP: mcastIP, Port: d.cfg.MulticastPort}

		// 1. Wildcard multicast & broadcast listener on MulticastPort
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
		} else {
			log.Printf("[DISCOVERY:WARN] cannot bind wildcard dual-mode socket on :%d: %v, falling back to ListenMulticastUDP", d.cfg.MulticastPort, err)
			if len(candidates) > 0 {
				for _, ifc := range candidates {
					if c, err := net.ListenMulticastUDP("udp4", ifc, group); err == nil {
						mcastConn = c
						break
					}
				}
			}
			if mcastConn == nil {
				if c, err := net.ListenMulticastUDP("udp4", nil, group); err == nil {
					mcastConn = c
				}
			}
		}
	}

	// 2. Outbound send socket configured with SO_BROADCAST
	lcSend := listenConfigBroadcast()
	if pc, err := lcSend.ListenPacket(context.Background(), "udp4", ":0"); err == nil {
		annSendConn = pc.(*net.UDPConn)
		annSendPConn = ipv4.NewPacketConn(annSendConn)
	} else {
		log.Printf("[DISCOVERY:WARN] cannot bind outbound broadcast socket: %v", err)
		if c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0}); err == nil {
			annSendConn = c
			annSendPConn = ipv4.NewPacketConn(annSendConn)
		} else if s, err := net.DialUDP("udp4", nil, group); err == nil {
			annSendConn = s
			dialed = true
		}
	}

	if mcastConn == nil || annSendConn == nil {
		log.Printf("[DISCOVERY:WARN] multicast unavailable; announcements will be unicast-only")
	}

	// 3. Unicast control listener on DiscoveryPort
	boundDiscoveryPort := d.cfg.DiscoveryPort
	if d.cfg.DiscoveryPort >= 0 {
		lcControl := listenConfigDualMode()
		if c, err := lcControl.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", d.cfg.DiscoveryPort)); err == nil {
			controlConn = c.(*net.UDPConn)
			if addr, ok := controlConn.LocalAddr().(*net.UDPAddr); ok {
				boundDiscoveryPort = addr.Port
			}
		} else {
			log.Printf("[DISCOVERY:WARN] cannot bind unicast control port %d: %v", d.cfg.DiscoveryPort, err)
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
	d.control = controlConn
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

	d.mu.Lock()
	d.ownAddr = net.JoinHostPort(advertisedIP, strconv.Itoa(boundDiscoveryPort))
	d.ownSync = net.JoinHostPort(advertisedIP, strconv.Itoa(syncPort))
	d.ownStream = net.JoinHostPort(advertisedIP, streamPort)
	d.mu.Unlock()

	ifaceNames := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ifaceNames = append(ifaceNames, c.Name)
	}
	bcastStrs := make([]string, 0, len(bcastAddrs))
	for _, b := range bcastAddrs {
		bcastStrs = append(bcastStrs, b.String())
	}
	log.Printf("[DISCOVERY:BIND] multicast=%s:%d control=%s ifaces=%v bcast_targets=%v",
		d.cfg.MulticastGroup, d.cfg.MulticastPort, d.ownAddr, ifaceNames, bcastStrs)

	return nil
}

// Init synchronously initializes discovery sockets and addresses (alias for setup).
func (d *Discovery) Init() error {
	return d.setup()
}

// interfaceIPv4 returns the first non-unspecified, non-loopback IPv4 address of an interface.
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
		return d.cfg.AdvHost
	}

	d.sockMu.RLock()
	candidates := d.candidateIfaces
	annSend := d.annSend
	d.sockMu.RUnlock()

	if len(candidates) > 0 {
		for _, ifc := range candidates {
			if ip := interfaceIPv4(ifc.Name); ip != "" {
				return ip
			}
		}
	}
	if annSend != nil {
		if la, ok := annSend.LocalAddr().(*net.UDPAddr); ok {
			if ip := la.IP; ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() {
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

// Run starts the announcement loop and receive listeners. It blocks until Close is called.
func (d *Discovery) Run() {
	if d.started.Swap(true) {
		return
	}
	d.wg.Add(1)
	defer d.wg.Done()

	d.sockMu.RLock()
	mcast := d.mcast
	control := d.control
	d.sockMu.RUnlock()

	if mcast == nil && control == nil {
		_ = d.setup()
		d.sockMu.RLock()
		mcast = d.mcast
		control = d.control
		d.sockMu.RUnlock()
	}

	if mcast != nil {
		d.wg.Add(1)
		go d.recvLoop(mcast, "multicast")
	}
	if control != nil {
		d.wg.Add(1)
		go d.recvLoop(control, "unicast")
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
			return
		}
	}
}

// sendAnnounceNow sends this node's announcement via multicast, directed subnet broadcast,
// limited global broadcast, and unicast to known peers.
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
		log.Printf("[DISCOVERY:WARN] marshal announcement: %v", err)
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

	if annSend != nil {
		if dialed {
			_, werr := annSend.Write(payload)
			if werr != nil && !d.isStopping() {
				log.Printf("[DISCOVERY:WARN] multicast announce: %v", werr)
			}
		} else {
			// 1. Multicast egress (per-interface if possible, or directly to group)
			if annSendPConn != nil && len(candidates) > 0 {
				for _, ifc := range candidates {
					_ = annSendPConn.SetMulticastInterface(ifc)
					_, _ = annSend.WriteToUDP(payload, group)
				}
			} else if group != nil {
				_, _ = annSend.WriteToUDP(payload, group)
			}

			// 2. Directed subnet broadcast egress
			for _, baddr := range bcastAddrs {
				_, _ = annSend.WriteToUDP(payload, baddr)
			}

			// 3. Limited global broadcast fallback
			if globalBcast != nil {
				_, _ = annSend.WriteToUDP(payload, globalBcast)
			}
		}
	}

	// 4. Known peer unicast refresh
	for _, addr := range peerAddrs {
		d.sendUnicast(addr, payload)
	}
}

func (d *Discovery) sendUnicast(addr string, payload []byte) {
	if addr == "" {
		return
	}
	d.sockMu.RLock()
	annSend := d.annSend
	dialed := d.dialed
	d.sockMu.RUnlock()

	if annSend != nil && !dialed {
		uaddr, err := net.ResolveUDPAddr("udp4", addr)
		if err == nil {
			if _, err := annSend.WriteToUDP(payload, uaddr); err == nil {
				return
			}
		}
	}

	conn, err := net.DialTimeout("udp4", addr, 500*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil && !d.isStopping() {
		log.Printf("[DISCOVERY:WARN] unicast announce to %s: %v", addr, err)
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
			log.Printf("[DISCOVERY:WARN] %s receive: %v", name, err)
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
	if (msg.Addr != "" && !ValidAddr(msg.Addr)) ||
		(msg.SyncAddr != "" && !ValidAddr(msg.SyncAddr)) ||
		(msg.StreamAddr != "" && !ValidAddr(msg.StreamAddr)) {
		return
	}

	now := time.Now()
	var shouldReply, isNew, isUpdated bool

	d.mu.Lock()
	p, ok := d.peers[msg.ID]
	if !ok {
		p = &Peer{
			ID:         msg.ID,
			Addr:       msg.Addr,
			SyncAddr:   msg.SyncAddr,
			StreamAddr: msg.StreamAddr,
			LastSeen:   now,
		}
		d.peers[msg.ID] = p
		isNew = true
		shouldReply = true
	} else {
		isUpdated = p.Addr != msg.Addr || p.SyncAddr != msg.SyncAddr || p.StreamAddr != msg.StreamAddr
		p.Addr = msg.Addr
		p.SyncAddr = msg.SyncAddr
		p.StreamAddr = msg.StreamAddr
		p.LastSeen = now
		if isUpdated {
			shouldReply = true
		}
	}
	d.mu.Unlock()

	if isNew {
		log.Printf("[DISCOVERY:PEER_NEW] id=%s addr=%s sync=%s stream=%s", msg.ID, msg.Addr, msg.SyncAddr, msg.StreamAddr)
	} else if isUpdated {
		log.Printf("[DISCOVERY:PEER_UPDATE] id=%s addr=%s sync=%s stream=%s", msg.ID, msg.Addr, msg.SyncAddr, msg.StreamAddr)
	}

	if !shouldReply || msg.Addr == "" {
		return
	}

	// Rate limit reactive replies: at most 1 reactive reply per peer per second
	d.replyMu.Lock()
	if last, exists := d.lastReplied[msg.ID]; exists && now.Sub(last) < time.Second {
		d.replyMu.Unlock()
		return
	}
	d.lastReplied[msg.ID] = now
	d.replyMu.Unlock()

	// Guard against broadcast storms: bound maximum concurrent reactive reply goroutines
	select {
	case d.reactiveSem <- struct{}{}:
	default:
		// Capacity exceeded; drop reactive reply and rely on periodic announce ticker.
		return
	}

	d.closeMu.RLock()
	if d.isStopping() {
		d.closeMu.RUnlock()
		<-d.reactiveSem
		return
	}
	d.wg.Add(1)
	d.closeMu.RUnlock()

	jitter := randomJitter(5*time.Millisecond, 50*time.Millisecond)

	go func(targetAddr string) {
		defer func() {
			<-d.reactiveSem
			d.wg.Done()
		}()

		t := time.NewTimer(jitter)
		defer t.Stop()

		select {
		case <-t.C:
		case <-d.stopCh:
			return
		}
		d.sendReactiveAnnounce(targetAddr)
	}(msg.Addr)
}

func (d *Discovery) sendReactiveAnnounce(targetAddr string) {
	if d.isStopping() || targetAddr == "" {
		return
	}
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
	d.mu.RUnlock()

	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	d.sendUnicast(targetAddr, payload)
}

func randomJitter(min, max time.Duration) time.Duration {
	diff := max - min
	if diff <= 0 {
		return min
	}
	return min + randv2.N(diff)
}

// prune removes peers that have not been refreshed within PeerTTL.
func (d *Discovery) prune() {
	cutoff := time.Now().Add(-d.cfg.PeerTTL)
	type expiredPeer struct {
		id       string
		lastSeen time.Time
	}
	var expired []expiredPeer

	d.mu.Lock()
	for id, p := range d.peers {
		if p.LastSeen.Before(cutoff) {
			expired = append(expired, expiredPeer{id: id, lastSeen: p.LastSeen})
			delete(d.peers, id)
		}
	}
	d.mu.Unlock()

	for _, ep := range expired {
		log.Printf("[DISCOVERY:PEER_EXPIRED] id=%s last_seen=%v_ago", ep.id, time.Since(ep.lastSeen))
	}

	d.replyMu.Lock()
	for id, t := range d.lastReplied {
		if t.Before(cutoff) {
			delete(d.lastReplied, id)
		}
	}
	d.replyMu.Unlock()
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
		d.closeMu.Lock()
		close(d.stopCh)
		d.closeMu.Unlock()

		d.closeSockets()
		d.wg.Wait()
	})
}

func (d *Discovery) closeSockets() {
	d.sockMu.Lock()
	conns := []*net.UDPConn{d.annSend, d.control, d.mcast}
	d.annSend = nil
	d.annSendPConn = nil
	d.control = nil
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
