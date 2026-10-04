package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio/urn"
)

var alsaNameRegex = regexp.MustCompile(`\(hw:(\d+)(?:,(\d+))?\)`)

const defaultGracePeriod = 500 * time.Millisecond

// StreamDirection indicates whether an audio stream is capture (input) or playback (output).
type StreamDirection uint8

const (
	DirectionInput  StreamDirection = 1
	DirectionOutput StreamDirection = 2
)

func (d StreamDirection) String() string {
	switch d {
	case DirectionInput:
		return "input"
	case DirectionOutput:
		return "output"
	default:
		return fmt.Sprintf("unknown(%d)", d)
	}
}

// StreamKey uniquely identifies a directional hardware audio stream by device URN and direction.
type StreamKey struct {
	URN       string
	Direction StreamDirection
}

// reopener represents streams that can reset their closed state for subsequent route bindings.
type reopener interface {
	Reopen()
}

// DeviceResolver maps canonical URNs and legacy aliases ("0", "1", "hw:0,0")
// to Device metadata and audio streams, managing ghost device transitions,
// reference-counted virtual stream handles, and multiplexed hardware routing.
type DeviceResolver struct {
	mu             sync.RWMutex
	devices        map[string]Device                     // Keyed by canonical URN
	aliases        map[string]string                     // Alias -> Canonical URN
	quarantined    map[string]bool                       // URN -> isQuarantined
	controllers    map[StreamKey]*deviceStreamController // Active directional controllers
	reboundStreams map[StreamKey]AudioStream             // Stored pre-bind streams if no controller active
	gracePeriod    time.Duration
}

// NewDeviceResolver creates a thread-safe device resolver.
func NewDeviceResolver() *DeviceResolver {
	r := &DeviceResolver{
		devices:        make(map[string]Device),
		aliases:        make(map[string]string),
		quarantined:    make(map[string]bool),
		controllers:    make(map[StreamKey]*deviceStreamController),
		reboundStreams: make(map[StreamKey]AudioStream),
		gracePeriod:    defaultGracePeriod,
	}
	_ = r.Refresh()
	return r
}

var (
	globalResolver     *DeviceResolver
	globalResolverOnce sync.Once
)

// GlobalResolver returns the singleton device resolver for the local daemon.
func GlobalResolver() *DeviceResolver {
	globalResolverOnce.Do(func() {
		globalResolver = NewDeviceResolver()
	})
	return globalResolver
}

// SetGracePeriod configures the damped teardown grace period.
func (r *DeviceResolver) SetGracePeriod(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gracePeriod = d
}

// GracePeriod returns the currently configured teardown grace period.
func (r *DeviceResolver) GracePeriod() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gracePeriod
}

// ActiveControllerCount returns the count of active directional controllers.
func (r *DeviceResolver) ActiveControllerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.controllers)
}

// Register registers or updates a device and registers legacy aliases.
func (r *DeviceResolver) Register(dev Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerLocked(dev)
}

func (r *DeviceResolver) registerLocked(dev Device) {
	if dev.URN == "" {
		if dev.ID != "" {
			dev.URN = dev.ID
		} else {
			dev.URN = urn.NewName(dev.Name)
		}
	}
	dev.ID = dev.URN

	if r.quarantined[dev.URN] {
		dev.Status = "quarantined"
	} else if dev.Status == "" {
		dev.Status = "active"
	}

	r.devices[dev.URN] = dev
	log.Printf("[AUDIO:DEV] Registered device URN=%q name=%q runtimeID=%d legacyID=%q status=%s", dev.URN, dev.Name, dev.RuntimeID, dev.LegacyID, dev.Status)

	// 1. PortAudio index aliases
	if dev.LegacyID != "" {
		r.aliases[dev.LegacyID] = dev.URN
	}
	r.aliases[strconv.Itoa(dev.RuntimeID)] = dev.URN

	// 2. Exact ALSA card/subdevice aliases from device name (hw:C,D)
	if matches := alsaNameRegex.FindStringSubmatch(dev.Name); len(matches) >= 2 {
		cIdx := matches[1]
		dIdx := "0"
		if len(matches) >= 3 && matches[2] != "" {
			dIdx = matches[2]
		}
		if dIdx == "0" {
			r.aliases[fmt.Sprintf("hw:%s", cIdx)] = dev.URN
		}
		r.aliases[fmt.Sprintf("hw:%s,%s", cIdx, dIdx)] = dev.URN
	}

	// 3. Fallback ALSA aliases on runtime index
	r.aliases[fmt.Sprintf("hw:%d", dev.RuntimeID)] = dev.URN
	r.aliases[fmt.Sprintf("hw:%d,0", dev.RuntimeID)] = dev.URN

	// 4. Human-readable slug alias
	if dev.Name != "" {
		r.aliases[urn.Slugify(dev.Name)] = dev.URN
	}
}

