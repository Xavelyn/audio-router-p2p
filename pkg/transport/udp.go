package transport

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
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
		return net.ErrClosed
	}
	raddr, err := net.ResolveUDPAddr("udp", to)
	if err != nil {
		return err
	}
	_, err = t.conn.WriteToUDP(data, raddr)
	return err
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
		// Copy the slice: the next ReadFromUDP call may reuse buf.
		data := make([]byte, n)
		copy(data, buf[:n])
		t.mu.Lock()
		handlers := make([]func([]byte, *net.UDPAddr), 0, len(t.handlers))
		for _, h := range t.handlers {
			handlers = append(handlers, h)
		}
		t.mu.Unlock()
		for _, h := range handlers {
			h(data, addr)
		}
	}
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
