package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	stdSync "sync"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api/metrics"
	"github.com/audio-router-p2p/pkg/api/telemetry"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/sync"
	netTelemetry "github.com/audio-router-p2p/pkg/telemetry"
	"github.com/audio-router-p2p/pkg/transport"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StreamManager lets the API start and stop live audio streams that back a route.
type StreamManager interface {
	StartRoute(route router.StreamRoute) error
	StopRoute(routeID string) error
	Stats() map[string]streamer.StreamerStats
}

// RoomInfo describes a room's state and members.
type RoomInfo struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Leader      string             `json:"leader,omitempty"`
	State       string             `json:"state,omitempty"`
	Members     []string           `json:"members,omitempty"`
	Inputs      []string           `json:"inputs,omitempty"`
	Outputs     []string           `json:"outputs,omitempty"`
	Synced      bool               `json:"synced"`
	InputGains  map[string]float64 `json:"input_gains,omitempty"`
	OutputGains map[string]float64 `json:"output_gains,omitempty"`
}

// RoomManager provides management operations for group audio rooms.
type RoomManager interface {
	ListRooms() []RoomInfo
	CreateRoom(id, name string) (RoomInfo, error)
	JoinRoom(id string) error
	LeaveRoom(id string) error
	AddInput(roomID, deviceID string) error
	RemoveInput(roomID, deviceID string) error
	AddOutput(roomID, deviceID string) error
	RemoveOutput(roomID, deviceID string) error
	SetRoomSync(roomID string, synced bool) error
	SetRoomBindings(roomID string, inputs, outputs []string, synced bool) error
}

// Server is the HTTP control-plane API.
type Server struct {
	cfg       *config.Config
	rt        *router.Router
	disc      *discovery.Discovery
	syncMgr   *sync.Manager
	streams   StreamManager
	roomMgr   RoomManager
	roomMgrMu stdSync.RWMutex
	mux       *http.ServeMux

	reqs         *prometheus.CounterVec
	peerCnt      prometheus.Gauge
	routesActive prometheus.Gauge
	registry     *prometheus.Registry

	hub      *telemetry.TelemetryHub
	v1Server *v1.V1Server

	aggregator       *netTelemetry.Aggregator
	networkCollector *metrics.NetworkMetricsCollector

	clusterMgr *cluster.Manager
}

// NewServer wires the API together. streams and roomMgr may be nil for read-only mode.
func NewServer(cfg *config.Config, rt *router.Router, disc *discovery.Discovery, syncMgr *sync.Manager, streams StreamManager, roomMgr ...RoomManager) *Server {
	var rm RoomManager
	if len(roomMgr) > 0 {
		rm = roomMgr[0]
	}
	s := &Server{
		cfg:      cfg,
		rt:       rt,
		disc:     disc,
		syncMgr:  syncMgr,
		streams:  streams,
		roomMgr:  rm,
		mux:      http.NewServeMux(),
		registry: prometheus.NewRegistry(),
	}
	s.reqs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "audio_router_api_requests_total",
		Help: "Total number of API requests by method and path.",
	}, []string{"method", "path"})
	s.peerCnt = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "audio_router_peers",
		Help: "Current number of discovered peers.",
	})
	s.routesActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "audio_router_active_routes",
		Help: "Current number of active stream routes.",
	})
	s.registry.MustRegister(s.reqs, s.peerCnt, s.routesActive)

	// Initialize network telemetry aggregator
	nodeID := ""
	if disc != nil {
		nodeID = disc.NodeID()
	}
	s.aggregator = netTelemetry.NewAggregator(netTelemetry.Config{
		NodeID:    nodeID,
		Discovery: disc,
		ClockSync: syncMgr,
		Streamer:  &streamProviderAdapter{streams: streams},
	})
	s.networkCollector = metrics.NewNetworkMetricsCollector(s)
	s.registry.MustRegister(s.networkCollector)

	// Initialize cluster manager (ADR 0011)
	clusterCfg := cluster.Config{
		ScrapeInterval: 2 * time.Second,
		ScrapeTimeout:  1500 * time.Millisecond,
		MaxWorkers:     8,
	}
	if cfg != nil {
		clusterCfg.AuthToken = cfg.AuthToken
		clusterCfg.ClusterID = cfg.RoomID
		if clusterCfg.ClusterID == "" {
			clusterCfg.ClusterID = cfg.RoomName
		}
	}
	s.clusterMgr = cluster.NewManager(
		clusterCfg,
		&localClusterNodeAdapter{s: s},
		&clusterDiscoveryAdapter{disc: disc},
	)

	if rm != nil {
		if rSetter, ok := rm.(interface{ SetRouter(*router.Router) }); ok && s.rt != nil {
			rSetter.SetRouter(s.rt)
		}
		if smSetter, ok := rm.(interface{ SetStreamManager(StreamManager) }); ok && s.streams != nil {
			smSetter.SetStreamManager(s.streams)
		}
	}

	// Pre-initialize telemetry hub and canonical v1 server
	_ = s.Hub()
	_ = s.V1()
	s.V1().SetAggregator(s.aggregator)
	s.V1().SetClusterManager(s.clusterMgr)

	s.routes()
	return s
}

