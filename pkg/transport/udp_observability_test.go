package transport

import (
	"bytes"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUDPObservabilityCountersAndLogging(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	// Create receiver transport
	recvTransport, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create receiver: %v", err)
	}
	defer recvTransport.Close()

	// Verify bind log
	if !strings.Contains(logBuf.String(), "[UDP:BIND] listening on") {
		t.Errorf("expected [UDP:BIND] log, got: %s", logBuf.String())
	}

	// Create sender transport
	sendTransport, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create sender: %v", err)
	}
	defer sendTransport.Close()

	// Send datagram to receiver when receiver has 0 handlers registered
	payload := []byte("unhandled-datagram-payload")
	if err := sendTransport.Send(recvTransport.Addr(), payload); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if sendTransport.PacketsSent() != 1 {
		t.Errorf("expected sendTransport.PacketsSent == 1, got %d", sendTransport.PacketsSent())
	}
	if sendTransport.BytesSent() != uint64(len(payload)) {
		t.Errorf("expected sendTransport.BytesSent == %d, got %d", len(payload), sendTransport.BytesSent())
	}

	// Register a handler to start the receive loop on recvTransport
	handled := make(chan []byte, 10)
	unregister := recvTransport.Receive(func(data []byte, addr *net.UDPAddr) {
		handled <- data
	})

	// Unregister immediately to trigger 0 handler drops
	unregister()

	// Send datagram to trigger zero-handler drop
	if err := sendTransport.Send(recvTransport.Addr(), payload); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	// Wait briefly for receive loop to process
	deadline := time.Now().Add(2 * time.Second)
	for recvTransport.ZeroHandlerDrop() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for zeroHandlerDrop to increment")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if recvTransport.PacketsRecv() == 0 {
		t.Errorf("expected recvTransport.PacketsRecv > 0, got %d", recvTransport.PacketsRecv())
	}
	if recvTransport.BytesRecv() == 0 {
		t.Errorf("expected recvTransport.BytesRecv > 0, got %d", recvTransport.BytesRecv())
	}

	// Re-register handler and send a valid datagram
	recvTransport.Receive(func(data []byte, addr *net.UDPAddr) {
		handled <- data
	})

	testData := []byte("valid-message")
	if err := sendTransport.Send(recvTransport.Addr(), testData); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	select {
	case got := <-handled:
		if string(got) != string(testData) {
			t.Errorf("expected %q, got %q", string(testData), string(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for valid datagram")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[UDP:SEND]") {
		t.Errorf("expected [UDP:SEND] log, got: %s", logs)
	}
	if !strings.Contains(logs, "[UDP:RECV]") {
		t.Errorf("expected [UDP:RECV] log, got: %s", logs)
	}
	if !strings.Contains(logs, "[UDP:RECV] WARN: datagram dropped - 0 handlers registered") {
		t.Errorf("expected zero-handler drop warning log, got: %s", logs)
	}
}

func TestUDPTransportSendErrors(t *testing.T) {
	tr, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create transport: %v", err)
	}
	defer tr.Close()

	// Invalid target address
	err = tr.Send("invalid:::address", []byte("fail"))
	if err == nil {
		t.Fatal("expected error sending to invalid address")
	}
	if tr.SendErrors() != 1 {
		t.Errorf("expected sendErrors == 1, got %d", tr.SendErrors())
	}
}

func TestAtomicShouldLogRateLimited(t *testing.T) {
	var last atomic.Int64
	interval := int64(100 * time.Millisecond)

	// First call should return true immediately
	if !ShouldLogRateLimited(&last, interval) {
		t.Fatal("expected first call to succeed immediately")
	}

	// Subsequent calls within interval should return false
	for i := 0; i < 5; i++ {
		if ShouldLogRateLimited(&last, interval) {
			t.Fatalf("call %d within interval returned true, expected false", i)
		}
	}

	// Concurrency test: multiple goroutines racing
	var wg sync.WaitGroup
	var successes atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ShouldLogRateLimited(&last, interval) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 0 {
		t.Fatalf("expected 0 concurrent successes during suppressed window, got %d", successes.Load())
	}

	// After sleeping past the interval, exactly one CAS should succeed
	time.Sleep(110 * time.Millisecond)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ShouldLogRateLimited(&last, interval) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("expected exactly 1 success after interval elapsed, got %d", successes.Load())
	}
}
