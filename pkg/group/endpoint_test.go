package group

import (
	"fmt"
	"testing"

	"github.com/audio-router-p2p/pkg/audio/urn"
)

func TestParseCanonicalEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		ref         string
		localNodeID string
		wantClean   string
		wantDir     string
		wantNodeID  string
		wantDevID   string
		wantIsLocal bool
	}{
		{
			name:        "bare local device",
			ref:         "mic-1",
			localNodeID: "nodeA",
			wantClean:   "mic-1",
			wantDir:     "",
			wantNodeID:  "",
			wantDevID:   "mic-1",
			wantIsLocal: true,
		},
		{
			name:        "directional input prefix in:",
			ref:         "in:mic-1",
			localNodeID: "nodeA",
			wantClean:   "mic-1",
			wantDir:     "in",
			wantNodeID:  "",
			wantDevID:   "mic-1",
			wantIsLocal: true,
		},
		{
			name:        "directional output prefix out:",
			ref:         "out:speaker-main",
			localNodeID: "nodeA",
			wantClean:   "speaker-main",
			wantDir:     "out",
			wantNodeID:  "",
			wantDevID:   "speaker-main",
			wantIsLocal: true,
		},
		{
			name:        "svelteflow node prefix input-",
			ref:         "input-nodeB/usb-dac",
			localNodeID: "nodeA",
			wantClean:   "nodeB/usb-dac",
			wantDir:     "in",
			wantNodeID:  "nodeB",
			wantDevID:   "usb-dac",
			wantIsLocal: false,
		},
		{
			name:        "svelteflow node prefix output-",
			ref:         "output-nodeA/hw:0,0",
			localNodeID: "nodeA",
			wantClean:   "nodeA/hw:0,0",
			wantDir:     "out",
			wantNodeID:  "nodeA",
			wantDevID:   "hw:0,0",
			wantIsLocal: true,
		},
		{
			name:        "node qualified slash local",
			ref:         "nodeA/mic-studio",
			localNodeID: "nodeA",
			wantClean:   "nodeA/mic-studio",
			wantDir:     "",
			wantNodeID:  "nodeA",
			wantDevID:   "mic-studio",
			wantIsLocal: true,
		},
		{
			name:        "node qualified slash remote",
			ref:         "nodeB/mic-remote",
			localNodeID: "nodeA",
			wantClean:   "nodeB/mic-remote",
			wantDir:     "",
			wantNodeID:  "nodeB",
			wantDevID:   "mic-remote",
			wantIsLocal: false,
		},
		{
			name:        "node qualified literal 'local'",
			ref:         "local/mic-desk",
			localNodeID: "nodeA",
			wantClean:   "local/mic-desk",
			wantDir:     "",
			wantNodeID:  "local",
			wantDevID:   "mic-desk",
			wantIsLocal: true,
		},
		{
			name:        "RFC 8141 node URN",
			ref:         "urn:audio:node:nodeA:dev:hw:0,0",
			localNodeID: "nodeA",
			wantClean:   "urn:audio:node:nodeA:dev:hw:0,0",
			wantDir:     "",
			wantNodeID:  "nodeA",
			wantDevID:   "hw:0,0",
			wantIsLocal: true,
		},
		{
			name:        "RFC 8141 virtual device URN",
			ref:         "in:urn:audio:virt:vdev-loopback",
			localNodeID: "nodeA",
			wantClean:   "urn:audio:virt:vdev-loopback",
			wantDir:     "in",
			wantNodeID:  "",
			wantDevID:   "urn:audio:virt:vdev-loopback",
			wantIsLocal: true,
		},
		{
			name:        "empty string",
			ref:         "   ",
			localNodeID: "nodeA",
			wantClean:   "",
			wantDir:     "",
			wantNodeID:  "",
			wantDevID:   "",
			wantIsLocal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := ParseCanonicalEndpoint(tt.ref, tt.localNodeID)
			if ep.Clean != tt.wantClean {
				t.Errorf("Clean: got %q, want %q", ep.Clean, tt.wantClean)
			}
			if ep.Direction != tt.wantDir {
				t.Errorf("Direction: got %q, want %q", ep.Direction, tt.wantDir)
			}
			if ep.NodeID != tt.wantNodeID {
				t.Errorf("NodeID: got %q, want %q", ep.NodeID, tt.wantNodeID)
			}
			if ep.DeviceID != tt.wantDevID {
				t.Errorf("DeviceID: got %q, want %q", ep.DeviceID, tt.wantDevID)
			}
			if ep.IsLocalNode != tt.wantIsLocal {
				t.Errorf("IsLocalNode: got %v, want %v", ep.IsLocalNode, tt.wantIsLocal)
			}
		})
	}
}

