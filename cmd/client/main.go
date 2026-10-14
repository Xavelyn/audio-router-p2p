package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
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
	"strconv"
	"strings"
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
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load error: %v", err)
	}

	// Discovery + clock sync run first: the DTLS client transport needs the
	// peer's stream address, which comes from the route table / discovery.
	nodeID, err := discovery.ResolveOrPersistNodeID(cfg)
	if err != nil {
		log.Fatalf("failed to resolve node identity: %v", err)
	}
	disc, err := discovery.New(cfg, nodeID)
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
			IsServer:   true, // server role: accept the peer's DTLS connection
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

	// Room Manager
	roomMgr := group.NewDefaultRoomManager(cfg, disc, sm, trans)
	roomMgr.SetRouter(rt)
	roomMgr.SetStreamManager(streams)
	devResolver := audio.GlobalResolver()
	roomMgr.SetDeviceResolver(devResolver)
	disc.SetStateDelegate(roomMgr)

	// HTTP API.
	apiSrv := api.NewServer(cfg, rt, disc, sm, streams, roomMgr)
	streams.SetHub(apiSrv.Hub())
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: apiSrv.Handler()}

	// Audio artifact telemetry aggregator (ADR 0034): 1 s sampling over a
	// 10 s window. The stream manager provides both sources (route-scoped
	// artifact counters and per-device XRun state); both it and the resolver
	// are fully wired at this point.
	artifactAgg := telemetry.NewArtifactAggregator(streams, streams, time.Second, 10*time.Second)
	apiSrv.SetArtifactAggregator(artifactAgg)

	if hub := apiSrv.Hub(); hub != nil {
		hub.Start(context.Background())
		defer hub.Stop()
	}

	if agg := apiSrv.Aggregator(); agg != nil {
		agg.Start(context.Background())
		defer agg.Stop()
	}

	artifactAgg.Start(context.Background())
	defer artifactAgg.Stop()

	if cm := apiSrv.ClusterManager(); cm != nil {
		roomMgr.SetClusterManager(cm)
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

	// Asynchronous route activation and room synchronization in background goroutine.
	// This ensures the HTTP server and management Web UI are immediately reachable
	// even if physical audio soundcards or drivers hang during device opening.
	bootCtx, cancelBoot := context.WithCancel(context.Background())
	defer cancelBoot()

	go func() {
		log.Printf("Starting asynchronous route and room initialization...")
		for _, route := range rt.List() {
			if bootCtx.Err() != nil {
				return
			}
			if err := streams.StartRoute(route); err != nil {
				log.Printf("route %s: %v", route.ID, err)
			}
		}

		if bootCtx.Err() != nil {
			return
		}

		if err := roomMgr.Load(cfg.RoomsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("load rooms from %s: %v", cfg.RoomsFile, err)
		}
		if bootCtx.Err() != nil {
			return
		}
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
			if bootCtx.Err() != nil {
				return
			}
			if err := roomMgr.JoinRoom(roomID); err != nil {
				log.Printf("auto-join room %s: %v", roomID, err)
			} else {
				log.Printf("auto-joined room %s (%s)", roomID, roomName)
			}
		}
		log.Printf("Asynchronous route and room initialization complete")
	}()

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	cancelBoot()
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}

	if hub := apiSrv.Hub(); hub != nil {
		hub.Stop()
	}

	if agg := apiSrv.Aggregator(); agg != nil {
		agg.Stop()
	}

	artifactAgg.Stop()

	if cm := apiSrv.ClusterManager(); cm != nil {
		cm.Stop()
	}
	_ = apiSrv.Close()

	_ = roomMgr.Close()
	streams.StopAll()
	disc.Close()
	sm.Close()
	_ = trans.Close()
	_ = audio.GlobalResolver().Close()
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
	if hostport == "local" || hostport == "localhost" {
		return true
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "local" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, l := range localIPs {
		if l == ip.String() || l == host {
			return true
		}
	}
	return false
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

	mu     sync.Mutex
	closed bool
	routes map[string]*activeStream
}

