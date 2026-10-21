package crypto

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pion/dtls/v2"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v2"
)

// DTLSConfig holds configuration for DTLS connection
type DTLSConfig struct {
	LocalAddr        *net.UDPAddr
	RemoteAddr       *net.UDPAddr
	CertFile         string
	KeyFile          string
	PSK              []byte        // Pre-shared key for PSK cipher suites (optional)
	IsServer         bool          // true for server (responder), false for client (initiator)
	HandshakeTimeout time.Duration // maximum time for the DTLS handshake; zero uses Pion's default
}

// DTLSConn wraps a DTLS connection with SRTP support
type DTLSConn struct {
	conn       *dtls.Conn
	srtpSend   *srtp.Context
	srtpRecv   *srtp.Context
	localAddr  *net.UDPAddr
	remoteAddr *net.UDPAddr
}

// dtlsRole represents the DTLS role
type dtlsRole int

const (
	roleClient dtlsRole = iota
	roleServer
)

// GenerateSelfSignedCert creates a self-signed certificate for DTLS
func GenerateSelfSignedCert(certFile, keyFile string, ips []net.IP) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serialNumber := big.NewInt(time.Now().UnixNano())
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "audio-router-p2p",
			Organization: []string{"audio-router-p2p"},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().AddDate(10, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: ips,
		DNSNames:    []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := writeFile(certFile, certPEM, 0644); err != nil {
		return err
	}
	if err := writeFile(keyFile, keyPEM, 0600); err != nil {
		return err
	}
	return nil
}

func writeFile(path string, data []byte, perm uint32) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, os.FileMode(perm))
}

func deriveSRTPKeys(state dtls.State, role dtlsRole) (sendKey, recvKey []byte, err error) {
	const (
		keyLen  = 16 // AES-128 SRTP master key
		saltLen = 14 // SRTP master salt
	)

	// RFC 5764 order: client_write_key | server_write_key | client_write_salt | server_write_salt
	keyMaterial, err := state.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 2*(keyLen+saltLen))
	if err != nil {
		return nil, nil, fmt.Errorf("export SRTP keying material: %w", err)
	}

	if len(keyMaterial) < 2*(keyLen+saltLen) {
		return nil, nil, fmt.Errorf("insufficient keying material")
	}

	clientKey := keyMaterial[:keyLen]
	serverKey := keyMaterial[keyLen : 2*keyLen]
	clientSalt := keyMaterial[2*keyLen : 2*keyLen+saltLen]
	serverSalt := keyMaterial[2*keyLen+saltLen : 2*(keyLen+saltLen)]

	// Send context uses our own write keys; receive context uses the peer's.
	if role == roleClient {
		sendKey = append(append([]byte{}, clientKey...), clientSalt...)
		recvKey = append(append([]byte{}, serverKey...), serverSalt...)
	} else {
		sendKey = append(append([]byte{}, serverKey...), serverSalt...)
		recvKey = append(append([]byte{}, clientKey...), clientSalt...)
	}
	return sendKey, recvKey, nil
}

// NewDTLSConn creates a new DTLS connection with SRTP support
func NewDTLSConn(cfg *DTLSConfig) (*DTLSConn, error) {
	if cfg == nil {
		return nil, fmt.Errorf("dtls: nil config")
	}
	role := roleClient
	if cfg.IsServer {
		role = roleServer
	}
	return NewDTLSConnWithRole(cfg, role)
}

func NewDTLSConnWithRole(cfg *DTLSConfig, role dtlsRole) (*DTLSConn, error) {
	if cfg == nil {
		return nil, fmt.Errorf("dtls: nil config")
	}
	if cfg.LocalAddr == nil {
		return nil, fmt.Errorf("dtls: LocalAddr is required")
	}
	if role == roleClient && cfg.RemoteAddr == nil {
		return nil, fmt.Errorf("dtls: client role requires a RemoteAddr")
	}

	var conn *dtls.Conn
	var err error

	if role == roleServer {
		listener, err := NewDTLSListener(cfg)
		if err != nil {
			return nil, err
		}
		defer listener.Close()
		return listener.Accept()
	}

	dtlsConfig, err := buildDTLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	conn, err = dtls.Dial("udp", cfg.RemoteAddr, dtlsConfig)
	if err != nil {
		return nil, fmt.Errorf("dtls dial: %w", err)
	}

	d := &DTLSConn{
		conn:       conn,
		localAddr:  cfg.LocalAddr,
		remoteAddr: cfg.RemoteAddr,
	}

	// Setup SRTP contexts
	if err := d.setupSRTPWithRole(role); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setup SRTP: %w", err)
	}

	return d, nil
}

