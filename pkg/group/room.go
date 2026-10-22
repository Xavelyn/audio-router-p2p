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
	"github.com/audio-router-p2p/pkg/audio/urn"
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
	disc      *discovery.Discovery
	syncMgr   *syncmgr.Manager
	transport streamer.Transport
	router    *router.Router
	streamMgr StreamManager

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

// RoomConfig holds the configuration for creating a room
type RoomConfig struct {
	ID            RoomID
	Name          string
	LocalPeerID   string
	Config        *config.Config
	Discovery     *discovery.Discovery
	SyncManager   *syncmgr.Manager
	Transport     streamer.Transport
	Router        *router.Router
	StreamManager StreamManager
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

func generateRoomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ID returns the room ID
func (r *Room) ID() RoomID { return r.id }

// Name returns the room name
func (r *Room) Name() string { return r.name }

// State returns the current room state
func (r *Room) State() RoomState { return r.state.Load().(RoomState) }

// Leader returns the current leader's peer ID
func (r *Room) Leader() string { return r.leader.Load().(string) }

// IsLeader returns true if we are the leader
func (r *Room) IsLeader() bool { return r.Leader() == r.localID }

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
	defer r.mu.Unlock()

	if len(args) == 0 {
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	for _, id := range r.inputs {
		if id == devID {
			return nil
		}
	}
	r.inputs = append(r.inputs, devID)
	r.recompileRoutesLocked()
	return nil
}

// RemoveInput unbinds an audio input device from the room and updates routes.
// Supports both RemoveInput(deviceID) and RemoveInput(roomID, deviceID).
func (r *Room) RemoveInput(args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(args) == 0 {
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	idx := -1
	for i, id := range r.inputs {
		if id == devID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		r.inputs = append(r.inputs[:idx], r.inputs[idx+1:]...)
		delete(r.inputGains, devID)
		r.recompileRoutesLocked()
	}
	return nil
}

// AddOutput binds an audio output device to the room and updates routes.
// Supports both AddOutput(deviceID) and AddOutput(roomID, deviceID).
func (r *Room) AddOutput(args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(args) == 0 {
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	for _, id := range r.outputs {
		if id == devID {
			return nil
		}
	}
	r.outputs = append(r.outputs, devID)
	r.recompileRoutesLocked()
	return nil
}

// RemoveOutput unbinds an audio output device from the room and updates routes.
// Supports both RemoveOutput(deviceID) and RemoveOutput(roomID, deviceID).
func (r *Room) RemoveOutput(args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(args) == 0 {
		return errors.New("device ID required")
	}
	devID := args[len(args)-1]
	if devID == "" {
		return errors.New("device ID cannot be empty")
	}
	if len(args) >= 2 && args[0] != "" && args[0] != string(r.id) {
		return fmt.Errorf("room ID mismatch: %s != %s", args[0], r.id)
	}

	idx := -1
	for i, id := range r.outputs {
		if id == devID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		r.outputs = append(r.outputs[:idx], r.outputs[idx+1:]...)
		delete(r.outputGains, devID)
		r.recompileRoutesLocked()
	}
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
	r.latencyPreset = string(p)
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
	defer r.mu.Unlock()

	r.inputs = cleanDeviceList(inputs)
	r.outputs = cleanDeviceList(outputs)
	r.synced = synced
	r.recompileRoutesLocked()
}

// SetRoomBindings updates inputs, outputs, and sync state.
// Accepts (inputs, outputs []string, synced bool) or (roomID string, inputs, outputs []string, synced bool).
func (r *Room) SetRoomBindings(args ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var inputs, outputs []string
	var synced bool

	if len(args) == 3 {
		var ok bool
		if inputs, ok = args[0].([]string); !ok {
			return errors.New("inputs must be []string")
		}
		if outputs, ok = args[1].([]string); !ok {
			return errors.New("outputs must be []string")
		}
		if synced, ok = args[2].(bool); !ok {
			return errors.New("synced must be bool")
		}
	} else if len(args) == 4 {
		if roomID, ok := args[0].(string); ok && roomID != "" && roomID != string(r.id) {
			return fmt.Errorf("room ID mismatch: %s != %s", roomID, r.id)
		}
		var ok bool
		if inputs, ok = args[1].([]string); !ok {
			return errors.New("inputs must be []string")
		}
		if outputs, ok = args[2].([]string); !ok {
			return errors.New("outputs must be []string")
		}
		if synced, ok = args[3].(bool); !ok {
			return errors.New("synced must be bool")
		}
	} else {
		return errors.New("invalid arguments for SetRoomBindings")
	}

	r.inputs = cleanDeviceList(inputs)
	r.outputs = cleanDeviceList(outputs)
	r.synced = synced
	r.recompileRoutesLocked()
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

func (r *Room) recompileRoutesLocked() {
	if r.router == nil {
		return
	}

	desired := make(map[string]router.MatrixRoute)
	for _, inDev := range r.inputs {
		for _, outDev := range r.outputs {
			inKey := inDev
			if strings.Contains(inDev, ":") {
				inKey = urn.Checksum(inDev)
			}
			outKey := outDev
			if strings.Contains(outDev, ":") {
				outKey = urn.Checksum(outDev)
			}
			routeID := fmt.Sprintf("room-%s-%s-%s", r.id, inKey, outKey)

			inGain := 1.0
			if g, ok := r.inputGains[inDev]; ok {
				inGain = g
			}
			outGain := 1.0
			if g, ok := r.outputGains[outDev]; ok {
				outGain = g
			}
			totalGain := inGain * outGain
			if totalGain < 0.0 {
				totalGain = 0.0
			} else if totalGain > 2.0 {
				totalGain = 2.0
			}

			coeffs := []float64{totalGain, 0.0, 0.0, totalGain}

			desired[routeID] = router.MatrixRoute{
				ID:   routeID,
				Name: fmt.Sprintf("room-%s-%s-%s", r.id, inDev, outDev),
				Source: router.MatrixEndpoint{
					NodeID:   "local",
					DeviceID: inDev,
					Layout:   router.LayoutStereo,
					Channels: []int{0, 1},
				},
				Destination: router.MatrixEndpoint{
					NodeID:   "local",
					DeviceID: outDev,
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
			if r.streamMgr != nil {
				_ = r.streamMgr.StopRoute(oldID)
			}
			_ = r.router.DeleteMatrix(oldID)
			delete(r.compiledRoutes, oldID)
		}
	}

	// Add or update desired routes
	for routeID, mr := range desired {
		_ = r.router.AddMatrix(mr)
		r.compiledRoutes[routeID] = struct{}{}
		if r.streamMgr != nil {
			sr := router.MatrixRouteToStreamRoute(mr)
			_ = r.streamMgr.StartRoute(sr)
		}
	}
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

	// immediately announce + recompute
	r.announcePresence()
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
	// recompute converges on a leader again. recomputeLeader then restores
	// RoomStateActive on the following tick.
	if left != nil {
		r.state.Store(RoomState(RoomStateElection))
	}
	r.mu.Unlock()

	// Only fire the member/state events after the mutation is visible.
	if left != nil && id != r.localID {
		if r.onStateChange != nil {
			r.onStateChange(RoomStateElection)
		}
		if r.onMemberLeave != nil {
			r.onMemberLeave(left)
		}
	}
	return left != nil
}

// recomputeLeader implements the deterministic lowest-ID rule.
// The minimum peer ID among the converged member set is always the leader.
// Every node runs this same function on the same member set, so all nodes
// agree on a single leader; no network election is required and no
// split-brain is possible (peer IDs are unique).
func (r *Room) recomputeLeader() {
	const peerTTL = 10 * time.Second
	now := time.Now()

	var present map[string]bool
	if r.disc != nil {
		present = make(map[string]bool, 16)
		for _, p := range r.disc.Peers() {
			present[p.ID] = true
		}
	}

	r.mu.RLock()
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
		if r.onLeaderChange != nil {
			r.onLeaderChange(r.Leader())
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
	if r.disc == nil {
		return
	}
	now := time.Now()
	for _, p := range r.disc.Peers() {
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
	// always include ourselves
	r.addMember(r.localID, &RoomMember{
		PeerID:   r.localID,
		NodeID:   r.localID,
		Addr:     "",
		LastSeen: now,
	})
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
		for _, p := range r.disc.Peers() {
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
