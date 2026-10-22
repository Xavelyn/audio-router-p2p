package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// StreamRoute describes how audio from one source device is delivered.
type StreamRoute struct {
	ID              string    `json:"id"`
	Direction       string    `json:"direction,omitempty"` // "receive" for a receiver-only route; otherwise send
	SourceDevice    int       `json:"sourceDevice"`
	SourceDeviceURN string    `json:"sourceDeviceUrn,omitempty"`
	DestDevice      int       `json:"destDevice,omitempty"`
	DestDeviceURN   string    `json:"destDeviceUrn,omitempty"`
	Destinations    []string  `json:"destinations"`
	SampleRate      int       `json:"sampleRate,omitempty"`
	Channels        int       `json:"channels,omitempty"`
	InChannels      int       `json:"inChannels,omitempty"`
	OutChannels     int       `json:"outChannels,omitempty"`
	Coefficients    []float64 `json:"coefficients,omitempty"`
	SlewMS          float64   `json:"slewMs,omitempty"`
	Muted           bool      `json:"muted,omitempty"`

	// Peer binding and negotiated format
	PeerID        string `json:"peerId,omitempty"`        // Bound peer ID (empty = any/unicast)
	PeerAddr      string `json:"peerAddr,omitempty"`      // Peer's stream address (host:port)
	Negotiated    bool   `json:"negotiated,omitempty"`    // Whether format was negotiated
	NegSampleRate int    `json:"negSampleRate,omitempty"` // Negotiated sample rate
	NegChannels   int    `json:"negChannels,omitempty"`   // Negotiated channels
	NegCodec      string `json:"negCodec,omitempty"`      // Negotiated codec (e.g., "pcm16")

	// LatencyPreset is the room latency preset applied to this route's
	// playout timing (low, balanced, reliable). Empty = balanced.
	LatencyPreset string `json:"latencyPreset,omitempty"`
}

// Router is a persistent, file-backed store of StreamRoutes.
type Router struct {
	mu           sync.RWMutex
	file         string
	routes       map[string]StreamRoute
	matrixRoutes map[string]MatrixRoute
}

// New loads the route file if present, otherwise starts empty.
func New(filePath string) (*Router, error) {
	r := &Router{
		file:         filePath,
		routes:       make(map[string]StreamRoute),
		matrixRoutes: make(map[string]MatrixRoute),
	}
	data, err := os.ReadFile(filePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &r.routes); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filePath, err)
		}
	}
	for _, sr := range r.routes {
		r.matrixRoutes[sr.ID] = StreamRouteToMatrixRoute(sr)
	}
	return r, nil
}

// getSnapshot returns a copy of the current route table.
func (r *Router) getSnapshot() map[string]StreamRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]StreamRoute, len(r.routes))
	for k, v := range r.routes {
		cp[k] = v
	}
	return cp
}

// Save persists the current route table. The mutex is not held while the file
// is written, so writers never deadlock on themselves.
func (r *Router) Save() error {
	snapshot := r.getSnapshot()
	if err := os.MkdirAll(filepath.Dir(r.file), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.file, data)
}

// writeFileAtomic writes via a temp file + rename so a crash cannot corrupt the
// persisted route table.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".routes-*.tmp")
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

// Get returns the route with the given ID.
func (r *Router) Get(id string) (StreamRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sr, ok := r.routes[id]
	return sr, ok
}

// List returns all routes sorted by ID for deterministic output.
func (r *Router) List() []StreamRoute {
	snapshot := r.getSnapshot()
	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]StreamRoute, 0, len(ids))
	for _, id := range ids {
		out = append(out, snapshot[id])
	}
	return out
}

// Add inserts (or replaces) a route and persists it.
func (r *Router) Add(route StreamRoute) error {
	if route.ID == "" {
		return fmt.Errorf("route id is required")
	}
	r.mu.Lock()
	r.routes[route.ID] = route
	r.matrixRoutes[route.ID] = StreamRouteToMatrixRoute(route)
	r.mu.Unlock()
	return r.Save()
}

// Delete removes a route and persists the change.
func (r *Router) Delete(id string) error {
	r.mu.Lock()
	_, existed := r.routes[id]
	delete(r.routes, id)
	delete(r.matrixRoutes, id)
	r.mu.Unlock()
	if !existed {
		return nil
	}
	return r.Save()
}