type activeStream struct {
	route  router.StreamRoute
	st     *streamer.Streamer
	in     audio.AudioStream
	out    audio.AudioStream
	cancel context.CancelFunc
}

func newStreamManager(cfg *config.Config, trans streamer.Transport, localIPs []string, disc *discovery.Discovery) *streamManager {
	return &streamManager{
		cfg:      cfg,
		trans:    trans,
		localIPs: localIPs,
		disc:     disc,
		resolver: audio.GlobalResolver(),
		routes:   make(map[string]*activeStream),
	}
}

func (m *streamManager) SetHub(hub *apiTelemetry.TelemetryHub) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hub = hub
	log.Printf("[STREAM:MGR] TelemetryHub attached; binding taps retroactively...")
	for id, as := range m.routes {
		if as.st != nil {
			tapID := id
			if as.in == nil && as.out != nil {
				tapID = id + ":out"
			}
			tap := m.hub.RegisterTap(tapID, as.route.Channels)
			as.st.SetAudioTap(tap)
			log.Printf("[STREAM:MGR] [AUDIO:METER] retroactively attached tap to route=%s (%d channels)", tapID, as.route.Channels)
		}
	}
}

func (m *streamManager) SetResolver(res *audio.DeviceResolver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolver = res
}

// StartRoute opens the source device and begins streaming per the route.
func (m *streamManager) StartRoute(route router.StreamRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(route)
}

func isRouteConfigIdentical(oldRoute, newRoute router.StreamRoute, isRemoteSource, isLocalDest bool, disc *discovery.Discovery, defaultPort string) bool {
	if oldRoute.ID != newRoute.ID {
		return false
	}
	if oldRoute.SourceNodeID != newRoute.SourceNodeID {
		return false
	}
	if oldRoute.PeerID != newRoute.PeerID {
		return false
	}

	// Compare source device
	if newRoute.SourceDeviceURN != "" || oldRoute.SourceDeviceURN != "" {
		if newRoute.SourceDeviceURN != oldRoute.SourceDeviceURN {
			return false
		}
	} else if newRoute.SourceDevice != oldRoute.SourceDevice {
		return false
	}

	// Compare destination device
	if newRoute.DestDeviceURN != "" || oldRoute.DestDeviceURN != "" {
		if newRoute.DestDeviceURN != oldRoute.DestDeviceURN {
			return false
		}
	} else if newRoute.DestDevice != oldRoute.DestDevice {
		return false
	}

	// Compare audio formatting
	if newRoute.Channels > 0 && oldRoute.Channels > 0 && newRoute.Channels != oldRoute.Channels {
		return false
	}
	if newRoute.OutChannels > 0 && oldRoute.OutChannels > 0 && newRoute.OutChannels != oldRoute.OutChannels {
		return false
	}
	if newRoute.SampleRate > 0 && oldRoute.SampleRate > 0 && newRoute.SampleRate != oldRoute.SampleRate {
		return false
	}

	// If this is an ingress route, destination is local playback on this node.
	if isRemoteSource && isLocalDest {
		return true
	}

	// Egress / remote route: check peer address resolution
	if newRoute.PeerID != "" && newRoute.PeerID != "local" {
		if disc != nil {
			resolved, err := streamer.ResolvePeerDestination(newRoute, disc, defaultPort)
			if err == nil && resolved != "" {
				if oldRoute.PeerAddr == resolved {
					return true
				}
				for _, d := range oldRoute.Destinations {
					if d == resolved {
						return true
					}
				}
				return false
			}
		}
		if len(newRoute.Destinations) == 0 {
			return true
		}
	}

	// Compare explicit destinations if provided
	if len(newRoute.Destinations) > 0 {
		if len(newRoute.Destinations) != len(oldRoute.Destinations) {
			return false
		}
		for i := range newRoute.Destinations {
			if newRoute.Destinations[i] != oldRoute.Destinations[i] {
				return false
			}
		}
		return true
	}

	// Purely local loop routes where newRoute.Destinations is empty
	if isLocalDest && !isRemoteSource {
		return true
	}

	return len(oldRoute.Destinations) == 0
}

