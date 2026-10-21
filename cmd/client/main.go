package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api"
	apiTelemetry "github.com/audio-router-p2p/pkg/api/telemetry"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/group"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
	"github.com/audio-router-p2p/pkg/telemetry"
	"github.com/audio-router-p2p/pkg/transport"
	"strconv"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load error: %v", err)
	}

	// Discovery + clock sync run first: the DTLS client transport needs the
	// peer's stream address, which comes from the route table / discovery.
	disc, err := discovery.New(cfg, "")
	if err != nil {
		log.Fatalf("discovery: %v", err)
	}
	go disc.Run()
	log.Printf("discovery started for node %s (advertised %s)", disc.NodeID(), disc.OwnAddr())

	// Routing table is loaded before the transport so encrypt mode can resolve
	// the (single) DTLS peer address.
	rt, err := router.New(cfg.RoutesFile)
	if err != nil {
		log.Fatalf("router init error: %v", err)
	}

	// Shared audio transport socket (UDP or DTLS). In encrypt mode the DAILY
	// DTLS/SRTP transport supports a single remote peer; derive it from the
	// route table and fail fast unless exactly one distinct peer is used.
	var trans streamer.Transport
	if cfg.Encrypt {
		remote := singleDTLSPeer(routesOf(rt, disc))
		if remote == "" {
			log.Fatal("encrypt mode requires exactly one distinct DTLS peer; " +
				"set a single route with a peerAddr (or a single peerId that " +
				"has been discovered) in " + cfg.RoutesFile)
		}

		// Parse PSK if provided
		var psk []byte
		if cfg.PSK != "" {
			psk, err = base64.StdEncoding.DecodeString(cfg.PSK)
			if err != nil {
				log.Fatalf("invalid PSK: %v", err)
			}
		}

		dtlsCfg := transport.DTLSConfig{
			ListenAddr: cfg.StreamPort,
			RemoteAddr: remote,
			CertFile:   cfg.CertFile,
			KeyFile:    cfg.KeyFile,
			PSK:        psk,
			IsServer:   cfg.DTLSRole != "client", // the other node must use the opposite role
		}
		trans, err = transport.NewDTLSTransport(dtlsCfg)
		if err != nil {
			log.Fatalf("DTLS transport: %v", err)
		}
		log.Printf("audio DTLS/SRTP transport listening on %s", trans.Addr())
	} else {
		udp, err := transport.NewUDPTransport(cfg.StreamPort)
		if err != nil {
			log.Fatalf("audio transport: %v", err)
		}
		trans = udp
		log.Printf("audio UDP transport listening on %s", trans.Addr())
	}

	// Clock sync.
	sm := syncmgr.NewManager(cfg, disc)
	go sm.Run()

	localIPs := collectLocalIPs(trans, disc)
	streams := newStreamManager(cfg, trans, localIPs, disc)
	for _, route := range rt.List() {
		if err := streams.StartRoute(route); err != nil {
			log.Printf("route %s: %v", route.ID, err)
		}
	}

	// Room Manager
	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streams)
	if cfg.RoomAutoJoin || cfg.RoomID != "" {
		roomID := cfg.RoomID
		if roomID == "" {
			roomID = "default"
		}
		roomName := cfg.RoomName
		if roomName == "" {
			roomName = "Default Room"
		}
		if _, err := roomMgr.CreateRoom(roomID, roomName); err != nil {
			log.Printf("auto-create room %s: %v", roomID, err)
		}
		if err := roomMgr.JoinRoom(roomID); err != nil {
			log.Printf("auto-join room %s: %v", roomID, err)
		} else {
			log.Printf("auto-joined room %s (%s)", roomID, roomName)
		}
	}

	// HTTP API.
	apiSrv := api.NewServer(cfg, rt, disc, sm, streams, roomMgr)
	streams.SetHub(apiSrv.Hub())
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: apiSrv.Handler()}

	if agg := apiSrv.Aggregator(); agg != nil {
		agg.Start(context.Background())
		defer agg.Stop()
	}

	if cm := apiSrv.ClusterManager(); cm != nil {
		cm.Start(context.Background())
		defer cm.Stop()
	}

	if cfg.NoTLS {
		go func() {
			log.Printf("API listening on http://%s (plaintext HTTP)", cfg.ListenAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("API server error: %v", err)
			}
		}()
	} else {
		if err := ensureTLS(cfg); err != nil {
			log.Fatalf("TLS: %v", err)
		}
		go func() {
			log.Printf("API listening on https://%s (TLS enabled - self-signed certificate)", cfg.ListenAddr)
			if err := srv.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("API server error: %v", err)
			}
		}()
	}

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}

	if agg := apiSrv.Aggregator(); agg != nil {
		agg.Stop()
	}

	if cm := apiSrv.ClusterManager(); cm != nil {
		cm.Stop()
	}
	_ = apiSrv.Close()

	_ = roomMgr.Close()
	streams.StopAll()
	disc.Close()
	sm.Close()
	_ = trans.Close()
	log.Println("Shutdown complete")
}

