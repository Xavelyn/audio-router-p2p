package audio

import (
	"testing"
)

func TestDeviceResolver_RegistrationAndAliases(t *testing.T) {
	resolver := NewDeviceResolver()

	dev := Device{
		ID:                "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0",
		URN:               "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0",
		LegacyID:          "2",
		RuntimeID:         2,
		Name:              "Focusrite Scarlett 2i2 USB",
		MaxInput:          2,
		MaxOutput:         2,
		DefaultSampleRate: 48000,
	}

	resolver.Register(dev)

	// Resolve by exact URN
	d1, ok1 := resolver.Resolve("urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0")
	if !ok1 || d1.Name != dev.Name {
		t.Fatalf("failed to resolve by URN: %+v", d1)
	}

	// Resolve by legacy ID "2"
	d2, ok2 := resolver.Resolve("2")
	if !ok2 || d2.URN != dev.URN {
		t.Fatalf("failed to resolve by legacy ID: %+v", d2)
	}

	// Resolve by ALSA string "hw:2" or "hw:2,0"
	d3, ok3 := resolver.Resolve("hw:2")
	if !ok3 || d3.URN != dev.URN {
		t.Fatalf("failed to resolve by hw:2: %+v", d3)
	}

	d4, ok4 := resolver.Resolve("hw:2,0")
	if !ok4 || d4.URN != dev.URN {
		t.Fatalf("failed to resolve by hw:2,0: %+v", d4)
	}

	// Resolve by name slug
	d5, ok5 := resolver.Resolve("focusrite-scarlett-2i2-usb")
	if !ok5 || d5.URN != dev.URN {
		t.Fatalf("failed to resolve by name slug: %+v", d5)
	}
}

func TestDeviceResolver_QuarantineAndGhost(t *testing.T) {
	resolver := NewDeviceResolver()

	dev := Device{
		ID:        "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0",
		URN:       "urn:audio:hw:usb:v=1235:p=8210:s=10042718:d=0",
		LegacyID:  "0",
		RuntimeID: 0,
		Name:      "Focusrite Scarlett 2i2 USB",
	}
	resolver.Register(dev)

	// Initially active
	d, _ := resolver.Resolve("0")
	if d.Status != "active" {
		t.Errorf("expected active status, got %s", d.Status)
	}

	// Quarantine
	resolver.Quarantine("0")
	if !resolver.IsQuarantined(dev.URN) {
		t.Fatal("expected device to be quarantined")
	}

	dQuar, _ := resolver.Resolve("0")
	if dQuar.Status != "quarantined" {
		t.Errorf("expected quarantined status, got %s", dQuar.Status)
	}

	// Open input should return Ghost stream without failing
	st, err := resolver.OpenInput("0", 48000, 2, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	if _, ok := st.(*GhostAudioStream); !ok {
		t.Errorf("expected GhostAudioStream for quarantined device, got %T", st)
	}
	_ = st.Close()

	// Rebind to activate
	vDev := NewVirtualAudioDevice()
	resolver.Rebind("0", vDev)

	if resolver.IsQuarantined(dev.URN) {
		t.Error("device should no longer be quarantined after Rebind")
	}
	dActive, _ := resolver.Resolve("0")
	if dActive.Status != "active" {
		t.Errorf("expected active status after rebind, got %s", dActive.Status)
	}
}

func TestDeviceResolver_AlsaNameRegex(t *testing.T) {
	resolver := NewDeviceResolver()
	dev := Device{
		ID:        "urn:audio:hw:alsa:pch:0",
		URN:       "urn:audio:hw:alsa:pch:0",
		LegacyID:  "5", // PortAudio index 5
		RuntimeID: 5,
		Name:      "HDA Intel PCH (hw:1,0)", // Real ALSA card 1, subdev 0
	}
	resolver.Register(dev)

	// Should resolve by real ALSA card "hw:1" and "hw:1,0"
	d1, ok1 := resolver.Resolve("hw:1")
	if !ok1 || d1.URN != dev.URN {
		t.Fatalf("failed to resolve by hw:1: %+v", d1)
	}

	d2, ok2 := resolver.Resolve("hw:1,0")
	if !ok2 || d2.URN != dev.URN {
		t.Fatalf("failed to resolve by hw:1,0: %+v", d2)
	}
}

func TestDeviceResolver_SiblingSubdeviceMigration(t *testing.T) {
	resolver := NewDeviceResolver()

	// Creative Sound Blaster Play! 3:
	// Subdevice 0: Playback ONLY (d=0)
	// Subdevice 1: Capture ONLY (d=1)
	spkURN := "urn:audio:hw:usb:v=041e:p=3232:s=0000005Z:d=0"
	micURN := "urn:audio:hw:usb:v=041e:p=3232:s=0000005Z:d=1"

	spkDev := Device{
		ID:        spkURN,
		URN:       spkURN,
		LegacyID:  "0",
		RuntimeID: 0,
		Name:      "Sound Blaster Play! 3 (hw:1,0)",
		MaxInput:  0,
		MaxOutput: 2,
	}
	micDev := Device{
		ID:        micURN,
		URN:       micURN,
		LegacyID:  "1",
		RuntimeID: 1,
		Name:      "Sound Blaster Play! 3 (hw:1,1)",
		MaxInput:  2,
		MaxOutput: 0,
	}

	resolver.Register(spkDev)
	resolver.Register(micDev)

	// If a user or legacy room configuration requested spkURN (d=0) as an INPUT:
	// Resolver OpenInput should auto-migrate to micURN (d=1)
	inStream, err := resolver.OpenInput(spkURN, 48000, 2, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	if inStream == nil {
		t.Fatal("expected valid input stream")
	}
	_ = inStream.Close()

	// ResolveInput should resolve to micURN
	migratedIn, okIn := resolver.ResolveInput(spkURN)
	if !okIn || migratedIn.URN != micURN {
		t.Errorf("expected ResolveInput migration to micURN (%s), got %+v", micURN, migratedIn)
	}

	// Conversely, if micURN (d=1) is requested as an OUTPUT:
	// Resolver OpenOutput should auto-migrate to spkURN (d=0)
	outStream, err := resolver.OpenOutput(micURN, 48000, 2, true)
	if err != nil {
		t.Fatalf("OpenOutput error: %v", err)
	}
	if outStream == nil {
		t.Fatal("expected valid output stream")
	}
	_ = outStream.Close()

	// ResolveOutput should resolve to spkURN
	migratedOut, okOut := resolver.ResolveOutput(micURN)
	if !okOut || migratedOut.URN != spkURN {
		t.Errorf("expected ResolveOutput migration to spkURN (%s), got %+v", spkURN, migratedOut)
	}

	// Verify that standard Resolve() was NOT corrupted in aliases:
	// spkURN must still resolve to spkURN, micURN must still resolve to micURN.
	dSpk, okSpk := resolver.Resolve(spkURN)
	if !okSpk || dSpk.URN != spkURN {
		t.Errorf("canonical Resolve(%s) corrupted: got %s", spkURN, dSpk.URN)
	}
	dMic, okMic := resolver.Resolve(micURN)
	if !okMic || dMic.URN != micURN {
		t.Errorf("canonical Resolve(%s) corrupted: got %s", micURN, dMic.URN)
	}
}
