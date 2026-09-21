package streamer

import (
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/transport"
)

// Transport interface abstracts the underlying transport (UDP or DTLS)
type Transport interface {
	Addr() string
	LocalAddr() *net.UDPAddr
	Send(to string, data []byte) error
	Receive(handler func([]byte, *net.UDPAddr))
	Wait()
	Closed() bool
	Close() error
}

// Ensure transport types implement the interface
var _ Transport = (*transport.UDPTransport)(nil)
var _ Transport = (*transport.DTLSTransport)(nil)

// StreamDirection indicates the direction of a stream.
type StreamDirection int

const (
	DirSendOnly StreamDirection = iota
	DirRecvOnly
	DirSendRecv
)

func (d StreamDirection) String() string {
	switch d {
	case DirSendOnly:
		return "sendonly"
	case DirRecvOnly:
		return "recvonly"
	case DirSendRecv:
		return "sendrecv"
	}
	return "sendrecv"
}

// RemotePeer represents a remote peer in a stream session.
type RemotePeer struct {
	Addr       string
	StreamID   [16]byte
	Direction  StreamDirection
	Negotiator *StreamNegotiator
	JitterBuf  JitterBufferInterface
	Resampler  *Resampler
	Output     audio.AudioStream
}

// StreamConfig holds configuration for a stream session.
type StreamConfig struct {
	SampleRate    int
	Channels      int
	PacketTime    int       // ms per packet
	JitterBufSize int       // frames
	MinDelay      time.Duration
	MaxDelay      time.Duration
}

func DefaultStreamConfig(sampleRate, channels int) StreamConfig {
	return StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    20,
		JitterBufSize: 200,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      200 * time.Millisecond,
	}
}

// Streamer handles bidirectional audio streaming with negotiation, jitter buffering, and resampling.
type Streamer struct {
	transport  Transport
	localIPs   []*net.IP
	config     StreamConfig
	streamID   [16]byte
	localNodeID string

	mu          sync.Mutex
	started     bool
	stopCh      chan struct{}
	stopOnce    sync.Once
	errCh       chan error
	doneCh      chan struct{}

	// Send path
	sendSeq     uint32

	// Receive path
	recvWG      sync.WaitGroup
	peers       map[string]*RemotePeer // keyed by peer address
	peersMu     sync.RWMutex
}

func NewStreamer(trans Transport, localIPs []string, config StreamConfig, localNodeID string) (*Streamer, error) {
	if config.SampleRate <= 0 {
		return nil, errors.New("streamer: sample rate must be positive")
	}
	if config.Channels < 1 {
		return nil, errors.New("streamer: channel count must be positive")
	}
	var sid [16]byte
	if _, err := rand.Read(sid[:]); err != nil {
		return nil, err
	}
	s := &Streamer{
		transport:   trans,
		config:      config,
		streamID:    sid,
		localNodeID: localNodeID,
		peers:       make(map[string]*RemotePeer),
	}
	for _, a := range localIPs {
		ip, err := parseIP(a)
		if err != nil {
			return nil, err
		}
		s.localIPs = append(s.localIPs, &ip)
	}
	return s, nil
}

func parseIP(hostport string) (net.IP, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	ip := net.ParseIP(host)
	if ip == nil {
		addrs, err := net.LookupIP(host)
		if err != nil || len(addrs) == 0 {
			return nil, errors.New("streamer: invalid local address " + hostport)
		}
		ip = addrs[0]
	}
	return ip, nil
}

// Start begins bidirectional streaming. It performs format negotiation with
// remote peers before starting the send/receive loops.
func (s *Streamer) Start(input audio.AudioStream, destAddrs []string, output audio.AudioStream) error {
	if input == nil && output == nil {
		return errors.New("streamer: both input and output are nil")
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("streamer: already started")
	}
	s.started = true
	s.stopCh = make(chan struct{})
	s.errCh = make(chan error, 2)
	s.doneCh = make(chan struct{})
	stopCh := s.stopCh
	s.mu.Unlock()

	// Initialize peers and negotiate
	s.peersMu.Lock()
	for _, addr := range destAddrs {
		isLocal := s.isLocalDest(addr)
		peer := &RemotePeer{
			Addr:      addr,
			Direction: DirSendRecv,
			Output:    output,
		}
		if !isLocal && s.transport != nil {
			peer.Negotiator = NewStreamNegotiator(s.streamID, s.localNodeID, "")
			jb := NewJitterBuffer(
				s.config.Channels*2,
				s.config.SampleRate,
				s.config.Channels,
				s.config.JitterBufSize,
			)
			jb.minDelay = s.config.MinDelay
			jb.maxDelay = s.config.MaxDelay
			peer.JitterBuf = jb
		}
		s.peers[addr] = peer
	}
	s.peersMu.Unlock()

	// Start send loop if we have input
	if input != nil {
		s.recvWG.Add(1)
		go s.sendLoop(input, stopCh)
	}

	// Start receive loop if we have output or local destinations
	if output != nil || s.hasLocalDest(destAddrs) {
		if s.transport != nil {
			s.transport.Receive(s.handlePacket)
		}
		s.recvWG.Add(1)
		go s.recvLoop(stopCh)
	}

	go func() {
		s.recvWG.Wait()
		close(s.doneCh)
	}()
	return nil
}

