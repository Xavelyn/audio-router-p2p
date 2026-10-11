package dsp

import (
	"encoding/binary"
	"sync"
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

func TestDTX_DefaultConfigAndExtendedHangover(t *testing.T) {
	// 1. Assert default configuration values
	cfg := DefaultDTXConfig()
	if cfg.HangoverDuration != 5*time.Second {
		t.Fatalf("expected DefaultDTXConfig.HangoverDuration == 5s, got %v", cfg.HangoverDuration)
	}
	if cfg.FrameDuration != 20*time.Millisecond {
		t.Fatalf("expected DefaultDTXConfig.FrameDuration == 20ms, got %v", cfg.FrameDuration)
	}

	dtx := NewDTXController(cfg)
	if dtx.hangoverFrames != 250 {
		t.Fatalf("expected dtx.hangoverFrames == 250, got %d", dtx.hangoverFrames)
	}

	// 2. Test fallback behavior when HangoverDuration <= 0
	fallbackCases := []struct {
		name string
		cfg  DTXConfig
	}{
		{
			name: "ZeroHangoverDuration",
			cfg: DTXConfig{
				Enabled:          true,
				SilenceThreshold: -50.0,
				HangoverDuration: 0,
				FrameDuration:    20 * time.Millisecond,
			},
		},
		{
			name: "NegativeHangoverDuration",
			cfg: DTXConfig{
				Enabled:          true,
				SilenceThreshold: -50.0,
				HangoverDuration: -1 * time.Second,
				FrameDuration:    20 * time.Millisecond,
			},
		},
		{
			name: "EmptyConfig",
			cfg: DTXConfig{
				Enabled: true,
			},
		},
	}

	for _, tc := range fallbackCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := NewDTXController(tc.cfg)
			if ctrl.config.HangoverDuration != 5*time.Second {
				t.Fatalf("%s: expected config.HangoverDuration fallback to 5s, got %v", tc.name, ctrl.config.HangoverDuration)
			}
			if ctrl.hangoverFrames != 250 {
				t.Fatalf("%s: expected hangoverFrames == 250, got %d", tc.name, ctrl.hangoverFrames)
			}
		})
	}

	// 3. Assert state transitions with 250 hangover frames:
	// Feeding 249 frames of silence after active audio stays in Hangover state,
	// and the 250th frame enters Silence state and triggers SID.
	samplesPerFrame := 960 // 20ms at 48kHz mono
	activeFrame := makeFrame(samplesPerFrame, 10000)
	silentFrame := makeFrame(samplesPerFrame, 0)

	// Step into Active state
	send, isSID, _ := dtx.ProcessFrame(activeFrame, 1)
	if !send || isSID {
		t.Fatalf("active frame: expected send=true, isSID=false, got send=%v, isSID=%v", send, isSID)
	}
	if dtx.State() != DTXStateActive {
		t.Fatalf("expected state Active, got %v", dtx.State())
	}

	// Feed 249 frames of silence -> must remain in Hangover state, send=true, isSID=false
	for i := 1; i <= 249; i++ {
		send, isSID, _ = dtx.ProcessFrame(silentFrame, 1)
		if !send || isSID {
			t.Fatalf("silence frame %d: expected send=true, isSID=false, got send=%v, isSID=%v", i, send, isSID)
		}
		if dtx.State() != DTXStateHangover {
			t.Fatalf("silence frame %d: expected state Hangover, got %v", i, dtx.State())
		}
	}

	// 250th frame of silence -> enters Silence state and triggers SID descriptor
	send, isSID, _ = dtx.ProcessFrame(silentFrame, 1)
	if !send || !isSID {
		t.Fatalf("250th silence frame: expected send=true, isSID=true (SID trigger), got send=%v, isSID=%v", send, isSID)
	}
	if dtx.State() != DTXStateSilence {
		t.Fatalf("expected state Silence on 250th frame, got %v", dtx.State())
	}
}

func TestDTX_ConcurrentAccess(t *testing.T) {
	cfg := DefaultDTXConfig()
	dtx := NewDTXController(cfg)
	silentFrame := makeFrame(960, 0)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader goroutines querying telemetry
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = dtx.State()
					_ = dtx.LastMetrics()
					_ = dtx.SuppressedPackets()
				}
			}
		}()
	}

	// Worker stepping the frame processing hot path
	for i := 0; i < 500; i++ {
		dtx.ProcessFrame(silentFrame, 1)
	}

	close(stop)
	wg.Wait()
}
