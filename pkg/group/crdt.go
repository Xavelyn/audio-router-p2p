package group

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTombstoneRetention specifies how long deleted room tombstones are preserved
	// to prevent resurrection across network partitions (72 hours per ADR 0017).
	DefaultTombstoneRetention = 72 * time.Hour
)

// HLT represents a Hybrid Lamport Timestamp with unique node ID tie-breaking.
type HLT struct {
	WallTime int64  `json:"wall"`    // Unix time in nanoseconds
	Counter  uint64 `json:"logical"` // Logical sequence counter
	NodeID   string `json:"node"`    // Canonical node ID
}

// After returns true if h dominates other in total order.
func (h HLT) After(other HLT) bool {
	if h.WallTime != other.WallTime {
		return h.WallTime > other.WallTime
	}
	if h.Counter != other.Counter {
		return h.Counter > other.Counter
	}
	return strings.Compare(h.NodeID, other.NodeID) > 0
}

// IsZero returns true if the timestamp has not been initialized.
func (h HLT) IsZero() bool {
	return h.WallTime == 0 && h.Counter == 0 && h.NodeID == ""
}

// RoomRecord is the payload stored in the LWW-Element-Set CRDT.
type RoomRecord struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Leader      string             `json:"leader"`
	Inputs      []string           `json:"inputs"`
	Outputs     []string           `json:"outputs"`
	Synced      bool               `json:"synced"`
	InputGains  map[string]float64 `json:"input_gains,omitempty"`
	OutputGains map[string]float64 `json:"output_gains,omitempty"`
	Timestamp   HLT                `json:"ts"`
	Deleted     bool               `json:"deleted"`
	DeletedAt   int64              `json:"deleted_at,omitempty"` // Unix nano

	// Nested LWW-Element-Set device modifications: deviceID -> HLT
	InputAdds     map[string]HLT `json:"input_adds,omitempty"`
	InputRemoves  map[string]HLT `json:"input_removes,omitempty"`
	OutputAdds    map[string]HLT `json:"output_adds,omitempty"`
	OutputRemoves map[string]HLT `json:"output_removes,omitempty"`
}

// Clone returns a deep copy of the RoomRecord.
func (r RoomRecord) Clone() RoomRecord {
	cp := r
	if r.Inputs != nil {
		cp.Inputs = make([]string, len(r.Inputs))
		copy(cp.Inputs, r.Inputs)
	}
	if r.Outputs != nil {
		cp.Outputs = make([]string, len(r.Outputs))
		copy(cp.Outputs, r.Outputs)
	}
	if r.InputGains != nil {
		cp.InputGains = make(map[string]float64, len(r.InputGains))
		for k, v := range r.InputGains {
			cp.InputGains[k] = v
		}
	}
	if r.OutputGains != nil {
		cp.OutputGains = make(map[string]float64, len(r.OutputGains))
		for k, v := range r.OutputGains {
			cp.OutputGains[k] = v
		}
	}
	if r.InputAdds != nil {
		cp.InputAdds = make(map[string]HLT, len(r.InputAdds))
		for k, v := range r.InputAdds {
			cp.InputAdds[k] = v
		}
	}
	if r.InputRemoves != nil {
		cp.InputRemoves = make(map[string]HLT, len(r.InputRemoves))
		for k, v := range r.InputRemoves {
			cp.InputRemoves[k] = v
		}
	}
	if r.OutputAdds != nil {
		cp.OutputAdds = make(map[string]HLT, len(r.OutputAdds))
		for k, v := range r.OutputAdds {
			cp.OutputAdds[k] = v
		}
	}
	if r.OutputRemoves != nil {
		cp.OutputRemoves = make(map[string]HLT, len(r.OutputRemoves))
		for k, v := range r.OutputRemoves {
			cp.OutputRemoves[k] = v
		}
	}
	return cp
}

// CRDTDeltaMsg carries a room modification delta for gossip dissemination.
type CRDTDeltaMsg struct {
	Type   string     `json:"type"` // "crdt_room_delta"
	NodeID string     `json:"node_id"`
	Record RoomRecord `json:"record"`
}

