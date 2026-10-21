package transport

import (
	"bytes"
	"errors"
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
	KeepAlive        time.Duration // keepalive write interval; peer is declared dead after five missed windows
}

// dtlsKeepAlive is a datagram both endpoints send periodically so that a
// silent (one-way audio) link is still bidirectionally live, and a crashed
// peer is detected instead of blocking the reader forever on UDP. The payload
// is never a valid audio packet (the wire header alone is larger).
var dtlsKeepAlive = []byte("ARP2P-KA")

const (
	defaultDTLSKeepAlive = 3 * time.Second
	minDTLSKeepAlive     = 500 * time.Millisecond
	keepAliveDeadWindows = 5
)

// errPeerUnreachable reports that the DTLS peer stopped responding.
var errPeerUnreachable = errors.New("dtls: peer unresponsive")

// DTLSTransport provides DTLS-encrypted UDP transport with SRTP support.
// When the connection drops, the transport dials (or accepts) again
// automatically: the client role retries with exponential backoff and the
// server role waits for the next inbound handshake on the same socket.
type DTLSTransport struct {
	mu        sync.Mutex
	conn      *crypto.DTLSConn
	connErr   error
	connDone  chan struct{} // closed when a connection becomes ready
	closed    atomic.Bool
	recvDone  chan struct{}
	recvOnce  sync.Once
	recvStart bool
	config    DTLSConfig
	listener  *crypto.DTLSListener
	localAddr *net.UDPAddr
	stop      chan struct{}
	// connectDone is closed when the accept/dial loop exits (transport closed).
	connectDone chan struct{}
	// lost is closed when the currently installed connection fails; nil while
	// no connection is installed.
	lost chan struct{}
	// liveness state shared with the keepalive loop.
	lastActivity atomic.Int64 // unix nanosecond of last received datagram
	livenessOnce sync.Once

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
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = defaultDTLSKeepAlive
	}
	if cfg.KeepAlive < minDTLSKeepAlive {
		cfg.KeepAlive = minDTLSKeepAlive
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
		recvDone:    make(chan struct{}),
		config:      cfg,
		connDone:    make(chan struct{}),
		localAddr:   localAddr,
		stop:        make(chan struct{}),
		connectDone: make(chan struct{}),
	}
	t.lastActivity.Store(time.Now().UnixNano())

	if cfg.IsServer {
		// Bind the UDP socket synchronously so the port is guaranteed
		// available before this constructor returns (the peer dials it).
		// Only the Accept is deferred to the background.
		listener, err := crypto.NewDTLSListener(cryptoCfg)
		if err != nil {
			return nil, err
		}
		t.listener = listener
		t.localAddr = listener.Addr().(*net.UDPAddr)
		go t.connectServer()
		return t, nil
	}

	// Client role: dial asynchronously; the server has already bound its port.
	go t.connectClient(cryptoCfg)

	return t, nil
}

// install registers a completed connection as the active one. The returned
// channel is closed when this connection fails.
func (t *DTLSTransport) install(conn *crypto.DTLSConn) (<-chan struct{}, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed.Load() {
		_ = conn.Close()
		return nil, false
	}
	t.conn = conn
	t.localAddr = conn.LocalAddr().(*net.UDPAddr)
	t.connErr = nil
	t.lost = make(chan struct{})
	t.lastActivity.Store(time.Now().UnixNano())
	close(t.connDone)
	return t.lost, true
}

// disconnect fails the current connection so the accept/dial loop reconnects.
// It must close the old connDone channel so loops parked on it wake up.
func (t *DTLSTransport) disconnect(conn *crypto.DTLSConn, err error) {
	t.mu.Lock()
	if t.conn == conn && !t.closed.Load() {
		// The current connDone channel was already closed by install; parkers
		// wake on that close. The replacement stays open until the next
		// install closes it, so loops re-capture a channel that will fire.
		t.conn = nil
		t.connErr = err
		t.connDone = make(chan struct{})
		close(t.lost)
		t.lost = nil
	}
	t.mu.Unlock()
	_ = conn.Close()
}

func (t *DTLSTransport) connectServer() {
	defer close(t.connectDone)
	for !t.closed.Load() {
		conn, err := t.listener.Accept()
		if err != nil {
			if t.closed.Load() {
				return
			}
			t.setConnErr(err)
			log.Printf("DTLS accept: %v", err)
			continue
		}
		lost, ok := t.install(conn)
		if !ok {
			return
		}
		// Even send-only transports need a reader to detect remote closes.
		t.startReader()
		select {
		case <-t.stop:
			return
		case <-lost:
		}
	}
}

