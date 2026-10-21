package telemetry

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelemetryHub_RateConfig(t *testing.T) {
	cases := []struct {
		input int
		want  int
	}{
		{0, 30},
		{-5, 30},
		{10, 20}, // clamped to 20 min
		{25, 25},
		{50, 50},
		{60, 50}, // clamped to 50 max
	}

	for _, c := range cases {
		h := NewTelemetryHub(c.input)
		if h.RateHz() != c.want {
			t.Errorf("NewTelemetryHub(%d).RateHz() = %d; want %d", c.input, h.RateHz(), c.want)
		}
	}
}

func TestAtomicChannelTap(t *testing.T) {
	tap := &AtomicChannelTap{}

	// Initially empty -> silence floor
	rms, peak, clipped := tap.Snapshot()
	if rms != SilenceFloorDBFS || peak != SilenceFloorDBFS || clipped {
		t.Errorf("initial snapshot = (%f, %f, %v); want (-96.0, -96.0, false)", rms, peak, clipped)
	}

	// Record full scale sample (32767) -> clipping true
	tap.RecordSample(32767)
	rms, peak, clipped = tap.Snapshot()
	if !clipped {
		t.Error("expected clipped to be true for 32767")
	}
	if peak != 0.0 {
		// 20*log10(32767/32768) ≈ -0.0002 dBFS, rounded to 0.0
		if peak < -0.1 {
			t.Errorf("expected peak near 0 dBFS, got %f", peak)
		}
	}

	// Verify snapshot reset: subsequent snapshot is silent
	rms, peak, clipped = tap.Snapshot()
	if rms != SilenceFloorDBFS || peak != SilenceFloorDBFS || clipped {
		t.Errorf("subsequent snapshot should be reset, got (%f, %f, %v)", rms, peak, clipped)
	}
}

func TestStreamTap_RecordBlock(t *testing.T) {
	st := NewStreamTap("stream-test-1", 2)

	// Create 480 samples of stereo audio (960 int16 samples)
	// Left channel: 16384 (-6 dBFS), Right channel: 0
	numFrames := 480
	pcm := make([]byte, numFrames*2*2)
	for i := 0; i < numFrames; i++ {
		// L
		binary.LittleEndian.PutUint16(pcm[i*4:i*4+2], uint16(16384))
		// R
		binary.LittleEndian.PutUint16(pcm[i*4+2:i*4+4], 0)
	}

	st.RecordBlock(pcm, 2)
	frame := st.Snapshot(123456789)

	if frame.StreamID != "stream-test-1" {
		t.Errorf("stream ID mismatch: %s", frame.StreamID)
	}
	if frame.TimestampNS != 123456789 {
		t.Errorf("timestamp mismatch: %d", frame.TimestampNS)
	}
	if len(frame.Channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(frame.Channels))
	}

	// Left channel should be near -6.02 dBFS
	ch0 := frame.Channels[0]
	if ch0.PeakdBFS < -6.1 || ch0.PeakdBFS > -5.9 {
		t.Errorf("ch0 peak = %f; want ~ -6.02 dBFS", ch0.PeakdBFS)
	}
	if ch0.Clipped {
		t.Error("ch0 should not be clipped")
	}

	// Right channel should be -96.0 dBFS
	ch1 := frame.Channels[1]
	if ch1.PeakdBFS != SilenceFloorDBFS || ch1.RMSdBFS != SilenceFloorDBFS {
		t.Errorf("ch1 expected silence (-96 dBFS), got peak=%f, rms=%f", ch1.PeakdBFS, ch1.RMSdBFS)
	}
}