// CRDTStateMsg carries a full CRDT state snapshot for anti-entropy sync.
type CRDTStateMsg struct {
	Type    string       `json:"type"` // "crdt_room_state"
	NodeID  string       `json:"node_id"`
	Records []RoomRecord `json:"records"`
}

// LWWSet represents a thread-safe LWW-Element-Set CRDT for room replication.
type LWWSet struct {
	mu         sync.RWMutex
	records    map[string]RoomRecord
	retention  time.Duration
	localNode  string
	lastWall   int64
	logicalSeq uint64
}

// NewLWWSet initializes a new LWW-Element-Set CRDT store.
func NewLWWSet(nodeID string, retention ...time.Duration) *LWWSet {
	ret := DefaultTombstoneRetention
	if len(retention) > 0 && retention[0] > 0 {
		ret = retention[0]
	}
	return &LWWSet{
		records:   make(map[string]RoomRecord),
		retention: ret,
		localNode: nodeID,
	}
}

// nextHLTLocked generates the next strictly increasing HLT with physical clock regression protection.
func (s *LWWSet) nextHLTLocked() HLT {
	now := time.Now().UnixNano()
	if now > s.lastWall {
		s.lastWall = now
		s.logicalSeq = 0
	} else {
		s.logicalSeq++
	}
	return HLT{
		WallTime: s.lastWall,
		Counter:  s.logicalSeq,
		NodeID:   s.localNode,
	}
}

// observeHLTLocked updates the logical clock counter on incoming timestamps.
func (s *LWWSet) observeHLTLocked(other HLT) {
	if other.WallTime > s.lastWall {
		s.lastWall = other.WallTime
		s.logicalSeq = other.Counter + 1
	} else if other.WallTime == s.lastWall {
		if other.Counter >= s.logicalSeq {
			s.logicalSeq = other.Counter + 1
		}
	}
}

// CreateRoom registers or updates a room record with a new HLT.
func (s *LWWSet) CreateRoom(id, name string) RoomRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := s.nextHLTLocked()
	rec, exists := s.records[id]
	if !exists {
		rec = RoomRecord{
			ID:            id,
			InputGains:    make(map[string]float64),
			OutputGains:   make(map[string]float64),
			InputAdds:     make(map[string]HLT),
			InputRemoves:  make(map[string]HLT),
			OutputAdds:    make(map[string]HLT),
			OutputRemoves: make(map[string]HLT),
		}
	}
	rec.Name = name
	rec.Deleted = false
	rec.DeletedAt = 0
	rec.Timestamp = ts
	s.records[id] = rec.Clone()
	return rec.Clone()
}

// DeleteRoom marks a room as deleted with a tombstone and new HLT.
func (s *LWWSet) DeleteRoom(id string) (RoomRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[id]
	if !exists {
		rec = RoomRecord{
			ID:            id,
			InputGains:    make(map[string]float64),
			OutputGains:   make(map[string]float64),
			InputAdds:     make(map[string]HLT),
			InputRemoves:  make(map[string]HLT),
			OutputAdds:    make(map[string]HLT),
			OutputRemoves: make(map[string]HLT),
		}
	}
	ts := s.nextHLTLocked()
	rec.Deleted = true
	rec.DeletedAt = ts.WallTime
	rec.Timestamp = ts
	s.records[id] = rec.Clone()
	return rec.Clone(), true
}

// SetLeader updates the room leader with a new HLT.
func (s *LWWSet) SetLeader(id, leader string) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.records[id]
	if !exists || rec.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	if rec.Leader == leader {
		return rec.Clone(), nil
	}
	rec = rec.Clone()
	ts := s.nextHLTLocked()
	rec.Leader = leader
	rec.Timestamp = ts
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// AddInput binds an input device to a room using nested LWW-Element-Set semantics.
func (s *LWWSet) AddInput(id, devID string) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.InputAdds == nil {
		rec.InputAdds = make(map[string]HLT)
	}
	rec.InputAdds[devID] = ts
	rec.Timestamp = ts
	recomputeInputs(&rec)
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// RemoveInput unbinds an input device from a room using nested LWW-Element-Set semantics.
// All matching active keys (evaluated via EndpointMatches) are tombstoned with the current HLT timestamp.
func (s *LWWSet) RemoveInput(id, devID string) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.InputRemoves == nil {
		rec.InputRemoves = make(map[string]HLT)
	}
	rec.InputRemoves[devID] = ts

	for k := range rec.InputAdds {
		if EndpointMatches(s.localNode, k, devID) {
			rec.InputRemoves[k] = ts
		}
	}
	for _, k := range rec.Inputs {
		if EndpointMatches(s.localNode, k, devID) {
			rec.InputRemoves[k] = ts
		}
	}

	rec.Timestamp = ts
	recomputeInputs(&rec)
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// RemoveInputSymmetric unbinds an input device from a room using symmetric canonical endpoint matching.
func (s *LWWSet) RemoveInputSymmetric(id, devID string) (RoomRecord, error) {
	return s.RemoveInput(id, devID)
}