func (s *Streamer) hasLocalDest(destAddrs []string) bool {
	for _, addr := range destAddrs {
		if s.isLocalDest(addr) {
			return true
		}
	}
	return false
}

// sendLoop reads from input, packetizes, and sends to all destinations.
func (s *Streamer) sendLoop(input audio.AudioStream, stopCh chan struct{}) {
	defer s.recvWG.Done()
	buf := make([]byte, 4096)

	for {
		select {
		case <-stopCh:
			return
		default:
		}
		n, err := input.Read(buf)
		if err == io.EOF {
			s.stopOnce.Do(func() { close(s.stopCh) })
			return
		}
		if err != nil {
			s.errCh <- err
			s.stopOnce.Do(func() { close(s.stopCh) })
			return
		}
		if n == 0 {
			select {
			case <-stopCh:
				return
			case <-time.After(2 * time.Millisecond):
			}
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])

		hdr := transport.PacketHeader{
			Seq:        s.sendSeq,
			Timestamp:  uint64(time.Now().UnixNano()),
			SampleRate: uint32(s.config.SampleRate),
			Channels:   uint8(s.config.Channels),
		}
		copy(hdr.StreamID[:], s.streamID[:])
		pkt := transport.Marshal(hdr, payload)

		s.peersMu.RLock()
		for _, peer := range s.peers {
			if s.isLocalDest(peer.Addr) {
				if peer.Output != nil {
					if _, err := peer.Output.Write(payload); err != nil {
						log.Printf("streamer: local write error: %v", err)
					}
				}
			} else if s.transport != nil {
				if err := s.transport.Send(peer.Addr, pkt); err != nil && !errors.Is(err, net.ErrClosed) {
					log.Printf("streamer: send to %s: %v", peer.Addr, err)
				}
			}
		}
		s.peersMu.RUnlock()
		s.sendSeq++
	}
}

// recvLoop plays out audio from jitter buffers to the output stream.
func (s *Streamer) recvLoop(stopCh chan struct{}) {
	defer s.recvWG.Done()

	// Combine all jitter buffers - we play out at the rate of the first peer
	ticker := time.NewTicker(time.Duration(s.config.PacketTime) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			s.playout()
		}
	}
}

func (s *Streamer) playout() {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()

	for _, peer := range s.peers {
		if peer.JitterBuf == nil {
			continue
		}
		data, _, err := peer.JitterBuf.PopNonBlock()
		if err != nil {
			continue // underrun, silence
		}

		// Resample if needed
		if peer.Resampler != nil {
			data, _ = peer.Resampler.Resample(data)
		}

		if peer.Output != nil {
			if _, err := peer.Output.Write(data); err != nil {
				log.Printf("streamer: output write error: %v", err)
			}
		}
	}
}

func (s *Streamer) handlePacket(data []byte, addr *net.UDPAddr) {
	hdr, payload, err := transport.Unmarshal(data)
	if err != nil {
		log.Printf("streamer: unmarshal error: %v", err)
		return
	}

	// Verify stream ID
	if hdr.StreamID != s.streamID {
		return // not for us
	}

	peerAddr := addr.String()
	s.peersMu.RLock()
	peer, ok := s.peers[peerAddr]
	s.peersMu.RUnlock()

	if !ok {
		// Unknown peer - could be a new stream, ignore for now
		return
	}

	if peer.JitterBuf != nil {
		if dropped, err := peer.JitterBuf.Push(hdr.Seq, int64(hdr.Timestamp), payload); err != nil {
			log.Printf("streamer: jitter buffer push error: %v", err)
		} else if dropped > 0 {
			log.Printf("streamer: jitter buffer overrun for %s", peerAddr)
		}
	}
}

// Stop requests the streamer to terminate.
func (s *Streamer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh != nil {
		s.stopOnce.Do(func() { close(s.stopCh) })
	}
	if s.transport != nil {
		s.transport.Close()
	}
}

// Wait blocks until the streamer exits and returns its result.
func (s *Streamer) Wait() error {
	s.mu.Lock()
	done := s.doneCh
	s.mu.Unlock()
	if done == nil {
		return errors.New("streamer: not started")
	}
	<-done
	select {
	case err := <-s.errCh:
		return err
	default:
		return nil
	}
}

// StreamID returns this streamer's 16-byte stream identifier.
func (s *Streamer) StreamID() [16]byte { return s.streamID }

// Stats returns statistics for all peer connections.
func (s *Streamer) Stats() map[string]JitterBufferStats {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()

	stats := make(map[string]JitterBufferStats)
	for addr, peer := range s.peers {
		if peer.JitterBuf != nil {
			if jb, ok := peer.JitterBuf.(*JitterBuffer); ok {
				stats[addr] = jb.Stats()
			}
		}
	}
	return stats
}

// isLocalDest reports whether hostport resolves to a local IP.
func (s *Streamer) isLocalDest(hostport string) bool {
	ip, err := parseIP(hostport)
	if err != nil {
		return false
	}
	for _, l := range s.localIPs {
		if l.Equal(ip) {
			return true
		}
	}
	return false
}