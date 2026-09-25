package crypto

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/dtls/v2"
)

// freeUDPPort allocates an ephemeral UDP port on loopback and releases it, so a
// test can bind a real socket to a known-free port.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// loopbackUDP builds a loopback UDP address for the given port.
func loopbackUDP(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
}

func TestDTLSConnCreation(t *testing.T) {
	certFile := "/tmp/test_cert.pem"
	keyFile := "/tmp/test_key.pem"

	err := GenerateSelfSignedCert(certFile, keyFile, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert failed: %v", err)
	}

	cfg := &DTLSConfig{
		LocalAddr:  &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000},
		CertFile:   certFile,
		KeyFile:    keyFile,
		IsServer:   false,
	}

	if cfg.CertFile != certFile {
		t.Error("cert file not set")
	}
	if cfg.KeyFile != keyFile {
		t.Error("key file not set")
	}
	if cfg.IsServer {
		t.Error("should be client")
	}
}

func TestDTLSRole(t *testing.T) {
	if roleClient != 0 {
		t.Error("roleClient should be 0")
	}
	if roleServer != 1 {
		t.Error("roleServer should be 1")
	}
}

func TestDeriveSRTPKeys(t *testing.T) {
	var state dtls.State
	_, _, err := deriveSRTPKeys(state, roleClient)
	if err == nil {
		t.Error("expected error with empty state")
	}
}

// TestNewDTLSConnFailFast verifies that NewDTLSConn rejects unusable configs
// at construction time (no doomed async dial). This is the fail-fast the
// findings report calls for in §2.3 encrypt mode, where main.go previously
// started a client-role dial with an empty RemoteAddr and silently delivered
// nothing.
func TestNewDTLSConnFailFast(t *testing.T) {
	// Fail-fast (2.3): unusable configs are rejected at construction time,
	// before any cert load or dial -- so no cert files or sockets are needed
	// for these paths.
	if _, err := NewDTLSConn(nil); err == nil {
		t.Error("expected error for nil config")
	}

	certOnly := &DTLSConfig{
		LocalAddr:  &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41001},
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41002},
		// Any unique literal path is fine: fail-fast triggers before cert load.
		CertFile: "/tmp/test_cert.pem",
		KeyFile:  "/tmp/test_key.pem",
		IsServer: false,
	}

	// Client role requires a LocalAddr before any dial.
	noLocal := *certOnly
	noLocal.LocalAddr = nil
	if _, err := NewDTLSConn(&noLocal); err == nil {
		t.Error("expected error for client with nil LocalAddr")
	}

	// Client role requires a RemoteAddr before any dial (no doomed dial).
	noRemote := *certOnly
	noRemote.RemoteAddr = nil
	if _, err := NewDTLSConn(&noRemote); err == nil {
		t.Error("expected error for client with nil RemoteAddr")
	}

	// Client role requires both LocalAddr and RemoteAddr; a config missing
	// RemoteAddr is a dead path that must fail fast (the transport cannot dial).
	if _, err := NewDTLSConn(&noRemote); err == nil {
		t.Error("expected error for client config without remote")
	}
}

func TestGenerateSelfSignedCert(t *testing.T) {
	certFile := "/tmp/test_cert2.pem"
	keyFile := "/tmp/test_key2.pem"

	err := GenerateSelfSignedCert(certFile, keyFile, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert failed: %v", err)
	}

	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		t.Error("cert file not created")
	}
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		t.Error("key file not created")
	}
}

// TestPSKHandshake verifies both sides of the PSK contract: a matching key
// completes a handshake and derives SRTP state, while a mismatched key fails
// within the configured handshake deadline (finding 4.2).
func TestPSKHandshake(t *testing.T) {
	t.Run("matching PSK", func(t *testing.T) {
		testPSKHandshake(t, []byte("correct-horse-battery-staple"), false)
	})
	t.Run("wrong PSK", func(t *testing.T) {
		testPSKHandshake(t, []byte("wrong-psk-entirely"), true)
	})
}

func testPSKHandshake(t *testing.T, clientPSK []byte, wantError bool) {
	t.Helper()
	const handshakeTimeout = time.Second

	serverCfg := &DTLSConfig{
		LocalAddr:        loopbackUDP(freeUDPPort(t)),
		PSK:              []byte("correct-horse-battery-staple"),
		IsServer:         true,
		HandshakeTimeout: handshakeTimeout,
	}
	listener, err := NewDTLSListener(serverCfg)
	if err != nil {
		t.Fatalf("NewDTLSListener: %v", err)
	}
	defer listener.Close()

	clientCfg := &DTLSConfig{
		LocalAddr:        loopbackUDP(0),
		RemoteAddr:       serverCfg.LocalAddr,
		PSK:              clientPSK,
		HandshakeTimeout: handshakeTimeout,
	}

	type result struct {
		conn *DTLSConn
		err  error
	}
	acceptCh := make(chan result, 1)
	clientCh := make(chan result, 1)
	go func() {
		conn, err := listener.Accept()
		acceptCh <- result{conn: conn, err: err}
	}()
	go func() {
		conn, err := NewDTLSConn(clientCfg)
		clientCh <- result{conn: conn, err: err}
	}()

	waitFor := func(name string, ch <-chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(4 * time.Second):
			t.Fatalf("%s did not finish within the bounded handshake deadline", name)
		}
		return result{}
	}
	clientResult := waitFor("DTLS client", clientCh)
	serverResult := waitFor("DTLS server", acceptCh)

	if clientResult.conn != nil {
		defer clientResult.conn.Close()
	}
	if serverResult.conn != nil {
		defer serverResult.conn.Close()
	}

	if wantError {
		if clientResult.err == nil {
			t.Error("wrong PSK unexpectedly completed the client handshake")
		}
		if serverResult.err == nil {
			t.Error("wrong PSK unexpectedly completed the server handshake")
		}
		return
	}

	if clientResult.err != nil {
		t.Fatalf("matching PSK client handshake: %v", clientResult.err)
	}
	if serverResult.err != nil {
		t.Fatalf("matching PSK server handshake: %v", serverResult.err)
	}
	if clientResult.conn == nil || serverResult.conn == nil {
		t.Fatal("matching PSK returned a nil connection")
	}
	if _, ok := clientResult.conn.SelectedSRTPProfile(); !ok {
		t.Error("matching PSK did not derive a client SRTP profile")
	}
	if _, ok := serverResult.conn.SelectedSRTPProfile(); !ok {
		t.Error("matching PSK did not derive a server SRTP profile")
	}
}
