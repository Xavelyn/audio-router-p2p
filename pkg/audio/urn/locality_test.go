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