// AddOutput binds an output device to a room using nested LWW-Element-Set semantics.
func (s *LWWSet) AddOutput(id, devID string) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.OutputAdds == nil {
		rec.OutputAdds = make(map[string]HLT)
	}
	rec.OutputAdds[devID] = ts
	rec.Timestamp = ts
	recomputeOutputs(&rec)
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// RemoveOutput unbinds an output device from a room using nested LWW-Element-Set semantics.
// All matching active keys (evaluated via EndpointMatches) are tombstoned with the current HLT timestamp.
func (s *LWWSet) RemoveOutput(id, devID string) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.OutputRemoves == nil {
		rec.OutputRemoves = make(map[string]HLT)
	}
	rec.OutputRemoves[devID] = ts

	for k := range rec.OutputAdds {
		if EndpointMatches(s.localNode, k, devID) {
			rec.OutputRemoves[k] = ts
		}
	}
	for _, k := range rec.Outputs {
		if EndpointMatches(s.localNode, k, devID) {
			rec.OutputRemoves[k] = ts
		}
	}

	rec.Timestamp = ts
	recomputeOutputs(&rec)
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// RemoveOutputSymmetric unbinds an output device from a room using symmetric canonical endpoint matching.
func (s *LWWSet) RemoveOutputSymmetric(id, devID string) (RoomRecord, error) {
	return s.RemoveOutput(id, devID)
}

// SetRoomBindings updates inputs, outputs, and sync state in one atomic transaction.
func (s *LWWSet) SetRoomBindings(id string, inputs, outputs []string, synced bool) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.InputAdds == nil {
		rec.InputAdds = make(map[string]HLT)
	}
	if rec.InputRemoves == nil {
		rec.InputRemoves = make(map[string]HLT)
	}
	if rec.OutputAdds == nil {
		rec.OutputAdds = make(map[string]HLT)
	}
	if rec.OutputRemoves == nil {
		rec.OutputRemoves = make(map[string]HLT)
	}

	newInSet := make(map[string]bool)
	for _, dev := range inputs {
		newInSet[dev] = true
		rec.InputAdds[dev] = ts
	}
	for _, dev := range rec.Inputs {
		if !newInSet[dev] {
			rec.InputRemoves[dev] = ts
		}
	}

	newOutSet := make(map[string]bool)
	for _, dev := range outputs {
		newOutSet[dev] = true
		rec.OutputAdds[dev] = ts
	}
	for _, dev := range rec.Outputs {
		if !newOutSet[dev] {
			rec.OutputRemoves[dev] = ts
		}
	}

	rec.Synced = synced
	rec.Timestamp = ts
	recomputeInputs(&rec)
	recomputeOutputs(&rec)
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// SetInputGain updates trim gain for an input device.
func (s *LWWSet) SetInputGain(id, devID string, gain float64) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.InputGains == nil {
		rec.InputGains = make(map[string]float64)
	}
	rec.InputGains[devID] = gain
	rec.Timestamp = ts
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// SetOutputGain updates trim gain for an output device.
func (s *LWWSet) SetOutputGain(id, devID string, gain float64) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	if rec.OutputGains == nil {
		rec.OutputGains = make(map[string]float64)
	}
	rec.OutputGains[devID] = gain
	rec.Timestamp = ts
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// SetRoomSync sets the room sync flag with a new HLT.
func (s *LWWSet) SetRoomSync(id string, synced bool) (RoomRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, exists := s.records[id]
	if !exists || raw.Deleted {
		return RoomRecord{}, fmt.Errorf("room %s not found", id)
	}
	rec := raw.Clone()
	ts := s.nextHLTLocked()
	rec.Synced = synced
	rec.Timestamp = ts
	s.records[id] = rec.Clone()
	return rec.Clone(), nil
}