// Refresh re-enumerates host devices and updates registration while preserving quarantine states.
func (r *DeviceResolver) Refresh() error {
	devs, err := ListDevices()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	activeSet := make(map[string]struct{}, len(devs))
	for _, d := range devs {
		r.registerLocked(d)
		activeSet[d.URN] = struct{}{}
	}

	// Mark previously known physical devices that vanished as quarantined
	for urnKey, dev := range r.devices {
		if dev.IsVirtual {
			continue
		}
		if _, stillPresent := activeSet[urnKey]; !stillPresent {
			r.quarantined[urnKey] = true
			dev.Status = "quarantined"
			r.devices[urnKey] = dev

			// Swap any active controllers for this URN to GhostAudioStream
			for key, ctrl := range r.controllers {
				if key.URN == urnKey {
					ghost := NewGhostAudioStream(ctrl.sampleRate, ctrl.channels)
					ctrl.swapPhysicalStream(ghost)
				}
			}
		}
	}

	return nil
}

func (r *DeviceResolver) resolveLocked(ref string) (Device, bool) {
	ref = strings.TrimSpace(ref)

	// Direct URN match
	if dev, ok := r.devices[ref]; ok {
		return dev, true
	}

	// Alias match
	if canonicalURN, ok := r.aliases[ref]; ok {
		if dev, found := r.devices[canonicalURN]; found {
			return dev, true
		}
	}

	// Dynamic fallback for "hw:X,Y" or "hw:X"
	for _, dev := range r.devices {
		if ref == strconv.Itoa(dev.RuntimeID) ||
			ref == fmt.Sprintf("hw:%d", dev.RuntimeID) ||
			ref == fmt.Sprintf("hw:%d,0", dev.RuntimeID) ||
			ref == dev.LegacyID {
			return dev, true
		}
	}

	return Device{}, false
}

// Resolve looks up a device by canonical URN or legacy alias.
func (r *DeviceResolver) Resolve(ref string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveLocked(ref)
}

// ResolveInput resolves a device reference specifically ensuring it can capture audio.
// If the resolved device has 0 input channels, it checks for a capture-capable sibling.
func (r *DeviceResolver) ResolveInput(ref string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ref = strings.TrimSpace(ref)
	dev, found := r.resolveLocked(ref)
	if !found || dev.MaxInput == 0 {
		if sibling, ok := r.findSiblingDeviceLocked(ref, true); ok {
			return sibling, true
		}
	}
	return dev, found
}

// ResolveOutput resolves a device reference specifically ensuring it can play audio.
// If the resolved device has 0 output channels, it checks for a playback-capable sibling.
func (r *DeviceResolver) ResolveOutput(ref string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ref = strings.TrimSpace(ref)
	dev, found := r.resolveLocked(ref)
	if !found || dev.MaxOutput == 0 {
		if sibling, ok := r.findSiblingDeviceLocked(ref, false); ok {
			return sibling, true
		}
	}
	return dev, found
}

// Quarantine marks a device as disconnected/quarantined.
// If an active controller exists, swaps its physical stream to GhostAudioStream.
func (r *DeviceResolver) Quarantine(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	devURN := ref
	if mapped, ok := r.aliases[ref]; ok {
		devURN = mapped
	}

	if dev, ok := r.devices[devURN]; ok {
		r.quarantined[devURN] = true
		dev.Status = "quarantined"
		r.devices[devURN] = dev
	} else {
		r.quarantined[devURN] = true
	}

	delete(r.reboundStreams, StreamKey{URN: devURN, Direction: DirectionInput})
	delete(r.reboundStreams, StreamKey{URN: devURN, Direction: DirectionOutput})

	for key, ctrl := range r.controllers {
		if key.URN == devURN {
			ghost := NewGhostAudioStream(ctrl.sampleRate, ctrl.channels)
			ctrl.swapPhysicalStream(ghost)
		}
	}
}

// Activate marks a quarantined device as active.
func (r *DeviceResolver) Activate(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	devURN := ref
	if mapped, ok := r.aliases[ref]; ok {
		devURN = mapped
	}

	delete(r.quarantined, devURN)
	if dev, ok := r.devices[devURN]; ok {
		dev.Status = "active"
		r.devices[devURN] = dev
	}
}

// IsQuarantined checks if a device is currently quarantined.
func (r *DeviceResolver) IsQuarantined(ref string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	devURN := ref
	if mapped, ok := r.aliases[ref]; ok {
		devURN = mapped
	}
	return r.quarantined[devURN]
}

func (r *DeviceResolver) findSiblingDeviceLocked(ref string, wantInput bool) (Device, bool) {
	idx := strings.LastIndex(ref, ":d=")
	if idx == -1 {
		return Device{}, false
	}
	base := ref[:idx]
	for _, dev := range r.devices {
		if strings.HasPrefix(dev.URN, base+":d=") {
			if wantInput && dev.MaxInput > 0 {
				return dev, true
			}
			if !wantInput && dev.MaxOutput > 0 {
				return dev, true
			}
		}
	}
	return Device{}, false
}