func (m *streamManager) startLocked(route router.StreamRoute) error {
	if m.closed {
		return errors.New("stream manager closed")
	}
	sr := route
	if sr.SampleRate <= 0 {
		sr.SampleRate = 44100
	}
	if sr.Channels < 1 {
		sr.Channels = 1
	}
	log.Printf("[STREAM:MGR] StartRoute called: routeID=%s srcNode=%q srcURN=%q dstNode=%q dstURN=%q peerID=%q dests=%v sr=%d ch=%d",
		sr.ID, sr.SourceNodeID, sr.SourceDeviceURN, sr.PeerID, sr.DestDeviceURN, sr.PeerID, sr.Destinations, sr.SampleRate, sr.Channels)

	localNodeID := ""
	if m.disc != nil {
		localNodeID = m.disc.NodeID()
	}

	isRemoteSource := sr.SourceNodeID != "" && sr.SourceNodeID != "local" && (localNodeID == "" || sr.SourceNodeID != localNodeID)
	isLocalDest := sr.PeerID == "" || sr.PeerID == "local" || (localNodeID != "" && sr.PeerID == localNodeID)

	if old, ok := m.routes[sr.ID]; ok {
		if old.st != nil && isRouteConfigIdentical(old.route, sr, isRemoteSource, isLocalDest, m.disc, m.cfg.StreamPort) {
			log.Printf("[STREAM:MGR] Preserving active stream for unchanged route=%s", sr.ID)
			return nil
		}
		log.Printf("[STREAM:MGR] Stopping pre-existing stream for route=%s", sr.ID)
		delete(m.routes, sr.ID)
		m.stopActiveStream(old)
	}

	tokenHash := sha256.Sum256([]byte(m.cfg.AuthToken))
	authFp := fmt.Sprintf("%x", tokenHash[:4])

	// Ingress route: remote audio source sending to our local output device.
	if isRemoteSource && isLocalDest {
		dstRef := sr.DestDeviceURN
		if dstRef == "" {
			dstRef = strconv.Itoa(sr.DestDevice)
		}
		chans := sr.OutChannels
		if chans <= 0 {
			chans = sr.Channels
		}
		if chans <= 0 {
			chans = 2
		}
		sampleRate := sr.SampleRate
		if sampleRate <= 0 {
			sampleRate = 48000
		}
		if m.resolver != nil {
			if dev, ok := m.resolver.Resolve(dstRef); ok && dev.DefaultSampleRate > 0 {
				sampleRate = int(dev.DefaultSampleRate)
			}
		}
		log.Printf("[STREAM:MGR] Opening local output device ref=%q for ingress route=%s from sourceNode=%s (sampleRate=%d, channels=%d)", dstRef, sr.ID, sr.SourceNodeID, sampleRate, chans)
		out := openOutputWithTimeout(m.resolver, dstRef, sampleRate, chans, m.cfg.UseEmulator, 2*time.Second)

		streamCfg := streamer.DefaultStreamConfig(sampleRate, chans)
		st, err := streamer.NewStreamer(m.trans, m.localIPs, streamCfg, localNodeID)
		if err != nil {
			if out != nil {
				_ = out.Close()
			}
			log.Printf("[STREAM:MGR] ERROR creating Streamer for ingress route=%s: %v", sr.ID, err)
			return err
		}

		// Bind deterministic StreamID derived from auth token and route ID so incoming packets match
		streamID := streamer.DeriveStreamID(m.cfg.AuthToken, sr.ID)
		st.SetStreamID(streamID)
		log.Printf("[ROUTE:DEST] ingress route=%s expecting streamID=%x authFp=%s sourceNode=%s -> local output=%s (sr=%d, ch=%d)",
			sr.ID, streamID, authFp, sr.SourceNodeID, dstRef, sampleRate, chans)

		if err := st.Start(nil, nil, out); err != nil {
			if out != nil {
				_ = out.Close()
			}
			log.Printf("[STREAM:MGR] ERROR starting Streamer for ingress route=%s: %v", sr.ID, err)
			return err
		}

		if m.hub != nil {
			tap := m.hub.RegisterTap(sr.ID+":out", chans)
			st.SetAudioTap(tap)
			log.Printf("[STREAM:MGR] [AUDIO:METER] attached tap to ingress route=%s (%d channels)", sr.ID+":out", chans)
		}

		ctx, cancel := context.WithCancel(context.Background())
		_ = ctx
		m.routes[sr.ID] = &activeStream{route: sr, st: st, in: nil, out: out, cancel: cancel}
		log.Printf("[STREAM:MGR] Ingress route %s active: receiving from node %s -> local output %s (%d Hz, %d ch)", sr.ID, sr.SourceNodeID, dstRef, sampleRate, chans)
		return nil
	}

	// Remote route between other nodes in cluster: neither source nor destination is this node.
	if isRemoteSource && !isLocalDest {
		log.Printf("[STREAM:MGR] Route %s is between remote peers (src=%s, dst=%s); ignoring locally", sr.ID, sr.SourceNodeID, sr.PeerID)
		return nil
	}

	srcRef := sr.SourceDeviceURN
	if srcRef == "" {
		srcRef = strconv.Itoa(sr.SourceDevice)
	}

	srcRate := sr.SampleRate
	if srcRate <= 0 {
		srcRate = 48000
	}
	srcChans := sr.Channels
	if srcChans <= 0 {
		srcChans = 2
	}
	if m.resolver != nil {
		if dev, ok := m.resolver.Resolve(srcRef); ok {
			if dev.DefaultSampleRate > 0 {
				srcRate = int(dev.DefaultSampleRate)
			}
			if dev.MaxInput > 0 && dev.MaxInput < srcChans {
				srcChans = dev.MaxInput
			}
		}
	}

	in := openInputWithTimeout(m.resolver, srcRef, srcRate, srcChans, m.cfg.UseEmulator, 2*time.Second)

	var out audio.AudioStream
	var destAddrs []string

	// If route targets a remote peer, resolve peer's stream address (host:port)
	if sr.PeerID != "" && sr.PeerID != "local" && (localNodeID == "" || sr.PeerID != localNodeID) {
		resolved, err := streamer.ResolvePeerDestination(sr, m.disc, m.cfg.StreamPort)
		if err == nil && resolved != "" {
			destAddrs = append(destAddrs, resolved)
			sr.PeerAddr = resolved
			log.Printf("[ROUTE:RESOLVE] route=%s peer=%s resolved_addr=%s (defaultPort=%s)",
				sr.ID, sr.PeerID, resolved, m.cfg.StreamPort)
		} else {
			log.Printf("[STREAM:MGR] WARN: could not resolve peer %s: %v", sr.PeerID, err)
		}
	}

	// Normalize any additional configured destinations, ensuring valid host:port
	for _, d := range sr.Destinations {
		if d == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(d); err == nil {
			if !contains(destAddrs, d) {
				destAddrs = append(destAddrs, d)
			}
			continue
		}
		// Try resolving through discovery if it matches a peer ID or peer Addr
		if m.disc != nil {
			if p := m.disc.PeerByID(d); p != nil {
				if p.StreamAddr != "" {
					if _, _, err := net.SplitHostPort(p.StreamAddr); err == nil {
						if !contains(destAddrs, p.StreamAddr) {
							destAddrs = append(destAddrs, p.StreamAddr)
						}
						continue
					}
				}
				host, _, _ := net.SplitHostPort(p.Addr)
				if host == "" {
					host = p.Addr
				}
				if host != "" {
					port := strings.TrimPrefix(m.cfg.StreamPort, ":")
					if port == "" {
						port = "45678"
					}
					joined := net.JoinHostPort(host, port)
					if !contains(destAddrs, joined) {
						destAddrs = append(destAddrs, joined)
					}
					continue
				}
			}
		}
		// Bare host without port
		port := strings.TrimPrefix(m.cfg.StreamPort, ":")
		if port == "" {
			port = "45678"
		}
		joined := net.JoinHostPort(d, port)
		if !contains(destAddrs, joined) {
			destAddrs = append(destAddrs, joined)
		}
	}

	hasLocalDest := false
	if isLocalDest && (sr.DestDeviceURN != "" || sr.DestDevice >= 0) {
		hasLocalDest = true
	} else if sr.PeerID == "" || sr.PeerID == "local" {
		for _, d := range destAddrs {
			if destIsLocal(d, m.localIPs) {
				hasLocalDest = true
				break
			}
		}
	}

	if hasLocalDest && (sr.DestDeviceURN != "" || sr.DestDevice >= 0) {
		dstRef := sr.DestDeviceURN
		if dstRef == "" {
			dstRef = strconv.Itoa(sr.DestDevice)
		}
		log.Printf("[STREAM:MGR] Opening local output device ref=%q for route=%s", dstRef, sr.ID)
		out = openOutputWithTimeout(m.resolver, dstRef, srcRate, srcChans, m.cfg.UseEmulator, 2*time.Second)

		if len(destAddrs) == 0 {
			destAddrs = []string{"127.0.0.1:45678"}
		}
	}

	streamCfg := streamer.DefaultStreamConfig(srcRate, srcChans)
	st, err := streamer.NewStreamer(m.trans, m.localIPs, streamCfg, localNodeID)
	if err != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
		}
		log.Printf("[STREAM:MGR] ERROR creating Streamer for route=%s: %v", sr.ID, err)
		return err
	}
	streamID := streamer.DeriveStreamID(m.cfg.AuthToken, sr.ID)
	st.SetStreamID(streamID)
	if len(destAddrs) == 0 {
		log.Printf("[ROUTE:DEST] route=%s dest=none streamID=%x authFp=%s sr=%d ch=%d",
			sr.ID, streamID, authFp, srcRate, srcChans)
	} else {
		for _, dest := range destAddrs {
			log.Printf("[ROUTE:DEST] route=%s dest=%s streamID=%x authFp=%s sr=%d ch=%d",
				sr.ID, dest, streamID, authFp, srcRate, srcChans)
		}
	}
	if err := st.Start(in, destAddrs, out); err != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
		}
		log.Printf("[STREAM:MGR] ERROR starting Streamer for route=%s: %v", sr.ID, err)
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
		log.Printf("[STREAM:MGR] [AUDIO:METER] attached tap to route=%s (%d channels)", sr.ID, sr.Channels)
	} else {
		log.Printf("[STREAM:MGR] [AUDIO:METER] WARN: hub is nil when starting route=%s; tap will be bound retroactively once hub attaches", sr.ID)
	}

	// Save the potentially updated route (with peer binding and validated destinations)
	sr.Destinations = destAddrs
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	m.routes[sr.ID] = &activeStream{route: sr, st: st, in: in, out: out, cancel: cancel}
	log.Printf("[STREAM:MGR] route %s: streaming device %s -> %d destinations (peer: %s, hasLocalOut: %t)",
		sr.ID, srcRef, len(destAddrs), sr.PeerID, out != nil)
	return nil
}

