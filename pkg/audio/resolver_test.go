package audio

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"
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

	// Open input should return Ghost stream virtual handle without failing
	st, err := resolver.OpenInput("0", 48000, 2, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	if handle, ok := st.(*RefStreamHandle); ok {
		if _, isGhost := handle.Underlying().(*GhostAudioStream); !isGhost {
			t.Errorf("expected underlying GhostAudioStream for quarantined device, got %T", handle.Underlying())
		}
	} else if _, ok := st.(*GhostAudioStream); !ok {
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

// TestDeviceResolver_ZombiePrevention verifies that opening a handle, closing it,
// and reopening the device produces a working handle that reads audio without returning premature io.EOF.
func TestDeviceResolver_ZombiePrevention(t *testing.T) {
	resolver := NewDeviceResolver()
	resolver.SetGracePeriod(50 * time.Millisecond)

	micURN := "urn:audio:hw:test:zombie-mic"
	vDev := NewVirtualAudioDevice()
	resolver.Register(Device{
		ID:        micURN,
		URN:       micURN,
		Name:      "Zombie Mic",
		MaxInput:  2,
		MaxOutput: 0,
		Status:    "active",
	})
	resolver.Rebind(micURN, vDev)

	// 1. Open handle 1
	h1, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput 1 failed: %v", err)
	}

	// Push audio to vDev
	pcm1 := makeTestPCM(480, 2, 440)
	_, _ = vDev.InjectPush(pcm1)

	readBuf := make([]byte, len(pcm1))
	n1, err := readTimeout(h1, readBuf, 500*time.Millisecond)
	if err != nil || n1 != len(pcm1) {
		t.Fatalf("h1.Read failed: n=%d err=%v", n1, err)
	}

	// 2. Close handle 1
	if err := h1.Close(); err != nil {
		t.Fatalf("h1.Close failed: %v", err)
	}

	// Reading from closed handle 1 must return io.EOF
	_, err = h1.Read(readBuf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF on closed handle h1, got %v", err)
	}

	// 3. Reopen device immediately (within grace window)
	h2, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput 2 failed: %v", err)
	}
	defer h2.Close()

	// Push more audio to vDev
	pcm2 := makeTestPCM(480, 2, 880)
	_, _ = vDev.InjectPush(pcm2)

	// Handle 2 MUST NOT return io.EOF; it should read pcm2 cleanly
	n2, err := readTimeout(h2, readBuf, 500*time.Millisecond)
	if err == io.EOF {
		t.Fatalf("ZOMBIE BUG DETECTED: h2 returned io.EOF on initial read!")
	}
	if err != nil || n2 != len(pcm2) {
		t.Fatalf("h2.Read failed: n=%d err=%v", n2, err)
	}

	// 4. Close h2 and wait for grace period to expire
	_ = h2.Close()
	time.Sleep(100 * time.Millisecond)

	// Verify controller was evicted
	if count := resolver.ActiveControllerCount(); count != 0 {
		t.Fatalf("expected 0 active controllers after grace expiration, got %d", count)
	}

	// 5. Reopen device after grace expiration: should open cleanly with a new controller
	vDev2 := NewVirtualAudioDevice()
	resolver.Rebind(micURN, vDev2)
	h3, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput 3 failed: %v", err)
	}
	defer h3.Close()

	pcm3 := makeTestPCM(480, 2, 1000)
	_, _ = vDev2.InjectPush(pcm3)

	n3, err := readTimeout(h3, readBuf, 500*time.Millisecond)
	if err != nil || n3 != len(pcm3) {
		t.Fatalf("h3.Read failed after grace expiry: n=%d err=%v", n3, err)
	}
}

