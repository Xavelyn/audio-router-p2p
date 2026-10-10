package group

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/audio-router-p2p/internal/config"
	"github.com/audio-router-p2p/pkg/api"
	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/discovery"
	"github.com/audio-router-p2p/pkg/router"
	"github.com/audio-router-p2p/pkg/streamer"
	syncmgr "github.com/audio-router-p2p/pkg/sync"
)

// DeviceProvider abstracts hardware and virtual audio I/O acquisition.
type DeviceProvider interface {
	GetInputDevice(name string) (audio.AudioStream, error)
	GetOutputDevice(name string) (audio.AudioStream, error)
}

// DefaultDeviceProvider acquires real or emulated audio devices from pkg/audio.
type DefaultDeviceProvider struct {
	SampleRate  int
	Channels    int
	UseEmulator bool
}

// NewDefaultDeviceProvider creates an initialized DefaultDeviceProvider.
func NewDefaultDeviceProvider(sampleRate, channels int, useEmulator bool) *DefaultDeviceProvider {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if channels <= 0 {
		channels = 2
	}
	return &DefaultDeviceProvider{
		SampleRate:  sampleRate,
		Channels:    channels,
		UseEmulator: useEmulator,
	}
}

// GetInputDevice returns an AudioStream input capture source.
func (p *DefaultDeviceProvider) GetInputDevice(name string) (audio.AudioStream, error) {
	return audio.OpenInput(-1, p.SampleRate, p.Channels, p.UseEmulator)
}

// GetOutputDevice returns an AudioStream output playback sink.
func (p *DefaultDeviceProvider) GetOutputDevice(name string) (audio.AudioStream, error) {
	return audio.OpenOutput(-1, p.SampleRate, p.Channels, p.UseEmulator)
}

// VirtualDeviceProvider provides mockable or virtual audio streams for testing.
type VirtualDeviceProvider struct {
	mu     sync.Mutex
	Input  audio.AudioStream
	Output audio.AudioStream
}

// NewVirtualDeviceProvider creates a VirtualDeviceProvider using injected devices.
func NewVirtualDeviceProvider(in, out audio.AudioStream) *VirtualDeviceProvider {
	return &VirtualDeviceProvider{
		Input:  in,
		Output: out,
	}
}

// GetInputDevice returns the configured input or a new VirtualAudioDevice.
func (v *VirtualDeviceProvider) GetInputDevice(name string) (audio.AudioStream, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Input != nil {
		return v.Input, nil
	}
	dev := audio.NewVirtualAudioDevice()
	v.Input = dev
	return dev, nil
}

// GetOutputDevice returns the configured output or a new VirtualAudioDevice.
func (v *VirtualDeviceProvider) GetOutputDevice(name string) (audio.AudioStream, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Output != nil {
		return v.Output, nil
	}
	dev := audio.NewVirtualAudioDevice()
	v.Output = dev
	return dev, nil
}

type managedRoom struct {
	room     *Room
	streamer *GroupStreamer
	joined   bool
}

// PersistedRoom is the file-backed format for serializing audio room state.
type PersistedRoom struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Leader        string             `json:"leader"`
	Inputs        []string           `json:"inputs"`
	Outputs       []string           `json:"outputs"`
	Synced        bool               `json:"synced"`
	InputGains    map[string]float64 `json:"input_gains,omitempty"`
	OutputGains   map[string]float64 `json:"output_gains,omitempty"`
	Timestamp     HLT                `json:"ts,omitempty"`
	Deleted       bool               `json:"deleted,omitempty"`
	DeletedAt     int64              `json:"deleted_at,omitempty"`
	InputAdds     map[string]HLT     `json:"input_adds,omitempty"`
	InputRemoves  map[string]HLT     `json:"input_removes,omitempty"`
	OutputAdds    map[string]HLT     `json:"output_adds,omitempty"`
	OutputRemoves map[string]HLT     `json:"output_removes,omitempty"`
}