func openInputWithTimeout(resolver *audio.DeviceResolver, ref string, sampleRate, channels int, useEmulator bool, timeout time.Duration) audio.AudioStream {
	if resolver == nil {
		return audio.NewGhostAudioStream(sampleRate, channels)
	}
	if timeout <= 0 {
		st, err := resolver.OpenInputAsync(ref, sampleRate, channels, useEmulator)
		if err == nil && st != nil {
			return st
		}
		return resolver.OpenCushion(ref, audio.DirectionInput, sampleRate, channels)
	}
	type res struct {
		stream audio.AudioStream
		err    error
	}
	ch := make(chan res, 1)
	go func() {
		st, err := resolver.OpenInput(ref, sampleRate, channels, useEmulator)
		ch <- res{stream: st, err: err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			log.Printf("[STREAM:MGR] WARN: OpenInput(%s) error: %v, falling back to cushion handle", ref, r.err)
			return resolver.OpenCushion(ref, audio.DirectionInput, sampleRate, channels)
		}
		return r.stream
	case <-time.After(timeout):
		log.Printf("[STREAM:MGR] WARN: OpenInput(%s) timed out after %v, falling back to cushion handle", ref, timeout)
		cushion := resolver.OpenCushion(ref, audio.DirectionInput, sampleRate, channels)
		go func() {
			r := <-ch
			if r.err != nil {
				log.Printf("[STREAM:MGR] WARN: Late OpenInput(%s) completed with error: %v", ref, r.err)
				return
			}
			if r.stream != nil {
				log.Printf("[STREAM:MGR] Late-opened input device %s acquired after timeout; promoting via RebindDirectional", ref)
				resolver.RebindDirectional(ref, audio.DirectionInput, r.stream)
				_ = r.stream.Close()
			}
		}()
		return cushion
	}
}

