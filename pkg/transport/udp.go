package transport

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// UDPTransport is a thin goroutine-safe wrapper around a UDP socket.
// It supports any number of registered receive handlers; each received
// datagram is delivered to every registered handler.
type UDPTransport struct {
	mu       sync.Mutex
	conn     *net.UDPConn
	closed   atomic.Bool
	recvDone chan struct{} // closed when the Receive loop exits
	recvOnce sync.Once

	handlers map[uint64]func([]byte, *net.UDPAddr)
	nextID   uint64

	// Observability & Telemetry Primitives (ADR 0021)
	packetsSent     atomic.Uint64
	bytesSent       atomic.Uint64
	sendErrors      atomic.Uint64
	packetsRecv     atomic.Uint64
	bytesRecv       atomic.Uint64
	zeroHandlerDrop atomic.Uint64

	firstRecvOnce       sync.Once
	firstSendOnce       sync.Once
	lastZeroDropLogNano atomic.Int64
	lastSendErrLogNano  atomic.Int64
}

// NewUDPTransport binds a UDP socket to listenAddr (e.g. ":45678" or "127.0.0.1:0").
func NewUDPTransport(listenAddr string) (*UDPTransport, error) {
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, err
	}
	// Use ListenConfig to set SO_REUSEADDR and allow port 0 for auto-assignment
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
		},
	}
	c, err := lc.ListenPacket(context.Background(), "udp", addr.String())
	if err != nil {
		return nil, err
	}
	udpConn, ok := c.(*net.UDPConn)
	if !ok {
		return nil, fmt.Errorf("expected UDPConn, got %T", c)
	}
	log.Printf("[UDP:BIND] listening on %s (reuseaddr=true)", udpConn.LocalAddr().String())
	return &UDPTransport{
		conn:     udpConn,
		recvDone: make(chan struct{}),
	}, nil
}

// Addr returns the local address as a string, or "" if the connection is nil.
func (t *UDPTransport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return ""
	}
	return t.conn.LocalAddr().String()
}

// LocalAddr returns the underlying local UDP address.
func (t *UDPTransport) LocalAddr() *net.UDPAddr {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.LocalAddr().(*net.UDPAddr)
}

// Send resolves to and writes a single datagram.
func (t *UDPTransport) Send(to string, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		t.logSendError(to, net.ErrClosed)
		return net.ErrClosed
	}
	raddr, err := net.ResolveUDPAddr("udp", to)
	if err != nil {
		t.logSendError(to, err)
		return err
	}
	n, err := t.conn.WriteToUDP(data, raddr)
	if err != nil {
		t.logSendError(to, err)
		return err
	}
	t.firstSendOnce.Do(func() {
		log.Printf("[UDP:SEND] first datagram sent to %s (len=%d bytes)", to, n)
	})
	t.packetsSent.Add(1)
	t.bytesSent.Add(uint64(n))
	return nil
}

func (t *UDPTransport) logSendError(to string, err error) {
	errCount := t.sendErrors.Add(1)
	if ShouldLogRateLimited(&t.lastSendErrLogNano, int64(time.Second)) {
		log.Printf("[UDP:SEND] ERROR: transmit to %s failed: %v (cumulative_errors=%d)", to, err, errCount)
	}
}

// Receive registers a handler for incoming datagrams and returns a function
// that unregisters it. Any number of handlers may be registered; each
// received datagram is delivered to every registered handler (streamers
// filter by stream ID internally). The receive loop starts on demand with
// the first registration and stops when the transport is closed.
func (t *UDPTransport) Receive(handler func([]byte, *net.UDPAddr)) (unregister func()) {
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

func (t *UDPTransport) receiveLoop() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if t.closed.Load() {
				close(t.recvDone)
				return
			}
			// Transient read error (e.g. ICMP) - back off briefly.
			log.Printf("UDP receive error: %v", err)
			continue
		}

		// First packet arrival edge trigger
		t.firstRecvOnce.Do(func() {
			log.Printf("[UDP:RECV] first datagram received on %s from %s (len=%d bytes)",
				t.Addr(), addr.String(), n)
		})

		// Update atomic counters for packets and bytes received
		t.packetsRecv.Add(1)
		t.bytesRecv.Add(uint64(n))

		t.mu.Lock()
		handlers := make([]func([]byte, *net.UDPAddr), 0, len(t.handlers))
		for _, h := range t.handlers {
			handlers = append(handlers, h)
		}
		t.mu.Unlock()

		if len(handlers) == 0 {
			drops := t.zeroHandlerDrop.Add(1)
			if ShouldLogRateLimited(&t.lastZeroDropLogNano, int64(time.Second)) {
				log.Printf("[UDP:RECV] WARN: datagram dropped - 0 handlers registered on socket=%s from=%s (bytes=%d, cumulative_drops=%d)",
					t.Addr(), addr.String(), n, drops)
			}
			continue
		}

		// Copy the slice: the next ReadFromUDP call may reuse buf.
		data := make([]byte, n)
		copy(data, buf[:n])
		for _, h := range handlers {
			h(data, addr)
		}
	}
}

// PacketsSent returns cumulative packets successfully transmitted.
func (t *UDPTransport) PacketsSent() uint64 {
	return t.packetsSent.Load()
}

// BytesSent returns cumulative bytes successfully transmitted.
func (t *UDPTransport) BytesSent() uint64 {
	return t.bytesSent.Load()
}

// SendErrors returns cumulative send errors.
func (t *UDPTransport) SendErrors() uint64 {
	return t.sendErrors.Load()
}

// PacketsRecv returns cumulative packets received on the socket.
func (t *UDPTransport) PacketsRecv() uint64 {
	return t.packetsRecv.Load()
}

// BytesRecv returns cumulative bytes received on the socket.
func (t *UDPTransport) BytesRecv() uint64 {
	return t.bytesRecv.Load()
}

// ZeroHandlerDrop returns cumulative packets dropped due to zero registered handlers.
func (t *UDPTransport) ZeroHandlerDrop() uint64 {
	return t.zeroHandlerDrop.Load()
}

// ShouldLogRateLimited evaluates whether a rate-limited log should be emitted.
// It executes with 0 heap allocations and zero mutex locks.
func ShouldLogRateLimited(lastLogNano *atomic.Int64, intervalNs int64) bool {
	now := time.Now().UnixNano()
	last := lastLogNano.Load()
	if now-last < intervalNs {
		return false
	}
	return lastLogNano.CompareAndSwap(last, now)
}

// Wait blocks until the Receive loop (if any) has exited.
func (t *UDPTransport) Wait() {
	<-t.recvDone
}

// Closed reports whether the transport has been closed.
func (t *UDPTransport) Closed() bool {
	return t.closed.Load()
}

// Close closes the underlying socket. It is idempotent.
func (t *UDPTransport) Close() error {
	t.closed.Store(true)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	return t.conn.Close()
}