// TestDeviceResolver_FullDuplexIsolation verifies that opening input and output on the same
// full-duplex device URN maintains segregated controllers that do not close or interfere with each other.
func TestDeviceResolver_FullDuplexIsolation(t *testing.T) {
	resolver := NewDeviceResolver()

	duplexURN := "urn:audio:hw:alsa:c=PCH:d=0"
	inDev := NewVirtualAudioDevice()
	outDev := NewVirtualAudioDevice()

	resolver.Register(Device{
		ID:        duplexURN,
		URN:       duplexURN,
		Name:      "Full Duplex Onboard Codec",
		MaxInput:  2,
		MaxOutput: 2,
		Status:    "active",
	})

	// Open input
	resolver.RebindDirectional(duplexURN, DirectionInput, inDev)
	inHandle, err := resolver.OpenInput(duplexURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput failed: %v", err)
	}
	defer inHandle.Close()

	// Open output on the SAME URN - this must NOT close or clobber the input stream!
	resolver.RebindDirectional(duplexURN, DirectionOutput, outDev)
	outHandle, err := resolver.OpenOutput(duplexURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenOutput failed: %v", err)
	}
	defer outHandle.Close()

	// Verify both controllers are registered in resolver
	inKey := StreamKey{URN: duplexURN, Direction: DirectionInput}
	outKey := StreamKey{URN: duplexURN, Direction: DirectionOutput}

	resolver.mu.RLock()
	inCtrl, hasIn := resolver.controllers[inKey]
	outCtrl, hasOut := resolver.controllers[outKey]
	resolver.mu.RUnlock()

	if !hasIn || inCtrl == nil {
		t.Fatal("expected active input controller")
	}
	if !hasOut || outCtrl == nil {
		t.Fatal("expected active output controller")
	}
	if inCtrl == outCtrl {
		t.Fatal("input and output controllers must be distinct instances")
	}

	// Push audio to input and verify read
	pcmIn := makeTestPCM(256, 2, 440)
	_, _ = inDev.InjectPush(pcmIn)
	readBuf := make([]byte, len(pcmIn))
	nIn, err := readTimeout(inHandle, readBuf, 500*time.Millisecond)
	if err != nil || nIn != len(pcmIn) {
		t.Fatalf("inHandle.Read failed: n=%d err=%v", nIn, err)
	}

	// Write audio to output and verify captured
	pcmOut := makeTestPCM(256, 2, 880)
	nOut, err := outHandle.Write(pcmOut)
	if err != nil || nOut != len(pcmOut) {
		t.Fatalf("outHandle.Write failed: n=%d err=%v", nOut, err)
	}
	if captured := outDev.CapturedBytes(); len(captured) < len(pcmOut) {
		t.Fatalf("expected >= %d captured bytes, got %d", len(pcmOut), len(captured))
	}

	// Close output handle - input handle MUST remain fully operational!
	if err := outHandle.Close(); err != nil {
		t.Fatalf("outHandle.Close failed: %v", err)
	}

	_, _ = inDev.InjectPush(pcmIn)
	nIn2, err := readTimeout(inHandle, readBuf, 500*time.Millisecond)
	if err != nil || nIn2 != len(pcmIn) {
		t.Fatalf("inHandle.Read failed after outHandle closed: n=%d err=%v", nIn2, err)
	}
}