// collectLocalIPs gathers every IP literal that belongs to this node so the
// streamer can bypass the network for local destinations.
func collectLocalIPs(trans streamer.Transport, disc *discovery.Discovery) []string {
	set := make(map[string]bool)
	if la := trans.LocalAddr(); la != nil {
		set[la.IP.String()] = true
	}
	if own := disc.OwnAddr(); own != "" {
		if host, _, err := net.SplitHostPort(own); err == nil && host != "" {
			set[host] = true
		}
	}
	set["127.0.0.1"] = true
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				set[ipn.IP.String()] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	return out
}

func destIsLocal(hostport string, localIPs []string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, l := range localIPs {
		if l == ip.String() || l == host {
			return true
		}
	}
	return false
}

func sameTransportPort(hostport string, trans streamer.Transport) bool {
	if trans == nil || trans.LocalAddr() == nil {
		return false
	}
	_, port, err := net.SplitHostPort(hostport)
	return err == nil && port == strconv.Itoa(trans.LocalAddr().Port)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// streamManager owns the live streamers that back the routing table.
type streamManager struct {
	cfg      *config.Config
	trans    streamer.Transport
	localIPs []string
	disc     *discovery.Discovery
	resolver *audio.DeviceResolver
	hub      *apiTelemetry.TelemetryHub
	outputs  map[string]*outputBus

	mu     sync.Mutex
	routes map[string]*activeStream
}

type activeStream struct {
	route     router.StreamRoute
	st        *streamer.Streamer
	in        audio.AudioStream
	out       audio.AudioStream
	matrix    *routeMatrix
	outputKey string
	watchStop chan struct{}
	watchDone chan struct{}
}

func newStreamManager(cfg *config.Config, trans streamer.Transport, localIPs []string, disc *discovery.Discovery) *streamManager {
	return &streamManager{
		cfg:      cfg,
		trans:    trans,
		localIPs: localIPs,
		disc:     disc,
		resolver: audio.GlobalResolver(),
		routes:   make(map[string]*activeStream),
		outputs:  make(map[string]*outputBus),
	}
}

func (m *streamManager) SetHub(hub *apiTelemetry.TelemetryHub) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hub = hub
}

