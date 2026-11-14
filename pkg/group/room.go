package group

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/audio/urn"
	"github.com/audio-router-p2p/pkg/cluster"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
)

// RoomID is a unique identifier for a room/group
type RoomID string

// RoomState represents the state of a room
type RoomState int

const (
	RoomStateCreated RoomState = iota
	RoomStateElection
	RoomStateActive
	RoomStateClosing
)

func (s RoomState) String() string {
	switch s {
	case RoomStateCreated:
		return "created"
	case RoomStateElection:
		return "election"
	case RoomStateActive:
		return "active"
	case RoomStateClosing:
		return "closing"
	default:
		return "unknown"
	}
}

// RoomMember represents a member of a room
type RoomMember struct {
	PeerID     string        `json:"peerId"`
	NodeID     string        `json:"nodeId"`
	Addr       string        `json:"addr"`       // discovery address
	StreamAddr string        `json:"streamAddr"` // audio stream address
	SyncAddr   string        `json:"syncAddr"`   // clock sync address
	JoinedAt   time.Time     `json:"joinedAt"`
	LastSeen   time.Time     `json:"lastSeen"`
	IsLeader   bool          `json:"isLeader"`
	Offset     time.Duration `json:"offset"` // clock offset from leader
	RTT        time.Duration `json:"rtt"`    // round-trip time to leader
	Drift      time.Duration `json:"drift"`  // clock drift rate (ns/s)
}

// StreamManager abstracts route start and stop operations for real-time audio.
type StreamManager interface {
	StartRoute(route router.StreamRoute) error
	StopRoute(id string) error
}

// Room is a group of peers that play audio in sync
type Room struct {
	mu        sync.RWMutex
	id        RoomID
	name      string
	state     atomic.Value
	leader    atomic.Value
	members   map[string]*RoomMember // keyed by peer ID
	localID   string
	cfg       *config.Config
	disc      DiscoveryProvider
	syncMgr   *syncmgr.Manager
	transport streamer.Transport
	router    *router.Router
	streamMgr StreamManager

	clusterMgr  ClusterDeviceProvider
	devResolver LocalDeviceResolver

	inputs         []string
	outputs        []string
	synced         bool
	latencyPreset  string // low, balanced, reliable (balanced is the default)
	inputGains     map[string]float64
	outputGains    map[string]float64
	compiledRoutes map[string]struct{}

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	loopOnce  sync.Once

	// deterministic leader rule: lowest peer ID wins
	muE sync.Mutex

	onMemberJoin   func(*RoomMember)
	onMemberLeave  func(*RoomMember)
	onLeaderChange func(string)
	onStateChange  func(RoomState)
}

// ClusterDeviceProvider enables room route resolution across cluster nodes.
type ClusterDeviceProvider interface {
	GetSnapshot() *cluster.ClusterSnapshot
}

// LocalDeviceResolver resolves local hardware or virtual audio devices.
type LocalDeviceResolver interface {
	Resolve(ref string) (audio.Device, bool)
}

// DiscoveryProvider provides peer topology and live peer membership.
type DiscoveryProvider interface {
	Peers() []discovery.Peer
	LivePeers() []discovery.Peer
	NodeID() string
}

// RoomConfig holds the configuration for creating a room
type RoomConfig struct {
	ID             RoomID
	Name           string
	LocalPeerID    string
	Config         *config.Config
	Discovery      DiscoveryProvider
	SyncManager    *syncmgr.Manager
	Transport      streamer.Transport
	Router         *router.Router
	StreamManager  StreamManager
	ClusterManager ClusterDeviceProvider
	DeviceResolver LocalDeviceResolver
}

// NewRoom creates a new room
func NewRoom(cfg RoomConfig) *Room {
	if cfg.ID == "" {
		cfg.ID = RoomID(generateRoomID())
	}
	localID := cfg.LocalPeerID
	if localID == "" && cfg.Discovery != nil {
		localID = cfg.Discovery.NodeID()
	}
	r := &Room{
		id:             cfg.ID,
		name:           cfg.Name,
		members:        make(map[string]*RoomMember),
		localID:        localID,
		cfg:            cfg.Config,
		disc:           cfg.Discovery,
		syncMgr:        cfg.SyncManager,
		transport:      cfg.Transport,
		router:         cfg.Router,
		streamMgr:      cfg.StreamManager,
		clusterMgr:     cfg.ClusterManager,
		devResolver:    cfg.DeviceResolver,
		inputs:         make([]string, 0),
		outputs:        make([]string, 0),
		latencyPreset:  string(streamer.LatencyPresetBalanced),
		inputGains:     make(map[string]float64),
		outputGains:    make(map[string]float64),
		compiledRoutes: make(map[string]struct{}),
		stopCh:         make(chan struct{}),
	}
	r.state.Store(RoomState(RoomStateCreated))
	r.leader.Store("")
	return r
}

// SetClusterManager updates the cluster manager and re-synthesizes routes.
func (r *Room) SetClusterManager(cm ClusterDeviceProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clusterMgr = cm
	r.recompileRoutesLocked()
}

// SetDeviceResolver updates the local device resolver and re-synthesizes routes.
func (r *Room) SetDeviceResolver(res LocalDeviceResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devResolver = res
	r.recompileRoutesLocked()
}

// SetLeader updates the current room leader peer ID.
func (r *Room) SetLeader(leader string) {
	r.leader.Store(leader)
}

func generateRoomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ID returns the room ID
func (r *Room) ID() RoomID { return r.id }

// Name returns the room name
func (r *Room) Name() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.name
}

// SetName safely updates the room display name.
func (r *Room) SetName(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.name = name
}

// State returns the current room state
func (r *Room) State() RoomState { return r.state.Load().(RoomState) }

// Leader returns the current leader's peer ID
func (r *Room) Leader() string { return r.leader.Load().(string) }

