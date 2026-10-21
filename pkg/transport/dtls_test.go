package transport

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func freeTransportUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("allocate UDP port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// TestDTLSTransportWrongPSKHandshakeFails verifies that the transport passes
// its handshake deadline to the crypto layer. A mismatched PSK must therefore
// produce a terminal connection error quickly on both peers, rather than
// leaving Pion's 30-second default handshake in the background.
func TestDTLSTransportWrongPSKHandshakeFails(t *testing.T) {
	const handshakeTimeout = 250 * time.Millisecond
	serverPort := freeTransportUDPPort(t)
	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort))

	server, err := NewDTLSTransport(DTLSConfig{
		ListenAddr:       serverAddr,
		PSK:              []byte("correct-horse-battery-staple"),
		IsServer:         true,
		HandshakeTimeout: handshakeTimeout,
	})
	if err != nil {
		t.Fatalf("create DTLS server: %v", err)
	}
	defer server.Close()

	client, err := NewDTLSTransport(DTLSConfig{
		ListenAddr:       "127.0.0.1:0",
		RemoteAddr:       serverAddr,
		PSK:              []byte("wrong-psk-entirely"),
		HandshakeTimeout: handshakeTimeout,
	})
	if err != nil {
		t.Fatalf("create DTLS client: %v", err)
	}
	defer client.Close()

	waitForError := func(name string, tr *DTLSTransport) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			tr.mu.Lock()
			connErr := tr.connErr
			tr.mu.Unlock()
			if connErr != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s did not terminate a wrong-PSK handshake within the configured timeout", name)
	}
	waitForError("server", server)
	waitForError("client", client)
}

// waitForConnected polls until the transport has an established DTLS
// connection (RemoteAddr is non-nil), or fails the test on the deadline.
func waitForConnected(t *testing.T, tr *DTLSTransport, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tr.RemoteAddr() != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not establish a DTLS connection within 5s", what)
}

// waitForLost polls until the transport's current connection has dropped.
func waitForLost(t *testing.T, tr *DTLSTransport, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tr.RemoteAddr() == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s connection did not drop within 5s", what)
}

func newReconnectServer(t *testing.T, port int, psk []byte) (*DTLSTransport, chan []byte) {
	t.Helper()
	server, err := NewDTLSTransport(DTLSConfig{
		ListenAddr:       net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		PSK:              psk,
		IsServer:         true,
		HandshakeTimeout: 3 * time.Second,
		KeepAlive:        500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create DTLS server: %v", err)
	}
	data := make(chan []byte, 16)
	server.Receive(func(b []byte, _ *net.UDPAddr) { data <- b })
	return server, data
}

func newReconnectClient(t *testing.T, remote string, psk []byte) *DTLSTransport {
	t.Helper()
	client, err := NewDTLSTransport(DTLSConfig{
		ListenAddr:       "127.0.0.1:0",
		RemoteAddr:       remote,
		PSK:              psk,
		HandshakeTimeout: 3 * time.Second,
		KeepAlive:        500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create DTLS client: %v", err)
	}
	return client
}

// TestDTLSClientReconnectsAfterPeerRestart verifies that when the remote
// endpoint closes, the client detects it and dials again, reaching a new
// server that takes over the same port, and that audio data flows end to end
// after recovery.
func TestDTLSClientReconnectsAfterPeerRestart(t *testing.T) {
	psk := []byte("reconnect-test-psk")
	port := freeTransportUDPPort(t)
	remote := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	server1, server1Data := newReconnectServer(t, port, psk)
	defer server1.Close()

	client := newReconnectClient(t, remote, psk)
	defer client.Close()
	clientReceive := make(chan []byte, 16)
	client.Receive(func(b []byte, _ *net.UDPAddr) { clientReceive <- b })

	waitForConnected(t, client, "client")
	if err := client.Send(remote, []byte("hello-first")); err != nil {
		t.Fatalf("send before restart: %v", err)
	}
	select {
	case b := <-server1Data:
		if string(b) != "hello-first" {
			t.Fatalf("server1 got %q, want hello-first", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server1 did not receive first payload")
	}

	// Restart the peer: close the old server (sends close_notify), then bring
	// up a new server on the same port. The client must reconnect on its own.
	server1.Close()
	waitForLost(t, client, "client after server close")
	server2, server2Data := newReconnectServer(t, port, psk)
	defer server2.Close()

	waitForConnected(t, client, "client after peer restart")
	if err := client.Send(remote, []byte("hello-second")); err != nil {
		t.Fatalf("send after restart: %v", err)
	}
	select {
	case b := <-server2Data:
		if string(b) != "hello-second" {
			t.Fatalf("server2 got %q, want hello-second", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server2 did not receive payload after reconnect")
	}

	// Keepalives are consumed by the transport and must not reach handlers.
	select {
	case b := <-clientReceive:
		t.Fatalf("client handler received unexpected datagram: %q", b)
	default:
	}
}

// TestDTLSServerReacceptsAfterClientRestart verifies the server role: after
// the client goes away, the same server transport accepts the next client's
// handshake on its existing socket without being recreated.
func TestDTLSServerReacceptsAfterClientRestart(t *testing.T) {
	psk := []byte("reconnect-test-psk")
	port := freeTransportUDPPort(t)
	remote := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	server, serverData := newReconnectServer(t, port, psk)
	defer server.Close()

	client1 := newReconnectClient(t, remote, psk)
	waitForConnected(t, client1, "client1")
	if err := client1.Send(remote, []byte("from-client1")); err != nil {
		t.Fatalf("send from client1: %v", err)
	}
	select {
	case b := <-serverData:
		if string(b) != "from-client1" {
			t.Fatalf("server got %q, want from-client1", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive client1 payload")
	}

	client1.Close()
	waitForLost(t, server, "server after client close")

	client2 := newReconnectClient(t, remote, psk)
	defer client2.Close()
	waitForConnected(t, client2, "client2")
	waitForConnected(t, server, "server after client2 handshake")
	if err := client2.Send(remote, []byte("from-client2")); err != nil {
		t.Fatalf("send from client2: %v", err)
	}
	select {
	case b := <-serverData:
		if string(b) != "from-client2" {
			t.Fatalf("server got %q, want from-client2", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive client2 payload after re-accept")
	}
}