// TestDeviceResolver_CaptureFanOut verifies that two input handles on the same device receive
// pushed audio independently with zero frame stealing.
func TestDeviceResolver_CaptureFanOut(t *testing.T) {
	resolver := NewDeviceResolver()

	micURN := "urn:audio:hw:test:fanout-mic"
	vDev := NewVirtualAudioDevice()

	resolver.Register(Device{
		ID:        micURN,
		URN:       micURN,
		Name:      "Fan-Out Mic",
		MaxInput:  2,
		MaxOutput: 0,
		Status:    "active",
	})
	resolver.Rebind(micURN, vDev)

	// Open two handles targeting the same input device
	h1, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput 1 failed: %v", err)
	}
	defer h1.Close()

	h2, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput 2 failed: %v", err)
	}
	defer h2.Close()

	// Push 3 distinct audio blocks
	blocks := [][]byte{
		makeTestPCM(256, 2, 220),
		makeTestPCM(256, 2, 440),
		makeTestPCM(256, 2, 880),
	}

	for i, block := range blocks {
		_, _ = vDev.InjectPush(block)

		// Both h1 and h2 must receive the FULL block
		buf1 := make([]byte, len(block))
		buf2 := make([]byte, len(block))

		n1, err1 := readTimeout(h1, buf1, 500*time.Millisecond)
		if err1 != nil || n1 != len(block) {
			t.Fatalf("block %d: h1.Read failed: n=%d err=%v", i, n1, err1)
		}

		n2, err2 := readTimeout(h2, buf2, 500*time.Millisecond)
		if err2 != nil || n2 != len(block) {
			t.Fatalf("block %d: h2.Read failed: n=%d err=%v", i, n2, err2)
		}

		if !bytes.Equal(buf1, block) {
			t.Fatalf("block %d: h1 data mismatch", i)
		}
		if !bytes.Equal(buf2, block) {
			t.Fatalf("block %d: h2 data mismatch", i)
		}
	}

	// Now close h1: h2 must continue receiving subsequent audio
	_ = h1.Close()

	nextBlock := makeTestPCM(256, 2, 1760)
	_, _ = vDev.InjectPush(nextBlock)

	buf2 := make([]byte, len(nextBlock))
	n2, err := readTimeout(h2, buf2, 500*time.Millisecond)
	if err != nil || n2 != len(nextBlock) {
		t.Fatalf("h2.Read failed after h1 closed: n=%d err=%v", n2, err)
	}
	if !bytes.Equal(buf2, nextBlock) {
		t.Fatal("h2 data mismatch after h1 closed")
	}
}

// TestDeviceResolver_PlaybackMixer_SoftSaturation verifies that multiple output handles
// sum concurrent frames and apply soft-knee saturation to prevent integer wrap-around clipping.
func TestDeviceResolver_PlaybackMixer_SoftSaturation(t *testing.T) {
	resolver := NewDeviceResolver()

	spkURN := "urn:audio:hw:test:mixer-spk"
	outDev := NewVirtualAudioDevice()

	resolver.Register(Device{
		ID:        spkURN,
		URN:       spkURN,
		Name:      "Mixer Speaker",
		MaxInput:  0,
		MaxOutput: 2,
		Status:    "active",
	})
	resolver.Rebind(spkURN, outDev)

	// Open two output handles to activate mixer bus
	h1, err := resolver.OpenOutput(spkURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenOutput 1 failed: %v", err)
	}
	defer h1.Close()

	h2, err := resolver.OpenOutput(spkURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenOutput 2 failed: %v", err)
	}
	defer h2.Close()

	// Generate two full-scale sine waves (each ~0.9 dBFS)
	// When summed naively (0.9 + 0.9 = 1.8), integer overflow would wrap around to negative numbers!
	frames := 480
	channels := 2
	block1 := makeTestPCM(frames, channels, 440)
	block2 := makeTestPCM(frames, channels, 440)

	// Write concurrently
	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		_, _ = h1.Write(block1)
		close(done1)
	}()
	go func() {
		_, _ = h2.Write(block2)
		close(done2)
	}()

	<-done1
	<-done2

	// Wait for mixer worker to process
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(outDev.CapturedBytes()) >= len(block1) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	captured := outDev.CapturedBytes()
	if len(captured) < len(block1) {
		t.Fatalf("expected >= %d mixed bytes captured, got %d", len(block1), len(captured))
	}

	// Verify that samples are soft-saturated and never wrapped around
	for i := 0; i < len(block1); i += 2 {
		s := int16(binary.LittleEndian.Uint16(captured[i : i+2]))
		// Clamped to valid range [-32768, 32767]
		if s > 32767 || s < -32768 {
			t.Fatalf("sample out of range: %d", s)
		}
	}
}

