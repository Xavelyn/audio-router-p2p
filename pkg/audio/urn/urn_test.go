package urn

import (
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Focusrite Scarlett 2i2 USB", "focusrite-scarlett-2i2-usb"},
		{"Built-in Microphone", "built-in-microphone"},
		{"Emulated Microphone", "emulated-microphone"},
		{"Emulated Speaker", "emulated-speaker"},
		{"hw:0,0", "hw-0-0"},
		{"  --Multiple___Delimiters..Test--  ", "multiple-delimiters-test"},
		{"", "unknown"},
		{"   ", "unknown"},
		{"!!!@@@###", "device"},
	}

	for _, tt := range tests {
		actual := Slugify(tt.input)
		if actual != tt.expected {
			t.Errorf("Slugify(%q) = %q; expected %q", tt.input, actual, tt.expected)
		}
	}
}

func TestChecksum(t *testing.T) {
	c1 := Checksum("Emulated Microphone")
	c2 := Checksum("Emulated Microphone")
	c3 := Checksum("Emulated Speaker")

	if len(c1) != 8 {
		t.Errorf("expected 8 hex characters, got len %d (%q)", len(c1), c1)
	}
	if c1 != c2 {
		t.Errorf("checksum must be deterministic: %q != %q", c1, c2)
	}
	if c1 == c3 {
		t.Errorf("checksum collision between different names: %q == %q", c1, c3)
	}
}

func TestURNConstructorsAndParse(t *testing.T) {
	// Tier 1: USB
	usbRaw := NewUSB("1235", "8210", "10042718", 0)
	if usbRaw != "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0" {
		t.Errorf("unexpected USB URN: %s", usbRaw)
	}
	u1, err := Parse(usbRaw)
	if err != nil {
		t.Fatalf("failed to parse USB URN: %v", err)
	}
	if u1.Scope != ScopeHardware || u1.Tier != TierUSB || u1.VendorID != "1235" || u1.ProductID != "8210" || u1.Serial != "10042718" || u1.Device != 0 {
		t.Errorf("unexpected parsed USB URN: %+v", u1)
	}
	if u1.String() != usbRaw {
		t.Errorf("String() mismatch: %s != %s", u1.String(), usbRaw)
	}

	// Tier 2: Path
	pathRaw := NewPath("pci-0000:00:14.0-usb-0:2.1:1.0", 0)
	if pathRaw != "urn:audio:hw:path:pci-0000:00:14.0-usb-0:2.1:1.0:d=0" {
		t.Errorf("unexpected Path URN: %s", pathRaw)
	}
	u2, err := Parse(pathRaw)
	if err != nil {
		t.Fatalf("failed to parse Path URN: %v", err)
	}
	if u2.Scope != ScopeHardware || u2.Tier != TierPath || u2.BusPath != "pci-0000:00:14.0-usb-0:2.1:1.0" || u2.Device != 0 {
		t.Errorf("unexpected parsed Path URN: %+v", u2)
	}

	// Tier 3: ALSA
	alsaRaw := NewALSA("PCH", 1)
	if alsaRaw != "urn:audio:hw:alsa:c=PCH:d=1" {
		t.Errorf("unexpected ALSA URN: %s", alsaRaw)
	}
	u3, err := Parse(alsaRaw)
	if err != nil {
		t.Fatalf("failed to parse ALSA URN: %v", err)
	}
	if u3.Scope != ScopeHardware || u3.Tier != TierALSA || u3.CardID != "PCH" || u3.Device != 1 {
		t.Errorf("unexpected parsed ALSA URN: %+v", u3)
	}

	// Tier 4: Name
	nameRaw := NewName("Built-in Microphone")
	u4, err := Parse(nameRaw)
	if err != nil {
		t.Fatalf("failed to parse Name URN: %v", err)
	}
	if u4.Scope != ScopeHardware || u4.Tier != TierName || u4.Slug != "built-in-microphone" || len(u4.Hash) != 8 {
		t.Errorf("unexpected parsed Name URN: %+v", u4)
	}

	// Virtual
	virtRaw := NewVirtual("monitor-bus")
	if virtRaw != "urn:audio:virt:monitor-bus" {
		t.Errorf("unexpected Virtual URN: %s", virtRaw)
	}
	uv, err := Parse(virtRaw)
	if err != nil {
		t.Fatalf("failed to parse Virtual URN: %v", err)
	}
	if uv.Scope != ScopeVirtual || uv.Tier != TierVirtual || uv.Slug != "monitor-bus" {
		t.Errorf("unexpected parsed Virtual URN: %+v", uv)
	}

	// Cluster Mesh
	nodeRaw := NewNode("stagebox-01", usbRaw)
	if nodeRaw != "urn:audio:node:stagebox-01:dev:"+usbRaw {
		t.Errorf("unexpected Node URN: %s", nodeRaw)
	}
	un, err := Parse(nodeRaw)
	if err != nil {
		t.Fatalf("failed to parse Node URN: %v", err)
	}
	if un.Scope != ScopeNode || un.Tier != TierNode || un.NodeID != "stagebox-01" || un.LocalURN == nil {
		t.Errorf("unexpected parsed Node URN: %+v", un)
	}
	if un.LocalURN.Tier != TierUSB || un.LocalURN.Serial != "10042718" {
		t.Errorf("unexpected nested LocalURN: %+v", un.LocalURN)
	}
}

func TestParseErrors(t *testing.T) {
	invalidCases := []string{
		"",
		"http://localhost:8080",
		"urn:other:hw:usb:v=1235:p=8210:s=123:d=0",
		"urn:audio:invalid_scope:foo",
		"urn:audio:hw:unknown_tier:foo",
		"urn:audio:hw:usb:v=1235:p=8210:d=0", // missing serial
		"urn:audio:hw:path:pci-0000",         // missing :d=
		"urn:audio:hw:alsa:c=PCH",            // missing :d=
		"urn:audio:hw:name:no-hash",          // missing hash
		"urn:audio:hw:name:slug:1234",        // hash not 8 chars
		"urn:audio:virt:",                    // empty slug
		"urn:audio:node:onlynode",            // missing :dev:
	}

	for _, raw := range invalidCases {
		_, err := Parse(raw)
		if err == nil {
			t.Errorf("expected error for invalid URN %q, got nil", raw)
		}
	}
}
