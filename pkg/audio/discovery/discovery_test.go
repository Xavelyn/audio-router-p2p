package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/audio-router-p2p/pkg/audio/urn"
)

func TestParseProcCards(t *testing.T) {
	procCardsSample := ` 0 [PCH            ]: HDA-Intel - HDA Intel PCH
                      HDA Intel PCH at 0xf7d10000 irq 51
 1 [USB            ]: USB-Audio - Scarlett 2i2 USB
                      Focusrite Scarlett 2i2 USB at usb-0000:00:14.0-2, high speed
`
	cards := ParseProcCards(procCardsSample)
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(cards))
	}

	c0, ok0 := cards[0]
	if !ok0 || c0.ID != "PCH" {
		t.Errorf("expected card 0 ID PCH, got %+v", c0)
	}

	c1, ok1 := cards[1]
	if !ok1 || c1.ID != "USB" {
		t.Errorf("expected card 1 ID USB, got %+v", c1)
	}
}

func TestDiscoverWithRoots_Tiers(t *testing.T) {
	tmpDir := t.TempDir()
	sysRoot := filepath.Join(tmpDir, "sys", "class", "sound")
	procRoot := filepath.Join(tmpDir, "proc", "asound")

	if err := os.MkdirAll(sysRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(procRoot, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Setup Card 0: USB Soundcard with serial (Tier 1)
	// Sysfs structure:
	// /sys/devices/pci0000:00/0000:00:14.0/usb1/1-2 (has idVendor, idProduct, serial)
	// /sys/devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/sound/card0
	usb0Dir := filepath.Join(tmpDir, "sys", "devices", "pci0000:00", "0000:00:14.0", "usb1", "1-2")
	usb0IfaceDir := filepath.Join(usb0Dir, "1-2:1.0", "sound", "card0")
	if err := os.MkdirAll(usb0IfaceDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(usb0Dir, "idVendor"), []byte("1235\n"), 0644)
	_ = os.WriteFile(filepath.Join(usb0Dir, "idProduct"), []byte("8210\n"), 0644)
	_ = os.WriteFile(filepath.Join(usb0Dir, "serial"), []byte("10042718\n"), 0644)

	// Symlink in /sys/class/sound/card0 -> ...
	card0Link := filepath.Join(sysRoot, "card0")
	if err := os.Symlink(usb0IfaceDir, card0Link); err != nil {
		t.Fatal(err)
	}
	// Symlink device inside card0
	if err := os.Symlink(filepath.Join(usb0Dir, "1-2:1.0"), filepath.Join(usb0IfaceDir, "device")); err != nil {
		t.Fatal(err)
	}
	// PCM devices pcmC0D0p, pcmC0D0c
	_ = os.WriteFile(filepath.Join(sysRoot, "pcmC0D0p"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(sysRoot, "pcmC0D0c"), []byte(""), 0644)

	// 2. Setup Card 1: Serial-less USB Soundcard (Tier 2)
	usb1Dir := filepath.Join(tmpDir, "sys", "devices", "pci-0000:00:14.0-usb-0:2.1:1.0")
	usb1CardDir := filepath.Join(usb1Dir, "sound", "card1")
	if err := os.MkdirAll(usb1CardDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(usb1Dir, "idVendor"), []byte("08bb\n"), 0644)
	_ = os.WriteFile(filepath.Join(usb1Dir, "idProduct"), []byte("2902\n"), 0644)
	// Note: no serial file here!

	card1Link := filepath.Join(sysRoot, "card1")
	if err := os.Symlink(usb1CardDir, card1Link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(usb1Dir, filepath.Join(usb1CardDir, "device")); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(sysRoot, "pcmC1D0p"), []byte(""), 0644)

	// 3. Setup Card 2: PCI Card with ALSA ID only (Tier 3)
	pciDir := filepath.Join(tmpDir, "sys", "devices", "pci0000:00", "0000:00:1f.3")
	pciCardDir := filepath.Join(pciDir, "sound", "card2")
	if err := os.MkdirAll(pciCardDir, 0755); err != nil {
		t.Fatal(err)
	}
	card2Link := filepath.Join(sysRoot, "card2")
	if err := os.Symlink(pciCardDir, card2Link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pciDir, filepath.Join(pciCardDir, "device")); err != nil {
		t.Fatal(err)
	}
	// Card ID in procfs
	procCard2Dir := filepath.Join(procRoot, "card2")
	if err := os.MkdirAll(procCard2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(procCard2Dir, "id"), []byte("PCH\n"), 0644)
	_ = os.WriteFile(filepath.Join(sysRoot, "pcmC2D0p"), []byte(""), 0644)

	// Discover devices
	devs, err := DiscoverWithRoots(sysRoot, procRoot)
	if err != nil {
		t.Fatalf("DiscoverWithRoots failed: %v", err)
	}

	if len(devs) != 3 {
		t.Fatalf("expected 3 discovered devices, got %d", len(devs))
	}

	// Card 0: Tier 1 USB
	d0 := devs[0]
	if d0.Tier != urn.TierUSB || d0.URN != "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0" {
		t.Errorf("card 0: expected Tier 1 USB, got tier=%s, URN=%s", d0.Tier, d0.URN)
	}

	// Card 1: Tier 2 Path
	d1 := devs[1]
	if d1.Tier != urn.TierPath || d1.BusPath != "pci-0000:00:14.0-usb-0:2.1:1.0" {
		t.Errorf("card 1: expected Tier 2 Path, got tier=%s, URN=%s, BusPath=%s", d1.Tier, d1.URN, d1.BusPath)
	}

	// Card 2: Tier 3 ALSA
	d2 := devs[2]
	if d2.Tier != urn.TierALSA || d2.CardID != "PCH" || d2.URN != "urn:audio:hw:alsa:c=PCH:d=0" {
		t.Errorf("card 2: expected Tier 3 ALSA, got tier=%s, URN=%s", d2.Tier, d2.URN)
	}
}

func TestDiscoverWithRoots_MissingDirectory(t *testing.T) {
	devs, err := DiscoverWithRoots("/non/existent/sys", "/non/existent/proc")
	if err == nil {
		t.Errorf("expected error for non-existent sysRoot without procRoot, got nil")
	}
	if len(devs) != 0 {
		t.Errorf("expected 0 devices, got %d", len(devs))
	}
}
