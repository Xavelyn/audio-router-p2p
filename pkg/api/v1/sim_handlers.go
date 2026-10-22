package v1

import (
	"encoding/json"
	"net/http"

	"github.com/audio-router-p2p/pkg/transport"
)

// handleNetworkSim handles GET and PUT /api/v1/network/sim.
//
// GET returns the active per-direction simulation profiles. PUT replaces
// them, enabling or tuning loss, latency, jitter, reordering, duplication,
// and bandwidth pacing at runtime. Servers without a UDP simulator (read-only
// mode or DTLS/encrypt mode) return 501.
func (s *V1Server) handleNetworkSim(w http.ResponseWriter, r *http.Request) {
	sim := s.sim.Load()
	if sim == nil {
		s.WriteError(w, http.StatusNotImplemented, "ERR_UNSUPPORTED",
			"network simulation is not available on this server (UDP mode only)")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.WriteJSON(w, http.StatusOK, sim.Profiles())

	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var profiles transport.SimProfiles
		if err := json.NewDecoder(r.Body).Decode(&profiles); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		sim.SetProfiles(profiles)
		s.WriteJSON(w, http.StatusOK, sim.Profiles())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