// DefaultRoomManager coordinates room lifecycles, discovery presence, deterministic
// leader election, and group audio streaming matching api.RoomManager.
type DefaultRoomManager struct {
	mu          sync.RWMutex
	cfg         *config.Config
	disc        *discovery.Discovery
	syncMgr     *syncmgr.Manager
	trans       streamer.Transport
	router      *router.Router
	streamMgr   StreamManager
	devProvider DeviceProvider

	clusterMgr  ClusterDeviceProvider
	devResolver LocalDeviceResolver

	roomsFile string
	rooms     map[string]*managedRoom
	crdt      *LWWSet
	closed    bool

	debounceTimer *time.Timer
	pendingEvents []audio.DeviceLifecycleEvent

	// OnLeaderChange is an optional notification hook for testing or external monitoring.
	OnLeaderChange func(roomID string, newLeader string)
}

var _ api.RoomManager = (*DefaultRoomManager)(nil)
var _ discovery.StateDelegate = (*DefaultRoomManager)(nil)

// NewDefaultRoomManager creates an initialized DefaultRoomManager.
func NewDefaultRoomManager(
	cfg *config.Config,
	disc *discovery.Discovery,
	syncMgr *syncmgr.Manager,
	trans streamer.Transport,
	devProvider ...DeviceProvider,
) *DefaultRoomManager {
	var dp DeviceProvider
	if len(devProvider) > 0 && devProvider[0] != nil {
		dp = devProvider[0]
	} else {
		useEmu := false
		if cfg != nil {
			useEmu = cfg.UseEmulator
		}
		dp = NewDefaultDeviceProvider(48000, 2, useEmu)
	}

	roomsFile := ""
	if cfg != nil {
		roomsFile = cfg.RoomsFile
		if roomsFile == "" {
			roomsFile = "rooms.json"
		}
	}

	localNodeID := ""
	if disc != nil {
		localNodeID = disc.NodeID()
	}

	mgr := &DefaultRoomManager{
		cfg:         cfg,
		disc:        disc,
		syncMgr:     syncMgr,
		trans:       trans,
		devProvider: dp,
		roomsFile:   roomsFile,
		rooms:       make(map[string]*managedRoom),
		crdt:        NewLWWSet(localNodeID),
	}
	if disc != nil {
		disc.SetStateDelegate(mgr)
	}
	audio.GlobalEventHub().RegisterListener(mgr)
	return mgr
}

// SetRouter configures the router and propagates it to all active managed rooms.
func (m *DefaultRoomManager) SetRouter(rt *router.Router) {
	m.mu.Lock()
	m.router = rt
	rooms := make([]*Room, 0, len(m.rooms))
	for _, mr := range m.rooms {
		rooms = append(rooms, mr.room)
	}
	m.mu.Unlock()

	for _, r := range rooms {
		r.SetRouter(rt)
	}
}

// SetStreamManager configures the stream manager and propagates it to active rooms.
func (m *DefaultRoomManager) SetStreamManager(sm StreamManager) {
	m.mu.Lock()
	m.streamMgr = sm
	rooms := make([]*Room, 0, len(m.rooms))
	for _, mr := range m.rooms {
		rooms = append(rooms, mr.room)
	}
	m.mu.Unlock()

	for _, r := range rooms {
		r.SetStreamManager(sm)
	}
}

// Router returns the configured matrix router.
func (m *DefaultRoomManager) Router() *router.Router {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.router
}

// SetDeviceProvider updates the device provider.
func (m *DefaultRoomManager) SetDeviceProvider(dp DeviceProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devProvider = dp
}

// SetClusterManager sets the cluster device provider and updates all rooms.
func (m *DefaultRoomManager) SetClusterManager(cm ClusterDeviceProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clusterMgr = cm
	for _, mr := range m.rooms {
		if mr.room != nil {
			mr.room.SetClusterManager(cm)
		}
	}
}

// SetDeviceResolver sets the local device resolver and updates all rooms.
func (m *DefaultRoomManager) SetDeviceResolver(res LocalDeviceResolver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devResolver = res
	for _, mr := range m.rooms {
		if mr.room != nil {
			mr.room.SetDeviceResolver(res)
		}
	}
}