// DTLSListener is a DTLS server that binds its UDP socket immediately and
// accepts peers asynchronously.
type DTLSListener struct {
	listener net.Listener
	cfg      *DTLSConfig
}

// NewDTLSListener binds the UDP socket for cfg.LocalAddr synchronously so the
// port is guaranteed available as soon as it returns. Use Accept to wait for a
// peer, or Close to release the socket.
func NewDTLSListener(cfg *DTLSConfig) (*DTLSListener, error) {
	dtlsConfig, err := buildDTLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	listener, err := dtls.Listen("udp", cfg.LocalAddr, dtlsConfig)
	if err != nil {
		return nil, fmt.Errorf("dtls listen: %w", err)
	}

	return &DTLSListener{listener: listener, cfg: cfg}, nil
}

// Accept waits for the next inbound connection and completes the DTLS
// handshake plus SRTP setup.
func (l *DTLSListener) Accept() (*DTLSConn, error) {
	dtlsConn, err := l.listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("dtls accept: %w", err)
	}
	conn := dtlsConn.(*dtls.Conn)

	d := &DTLSConn{
		conn:       conn,
		localAddr:  l.cfg.LocalAddr,
		remoteAddr: l.cfg.RemoteAddr,
	}

	// Setup SRTP contexts (server write keys are used for sends)
	if err := d.setupSRTPWithRole(roleServer); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setup SRTP: %w", err)
	}

	return d, nil
}

// Close releases the listener's UDP socket; accepted connections are unaffected.
func (l *DTLSListener) Close() error {
	return l.listener.Close()
}

// Addr is the bound address, available before a peer connects.
func (l *DTLSListener) Addr() net.Addr { return l.listener.Addr() }

func buildDTLSConfig(cfg *DTLSConfig) (*dtls.Config, error) {
	var cert *tls.Certificate
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		c, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load cert: %w", err)
		}
		cert = &c
	}

	dtlsConfig := &dtls.Config{
		// Certificate-chain and hostname verification are disabled because
		// nodes use self-signed certificates. This is not peer authentication:
		// the discovery AuthToken is currently LAN-level only. PSK mode, when
		// configured, authenticates the DTLS peer through the shared key.
		InsecureSkipVerify:   true,
		Certificates:         []tls.Certificate{},
		ExtendedMasterSecret: dtls.RequestExtendedMasterSecret,
		MTU:                  1500,
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
			dtls.SRTP_AEAD_AES_128_GCM,
		},
	}

	if cert != nil {
		dtlsConfig.Certificates = []tls.Certificate{*cert}
	}

	if cfg.HandshakeTimeout > 0 {
		dtlsConfig.ConnectContextMaker = func() (context.Context, func()) {
			return context.WithTimeout(context.Background(), cfg.HandshakeTimeout)
		}
	}

	if len(cfg.PSK) > 0 {
		dtlsConfig.PSK = func(hint []byte) ([]byte, error) {
			return cfg.PSK, nil
		}
		dtlsConfig.PSKIdentityHint = []byte("audio-router-p2p")
		dtlsConfig.CipherSuites = []dtls.CipherSuiteID{
			dtls.TLS_PSK_WITH_AES_128_CCM_8,
			dtls.TLS_PSK_WITH_AES_256_CCM_8,
		}
	} else {
		dtlsConfig.CipherSuites = []dtls.CipherSuiteID{
			dtls.TLS_ECDHE_ECDSA_WITH_AES_128_CCM,
		}
	}

	return dtlsConfig, nil
}