// StartRoute opens the source device and begins streaming per the route.
func (m *streamManager) StartRoute(route router.StreamRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sr := route
	if sr.Direction != "" && sr.Direction != "send" && sr.Direction != "receive" {
		return fmt.Errorf("route %s: unsupported direction %q", sr.ID, sr.Direction)
	}
	if sr.ID == "" {
		return errors.New("route ID is required")
	}
	if sr.SampleRate <= 0 {
		sr.SampleRate = 44100
	}
	if sr.Channels < 1 {
		sr.Channels = 1
	}
	if sr.InChannels != 0 && sr.InChannels != sr.Channels || sr.OutChannels != 0 && sr.OutChannels != sr.Channels {
		return fmt.Errorf("route %s: channel-count conversion is not supported by live PCM routes", sr.ID)
	}
	if len(sr.Coefficients) != 0 {
		mr := router.StreamRouteToMatrixRoute(sr)
		if err := mr.Validate(); err != nil {
			return err
		}
	}
	if old, ok := m.routes[sr.ID]; ok {
		m.stopLocked(old)
		delete(m.routes, sr.ID)
	}

	srcRef := sr.SourceDeviceURN
	if srcRef == "" {
		srcRef = strconv.Itoa(sr.SourceDevice)
	}

	var in audio.AudioStream
	var err error
	if sr.Direction != "receive" {
		in, err = m.resolver.OpenInput(srcRef, sr.SampleRate, sr.Channels, m.cfg.UseEmulator)
		if err != nil {
			return fmt.Errorf("open input device %s: %w", srcRef, err)
		}
	}

	var out audio.AudioStream
	destAddrs := append([]string(nil), sr.Destinations...)
	// The matrix API uses a legacy loopback placeholder for local endpoints.
	// Resolve it to this daemon's actual stream socket (which may use -stream-port
	// or port 0), without treating a different loopback daemon as local.
	if sr.PeerID == "local" && sr.Direction != "receive" && m.trans != nil {
		for i, dest := range destAddrs {
			if dest == "127.0.0.1:45678" {
				local := m.trans.LocalAddr()
				if local != nil {
					destAddrs[i] = net.JoinHostPort("127.0.0.1", strconv.Itoa(local.Port))
				}
			}
		}
	}

	// If route has a peer ID, try to bind to that peer
	if sr.Direction != "receive" && sr.PeerID != "" && sr.PeerID != "local" && m.disc != nil {
		// A peer-bound UDP route follows discovery, not a potentially stale
		// persisted address. Do not send to the old endpoint while it is gone.
		if _, ok := m.trans.(*transport.UDPTransport); ok {
			destAddrs = nil
			sr.PeerAddr = ""
			if peer := m.disc.PeerByID(sr.PeerID); peer != nil && peer.StreamAddr != "" {
				destAddrs = []string{peer.StreamAddr}
				sr.PeerAddr = peer.StreamAddr
			}
		} else if peer := m.disc.PeerByID(sr.PeerID); peer != nil && peer.StreamAddr != "" {
			if !contains(destAddrs, peer.StreamAddr) {
				destAddrs = append(destAddrs, peer.StreamAddr)
			}
			sr.PeerAddr = peer.StreamAddr
		}
	} else if sr.Direction != "receive" && len(destAddrs) > 0 {
		// Try to match destinations to discovered peers
		for i, dest := range destAddrs {
			if peer := m.disc.PeerByAddr(dest); peer != nil && peer.StreamAddr != "" {
				destAddrs[i] = peer.StreamAddr
				if sr.PeerID == "" {
					sr.PeerID = peer.ID
				}
			}
		}
	}

	if sr.Direction == "receive" {
		destAddrs = nil
	}
	openOutput := sr.Direction == "receive"
	if !openOutput {
		for _, d := range destAddrs {
			if destIsLocal(d, m.localIPs) && sameTransportPort(d, m.trans) {
				openOutput = true
				break
			}
		}
	}
	streamCfg, err := streamer.NetworkPCMConfig(sr.SampleRate, sr.Channels)
	if err != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
		}
		return err
	}
	var outputKey string
	if openOutput {
		dstRef := sr.DestDeviceURN
		if dstRef == "" {
			dstRef = strconv.Itoa(sr.DestDevice)
		}
		key := dstRef
		if dev, found := m.resolver.Resolve(dstRef); found {
			key = dev.URN
		}
		outputKey = key
		bus := m.outputs[key]
		if bus != nil && (bus.rate != sr.SampleRate || bus.ch != sr.Channels || bus.period != time.Duration(streamCfg.PacketTime)*time.Millisecond) {
			if in != nil {
				_ = in.Close()
			}
			return fmt.Errorf("output %s already in use with a different PCM format", dstRef)
		}
		if bus == nil {
			device, openErr := m.resolver.OpenOutput(dstRef, sr.SampleRate, sr.Channels, m.cfg.UseEmulator)
			if openErr != nil {
				if in != nil {
					_ = in.Close()
				}
				return fmt.Errorf("open local output device %s: %w", dstRef, openErr)
			}
			bus = newOutputBus(device, sr.SampleRate, sr.Channels, streamCfg.PacketTime)
			m.outputs[key] = bus
		}
		out = bus.add(sr.ID)
	}
	var matrix *routeMatrix
	if sr.Direction == "receive" {
		matrix, err = newRouteMatrix(out, sr)
		if err == nil {
			out = matrix
		}
	} else {
		matrix, err = newRouteMatrix(in, sr)
		if err == nil {
			in = matrix
		}
	}
	if err != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
			m.removeEmptyOutputLocked(outputKey)
		}
		return err
	}
	st, err := streamer.NewStreamer(m.trans, m.localIPs, streamCfg, m.disc.NodeID())
	if err != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
			m.removeEmptyOutputLocked(outputKey)
		}
		return err
	}
	// Both endpoints of a route must use the same ID to demultiplex packets
	// on the shared transport. Keep the ID stable across route restarts while
	// separating routes (and deployments with different auth tokens).
	st.SetStreamID(streamer.DeriveStreamID(m.cfg.AuthToken, "route:"+sr.ID))
	if err := st.Start(in, destAddrs, out); err != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
			m.removeEmptyOutputLocked(outputKey)
		}
		return err
	}
	if sr.PeerID != "" {
		for _, dest := range destAddrs {
			st.SetPeerID(dest, sr.PeerID)
		}
	}

	if m.hub != nil {
		tap := m.hub.RegisterTap(sr.ID, sr.Channels)
		st.SetAudioTap(tap)
	}

	// Save the potentially updated route (with peer binding)
	as := &activeStream{route: sr, st: st, in: in, out: out, matrix: matrix, outputKey: outputKey}
	m.routes[sr.ID] = as
	if sr.Direction != "receive" && sr.PeerID != "" && sr.PeerID != "local" && out == nil && m.disc != nil {
		if _, ok := m.trans.(*transport.UDPTransport); ok {
			as.watchStop = make(chan struct{})
			as.watchDone = make(chan struct{})
			go m.followPeer(as, as.watchStop, as.watchDone)
		}
	}
	log.Printf("route %s: streaming device %s -> %d destinations (peer: %s)", sr.ID, srcRef, len(destAddrs), sr.PeerID)
	return nil
}