// newRoomInstanceLocked is the authoritative factory for all Room instances.
// Must be called with m.mu held.
func (m *DefaultRoomManager) newRoomInstanceLocked(id RoomID, name, leader string) *Room {
	localPeerID := ""
	var disc DiscoveryProvider
	if m.disc != nil {
		localPeerID = m.disc.NodeID()
		disc = m.disc
	}

	var devRes LocalDeviceResolver
	if m.devResolver != nil {
		devRes = m.devResolver
	} else if m.devProvider != nil {
		if lr, ok := m.devProvider.(LocalDeviceResolver); ok {
			devRes = lr
		}
	}
	if devRes == nil {
		devRes = audio.GlobalResolver()
	}

	room := NewRoom(RoomConfig{
		ID:             id,
		Name:           name,
		LocalPeerID:    localPeerID,
		Config:         m.cfg,
		Discovery:      disc,
		SyncManager:    m.syncMgr,
		Transport:      m.trans,
		Router:         m.router,
		StreamManager:  m.streamMgr,
		ClusterManager: m.clusterMgr,
		DeviceResolver: devRes,
	})

	roomID := room.ID()
	room.SetCallbacks(
		nil,
		nil,
		func(newLeader string) {
			go m.handleLeaderChange(roomID, newLeader)
		},
		nil,
	)

	if leader != "" {
		room.SetLeader(leader)
	}

	return room
}

// ListRooms lists all known rooms and their current statuses.
func (m *DefaultRoomManager) ListRooms() []api.RoomInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]api.RoomInfo, 0, len(m.rooms))
	for _, mr := range m.rooms {
		out = append(out, m.roomInfoLocked(mr.room))
	}
	return out
}

func (m *DefaultRoomManager) roomInfoLocked(r *Room) api.RoomInfo {
	members := r.Members()
	memberIDs := make([]string, len(members))
	for i, mem := range members {
		memberIDs[i] = mem.PeerID
	}

	return api.RoomInfo{
		ID:          string(r.ID()),
		Name:        r.Name(),
		Leader:      r.Leader(),
		State:       r.State().String(),
		Members:     memberIDs,
		Inputs:      r.Inputs(),
		Outputs:     r.Outputs(),
		Synced:      r.Synced(),
		InputGains:  r.InputGains(),
		OutputGains: r.OutputGains(),
	}
}

// CreateRoom registers a new room. If id is empty, a random ID is generated.
func (m *DefaultRoomManager) CreateRoom(id, name string) (api.RoomInfo, error) {
	m.mu.Lock()

	if m.closed {
		m.mu.Unlock()
		return api.RoomInfo{}, errors.New("room manager is closed")
	}

	if id == "" {
		id = generateRoomID()
	}

	rec := m.crdt.CreateRoom(id, name)

	if mr, ok := m.rooms[id]; ok {
		mr.room.SetName(name)
		info := m.roomInfoLocked(mr.room)
		m.mu.Unlock()
		m.broadcastDelta(rec)
		_ = m.Save()
		return info, nil
	}

	room := m.newRoomInstanceLocked(RoomID(id), name, "")

	mr := &managedRoom{
		room: room,
	}
	m.rooms[id] = mr
	info := m.roomInfoLocked(room)
	m.mu.Unlock()

	m.broadcastDelta(rec)
	_ = m.Save()
	return info, nil
}

// DeleteRoom removes a room, tears down any active stream, cleans up routes, and persists.
func (m *DefaultRoomManager) DeleteRoom(id string) error {
	rec, _ := m.crdt.DeleteRoom(id)

	m.mu.Lock()
	mr, ok := m.rooms[id]
	if !ok {
		m.mu.Unlock()
		m.broadcastDelta(rec)
		_ = m.Save()
		return fmt.Errorf("room %s not found", id)
	}
	gs := mr.streamer
	mr.streamer = nil
	mr.joined = false
	delete(m.rooms, id)
	m.mu.Unlock()

	if gs != nil {
		gs.Stop()
	}
	_ = mr.room.Leave()
	m.broadcastDelta(rec)
	_ = m.Save()
	return nil
}

// AddInput binds an audio input device to a room and re-synthesizes intra-room routes.
func (m *DefaultRoomManager) AddInput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	if err := mr.room.AddInput(deviceID); err != nil {
		return err
	}
	if rec, err := m.crdt.AddInput(roomID, deviceID); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// RemoveInput unbinds an audio input device from a room and cleans up routes.
func (m *DefaultRoomManager) RemoveInput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	if err := mr.room.RemoveInput(deviceID); err != nil {
		return err
	}
	if rec, err := m.crdt.RemoveInput(roomID, deviceID); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// AddOutput binds an audio output device to a room and re-synthesizes intra-room routes.
func (m *DefaultRoomManager) AddOutput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	if err := mr.room.AddOutput(deviceID); err != nil {
		return err
	}
	if rec, err := m.crdt.AddOutput(roomID, deviceID); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// RemoveOutput unbinds an audio output device from a room and cleans up routes.
