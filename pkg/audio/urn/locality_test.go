package urn

import (
	"testing"
)

func TestClassifyDeviceLocality(t *testing.T) {
	localNode := "node-alpha"

	tests := []struct {
		name         string
		urn          string
		wantLocality Locality
		wantNodeID   string
	}{
		{
			name:         "Empty string defaults to local",
			urn:          "",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Whitespace string defaults to local",
			urn:          "   ",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Plain legacy device name",
			urn:          "default",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "ALSA legacy string",
			urn:          "hw:0,0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Hardware ALSA URN",
			urn:          "urn:audio:hw:alsa:c=PCH:d=0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Hardware USB URN",
			urn:          "urn:audio:hw:usb:v=046d:p=0825:s=12345:d=0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Hardware Path URN",
			urn:          "urn:audio:hw:path:pci-0000_00_1f.3:d=0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Hardware Name URN",
			urn:          "urn:audio:hw:name:built-in-audio:a1b2c3d4",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Virtual Loopback URN",
			urn:          "urn:audio:virt:monitor-bus",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Node URN matching local node ID",
			urn:          "urn:audio:node:node-alpha:dev:urn:audio:hw:alsa:c=PCH:d=0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Node URN with literal 'local'",
			urn:          "urn:audio:node:local:dev:urn:audio:virt:mix",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Node URN with empty node ID",
			urn:          "urn:audio:node::dev:urn:audio:hw:alsa:c=PCH:d=0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Node URN with remote node ID",
			urn:          "urn:audio:node:node-beta:dev:urn:audio:hw:alsa:c=PCH:d=0",
			wantLocality: LocalityRemote,
			wantNodeID:   "node-beta",
		},
		{
			name:         "Malformed Node URN with remote node ID",
			urn:          "urn:audio:node:node-gamma:incomplete",
			wantLocality: LocalityRemote,
			wantNodeID:   "node-gamma",
		},
		{
			name:         "Canonical slash with remote node ID",
			urn:          "node-beta/hw:0,0",
			wantLocality: LocalityRemote,
			wantNodeID:   "node-beta",
		},
		{
			name:         "Canonical slash with hex node ID remote",
			urn:          "20df5bc09db978cc75c41086fdc614bf/hw:1,0",
			wantLocality: LocalityRemote,
			wantNodeID:   "20df5bc09db978cc75c41086fdc614bf",
		},
		{
			name:         "Canonical slash matching local node ID",
			urn:          "node-alpha/hw:0,0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Canonical slash with literal 'local'",
			urn:          "local/hw:0,0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Leading slash Linux device path not treated as node ID",
			urn:          "/dev/snd/pcmC0D0c",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Slash with whitespace remote node ID",
			urn:          "  node-beta  /  hw:0,0 ",
			wantLocality: LocalityRemote,
			wantNodeID:   "node-beta",
		},
		{
			name:         "Canonical slash with empty node ID",
			urn:          "/hw:0,0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "RFC 8141 URN with slash in codec name is local",
			urn:          "urn:audio:hw:alsa:c=ALC892/897:d=0",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Remote node prefix with URN device is remote",
			urn:          "remoteNode/urn:audio:hw:default",
			wantLocality: LocalityRemote,
			wantNodeID:   "remoteNode",
		},
		{
			name:         "Local node prefix with URN device is local",
			urn:          "node-alpha/urn:audio:hw:default",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
		{
			name:         "Literal local prefix with URN device is local",
			urn:          "local/urn:audio:hw:default",
			wantLocality: LocalityLocal,
			wantNodeID:   localNode,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, nodeID := ClassifyDeviceLocality(tt.urn, localNode)
			if loc != tt.wantLocality {
				t.Errorf("ClassifyDeviceLocality(%q) locality = %v, want %v", tt.urn, loc, tt.wantLocality)
			}
			if nodeID != tt.wantNodeID {
				t.Errorf("ClassifyDeviceLocality(%q) nodeID = %v, want %v", tt.urn, nodeID, tt.wantNodeID)
			}

			isLocal := IsLocalEndpoint(tt.urn, localNode)
			if isLocal != (tt.wantLocality == LocalityLocal) {
				t.Errorf("IsLocalEndpoint(%q) = %v, want %v", tt.urn, isLocal, tt.wantLocality == LocalityLocal)
			}
		})
	}
}