// GetMatrix returns the MatrixRoute with the given ID.
func (r *Router) GetMatrix(id string) (MatrixRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mr, ok := r.matrixRoutes[id]
	mr.Coefficients = append([]float64(nil), mr.Coefficients...)
	return mr, ok
}

// ListMatrix returns all MatrixRoutes sorted by ID.
func (r *Router) ListMatrix() []MatrixRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.matrixRoutes))
	for id := range r.matrixRoutes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]MatrixRoute, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.matrixRoutes[id])
	}
	return out
}

// AddMatrix inserts or replaces a MatrixRoute and persists it.
func (r *Router) AddMatrix(mr MatrixRoute) error {
	if err := mr.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	r.matrixRoutes[mr.ID] = mr
	r.routes[mr.ID] = MatrixRouteToStreamRoute(mr)
	r.mu.Unlock()
	return r.Save()
}

// DeleteMatrix removes a MatrixRoute.
func (r *Router) DeleteMatrix(id string) error {
	return r.Delete(id)
}

// UpdateCrosspoint updates the gain for an individual crosspoint with slewing.
func (r *Router) UpdateCrosspoint(id string, outCh, inCh int, gain float64, slewMS float64) (MatrixRoute, error) {
	r.mu.Lock()
	mr, ok := r.matrixRoutes[id]
	if !ok {
		r.mu.Unlock()
		return MatrixRoute{}, fmt.Errorf("route %s not found", id)
	}
	if outCh < 0 || outCh >= mr.OutChannels || inCh < 0 || inCh >= mr.InChannels {
		r.mu.Unlock()
		return MatrixRoute{}, fmt.Errorf("crosspoint out of range: out=%d, in=%d", outCh, inCh)
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain < 0.0 || gain > 2.0 {
		r.mu.Unlock()
		return MatrixRoute{}, fmt.Errorf("gain out of bounds [0.0, 2.0]: %f", gain)
	}
	idx := outCh*mr.InChannels + inCh
	mr.Coefficients = append([]float64(nil), mr.Coefficients...)
	mr.Coefficients[idx] = gain
	if slewMS > 0 {
		mr.SlewMS = slewMS
		mr.State = "slewing"
	}
	r.matrixRoutes[id] = mr
	r.routes[id] = MatrixRouteToStreamRoute(mr)
	r.mu.Unlock()

	_ = r.Save()
	return mr, nil
}

// PatchMatrix applies partial updates to a MatrixRoute.
func (r *Router) PatchMatrix(id string, name *string, coeffs []float64, slewMS *float64, muted *bool) (MatrixRoute, error) {
	r.mu.Lock()
	mr, ok := r.matrixRoutes[id]
	if !ok {
		r.mu.Unlock()
		return MatrixRoute{}, fmt.Errorf("route %s not found", id)
	}
	if name != nil {
		mr.Name = *name
	}
	if coeffs != nil {
		expected := mr.InChannels * mr.OutChannels
		if len(coeffs) != expected {
			r.mu.Unlock()
			return MatrixRoute{}, fmt.Errorf("coefficients length mismatch: expected %d, got %d", expected, len(coeffs))
		}
		for i, c := range coeffs {
			if math.IsNaN(c) || math.IsInf(c, 0) || c < 0.0 || c > 2.0 {
				r.mu.Unlock()
				return MatrixRoute{}, fmt.Errorf("coefficient at %d out of bounds [0.0, 2.0]: %f", i, c)
			}
		}
		mr.Coefficients = make([]float64, len(coeffs))
		copy(mr.Coefficients, coeffs)
		mr.State = "slewing"
	}
	if slewMS != nil {
		if *slewMS < 0 {
			r.mu.Unlock()
			return MatrixRoute{}, errors.New("slew_ms must be non-negative")
		}
		mr.SlewMS = *slewMS
	}
	if muted != nil {
		mr.Muted = *muted
	}
	r.matrixRoutes[id] = mr
	r.routes[id] = MatrixRouteToStreamRoute(mr)
	r.mu.Unlock()

	_ = r.Save()
	return mr, nil
}

// StreamRouteToMatrixRoute converts a legacy StreamRoute to canonical MatrixRoute.
func StreamRouteToMatrixRoute(sr StreamRoute) MatrixRoute {
	ch := sr.Channels
	if ch <= 0 {
		ch = 2
	}
	srcNode := "local"
	dstNode := "local"
	inCh, outCh := ch, ch
	if sr.InChannels > 0 {
		inCh = sr.InChannels
	}
	if sr.OutChannels > 0 {
		outCh = sr.OutChannels
	}
	coeffs := NewDefaultDownmixMatrix(inCh, outCh).Coefficients
	if len(sr.Coefficients) == inCh*outCh {
		coeffs = append([]float64(nil), sr.Coefficients...)
	}
	if sr.Direction == "receive" {
		srcNode = sr.PeerID
	}
	if sr.PeerID != "" {
		if sr.Direction != "receive" {
			dstNode = sr.PeerID
		}
	}
	if sr.Direction == "receive" && sr.PeerID == "" {
		srcNode = "remote"
	}
	inChans := make([]int, inCh)
	outChans := make([]int, outCh)
	for i := 0; i < inCh; i++ {
		inChans[i] = i
	}
	for i := 0; i < outCh; i++ {
		outChans[i] = i
	}
	srcDevice, dstDevice := fmt.Sprintf("%d", sr.SourceDevice), fmt.Sprintf("%d", sr.DestDevice)
	if sr.SourceDeviceURN != "" {
		srcDevice = sr.SourceDeviceURN
	}
	if sr.DestDeviceURN != "" {
		dstDevice = sr.DestDeviceURN
	}
	slew := sr.SlewMS
	if slew <= 0 {
		slew = 20
	}
	return MatrixRoute{
		ID:   sr.ID,
		Name: sr.ID,
		Source: MatrixEndpoint{
			NodeID:   srcNode,
			DeviceID: srcDevice,
			Layout:   LayoutStereo,
			Channels: inChans,
		},
		Destination: MatrixEndpoint{
			NodeID:   dstNode,
			DeviceID: dstDevice,
			Layout:   LayoutStereo,
			Channels: outChans,
		},
		InChannels:    inCh,
		OutChannels:   outCh,
		Coefficients:  coeffs,
		SlewMS:        slew,
		Muted:         sr.Muted,
		State:         "active",
		LatencyPreset: sr.LatencyPreset,
	}
}

// MatrixRouteToStreamRoute converts a canonical MatrixRoute to legacy StreamRoute.
func MatrixRouteToStreamRoute(mr MatrixRoute) StreamRoute {
	direction := ""
	if mr.Source.NodeID != "" && mr.Source.NodeID != "local" {
		direction = "receive"
	}
	var srcDev int
	trimmedSrc := strings.TrimPrefix(mr.Source.DeviceID, "hw:")
	trimmedSrc = strings.TrimPrefix(trimmedSrc, "dev:")
	trimmedSrc = strings.TrimPrefix(trimmedSrc, "virt-")
	trimmedSrc = strings.TrimPrefix(trimmedSrc, "virt:")
	if _, err := fmt.Sscanf(trimmedSrc, "%d", &srcDev); err != nil {
		srcDev = 0
	}

	var dstDev int = -1
	trimmedDst := strings.TrimPrefix(mr.Destination.DeviceID, "hw:")
	trimmedDst = strings.TrimPrefix(trimmedDst, "dev:")
	trimmedDst = strings.TrimPrefix(trimmedDst, "virt-")
	trimmedDst = strings.TrimPrefix(trimmedDst, "virt:")
	if _, err := fmt.Sscanf(trimmedDst, "%d", &dstDev); err != nil {
		dstDev = -1
	}

	var dests []string
	if mr.Destination.NodeID != "" && mr.Destination.NodeID != "local" {
		dests = []string{mr.Destination.NodeID}
	} else if len(mr.Destination.Channels) > 0 {
		dests = []string{"127.0.0.1:45678"}
	}
	return StreamRoute{
		ID:              mr.ID,
		Direction:       direction,
		SourceDevice:    srcDev,
		SourceDeviceURN: mr.Source.DeviceID,
		DestDevice:      dstDev,
		DestDeviceURN:   mr.Destination.DeviceID,
		Destinations:    dests,
		SampleRate:      48000,
		Channels:        mr.OutChannels,
		InChannels:      mr.InChannels,
		OutChannels:     mr.OutChannels,
		Coefficients:    append([]float64(nil), mr.Coefficients...),
		SlewMS:          mr.SlewMS,
		Muted:           mr.Muted,
		PeerID:          mr.Destination.NodeID,
		LatencyPreset:   mr.LatencyPreset,
	}
}