func (m *DefaultRoomManager) RemoveOutput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	if err := mr.room.RemoveOutput(deviceID); err != nil {
		return err
	}
	if rec, err := m.crdt.RemoveOutput(roomID, deviceID); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// SetRoomSync configures the distributed synchronization flag for a room.
func (m *DefaultRoomManager) SetRoomSync(roomID string, synced bool) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	if err := mr.room.SetRoomSync(synced); err != nil {
		return err
	}
	if rec, err := m.crdt.SetRoomSync(roomID, synced); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// SetRoomBindings updates inputs, outputs, and sync state for a room in a single atomic transaction.
func (m *DefaultRoomManager) SetRoomBindings(roomID string, inputs, outputs []string, synced bool) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	if err := mr.room.SetRoomBindings(inputs, outputs, synced); err != nil {
		return err
	}
	if rec, err := m.crdt.SetRoomBindings(roomID, inputs, outputs, synced); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// SetInputGain updates trim gain for an input device in a room.
func (m *DefaultRoomManager) SetInputGain(roomID, deviceID string, gain float64) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	mr.room.SetInputGain(deviceID, gain)
	if rec, err := m.crdt.SetInputGain(roomID, deviceID, gain); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// SetOutputGain updates trim gain for an output device in a room.
func (m *DefaultRoomManager) SetOutputGain(roomID, deviceID string, gain float64) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	mr.room.SetOutputGain(deviceID, gain)
	if rec, err := m.crdt.SetOutputGain(roomID, deviceID, gain); err == nil {
		m.broadcastDelta(rec)
	}
	_ = m.Save()
	return nil
}

// JoinRoom joins the specified room and begins audio streaming.
func (m *DefaultRoomManager) JoinRoom(id string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("room manager is closed")
	}

	mr, ok := m.rooms[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("room %s not found", id)
	}
	if mr.joined {
		m.mu.Unlock()
		return nil
	}

	dp := m.devProvider
	m.mu.Unlock()

	// Acquire audio devices
	var in, out audio.AudioStream
	if dp != nil {
		var err error
		in, err = dp.GetInputDevice("")
		if err != nil {
			log.Printf("room manager %s: warning: failed to get input device: %v", id, err)
		}
		out, err = dp.GetOutputDevice("")
		if err != nil {
			log.Printf("room manager %s: warning: failed to get output device: %v", id, err)
		}
	}

	// Create GroupStreamer
	gs, err := NewGroupStreamer(GroupStreamerConfig{
		Room:         mr.room,
		Input:        in,
		Output:       out,
		StartLatency: 20 * time.Millisecond,
		SampleRate:   48000,
		Channels:     2,
	})
	if err != nil {
		return fmt.Errorf("create group streamer for room %s: %w", id, err)
	}

	if err := gs.Start(); err != nil {
		return fmt.Errorf("start group streamer for room %s: %w", id, err)
	}

	m.mu.Lock()
	mr.streamer = gs
	mr.joined = true
	m.mu.Unlock()

	return nil
}

// LeaveRoom halts streaming and leaves the specified room.
func (m *DefaultRoomManager) LeaveRoom(id string) error {
	m.mu.Lock()
	mr, ok := m.rooms[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("room %s not found", id)
	}

	if !mr.joined {
		m.mu.Unlock()
		return nil
	}

	gs := mr.streamer
	mr.streamer = nil
	mr.joined = false
	m.mu.Unlock()

	if gs != nil {
		gs.Stop()
	}
	return mr.room.Leave()
}

// Close gracefully closes all managed rooms and tears down active streamers.
func (m *DefaultRoomManager) Close() error {
	audio.GlobalEventHub().UnregisterListener(m)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true

	if m.debounceTimer != nil {
		m.debounceTimer.Stop()
		m.debounceTimer = nil
	}
	m.pendingEvents = nil

	activeStreamers := make([]*GroupStreamer, 0, len(m.rooms))
	roomsToLeave := make([]*Room, 0, len(m.rooms))

	for _, mr := range m.rooms {
		if mr.joined {
			if mr.streamer != nil {
				activeStreamers = append(activeStreamers, mr.streamer)
				mr.streamer = nil
			}
			roomsToLeave = append(roomsToLeave, mr.room)
			mr.joined = false
		}
	}
	m.mu.Unlock()

	for _, gs := range activeStreamers {
		gs.Stop()
	}
	for _, r := range roomsToLeave {
		_ = r.Leave()
	}
	_ = m.Save()
	return nil
}

