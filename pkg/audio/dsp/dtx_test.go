package dsp

import (
	"encoding/binary"
	"testing"
	"time"
)

// makeFrame generates a 16-bit mono PCM frame with the given amplitude.
func makeFrame(samples int, amp int16) []byte {
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(amp))
	}
	return buf
}

func TestDTXStateMachine_FullCycle(t *testing.T) {
	// 5 frames hangover (100ms), 10 frames SID interval (200ms), 20ms frame duration
	cfg := DTXConfig{
		Enabled:          true,
		SilenceThreshold: -50.0,
		HangoverDuration: 100 * time.Millisecond, // 5 frames
		SIDInterval:      200 * time.Millisecond, // 10 frames
		FrameDuration:    20 * time.Millisecond,
	}

	dtx := NewDTXController(cfg)
	samplesPerFrame := 960 // 20ms at 48kHz mono

	activeFrame := makeFrame(samplesPerFrame, 10000) // ~ -10 dBFS
	silentFrame := makeFrame(samplesPerFrame, 0)     // -96 dBFS

	// 1. Initial active frames
	for i := 0; i < 3; i++ {
		send, isSID, _ := dtx.ProcessFrame(activeFrame, 1)
		if !send || isSID {
			t.Fatalf("frame %d: expected send=true, isSID=false, got send=%v, isSID=%v", i, send, isSID)
		}
		if dtx.State() != DTXStateActive {
			t.Fatalf("expected state Active, got %v", dtx.State())
		}
	}

	// 2. Transition into Hangover (5 frames countdown: frames 0..3 are hangover, frame 4 expires)
	for i := 0; i < 4; i++ {
		send, isSID, _ := dtx.ProcessFrame(silentFrame, 1)
		if !send || isSID {
			t.Fatalf("hangover frame %d: expected send=true, isSID=false, got send=%v, isSID=%v", i, send, isSID)
		}
		if dtx.State() != DTXStateHangover {
			t.Fatalf("expected state Hangover, got %v", dtx.State())
		}
	}

	// Frame 5 of silence: hangover counter reaches 0 -> enters Silence and dispatches first SID
	send, isSID, _ := dtx.ProcessFrame(silentFrame, 1)
	if !send || !isSID {
		t.Fatalf("hangover expiry frame: expected send=true, isSID=true, got send=%v, isSID=%v", send, isSID)
	}
	if dtx.State() != DTXStateSilence {
		t.Fatalf("expected state Silence, got %v", dtx.State())
	}

	// 3. In Silence: next 9 frames should be suppressed
	for i := 1; i <= 9; i++ {
		send, isSID, _ := dtx.ProcessFrame(silentFrame, 1)
		if send || isSID {
			t.Fatalf("silence suppressed frame %d: expected send=false, isSID=false, got send=%v, isSID=%v", i, send, isSID)
		}
		if dtx.State() != DTXStateSilence {
			t.Fatalf("expected state Silence, got %v", dtx.State())
		}
	}

	if dtx.SuppressedPackets() != 9 {
		t.Fatalf("expected 9 suppressed packets, got %d", dtx.SuppressedPackets())
	}

	// 10th frame in silence: SID interval reached -> emit SID packet
	send, isSID, _ = dtx.ProcessFrame(silentFrame, 1)
	if !send || !isSID {
		t.Fatalf("periodic SID frame: expected send=true, isSID=true, got send=%v, isSID=%v", send, isSID)
	}
	if dtx.State() != DTXStateSilence {
		t.Fatalf("expected state Silence, got %v", dtx.State())
	}

	// Suppressed count should remain 9 because SID was sent
	if dtx.SuppressedPackets() != 9 {
		t.Fatalf("expected 9 suppressed packets, got %d", dtx.SuppressedPackets())
	}

	// 4. Re-activation: speech resumes
	send, isSID, _ = dtx.ProcessFrame(activeFrame, 1)
	if !send || isSID {
		t.Fatalf("re-activation: expected send=true, isSID=false, got send=%v, isSID=%v", send, isSID)
	}
	if dtx.State() != DTXStateActive {
		t.Fatalf("expected state Active, got %v", dtx.State())
	}
}

func TestDTX_Disabled(t *testing.T) {
	cfg := DTXConfig{
		Enabled: false,
	}
	dtx := NewDTXController(cfg)
	silentFrame := makeFrame(960, 0)

	for i := 0; i < 50; i++ {
		send, isSID, _ := dtx.ProcessFrame(silentFrame, 1)
		if !send || isSID {
			t.Fatalf("disabled DTX should always send active audio: send=%v, isSID=%v", send, isSID)
		}
	}
	if dtx.SuppressedPackets() != 0 {
		t.Fatalf("expected 0 suppressed packets when disabled, got %d", dtx.SuppressedPackets())
	}
}
