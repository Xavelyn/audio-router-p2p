package crypto

import (
	"net"
	"os"
	"testing"

	"github.com/pion/dtls/v2"
)

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