// IsLeader returns true if we are the leader
func (r *Room) IsLeader() bool { return r.Leader() == r.localID }

// IsLocalOnly reports whether the room is strictly local-only (all assigned inputs and outputs are local endpoints).
func (r *Room) IsLocalOnly() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isLocalOnlyLocked()
}

// canonicalNodeIDLocked returns the effective node identifier, normalizing "local" or empty
// strings to r.localID when available, or "local" as a fallback sentinel.
func (r *Room) canonicalNodeIDLocked(nodeID string) string {
	if nodeID == "" || nodeID == "local" {
		if r.localID != "" {
			return r.localID
		}
		return "local"
	}
	return nodeID
}

// isLocalNodeLocked reports whether nodeID represents the local node.
func (r *Room) isLocalNodeLocked(nodeID string) bool {
	return nodeID == "local" || (r.localID != "" && nodeID == r.localID)
}

func (r *Room) singleOwningNodeLocked() (string, bool) {
	if len(r.inputs) == 0 && len(r.outputs) == 0 {
		return "", false
	}
	var owner string
	for _, dev := range r.inputs {
		rawNode, _ := r.resolveDeviceEndpointLocked(dev)
		nodeID := r.canonicalNodeIDLocked(rawNode)
		if owner == "" {
			owner = nodeID
		} else if nodeID != owner {
			return "", false
		}
	}
	for _, dev := range r.outputs {
		rawNode, _ := r.resolveDeviceEndpointLocked(dev)
		nodeID := r.canonicalNodeIDLocked(rawNode)
		if owner == "" {
			owner = nodeID
		} else if nodeID != owner {
			return "", false
		}
	}
	return owner, true
}

func (r *Room) isLocalOnlyLocked() bool {
	owner, ok := r.singleOwningNodeLocked()
	return ok && r.isLocalNodeLocked(owner)
}

// Transport returns the streamer transport configured for this room.
func (r *Room) Transport() streamer.Transport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.transport
}

// SetTransport updates the streamer transport for this room.
func (r *Room) SetTransport(t streamer.Transport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transport = t
}

// MarshalJSON provides a custom JSON serialization for Room.
func (r *Room) MarshalJSON() ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	members := make([]*RoomMember, 0, len(r.members))
	for _, m := range r.members {
		cp := *m
		members = append(members, &cp)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PeerID < members[j].PeerID })

	inputs := make([]string, len(r.inputs))
	copy(inputs, r.inputs)

	outputs := make([]string, len(r.outputs))
	copy(outputs, r.outputs)

	inGains := make(map[string]float64, len(r.inputGains))
	for k, v := range r.inputGains {
		inGains[k] = v
	}

	outGains := make(map[string]float64, len(r.outputGains))
	for k, v := range r.outputGains {
		outGains[k] = v
	}

	return json.Marshal(struct {
		ID            string             `json:"id"`
		Name          string             `json:"name"`
		Leader        string             `json:"leader"`
		State         string             `json:"state"`
		Members       []*RoomMember      `json:"members"`
		Inputs        []string           `json:"inputs"`
		Outputs       []string           `json:"outputs"`
		Synced        bool               `json:"synced"`
		LatencyPreset string             `json:"latency_preset,omitempty"`
		InputGains    map[string]float64 `json:"input_gains,omitempty"`
		OutputGains   map[string]float64 `json:"output_gains,omitempty"`
	}{
		ID:            string(r.id),
		Name:          r.name,
		Leader:        r.Leader(),
		State:         r.State().String(),
		Members:       members,
		Inputs:        inputs,
		Outputs:       outputs,
		Synced:        r.synced,
		LatencyPreset: r.latencyPreset,
		InputGains:    inGains,
		OutputGains:   outGains,
	})
}

// SetRouter sets or replaces the Matrix Router backing this room and recompiles routes.
func (r *Room) SetRouter(rt *router.Router) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.router != nil && r.router != rt {
		r.clearCompiledRoutesLocked()
	}
	r.router = rt
	r.recompileRoutesLocked()
}

// SetStreamManager sets or replaces the StreamManager for real-time streaming routes.
func (r *Room) SetStreamManager(sm StreamManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamMgr = sm
	r.recompileRoutesLocked()
}

// Router returns the active router.Router reference.
func (r *Room) Router() *router.Router {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.router
}

// Inputs returns a snapshot copy of assigned input device IDs.
func (r *Room) Inputs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.inputs))
	copy(out, r.inputs)
	return out
}

// Outputs returns a snapshot copy of assigned output device IDs.
func (r *Room) Outputs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.outputs))
	copy(out, r.outputs)
	return out
}

// Synced returns whether peer synchronization is active for the room.
func (r *Room) Synced() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.synced
}

// InputGains returns a copy of per-device input gain modifiers.
func (r *Room) InputGains() map[string]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]float64, len(r.inputGains))
	for k, v := range r.inputGains {
		out[k] = v
	}
	return out
}

// OutputGains returns a copy of per-device output gain modifiers.
func (r *Room) OutputGains() map[string]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]float64, len(r.outputGains))
	for k, v := range r.outputGains {
		out[k] = v
	}
	return out
}

// SetInputGain updates the trim gain for an input device and recompiles routes.
func (r *Room) SetInputGain(deviceID string, gain float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inputGains == nil {
		r.inputGains = make(map[string]float64)
	}
	r.inputGains[deviceID] = gain
	r.recompileRoutesLocked()
}

// SetOutputGain updates the trim gain for an output device and recompiles routes.
func (r *Room) SetOutputGain(deviceID string, gain float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outputGains == nil {
		r.outputGains = make(map[string]float64)
	}
	r.outputGains[deviceID] = gain
	r.recompileRoutesLocked()
}

