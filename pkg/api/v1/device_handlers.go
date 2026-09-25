package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/audio-router-p2p/pkg/audio"
)

// DeviceState tracks runtime volume and mute settings for an audio device.
type DeviceState struct {
	Volume float64
	Muted  bool
}

// VirtualDeviceEntry tracks an in-memory loopback device.
type VirtualDeviceEntry struct {
	Device AudioDevice
	Dev    *audio.VirtualAudioDevice
}

// DeviceRegistry abstracts physical sound cards and in-memory virtual loopback interfaces.
type DeviceRegistry struct {
	mu           sync.RWMutex
	virtualDevs  map[string]*VirtualDeviceEntry
	deviceStates map[string]*DeviceState
	nextVirtID   int
}

// NewDeviceRegistry creates a thread-safe device registry.
func NewDeviceRegistry() *DeviceRegistry {
	return &DeviceRegistry{
		virtualDevs:  make(map[string]*VirtualDeviceEntry),
		deviceStates: make(map[string]*DeviceState),
		nextVirtID:   1,
	}
}

// GetSupportedLayouts determines standard channel layouts based on channel count.
func GetSupportedLayouts(channels int) []string {
	if channels >= 8 {
		return []string{"mono", "stereo", "quad", "5.1", "7.1"}
	}
	if channels >= 6 {
		return []string{"mono", "stereo", "quad", "5.1"}
	}
	if channels >= 4 {
		return []string{"mono", "stereo", "quad"}
	}
	if channels >= 2 {
		return []string{"mono", "stereo"}
	}
	return []string{"mono"}
}

// ListDevices returns all physical and virtual devices matching the requested kind filter.
func (reg *DeviceRegistry) ListDevices(kind string) ([]AudioDevice, error) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	var result []AudioDevice

	// 1. Enumerate physical devices
	if kind != "virtual" {
		phys, err := audio.ListDevices()
		if err == nil {
			for _, d := range phys {
				idStr := strconv.Itoa(d.ID)
				state := reg.deviceStates[idStr]
				vol := 1.0
				muted := false
				if state != nil {
					vol = state.Volume
					muted = state.Muted
				}
				maxCh := d.MaxInput
				if d.MaxOutput > maxCh {
					maxCh = d.MaxOutput
				}

				dev := AudioDevice{
					ID:                idStr,
					Name:              d.Name,
					MaxInput:          d.MaxInput,
					MaxOutput:         d.MaxOutput,
					DefaultSampleRate: d.DefaultSampleRate,
					IsVirtual:         false,
					Volume:            vol,
					Muted:             muted,
					SupportedLayouts:  GetSupportedLayouts(maxCh),
				}

				// Apply kind filter
				if kind == "input" && dev.MaxInput == 0 {
					continue
				}
				if kind == "output" && dev.MaxOutput == 0 {
					continue
				}
				result = append(result, dev)
			}
		}
	}

	// 2. Enumerate virtual devices
	if kind == "all" || kind == "virtual" || kind == "input" || kind == "output" {
		for _, vd := range reg.virtualDevs {
			d := vd.Device
			state := reg.deviceStates[d.ID]
			if state != nil {
				d.Volume = state.Volume
				d.Muted = state.Muted
			}
			if kind == "input" && d.MaxInput == 0 {
				continue
			}
			if kind == "output" && d.MaxOutput == 0 {
				continue
			}
			result = append(result, d)
		}
	}

	if result == nil {
		result = []AudioDevice{}
	}
	return result, nil
}

// GetDevice retrieves device metadata by ID.
func (reg *DeviceRegistry) GetDevice(id string) (AudioDevice, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	// Check virtual devices
	if vd, ok := reg.virtualDevs[id]; ok {
		d := vd.Device
		if state := reg.deviceStates[id]; state != nil {
			d.Volume = state.Volume
			d.Muted = state.Muted
		}
		return d, true
	}

	// Check physical devices
	phys, err := audio.ListDevices()
	if err == nil {
		for _, d := range phys {
			idStr := strconv.Itoa(d.ID)
			if idStr == id {
				vol := 1.0
				muted := false
				if state := reg.deviceStates[idStr]; state != nil {
					vol = state.Volume
					muted = state.Muted
				}
				maxCh := d.MaxInput
				if d.MaxOutput > maxCh {
					maxCh = d.MaxOutput
				}
				return AudioDevice{
					ID:                idStr,
					Name:              d.Name,
					MaxInput:          d.MaxInput,
					MaxOutput:         d.MaxOutput,
					DefaultSampleRate: d.DefaultSampleRate,
					IsVirtual:         false,
					Volume:            vol,
					Muted:             muted,
					SupportedLayouts:  GetSupportedLayouts(maxCh),
				}, true
			}
		}
	}

	return AudioDevice{}, false
}