func openOutputWithTimeout(resolver *audio.DeviceResolver, ref string, sampleRate, channels int, useEmulator bool, timeout time.Duration) audio.AudioStream {
	if resolver == nil {
		return audio.NewGhostAudioStream(sampleRate, channels)
	}
	if timeout <= 0 {
		st, err := resolver.OpenOutputAsync(ref, sampleRate, channels, useEmulator)
		if err == nil && st != nil {
			return st
		}
		return resolver.OpenCushion(ref, audio.DirectionOutput, sampleRate, channels)
	}
	type res struct {
		stream audio.AudioStream
		err    error
	}
	ch := make(chan res, 1)
	go func() {
		st, err := resolver.OpenOutput(ref, sampleRate, channels, useEmulator)
		ch <- res{stream: st, err: err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			log.Printf("[STREAM:MGR] WARN: OpenOutput(%s) error: %v, falling back to cushion handle", ref, r.err)
			return resolver.OpenCushion(ref, audio.DirectionOutput, sampleRate, channels)
		}
		return r.stream
	case <-time.After(timeout):
		log.Printf("[STREAM:MGR] WARN: OpenOutput(%s) timed out after %v, falling back to cushion handle", ref, timeout)
		cushion := resolver.OpenCushion(ref, audio.DirectionOutput, sampleRate, channels)
		go func() {
			r := <-ch
			if r.err != nil {
				log.Printf("[STREAM:MGR] WARN: Late OpenOutput(%s) completed with error: %v", ref, r.err)
				return
			}
			if r.stream != nil {
				log.Printf("[STREAM:MGR] Late-opened output device %s acquired after timeout; promoting via RebindDirectional", ref)
				resolver.RebindDirectional(ref, audio.DirectionOutput, r.stream)
				_ = r.stream.Close()
			}
		}()
		return cushion
	}
}