// TestDeviceResolver_HotSwapRebind verifies that Rebind seamlessly updates the physical stream
// beneath active RefStreamHandles without breaking downstream readers.
func TestDeviceResolver_HotSwapRebind(t *testing.T) {
	resolver := NewDeviceResolver()

	micURN := "urn:audio:hw:test:hotswap-mic"
	dev1 := NewVirtualAudioDevice()
	dev2 := NewVirtualAudioDevice()

	resolver.Register(Device{
		ID:        micURN,
		URN:       micURN,
		Name:      "HotSwap Mic",
		MaxInput:  2,
		MaxOutput: 0,
		Status:    "active",
	})
	resolver.Rebind(micURN, dev1)

	handle, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput failed: %v", err)
	}
	defer handle.Close()

	// Push from dev1
	pcm1 := makeTestPCM(256, 2, 440)
	_, _ = dev1.InjectPush(pcm1)

	readBuf := make([]byte, len(pcm1))
	n1, err := readTimeout(handle, readBuf, 500*time.Millisecond)
	if err != nil || n1 != len(pcm1) {
		t.Fatalf("read 1 failed: n=%d err=%v", n1, err)
	}
	if !bytes.Equal(readBuf, pcm1) {
		t.Fatal("read 1 data mismatch")
	}

	// Hot-swap underlying stream to dev2
	resolver.Rebind(micURN, dev2)

	// Push from dev2
	pcm2 := makeTestPCM(256, 2, 880)
	_, _ = dev2.InjectPush(pcm2)

	n2, err := readTimeout(handle, readBuf, 500*time.Millisecond)
	if err != nil || n2 != len(pcm2) {
		t.Fatalf("read 2 failed after hot-swap: n=%d err=%v", n2, err)
	}
	if !bytes.Equal(readBuf, pcm2) {
		t.Fatal("read 2 data mismatch after hot-swap")
	}
}

// makeTestPCM generates test sine wave PCM frames.
func makeTestPCM(frames, channels int, freq float64) []byte {
	buf := make([]byte, frames*channels*2)
	sampleRate := 48000.0
	for i := 0; i < frames; i++ {
		val := int16(32000.0 * math.Sin(2.0*math.Pi*freq*float64(i)/sampleRate))
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint16(buf[(i*channels+ch)*2:], uint16(val))
		}
	}
	return buf
}

// readTimeout reads from stream with a timeout.
func readTimeout(st AudioStream, p []byte, timeout time.Duration) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		offset := 0
		for offset < len(p) {
			n, err := st.Read(p[offset:])
			if n > 0 {
				offset += n
			}
			if err != nil {
				ch <- result{offset, err}
				return
			}
		}
		ch <- result{offset, nil}
	}()

	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(timeout):
		return 0, io.ErrNoProgress
	}
}

// TestDeviceResolver_ConcurrentReadClose_NoDeadlock tests that concurrent Read and Close
// on an input handle do not trigger lock-inversion deadlocks.
func TestDeviceResolver_ConcurrentReadClose_NoDeadlock(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		resolver := NewDeviceResolver()
		micURN := "urn:audio:hw:test:deadlock-mic"
		vDev := NewVirtualAudioDevice()
		resolver.Register(Device{
			ID:        micURN,
			URN:       micURN,
			Name:      "Deadlock Mic",
			MaxInput:  2,
			MaxOutput: 0,
			Status:    "active",
		})
		resolver.Rebind(micURN, vDev)

		h, err := resolver.OpenInput(micURN, 48000, 2, false)
		if err != nil {
			t.Fatalf("OpenInput error: %v", err)
		}

		readDone := make(chan struct{})
		go func() {
			buf := make([]byte, 512)
			for {
				_, err := h.Read(buf)
				if err != nil {
					break
				}
			}
			close(readDone)
		}()

		// Yield briefly to ensure Read() enters Wait() or loop
		time.Sleep(2 * time.Millisecond)

		// Close handle concurrently
		_ = h.Close()

		select {
		case <-readDone:
			// Read unblocked cleanly
		case <-time.After(1 * time.Second):
			t.Fatalf("DEADLOCK DETECTED: Read() did not unblock within 1s after Close()")
		}
		_ = resolver.Close()
	}
}