// OnDeviceLifecycleEvent implements audio.DeviceLifecycleListener, reactively driving
// route teardown on device disconnection and fast-path reconnection on device arrival.
func (m *DefaultRoomManager) OnDeviceLifecycleEvent(evt audio.DeviceLifecycleEvent) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	rooms := make([]*Room, 0, len(m.rooms))
	for _, mr := range m.rooms {
		if mr.room != nil {
			rooms = append(rooms, mr.room)
		}
	}
	m.mu.Unlock()

	switch evt.Type {
	case audio.DeviceRemoved, audio.DeviceQuarantined:
		// Immediate route pruning and stream shutdown (< 1ms)
		for _, r := range rooms {
			r.OnDeviceLifecycle(evt)
		}

		// Update CRDT tombstones symmetrically - scoped to rooms containing a matching endpoint
		m.mu.Lock()
		localNode := ""
		if m.disc != nil {
			localNode = m.disc.NodeID()
		}

		changedAny := false
		for roomID := range m.rooms {
			rec, ok := m.crdt.GetRecord(roomID)
			if !ok || rec.Deleted {
				continue
			}

			hasInput := false
			for _, in := range rec.Inputs {
				if (evt.DeviceURN != "" && EndpointMatches(localNode, in, evt.DeviceURN)) ||
					(evt.LegacyID != "" && EndpointMatches(localNode, in, evt.LegacyID)) {
					hasInput = true
					break
				}
			}

			hasOutput := false
			for _, out := range rec.Outputs {
				if (evt.DeviceURN != "" && EndpointMatches(localNode, out, evt.DeviceURN)) ||
					(evt.LegacyID != "" && EndpointMatches(localNode, out, evt.LegacyID)) {
					hasOutput = true
					break
				}
			}

			if hasInput {
				if evt.DeviceURN != "" {
					if updated, err := m.crdt.RemoveInputSymmetric(roomID, evt.DeviceURN); err == nil {
						m.broadcastDelta(updated)
						changedAny = true
					}
				}
				if evt.LegacyID != "" && evt.LegacyID != evt.DeviceURN {
					if curRec, ok := m.crdt.GetRecord(roomID); ok {
						stillHas := false
						for _, in := range curRec.Inputs {
							if EndpointMatches(localNode, in, evt.LegacyID) {
								stillHas = true
								break
							}
						}
						if stillHas {
							if updated, err := m.crdt.RemoveInputSymmetric(roomID, evt.LegacyID); err == nil {
								m.broadcastDelta(updated)
								changedAny = true
							}
						}
					}
				}
			}

			if hasOutput {
				if evt.DeviceURN != "" {
					if updated, err := m.crdt.RemoveOutputSymmetric(roomID, evt.DeviceURN); err == nil {
						m.broadcastDelta(updated)
						changedAny = true
					}
				}
				if evt.LegacyID != "" && evt.LegacyID != evt.DeviceURN {
					if curRec, ok := m.crdt.GetRecord(roomID); ok {
						stillHas := false
						for _, out := range curRec.Outputs {
							if EndpointMatches(localNode, out, evt.LegacyID) {
								stillHas = true
								break
							}
						}
						if stillHas {
							if updated, err := m.crdt.RemoveOutputSymmetric(roomID, evt.LegacyID); err == nil {
								m.broadcastDelta(updated)
								changedAny = true
							}
						}
					}
				}
			}
		}
		m.mu.Unlock()
		if changedAny {
			_ = m.Save()
		}

	case audio.DeviceCreated, audio.DeviceRestored:
		// Fast-path reconnection with a 5ms debounce window aggregating multi-device bursts
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		m.pendingEvents = append(m.pendingEvents, evt)
		if m.debounceTimer != nil {
			m.debounceTimer.Stop()
		}
		m.debounceTimer = time.AfterFunc(5*time.Millisecond, func() {
			m.mu.Lock()
			if m.closed {
				m.pendingEvents = nil
				m.mu.Unlock()
				return
			}
			events := m.pendingEvents
			m.pendingEvents = nil
			rms := make([]*Room, 0, len(m.rooms))
			for _, mr := range m.rooms {
				if mr.room != nil {
					rms = append(rms, mr.room)
				}
			}
			m.mu.Unlock()

			for _, e := range events {
				for _, r := range rms {
					r.OnDeviceLifecycle(e)
				}
			}
		})
		m.mu.Unlock()
	}
}