func (d *DTLSConn) setupSRTPWithRole(role dtlsRole) error {
	profile, ok := d.conn.SelectedSRTPProtectionProfile()
	if !ok {
		return fmt.Errorf("no SRTP profile negotiated")
	}

	state := d.conn.ConnectionState()
	sendKey, recvKey, err := deriveSRTPKeys(state, role)
	if err != nil {
		return err
	}

	srtpProfile := srtp.ProtectionProfile(profile)
	const keyLen, saltLen = 16, 14

	// Create SRTP contexts for send and receive. sendKey/recvKey are
	// key(16) preceded by salt(14) as produced by deriveSRTPKeys.
	sendCtx, err := srtp.CreateContext(sendKey[:keyLen], sendKey[keyLen:keyLen+saltLen], srtpProfile)
	if err != nil {
		return fmt.Errorf("create send SRTP context: %w", err)
	}

	recvCtx, err := srtp.CreateContext(recvKey[:keyLen], recvKey[keyLen:keyLen+saltLen], srtpProfile)
	if err != nil {
		return fmt.Errorf("create recv SRTP context: %w", err)
	}

	d.srtpSend = sendCtx
	d.srtpRecv = recvCtx

	return nil
}

// EncryptRTP encrypts an RTP packet using SRTP
func (d *DTLSConn) EncryptRTP(dst []byte, plaintext []byte, header *rtp.Header) ([]byte, error) {
	if d.srtpSend == nil {
		return nil, fmt.Errorf("SRTP send context not initialized")
	}
	return d.srtpSend.EncryptRTP(dst, plaintext, header)
}

// DecryptRTP decrypts an SRTP packet
func (d *DTLSConn) DecryptRTP(dst []byte, encrypted []byte, header *rtp.Header) ([]byte, error) {
	if d.srtpRecv == nil {
		return nil, fmt.Errorf("SRTP recv context not initialized")
	}
	return d.srtpRecv.DecryptRTP(dst, encrypted, header)
}

// EncryptRTCP encrypts an RTCP packet using SRTP
func (d *DTLSConn) EncryptRTCP(dst []byte, plaintext []byte, header *rtcp.Header) ([]byte, error) {
	if d.srtpSend == nil {
		return nil, fmt.Errorf("SRTP send context not initialized")
	}
	return d.srtpSend.EncryptRTCP(dst, plaintext, header)
}

// DecryptRTCP decrypts an SRTCP packet
func (d *DTLSConn) DecryptRTCP(dst []byte, encrypted []byte, header *rtcp.Header) ([]byte, error) {
	if d.srtpRecv == nil {
		return nil, fmt.Errorf("SRTP recv context not initialized")
	}
	return d.srtpRecv.DecryptRTCP(dst, encrypted, header)
}

// Write writes data to the DTLS connection
func (d *DTLSConn) Write(p []byte) (int, error) {
	return d.conn.Write(p)
}

// Read reads data from the DTLS connection
func (d *DTLSConn) Read(p []byte) (int, error) {
	return d.conn.Read(p)
}

// LocalAddr returns the local network address
func (d *DTLSConn) LocalAddr() net.Addr {
	return d.conn.LocalAddr()
}

// RemoteAddr returns the remote network address
func (d *DTLSConn) RemoteAddr() net.Addr {
	return d.conn.RemoteAddr()
}

// Close closes the DTLS connection
func (d *DTLSConn) Close() error {
	var err error
	if d.srtpSend != nil {
		// srtp.Context doesn't have Close, just nil it
		d.srtpSend = nil
	}
	if d.srtpRecv != nil {
		d.srtpRecv = nil
	}
	if e := d.conn.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// SetReadDeadline sets the read deadline
func (d *DTLSConn) SetReadDeadline(t time.Time) error {
	return d.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline
func (d *DTLSConn) SetWriteDeadline(t time.Time) error {
	return d.conn.SetWriteDeadline(t)
}

// Handshake is retained for API compatibility. Connections returned by this
// package complete their DTLS handshake before the constructor returns.
func (d *DTLSConn) Handshake(ctx context.Context) error {
	return nil
}

// ConnectionState returns the DTLS connection state
func (d *DTLSConn) ConnectionState() dtls.State {
	return d.conn.ConnectionState()
}

// SelectedSRTPProfile returns the negotiated SRTP profile
func (d *DTLSConn) SelectedSRTPProfile() (dtls.SRTPProtectionProfile, bool) {
	return d.conn.SelectedSRTPProtectionProfile()
}
