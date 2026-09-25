package audio

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestVirtualAudioDevicePushRead(t *testing.T) {
	dev := NewVirtualAudioDevice()
	defer dev.Close()

	payload := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	n, err := dev.InjectPush(payload)
	if err != nil {
		t.Fatalf("InjectPush error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("expected push length %d, got %d", len(payload), n)
	}
	if dev.Buffered() != len(payload) {
		t.Fatalf("expected buffered %d, got %d", len(payload), dev.Buffered())
	}

	buf := make([]byte, 4)
	readN, err := dev.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if readN != 4 || !bytes.Equal(buf, payload[:4]) {
		t.Fatalf("unexpected read: n=%d data=%v", readN, buf)
	}

	buf2 := make([]byte, 8)
	readN2, err := dev.Read(buf2)
	if err != nil {
		t.Fatalf("Read 2 error: %v", err)
	}
	if readN2 != 4 || !bytes.Equal(buf2[:readN2], payload[4:]) {
		t.Fatalf("unexpected read 2: n=%d data=%v", readN2, buf2[:readN2])
	}
}

func TestVirtualAudioDeviceWriteCaptured(t *testing.T) {
	dev := NewVirtualAudioDevice()
	defer dev.Close()

	data1 := []byte{10, 20, 30}
	data2 := []byte{40, 50}

	n, err := dev.Write(data1)
	if err != nil || n != len(data1) {
		t.Fatalf("Write 1 failed: n=%d err=%v", n, err)
	}
	n, err = dev.Write(data2)
	if err != nil || n != len(data2) {
		t.Fatalf("Write 2 failed: n=%d err=%v", n, err)
	}

	captured := dev.CapturedBytes()
	expected := []byte{10, 20, 30, 40, 50}
	if !bytes.Equal(captured, expected) {
		t.Fatalf("CapturedBytes mismatch: got %v, want %v", captured, expected)
	}
	if dev.Buffered() != len(expected) {
		t.Fatalf("Buffered mismatch: got %d, want %d", dev.Buffered(), len(expected))
	}
}

func TestVirtualAudioDeviceReset(t *testing.T) {
	dev := NewVirtualAudioDevice()
	defer dev.Close()

	dev.InjectPush([]byte{1, 2, 3})
	dev.Write([]byte{4, 5, 6})

	dev.Reset()
	if len(dev.CapturedBytes()) != 0 {
		t.Fatalf("expected empty captured bytes after reset")
	}
	if dev.Buffered() != 0 {
		t.Fatalf("expected 0 buffered bytes after reset")
	}
}

func TestVirtualAudioDeviceCloseEOF(t *testing.T) {
	dev := NewVirtualAudioDevice()

	// Goroutine reading from empty device
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := dev.Read(buf)
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if err := dev.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF on close, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for Read to unblock on close")
	}

	// Read again should immediately return EOF
	buf := make([]byte, 16)
	n, err := dev.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("expected immediate EOF on closed device, got n=%d err=%v", n, err)
	}

	// Write and InjectPush on closed device should return ErrClosedPipe
	if _, err := dev.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe on Write to closed device, got: %v", err)
	}
	if _, err := dev.InjectPush([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe on InjectPush to closed device, got: %v", err)
	}
}

func TestVirtualAudioDeviceConcurrent(t *testing.T) {
	dev := NewVirtualAudioDevice()
	defer dev.Close()

	const iterations = 50
	const chunkSize = 64
	sample := bytes.Repeat([]byte{0x42}, chunkSize)

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := dev.InjectPush(sample); err != nil {
				t.Errorf("InjectPush error: %v", err)
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Consumer
	go func() {
		defer wg.Done()
		total := 0
		buf := make([]byte, chunkSize)
		for total < iterations*chunkSize {
			n, err := dev.Read(buf)
			if err != nil {
				t.Errorf("Read error: %v", err)
				return
			}
			total += n
		}
	}()

	wg.Wait()
}

func TestVirtualAudioDevicePacing(t *testing.T) {
	dev := NewVirtualAudioDevice()
	defer dev.Close()

	// 1000 Hz, 1 channel, 16-bit = 2000 bytes/sec
	dev.SetPacing(true, 1000, 1)

	// 200 bytes = 100ms
	data := make([]byte, 200)
	dev.InjectPush(data)

	start := time.Now()
	buf := make([]byte, 200)
	n, err := dev.Read(buf)
	elapsed := time.Since(start)

	if err != nil || n != 200 {
		t.Fatalf("Read failed: n=%d err=%v", n, err)
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("expected pacing delay >= 80ms, got %v", elapsed)
	}
}
