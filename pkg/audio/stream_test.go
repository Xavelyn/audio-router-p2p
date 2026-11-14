package audio

import (
	"io"
	"testing"
	"time"
)

// TestRingBufWrite_DropAccounting verifies the exact dropped-byte contract of
// ringBuf.write (ADR 0034): 0 when the write fits, frame-aligned exact bytes
// when the ring forces drops, the truncation case for oversized writes, and
// the partial-n edge.
func TestRingBufWrite_DropAccounting(t *testing.T) {
	const frameSize = 2 // mono int16

	t.Run("normal write fits returns 0", func(t *testing.T) {
		rb := newRingBuf(64, frameSize) // cap 64
		if got := rb.write(make([]byte, 32)); got != 0 {
			t.Fatalf("dropped=%d, want 0 for a fitting write", got)
		}
		if rb.n != 32 {
			t.Fatalf("n=%d, want 32", rb.n)
		}
		// A write that exactly fills the remaining space also drops nothing.
		if got := rb.write(make([]byte, 32)); got != 0 {
			t.Fatalf("dropped=%d, want 0 for an exactly-filling write", got)
		}
		if rb.n != 64 {
			t.Fatalf("n=%d, want 64", rb.n)
		}
	})

	t.Run("full ring write drops exact frame-aligned bytes", func(t *testing.T) {
		rb := newRingBuf(16, frameSize) // cap 16
		first := make([]byte, 16)
		for i := 0; i < 8; i++ {
			first[i] = 0xAB // oldest half
		}
		for i := 8; i < 16; i++ {
			first[i] = 0xCD // newest half
		}
		if got := rb.write(first); got != 0 {
			t.Fatalf("initial fill dropped=%d, want 0", got)
		}
		// Ring is full; writing 8 bytes must evict exactly the 8 oldest bytes.
		second := make([]byte, 8)
		for i := range second {
			second[i] = 0xEE
		}
		if got := rb.write(second); got != 8 {
			t.Fatalf("dropped=%d, want 8", got)
		}
		if rb.n != 16 {
			t.Fatalf("n=%d, want 16 (ring stays full)", rb.n)
		}
		// Verify drops came from the OLDEST side: the 0xAB half must be gone,
		// ring contents must be [0xCD x8, 0xEE x8].
		readBuf := make([]byte, 16)
		if got := rb.read(readBuf); got != 16 {
			t.Fatalf("read=%d, want 16", got)
		}
		for i := 0; i < 8; i++ {
			if readBuf[i] != 0xCD {
				t.Fatalf("byte %d = 0x%02X, want 0xCD (oldest-side eviction violated)", i, readBuf[i])
			}
		}
		for i := 8; i < 16; i++ {
			if readBuf[i] != 0xEE {
				t.Fatalf("byte %d = 0x%02X, want 0xEE", i, readBuf[i])
			}
		}
	})

	t.Run("oversized write truncates to newest cap bytes", func(t *testing.T) {
		rb := newRingBuf(16, frameSize) // cap 16
		// len(p) == 2*cap: only the newest cap bytes are kept.
		if got := rb.write(make([]byte, 32)); got != 32-16 {
			t.Fatalf("dropped=%d, want %d (len(p)-cap)", got, 32-16)
		}
		if rb.n != 16 {
			t.Fatalf("n=%d, want 16", rb.n)
		}
	})

	t.Run("oversized write on partially filled ring", func(t *testing.T) {
		rb := newRingBuf(16, frameSize)
		if got := rb.write(make([]byte, 4)); got != 0 {
			t.Fatalf("initial fill dropped=%d, want 0", got)
		}
		// 24 >= 16 → 8 bytes truncated up front, then the 4 oldest bytes
		// (two frames) are evicted to make room: 8 + 4 = 12 exact.
		if got := rb.write(make([]byte, 24)); got != 12 {
			t.Fatalf("dropped=%d, want 12 (8 truncated + 4 evicted)", got)
		}
		if rb.n != 16 {
			t.Fatalf("n=%d, want 16", rb.n)
		}
	})

	t.Run("zero-length write returns 0 without panic", func(t *testing.T) {
		rb := newRingBuf(16, frameSize)
		rb.write(make([]byte, 8))
		if got := rb.write(nil); got != 0 {
			t.Fatalf("dropped=%d, want 0 for nil write", got)
		}
		if got := rb.write([]byte{}); got != 0 {
			t.Fatalf("dropped=%d, want 0 for empty write", got)
		}
		if rb.n != 8 {
			t.Fatalf("n=%d, want 8 (unchanged)", rb.n)
		}
	})

	t.Run("partial n drops exactly the remaining bytes", func(t *testing.T) {
		rb := newRingBuf(16, frameSize) // cap 16, frameSize 2
		// Leave a lone partial byte in the ring.
		if got := rb.write(make([]byte, 1)); got != 0 {
			t.Fatalf("partial fill dropped=%d, want 0", got)
		}
		// A write exactly equal to cap: no truncation (len(p) >= cap keeps
		// all 16), but the loop must evict the single partial byte first.
		if got := rb.write(make([]byte, 16)); got != 1 {
			t.Fatalf("dropped=%d, want 1 (the lone partial byte)", got)
		}
		if rb.n != 16 {
			t.Fatalf("n=%d, want 16", rb.n)
		}
	})
}

