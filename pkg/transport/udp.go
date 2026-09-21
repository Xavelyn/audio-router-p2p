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
type UDPTransport struct {
	mu       sync.Mutex
	conn     *net.UDPConn
	closed   atomic.Bool
	recvDone chan struct{} // closed when the Receive loop exits
	recvOnce sync.Once
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

// Receive spawns a goroutine that delivers a copy of each received datagram
// to handler. The goroutine exits when the transport is closed. Use Wait() to
// block until it has fully stopped.
func (t *UDPTransport) Receive(handler func([]byte, *net.UDPAddr)) {
	t.recvOnce.Do(func() {
		go t.receiveLoop(handler)
	})
}

func (t *UDPTransport) receiveLoop(handler func([]byte, *net.UDPAddr)) {
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
		handler(data, addr)
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
