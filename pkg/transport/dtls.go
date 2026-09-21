package transport

import (
	"context"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/crypto"
	"github.com/pion/dtls/v2"
)

// DTLSConfig holds configuration for DTLS transport
type DTLSConfig struct {
	ListenAddr string        // Local address to bind (e.g., ":45678")
	RemoteAddr string        // Remote address to connect to (for client)
	CertFile   string        // Path to certificate file
	KeyFile    string        // Path to key file
	PSK        []byte        // Pre-shared key (optional)
	IsServer   bool          // true for server (responder), false for client (initiator)
	HandshakeTimeout time.Duration // Timeout for DTLS handshake
}

// DTLSTransport provides DTLS-encrypted UDP transport with SRTP support
type DTLSTransport struct {
	mu       sync.Mutex
	conn     *crypto.DTLSConn
	closed   atomic.Bool
	recvDone chan struct{}
	recvOnce sync.Once
	config   DTLSConfig
}

// NewDTLSTransport creates a new DTLS transport
func NewDTLSTransport(cfg DTLSConfig) (*DTLSTransport, error) {
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}

	localAddr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}

	var remoteAddr *net.UDPAddr
	if cfg.RemoteAddr != "" {
		remoteAddr, err = net.ResolveUDPAddr("udp", cfg.RemoteAddr)
		if err != nil {
			return nil, err
		}
	}

	cryptoCfg := &crypto.DTLSConfig{
		LocalAddr:  localAddr,
		RemoteAddr: remoteAddr,
		CertFile:   cfg.CertFile,
		KeyFile:    cfg.KeyFile,
		PSK:        cfg.PSK,
		IsServer:   cfg.IsServer,
	}

	dtlsConn, err := crypto.NewDTLSConn(cryptoCfg)
	if err != nil {
		return nil, err
	}

	t := &DTLSTransport{
		conn:     dtlsConn,
		recvDone: make(chan struct{}),
		config:   cfg,
	}

	// Perform handshake
	ctx, cancel := context.WithTimeout(context.Background(), cfg.HandshakeTimeout)
	defer cancel()
	if err := dtlsConn.Handshake(ctx); err != nil {
		dtlsConn.Close()
		return nil, err
	}

	return t, nil
}

// Addr returns the local address as a string
func (t *DTLSTransport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return ""
	}
	return t.conn.LocalAddr().String()
}

// LocalAddr returns the underlying local UDP address
func (t *DTLSTransport) LocalAddr() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.LocalAddr().(*net.UDPAddr)
}

// RemoteAddr returns the remote address
func (t *DTLSTransport) RemoteAddr() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.RemoteAddr().(*net.UDPAddr)
}

// Send writes encrypted data to the remote peer
func (t *DTLSTransport) Send(to string, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return net.ErrClosed
	}

	// For DTLS client with fixed remote, the connection is already connected
	// For server, we need to handle multiple peers (not implemented yet)
	// Currently we only support single-peer DTLS connections
	_, err := t.conn.Write(data)
	return err
}

// Receive spawns a goroutine that delivers received datagrams to handler
func (t *DTLSTransport) Receive(handler func([]byte, *net.UDPAddr)) {
	t.recvOnce.Do(func() {
		go t.receiveLoop(handler)
	})
}

func (t *DTLSTransport) receiveLoop(handler func([]byte, *net.UDPAddr)) {
	buf := make([]byte, 65535)
	for {
		n, err := t.conn.Read(buf)
		if err != nil {
			if t.closed.Load() {
				close(t.recvDone)
				return
			}
			log.Printf("DTLS receive error: %v", err)
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		// For DTLS, the remote address is fixed
		var raddr *net.UDPAddr
		if t.config.RemoteAddr != "" {
			raddr, _ = net.ResolveUDPAddr("udp", t.config.RemoteAddr)
		} else {
			raddr = t.conn.RemoteAddr().(*net.UDPAddr)
		}

		handler(data, raddr)
	}
}

// Wait blocks until the Receive loop has exited
func (t *DTLSTransport) Wait() {
	<-t.recvDone
}

// Closed reports whether the transport has been closed
func (t *DTLSTransport) Closed() bool {
	return t.closed.Load()
}

// Close closes the DTLS connection
func (t *DTLSTransport) Close() error {
	t.closed.Store(true)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.Close()
}

// EncryptRTP encrypts an RTP packet using SRTP
func (t *DTLSTransport) EncryptRTP(dst, plaintext []byte, header interface{}) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil, net.ErrClosed
	}
	// Note: This requires the header to be *rtp.Header
	// The caller must type assert
	return nil, nil // Placeholder - actual encryption happens in streamer
}

// DecryptRTP decrypts an SRTP packet
func (t *DTLSTransport) DecryptRTP(dst, encrypted []byte, header interface{}) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil, net.ErrClosed
	}
	return nil, nil // Placeholder
}

// SelectedSRTPProfile returns the negotiated SRTP profile
func (t *DTLSTransport) SelectedSRTPProfile() (dtls.SRTPProtectionProfile, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return 0, false
	}
	return t.conn.SelectedSRTPProfile()
}