// OpenInput resolves the device identifier and opens an input stream handle.
// If the device is quarantined, disconnected, or fails to open, a RefStreamHandle
// wrapping GhostAudioStream is returned.
func (r *DeviceResolver) OpenInput(ref string, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	log.Printf("[AUDIO:DEV] OpenInput called: ref=%q sampleRate=%d channels=%d emulator=%t", ref, sampleRate, channels, useEmulator)
	dev, found := r.Resolve(ref)
	if !found || dev.MaxInput == 0 {
		r.mu.Lock()
		if sibling, ok := r.findSiblingDeviceLocked(ref, true); ok {
			if !strings.Contains(ref, ":d=") {
				r.aliases[ref] = sibling.URN
			}
			dev = sibling
			found = true
			log.Printf("[AUDIO:DEV] Sibling auto-migration: ref=%q has no input channels, migrating to capture sibling URN=%q (runtimeID=%d)", ref, sibling.URN, sibling.RuntimeID)
		}
		r.mu.Unlock()
	}

	if !found {
		if useEmulator {
			devID := 0
			if id, err := strconv.Atoi(ref); err == nil {
				devID = id
			}
			r.Register(Device{
				ID:        ref,
				URN:       ref,
				RuntimeID: devID,
				Name:      fmt.Sprintf("Emulator Device %s", ref),
				MaxInput:  channels,
				MaxOutput: channels,
				Status:    "active",
			})
			return r.OpenInput(ref, sampleRate, channels, true)
		}
		log.Printf("[AUDIO:DEV] WARN: device ref=%q not found in resolver catalog. Falling back to GhostAudioStream (silent)", ref)
		r.Quarantine(ref)
		r.Register(Device{
			ID:        ref,
			URN:       ref,
			Name:      fmt.Sprintf("Quarantined Device %s", ref),
			Status:    "quarantined",
		})
		return r.OpenInput(ref, sampleRate, channels, useEmulator)
	}

	key := StreamKey{URN: dev.URN, Direction: DirectionInput}

	r.mu.Lock()
	defer r.mu.Unlock()

	if ctrl, ok := r.controllers[key]; ok {
		ctrl.mu.Lock()
		if !ctrl.closed {
			if ctrl.graceTimer != nil {
				ctrl.graceTimer.Stop()
				ctrl.graceTimer = nil
			}
			ctrl.refCount++
			handle := ctrl.newInputHandleLocked()
			ctrl.mu.Unlock()
			log.Printf("[AUDIO:DEV] SUCCESS: reusing active input stream controller for URN=%q refCount=%d", dev.URN, ctrl.refCount)
			return handle, nil
		}
		ctrl.mu.Unlock()
	}

	// Prepare physical stream
	var phys AudioStream
	if rebound, ok := r.reboundStreams[key]; ok && rebound != nil {
		phys = rebound
		if dev.IsVirtual {
			if ro, ok := phys.(reopener); ok {
				ro.Reopen()
			}
		} else {
			delete(r.reboundStreams, key)
		}
		log.Printf("[AUDIO:DEV] Using rebound physical input stream for URN=%q", dev.URN)
	} else if dev.IsVirtual {
		log.Printf("[AUDIO:DEV] WARN: virtual device URN=%q has no physical stream, using GhostAudioStream", dev.URN)
		phys = NewGhostAudioStream(sampleRate, channels)
	} else if dev.Status == "quarantined" || dev.Status == "disconnected" || r.quarantined[dev.URN] {
		log.Printf("[AUDIO:DEV] WARN: device URN=%q status is %q. Returning GhostAudioStream handle (silent)", dev.URN, dev.Status)
		phys = NewGhostAudioStream(sampleRate, channels)
	} else {
		log.Printf("[AUDIO:DEV] Resolved input ref=%q to device URN=%q runtimeID=%d (Name=%q)", ref, dev.URN, dev.RuntimeID, dev.Name)
		st, err := OpenInput(dev.RuntimeID, sampleRate, channels, useEmulator)
		if err != nil {
			log.Printf("[AUDIO:DEV] ERROR: failed to open hardware input URN=%q runtimeID=%d: %v", dev.URN, dev.RuntimeID, err)
			return nil, err
		}
		phys = st
	}

	gp := r.gracePeriod
	if gp <= 0 {
		gp = defaultGracePeriod
	}

	ctrl := newDeviceStreamController(r, key, phys, sampleRate, channels, gp)
	r.controllers[key] = ctrl

	ctrl.mu.Lock()
	ctrl.refCount = 1
	handle := ctrl.newInputHandleLocked()
	ctrl.mu.Unlock()

	go ctrl.captureWorker()

	log.Printf("[AUDIO:DEV] SUCCESS: hardware input stream controller online for URN=%q", dev.URN)
	return handle, nil
}