func TestEmulatorInputRead(t *testing.T) {
	in, err := OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	defer in.Close()

	time.Sleep(20 * time.Millisecond)
	buf := make([]byte, 1024)
	n, err := in.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if n == 0 {
		t.Fatal("expected non-zero audio data")
	}
	// Reads must be aligned to frame boundaries (2 bytes for mono int16).
	if n%2 != 0 {
		t.Fatalf("read returned %d bytes, not frame-aligned", n)
	}
}

func TestEmulatorInputEOFAfterClose(t *testing.T) {
	in, err := OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	_ = in.Close()

	// Drain whatever was buffered; eventually Read must return io.EOF.
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n == 0 {
			t.Fatal("got 0 bytes with nil error after close")
		}
	}
}

func TestEmulatorBufferBounded(t *testing.T) {
	// With a slow consumer the ring buffer must stay bounded.
	in, err := OpenInput(0, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenInput error: %v", err)
	}
	defer in.Close()

	// Let the producer run without a consumer for a while.
	time.Sleep(150 * time.Millisecond)

	ei, ok := in.(*emulatorInput)
	if !ok {
		t.Fatal("expected emulator input")
	}
	ei.mu.Lock()
	n := ei.buf.n
	capBytes := cap(ei.buf.data)
	ei.mu.Unlock()
	if n > capBytes {
		t.Fatalf("buffer grew beyond capacity: %d > %d", n, capBytes)
	}
}

func TestEmulatorOutputWrite(t *testing.T) {
	out, err := OpenOutput(1, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenOutput error: %v", err)
	}
	defer out.Close()

	payload := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}
	n, err := out.Write(payload)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("write length mismatch: got %d want %d", n, len(payload))
	}
	if got := OutputBufferSize(out); got != len(payload) {
		t.Fatalf("buffered length mismatch: got %d want %d", got, len(payload))
	}
}

func TestEmulatorInputNormalizesParams(t *testing.T) {
	// The emulator is lenient: it coerces invalid params to sane defaults.
	in, err := OpenInput(0, 0, 0, true)
	if err != nil {
		t.Fatalf("OpenInput: %v", err)
	}
	defer in.Close()
	ei, ok := in.(*emulatorInput)
	if !ok {
		t.Fatal("expected emulator input")
	}
	if ei.sampleRate != 44100 {
		t.Fatalf("sample rate not normalized: %d", ei.sampleRate)
	}
	if ei.channels != 1 {
		t.Fatalf("channels not normalized: %d", ei.channels)
	}
	// And the emulator must still produce frame-aligned audio.
	buf := make([]byte, 2048)
	n, err := in.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n%2 != 0 {
		t.Fatalf("read %d bytes, not frame-aligned", n)
	}
}

func TestEmulatorOutputDrained(t *testing.T) {
	// Case 1: Empty on close
	out1, err := OpenOutput(1, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenOutput: %v", err)
	}
	eo1, ok := out1.(*emulatorOutput)
	if !ok {
		t.Fatal("expected emulatorOutput")
	}
	if eo1.Drained() {
		t.Fatal("expected Drained() to be false before close")
	}
	if err := eo1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !eo1.Drained() {
		t.Fatal("expected Drained() to be true immediately when closed with empty buffer")
	}

	// Case 2: Buffer populated on close, drained subsequently
	out2, err := OpenOutput(2, 44100, 1, true)
	if err != nil {
		t.Fatalf("OpenOutput: %v", err)
	}
	eo2, ok := out2.(*emulatorOutput)
	if !ok {
		t.Fatal("expected emulatorOutput")
	}
	if _, err := eo2.Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := eo2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if eo2.Drained() {
		t.Fatal("expected Drained() to be false when closed with buffered data")
	}
	drainedData := eo2.Drain()
	if len(drainedData) != 4 {
		t.Fatalf("expected 4 bytes drained, got %d", len(drainedData))
	}
	if !eo2.Drained() {
		t.Fatal("expected Drained() to be true after Drain() emptied the closed buffer")
	}
}