// Aggregator returns the central network telemetry aggregator instance.
func (s *Server) Aggregator() *netTelemetry.Aggregator {
	return s.aggregator
}

// SetAggregator configures an external or custom network telemetry aggregator.
func (s *Server) SetAggregator(agg *netTelemetry.Aggregator) {
	s.aggregator = agg
	if s.v1Server != nil {
		s.v1Server.SetAggregator(agg)
	}
}

// GetHealthSnapshot supplies the latest atomic snapshot to the Prometheus collector.
func (s *Server) GetHealthSnapshot() *netTelemetry.NetworkHealthSnapshot {
	if s.aggregator != nil {
		return s.aggregator.GetHealthSnapshot()
	}
	return nil
}

// ClusterManager returns the cluster telemetry aggregator and device management subsystem.
func (s *Server) ClusterManager() *cluster.Manager {
	return s.clusterMgr
}

// SetClusterManager configures an external or custom cluster manager.
func (s *Server) SetClusterManager(mgr *cluster.Manager) {
	s.clusterMgr = mgr
	if s.v1Server != nil {
		s.v1Server.SetClusterManager(mgr)
	}
}

// SetNetworkSimulator exposes the network-condition simulator for the
// /api/v1/network/sim endpoint.
func (s *Server) SetNetworkSimulator(sim *transport.NetworkSimulator) {
	s.V1().SetSimulator(sim)
}

// Close gracefully shuts down the API server and its background subsystems.
func (s *Server) Close() error {
	if s.clusterMgr != nil {
		_ = s.clusterMgr.Close()
	}
	return nil
}

type localClusterNodeAdapter struct {
	s *Server
}

func (a *localClusterNodeAdapter) NodeID() string {
	if a.s.disc != nil {
		return a.s.disc.NodeID()
	}
	return "local"
}

func (a *localClusterNodeAdapter) NodeName() string {
	if a.s.cfg != nil && a.s.cfg.AdvHost != "" {
		return a.s.cfg.AdvHost
	}
	return a.NodeID()
}

func (a *localClusterNodeAdapter) APIAddr() string {
	if a.s.disc != nil {
		return a.s.disc.APIAddr()
	}
	return ""
}

func (a *localClusterNodeAdapter) UptimeSeconds() float64 {
	return a.s.V1().UptimeSeconds()
}

func (a *localClusterNodeAdapter) ListDevices(kind string) ([]cluster.AudioDeviceSummary, error) {
	devs, err := a.s.V1().DeviceRegistry().ListDevices(kind)
	if err != nil {
		return nil, err
	}
	nodeID := a.NodeID()
	nodeName := a.NodeName()
	out := make([]cluster.AudioDeviceSummary, len(devs))
	for i, d := range devs {
		direction := "duplex"
		if d.MaxInput > 0 && d.MaxOutput == 0 {
			direction = "input"
		} else if d.MaxOutput > 0 && d.MaxInput == 0 {
			direction = "output"
		}
		devURN := d.URN
		if devURN == "" {
			devURN = d.ID
		}
		out[i] = cluster.AudioDeviceSummary{
			ID:                devURN,
			DeviceID:          devURN,
			URN:               devURN,
			NodeID:            nodeID,
			NodeName:          nodeName,
			Name:              d.Name,
			Direction:         direction,
			MaxInput:          d.MaxInput,
			MaxOutput:         d.MaxOutput,
			DefaultSampleRate: d.DefaultSampleRate,
			IsVirtual:         d.IsVirtual,
			Volume:            d.Volume,
			Muted:             d.Muted,
			SupportedLayouts:  d.SupportedLayouts,
			State:             "active",
		}
	}
	return out, nil
}