// AddInput binds an audio input device to the room and updates routes.
// Supports both AddInput(deviceID) and AddInput(roomID, deviceID).
func (r *Room) AddInput(args ...string) error {
	r.mu.Lock()

	if len(args) == 0 {
		r.mu.Unlock()
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		r.mu.Unlock()
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		r.mu.Unlock()
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	log.Printf("[ROOM:ROUTE] AddInput called: room=%s devID=%q", r.id, devID)
	for _, id := range r.inputs {
		if id == devID {
			log.Printf("[ROOM:ROUTE] devID=%q already bound to room=%s", devID, r.id)
			r.mu.Unlock()
			return nil
		}
	}
	r.inputs = append(r.inputs, devID)
	r.recompileRoutesLocked()
	log.Printf("[ROOM:ROUTE] AddInput completed: room=%s devID=%q (inputs=%v, outputs=%v)", r.id, devID, r.inputs, r.outputs)
	r.mu.Unlock()
	r.recomputeLeader()
	return nil
}

// RemoveInput unbinds an audio input device from the room and updates routes.
// Supports both RemoveInput(deviceID) and RemoveInput(roomID, deviceID).
func (r *Room) RemoveInput(args ...string) error {
	r.mu.Lock()

	if len(args) == 0 {
		r.mu.Unlock()
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		r.mu.Unlock()
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		r.mu.Unlock()
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	log.Printf("[ROOM:ROUTE] RemoveInput called: room=%s devID=%q", r.id, devID)
	localNodeID := r.localID
	var remaining []string
	removedAny := false
	for _, id := range r.inputs {
		if EndpointMatches(localNodeID, id, devID) {
			removedAny = true
			delete(r.inputGains, id)
			delete(r.inputGains, devID)
		} else {
			remaining = append(remaining, id)
		}
	}
	for gKey := range r.inputGains {
		if EndpointMatches(localNodeID, gKey, devID) {
			delete(r.inputGains, gKey)
		}
	}
	if removedAny {
		r.inputs = remaining
		r.recompileRoutesLocked()
		log.Printf("[ROOM:ROUTE] RemoveInput completed: room=%s devID=%q (inputs=%v, outputs=%v)", r.id, devID, r.inputs, r.outputs)
		r.mu.Unlock()
		r.recomputeLeader()
		return nil
	}
	r.mu.Unlock()
	return nil
}

// AddOutput binds an audio output device to the room and updates routes.
// Supports both AddOutput(deviceID) and AddOutput(roomID, deviceID).
func (r *Room) AddOutput(args ...string) error {
	r.mu.Lock()

	if len(args) == 0 {
		r.mu.Unlock()
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		r.mu.Unlock()
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		r.mu.Unlock()
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	log.Printf("[ROOM:ROUTE] AddOutput called: room=%s devID=%q", r.id, devID)
	for _, id := range r.outputs {
		if id == devID {
			log.Printf("[ROOM:ROUTE] devID=%q already bound to room=%s", devID, r.id)
			r.mu.Unlock()
			return nil
		}
	}
	r.outputs = append(r.outputs, devID)
	r.recompileRoutesLocked()
	log.Printf("[ROOM:ROUTE] AddOutput completed: room=%s devID=%q (inputs=%v, outputs=%v)", r.id, devID, r.inputs, r.outputs)
	r.mu.Unlock()
	r.recomputeLeader()
	return nil
}

// RemoveOutput unbinds an audio output device from the room and updates routes.
// Supports both RemoveOutput(deviceID) and RemoveOutput(roomID, deviceID).
func (r *Room) RemoveOutput(args ...string) error {
	r.mu.Lock()

	if len(args) == 0 {
		r.mu.Unlock()
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		r.mu.Unlock()
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		r.mu.Unlock()
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	log.Printf("[ROOM:ROUTE] RemoveOutput called: room=%s devID=%q", r.id, devID)
	localNodeID := r.localID
	var remaining []string
	removedAny := false
	for _, id := range r.outputs {
		if EndpointMatches(localNodeID, id, devID) {
			removedAny = true
			delete(r.outputGains, id)
			delete(r.outputGains, devID)
		} else {
			remaining = append(remaining, id)
		}
	}
	for gKey := range r.outputGains {
		if EndpointMatches(localNodeID, gKey, devID) {
			delete(r.outputGains, gKey)
		}
	}
	if removedAny {
		r.outputs = remaining
		r.recompileRoutesLocked()
		log.Printf("[ROOM:ROUTE] RemoveOutput completed: room=%s devID=%q (inputs=%v, outputs=%v)", r.id, devID, r.inputs, r.outputs)
		r.mu.Unlock()
		r.recomputeLeader()
		return nil
	}
	r.mu.Unlock()
	return nil
}

// SetSynced updates peer sync flag.
func (r *Room) SetSynced(synced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synced = synced
}

// LatencyPreset returns the room's latency preset name (low, balanced, or
// reliable).
func (r *Room) LatencyPreset() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.latencyPreset
}

// SetLatencyPreset sets the room's latency preset and recompiles its routes
// so the new playout timing takes effect on every room stream. Unknown
// preset names are rejected.
func (r *Room) SetLatencyPreset(preset string) error {
	p := streamer.LatencyPreset(preset)
	if preset == "" || !p.Valid() {
		return fmt.Errorf("unknown latency preset %q (want one of %s)",
			preset, strings.Join(streamer.ValidPresets(), ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latencyPreset != string(p) {
		r.latencyPreset = string(p)
		r.clearCompiledRoutesLocked()
	}
	r.recompileRoutesLocked()
	return nil
}

// SetRoomSync sets the peer sync status.
// Accepts (synced bool) or (roomID string, synced bool).
func (r *Room) SetRoomSync(args ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(args) == 0 {
		return errors.New("synced argument required")
	}
	if len(args) == 1 {
		if s, ok := args[0].(bool); ok {
			r.synced = s
			return nil
		}
		return errors.New("synced argument must be a boolean")
	}
	if len(args) >= 2 {
		if roomID, ok := args[0].(string); ok && roomID != "" && roomID != string(r.id) {
			return fmt.Errorf("room ID mismatch: %s != %s", roomID, r.id)
		}
		if s, ok := args[1].(bool); ok {
			r.synced = s
			return nil
		}
		return errors.New("synced argument must be a boolean")
	}
	return nil
}

// SetBindings replaces inputs, outputs, and sync state atomically.
func (r *Room) SetBindings(inputs, outputs []string, synced bool) {
	r.mu.Lock()
	r.inputs = cleanDeviceList(inputs)
	r.outputs = cleanDeviceList(outputs)
	r.synced = synced
	r.recompileRoutesLocked()
	r.mu.Unlock()
	r.recomputeLeader()
}

// SetRoomBindings updates inputs, outputs, and sync state.
// Accepts (inputs, outputs []string, synced bool) or (roomID string, inputs, outputs []string, synced bool).
func (r *Room) SetRoomBindings(args ...any) error {
	r.mu.Lock()

	var inputs, outputs []string
	var synced bool

	if len(args) == 3 {
		var ok bool
		if inputs, ok = args[0].([]string); !ok {
			r.mu.Unlock()
			return errors.New("inputs must be []string")
		}
		if outputs, ok = args[1].([]string); !ok {
			r.mu.Unlock()
			return errors.New("outputs must be []string")
		}
		if synced, ok = args[2].(bool); !ok {
			r.mu.Unlock()
			return errors.New("synced must be bool")
		}
	} else if len(args) == 4 {
		if roomID, ok := args[0].(string); ok && roomID != "" && roomID != string(r.id) {
			r.mu.Unlock()
			return fmt.Errorf("room ID mismatch: %s != %s", roomID, r.id)
		}
		var ok bool
		if inputs, ok = args[1].([]string); !ok {
			r.mu.Unlock()
			return errors.New("inputs must be []string")
		}
		if outputs, ok = args[2].([]string); !ok {
			r.mu.Unlock()
			return errors.New("outputs must be []string")
		}
		if synced, ok = args[3].(bool); !ok {
			r.mu.Unlock()
			return errors.New("synced must be bool")
		}
	} else {
		r.mu.Unlock()
		return errors.New("invalid arguments for SetRoomBindings")
	}

	r.inputs = cleanDeviceList(inputs)
	r.outputs = cleanDeviceList(outputs)
	r.synced = synced
	r.recompileRoutesLocked()
	r.mu.Unlock()
	r.recomputeLeader()
	return nil
}

func cleanDeviceList(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; !ok {
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}

func (r *Room) clearCompiledRoutesLocked() {
	for routeID := range r.compiledRoutes {
		if r.streamMgr != nil {
			_ = r.streamMgr.StopRoute(routeID)
		}
		if r.router != nil {
			_ = r.router.DeleteMatrix(routeID)
		}
	}
	r.compiledRoutes = make(map[string]struct{})
}

func (r *Room) resolveDeviceEndpointLocked(devRef string) (nodeID, cleanDevID string) {
	devRef = strings.TrimSpace(devRef)
	if devRef == "" {
		return "local", ""
	}

	// Step 1: Explicit canonical slash prefix "<node_id>/<device_id>"
	if !strings.HasPrefix(devRef, "urn:") {
		if slash := strings.Index(devRef, "/"); slash > 0 {
			parsedNode := strings.TrimSpace(devRef[:slash])
			parsedDev := strings.TrimSpace(devRef[slash+1:])
			if parsedDev != "" {
				// If parsed node is already local, return local
				if parsedNode == "local" || (r.localID != "" && parsedNode == r.localID) {
					if r.localID != "" {
						return r.localID, parsedDev
					}
					return "local", parsedDev
				}

				// Only perform reconciliation if discovery or cluster manager is configured
				if r.disc != nil || r.clusterMgr != nil {
					isPeerLive := false
					if r.disc != nil {
						for _, peer := range r.disc.LivePeers() {
							if peer.ID == parsedNode {
								isPeerLive = true
								break
							}
						}
						if !isPeerLive {
							for _, peer := range r.disc.Peers() {
								if peer.ID == parsedNode {
									isPeerLive = true
									break
								}
							}
						}
					}
					if !isPeerLive && r.clusterMgr != nil {
						if snap := r.clusterMgr.GetSnapshot(); snap != nil {
							if snap.Nodes != nil {
								if _, exists := snap.Nodes[parsedNode]; exists {
									isPeerLive = true
								}
							}
							if !isPeerLive && snap.ClusterDevices != nil {
								for _, cd := range snap.ClusterDevices {
									if cd.NodeID == parsedNode {
										isPeerLive = true
										break
									}
								}
							}
						}
					}
					if !isPeerLive && r.members != nil {
						if _, exists := r.members[parsedNode]; exists {
							isPeerLive = true
						}
					}

					// If parsedNode is NOT an active live peer in the cluster, check if parsedDev exists locally
					if !isPeerLive {
						isLocalDev := false
						if r.devResolver != nil {
							_, isLocalDev = r.devResolver.Resolve(parsedDev)
						} else if gr := audio.GlobalResolver(); gr != nil {
							_, isLocalDev = gr.Resolve(parsedDev)
						}
						if !isLocalDev && r.devResolver == nil && (strings.HasPrefix(parsedDev, "urn:audio:hw:") || strings.HasPrefix(parsedDev, "urn:audio:virt:")) {
							isLocalDev = true
						}

						// Stale remote node prefix reconciled to active local node
						if isLocalDev {
							target := "local"
							if r.localID != "" {
								target = r.localID
							}
							log.Printf("[ROOM:RECONCILE] room=%s: reconciled stale node prefix %q -> local node %q for device %q",
								r.id, parsedNode, target, parsedDev)
							return target, parsedDev
						}
					}
				}

				return parsedNode, parsedDev
			}
		}
	}

	// Step 2: RFC 8141 node URN "urn:audio:node:<nodeID>:dev:<devID>"
	const nodePrefix = "urn:audio:node:"
	if strings.HasPrefix(devRef, nodePrefix) {
		remainder := strings.TrimPrefix(devRef, nodePrefix)
		if idx := strings.Index(remainder, ":dev:"); idx > 0 {
			return remainder[:idx], remainder[idx+5:]
		}
	}

	cleanDevID = devRef

	// Step 3: Cluster snapshot consensus (prioritized over local)
	if r.clusterMgr != nil {
		if snap := r.clusterMgr.GetSnapshot(); snap != nil {
			var matchingNodes []string
			seenNodes := make(map[string]bool)
			for _, cd := range snap.ClusterDevices {
				if cd.URN == cleanDevID || cd.ID == cleanDevID || cd.DeviceID == cleanDevID {
					if cd.NodeID != "" && !seenNodes[cd.NodeID] {
						seenNodes[cd.NodeID] = true
						matchingNodes = append(matchingNodes, cd.NodeID)
					}
				}
			}
			if len(matchingNodes) == 1 {
				return matchingNodes[0], cleanDevID
			} else if len(matchingNodes) > 1 {
				// Deterministic tie-breaker: check if local node is an explicit cluster owner
				hasLocal := false
				for _, n := range matchingNodes {
					if n == r.localID {
						hasLocal = true
						break
					}
				}
				if hasLocal {
					if r.localID != "" {
						return r.localID, cleanDevID
					}
					return "local", cleanDevID
				}
				// If local node does not own it, select lowest lexicographical remote node
				sort.Strings(matchingNodes)
				return matchingNodes[0], cleanDevID
			}
		}
	}

	// Step 4: Local device resolver (r.devResolver / audio.GlobalResolver())
	if r.devResolver != nil {
		if _, ok := r.devResolver.Resolve(cleanDevID); ok {
			if r.localID != "" {
				return r.localID, cleanDevID
			}
			return "local", cleanDevID
		}
	} else if gr := audio.GlobalResolver(); gr != nil {
		if _, ok := gr.Resolve(cleanDevID); ok {
			if r.localID != "" {
				return r.localID, cleanDevID
			}
			return "local", cleanDevID
		}
	}

	// Host-bound physical hardware and virtual URNs unlisted in cluster snapshot are local
	if strings.HasPrefix(cleanDevID, "urn:audio:hw:") || strings.HasPrefix(cleanDevID, "urn:audio:virt:") {
		if r.localID != "" {
			return r.localID, cleanDevID
		}
		return "local", cleanDevID
	}

	// Step 5: Fallback to single live peer in discovery (if 1 live peer exists), else r.localID, cleanDevID
	if r.disc != nil {
		live := r.disc.LivePeers()
		if len(live) == 1 && live[0].ID != "" {
			return live[0].ID, cleanDevID
		}
	}

	if r.localID != "" {
		return r.localID, cleanDevID
	}
	return "local", cleanDevID
}

// ResolveDeviceEndpoint resolves devRef to its owning nodeID and clean device ID.
func (r *Room) ResolveDeviceEndpoint(devRef string) (nodeID, cleanDevID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveDeviceEndpointLocked(devRef)
}

// resolveDeviceEndpoint resolves devRef to its owning nodeID and clean device ID.
func (r *Room) resolveDeviceEndpoint(devRef string) (nodeID, cleanDevID string) {
	return r.ResolveDeviceEndpoint(devRef)
}

func (r *Room) recompileRoutesLocked() {
	if r.router == nil {
		return
	}

	log.Printf("[ROOM:ROUTE] Recompiling routes for room=%s (inputs=%v, outputs=%v)", r.id, r.inputs, r.outputs)

	type endpoint struct {
		raw        string
		nodeID     string
		cleanDevID string
		key        string
	}

	endpointKey := func(nodeID, cleanDev string) string {
		key := cleanDev
		if strings.Contains(cleanDev, ":") {
			key = urn.Checksum(cleanDev)
		}
		if nodeID != "" && nodeID != "local" {
			key = nodeID + "-" + key
		}
		return key
	}

	// Pre-resolve endpoints: O(M + N) instead of O(M * N) inside the crossbar loop
	resolvedInputs := make([]endpoint, len(r.inputs))
	for i, inDev := range r.inputs {
		nodeID, cleanDev := r.resolveDeviceEndpointLocked(inDev)
		key := endpointKey(nodeID, cleanDev)
		resolvedInputs[i] = endpoint{raw: inDev, nodeID: nodeID, cleanDevID: cleanDev, key: key}
	}

	resolvedOutputs := make([]endpoint, len(r.outputs))
	for i, outDev := range r.outputs {
		nodeID, cleanDev := r.resolveDeviceEndpointLocked(outDev)
		key := endpointKey(nodeID, cleanDev)
		resolvedOutputs[i] = endpoint{raw: outDev, nodeID: nodeID, cleanDevID: cleanDev, key: key}
	}

	desired := make(map[string]router.MatrixRoute, len(resolvedInputs)*len(resolvedOutputs))
	for _, in := range resolvedInputs {
		for _, out := range resolvedOutputs {
			routeID := fmt.Sprintf("room-%s-%s-%s", r.id, in.key, out.key)

			inGain := 1.0
			if g, ok := r.inputGains[in.raw]; ok {
				inGain = g
			} else if g, ok := r.inputGains[in.cleanDevID]; ok {
				inGain = g
			}
			outGain := 1.0
			if g, ok := r.outputGains[out.raw]; ok {
				outGain = g
			} else if g, ok := r.outputGains[out.cleanDevID]; ok {
				outGain = g
			}
			totalGain := inGain * outGain
			if totalGain < 0.0 {
				totalGain = 0.0
			} else if totalGain > 2.0 {
				totalGain = 2.0
			}

			coeffs := []float64{totalGain, 0.0, 0.0, totalGain}

			log.Printf("[ROOM:ROUTE] Synthesized intra-room route=%s in=%s (%s) out=%s (%s) gain=%.2f",
				routeID, in.cleanDevID, in.nodeID, out.cleanDevID, out.nodeID, totalGain)

			desired[routeID] = router.MatrixRoute{
				ID:   routeID,
				Name: fmt.Sprintf("room-%s-%s-%s", r.id, in.cleanDevID, out.cleanDevID),
				Source: router.MatrixEndpoint{
					NodeID:   in.nodeID,
					DeviceID: in.cleanDevID,
					Layout:   router.LayoutStereo,
					Channels: []int{0, 1},
				},
				Destination: router.MatrixEndpoint{
					NodeID:   out.nodeID,
					DeviceID: out.cleanDevID,
					Layout:   router.LayoutStereo,
					Channels: []int{0, 1},
				},
				InChannels:    2,
				OutChannels:   2,
				Coefficients:  coeffs,
				SlewMS:        20.0,
				Muted:         false,
				State:         "active",
				LatencyPreset: r.latencyPreset,
			}
		}
	}

	// Remove routes no longer wanted
	for oldID := range r.compiledRoutes {
		if _, stillWanted := desired[oldID]; !stillWanted {
			log.Printf("[ROOM:ROUTE] Stopping obsolete route=%s", oldID)
			if r.streamMgr != nil {
				if err := r.streamMgr.StopRoute(oldID); err != nil {
					log.Printf("[ROOM:ROUTE] ERROR stopping stream route %s: %v", oldID, err)
				}
			}
			if err := r.router.DeleteMatrix(oldID); err != nil {
				log.Printf("[ROOM:ROUTE] ERROR deleting matrix route %s: %v", oldID, err)
			}
			delete(r.compiledRoutes, oldID)
		}
	}

	// Add or update desired routes
	for routeID, mr := range desired {
		if err := r.router.AddMatrix(mr); err != nil {
			log.Printf("[ROOM:ROUTE] ERROR adding matrix route %s: %v", routeID, err)
		}
		if _, alreadyCompiled := r.compiledRoutes[routeID]; alreadyCompiled {
			continue
		}
		r.compiledRoutes[routeID] = struct{}{}
		if r.streamMgr != nil {
			sr := router.MatrixRouteToStreamRoute(mr)
			log.Printf("[ROOM:ROUTE] Starting stream route=%s (srcURN=%s dstURN=%s dests=%v)",
				sr.ID, sr.SourceDeviceURN, sr.DestDeviceURN, sr.Destinations)
			if err := r.streamMgr.StartRoute(sr); err != nil {
				log.Printf("[ROOM:ROUTE] CRITICAL: failed to start stream route %s: %v", routeID, err)
				delete(r.compiledRoutes, routeID)
			} else {
				log.Printf("[ROOM:ROUTE] SUCCESS: stream route %s started", routeID)
			}
		}
	}
}

// CompiledRoutes returns a slice of active compiled route IDs.
func (r *Room) CompiledRoutes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	routes := make([]string, 0, len(r.compiledRoutes))
	for id := range r.compiledRoutes {
		routes = append(routes, id)
	}
	sort.Strings(routes)
	return routes
}

// Members returns all current members
func (r *Room) Members() []*RoomMember {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*RoomMember, 0, len(r.members))
	for _, m := range r.members {
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
}

// Member returns a specific member by peer ID
func (r *Room) Member(peerID string) *RoomMember {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m, ok := r.members[peerID]; ok {
		cp := *m
		return &cp
	}
	return nil
}

// LocalMember returns our own member info
func (r *Room) LocalMember() *RoomMember { return r.Member(r.localID) }

// Join starts the room loop
func (r *Room) Join() error {
	log.Printf("room %s: join (localID=%s)", r.id, r.localID)
	r.loopOnce.Do(func() {
		r.wg.Add(1)
		go r.roomLoop()
	})
	return nil
}

// Leave stops the room loop and clears active routes
func (r *Room) Leave() error {
	r.closeOnce.Do(func() {
		close(r.stopCh)
	})
	r.wg.Wait()
	r.mu.Lock()
	r.clearCompiledRoutesLocked()
	r.mu.Unlock()
	return nil
}

// Close gracefully terminates the room and cleans up resources.
func (r *Room) Close() error {
	return r.Leave()
}

// RecompileRoutes manually triggers route re-synthesis across the crossbar matrix.
func (r *Room) RecompileRoutes() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recompileRoutesLocked()
}

// OnDeviceLifecycle reactively handles physical and virtual device arrival or teardown.
func (r *Room) OnDeviceLifecycle(evt audio.DeviceLifecycleEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch evt.Type {
	case audio.DeviceRemoved, audio.DeviceQuarantined:
		changed := false
		var remainingInputs []string
		for _, in := range r.inputs {
			matched := false
			if evt.DeviceURN != "" && EndpointMatches(r.localID, in, evt.DeviceURN) {
				matched = true
			}
			if !matched && evt.LegacyID != "" && EndpointMatches(r.localID, in, evt.LegacyID) {
				matched = true
			}
			if matched {
				changed = true
				delete(r.inputGains, in)
				if evt.DeviceURN != "" {
					delete(r.inputGains, evt.DeviceURN)
				}
				if evt.LegacyID != "" {
					delete(r.inputGains, evt.LegacyID)
				}
			} else {
				remainingInputs = append(remainingInputs, in)
			}
		}

		var remainingOutputs []string
		for _, out := range r.outputs {
			matched := false
			if evt.DeviceURN != "" && EndpointMatches(r.localID, out, evt.DeviceURN) {
				matched = true
			}
			if !matched && evt.LegacyID != "" && EndpointMatches(r.localID, out, evt.LegacyID) {
				matched = true
			}
			if matched {
				changed = true
				delete(r.outputGains, out)
				if evt.DeviceURN != "" {
					delete(r.outputGains, evt.DeviceURN)
				}
				if evt.LegacyID != "" {
					delete(r.outputGains, evt.LegacyID)
				}
			} else {
				remainingOutputs = append(remainingOutputs, out)
			}
		}

		if changed {
			r.inputs = remainingInputs
			r.outputs = remainingOutputs
			r.recompileRoutesLocked()
		}

	case audio.DeviceCreated, audio.DeviceRestored:
		hasMatch := false
		for _, in := range r.inputs {
			if (evt.DeviceURN != "" && EndpointMatches(r.localID, in, evt.DeviceURN)) ||
				(evt.LegacyID != "" && EndpointMatches(r.localID, in, evt.LegacyID)) {
				hasMatch = true
				break
			}
		}
		if !hasMatch {
			for _, out := range r.outputs {
				if (evt.DeviceURN != "" && EndpointMatches(r.localID, out, evt.DeviceURN)) ||
					(evt.LegacyID != "" && EndpointMatches(r.localID, out, evt.LegacyID)) {
					hasMatch = true
					break
				}
			}
		}
		if hasMatch {
			r.recompileRoutesLocked()
		}
	}
}

// OnDeviceLifecycleEvent implements audio.DeviceLifecycleListener.
func (r *Room) OnDeviceLifecycleEvent(evt audio.DeviceLifecycleEvent) {
	r.OnDeviceLifecycle(evt)
}

// SetCallbacks registers room event callbacks.
// onMemberJoin/onMemberLeave receive member events;
// onLeaderChange receives the new leader peer ID;
// onStateChange receives room state transitions.
func (r *Room) SetCallbacks(
	onMemberJoin func(*RoomMember),
	onMemberLeave func(*RoomMember),
	onLeaderChange func(string),
	onStateChange func(RoomState),
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMemberJoin = onMemberJoin
	r.onMemberLeave = onMemberLeave
	r.onLeaderChange = onLeaderChange
	r.onStateChange = onStateChange
	log.Printf("room %s: SetCallbacks stored (leader=lowest-ID deterministic)", r.id)
}

// roomLoop is the single infinite loop: announce, converge, elect, sync.
func (r *Room) roomLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// immediately announce + sync + recompute
	r.announcePresence()
	r.syncMembersFromDiscovery()
	r.recomputeLeader()

	for {
		select {
		case <-ticker.C:
			r.announcePresence()
			r.syncMembersFromDiscovery()
			r.recomputeLeader()
			r.checkPeerTimeouts()
		case <-r.stopCh:
			log.Printf("room %s: stopCh closed, exiting loop", r.id)
			return
		}
	}
}

// addMember adds or refreshes a member.
func (r *Room) addMember(id string, m *RoomMember) {
	r.mu.Lock()
	var joined *RoomMember
	if cur, ok := r.members[id]; ok {
		if m.Addr != "" {
			cur.Addr = m.Addr
		}
		if m.StreamAddr != "" {
			cur.StreamAddr = m.StreamAddr
		}
		if m.SyncAddr != "" {
			cur.SyncAddr = m.SyncAddr
		}
		// carry the peer's own presence timestamp: when a remote stops announcing
		// (e.g. it closed transport), the discovery entry's LastSeen freezes, so
		// this member ages out via peerTTL even though discovery still lists the
		// stale peer for its own longer cache TTL.
		cur.LastSeen = m.LastSeen
	} else {
		m.LastSeen = time.Now()
		r.members[id] = m
		joined = m
	}
	r.mu.Unlock()

	if joined != nil && id != r.localID && r.onMemberJoin != nil {
		r.onMemberJoin(joined)
	}
}

// removeMember removes a member; returns true if removed.
func (r *Room) removeMember(id string) bool {
	r.mu.Lock()
	var left *RoomMember
	if cur, ok := r.members[id]; ok {
		left = cur
		delete(r.members, id)
	}
	// Any membership change re-enters the Election phase until the next
	// recompute converges on a leader again. Single-node rooms are immune unless the owner leaves.
	owner, isSingleNode := r.singleOwningNodeLocked()
	ownerLeaves := !isSingleNode || id == owner || (owner == "local" && id == r.localID)
	if left != nil && ownerLeaves {
		r.state.Store(RoomState(RoomStateElection))
	}

	cbState := r.onStateChange
	cbLeave := r.onMemberLeave
	r.mu.Unlock()

	// Only fire the member/state events after the mutation is visible.
	if left != nil && id != r.localID {
		if cbState != nil && ownerLeaves {
			cbState(RoomStateElection)
		}
		if cbLeave != nil {
			cbLeave(left)
		}
	}
	return left != nil
}

// recomputeLeader implements deterministic leader convergence.
//
// When a room contains endpoints belonging to a single node:
// The leader is unconditionally set to that owning node and room state to Active,
// provided the owning node is local or verified healthy in discovery.
func (r *Room) recomputeLeader() {
	if r.disc != nil {
		r.syncMembersFromDiscovery()
	}

	const peerTTL = 10 * time.Second
	now := time.Now()

	var present map[string]bool
	if r.disc != nil {
		present = make(map[string]bool, 16)
		for _, p := range r.disc.LivePeers() {
			present[p.ID] = true
		}
	}

	r.mu.RLock()
	owner, isSingleNode := r.singleOwningNodeLocked()
	cbLeader := r.onLeaderChange

	if isSingleNode && owner != "" {
		isLocal := r.isLocalNodeLocked(owner)
		var isOwnerHealthy bool
		if isLocal {
			isOwnerHealthy = true
		} else {
			m, exists := r.members[owner]
			isOwnerHealthy = exists &&
				now.Sub(m.LastSeen) <= peerTTL &&
				(r.disc == nil || present[owner])
		}

		if isOwnerHealthy {
			r.mu.RUnlock()
			leaderID := owner
			if leaderID == "local" && r.localID != "" {
				leaderID = r.localID
			}

			old := r.Leader()
			r.leader.Store(leaderID)
			r.state.Store(RoomState(RoomStateActive))
			if isLocal {
				log.Printf("room %s: recomputeLeader (local-only) -> leader=%s", r.id, leaderID)
			} else {
				log.Printf("room %s: recomputeLeader (single-node) -> leader=%s (localID=%s)", r.id, leaderID, r.localID)
			}
			if old != "" && old != leaderID {
				log.Printf("room %s: leader change %q -> %q", r.id, old, leaderID)
				if cbLeader != nil {
					cbLeader(leaderID)
				}
			}
			return
		}

		// Remote owner is absent or unhealthy: defer assertion and remain in Election
		r.mu.RUnlock()
		log.Printf("room %s: single owning node %s is absent or unhealthy, deferring active leader assertion", r.id, owner)
		return
	}

	var lowest string
	for id := range r.members {
		if lowest == "" || id < lowest {
			lowest = id
		}
	}
	// If any non-local member is stale or no longer visible in discovery, the
	// set is not yet converged: stay in the current (Election) state rather
	// than asserting Active. Without this, a departed peer resurrected from
	// discovery's cache would flip the state to Active on one tick and back
	// to Election on the next, racing consumer assertions.
	unhealthy := false
	for id, m := range r.members {
		if id == r.localID {
			continue
		}
		if now.Sub(m.LastSeen) > peerTTL || (r.disc != nil && !present[id]) {
			unhealthy = true
			break
		}
	}
	r.mu.RUnlock()

	if lowest == "" || unhealthy {
		return
	}

	old := r.Leader()
	if r.localID == lowest {
		r.leader.Store(r.localID)
		r.state.Store(RoomState(RoomStateActive))
		log.Printf("room %s: recomputeLeader -> became leader (lowestID=%s)", r.id, lowest)
	} else {
		r.leader.Store(lowest)
		r.state.Store(RoomState(RoomStateActive))
		log.Printf("room %s: recomputeLeader -> leader=%s (localID=%s)", r.id, lowest, r.localID)
	}
	// Fire the leader-change callback only for real leader transfers, not the
	// initial "" -> X establishment on the first converge tick. This keeps the
	// join callback the first event consumers see after joining, which the
	// integration oracles rely on (shared event channel ordering).
	if old != "" && r.Leader() != old {
		log.Printf("room %s: leader change %q -> %q", r.id, old, r.Leader())
		if cbLeader != nil {
			cbLeader(r.Leader())
		}
	}
}

// becomeLeader marks us as leader and is used for the deterministic path.
func (r *Room) becomeLeader() {
	r.leader.Store(r.localID)
	r.state.Store(RoomState(RoomStateActive))
	log.Printf("room %s: became leader", r.id)
}

// announcePresence sends a room announcement (kept from discovery layer).
func (r *Room) announcePresence() {
	if r.disc == nil {
		return
	}
	// this is handled by discovery's own announce loop in this build
	log.Printf("room %s: announcePresence tick", r.id)
}

// syncMembersFromDiscovery merges discovery peers into the member set.
func (r *Room) syncMembersFromDiscovery() {
	now := time.Now()
	// Always refresh our own presence
	if r.localID != "" {
		r.addMember(r.localID, &RoomMember{
			PeerID:   r.localID,
			NodeID:   r.localID,
			LastSeen: now,
		})
	}
	if r.disc == nil {
		return
	}
	for _, p := range r.disc.LivePeers() {
		r.addMember(p.ID, &RoomMember{
			PeerID:     p.ID,
			NodeID:     p.ID,
			Addr:       p.Addr,
			StreamAddr: p.StreamAddr,
			SyncAddr:   p.SyncAddr,
			JoinedAt:   now,
			LastSeen:   p.LastSeen, // presence-freshness: freeze when peer stops announcing
		})
	}
}

// checkPeerTimeouts removes members that are stale OR no longer present
// in discovery. Presence is the primary signal: a remote that discovery no
// longer lists is gone, so we evict it on the next tick rather than waiting
// a second full TTL for LastSeen to age out.
func (r *Room) checkPeerTimeouts() {
	const peerTTL = 10 * time.Second
	now := time.Now()

	var present map[string]bool
	if r.disc != nil {
		present = make(map[string]bool, 16)
		for _, p := range r.disc.LivePeers() {
			present[p.ID] = true
		}
	}

	var gone []string
	r.mu.RLock()
	for id, m := range r.members {
		if id == r.localID {
			continue
		}
		stale := now.Sub(m.LastSeen) > peerTTL
		absent := r.disc != nil && !present[id]
		if stale || absent {
			gone = append(gone, id)
		}
	}
	r.mu.RUnlock()

	for _, id := range gone {
		lastSeen, ok := func() (time.Time, bool) {
			r.mu.RLock()
			defer r.mu.RUnlock()
			m, ok := r.members[id]
			if !ok {
				return time.Time{}, false
			}
			return m.LastSeen, true
		}()
		if !ok {
			continue
		}
		if r.removeMember(id) {
			log.Printf("room %s: peer %s removed (stale=%v absent=%v)", r.id, id, now.Sub(lastSeen) > peerTTL, r.disc != nil && !present[id])
		}
	}
}

// getPeerAddr returns the UDP addr of a peer for transport.
func (r *Room) getPeerAddr(peerID string) string {
	m := r.Member(peerID)
	if m == nil {
		return ""
	}
	return m.Addr
}

// used by tests / transport helpers
func (r *Room) sendToAddr(addr string, data []byte) error {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp4", nil, udpAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(data)
	return err
}

func parsePort(addr string) int {
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(ps)
	return n
}
