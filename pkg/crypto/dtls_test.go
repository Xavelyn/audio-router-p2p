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