func (a *localClusterNodeAdapter) UpdateDeviceState(id string, vol *float64, muted *bool) (cluster.AudioDeviceSummary, bool) {
	dev, found := a.s.V1().DeviceRegistry().UpdateDeviceState(id, vol, muted)
	if !found {
		return cluster.AudioDeviceSummary{}, false
	}
	direction := "duplex"
	if dev.MaxInput > 0 && dev.MaxOutput == 0 {
		direction = "input"
	} else if dev.MaxOutput > 0 && dev.MaxInput == 0 {
		direction = "output"
	}
	devURN := dev.URN
	if devURN == "" {
		devURN = dev.ID
	}
	return cluster.AudioDeviceSummary{
		ID:                devURN,
		DeviceID:          devURN,
		URN:               devURN,
		NodeID:            a.NodeID(),
		NodeName:          a.NodeName(),
		Name:              dev.Name,
		Direction:         direction,
		MaxInput:          dev.MaxInput,
		MaxOutput:         dev.MaxOutput,
		DefaultSampleRate: dev.DefaultSampleRate,
		IsVirtual:         dev.IsVirtual,
		Volume:            dev.Volume,
		Muted:             dev.Muted,
		SupportedLayouts:  dev.SupportedLayouts,
		State:             "active",
	}, true
}

func (a *localClusterNodeAdapter) HealthSnapshot() *netTelemetry.NetworkHealthSnapshot {
	return a.s.GetHealthSnapshot()
}

func (a *localClusterNodeAdapter) ActiveStreams() int {
	if a.s.streams != nil {
		return len(a.s.streams.Stats())
	}
	return 0
}

type clusterDiscoveryAdapter struct {
	disc *discovery.Discovery
}

func (a *clusterDiscoveryAdapter) NodeID() string {
	if a.disc != nil {
		return a.disc.NodeID()
	}
	return "local"
}

func (a *clusterDiscoveryAdapter) Peers() []discovery.Peer {
	if a.disc != nil {
		return a.disc.Peers()
	}
	return nil
}

func (a *clusterDiscoveryAdapter) PeerByID(id string) *discovery.Peer {
	if a.disc != nil {
		return a.disc.PeerByID(id)
	}
	return nil
}

func (a *clusterDiscoveryAdapter) PeerState(id string) string {
	if a.disc != nil {
		return a.disc.PeerState(id)
	}
	return "alive"
}

type streamProviderAdapter struct {
	streams StreamManager
}

func (a *streamProviderAdapter) StreamLinkStats() []netTelemetry.StreamLinkStats {
	if a.streams == nil {
		return nil
	}
	if direct, ok := a.streams.(interface {
		StreamLinkStats() []netTelemetry.StreamLinkStats
	}); ok {
		return direct.StreamLinkStats()
	}
	raw := a.streams.Stats()
	var out []netTelemetry.StreamLinkStats
	for streamID, st := range raw {
		sid := st.StreamID
		if sid == "" {
			sid = streamID
		}
		for peerAddr, jb := range st.Peers {
			plcConcealed := jb.PLC.ConcealedFramesTotal
			if plcConcealed == 0 {
				plcConcealed = st.PLC.ConcealedFramesTotal
			}
			out = append(out, netTelemetry.StreamLinkStats{
				StreamID:            sid,
				PeerID:              peerAddr,
				PacketsSent:         st.PacketsSent,
				PacketsReceived:     st.PacketsReceived,
				JitterDepthFrames:   jb.BufferedFrames,
				JitterTargetDelayMS: float64(jb.TargetDelay.Milliseconds()),
				Underruns:           jb.Underruns,
				Overruns:            jb.Overruns,
				FECRecovered:        st.FEC.PacketsRecoveredTotal,
				PLCConcealed:        plcConcealed,
			})
		}
	}
	return out
}

// Hub returns the telemetry hub instance, lazily allocating if necessary.
func (s *Server) Hub() *telemetry.TelemetryHub {
	if s.hub == nil {
		s.hub = telemetry.NewTelemetryHub(30)
	}
	return s.hub
}

// V1 returns the canonical OpenAPI 3.1 V1Server instance, lazily allocating if necessary.
func (s *Server) V1() *v1.V1Server {
	if s.v1Server == nil {
		s.v1Server = v1.NewV1Server(
			s.cfg,
			s.rt,
			s.disc,
			s.syncMgr,
			s.streams,
			&v1RoomManagerAdapter{s: s},
			s.Hub(),
		)
	}
	return s.v1Server
}

// SetRoomManager configures the room manager for group endpoints.
func (s *Server) SetRoomManager(rm RoomManager) {
	s.roomMgrMu.Lock()
	defer s.roomMgrMu.Unlock()
	s.roomMgr = rm
	if rm != nil {
		if rSetter, ok := rm.(interface{ SetRouter(*router.Router) }); ok && s.rt != nil {
			rSetter.SetRouter(s.rt)
		}
		if smSetter, ok := rm.(interface{ SetStreamManager(StreamManager) }); ok && s.streams != nil {
			smSetter.SetStreamManager(s.streams)
		}
	}
}