// StopRoute stops and releases a route's streams immediately and asynchronously.
func (m *streamManager) StopRoute(routeID string) error {
	m.mu.Lock()
	as, ok := m.routes[routeID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.routes, routeID)
	m.mu.Unlock()

	m.stopActiveStream(as)
	return nil
}

// StopAll stops every active stream concurrently and asynchronously.
func (m *streamManager) StopAll() {
	m.mu.Lock()
	m.closed = true
	toStop := make([]*activeStream, 0, len(m.routes))
	for id, as := range m.routes {
		toStop = append(toStop, as)
		delete(m.routes, id)
	}
	m.mu.Unlock()

	for _, as := range toStop {
		m.stopActiveStream(as)
	}
}

// Stats returns statistics for all active streams.
func (m *streamManager) Stats() map[string]streamer.StreamerStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make(map[string]streamer.StreamerStats, len(m.routes))
	for id, as := range m.routes {
		if as.st != nil {
			st := as.st.Stats()
			// ADR 0034: augment with route-level device binding and ring-drop
			// counters so the stream-stats handler can build the devices overlay.
			st.SourceDeviceURN = as.route.SourceDeviceURN
			st.DestDeviceURN = as.route.DestDeviceURN
			if m.resolver != nil {
				if urn := as.route.SourceDeviceURN; urn != "" {
					st.SourceGhost = m.resolver.IsQuarantined(urn)
				}
				if urn := as.route.DestDeviceURN; urn != "" {
					st.DestGhost = m.resolver.IsQuarantined(urn)
				}
			}
			if as.in != nil {
				if dc, ok := as.in.(audio.DropCounter); ok {
					st.CaptureDropBytes = dc.DroppedBytes()
					st.CaptureDropEvents = dc.DropEvents()
				}
			}
			if as.out != nil {
				if dc, ok := as.out.(audio.DropCounter); ok {
					st.OutputDropBytes = dc.DroppedBytes()
					st.OutputDropEvents = dc.DropEvents()
				}
			}
			res[id] = st
		}
	}
	return res
}

