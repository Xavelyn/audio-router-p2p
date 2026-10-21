package audio

import (
	"io"
	"testing"
	"time"
)

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
