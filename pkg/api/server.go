package api

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/sync"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StreamManager lets the API start and stop live audio streams that back a route.
type StreamManager interface {
	StartRoute(route router.StreamRoute) error
	StopRoute(routeID string) error
}

// Server is the HTTP control-plane API.
type Server struct {
	cfg     *config.Config
	rt      *router.Router
	disc    *discovery.Discovery
	syncMgr *sync.Manager
	streams StreamManager
	mux     *http.ServeMux

	reqs         *prometheus.CounterVec
	peerCnt      prometheus.Gauge
	routesActive prometheus.Gauge
	registry     *prometheus.Registry
}

// NewServer wires the API together. streams may be nil for read-only mode.
func NewServer(cfg *config.Config, rt *router.Router, disc *discovery.Discovery, syncMgr *sync.Manager, streams StreamManager) *Server {
	s := &Server{
		cfg:      cfg,
		rt:       rt,
		disc:     disc,
		syncMgr:  syncMgr,
		streams:  streams,
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
	s.routes()
	return s
}

// Handler returns the fully-wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverPanic(s.metrics(s.authenticate(s.mux)))
}

func (s *Server) routes() {
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/devices", s.handleDevices)
	s.mux.HandleFunc("/peers", s.handlePeers)
	s.mux.HandleFunc("/sync", s.handleSync)
	s.mux.HandleFunc("/routes", s.handleRoutes)
	s.mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
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
		if s.cfg.AuthToken != "" {
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
		s.reqs.WithLabelValues(r.Method, r.URL.Path).Inc()
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var syncInfo map[string]sync.PeerOffset
	if s.syncMgr != nil {
		syncInfo = s.syncMgr.Offsets()
	}
	active := s.rt.List()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "ok",
		"nodeID":      s.disc.NodeID(),
		"addr":        s.disc.OwnAddr(),
		"peerCount":   len(s.disc.Peers()),
		"routeCount":  len(active),
		"syncPeers":   len(syncInfo),
		"uptimeNanos": time.Now().UnixNano(),
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	devs, err := audio.ListDevices()
	if err != nil {
		log.Printf("api: list devices: %v", err)
		devs = []audio.Device{}
	}
	writeJSON(w, http.StatusOK, devs)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	peers := s.disc.Peers()
	s.peerCnt.Set(float64(len(peers)))
	writeJSON(w, http.StatusOK, peers)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var offsets map[string]sync.PeerOffset
	if s.syncMgr != nil {
		offsets = s.syncMgr.Offsets()
	}
	writeJSON(w, http.StatusOK, offsets)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes := s.rt.List()
		s.routesActive.Set(float64(len(routes)))
		writeJSON(w, http.StatusOK, routes)

	case http.MethodPost:
		var sr router.StreamRoute
		if err := json.NewDecoder(r.Body).Decode(&sr); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if sr.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "route id is required"})
			return
		}
		if err := s.rt.Add(sr); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if s.streams != nil {
			if err := s.streams.StartRoute(sr); err != nil {
				_ = s.rt.Delete(sr.ID)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusCreated, sr)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
			return
		}
		if s.streams != nil {
			if err := s.streams.StopRoute(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := s.rt.Delete(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