// RouteArtifactSamples implements telemetry.RouteArtifactSource (ADR 0034):
// one lock-free artifact sample per active route, augmented with the route
// identity, device URNs, ghost flags, and exact ring-drop counters.
func (m *streamManager) RouteArtifactSamples() []telemetry.RouteArtifactSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]telemetry.RouteArtifactSample, 0, len(m.routes))
	for _, as := range m.routes {
		if as.st == nil {
			continue
		}
		smp := as.st.ArtifactSample()
		smp.RouteID = as.route.ID
		smp.SourceDeviceURN = as.route.SourceDeviceURN
		smp.DestDeviceURN = as.route.DestDeviceURN
		if m.resolver != nil {
			if urn := as.route.SourceDeviceURN; urn != "" {
				smp.SourceGhost = m.resolver.IsQuarantined(urn)
			}
			if urn := as.route.DestDeviceURN; urn != "" {
				smp.DestGhost = m.resolver.IsQuarantined(urn)
			}
		}
		if as.in != nil {
			if dc, ok := as.in.(audio.DropCounter); ok {
				smp.CaptureDropBytes = dc.DroppedBytes()
				smp.CaptureDropEvents = dc.DropEvents()
			}
		}
		if as.out != nil {
			if dc, ok := as.out.(audio.DropCounter); ok {
				smp.OutputDropBytes = dc.DroppedBytes()
				smp.OutputDropEvents = dc.DropEvents()
			}
		}
		out = append(out, smp)
	}
	return out
}

// DeviceArtifactSamples implements telemetry.DeviceArtifactSource (ADR 0034):
// one sample per active directional device controller, carrying the
// direction-appropriate device-lifetime hardware XRun total.
func (m *streamManager) DeviceArtifactSamples() []telemetry.DeviceArtifactSample {
	if m.resolver == nil {
		return nil
	}
	stats := m.resolver.ActiveControllerStats()
	out := make([]telemetry.DeviceArtifactSample, 0, len(stats))
	for _, ds := range stats {
		xruns := ds.HWXrunsOut
		if ds.Direction == audio.DirectionInput {
			xruns = ds.HWXrunsIn
		}
		out = append(out, telemetry.DeviceArtifactSample{
			URN:       ds.URN,
			Direction: ds.Direction,
			Ghost:     ds.Ghost,
			HWXRuns:   xruns,
		})
	}
	return out
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
	m.stopActiveStream(as)
}

func (m *streamManager) stopActiveStream(as *activeStream) {
	if as == nil {
		return
	}
	if m.hub != nil {
		m.hub.UnregisterTap(as.route.ID)
		m.hub.UnregisterTap(as.route.ID + ":out")
	}
	if as.cancel != nil {
		as.cancel()
	}
	if as.st != nil {
		as.st.StopFast()
	}
	in := as.in
	out := as.out
	st := as.st
	routeID := as.route.ID
	as.in = nil
	as.out = nil

	go func(inStream, outStream audio.AudioStream, streamerInst *streamer.Streamer, rID string) {
		if inStream != nil {
			_ = inStream.Close()
		}
		if outStream != nil {
			_ = outStream.Close()
		}
		if streamerInst != nil {
			_ = streamerInst.Wait()
		}
	}(in, out, st, routeID)
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