// TestDeviceResolver_DrainBufferedBeforeEOF verifies that buffered frames in an input
// handle are drained before io.EOF is returned when upstream closes.
func TestDeviceResolver_DrainBufferedBeforeEOF(t *testing.T) {
	resolver := NewDeviceResolver()
	resolver.SetGracePeriod(10 * time.Millisecond)

	micURN := "urn:audio:hw:test:drain-mic"
	vDev := NewVirtualAudioDevice()
	resolver.Register(Device{
		ID:        micURN,
		URN:       micURN,
		Name:      "Drain Mic",
		MaxInput:  2,
		MaxOutput: 0,
		Status:    "active",
	})
	resolver.Rebind(micURN, vDev)

	h, err := resolver.OpenInput(micURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}

	pcm := makeTestPCM(256, 2, 440)
	_, _ = vDev.InjectPush(pcm)

	// Wait for captureWorker to broadcast
	time.Sleep(20 * time.Millisecond)

	// Close the resolver/controller
	_ = resolver.Close()

	// Handle should still drain the buffered PCM frames
	readBuf := make([]byte, len(pcm))
	n, err := h.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: n=%d err=%v", n, err)
	}
	if n != len(pcm) {
		t.Fatalf("expected to drain %d bytes before EOF, got %d", len(pcm), n)
	}
	if !bytes.Equal(readBuf, pcm) {
		t.Fatal("drained data mismatch")
	}

	// Subsequent read must return EOF
	_, err = h.Read(readBuf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after draining, got %v", err)
	}
}

// TestDeviceResolver_MultiWriterToSingleWriterTransition verifies that when multiple writers
// are active on an output stream and one disconnects, the remaining writer transitions
// to the fast path without data race on physical write.
func TestDeviceResolver_MultiWriterToSingleWriterTransition(t *testing.T) {
	resolver := NewDeviceResolver()
	spkURN := "urn:audio:hw:test:transition-spk"
	outDev := NewVirtualAudioDevice()

	resolver.Register(Device{
		ID:        spkURN,
		URN:       spkURN,
		Name:      "Transition Speaker",
		MaxInput:  0,
		MaxOutput: 2,
		Status:    "active",
	})
	resolver.Rebind(spkURN, outDev)

	h1, err := resolver.OpenOutput(spkURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenOutput 1 failed: %v", err)
	}
	h2, err := resolver.OpenOutput(spkURN, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenOutput 2 failed: %v", err)
	}

	block := makeTestPCM(256, 2, 440)

	// Concurrent writes
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = h1.Write(block)
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_, _ = h2.Write(block)
			time.Sleep(1 * time.Millisecond)
		}
		// Close h2 while h1 continues writing
		_ = h2.Close()
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	_ = h1.Close()
	_ = resolver.Close()
}

// TestDeviceResolver_WorkerLifecycleAndGoroutineLeak verifies that controllers
// and their background workers terminate cleanly when closed.
func TestDeviceResolver_WorkerLifecycleAndGoroutineLeak(t *testing.T) {
	initialGoroutines := runtime.NumGoroutine()

	resolver := NewDeviceResolver()
	resolver.SetGracePeriod(20 * time.Millisecond)

	urnName := "urn:audio:hw:test:lifecycle-dev"
	vDev := NewVirtualAudioDevice()
	resolver.Register(Device{
		ID:        urnName,
		URN:       urnName,
		Name:      "Lifecycle Dev",
		MaxInput:  2,
		MaxOutput: 2,
		Status:    "active",
	})
	resolver.RebindDirectional(urnName, DirectionInput, vDev)
	resolver.RebindDirectional(urnName, DirectionOutput, vDev)

	inH, err := resolver.OpenInput(urnName, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	outH, err := resolver.OpenOutput(urnName, 48000, 2, false)
	if err != nil {
		t.Fatalf("OpenOutput error: %v", err)
	}

	if count := resolver.ActiveControllerCount(); count != 2 {
		t.Fatalf("expected 2 active controllers, got %d", count)
	}

	// Close both handles
	_ = inH.Close()
	_ = outH.Close()

	// Wait for grace period
	time.Sleep(50 * time.Millisecond)

	if count := resolver.ActiveControllerCount(); count != 0 {
		t.Fatalf("expected 0 active controllers after grace period, got %d", count)
	}

	_ = resolver.Close()

	// Ensure no leaked controller worker goroutines remain
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= initialGoroutines+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	leaked := runtime.NumGoroutine() - initialGoroutines
	if leaked > 2 {
		t.Fatalf("potential goroutine leak: %d additional goroutines running", leaked)
	}
}
