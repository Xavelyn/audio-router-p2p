package transport

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/crypto"
	"github.com/pion/dtls/v2"
	"github.com/pion/rtp"
)

// DTLSConfig holds configuration for DTLS transport
type DTLSConfig struct {
	ListenAddr       string        // Local address to bind (e.g., ":45678")
	RemoteAddr       string        // Remote address to connect to (for client)
	CertFile         string        // Path to certificate file
	KeyFile          string        // Path to key file
	PSK              []byte        // Pre-shared key (optional)
	IsServer         bool          // true for server (responder), false for client (initiator)
	HandshakeTimeout time.Duration // maximum time for the DTLS handshake and connection wait
}

// DTLSTransport provides DTLS-encrypted UDP transport with SRTP support
type DTLSTransport struct {
	mu       sync.Mutex
	conn     *crypto.DTLSConn
	connErr  error
	connDone chan struct{} // closed when conn is established or failed
	closed   atomic.Bool
	recvDone chan struct{}
	recvOnce sync.Once
	config   DTLSConfig

	handlers map[uint64]func([]byte, *net.UDPAddr)
	nextID   uint64
}

// NewDTLSTransport creates a new DTLS transport.
// For the server role, the listener accept runs in the background so the
// constructor returns immediately; callers obtain a usable connection once
// the peer has completed the DTLS handshake (see waitForConn).
func NewDTLSTransport(cfg DTLSConfig) (*DTLSTransport, error) {
	// Fail fast on misconfiguration at construction time instead of starting a
	// doomed async dial: a client (initiator) role requires a concrete remote.
	if !cfg.IsServer && cfg.RemoteAddr == "" {
		return nil, fmt.Errorf("dtls: client role requires a non-empty RemoteAddr " +
			"(set the single DTLS peer's stream address in routes.json)")
	}
	if cfg.HandshakeTimeout <= 0 {
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
		LocalAddr:        localAddr,
		RemoteAddr:       remoteAddr,
		CertFile:         cfg.CertFile,
		KeyFile:          cfg.KeyFile,
		PSK:              cfg.PSK,
		IsServer:         cfg.IsServer,
		HandshakeTimeout: cfg.HandshakeTimeout,
	}

	t := &DTLSTransport{
		recvDone: make(chan struct{}),
		config:   cfg,
		connDone: make(chan struct{}),
	}

	if cfg.IsServer {
		// Bind the UDP socket synchronously so the port is guaranteed
		// available before this constructor returns (the peer dials it).
		// Only the Accept is deferred to the background.
		listener, err := crypto.NewDTLSListener(cryptoCfg)
		if err != nil {
			return nil, err
		}
		go func() {
			dtlsConn, err := listener.Accept()
			if err != nil {
				listener.Close()
				t.setConnErr(err)
				close(t.connDone)
				return
			}
			t.mu.Lock()
			t.conn = dtlsConn
			t.mu.Unlock()
			close(t.connDone)
		}()
		return t, nil
	}

	// Client role: dial asynchronously; the server started above has already
	// bound its port, so the dial will always reach it.
	go func() {
		dtlsConn, err := crypto.NewDTLSConn(cryptoCfg)
		if err != nil {
			log.Printf("DTLS transport %q (false): setup error: %v", cfg.ListenAddr, err)
			t.setConnErr(err)
			close(t.connDone)
			return
		}
		t.mu.Lock()
		t.conn = dtlsConn
		t.mu.Unlock()
		close(t.connDone)
	}()

	return t, nil
}

// waitForConn blocks until the DTLS connection (accept or dial) is ready.
func (t *DTLSTransport) waitForConn() error {
	timeout := t.config.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-t.connDone:
	case <-time.After(timeout):
		return fmt.Errorf("dtls connection timeout after %s", timeout)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		if t.connErr != nil {
			return t.connErr
		}
		return net.ErrClosed
	}
	return nil
}

func (t *DTLSTransport) setConnErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connErr = err
}

// Addr returns the local address as a string
func (t *DTLSTransport) Addr() string {
	if err := t.waitForConn(); err != nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return ""
	}
	return t.conn.LocalAddr().String()
}

// LocalAddr returns the underlying local UDP address
func (t *DTLSTransport) LocalAddr() *net.UDPAddr {
	if err := t.waitForConn(); err != nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.LocalAddr().(*net.UDPAddr)
}

// RemoteAddr returns the remote address
func (t *DTLSTransport) RemoteAddr() *net.UDPAddr {
	if err := t.waitForConn(); err != nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.RemoteAddr().(*net.UDPAddr)
}

// Send writes encrypted data to the remote peer
func (t *DTLSTransport) Send(to string, data []byte) error {
	if err := t.waitForConn(); err != nil {
		return err
	}
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

// Receive registers a handler for incoming datagrams and returns a function
// that unregisters it. Any number of handlers may be registered; each
// received datagram is delivered to every registered handler. The receive
// loop starts on demand with the first registration.
func (t *DTLSTransport) Receive(handler func([]byte, *net.UDPAddr)) (unregister func()) {
	t.mu.Lock()
	if t.handlers == nil {
		t.handlers = make(map[uint64]func([]byte, *net.UDPAddr))
	}
	t.nextID++
	id := t.nextID
	t.handlers[id] = handler
	t.mu.Unlock()

	t.recvOnce.Do(func() {
		go t.receiveLoop()
	})

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.handlers, id)
			t.mu.Unlock()
		})
	}
}

func (t *DTLSTransport) receiveLoop() {
	if err := t.waitForConn(); err != nil {
		log.Printf("DTLS receive: connection not established: %v", err)
		close(t.recvDone)
		return
	}
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

		t.mu.Lock()
		handlers := make([]func([]byte, *net.UDPAddr), 0, len(t.handlers))
		for _, h := range t.handlers {
			handlers = append(handlers, h)
		}
		t.mu.Unlock()
		for _, h := range handlers {
			h(data, raddr)
		}
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
	if err := t.waitForConn(); err != nil {
		return nil, err
	}
	h, ok := header.(*rtp.Header)
	if !ok {
		return nil, fmt.Errorf("EncryptRTP: header must be *rtp.Header")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil, net.ErrClosed
	}
	return t.conn.EncryptRTP(dst, plaintext, h)
}

// DecryptRTP decrypts an SRTP packet
func (t *DTLSTransport) DecryptRTP(dst, encrypted []byte, header interface{}) ([]byte, error) {
	if err := t.waitForConn(); err != nil {
		return nil, err
	}
	h, ok := header.(*rtp.Header)
	if !ok {
		return nil, fmt.Errorf("DecryptRTP: header must be *rtp.Header")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil, net.ErrClosed
	}
	return t.conn.DecryptRTP(dst, encrypted, h)
}

// SelectedSRTPProfile returns the negotiated SRTP profile
func (t *DTLSTransport) SelectedSRTPProfile() (dtls.SRTPProtectionProfile, bool) {
	if err := t.waitForConn(); err != nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return 0, false
	}
	return t.conn.SelectedSRTPProfile()
}
