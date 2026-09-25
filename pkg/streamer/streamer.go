package streamer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/audio/dsp"
	"github.com/audio-router-p2p/pkg/transport"
)

// Transport interface abstracts the underlying transport (UDP or DTLS)
type Transport interface {
	Addr() string
	LocalAddr() *net.UDPAddr
	Send(to string, data []byte) error
	// Receive registers a handler for incoming datagrams and returns a
	// function that unregisters it. Transports may be shared by multiple
	// streamers (one socket per node), each registering its own handler.
	Receive(handler func([]byte, *net.UDPAddr)) (unregister func())
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
	Addr           string
	StreamID       [16]byte
	Direction      StreamDirection
	Negotiator     *StreamNegotiator
	JitterBuf      JitterBufferInterface
	Resampler      *Resampler
	IngressAdapter *IngressAdapter
	Output         audio.AudioStream
}

// StreamConfig holds configuration for a stream session.
type StreamConfig struct {
	SampleRate    int
	Channels      int
	PacketTime    int // ms per packet
	JitterBufSize int // frames
	MinDelay      time.Duration
	MaxDelay      time.Duration
	DTX           dsp.DTXConfig
}

func DefaultStreamConfig(sampleRate, channels int) StreamConfig {
	return StreamConfig{
		SampleRate:    sampleRate,
		Channels:      channels,
		PacketTime:    20,
		JitterBufSize: 200,
		MinDelay:      20 * time.Millisecond,
		MaxDelay:      200 * time.Millisecond,
		DTX:           dsp.DefaultDTXConfig(),
	}
}

// Streamer handles bidirectional audio streaming with negotiation, jitter buffering, and resampling.
type Streamer struct {
	transport   Transport
	localIPs    []*net.IP
	config      StreamConfig
	streamID    [16]byte
	localNodeID string

	mu       sync.Mutex
	started  bool
	output   audio.AudioStream
	stopCh   chan struct{}
	stopOnce sync.Once
	errCh    chan error
	doneCh   chan struct{}

	// DSP and DTX
	dtx               *dsp.DTXController
	packetsSuppressed atomic.Uint64

	// Send path
	sendSeq atomic.Uint32

	// Receive path
	recvWG      sync.WaitGroup
	peers       map[string]*RemotePeer // keyed by peer address
	peersMu     sync.RWMutex
	packetsRecv atomic.Uint64

	// unregister removes this streamer's handler from the (possibly shared)
	// transport. Set in Start, called in Stop.
	unregister func()
}

// DeriveStreamID deterministically computes a 16-byte stream identifier using
// HMAC-SHA256 (if a secret is provided) or SHA256 (if secret is empty).
func DeriveStreamID(secret, contextID string) [16]byte {
	var sum []byte
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(contextID))
		sum = mac.Sum(nil)
	} else {
		h := sha256.Sum256([]byte(contextID))
		sum = h[:]
	}
	var id [16]byte
	copy(id[:], sum[:16])
	return id
}

// SetStreamID sets the stream identifier.
func (s *Streamer) SetStreamID(id [16]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamID = id
}

func (s *Streamer) packetBytes() int {
	pb := (s.config.SampleRate * s.config.PacketTime / 1000) * s.config.Channels * 2
	if pb <= 0 {
		pb = s.config.Channels * 2 * 256
	}
	return pb
}

