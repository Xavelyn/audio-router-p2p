package group

import (
	"errors"
	"fmt"
	"log"
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

	rooms  map[string]*managedRoom
	closed bool

	// OnLeaderChange is an optional notification hook for testing or external monitoring.
	OnLeaderChange func(roomID string, newLeader string)
}

var _ api.RoomManager = (*DefaultRoomManager)(nil)

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

	return &DefaultRoomManager{
		cfg:         cfg,
		disc:        disc,
		syncMgr:     syncMgr,
		trans:       trans,
		devProvider: dp,
		rooms:       make(map[string]*managedRoom),
	}
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
	defer m.mu.Unlock()

	if m.closed {
		return api.RoomInfo{}, errors.New("room manager is closed")
	}

	if id == "" {
		id = generateRoomID()
	}

	if mr, ok := m.rooms[id]; ok {
		return m.roomInfoLocked(mr.room), nil
	}

	localPeerID := ""
	if m.disc != nil {
		localPeerID = m.disc.NodeID()
	}

	room := NewRoom(RoomConfig{
		ID:            RoomID(id),
		Name:          name,
		LocalPeerID:   localPeerID,
		Config:        m.cfg,
		Discovery:     m.disc,
		SyncManager:   m.syncMgr,
		Transport:     m.trans,
		Router:        m.router,
		StreamManager: m.streamMgr,
	})

	roomID := room.ID()
	room.SetCallbacks(
		nil,
		nil,
		func(newLeader string) {
			m.handleLeaderChange(roomID, newLeader)
		},
		nil,
	)

	mr := &managedRoom{
		room: room,
	}
	m.rooms[id] = mr

	return m.roomInfoLocked(room), nil
}

// AddInput binds an audio input device to a room and re-synthesizes intra-room routes.
func (m *DefaultRoomManager) AddInput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	return mr.room.AddInput(deviceID)
}

// RemoveInput unbinds an audio input device from a room and cleans up routes.
func (m *DefaultRoomManager) RemoveInput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	return mr.room.RemoveInput(deviceID)
}

// AddOutput binds an audio output device to a room and re-synthesizes intra-room routes.
func (m *DefaultRoomManager) AddOutput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	return mr.room.AddOutput(deviceID)
}

// RemoveOutput unbinds an audio output device from a room and cleans up routes.
func (m *DefaultRoomManager) RemoveOutput(roomID, deviceID string) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	return mr.room.RemoveOutput(deviceID)
}

// SetRoomSync configures the distributed synchronization flag for a room.
func (m *DefaultRoomManager) SetRoomSync(roomID string, synced bool) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	return mr.room.SetRoomSync(synced)
}

// SetRoomBindings updates inputs, outputs, and sync state for a room in a single atomic transaction.
func (m *DefaultRoomManager) SetRoomBindings(roomID string, inputs, outputs []string, synced bool) error {
	m.mu.RLock()
	mr, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	return mr.room.SetRoomBindings(inputs, outputs, synced)
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
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true

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
	return nil
}

func (m *DefaultRoomManager) handleLeaderChange(roomID RoomID, newLeader string) {
	log.Printf("room manager: room %s leader changed to %s", roomID, newLeader)

	m.mu.RLock()
	cb := m.OnLeaderChange
	m.mu.RUnlock()

	if cb != nil {
		cb(string(roomID), newLeader)
	}
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