func (m *DefaultRoomManager) handleLeaderChange(roomID RoomID, newLeader string) {
	log.Printf("room manager: room %s leader changed to %s", roomID, newLeader)

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return
	}
	cb := m.OnLeaderChange
	m.mu.RUnlock()

	if rec, ok := m.crdt.GetRecord(string(roomID)); ok && rec.Leader == newLeader {
		if cb != nil {
			cb(string(roomID), newLeader)
		}
		return
	}

	if rec, err := m.crdt.SetLeader(string(roomID), newLeader); err == nil {
		m.broadcastDelta(rec)
		_ = m.Save()
	}

	if cb != nil {
		cb(string(roomID), newLeader)
	}
}

func (m *DefaultRoomManager) broadcastDelta(rec RoomRecord) {
	if m.disc == nil {
		return
	}
	buf, err := MarshalCRDTDelta(m.disc.NodeID(), rec)
	if err == nil {
		m.disc.Broadcast(buf)
	}
}

// CRDT returns the underlying LWWSet CRDT store.
func (m *DefaultRoomManager) CRDT() *LWWSet {
	return m.crdt
}

// applyRecordLocked synchronizes a RoomRecord with the in-memory Room instances.
// Must be called with m.mu held. Returns any active streamers or rooms that need cleanup after unlock.
func (m *DefaultRoomManager) applyRecordLocked(rec RoomRecord) (*GroupStreamer, *Room) {
	if rec.Deleted {
		if mr, exists := m.rooms[rec.ID]; exists {
			gs := mr.streamer
			room := mr.room
			mr.streamer = nil
			mr.joined = false
			delete(m.rooms, rec.ID)
			return gs, room
		}
		return nil, nil
	}

	mr, exists := m.rooms[rec.ID]
	if !exists {
		room := m.newRoomInstanceLocked(RoomID(rec.ID), rec.Name, rec.Leader)
		for dev, gain := range rec.InputGains {
			room.SetInputGain(dev, gain)
		}
		for dev, gain := range rec.OutputGains {
			room.SetOutputGain(dev, gain)
		}
		room.SetBindings(rec.Inputs, rec.Outputs, rec.Synced)

		m.rooms[rec.ID] = &managedRoom{
			room: room,
		}
		return nil, nil
	}

	// Update existing room
	mr.room.SetName(rec.Name)
	if rec.Leader != "" {
		mr.room.SetLeader(rec.Leader)
	}
	for dev, gain := range rec.InputGains {
		mr.room.SetInputGain(dev, gain)
	}
	for dev, gain := range rec.OutputGains {
		mr.room.SetOutputGain(dev, gain)
	}
	mr.room.SetBindings(rec.Inputs, rec.Outputs, rec.Synced)
	return nil, nil
}

// MergeRecord merges a single room delta into the CRDT and reconciles active rooms.
func (m *DefaultRoomManager) MergeRecord(rec RoomRecord) {
	merged, changed := m.crdt.MergeRecord(rec)
	if !changed {
		return
	}
	m.mu.Lock()
	gs, rm := m.applyRecordLocked(merged)
	m.mu.Unlock()

	if gs != nil {
		gs.Stop()
	}
	if rm != nil {
		_ = rm.Leave()
	}
	_ = m.Save()
}

// MergeState merges multiple records into the CRDT and reconciles active rooms.
func (m *DefaultRoomManager) MergeState(records []RoomRecord) {
	changedIDs := m.crdt.MergeState(records)
	if len(changedIDs) == 0 {
		return
	}
	var toStop []*GroupStreamer
	var toLeave []*Room

	m.mu.Lock()
	for _, id := range changedIDs {
		if rec, ok := m.crdt.GetRecord(id); ok {
			gs, rm := m.applyRecordLocked(rec)
			if gs != nil {
				toStop = append(toStop, gs)
			}
			if rm != nil {
				toLeave = append(toLeave, rm)
			}
		}
	}
	m.mu.Unlock()

	for _, gs := range toStop {
		gs.Stop()
	}
	for _, rm := range toLeave {
		_ = rm.Leave()
	}
	_ = m.Save()
}

