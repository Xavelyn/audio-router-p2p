package audio

import (
	"io"
	"testing"
	"time"
)

func TestGhostAudioStream_ReadSilence(t *testing.T) {
	gst := NewGhostAudioStream(48000, 2)
	gst.SetPaced(false) // fast test
	defer gst.Close()

	buf := make([]byte, 1024)
	for i := range buf {
		buf[i] = 0xAA // dirty buffer
	}

	n, err := gst.Read(buf)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("expected read %d bytes, got %d", len(buf), n)
	}

	for i, b := range buf {
		if b != 0 {
			t.Fatalf("byte at index %d is not zero: %d", i, b)
		}
	}
}

func TestGhostAudioStream_WriteDiscard(t *testing.T) {
	gst := NewGhostAudioStream(48000, 2)
	defer gst.Close()

	buf := make([]byte, 512)
	n, err := gst.Write(buf)
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("expected write %d bytes, got %d", len(buf), n)
	}

	if gst.Buffered() != 0 {
		t.Errorf("buffered bytes should be 0, got %d", gst.Buffered())
	}
}

func TestGhostAudioStream_CloseUnblocks(t *testing.T) {
	gst := NewGhostAudioStream(48000, 2)
	gst.SetPaced(true)

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 48000*4) // 1 full second
		_, err := gst.Read(buf)
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	_ = gst.Close()

	select {
	case err := <-errCh:
		if err != io.EOF {
			t.Errorf("expected io.EOF on closed ghost stream, got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ghost stream Read blocked after Close")
	}
}

func TestGhostAudioStream_ConcurrentSetPaced(t *testing.T) {
	gst := NewGhostAudioStream(48000, 2)
	defer gst.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			gst.SetPaced(i%2 == 0)
			time.Sleep(1 * time.Millisecond)
		}
	}()

	buf := make([]byte, 128)
	for {
		select {
		case <-done:
			return
		default:
			_, _ = gst.Read(buf)
		}
	}
}
