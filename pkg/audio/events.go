package audio

import (
	"sync"
	"time"
)

// DeviceEventType defines the category of device lifecycle transition.
type DeviceEventType string

const (
	// DeviceCreated indicates a hardware interface attached or virtual device was created.
	DeviceCreated DeviceEventType = "DeviceCreated"
	// DeviceRemoved indicates a hardware interface was disconnected or virtual device deleted.
	DeviceRemoved DeviceEventType = "DeviceRemoved"
	// DeviceQuarantined indicates a device experienced I/O failure or vanished from host topology.
	DeviceQuarantined DeviceEventType = "DeviceQuarantined"
	// DeviceRestored indicates a quarantined device resumed healthy operation.
	DeviceRestored DeviceEventType = "DeviceRestored"
)

// DeviceLifecycleEvent encapsulates state changes in physical and virtual audio endpoints.
type DeviceLifecycleEvent struct {
	Type      DeviceEventType `json:"type"`
	DeviceURN string          `json:"deviceURN"`
	LegacyID  string          `json:"legacyID,omitempty"`
	NodeID    string          `json:"nodeID,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
}

// DeviceLifecycleListener receives real-time notifications of endpoint lifecycle events.
type DeviceLifecycleListener interface {
	OnDeviceLifecycleEvent(evt DeviceLifecycleEvent)
}

// DeviceEventHub coordinates thread-safe publish-subscribe notification for device events.
type DeviceEventHub struct {
	mu        sync.RWMutex
	listeners []DeviceLifecycleListener
}

// NewDeviceEventHub initializes a new reactive lifecycle event bus.
func NewDeviceEventHub() *DeviceEventHub {
	return &DeviceEventHub{
		listeners: make([]DeviceLifecycleListener, 0),
	}
}

var (
	globalEventHub     *DeviceEventHub
	globalEventHubOnce sync.Once
)

// GlobalEventHub returns the process-wide singleton device event bus.
func GlobalEventHub() *DeviceEventHub {
	globalEventHubOnce.Do(func() {
		globalEventHub = NewDeviceEventHub()
	})
	return globalEventHub
}

// RegisterListener registers a listener to receive device lifecycle events.
func (h *DeviceEventHub) RegisterListener(l DeviceLifecycleListener) {
	if l == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.listeners {
		if existing == l {
			return
		}
	}
	h.listeners = append(h.listeners, l)
}

// UnregisterListener unregisters a previously registered device lifecycle listener.
func (h *DeviceEventHub) UnregisterListener(l DeviceLifecycleListener) {
	if l == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, existing := range h.listeners {
		if existing == l {
			h.listeners = append(h.listeners[:i], h.listeners[i+1:]...)
			return
		}
	}
}

// Dispatch broadcasts an event synchronously to all registered listeners.
func (h *DeviceEventHub) Dispatch(evt DeviceLifecycleEvent) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}

	h.mu.RLock()
	listeners := make([]DeviceLifecycleListener, len(h.listeners))
	copy(listeners, h.listeners)
	h.mu.RUnlock()

	for _, l := range listeners {
		l.OnDeviceLifecycleEvent(evt)
	}
}