// NotifyMsg receives incoming gossip delta broadcasts from memberlist.
func (m *DefaultRoomManager) NotifyMsg(buf []byte) {
	if len(buf) == 0 {
		return
	}
	var delta CRDTDeltaMsg
	if err := json.Unmarshal(buf, &delta); err == nil && delta.Type == "crdt_room_delta" {
		m.MergeRecord(delta.Record)
		return
	}
	var state CRDTStateMsg
	if err := json.Unmarshal(buf, &state); err == nil && state.Type == "crdt_room_state" {
		m.MergeState(state.Records)
		return
	}
	var records []RoomRecord
	if err := json.Unmarshal(buf, &records); err == nil {
		m.MergeState(records)
		return
	}
}

// GetBroadcasts returns pending user broadcast messages (implemented via TransmitLimitedQueue).
func (m *DefaultRoomManager) GetBroadcasts(overhead, limit int) [][]byte {
	return nil
}

// LocalState serializes the full CRDT state (including active tombstones within 72h) for TCP push/pull.
func (m *DefaultRoomManager) LocalState(join bool) []byte {
	m.crdt.PruneTombstones()
	data, err := json.Marshal(m.crdt.ExportState())
	if err != nil {
		return nil
	}
	return data
}

// MergeRemoteState deserializes and merges the remote CRDT state after TCP push/pull.
func (m *DefaultRoomManager) MergeRemoteState(buf []byte, join bool) {
	if len(buf) == 0 {
		return
	}
	var records []RoomRecord
	if err := json.Unmarshal(buf, &records); err == nil {
		m.MergeState(records)
		return
	}
	var state CRDTStateMsg
	if err := json.Unmarshal(buf, &state); err == nil && state.Type == "crdt_room_state" {
		m.MergeState(state.Records)
		return
	}
}

// ExportCRDTRooms exports the serialized CRDT room state for HTTP anti-entropy.
func (m *DefaultRoomManager) ExportCRDTRooms() ([]byte, error) {
	m.crdt.PruneTombstones()
	return m.crdt.ExportStateJSON()
}

// MergeCRDTRooms merges incoming serialized CRDT room state for HTTP anti-entropy.
func (m *DefaultRoomManager) MergeCRDTRooms(data []byte) error {
	var records []RoomRecord
	if err := json.Unmarshal(data, &records); err != nil {
		var state CRDTStateMsg
		if err2 := json.Unmarshal(data, &state); err2 == nil {
			records = state.Records
		} else {
			return err
		}
	}
	m.MergeState(records)
	return nil
}

// GetRoom returns a Room by ID if managed.
func (m *DefaultRoomManager) GetRoom(id string) (*Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mr, ok := m.rooms[id]
	if !ok {
		return nil, false
	}
	return mr.room, true
}

// GetStreamer returns the active GroupStreamer for a room ID if running.
func (m *DefaultRoomManager) GetStreamer(id string) (*GroupStreamer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mr, ok := m.rooms[id]
	if !ok || mr.streamer == nil {
		return nil, false
	}
	return mr.streamer, true
}

// SetRoomsFile sets the backing file path for room configuration persistence.
func (m *DefaultRoomManager) SetRoomsFile(path string) {
	m.mu.Lock()
	m.roomsFile = path
	m.mu.Unlock()
}