// MergeRecord merges a single incoming room record into the CRDT.
// Returns the merged record and whether local state was updated.
func (s *LWWSet) MergeRecord(incoming RoomRecord) (RoomRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.observeHLTLocked(incoming.Timestamp)
	return s.mergeRecordLocked(incoming)
}

// mergeRecordLocked implements join-semilattice merge for a room record.
func (s *LWWSet) mergeRecordLocked(incoming RoomRecord) (RoomRecord, bool) {
	raw, exists := s.records[incoming.ID]
	if !exists {
		// New record
		rec := incoming.Clone()
		recomputeInputs(&rec)
		recomputeOutputs(&rec)
		s.records[incoming.ID] = rec.Clone()
		return rec.Clone(), true
	}

	local := raw.Clone()
	inClone := incoming.Clone()
	changed := false

	// Merge nested device LWW sets
	if mergeNestedSets(&local, &inClone) {
		changed = true
	}

	// Compare top-level timestamps: latest write wins, tie-break on nodeID
	if incoming.Timestamp.After(local.Timestamp) {
		local.Name = incoming.Name
		local.Leader = incoming.Leader
		local.Synced = incoming.Synced
		local.Deleted = incoming.Deleted
		local.DeletedAt = incoming.DeletedAt
		local.Timestamp = incoming.Timestamp

		// Gains updated from winning incoming record
		if incoming.InputGains != nil {
			local.InputGains = make(map[string]float64, len(incoming.InputGains))
			for k, v := range incoming.InputGains {
				local.InputGains[k] = v
			}
		}
		if incoming.OutputGains != nil {
			local.OutputGains = make(map[string]float64, len(incoming.OutputGains))
			for k, v := range incoming.OutputGains {
				local.OutputGains[k] = v
			}
		}
		changed = true
	}

	recomputeInputs(&local)
	recomputeOutputs(&local)

	if changed {
		s.records[local.ID] = local.Clone()
	}
	return local.Clone(), changed
}

// MergeState merges multiple records (full state sync) into the CRDT.
// Returns the slice of room IDs that underwent changes.
func (s *LWWSet) MergeState(records []RoomRecord) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var changedIDs []string
	for _, rec := range records {
		s.observeHLTLocked(rec.Timestamp)
		if _, changed := s.mergeRecordLocked(rec); changed {
			changedIDs = append(changedIDs, rec.ID)
		}
	}
	return changedIDs
}

// PruneTombstones removes tombstones older than the retention window (default 72h).
func (s *LWWSet) PruneTombstones(now ...time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := time.Now()
	if len(now) > 0 {
		t = now[0]
	}

	cutoff := t.Add(-s.retention).UnixNano()
	pruned := 0
	for id, rec := range s.records {
		if rec.Deleted && rec.DeletedAt < cutoff {
			delete(s.records, id)
			pruned++
		}
	}
	return pruned
}

// GetRoom returns the active room record, or false if deleted/not found.
func (s *LWWSet) GetRoom(id string) (RoomRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.records[id]
	if !ok || rec.Deleted {
		return RoomRecord{}, false
	}
	return rec.Clone(), true
}

// GetRecord returns any record (including deleted tombstones) by ID.
func (s *LWWSet) GetRecord(id string) (RoomRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.records[id]
	if !ok {
		return RoomRecord{}, false
	}
	return rec.Clone(), true
}

// ListActive returns all non-deleted room records sorted by ID.
func (s *LWWSet) ListActive() []RoomRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []RoomRecord
	for _, rec := range s.records {
		if !rec.Deleted {
			out = append(out, rec.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ExportState exports all room records (including active tombstones) for synchronization.
func (s *LWWSet) ExportState() []RoomRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]RoomRecord, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ExportStateJSON exports serialized JSON of all room records while holding the read lock.
func (s *LWWSet) ExportStateJSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]RoomRecord, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return json.Marshal(out)
}

