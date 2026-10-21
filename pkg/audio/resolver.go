package audio

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/audio-router-p2p/pkg/audio/urn"
)

var alsaNameRegex = regexp.MustCompile(`\(hw:(\d+)(?:,(\d+))?\)`)

// DeviceResolver maps canonical URNs and legacy aliases ("0", "1", "hw:0,0")
// to Device metadata and audio streams, managing ghost device transitions.
type DeviceResolver struct {
	mu          sync.RWMutex
	devices     map[string]Device      // Keyed by canonical URN
	aliases     map[string]string      // Alias -> Canonical URN
	quarantined map[string]bool        // URN -> isQuarantined
	streams     map[string]AudioStream // URN -> active stream
}

// NewDeviceResolver creates a thread-safe device resolver.
func NewDeviceResolver() *DeviceResolver {
	r := &DeviceResolver{
		devices:     make(map[string]Device),
		aliases:     make(map[string]string),
		quarantined: make(map[string]bool),
		streams:     make(map[string]AudioStream),
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
		r.aliases[fmt.Sprintf("hw:%s", cIdx)] = dev.URN
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
			if st, ok := r.streams[urnKey]; ok && st != nil {
				_ = st.Close()
				delete(r.streams, urnKey)
			}
		}
	}

	return nil
}

// Resolve looks up a device by canonical URN or legacy alias.
func (r *DeviceResolver) Resolve(ref string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

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

// Quarantine marks a device as disconnected/quarantined.
func (r *DeviceResolver) Quarantine(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	devURN := ref
	if mapped, ok := r.aliases[ref]; ok {
		devURN = mapped
	}

	// Prevent memory leak: only quarantine known devices
	if dev, ok := r.devices[devURN]; ok {
		r.quarantined[devURN] = true
		dev.Status = "quarantined"
		r.devices[devURN] = dev

		// Terminate underlying physical hardware stream
		if st, ok := r.streams[devURN]; ok && st != nil {
			_ = st.Close()
			delete(r.streams, devURN)
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

// OpenInput resolves the device identifier and opens an input stream.
// If the device is quarantined, disconnected, or fails to open, a GhostAudioStream is returned.
func (r *DeviceResolver) OpenInput(ref string, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	dev, found := r.Resolve(ref)
	if !found || dev.Status == "quarantined" || dev.Status == "disconnected" {
		r.Quarantine(ref)
		ghost := NewGhostAudioStream(sampleRate, channels)
		return ghost, nil
	}

	st, err := OpenInput(dev.RuntimeID, sampleRate, channels, useEmulator)
	if err != nil {
		r.Quarantine(dev.URN)
		ghost := NewGhostAudioStream(sampleRate, channels)
		return ghost, nil
	}

	r.mu.Lock()
	if old, ok := r.streams[dev.URN]; ok && old != nil {
		_ = old.Close()
	}
	r.streams[dev.URN] = st
	r.mu.Unlock()

	return st, nil
}

// OpenOutput resolves the device identifier and opens an output stream.
// If the device is quarantined, disconnected, or fails to open, a GhostAudioStream is returned.
func (r *DeviceResolver) OpenOutput(ref string, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	dev, found := r.Resolve(ref)
	if !found || dev.Status == "quarantined" || dev.Status == "disconnected" {
		r.Quarantine(ref)
		ghost := NewGhostAudioStream(sampleRate, channels)
		return ghost, nil
	}

	st, err := OpenOutput(dev.RuntimeID, sampleRate, channels, useEmulator)
	if err != nil {
		r.Quarantine(dev.URN)
		ghost := NewGhostAudioStream(sampleRate, channels)
		return ghost, nil
	}

	r.mu.Lock()
	if old, ok := r.streams[dev.URN]; ok && old != nil {
		_ = old.Close()
	}
	r.streams[dev.URN] = st
	r.mu.Unlock()

	return st, nil
}

// Rebind swaps out any active stream for the device with a new physical stream and marks it active.
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

	if old, ok := r.streams[devURN]; ok && old != nil {
		_ = old.Close()
	}
	r.streams[devURN] = physicalStream
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
	return out
}