// UpdateDeviceState modifies volume and/or muted state for a device.
func (reg *DeviceRegistry) UpdateDeviceState(id string, vol *float64, muted *bool) (AudioDevice, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	state, ok := reg.deviceStates[id]
	if !ok {
		state = &DeviceState{Volume: 1.0, Muted: false}
		reg.deviceStates[id] = state
	}
	if vol != nil {
		state.Volume = *vol
	}
	if muted != nil {
		state.Muted = *muted
	}

	reg.mu.Unlock()
	dev, found := reg.GetDevice(id)
	reg.mu.Lock()
	return dev, found
}

// CreateVirtualDevice provisions an in-memory loopback device.
func (reg *DeviceRegistry) CreateVirtualDevice(req CreateVirtualDeviceRequest) (AudioDevice, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	virtID := fmt.Sprintf("virt-%d", reg.nextVirtID)
	reg.nextVirtID++

	maxIn := 0
	maxOut := 0
	switch req.Direction {
	case "input":
		maxIn = req.Channels
	case "output":
		maxOut = req.Channels
	case "duplex":
		maxIn = req.Channels
		maxOut = req.Channels
	default:
		return AudioDevice{}, fmt.Errorf("invalid direction: %s", req.Direction)
	}

	dev := AudioDevice{
		ID:                virtID,
		Name:              req.Name,
		MaxInput:          maxIn,
		MaxOutput:         maxOut,
		DefaultSampleRate: float64(req.SampleRate),
		IsVirtual:         true,
		Volume:            1.0,
		Muted:             false,
		SupportedLayouts:  GetSupportedLayouts(req.Channels),
	}

	vDev := audio.NewVirtualAudioDevice()
	reg.virtualDevs[virtID] = &VirtualDeviceEntry{
		Device: dev,
		Dev:    vDev,
	}
	reg.deviceStates[virtID] = &DeviceState{Volume: 1.0, Muted: false}

	return dev, nil
}

// DeleteVirtualDevice removes and closes an in-memory loopback device.
func (reg *DeviceRegistry) DeleteVirtualDevice(id string) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	entry, ok := reg.virtualDevs[id]
	if !ok {
		return false
	}
	entry.Dev.Close()
	delete(reg.virtualDevs, id)
	delete(reg.deviceStates, id)
	return true
}

// handleDevices handles GET /api/v1/devices.
func (s *V1Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "all"
	}
	devs, err := s.devReg.ListDevices(kind)
	if err != nil {
		s.WriteError(w, http.StatusInternalServerError, "ERR_DEVICE_LIST", err.Error())
		return
	}
	s.WriteJSON(w, http.StatusOK, devs)
}

// handleDevicesSubpath handles /api/v1/devices/{deviceId} and /api/v1/devices/virtual.
func (s *V1Server) handleDevicesSubpath(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/devices/")
	if sub == "" {
		s.handleDevices(w, r)
		return
	}

	// Route 1: POST /api/v1/devices/virtual
	if sub == "virtual" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req CreateVirtualDeviceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		if req.Name == "" || req.Channels <= 0 || req.SampleRate <= 0 {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_PARAMS", "missing required fields")
			return
		}
		dev, err := s.devReg.CreateVirtualDevice(req)
		if err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_CREATE_VIRTUAL", err.Error())
			return
		}
		s.WriteJSON(w, http.StatusCreated, dev)
		return
	}

	// Route 2: DELETE /api/v1/devices/virtual/{deviceId}
	if strings.HasPrefix(sub, "virtual/") {
		virtID := strings.TrimPrefix(sub, "virtual/")
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !s.devReg.DeleteVirtualDevice(virtID) {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "virtual device not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Route 3: GET or PATCH /api/v1/devices/{deviceId}
	deviceID := sub
	switch r.Method {
	case http.MethodGet:
		dev, found := s.devReg.GetDevice(deviceID)
		if !found {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "device not found")
			return
		}
		s.WriteJSON(w, http.StatusOK, dev)

	case http.MethodPatch:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req UpdateDeviceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.WriteError(w, http.StatusBadRequest, "ERR_INVALID_BODY", err.Error())
			return
		}
		if req.Volume != nil && (*req.Volume < 0.0 || *req.Volume > 2.0) {
			s.WriteError(w, http.StatusBadRequest, "ERR_OUT_OF_BOUNDS", "volume must be between 0.0 and 2.0")
			return
		}
		dev, found := s.devReg.UpdateDeviceState(deviceID, req.Volume, req.Muted)
		if !found {
			s.WriteError(w, http.StatusNotFound, "ERR_NOT_FOUND", "device not found")
			return
		}
		s.WriteJSON(w, http.StatusOK, dev)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