func (s *Server) getRoomManager() RoomManager {
	s.roomMgrMu.RLock()
	defer s.roomMgrMu.RUnlock()
	return s.roomMgr
}

// v1RoomManagerAdapter adapts api.RoomManager to v1.RoomManager dynamically.
type v1RoomManagerAdapter struct {
	s *Server
}

func (a *v1RoomManagerAdapter) ListRooms() []v1.RoomInfo {
	rm := a.s.getRoomManager()
	if rm == nil {
		return nil
	}
	rooms := rm.ListRooms()
	res := make([]v1.RoomInfo, len(rooms))
	for i, r := range rooms {
		res[i] = v1.RoomInfo{
			ID:          r.ID,
			Name:        r.Name,
			Leader:      r.Leader,
			State:       r.State,
			Members:     r.Members,
			Inputs:      r.Inputs,
			Outputs:     r.Outputs,
			Synced:      r.Synced,
			InputGains:  r.InputGains,
			OutputGains: r.OutputGains,
		}
	}
	return res
}

func (a *v1RoomManagerAdapter) CreateRoom(id, name string) (v1.RoomInfo, error) {
	rm := a.s.getRoomManager()
	if rm == nil {
		return v1.RoomInfo{}, fmt.Errorf("room manager not configured")
	}
	r, err := rm.CreateRoom(id, name)
	if err != nil {
		return v1.RoomInfo{}, err
	}
	return v1.RoomInfo{
		ID:          r.ID,
		Name:        r.Name,
		Leader:      r.Leader,
		State:       r.State,
		Members:     r.Members,
		Inputs:      r.Inputs,
		Outputs:     r.Outputs,
		Synced:      r.Synced,
		InputGains:  r.InputGains,
		OutputGains: r.OutputGains,
	}, nil
}

func (a *v1RoomManagerAdapter) JoinRoom(id string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.JoinRoom(id)
}

func (a *v1RoomManagerAdapter) LeaveRoom(id string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.LeaveRoom(id)
}

func (a *v1RoomManagerAdapter) AddInput(roomID, deviceID string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.AddInput(roomID, deviceID)
}

func (a *v1RoomManagerAdapter) RemoveInput(roomID, deviceID string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.RemoveInput(roomID, deviceID)
}

func (a *v1RoomManagerAdapter) AddOutput(roomID, deviceID string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.AddOutput(roomID, deviceID)
}

func (a *v1RoomManagerAdapter) RemoveOutput(roomID, deviceID string) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.RemoveOutput(roomID, deviceID)
}

func (a *v1RoomManagerAdapter) SetRoomSync(roomID string, synced bool) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.SetRoomSync(roomID, synced)
}

func (a *v1RoomManagerAdapter) SetRoomBindings(roomID string, inputs, outputs []string, synced bool) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	return rm.SetRoomBindings(roomID, inputs, outputs, synced)
}

func (a *v1RoomManagerAdapter) SetInputGain(roomID, deviceID string, gain float64) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	if gSetter, ok := rm.(interface {
		SetInputGain(string, string, float64) error
	}); ok {
		return gSetter.SetInputGain(roomID, deviceID, gain)
	}
	return nil
}

func (a *v1RoomManagerAdapter) SetOutputGain(roomID, deviceID string, gain float64) error {
	rm := a.s.getRoomManager()
	if rm == nil {
		return fmt.Errorf("room manager not configured")
	}
	if gSetter, ok := rm.(interface {
		SetOutputGain(string, string, float64) error
	}); ok {
		return gSetter.SetOutputGain(roomID, deviceID, gain)
	}
	return nil
}

// Handler returns the fully-wrapped HTTP handler with CORS, body size limiting, metrics, and panic recovery.
func (s *Server) Handler() http.Handler {
	return s.cors(s.recoverPanic(s.metrics(s.maxBytes(s.mux))))
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS, HEAD")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Auth-Token, Accept, Origin, Cache-Control, X-Requested-With")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Type, Date, ETag")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	// 1. Prometheus metrics endpoint
	s.mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))

	// 2. Canonical /status alias simply forwarding to /api/v1/system/status
	s.mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/api/v1/system/status"
		s.mux.ServeHTTP(w, r2)
	})

	// 3. Canonical OpenAPI 3.1 V1 endpoints
	s.V1().MountRoutes(s.mux)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

// maxBytes limits incoming request bodies to 1MB.
func (s *Server) maxBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.reqs != nil {
			s.reqs.WithLabelValues(r.Method, r.URL.Path).Inc()
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("api: panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