func TestEndpointMatches_Permutations(t *testing.T) {
	localNode := "node-local"

	tests := []struct {
		name      string
		candidate string
		target    string
		wantMatch bool
	}{
		// 1. Literal exact & case-insensitive
		{
			name:      "exact match",
			candidate: "mic-1",
			target:    "mic-1",
			wantMatch: true,
		},
		{
			name:      "case-insensitive match",
			candidate: "Mic-1",
			target:    "mic-1",
			wantMatch: true,
		},

		// 2. Directional prefix stripping
		{
			name:      "directional in: prefix against bare",
			candidate: "in:mic-1",
			target:    "mic-1",
			wantMatch: true,
		},
		{
			name:      "directional out: prefix against bare",
			candidate: "out:speaker-1",
			target:    "speaker-1",
			wantMatch: true,
		},
		{
			name:      "canvas input- vs output- prefixes on same device",
			candidate: "input-hw:0,0",
			target:    "output-hw:0,0",
			wantMatch: true,
		},
		{
			name:      "input: vs out: on qualified node",
			candidate: "input:node-local/mic-1",
			target:    "out:node-local/mic-1",
			wantMatch: true,
		},

		// 3. Node qualification and isolation (ADR 0029 effective node semantics)
		{
			name:      "local-qualified matches unqualified bare",
			candidate: "node-local/mic-1",
			target:    "mic-1",
			wantMatch: true,
		},
		{
			name:      "local keyword matches local-qualified",
			candidate: "local/mic-1",
			target:    "node-local/mic-1",
			wantMatch: true,
		},
		{
			name:      "local keyword matches unqualified",
			candidate: "local/mic-1",
			target:    "mic-1",
			wantMatch: true,
		},
		{
			name:      "remote node does NOT match unqualified (prevents cross-node collision)",
			candidate: "node-remote/mic-1",
			target:    "mic-1",
			wantMatch: false,
		},
		{
			name:      "remote node does NOT match local node with same dev name",
			candidate: "node-remote/mic-1",
			target:    "node-local/mic-1",
			wantMatch: false,
		},
		{
			name:      "identical remote nodes match",
			candidate: "node-remote/mic-1",
			target:    "in:node-remote/mic-1",
			wantMatch: true,
		},
		{
			name:      "different remote nodes do not match",
			candidate: "node-remote-1/mic-1",
			target:    "node-remote-2/mic-1",
			wantMatch: false,
		},

		// 4. RFC 8141 URNs
		{
			name:      "RFC 8141 node URN matches slash qualified",
			candidate: "urn:audio:node:node-local:dev:mic-1",
			target:    "node-local/mic-1",
			wantMatch: true,
		},
		{
			name:      "RFC 8141 node URN matches unqualified on local node",
			candidate: "urn:audio:node:node-local:dev:mic-1",
			target:    "mic-1",
			wantMatch: true,
		},
		{
			name:      "RFC 8141 remote node URN does NOT match local unqualified",
			candidate: "urn:audio:node:node-remote:dev:mic-1",
			target:    "mic-1",
			wantMatch: false,
		},
		{
			name:      "virtual device URN matches bare slug",
			candidate: "urn:audio:virt:vdev-monitor",
			target:    "vdev-monitor",
			wantMatch: true,
		},
		{
			name:      "virtual device URN with canvas prefix matches bare slug",
			candidate: "in:urn:audio:virt:vdev-monitor",
			target:    "output-vdev-monitor",
			wantMatch: true,
		},

		// 5. ALSA aliases and shorthands
		{
			name:      "ALSA hw:0 matches hw:0,0",
			candidate: "hw:0",
			target:    "hw:0,0",
			wantMatch: true,
		},
		{
			name:      "ALSA integer 0 matches hw:0,0",
			candidate: "0",
			target:    "hw:0,0",
			wantMatch: true,
		},
		{
			name:      "ALSA subdevice mismatch does NOT match",
			candidate: "hw:0,1",
			target:    "hw:0,0",
			wantMatch: false,
		},
		{
			name:      "ALSA card mismatch does NOT match",
			candidate: "hw:1,0",
			target:    "hw:0,0",
			wantMatch: false,
		},
		{
			name:      "RFC 8141 ALSA URN matches hw shorthand",
			candidate: "urn:audio:hw:alsa:c=0:d=0",
			target:    "hw:0",
			wantMatch: true,
		},
		{
			name:      "RFC 8141 ALSA URN matches hw:0,0 with node qualification",
			candidate: "node-local/urn:audio:hw:alsa:c=0:d=0",
			target:    "hw:0,0",
			wantMatch: true,
		},

		// 6. Checksum and slug equivalents
		{
			name:      "name URN with checksum matches bare name",
			candidate: fmt.Sprintf("urn:audio:hw:name:studio-mic:%s", urn.Checksum("studio-mic")),
			target:    "studio-mic",
			wantMatch: true,
		},
		{
			name:      "checksum slug matches bare slug",
			candidate: fmt.Sprintf("studio-mic:%s", urn.Checksum("studio-mic")),
			target:    "studio-mic",
			wantMatch: true,
		},

		// 7. Empty and whitespace
		{
			name:      "empty candidate vs valid target",
			candidate: "",
			target:    "mic-1",
			wantMatch: false,
		},
		{
			name:      "valid candidate vs empty target",
			candidate: "mic-1",
			target:    "   ",
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test forward direction
			gotForward := EndpointMatches(localNode, tt.candidate, tt.target)
			if gotForward != tt.wantMatch {
				t.Errorf("EndpointMatches(%q, %q) = %v, want %v", tt.candidate, tt.target, gotForward, tt.wantMatch)
			}

			// Test reverse symmetry (A matches B <=> B matches A)
			gotReverse := EndpointMatches(localNode, tt.target, tt.candidate)
			if gotReverse != gotForward {
				t.Errorf("Asymmetric EndpointMatches: forward(%q, %q)=%v, reverse(%q, %q)=%v",
					tt.candidate, tt.target, gotForward, tt.target, tt.candidate, gotReverse)
			}
		})
	}
}

