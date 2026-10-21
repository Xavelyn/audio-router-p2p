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