func (t *DTLSTransport) connectClient(cfg *crypto.DTLSConfig) {
	defer close(t.connectDone)
	backoff := 250 * time.Millisecond
	for !t.closed.Load() {
		conn, err := crypto.NewDTLSConn(cfg)
		if err == nil {
			backoff = 250 * time.Millisecond
			lost, ok := t.install(conn)
			if !ok {
				return
			}
			t.startReader()
			select {
			case <-t.stop:
				return
			case <-lost:
			}
			continue
		}
		t.setConnErr(err)
		log.Printf("DTLS connect: %v", err)
		select {
		case <-t.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// livenessLoop keeps the link bidirectionally live and detects crashed peers.
// Both roles run it: a healthy one-way audio stream still receives the peer's
// keepalive, while a dead peer goes silent and is disconnected after several
// missed windows so the accept/dial loop can reconnect.
func (t *DTLSTransport) livenessLoop() {
	interval := t.config.KeepAlive
	deadAfter := keepAliveDeadWindows * interval
	for !t.closed.Load() {
		t.mu.Lock()
		ready, lost := t.connDone, t.lost
		t.mu.Unlock()
		if lost == nil {
			select {
			case <-t.stop:
				return
			case <-ready:
			}
			continue
		}
	waiting:
		for {
			select {
			case <-t.stop:
				return
			case <-lost:
				break waiting
			case <-time.After(interval):
			}
			t.mu.Lock()
			conn := t.conn
			t.mu.Unlock()
			if conn == nil {
				continue
			}
			if _, err := conn.Write(dtlsKeepAlive); err != nil && !t.closed.Load() {
				t.disconnect(conn, err)
				continue waiting
			}
			if time.Since(time.Unix(0, t.lastActivity.Load())) > deadAfter {
				t.disconnect(conn, errPeerUnreachable)
				continue waiting
			}
		}
	}
}

// waitForConn blocks until the DTLS connection (accept or dial) is ready.
func (t *DTLSTransport) waitForConn() error {
	timeout := t.config.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	t.mu.Lock()
	ready := t.connDone
	t.mu.Unlock()
	select {
	case <-t.stop:
		return net.ErrClosed
	case <-ready:
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
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.localAddr.String()
}

// LocalAddr returns the underlying local UDP address
func (t *DTLSTransport) LocalAddr() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.localAddr
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

// Send writes encrypted data to the remote peer. It blocks until the
// (re)handshake completes, so a caller sending during a short reconnect gap
// waits for the link instead of stalling the streamer indefinitely.
func (t *DTLSTransport) Send(to string, data []byte) error {
	if err := t.waitForConn(); err != nil {
		return err
	}
	t.mu.Lock()
	if t.conn == nil {
		t.mu.Unlock()
		return net.ErrClosed
	}
	conn := t.conn
	t.mu.Unlock()
	// For DTLS client with fixed remote, the connection is already connected
	// For server, we need to handle multiple peers (not implemented yet)
	// Currently we only support single-peer DTLS connections
	_, err := conn.Write(data)
	if err != nil && !t.closed.Load() {
		t.disconnect(conn, err)
	}
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

	t.startReader()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.handlers, id)
			t.mu.Unlock()
		})
	}
}

func (t *DTLSTransport) startReader() {
	t.recvOnce.Do(func() {
		t.livenessOnce.Do(func() { go t.livenessLoop() })
		t.mu.Lock()
		t.recvStart = true
		t.mu.Unlock()
		go t.receiveLoop()
	})
}

func (t *DTLSTransport) receiveLoop() {
	defer close(t.recvDone)
	buf := make([]byte, 65535)
	for {
		t.mu.Lock()
		ready, conn := t.connDone, t.conn
		t.mu.Unlock()
		if conn == nil {
			select {
			case <-t.stop:
				return
			case <-ready:
				continue
			}
		}
		n, err := conn.Read(buf)
		if err != nil {
			if t.closed.Load() {
				return
			}
			t.disconnect(conn, err)
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		// For DTLS, the remote address is fixed
		var raddr *net.UDPAddr
		raddr = conn.RemoteAddr().(*net.UDPAddr)

		t.lastActivity.Store(time.Now().UnixNano())
		if bytes.Equal(data, dtlsKeepAlive) {
			continue
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
	if t.recvStarted() {
		<-t.recvDone
	}
}

func (t *DTLSTransport) recvStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.recvStart
}

// Closed reports whether the transport has been closed
func (t *DTLSTransport) Closed() bool {
	return t.closed.Load()
}

// Close closes the DTLS connection
func (t *DTLSTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(t.stop)
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if t.listener != nil {
		_ = t.listener.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
	<-t.connectDone
	return nil
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