func NewStreamer(trans Transport, localIPs []string, config StreamConfig, localNodeID string) (*Streamer, error) {
	if config.SampleRate <= 0 {
		return nil, errors.New("streamer: sample rate must be positive")
	}
	if config.Channels < 1 {
		return nil, errors.New("streamer: channel count must be positive")
	}
	if config.DTX == (dsp.DTXConfig{}) {
		config.DTX = dsp.DefaultDTXConfig()
	}
	if config.DTX.FrameDuration <= 0 {
		config.DTX.FrameDuration = time.Duration(config.PacketTime) * time.Millisecond
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
		dtx:         dsp.NewDTXController(config.DTX),
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
	s.output = output
	s.stopCh = make(chan struct{})
	s.errCh = make(chan error, 2)
	s.doneCh = make(chan struct{})
	stopCh := s.stopCh
	s.mu.Unlock()

	packetBytes := s.packetBytes()

	// Initialize peers and negotiate
	s.peersMu.Lock()
	for _, addr := range destAddrs {
		isLocal := s.isLocalDest(addr)
		peer := &RemotePeer{
			Addr:           addr,
			Direction:      DirSendRecv,
			Output:         output,
			IngressAdapter: NewIngressAdapter(s.config.SampleRate, s.config.Channels),
		}
		if !isLocal && s.transport != nil {
			// No SDP handshake runs here (see the de-scope note in
			// negotiation.go): the format is fixed per route from config and
			// is carried per packet in transport.PacketHeader. RemotePeer
			// .Negotiator and .Resampler stay nil until that is wired.
			jb := NewJitterBuffer(
				packetBytes,
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
			s.unregister = s.transport.Receive(s.handlePacket)
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
	packetBytes := s.packetBytes()
	packetBuf := make([]byte, packetBytes)
	offset := 0

	for {
		select {
		case <-stopCh:
			return
		default:
		}
		n, err := input.Read(packetBuf[offset:])
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
		offset += n
		if offset < packetBytes {
			continue
		}

		offset = 0

		// Step DTX finite state machine and analyze signal energy
		shouldSend, isSID, _ := s.dtx.ProcessFrame(packetBuf[:packetBytes], s.config.Channels)
		if !shouldSend {
			s.packetsSuppressed.Add(1)
			continue
		}

		var flags uint8 = transport.FlagActive
		if isSID {
			flags = transport.FlagSID
		}

		s.mu.Lock()
		sid := s.streamID
		s.mu.Unlock()

		seq := s.sendSeq.Add(1) - 1
		hdr := transport.PacketHeader{
			Seq:        seq,
			Timestamp:  uint64(time.Now().UnixNano()),
			SampleRate: uint32(s.config.SampleRate),
			Channels:   uint8(s.config.Channels),
			Flags:      flags,
			StreamID:   sid,
		}
		pkt := transport.Marshal(hdr, packetBuf[:packetBytes])

		s.peersMu.RLock()
		for _, peer := range s.peers {
			if s.isLocalDest(peer.Addr) {
				if peer.Output != nil {
					if _, err := peer.Output.Write(packetBuf[:packetBytes]); err != nil {
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

	var emptyID [16]byte
	s.mu.Lock()
	sid := s.streamID
	outStream := s.output
	s.mu.Unlock()

	peerAddr := addr.String()
	s.peersMu.RLock()
	peer, ok := s.peers[peerAddr]
	s.peersMu.RUnlock()

	// Accept packets whose hdr.StreamID == s.streamID OR if s.streamID == [16]byte{}
	// (promiscuous/accept-all) OR if peer exists in s.peers.
	if hdr.StreamID != sid && sid != emptyID && !ok {
		return // not for us
	}

	if !ok {
		// If peer not in s.peers but output exists on the streamer,
		// dynamically register or accept peer so that receiver nodes can receive incoming streams!
		if outStream == nil {
			return
		}
		packetBytes := s.packetBytes()
		jb := NewJitterBuffer(
			packetBytes,
			s.config.SampleRate,
			s.config.Channels,
			s.config.JitterBufSize,
		)
		jb.minDelay = s.config.MinDelay
		jb.maxDelay = s.config.MaxDelay

		peer = &RemotePeer{
			Addr:           peerAddr,
			StreamID:       hdr.StreamID,
			Direction:      DirRecvOnly,
			Output:         outStream,
			JitterBuf:      jb,
			IngressAdapter: NewIngressAdapter(s.config.SampleRate, s.config.Channels),
		}

		s.peersMu.Lock()
		if existing, exists := s.peers[peerAddr]; exists {
			peer = existing
		} else {
			const maxDynamicPeers = 64
			if len(s.peers) >= maxDynamicPeers {
				s.peersMu.Unlock()
				return
			}
			s.peers[peerAddr] = peer
		}
		s.peersMu.Unlock()
	}

	s.packetsRecv.Add(1)

	if len(payload) == 0 && (hdr.Flags&transport.FlagSID) != 0 {
		payload = make([]byte, s.packetBytes())
	}

	if peer.IngressAdapter != nil {
		normalizedPayload, err := peer.IngressAdapter.Process(payload, int(hdr.SampleRate), int(hdr.Channels))
		if err != nil {
			log.Printf("streamer: ingress format adapter error: %v", err)
			return
		}
		payload = normalizedPayload
	}

	if peer.JitterBuf != nil {
		if dropped, err := peer.JitterBuf.PushWithFlags(hdr.Seq, int64(hdr.Timestamp), payload, hdr.Flags); err != nil {
			log.Printf("streamer: jitter buffer push error: %v", err)
		} else if dropped > 0 {
			log.Printf("streamer: jitter buffer overrun for %s", peerAddr)
		}
	}
}

// Stop requests the streamer to terminate.
// The streamer does not own the transport: transports are shared across
// routes (one socket per node), so closing it here would tear down every
// other active route. The owner (streamManager / caller) closes the
// transport. We only unregister our receive handler so we stop consuming
// packets targeting other routes.
func (s *Streamer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh != nil {
		s.stopOnce.Do(func() { close(s.stopCh) })
	}
	if s.unregister != nil {
		s.unregister()
		s.unregister = nil
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
func (s *Streamer) StreamID() [16]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamID
}

// StreamerStats holds aggregated telemetry and per-peer jitter buffer statistics.
type StreamerStats struct {
	StreamID          string                       `json:"streamId"`
	PacketsSent       uint64                       `json:"packetsSent"`
	PacketsReceived   uint64                       `json:"packetsReceived"`
	Underruns         uint64                       `json:"underruns"`
	Overruns          uint64                       `json:"overruns"`
	LatePackets       uint64                       `json:"latePackets"`
	EarlyPackets      uint64                       `json:"earlyPackets"`
	Peers             map[string]JitterBufferStats `json:"peers,omitempty"`
	RMSdBFS           float64                      `json:"rms_dbfs"`
	PeakdBFS          float64                      `json:"peak_dbfs"`
	DTXState          string                       `json:"dtx_state"`
	PacketsSuppressed uint64                       `json:"dtx_packets_suppressed_total"`
}

// Stats returns statistics for this streamer and all peer connections.
func (s *Streamer) Stats() StreamerStats {
	s.mu.Lock()
	sid := hex.EncodeToString(s.streamID[:])
	s.mu.Unlock()

	s.peersMu.RLock()
	defer s.peersMu.RUnlock()

	stats := StreamerStats{
		StreamID:          sid,
		PacketsSent:       uint64(s.sendSeq.Load()),
		PacketsReceived:   s.packetsRecv.Load(),
		PacketsSuppressed: s.packetsSuppressed.Load(),
		Peers:             make(map[string]JitterBufferStats, len(s.peers)),
	}

	if s.dtx != nil {
		metrics := s.dtx.LastMetrics()
		stats.RMSdBFS = metrics.RMSdBFS
		stats.PeakdBFS = metrics.PeakdBFS
		stats.DTXState = s.dtx.State().String()
	}

	for addr, peer := range s.peers {
		if peer.JitterBuf != nil {
			if jb, ok := peer.JitterBuf.(*JitterBuffer); ok {
				jbStats := jb.Stats()
				stats.Peers[addr] = jbStats
				stats.Underruns += jbStats.Underruns
				stats.Overruns += jbStats.Overruns
				stats.LatePackets += jbStats.LatePackets
				stats.EarlyPackets += jbStats.EarlyPackets
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