func TestRoom_RemoveInputOutput_CanonicalMatching(t *testing.T) {
	localNode := "nodeA"
	room := NewRoom(RoomConfig{
		ID:          "room-canon-test",
		Name:        "Canonical Matching Room",
		LocalPeerID: localNode,
	})

	// Add node-qualified input and output
	room.AddInput("nodeA/mic-studio")
	room.AddInput("nodeB/mic-remote")
	room.AddOutput("nodeA/hw:0,0")
	room.AddOutput("nodeB/hw:1,0")

	if len(room.Inputs()) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(room.Inputs()))
	}
	if len(room.Outputs()) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(room.Outputs()))
	}

	// 1. Remove local input using bare unqualified name with directional tag "in:mic-studio"
	room.RemoveInput("in:mic-studio")

	inputs := room.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("expected 1 remaining input after canonical prune, got %d: %v", len(inputs), inputs)
	}
	if inputs[0] != "nodeB/mic-remote" {
		t.Errorf("expected nodeB/mic-remote to remain, got %q", inputs[0])
	}

	// 2. Attempt to remove nodeB/mic-remote using unqualified "mic-remote" (should NOT prune due to node isolation)
	room.RemoveInput("mic-remote")
	if len(room.Inputs()) != 1 {
		t.Fatalf("expected nodeB/mic-remote to NOT be pruned with unqualified ref, inputs: %v", room.Inputs())
	}

	// 3. Remove nodeB input using exact qualified reference
	room.RemoveInput("nodeB/mic-remote")
	if len(room.Inputs()) != 0 {
		t.Fatalf("expected 0 inputs after qualified prune, got %d", len(room.Inputs()))
	}

	// 4. Remove local output using ALSA alias "hw:0"
	room.RemoveOutput("hw:0")
	outputs := room.Outputs()
	if len(outputs) != 1 {
		t.Fatalf("expected 1 remaining output after ALSA alias prune, got %d: %v", len(outputs), outputs)
	}
	if outputs[0] != "nodeB/hw:1,0" {
		t.Errorf("expected nodeB/hw:1,0 to remain, got %q", outputs[0])
	}

	// 5. Remove nodeB output using RFC 8141 URN
	room.RemoveOutput("urn:audio:node:nodeB:dev:hw:1,0")
	if len(room.Outputs()) != 0 {
		t.Fatalf("expected 0 outputs after URN prune, got %d", len(room.Outputs()))
	}
}

func TestCRDT_RemoveInputOutputSymmetric(t *testing.T) {
	crdt := NewLWWSet("nodeX")
	roomID := "room-crdt-canon"
	crdt.CreateRoom(roomID, "CRDT Canon Room")

	// Add inputs and outputs with various qualifications
	if _, err := crdt.AddInput(roomID, "nodeX/mic-1"); err != nil {
		t.Fatalf("AddInput failed: %v", err)
	}
	if _, err := crdt.AddInput(roomID, "nodeY/mic-1"); err != nil {
		t.Fatalf("AddInput failed: %v", err)
	}
	if _, err := crdt.AddOutput(roomID, "nodeX/hw:0,0"); err != nil {
		t.Fatalf("AddOutput failed: %v", err)
	}

	rec, _ := crdt.GetRoom(roomID)
	if len(rec.Inputs) != 2 {
		t.Fatalf("expected 2 active inputs, got %d", len(rec.Inputs))
	}

	// Remove local mic-1 using bare name via symmetric unbind
	updated, err := crdt.RemoveInputSymmetric(roomID, "mic-1")
	if err != nil {
		t.Fatalf("RemoveInputSymmetric failed: %v", err)
	}
	if len(updated.Inputs) != 1 {
		t.Fatalf("expected 1 active input after symmetric removal, got %d: %v", len(updated.Inputs), updated.Inputs)
	}
	if updated.Inputs[0] != "nodeY/mic-1" {
		t.Errorf("expected nodeY/mic-1 to remain active, got %q", updated.Inputs[0])
	}

	// Remove output using ALSA alias "hw:0"
	updated, err = crdt.RemoveOutputSymmetric(roomID, "hw:0")
	if err != nil {
		t.Fatalf("RemoveOutputSymmetric failed: %v", err)
	}
	if len(updated.Outputs) != 0 {
		t.Fatalf("expected 0 active outputs, got %d: %v", len(updated.Outputs), updated.Outputs)
	}
}
