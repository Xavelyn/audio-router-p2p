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

func portOf(tr *UDPTransport) string {
	_, p, err := net.SplitHostPort(tr.Addr())
	if err != nil {
		return "0"
	}
	return p
}