// OpenOutput resolves the device identifier and opens an output stream handle.
// If the device is quarantined, disconnected, or fails to open, a RefStreamHandle
// wrapping GhostAudioStream is returned.
func (r *DeviceResolver) OpenOutput(ref string, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	log.Printf("[AUDIO:DEV] OpenOutput called: ref=%q sampleRate=%d channels=%d emulator=%t", ref, sampleRate, channels, useEmulator)
	dev, found := r.Resolve(ref)
	if !found || dev.MaxOutput == 0 {
		r.mu.Lock()
		if sibling, ok := r.findSiblingDeviceLocked(ref, false); ok {
			if !strings.Contains(ref, ":d=") {
				r.aliases[ref] = sibling.URN
			}
			dev = sibling
			found = true
			log.Printf("[AUDIO:DEV] Sibling auto-migration: ref=%q has no output channels, migrating to playback sibling URN=%q (runtimeID=%d)", ref, sibling.URN, sibling.RuntimeID)
		}
		r.mu.Unlock()
	}

	if !found {
		if useEmulator {
			devID := 1
			if id, err := strconv.Atoi(ref); err == nil {
				devID = id
			}
			r.Register(Device{
				ID:        ref,
				URN:       ref,
				RuntimeID: devID,
				Name:      fmt.Sprintf("Emulator Device %s", ref),
				MaxInput:  channels,
				MaxOutput: channels,
				Status:    "active",
			})
			return r.OpenOutput(ref, sampleRate, channels, true)
		}
		log.Printf("[AUDIO:DEV] WARN: device ref=%q not found in resolver catalog. Falling back to GhostAudioStream (silent)", ref)
		r.Quarantine(ref)
		r.Register(Device{
			ID:        ref,
			URN:       ref,
			Name:      fmt.Sprintf("Quarantined Device %s", ref),
			Status:    "quarantined",
		})
		return r.OpenOutput(ref, sampleRate, channels, useEmulator)
	}

	key := StreamKey{URN: dev.URN, Direction: DirectionOutput}

	r.mu.Lock()
	defer r.mu.Unlock()

	if ctrl, ok := r.controllers[key]; ok {
		ctrl.mu.Lock()
		if !ctrl.closed {
			if ctrl.graceTimer != nil {
				ctrl.graceTimer.Stop()
				ctrl.graceTimer = nil
			}
			ctrl.refCount++
			handle := ctrl.newOutputHandleLocked()
			ctrl.mu.Unlock()
			log.Printf("[AUDIO:DEV] SUCCESS: reusing active output stream controller for URN=%q refCount=%d", dev.URN, ctrl.refCount)
			return handle, nil
		}
		ctrl.mu.Unlock()
	}

	// Prepare physical stream
	var phys AudioStream
	if rebound, ok := r.reboundStreams[key]; ok && rebound != nil {
		phys = rebound
		if dev.IsVirtual {
			if ro, ok := phys.(reopener); ok {
				ro.Reopen()
			}
		} else {
			delete(r.reboundStreams, key)
		}
		log.Printf("[AUDIO:DEV] Using rebound physical output stream for URN=%q", dev.URN)
	} else if dev.IsVirtual {
		log.Printf("[AUDIO:DEV] WARN: virtual device URN=%q has no physical stream, using GhostAudioStream", dev.URN)
		phys = NewGhostAudioStream(sampleRate, channels)
	} else if dev.Status == "quarantined" || dev.Status == "disconnected" || r.quarantined[dev.URN] {
		log.Printf("[AUDIO:DEV] WARN: device URN=%q status is %q. Returning GhostAudioStream handle (silent)", dev.URN, dev.Status)
		phys = NewGhostAudioStream(sampleRate, channels)
	} else {
		log.Printf("[AUDIO:DEV] Resolved output ref=%q to device URN=%q runtimeID=%d (Name=%q)", ref, dev.URN, dev.RuntimeID, dev.Name)
		st, err := OpenOutput(dev.RuntimeID, sampleRate, channels, useEmulator)
		if err != nil {
			log.Printf("[AUDIO:DEV] ERROR: failed to open hardware output URN=%q runtimeID=%d: %v", dev.URN, dev.RuntimeID, err)
			return nil, err
		}
		phys = st
	}

	gp := r.gracePeriod
	if gp <= 0 {
		gp = defaultGracePeriod
	}

	ctrl := newDeviceStreamController(r, key, phys, sampleRate, channels, gp)
	r.controllers[key] = ctrl

	ctrl.mu.Lock()
	ctrl.refCount = 1
	handle := ctrl.newOutputHandleLocked()
	ctrl.mu.Unlock()

	go ctrl.playbackMixerWorker()

	log.Printf("[AUDIO:DEV] SUCCESS: hardware output stream controller online for URN=%q", dev.URN)
	return handle, nil
}

// RebindDirectional atomically swaps out the underlying physical stream for the active
// controller of the given direction targeting ref, or stores it for subsequent OpenInput/OpenOutput.
func (r *DeviceResolver) RebindDirectional(ref string, dir StreamDirection, physicalStream AudioStream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rebindDirectionalLocked(ref, dir, physicalStream)
}

func (r *DeviceResolver) rebindDirectionalLocked(ref string, dir StreamDirection, physicalStream AudioStream) {
	devURN := ref
	if mapped, ok := r.aliases[ref]; ok {
		devURN = mapped
	}

	delete(r.quarantined, devURN)
	if dev, ok := r.devices[devURN]; ok {
		dev.Status = "active"
		r.devices[devURN] = dev
	}

	key := StreamKey{URN: devURN, Direction: dir}
	if ctrl, ok := r.controllers[key]; ok {
		ctrl.swapPhysicalStream(physicalStream)
	}
	r.reboundStreams[key] = physicalStream
}

