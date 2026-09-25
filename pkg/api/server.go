package api

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	stdSync "sync"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api/telemetry"
	v1 "github.com/audio-router-p2p/pkg/api/v1"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/sync"
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
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Leader  string   `json:"leader,omitempty"`
	State   string   `json:"state,omitempty"`
	Members []string `json:"members,omitempty"`
}

// RoomManager provides management operations for group audio rooms.
type RoomManager interface {
	ListRooms() []RoomInfo
	CreateRoom(id, name string) (RoomInfo, error)
	JoinRoom(id string) error
	LeaveRoom(id string) error
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

	// Pre-initialize telemetry hub and canonical v1 server
	_ = s.Hub()
	_ = s.V1()

	s.routes()
	return s
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
}

func (s *Server) getRoomManager() RoomManager {
	s.roomMgrMu.RLock()
	defer s.roomMgrMu.RUnlock()
	return s.roomMgr
}

// Handler returns the fully-wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverPanic(s.metrics(s.authenticate(s.mux)))
}

func (s *Server) routes() {
	// 1. Backward-compatible unversioned endpoints
	s.registerLegacyRoutes()

	// 2. Prometheus metrics
	s.mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))

	// 3. OpenAPI 3.1 canonical V1 endpoints
	s.V1().MountRoutes(s.mux)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Canonical OpenAPI 3.1 routes (/api/v1/*) manage their own authentication middleware
		// with multi-modal credential extraction and standardized ErrorResponse.
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		if s.cfg != nil && s.cfg.AuthToken != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				token = r.Header.Get("X-Auth-Token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if s.reqs != nil {
			s.reqs.WithLabelValues(r.Method, r.URL.Path).Inc()
		}
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