// ImportState seeds or merges initial state records (used on Load).
func (s *LWWSet) ImportState(records []RoomRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range records {
		s.observeHLTLocked(rec.Timestamp)
		s.mergeRecordLocked(rec)
	}
}

// Helper functions for nested sets
func mergeNestedSets(local, incoming *RoomRecord) bool {
	changed := false
	if local.InputAdds == nil {
		local.InputAdds = make(map[string]HLT)
	}
	if local.InputRemoves == nil {
		local.InputRemoves = make(map[string]HLT)
	}
	if local.OutputAdds == nil {
		local.OutputAdds = make(map[string]HLT)
	}
	if local.OutputRemoves == nil {
		local.OutputRemoves = make(map[string]HLT)
	}

	for dev, inTS := range incoming.InputAdds {
		locTS, exists := local.InputAdds[dev]
		if !exists || inTS.After(locTS) {
			local.InputAdds[dev] = inTS
			changed = true
		}
	}
	for dev, inTS := range incoming.InputRemoves {
		locTS, exists := local.InputRemoves[dev]
		if !exists || inTS.After(locTS) {
			local.InputRemoves[dev] = inTS
			changed = true
		}
	}
	for dev, inTS := range incoming.OutputAdds {
		locTS, exists := local.OutputAdds[dev]
		if !exists || inTS.After(locTS) {
			local.OutputAdds[dev] = inTS
			changed = true
		}
	}
	for dev, inTS := range incoming.OutputRemoves {
		locTS, exists := local.OutputRemoves[dev]
		if !exists || inTS.After(locTS) {
			local.OutputRemoves[dev] = inTS
			changed = true
		}
	}
	return changed
}

func recomputeInputs(r *RoomRecord) {
	if r.InputAdds == nil {
		r.InputAdds = make(map[string]HLT)
	}
	if r.InputRemoves == nil {
		r.InputRemoves = make(map[string]HLT)
	}

	// For legacy records loaded without nested maps
	if len(r.InputAdds) == 0 && len(r.Inputs) > 0 {
		for _, dev := range r.Inputs {
			r.InputAdds[dev] = r.Timestamp
		}
	}

	active := make(map[string]bool)
	for dev, addTS := range r.InputAdds {
		remTS, hasRem := r.InputRemoves[dev]
		if !hasRem || addTS.After(remTS) {
			active[dev] = true
		}
	}
	inputs := make([]string, 0, len(active))
	for dev := range active {
		inputs = append(inputs, dev)
	}
	sort.Strings(inputs)
	r.Inputs = inputs
}

func recomputeOutputs(r *RoomRecord) {
	if r.OutputAdds == nil {
		r.OutputAdds = make(map[string]HLT)
	}
	if r.OutputRemoves == nil {
		r.OutputRemoves = make(map[string]HLT)
	}

	// For legacy records loaded without nested maps
	if len(r.OutputAdds) == 0 && len(r.Outputs) > 0 {
		for _, dev := range r.Outputs {
			r.OutputAdds[dev] = r.Timestamp
		}
	}

	active := make(map[string]bool)
	for dev, addTS := range r.OutputAdds {
		remTS, hasRem := r.OutputRemoves[dev]
		if !hasRem || addTS.After(remTS) {
			active[dev] = true
		}
	}
	outputs := make([]string, 0, len(active))
	for dev := range active {
		outputs = append(outputs, dev)
	}
	sort.Strings(outputs)
	r.Outputs = outputs
}

// MarshalCRDTDelta serializes a single delta for gossip datagrams.
func MarshalCRDTDelta(nodeID string, rec RoomRecord) ([]byte, error) {
	msg := CRDTDeltaMsg{
		Type:   "crdt_room_delta",
		NodeID: nodeID,
		Record: rec,
	}
	return json.Marshal(msg)
}

// MarshalCRDTState serializes the full CRDT state for anti-entropy sync.
func MarshalCRDTState(nodeID string, records []RoomRecord) ([]byte, error) {
	msg := CRDTStateMsg{
		Type:    "crdt_room_state",
		NodeID:  nodeID,
		Records: records,
	}
	return json.Marshal(msg)
}