// Rebind atomically swaps out the underlying physical stream for any active
// controller(s) targeting ref without breaking active RefStreamHandles. If no
// controller is currently active, it stores the stream for future OpenInput/OpenOutput.
func (r *DeviceResolver) Rebind(ref string, physicalStream AudioStream) {
	r.mu.Lock()
	defer r.mu.Unlock()

	devURN := ref
	if mapped, ok := r.aliases[ref]; ok {
		devURN = mapped
	}

	delete(r.quarantined, devURN)
	if dev, ok := r.devices[devURN]; ok {
		dev.Status = "active"
		r.devices[devURN] = dev
	}

	// Determine direction if physicalStream is direction-specific
	isInputOnly := false
	isOutputOnly := false
	switch physicalStream.(type) {
	case *emulatorInput, *portaudioInput:
		isInputOnly = true
	case *emulatorOutput, *portaudioOutput:
		isOutputOnly = true
	}

	if isInputOnly {
		r.rebindDirectionalLocked(devURN, DirectionInput, physicalStream)
		return
	}
	if isOutputOnly {
		r.rebindDirectionalLocked(devURN, DirectionOutput, physicalStream)
		return
	}

	// If device metadata indicates it only has input or only output
	if dev, found := r.devices[devURN]; found {
		if dev.MaxInput > 0 && dev.MaxOutput == 0 {
			r.rebindDirectionalLocked(devURN, DirectionInput, physicalStream)
			return
		}
		if dev.MaxOutput > 0 && dev.MaxInput == 0 {
			r.rebindDirectionalLocked(devURN, DirectionOutput, physicalStream)
			return
		}
	}

	for key, ctrl := range r.controllers {
		if key.URN == devURN {
			ctrl.swapPhysicalStream(physicalStream)
		}
	}

	r.reboundStreams[StreamKey{URN: devURN, Direction: DirectionInput}] = physicalStream
	r.reboundStreams[StreamKey{URN: devURN, Direction: DirectionOutput}] = physicalStream
}

// ListDevices returns all registered devices with their current statuses.
func (r *DeviceResolver) ListDevices() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		if r.quarantined[d.URN] {
			d.Status = "quarantined"
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsVirtual != out[j].IsVirtual {
			return !out[i].IsVirtual
		}
		if out[i].RuntimeID != out[j].RuntimeID {
			return out[i].RuntimeID < out[j].RuntimeID
		}
		return out[i].URN < out[j].URN
	})
	return out
}

