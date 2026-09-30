package v1

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api/telemetry"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/sync"
	netTelemetry "github.com/audio-router-p2p/pkg/telemetry"
)

// StreamManager provides route streaming operations.
type StreamManager interface {
	StartRoute(route router.StreamRoute) error
	StopRoute(routeID string) error
	Stats() map[string]streamer.StreamerStats
}

// RoomManager provides conference room management operations.
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

// V1Server provides REST, SSE, and WebSocket endpoints for OpenAPI 3.1 control plane.
type V1Server struct {
	cfg       *config.Config
	router    *router.Router
	discovery *discovery.Discovery
	syncMgr   *sync.Manager
	streams   StreamManager
	roomMgr   RoomManager
	hub       *telemetry.TelemetryHub
	devReg    *DeviceRegistry
	startTime time.Time

	aggregator *netTelemetry.Aggregator
	clusterMgr *cluster.Manager
}

// SetAggregator configures the central network telemetry aggregator.
func (s *V1Server) SetAggregator(agg *netTelemetry.Aggregator) {
	s.aggregator = agg
}

// SetClusterManager configures the cluster manager.
func (s *V1Server) SetClusterManager(mgr *cluster.Manager) {
	s.clusterMgr = mgr
}

// ClusterManager returns the cluster manager instance.
func (s *V1Server) ClusterManager() *cluster.Manager {
	return s.clusterMgr
}

// UptimeSeconds returns the uptime of the server in seconds.
func (s *V1Server) UptimeSeconds() float64 {
	return time.Since(s.startTime).Seconds()
}

// NewV1Server constructs an initialized V1Server.
func NewV1Server(
	cfg *config.Config,
	rt *router.Router,
	disc *discovery.Discovery,
	syncMgr *sync.Manager,
	streams StreamManager,
	roomMgr RoomManager,
	hub *telemetry.TelemetryHub,
) *V1Server {
	if hub == nil {
		hub = telemetry.NewTelemetryHub(30)
	}
	s := &V1Server{
		cfg:       cfg,
		router:    rt,
		discovery: disc,
		syncMgr:   syncMgr,
		streams:   streams,
		roomMgr:   roomMgr,
		hub:       hub,
		devReg:    NewDeviceRegistry(),
		startTime: time.Now(),
	}
	return s
}

// Hub returns the telemetry hub instance.
func (s *V1Server) Hub() *telemetry.TelemetryHub {
	return s.hub
}

// DeviceRegistry returns the device registry instance.
func (s *V1Server) DeviceRegistry() *DeviceRegistry {
	return s.devReg
}

// Authenticate verifies request credentials (Bearer token, header, cookie, or query parameter).
func (s *V1Server) Authenticate(r *http.Request) bool {
	if s.cfg == nil || s.cfg.AuthToken == "" {
		return true
	}
	expected := []byte(s.cfg.AuthToken)

	// 1. Authorization: Bearer <token>
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), expected) == 1 {
			return true
		}
	}

	// 2. X-Auth-Token: <token>
	if xToken := r.Header.Get("X-Auth-Token"); xToken != "" {
		if subtle.ConstantTimeCompare([]byte(xToken), expected) == 1 {
			return true
		}
	}

	// 3. Cookie: audio_session=<token>
	if cookie, err := r.Cookie("audio_session"); err == nil && cookie.Value != "" {
		if subtle.ConstantTimeCompare([]byte(cookie.Value), expected) == 1 {
			return true
		}
	}

	// 4. Query parameter: token=<token>
	if qToken := r.URL.Query().Get("token"); qToken != "" {
		if subtle.ConstantTimeCompare([]byte(qToken), expected) == 1 {
			return true
		}
	}

	return false
}

// AuthMiddleware wraps a handler with OpenAPI security checks.
func (s *V1Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.Authenticate(r) {
			s.WriteError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "missing or invalid authentication credentials")
			return
		}
		next(w, r)
	}
}

// WriteJSON serializes v as application/json with the given HTTP status code.
func (s *V1Server) WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api/v1: encode error: %v", err)
	}
}

// WriteError serializes standard OpenAPI 3.1 ErrorResponse.
func (s *V1Server) WriteError(w http.ResponseWriter, statusCode int, code, msg string) {
	s.WriteJSON(w, statusCode, ErrorResponse{
		Error: msg,
		Code:  code,
	})
}

// MountRoutes mounts all /api/v1/* routes onto the provided mux.
func (s *V1Server) MountRoutes(mux *http.ServeMux) {
	// System & Node Management
	mux.HandleFunc("/api/v1/system/status", s.AuthMiddleware(s.handleSystemStatus))
	mux.HandleFunc("/api/v1/system/peers", s.AuthMiddleware(s.handleSystemPeers))
	mux.HandleFunc("/api/v1/system/clock-sync", s.AuthMiddleware(s.handleSystemClockSync))

	// Audio Device & Node Registry
	mux.HandleFunc("/api/v1/devices", s.AuthMiddleware(s.handleDevices))
	mux.HandleFunc("/api/v1/devices/", s.AuthMiddleware(s.handleDevicesSubpath))

	// Matrix Routing
	mux.HandleFunc("/api/v1/routes", s.AuthMiddleware(s.handleRoutes))
	mux.HandleFunc("/api/v1/routes/", s.AuthMiddleware(s.handleRoutesSubpath))

	// Room Orchestration
	mux.HandleFunc("/api/v1/rooms", s.AuthMiddleware(s.handleRooms))
	mux.HandleFunc("/api/v1/rooms/", s.AuthMiddleware(s.handleRoomsSubpath))

	// Cluster Management & Observability (ADR 0011)
	mux.HandleFunc("/api/v1/cluster/nodes", s.AuthMiddleware(s.handleClusterNodes))
	mux.HandleFunc("/api/v1/cluster/nodes/", s.AuthMiddleware(s.handleClusterNodesSubpath))
	mux.HandleFunc("/api/v1/cluster/devices", s.AuthMiddleware(s.handleClusterDevices))

	// Telemetry & Diagnostics
	mux.HandleFunc("/api/v1/network/health", s.AuthMiddleware(s.handleNetworkHealth))
	mux.HandleFunc("/api/v1/telemetry/stream-stats", s.AuthMiddleware(s.handleTelemetryStreamStats))
	mux.HandleFunc("/api/v1/telemetry/meters", s.AuthMiddleware(s.handleTelemetryMetersSSE))
	mux.HandleFunc("/api/v1/telemetry/ws", s.handleTelemetryWebSocket)
}

// handleNetworkHealth handles GET /api/v1/network/health.
func (s *V1Server) handleNetworkHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.aggregator != nil {
		snap := s.aggregator.GetHealthSnapshot()
		s.WriteJSON(w, http.StatusOK, snap)
		return
	}

	nodeID := ""
	if s.discovery != nil {
		nodeID = s.discovery.NodeID()
	}

	resp := netTelemetry.NetworkHealthSnapshot{
		NodeID:        nodeID,
		Timestamp:     time.Now(),
		HealthScore:   100.0,
		HealthGrade:   netTelemetry.GradeExcellent,
		LatencyMatrix: make(map[string]float64),
		Peers:         make(map[string]netTelemetry.PeerLinkHealth),
		Streams:       make(map[string]netTelemetry.StreamLinkStats),
	}
	if s.discovery != nil {
		resp.Cluster = s.discovery.Telemetry()
	}
	if s.syncMgr != nil {
		resp.Sync = s.syncMgr.Telemetry()
	}
	s.WriteJSON(w, http.StatusOK, resp)
}