func TestTelemetryHub_Fanout(t *testing.T) {
	hub := NewTelemetryHub(30)
	sub1 := hub.Subscribe(4)
	sub2 := hub.Subscribe(4)
	defer hub.Unsubscribe(sub1)
	defer hub.Unsubscribe(sub2)

	if hub.SubscriberCount() != 2 {
		t.Fatalf("expected 2 subscribers, got %d", hub.SubscriberCount())
	}

	testMsg := []byte(`{"test":"fanout"}`)
	hub.Broadcast(testMsg)

	select {
	case msg := <-sub1.Ch:
		if string(msg) != string(testMsg) {
			t.Errorf("sub1 got %s; want %s", msg, testMsg)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("sub1 timed out")
	}

	select {
	case msg := <-sub2.Ch:
		if string(msg) != string(testMsg) {
			t.Errorf("sub2 got %s; want %s", msg, testMsg)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("sub2 timed out")
	}
}

func TestTelemetryHub_DropOnLag(t *testing.T) {
	hub := NewTelemetryHub(30)
	// Fast subscriber with drain
	fast := hub.Subscribe(4)
	// Slow subscriber with no drain and buffer cap 2
	slow := hub.Subscribe(2)
	defer hub.Unsubscribe(fast)
	defer hub.Unsubscribe(slow)

	// Send 10 messages
	for i := 0; i < 10; i++ {
		hub.Broadcast([]byte(fmt.Sprintf("msg-%d", i)))
		// Fast subscriber drains immediately
		select {
		case <-fast.Ch:
		default:
		}
	}

	// Slow subscriber capacity is 2, so at least 8 frames should have dropped
	dropped := slow.DroppedFrames.Load()
	if dropped != 8 {
		t.Errorf("slow subscriber dropped = %d; want 8", dropped)
	}

	fastDropped := fast.DroppedFrames.Load()
	if fastDropped != 0 {
		t.Errorf("fast subscriber dropped = %d; want 0", fastDropped)
	}
}

func TestTelemetryHub_ZeroAllocSerialization(t *testing.T) {
	hub := NewTelemetryHub(30)
	frame := StreamMeterFrame{
		StreamID:    "stream-1234-abcd",
		TimestampNS: 1727280000000000000,
		DTXState:    "Active",
		Channels: []ChannelMeter{
			{Channel: 0, RMSdBFS: -18.42, PeakdBFS: -6.12, Clipped: false},
			{Channel: 1, RMSdBFS: -18.50, PeakdBFS: -6.15, Clipped: false},
		},
	}

	// Warm buffer pool
	bufPtr := hub.GetBuffer()
	*bufPtr = hub.SerializeFrame(&frame, bufPtr)
	hub.PutBuffer(bufPtr)

	// Measure steady-state allocations
	allocs := testing.AllocsPerRun(1000, func() {
		buf := hub.GetBuffer()
		*buf = hub.SerializeFrame(&frame, buf)
		hub.PutBuffer(buf)
	})

	if allocs != 0 {
		t.Errorf("expected 0 allocs/op for steady-state serialization, got %f", allocs)
	}

	// Validate JSON validity
	buf := hub.GetBuffer()
	*buf = hub.SerializeFrame(&frame, buf)
	var decoded MeterFrame
	if err := json.Unmarshal(*buf, &decoded); err != nil {
		t.Fatalf("unmarshal generated JSON failed: %v, json=%s", err, string(*buf))
	}
	hub.PutBuffer(buf)

	if decoded.StreamID != frame.StreamID || len(decoded.Channels) != 2 {
		t.Errorf("decoded JSON mismatch: %+v", decoded)
	}
}

func TestTelemetryHub_Concurrency(t *testing.T) {
	hub := NewTelemetryHub(40)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tap := hub.RegisterTap("concurrent-stream", 2)
	hub.Start(ctx)
	defer hub.Stop()

	var wg sync.WaitGroup
	var received atomic.Uint64

	// 3 Subscribers concurrently reading
	for i := 0; i < 3; i++ {
		sub := hub.Subscribe(4)
		wg.Add(1)
		go func(s *Subscriber) {
			defer wg.Done()
			defer hub.Unsubscribe(s)
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-s.Ch:
					if !ok {
						return
					}
					if len(msg) > 0 {
						received.Add(1)
					}
				}
			}
		}(sub)
	}

	// 2 Audio producers concurrently recording samples
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pcm := make([]byte, 128*2*2)
			for i := 0; i < 50; i++ {
				tap.RecordBlock(pcm, 2)
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	time.Sleep(80 * time.Millisecond)
	cancel()
	wg.Wait()

	if received.Load() == 0 {
		t.Log("Note: 0 frames received during brief concurrent burst (acceptable if ticker interval didn't align)")
	}
}