// Close closes all active controllers, canceling grace timers, and closing underlying streams.
func (r *DeviceResolver) Close() error {
	r.mu.Lock()
	ctrls := make([]*deviceStreamController, 0, len(r.controllers))
	for _, ctrl := range r.controllers {
		ctrls = append(ctrls, ctrl)
	}
	r.controllers = make(map[StreamKey]*deviceStreamController)
	rebound := r.reboundStreams
	r.reboundStreams = make(map[StreamKey]AudioStream)
	r.mu.Unlock()

	var firstErr error
	for _, ctrl := range ctrls {
		if err := ctrl.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, st := range rebound {
		if st != nil {
			if err := st.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// inputSubscriber manages an isolated FIFO ring buffer for one capture handle.
type inputSubscriber struct {
	mu        sync.Mutex
	cond      *sync.Cond
	buf       *ringBuf
	closed    bool
	dropCount atomic.Uint64
	lastLog   atomic.Int64
}

func newInputSubscriber(frameSize int) *inputSubscriber {
	s := &inputSubscriber{
		buf: newRingBuf(64*1024, frameSize),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *inputSubscriber) push(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	avail := s.buf.cap() - s.buf.n
	if len(data) > avail {
		droppedBytes := len(data) - avail
		s.dropCount.Add(uint64(droppedBytes))
		now := time.Now().UnixNano()
		last := s.lastLog.Load()
		if now-last > int64(time.Second) && s.lastLog.CompareAndSwap(last, now) {
			log.Printf("[AUDIO:DEV] Capture subscriber ring buffer overrun: dropped %d bytes", droppedBytes)
		}
	}
	s.buf.write(data)
	s.cond.Broadcast()
}

// outputSubscriber manages a buffered frame queue for one playback handle.
type outputSubscriber struct {
	mu     sync.Mutex
	buf    *ringBuf
	closed bool
}

func newOutputSubscriber(frameSize int) *outputSubscriber {
	return &outputSubscriber{
		buf: newRingBuf(64*1024, frameSize),
	}
}

// deviceStreamController coordinates hardware I/O, reference counting,
// fan-out capture broadcasting, soft-saturated playback mixing, and damped teardown.
type deviceStreamController struct {
	resolver    *DeviceResolver
	key         StreamKey
	sampleRate  int
	channels    int
	frameSize   int

	mu          sync.Mutex
	refCount    int
	closed      bool
	closeCh     chan struct{}
	closeOnce   sync.Once
	gracePeriod time.Duration
	graceTimer  *time.Timer

	// Underlying physical stream (protected by mu)
	physical AudioStream

	// Capture Broadcaster state
	subscribers map[uint64]*inputSubscriber
	nextSubID   uint64

	// Playback Mixer state
	outSubs   map[uint64]*outputSubscriber
	nextOutID uint64
	writeMu   sync.Mutex // Serializes direct fast-path writes
	mixerCond *sync.Cond
}

func newDeviceStreamController(r *DeviceResolver, key StreamKey, phys AudioStream, sampleRate, channels int, gracePeriod time.Duration) *deviceStreamController {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if channels <= 0 {
		channels = 2
	}
	c := &deviceStreamController{
		resolver:    r,
		key:         key,
		sampleRate:  sampleRate,
		channels:    channels,
		frameSize:   channels * 2,
		closeCh:     make(chan struct{}),
		gracePeriod: gracePeriod,
		physical:    phys,
		subscribers: make(map[uint64]*inputSubscriber),
		outSubs:     make(map[uint64]*outputSubscriber),
	}
	c.mixerCond = sync.NewCond(&c.mu)
	return c
}

func (c *deviceStreamController) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *deviceStreamController) getPhysicalStream() AudioStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.physical
}

func (c *deviceStreamController) swapPhysicalStream(newStream AudioStream) {
	c.mu.Lock()
	old := c.physical
	c.physical = newStream
	c.mu.Unlock()

	if old != nil && old != newStream {
		_ = old.Close()
	}
}

func (c *deviceStreamController) newInputHandleLocked() *RefStreamHandle {
	c.nextSubID++
	id := c.nextSubID
	sub := newInputSubscriber(c.frameSize)
	c.subscribers[id] = sub

	return &RefStreamHandle{
		controller: c,
		dir:        DirectionInput,
		inSubID:    id,
		inSub:      sub,
	}
}

func (c *deviceStreamController) newOutputHandleLocked() *RefStreamHandle {
	c.nextOutID++
	id := c.nextOutID
	sub := newOutputSubscriber(c.frameSize)
	c.outSubs[id] = sub

	return &RefStreamHandle{
		controller: c,
		dir:        DirectionOutput,
		outSubID:   id,
		outSub:     sub,
	}
}

func (c *deviceStreamController) unregisterInputSub(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sub, ok := c.subscribers[id]; ok {
		delete(c.subscribers, id)
		sub.mu.Lock()
		sub.closed = true
		sub.cond.Broadcast()
		sub.mu.Unlock()
	}
}

func (c *deviceStreamController) unregisterOutputSub(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sub, ok := c.outSubs[id]; ok {
		delete(c.outSubs, id)
		sub.mu.Lock()
		sub.closed = true
		sub.mu.Unlock()
		if c.mixerCond != nil {
			c.mixerCond.Broadcast()
		}
	}
}

func (c *deviceStreamController) releaseHandle(h *RefStreamHandle) {
	if h.dir == DirectionInput {
		c.unregisterInputSub(h.inSubID)
	} else if h.dir == DirectionOutput {
		c.unregisterOutputSub(h.outSubID)
	}

	c.mu.Lock()
	c.refCount--
	if c.refCount <= 0 {
		c.refCount = 0
		if !c.closed && c.graceTimer == nil {
			c.graceTimer = time.AfterFunc(c.gracePeriod, func() {
				c.teardown()
			})
		}
	}
	c.mu.Unlock()
}

func (c *deviceStreamController) teardown() {
	c.resolver.mu.Lock()
	c.mu.Lock()
	if c.refCount > 0 || c.closed {
		c.mu.Unlock()
		c.resolver.mu.Unlock()
		return
	}

	c.closed = true
	if c.resolver.controllers[c.key] == c {
		delete(c.resolver.controllers, c.key)
	}
	dev, isDev := c.resolver.devices[c.key.URN]
	isVirtual := isDev && dev.IsVirtual
	if !isVirtual {
		delete(c.resolver.reboundStreams, c.key)
	}
	c.closeOnce.Do(func() { close(c.closeCh) })
	phys := c.physical
	c.physical = nil

	// Wake all subscribers
	for _, sub := range c.subscribers {
		sub.mu.Lock()
		sub.closed = true
		sub.cond.Broadcast()
		sub.mu.Unlock()
	}
	for _, sub := range c.outSubs {
		sub.mu.Lock()
		sub.closed = true
		sub.mu.Unlock()
	}
	if c.mixerCond != nil {
		c.mixerCond.Broadcast()
	}
	c.mu.Unlock()
	c.resolver.mu.Unlock()

	if phys != nil {
		_ = phys.Close()
	}
}

func (c *deviceStreamController) close() error {
	c.mu.Lock()
	if c.graceTimer != nil {
		c.graceTimer.Stop()
		c.graceTimer = nil
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.closeOnce.Do(func() { close(c.closeCh) })
	phys := c.physical
	c.physical = nil

	for _, sub := range c.subscribers {
		sub.mu.Lock()
		sub.closed = true
		sub.cond.Broadcast()
		sub.mu.Unlock()
	}
	for _, sub := range c.outSubs {
		sub.mu.Lock()
		sub.closed = true
		sub.mu.Unlock()
	}
	if c.mixerCond != nil {
		c.mixerCond.Broadcast()
	}
	c.mu.Unlock()

	if phys != nil {
		return phys.Close()
	}
	return nil
}

// captureWorker continuously reads from the physical capture stream and
// broadcasts PCM frames to all subscribed handles' ring buffers.
func (c *deviceStreamController) captureWorker() {
	frameSize := c.frameSize
	if frameSize <= 0 {
		frameSize = 4
	}
	bufSize := 2048 * frameSize
	if bufSize < 8192 {
		bufSize = 8192
	}
	readBuf := make([]byte, bufSize)

	for {
		if c.isClosed() {
			return
		}
		phys := c.getPhysicalStream()
		if phys == nil {
			select {
			case <-c.closeCh:
				return
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		n, err := phys.Read(readBuf)
		if c.isClosed() {
			return
		}

		if err != nil {
			// Did stream swap beneath us?
			if c.getPhysicalStream() != phys {
				continue
			}
			if err == io.EOF {
				select {
				case <-c.closeCh:
					return
				case <-time.After(10 * time.Millisecond):
					continue
				}
			}
			select {
			case <-c.closeCh:
				return
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		if n > 0 {
			c.broadcastCapture(readBuf[:n])
		}
	}
}

func (c *deviceStreamController) broadcastCapture(data []byte) {
	c.mu.Lock()
	if c.closed || len(c.subscribers) == 0 {
		c.mu.Unlock()
		return
	}
	var stackSubs [4]*inputSubscriber
	var subs []*inputSubscriber
	if len(c.subscribers) <= len(stackSubs) {
		subs = stackSubs[:0]
	} else {
		subs = make([]*inputSubscriber, 0, len(c.subscribers))
	}
	for _, sub := range c.subscribers {
		subs = append(subs, sub)
	}
	c.mu.Unlock()

	for _, sub := range subs {
		sub.push(data)
	}
}

// writeOutput handles output frame writing with fast-path optimization for single
// writers and multi-channel summing with soft saturation for multiple writers.
func (c *deviceStreamController) writeOutput(h *RefStreamHandle, p []byte) (int, error) {
	if c.isClosed() {
		return 0, io.ErrClosedPipe
	}

	c.mu.Lock()
	count := len(c.outSubs)
	c.mu.Unlock()

	if count <= 1 {
		// Fast-path direct write
		c.writeMu.Lock()
		defer c.writeMu.Unlock()

		if c.isClosed() {
			return 0, io.ErrClosedPipe
		}

		// If subscriber has leftover buffered frames, drain them first
		if h.outSub != nil {
			h.outSub.mu.Lock()
			if h.outSub.closed {
				h.outSub.mu.Unlock()
				return 0, io.ErrClosedPipe
			}
			if h.outSub.buf.n > 0 {
				var drainBuf [4096]byte
				for h.outSub.buf.n > 0 {
					rn := h.outSub.buf.read(drainBuf[:])
					if rn <= 0 {
						break
					}
					h.outSub.mu.Unlock()
					phys := c.getPhysicalStream()
					if phys != nil {
						_, _ = phys.Write(drainBuf[:rn])
					}
					h.outSub.mu.Lock()
				}
			}
			h.outSub.mu.Unlock()
		}

		phys := c.getPhysicalStream()
		if phys == nil {
			return 0, io.ErrClosedPipe
		}
		return phys.Write(p)
	}

	// Multi-writer mixer path
	if h.outSub == nil {
		return 0, io.ErrClosedPipe
	}
	h.outSub.mu.Lock()
	if h.outSub.closed {
		h.outSub.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	h.outSub.buf.write(p)
	h.outSub.mu.Unlock()

	c.mu.Lock()
	if c.mixerCond != nil {
		c.mixerCond.Signal()
	}
	c.mu.Unlock()

	return len(p), nil
}

// playbackMixerWorker sums frames from multiple active output handles,
// applying soft-knee saturation to prevent digital wrap-around clipping.
func (c *deviceStreamController) playbackMixerWorker() {
	frameSize := c.frameSize
	if frameSize <= 0 {
		frameSize = 4
	}
	chans := c.channels
	if chans <= 0 {
		chans = 2
	}

	const maxMixBytes = 4096
	tempBuf := make([]byte, maxMixBytes)
	outBuf := make([]byte, maxMixBytes)
	accum := make([]float64, maxMixBytes/2)

	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}

		// Fast-path handles 0 or 1 writers directly; worker rests
		if len(c.outSubs) <= 1 {
			c.mixerCond.Wait()
			if c.closed {
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
			continue
		}

		hasData := false
		maxAvail := 0
		for _, sub := range c.outSubs {
			sub.mu.Lock()
			if sub.buf.n > 0 {
				hasData = true
				if sub.buf.n > maxAvail {
					maxAvail = sub.buf.n
				}
			}
			sub.mu.Unlock()
		}

		if !hasData || maxAvail == 0 {
			c.mixerCond.Wait()
			if c.closed {
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
			continue
		}

		mixLen := maxAvail
		if mixLen > maxMixBytes {
			mixLen = maxMixBytes
		}
		mixLen -= mixLen % frameSize
		if mixLen == 0 {
			c.mu.Unlock()
			continue
		}

		numFrames := mixLen / frameSize
		numSamples := numFrames * chans
		clear(accum[:numSamples])

		for _, sub := range c.outSubs {
			sub.mu.Lock()
			n := sub.buf.read(tempBuf[:mixLen])
			sub.mu.Unlock()

			framesRead := n / frameSize
			for f := 0; f < framesRead; f++ {
				for ch := 0; ch < chans; ch++ {
					sampleOffset := (f*chans + ch) * 2
					sampleVal := int16(binary.LittleEndian.Uint16(tempBuf[sampleOffset : sampleOffset+2]))
					accum[f*chans+ch] += float64(sampleVal)
				}
			}
		}

		for f := 0; f < numFrames; f++ {
			for ch := 0; ch < chans; ch++ {
				val := accum[f*chans+ch]
				norm := val / 32768.0
				sat := saturateFloat(norm)
				clamped := sat * 32767.0
				if clamped > 32767.0 {
					clamped = 32767.0
				} else if clamped < -32768.0 {
					clamped = -32768.0
				}
				sampleOut := int16(math.Round(clamped))
				destOffset := (f*chans + ch) * 2
				binary.LittleEndian.PutUint16(outBuf[destOffset:destOffset+2], uint16(sampleOut))
			}
		}

		phys := c.physical
		c.mu.Unlock()

		if phys != nil {
			c.writeMu.Lock()
			_, _ = phys.Write(outBuf[:mixLen])
			c.writeMu.Unlock()
		}
	}
}

// saturateFloat applies a smooth soft-knee compression curve using tanh for
// signal levels exceeding 0.8 to prevent digital wrap-around clipping.
func saturateFloat(normalized float64) float64 {
	const threshold = 0.8
	abs := math.Abs(normalized)
	if abs <= threshold {
		return normalized
	}
	excess := abs - threshold
	compressed := threshold + (1.0-threshold)*math.Tanh(excess/(1.0-threshold))
	if normalized < 0 {
		return -compressed
	}
	return compressed
}

// RefStreamHandle is a reference-counted virtual handle implementing AudioStream.
// It proxies I/O to a deviceStreamController, allowing transparent capture fan-out,
// playback soft-mixing, hot-swap rebinding, and damped driver teardown.
type RefStreamHandle struct {
	controller *deviceStreamController
	dir        StreamDirection
	inSubID    uint64
	inSub      *inputSubscriber
	outSubID   uint64
	outSub     *outputSubscriber
	closed     atomic.Bool
}

var _ AudioStream = (*RefStreamHandle)(nil)
var _ BufferedStream = (*RefStreamHandle)(nil)

// Read drains PCM frames from the handle's dedicated capture buffer.
// If the handle or controller is closed, it returns io.EOF.
func (h *RefStreamHandle) Read(p []byte) (int, error) {
	if h.closed.Load() {
		return 0, io.EOF
	}
	if h.dir != DirectionInput || h.inSub == nil {
		return 0, io.EOF
	}

	sub := h.inSub
	sub.mu.Lock()
	defer sub.mu.Unlock()

	for sub.buf.n == 0 && !sub.closed && !h.closed.Load() {
		sub.cond.Wait()
	}

	if h.closed.Load() {
		return 0, io.EOF
	}
	if sub.buf.n == 0 && sub.closed {
		return 0, io.EOF
	}

	n := sub.buf.read(p)
	return n, nil
}

// Write pushes PCM frames to the output controller.
func (h *RefStreamHandle) Write(p []byte) (int, error) {
	if h.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if h.dir != DirectionOutput {
		return 0, io.ErrClosedPipe
	}
	return h.controller.writeOutput(h, p)
}

// Buffered returns the number of bytes currently buffered in the handle.
func (h *RefStreamHandle) Buffered() int {
	if h.closed.Load() {
		return 0
	}
	if h.dir == DirectionOutput {
		if h.outSub != nil {
			h.outSub.mu.Lock()
			n := h.outSub.buf.n
			h.outSub.mu.Unlock()
			if n > 0 {
				return n
			}
		}
		if phys := h.controller.getPhysicalStream(); phys != nil {
			return OutputBufferSize(phys)
		}
		return 0
	}
	if h.dir == DirectionInput && h.inSub != nil {
		h.inSub.mu.Lock()
		defer h.inSub.mu.Unlock()
		return h.inSub.buf.n
	}
	return 0
}

// Underlying returns the active physical stream managed by this handle's controller.
func (h *RefStreamHandle) Underlying() AudioStream {
	if h.controller == nil {
		return nil
	}
	return h.controller.getPhysicalStream()
}

// Direction returns the StreamDirection of this handle.
func (h *RefStreamHandle) Direction() StreamDirection {
	return h.dir
}

// Close releases the virtual handle, decrementing the controller ref-count
// and initiating a 500ms damped teardown timer if this was the last reference.
func (h *RefStreamHandle) Close() error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	h.controller.releaseHandle(h)
	return nil
}