// Load loads persisted rooms from disk and reconstructs their configurations and bindings.
func (m *DefaultRoomManager) Load(filePath ...string) error {
	path := ""
	if len(filePath) > 0 && filePath[0] != "" {
		path = filePath[0]
	}

	m.mu.Lock()
	if path != "" {
		m.roomsFile = path
	}
	file := m.roomsFile
	m.mu.Unlock()

	if file == "" {
		return nil
	}

	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", file, err)
	}
	if len(data) == 0 {
		return nil
	}

	var persisted map[string]PersistedRoom
	if err := json.Unmarshal(data, &persisted); err != nil {
		return fmt.Errorf("parsing %s: %w", file, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	localPeerID := ""
	if m.disc != nil {
		localPeerID = m.disc.NodeID()
	}
	localNodeID := localPeerID
	if localNodeID == "" {
		localNodeID = "local"
	}

	normalizeLoadedDev := func(dev string) string {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			return ""
		}
		if strings.Contains(dev, "/") || strings.HasPrefix(dev, "urn:audio:node:") {
			return dev
		}
		return localNodeID + "/" + dev
	}
	normalizeSlice := func(devs []string) []string {
		if devs == nil {
			return nil
		}
		out := make([]string, 0, len(devs))
		for _, dev := range devs {
			norm := normalizeLoadedDev(dev)
			if norm != "" {
				out = append(out, norm)
			}
		}
		return out
	}
	normalizeMapHLT := func(m map[string]HLT) map[string]HLT {
		if m == nil {
			return nil
		}
		out := make(map[string]HLT, len(m))
		for k, v := range m {
			norm := normalizeLoadedDev(k)
			if norm != "" {
				out[norm] = v
			}
		}
		return out
	}
	normalizeMapFloat := func(m map[string]float64) map[string]float64 {
		if m == nil {
			return nil
		}
		out := make(map[string]float64, len(m))
		for k, v := range m {
			norm := normalizeLoadedDev(k)
			if norm != "" {
				out[norm] = v
			}
		}
		return out
	}

	var records []RoomRecord
	for id, pr := range persisted {
		rec := RoomRecord{
			ID:            id,
			Name:          pr.Name,
			Leader:        pr.Leader,
			Inputs:        normalizeSlice(pr.Inputs),
			Outputs:       normalizeSlice(pr.Outputs),
			Synced:        pr.Synced,
			InputGains:    normalizeMapFloat(pr.InputGains),
			OutputGains:   normalizeMapFloat(pr.OutputGains),
			Timestamp:     pr.Timestamp,
			Deleted:       pr.Deleted,
			DeletedAt:     pr.DeletedAt,
			InputAdds:     normalizeMapHLT(pr.InputAdds),
			InputRemoves:  normalizeMapHLT(pr.InputRemoves),
			OutputAdds:    normalizeMapHLT(pr.OutputAdds),
			OutputRemoves: normalizeMapHLT(pr.OutputRemoves),
		}
		if rec.Timestamp.IsZero() {
			rec.Timestamp = HLT{WallTime: time.Now().UnixNano(), Counter: 1, NodeID: localPeerID}
		}
		records = append(records, rec)
	}
	m.crdt.ImportState(records)

	for _, pr := range m.crdt.ListActive() {
		id := pr.ID
		if _, exists := m.rooms[id]; exists {
			continue
		}

		room := m.newRoomInstanceLocked(RoomID(id), pr.Name, pr.Leader)

		for dev, gain := range pr.InputGains {
			room.SetInputGain(dev, gain)
		}
		for dev, gain := range pr.OutputGains {
			room.SetOutputGain(dev, gain)
		}
		room.SetBindings(pr.Inputs, pr.Outputs, pr.Synced)

		m.rooms[id] = &managedRoom{
			room: room,
		}
		log.Printf("[ROOM:PERSIST] Restored room %s (%q) inputs=%v outputs=%v synced=%v", id, pr.Name, pr.Inputs, pr.Outputs, pr.Synced)
	}
	return nil
}

func (m *DefaultRoomManager) getSnapshot() (string, map[string]PersistedRoom) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.roomsFile == "" {
		return "", nil
	}
	m.crdt.PruneTombstones()
	allRecs := m.crdt.ExportState()
	snapshot := make(map[string]PersistedRoom, len(allRecs))
	for _, r := range allRecs {
		snapshot[r.ID] = PersistedRoom{
			ID:            r.ID,
			Name:          r.Name,
			Leader:        r.Leader,
			Inputs:        r.Inputs,
			Outputs:       r.Outputs,
			Synced:        r.Synced,
			InputGains:    r.InputGains,
			OutputGains:   r.OutputGains,
			Timestamp:     r.Timestamp,
			Deleted:       r.Deleted,
			DeletedAt:     r.DeletedAt,
			InputAdds:     r.InputAdds,
			InputRemoves:  r.InputRemoves,
			OutputAdds:    r.OutputAdds,
			OutputRemoves: r.OutputRemoves,
		}
	}
	return m.roomsFile, snapshot
}

// Save persists the current room configuration snapshot to disk atomically.
func (m *DefaultRoomManager) Save() error {
	file, snapshot := m.getSnapshot()
	if file == "" || snapshot == nil {
		return nil
	}
	dir := filepath.Dir(file)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(file, data)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".rooms-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
