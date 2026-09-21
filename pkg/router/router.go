package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// StreamRoute describes how audio from one source device is delivered.
type StreamRoute struct {
	ID           string   `json:"id"`
	SourceDevice int      `json:"sourceDevice"`
	Destinations []string `json:"destinations"`
	SampleRate   int      `json:"sampleRate,omitempty"`
	Channels     int      `json:"channels,omitempty"`

	// Peer binding and negotiated format
	PeerID       string `json:"peerId,omitempty"`       // Bound peer ID (empty = any/unicast)
	PeerAddr     string `json:"peerAddr,omitempty"`     // Peer's stream address (host:port)
	Negotiated   bool   `json:"negotiated,omitempty"`   // Whether format was negotiated
	NegSampleRate int   `json:"negSampleRate,omitempty"` // Negotiated sample rate
	NegChannels  int    `json:"negChannels,omitempty"`  // Negotiated channels
	NegCodec     string `json:"negCodec,omitempty"`     // Negotiated codec (e.g., "pcm16")
}

// Router is a persistent, file-backed store of StreamRoutes.
type Router struct {
	mu     sync.RWMutex
	file   string
	routes map[string]StreamRoute
}

// New loads the route file if present, otherwise starts empty.
func New(filePath string) (*Router, error) {
	r := &Router{file: filePath, routes: make(map[string]StreamRoute)}
	data, err := os.ReadFile(filePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &r.routes); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filePath, err)
		}
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
	r.mu.Unlock()
	return r.Save()
}

// Delete removes a route and persists the change.
func (r *Router) Delete(id string) error {
	r.mu.Lock()
	_, existed := r.routes[id]
	delete(r.routes, id)
	r.mu.Unlock()
	if !existed {
		return nil
	}
	return r.Save()
}
