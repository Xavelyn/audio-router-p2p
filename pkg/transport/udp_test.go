package transport

import (
	"net"
	"testing"
	"time"
)

func TestUDPTransportSendReceive(t *testing.T) {
	// Server: listen on a random UDP port
	srvAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve server addr: %v", err)
	}
	srvConn, err := net.ListenUDP("udp", srvAddr)
	if err != nil {
		t.Fatalf("listen server udp: %v", err)
	}
	defer srvConn.Close()

	// Client transport: bind to any free port ("0")
	clientTransport, err := NewUDPTransport(":0")
	if err != nil {
		t.Fatalf("create client transport: %v", err)
	}
	defer clientTransport.Close()

	// Send payload to the server address
	payload := []byte("test-payload")
	if err := clientTransport.Send(srvConn.LocalAddr().String(), payload); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Verify server receives the payload
	buf := make([]byte, 1024)
	srvConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := srvConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("server read timeout: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("payload mismatch: got %s want %s", string(buf[:n]), string(payload))
	}
}

func TestUDPTransportReceiveStopsOnClose(t *testing.T) {
	tr, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}

	received := make(chan struct{}, 8)
	tr.Receive(func(data []byte, _ *net.UDPAddr) {
		received <- struct{}{}
	})

	// Send a packet so the receive loop is active.
	if err := tr.Send("127.0.0.1:"+portOf(tr), []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("receive loop did not deliver self-sent packet")
	}

	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitCh := make(chan struct{})
	go func() {
		tr.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("receive loop did not exit after Close")
	}
}

// TestUDPTransportMultiHandler verifies the shared-socket semantics used by
// multi-route operation: multiple handlers receive every datagram, and
// unregistering one handler stops exactly that handler without affecting the
// others or the socket.
func TestUDPTransportMultiHandler(t *testing.T) {
	tr, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}
	defer tr.Close()

	h1 := make(chan []byte, 4)
	h2 := make(chan []byte, 4)
	unreg1 := tr.Receive(func(data []byte, _ *net.UDPAddr) {
		select {
		case h1 <- append([]byte(nil), data...):
		default:
		}
	})
	tr.Receive(func(data []byte, _ *net.UDPAddr) {
		select {
		case h2 <- append([]byte(nil), data...):
		default:
		}
	})

	// Self-send a datagram: both registered handlers must see it.
	if err := tr.Send("127.0.0.1:"+portOf(tr), []byte("one")); err != nil {
		t.Fatalf("send: %v", err)
	}
	for name, ch := range map[string]chan []byte{"h1": h1, "h2": h2} {
		select {
		case got := <-ch:
			if string(got) != "one" {
				t.Fatalf("%s received %q, want %q", name, got, "one")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not receive datagram", name)
		}
	}

	// Unregister h1, then send again: only h2 receives.
	unreg1()
	if err := tr.Send("127.0.0.1:"+portOf(tr), []byte("two")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-h2:
		if string(got) != "two" {
			t.Fatalf("h2 received %q, want %q", got, "two")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("h2 did not receive datagram after h1 unregistered")
	}
	select {
	case got := <-h1:
		t.Fatalf("h1 still received after unregister: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	// Unregistering twice must be safe (idempotent).
	unreg1()
}

func portOf(tr *UDPTransport) string {
	_, p, err := net.SplitHostPort(tr.Addr())
	if err != nil {
		return "0"
	}
	return p
}