// UpdateMatrix adjusts an active route without restarting capture or transport.
func (m *streamManager) UpdateMatrix(route router.MatrixRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	as, ok := m.routes[route.ID]
	if !ok || as.matrix == nil {
		return fmt.Errorf("route %s is not active", route.ID)
	}
	return as.matrix.Update(route)
}

// StopRoute stops and releases a route's streams.
func (m *streamManager) StopRoute(routeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	as, ok := m.routes[routeID]
	if !ok {
		return nil
	}
	delete(m.routes, routeID)
	m.stopLocked(as)
	return nil
}

// StopAll stops every active stream.
func (m *streamManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, as := range m.routes {
		delete(m.routes, id)
		m.stopLocked(as)
	}
}

// Stats returns statistics for all active streams.
func (m *streamManager) Stats() map[string]streamer.StreamerStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make(map[string]streamer.StreamerStats, len(m.routes))
	for id, as := range m.routes {
		if as.st != nil {
			res[id] = as.st.Stats()
		}
	}
	return res
}

// StreamLinkStats returns atomic link metrics across all active streams.
func (m *streamManager) StreamLinkStats() []telemetry.StreamLinkStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []telemetry.StreamLinkStats
	for _, as := range m.routes {
		if as.st != nil {
			out = append(out, as.st.StreamLinkStats()...)
		}
	}
	return out
}

func (m *streamManager) stopLocked(as *activeStream) {
	if as.watchStop != nil {
		close(as.watchStop)
		<-as.watchDone
	}
	if m.hub != nil {
		m.hub.UnregisterTap(as.route.ID)
	}
	as.st.Stop()
	done := make(chan struct{})
	go func() {
		_ = as.st.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("route %s: stream did not stop in time", as.route.ID)
	}
	if as.in != nil {
		_ = as.in.Close()
	}
	if as.out != nil {
		_ = as.out.Close()
		m.removeEmptyOutputLocked(as.outputKey)
	}
}

// Discovery polling is deliberately outside the manager lock: a slow
// resolution must not stall API updates or route shutdown. The streamer
// serializes destination swaps with the send loop.
func (m *streamManager) followPeer(as *activeStream, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	current := as.route.PeerAddr
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		addr := ""
		if peer := m.disc.PeerByID(as.route.PeerID); peer != nil {
			addr = peer.StreamAddr
		}
		if addr == current {
			continue
		}
		if err := as.st.RebindPeer(as.route.PeerID, addr); err != nil {
			log.Printf("route %s: peer rebind: %v", as.route.ID, err)
			continue
		}
		log.Printf("route %s: peer %s stream endpoint %q -> %q", as.route.ID, as.route.PeerID, current, addr)
		current = addr
	}
}

// Caller holds the stream manager lock; only this manager adds/removes slots.
func (m *streamManager) removeEmptyOutputLocked(key string) {
	if bus := m.outputs[key]; bus != nil {
		bus.mu.Lock()
		closed := bus.closed
		bus.mu.Unlock()
		if closed {
			delete(m.outputs, key)
		}
	}
}

// ensureTLS generates a self-signed certificate when no cert/key pair exists,
// so the binary runs out of the box.
func ensureTLS(cfg *config.Config) error {
	if fileExists(cfg.CertFile) && fileExists(cfg.KeyFile) {
		return nil
	}
	if fileExists(cfg.CertFile) != fileExists(cfg.KeyFile) {
		return errors.New("only one of cert/key exists")
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	// Collect all local IPs for SAN
	var ips []net.IP
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip := ipn.IP.To4(); ip != nil {
					ips = append(ips, ip)
				} else if ip := ipn.IP.To16(); ip != nil {
					ips = append(ips, ip)
				}
			}
		}
	}
	// Ensure localhost is included
	ips = append(ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "audio-router-p2p", Organization: []string{"audio-router-p2p"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(filepath.Dir(cfg.CertFile), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.CertFile, certPEM, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.KeyFile, keyPEM, 0600); err != nil {
		return err
	}
	log.Printf("generated self-signed TLS certificate %s / %s", cfg.CertFile, cfg.KeyFile)
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
