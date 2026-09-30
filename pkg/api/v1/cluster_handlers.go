package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/audio-router-p2p/pkg/cluster"
)

// handleClusterNodes handles GET /api/v1/cluster/nodes.
func (s *V1Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.clusterMgr == nil {
		s.WriteError(w, http.StatusServiceUnavailable, "ERR_CLUSTER_UNAVAILABLE", "cluster manager not configured")
		return
	}

	snap := s.clusterMgr.GetSnapshot()
	if snap == nil {
		s.WriteError(w, http.StatusInternalServerError, "ERR_CLUSTER_SNAPSHOT", "failed to obtain cluster snapshot")
		return
	}

	var healthyCount, degradedCount int
	nodeList := make([]cluster.ClusterNodeSummary, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		if n.HealthScore >= 75.0 && (n.SWIMState == "alive" || n.IsLocal) && n.ErrorReason == "" {
			healthyCount++
		} else {
			degradedCount++
		}
		nodeList = append(nodeList, n)
	}
	sort.Slice(nodeList, func(i, j int) bool { return nodeList[i].ID < nodeList[j].ID })

	clusterID := ""
	if s.cfg != nil {
		clusterID = s.cfg.RoomID
		if clusterID == "" {
			clusterID = s.cfg.RoomName
		}
	}

	resp := cluster.ClusterNodesResponse{
		ClusterID:     clusterID,
		TotalNodes:    snap.TotalNodes,
		HealthyNodes:  healthyCount,
		DegradedNodes: degradedCount,
		ActiveNodes:   snap.ActiveNodes,
		Timestamp:     snap.GeneratedAt,
		Nodes:         nodeList,
		NodesMap:      snap.Nodes,
		LatencyMatrix: snap.LatencyMatrix,
	}
	s.WriteJSON(w, http.StatusOK, resp)
}

// handleClusterDevices handles GET /api/v1/cluster/devices.
// Supports query parameter ?direction=input|output|duplex.
func (s *V1Server) handleClusterDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.clusterMgr == nil {
		s.WriteError(w, http.StatusServiceUnavailable, "ERR_CLUSTER_UNAVAILABLE", "cluster manager not configured")
		return
	}

	snap := s.clusterMgr.GetSnapshot()
	if snap == nil {
		s.WriteError(w, http.StatusInternalServerError, "ERR_CLUSTER_SNAPSHOT", "failed to obtain cluster snapshot")
		return
	}

	directionFilter := strings.ToLower(r.URL.Query().Get("direction"))
	var filtered []cluster.AudioDeviceSummary
	for _, dev := range snap.ClusterDevices {
		if directionFilter == "input" {
			if dev.Direction != "input" && dev.Direction != "duplex" {
				continue
			}
		} else if directionFilter == "output" {
			if dev.Direction != "output" && dev.Direction != "duplex" {
				continue
			}
		} else if directionFilter != "" && directionFilter != "all" {
			if dev.Direction != directionFilter {
				continue
			}
		}
		filtered = append(filtered, dev)
	}

	if filtered == nil {
		filtered = []cluster.AudioDeviceSummary{}
	}

	resp := cluster.ClusterDevicesResponse{
		Timestamp: snap.GeneratedAt,
		Devices:   filtered,
	}
	s.WriteJSON(w, http.StatusOK, resp)
}

// handleClusterNodesSubpath routes /api/v1/cluster/nodes/{nodeId}/devices/{deviceId}.
func (s *V1Server) handleClusterNodesSubpath(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/cluster/nodes/")
	parts := strings.Split(sub, "/")
	// Expected format: {nodeId}/devices/{deviceId...}
	if len(parts) >= 3 && parts[1] == "devices" {
		rawNodeID := parts[0]
		nodeID, err := url.PathUnescape(rawNodeID)
		if err != nil {
			nodeID = rawNodeID
		}

		// Rejoin remaining parts in case device ID contains slashes or escaped characters
		rawDeviceID := strings.Join(parts[2:], "/")
		deviceID, err := url.PathUnescape(rawDeviceID)
		if err != nil {
			deviceID = rawDeviceID
		}
		if deviceID == "" || nodeID == "" {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "node_id and device_id are required")
			return
		}

		if r.Method == http.MethodPatch {
			s.handlePatchClusterNodeDevice(w, r, nodeID, deviceID)
			return
		}

		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "endpoint not found")
}

func (s *V1Server) handlePatchClusterNodeDevice(w http.ResponseWriter, r *http.Request, nodeID, deviceID string) {
	if s.clusterMgr == nil {
		s.WriteError(w, http.StatusServiceUnavailable, "ERR_CLUSTER_UNAVAILABLE", "cluster manager not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req cluster.DeviceMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
		return
	}

	if req.Volume != nil && (math.IsNaN(*req.Volume) || math.IsInf(*req.Volume, 0) || *req.Volume < 0.0 || *req.Volume > 2.0) {
		s.WriteError(w, http.StatusBadRequest, "ERR_OUT_OF_BOUNDS", "volume must be between 0.0 and 2.0")
		return
	}

	resp, err := s.clusterMgr.MutateRemoteDevice(r.Context(), nodeID, deviceID, req)
	if err != nil {
		if errors.Is(err, cluster.ErrNodeNotFound) {
			s.WriteError(w, http.StatusNotFound, "ERR_NODE_NOT_FOUND", fmt.Sprintf("node %q not found", nodeID))
			return
		}
		if errors.Is(err, cluster.ErrDeviceNotFound) {
			s.WriteError(w, http.StatusNotFound, "ERR_DEVICE_NOT_FOUND", fmt.Sprintf("device %q not found on node %q", deviceID, nodeID))
			return
		}
		if errors.Is(err, cluster.ErrRemoteNodeUnavailable) {
			s.WriteError(w, http.StatusBadGateway, "ERR_REMOTE_NODE_UNAVAILABLE", err.Error())
			return
		}
		s.WriteError(w, http.StatusBadRequest, "ERR_MUTATION_FAILED", err.Error())
		return
	}

	s.WriteJSON(w, http.StatusOK, resp)
}
