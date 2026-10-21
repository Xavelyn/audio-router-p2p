package streamer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/audio/dsp"
	"github.com/audio-router-p2p/pkg/telemetry"
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

// AudioTap records audio sample blocks and DTX state for real-time telemetry and metering.
type AudioTap interface {
	RecordBlock(pcm []byte, channels int)
	SetDTXState(state string)
}

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
	PeerID         string
	StreamID       [16]byte
	Direction      StreamDirection
	Negotiator     *StreamNegotiator
	JitterBuf      JitterBufferInterface
	Resampler      *Resampler
	IngressAdapter *IngressAdapter
	Output         audio.AudioStream
	FECDecoder     *transport.FECDecoder
	ASRC           *dsp.ASRCResampler
	asrcOutBuf     []byte
	lastPacket     time.Time // guarded by Streamer.peersMu for dynamically learned peers

	// Telemetry atomic accumulators for lock-free interrogation
	packetsSent  atomic.Uint64
	packetsRecv  atomic.Uint64
	bytesSent    atomic.Uint64
	bytesRecv    atomic.Uint64
	packetsLost  atomic.Uint64
	fecRecovered atomic.Uint64
	lastSeq      atomic.Uint32
	hasSeq       atomic.Bool

	lastRateCalc  atomic.Int64 // unix nano
	lastRateBytes atomic.Uint64
	effectiveKbps atomic.Uint64 // bits representation of float64
}

// FECConfig configures RFC 5109 1-D XOR Parity Matrix Forward Error Correction.
type FECConfig struct {
	Enabled   bool `json:"enabled"`
	GroupSize int  `json:"groupSize"` // K media packets per 1 parity packet (default 4)
}

// StreamConfig holds configuration for a stream session.
type StreamConfig struct {
	SampleRate      int
	Channels        int
	PacketTime      int // ms per packet
	JitterBufSize   int // frames
	MinDelay        time.Duration
	MaxDelay        time.Duration
	PeerIdleTimeout time.Duration // expiry for dynamically learned receive-only peers
	DTX             dsp.DTXConfig
	FEC             FECConfig
}

func DefaultStreamConfig(sampleRate, channels int) StreamConfig {
	return StreamConfig{
		SampleRate:      sampleRate,
		Channels:        channels,
		PacketTime:      20,
		JitterBufSize:   200,
		MinDelay:        20 * time.Millisecond,
		MaxDelay:        200 * time.Millisecond,
		PeerIdleTimeout: 5 * time.Second,
		DTX:             dsp.DefaultDTXConfig(),
		FEC: FECConfig{
			Enabled:   false,
			GroupSize: 4,
		},
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
	mixPCM   []byte
	mixAccum []int32
	stopCh   chan struct{}
	stopOnce sync.Once
	errCh    chan error
	doneCh   chan struct{}

	// DSP and DTX
	dtx               *dsp.DTXController
	packetsSuppressed atomic.Uint64
	audioTap          AudioTap
	audioTapMu        sync.RWMutex

	// Send path
	sendSeq       atomic.Uint32
	fecEncoder    *transport.FECEncoder
	fecEncoderMu  sync.Mutex
	fecParitySent atomic.Uint64

	// Receive path
	recvWG        sync.WaitGroup
	peers         map[string]*RemotePeer // keyed by peer address
	peersMu       sync.RWMutex
	packetsRecv   atomic.Uint64
	fecParityRecv atomic.Uint64
	fecRecovered  atomic.Uint64

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

// SetAudioTap attaches an audio telemetry tap for real-time RMS/peak metering.
func (s *Streamer) SetAudioTap(tap AudioTap) {
	s.audioTapMu.Lock()
	defer s.audioTapMu.Unlock()
	s.audioTap = tap
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
	if config.PeerIdleTimeout <= 0 {
		config.PeerIdleTimeout = 5 * time.Second
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

	if config.FEC.Enabled {
		groupSize := config.FEC.GroupSize
		if groupSize <= 0 {
			groupSize = 4
		}
		pb := (config.SampleRate * config.PacketTime / 1000) * config.Channels * 2
		if pb <= 0 {
			pb = config.Channels * 2 * 256
		}
		enc, err := transport.NewFECEncoder(groupSize, pb)
		if err != nil {
			return nil, err
		}
		s.fecEncoder = enc
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
		fecGroupSize := s.config.FEC.GroupSize
		if fecGroupSize <= 0 {
			fecGroupSize = 4
		}
		fecDec, _ := transport.NewFECDecoder(fecGroupSize, packetBytes)
		peer := &RemotePeer{
			Addr:           addr,
			Direction:      DirSendRecv,
			Output:         output,
			IngressAdapter: NewIngressAdapter(s.config.SampleRate, s.config.Channels),
			FECDecoder:     fecDec,
			ASRC:           dsp.NewASRCResampler(s.config.Channels),
			asrcOutBuf:     make([]byte, packetBytes*2+64),
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
			jb.targetDelay = s.config.MinDelay
			tw := float64(s.config.MinDelay) / float64(jb.frameDuration())
			if tw < 1.0 {
				tw = 1.0
			}
			jb.driftCtrl = NewClockDriftController(DefaultClockDriftConfig(tw))
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

		s.audioTapMu.RLock()
		tap := s.audioTap
		s.audioTapMu.RUnlock()
		if tap != nil {
			tap.RecordBlock(packetBuf[:packetBytes], s.config.Channels)
			tap.SetDTXState(s.dtx.State().String())
		}

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
						if !strings.Contains(err.Error(), "underflow") {
							log.Printf("streamer: local write error: %v", err)
						}
					} else {
						peer.packetsSent.Add(1)
						peer.bytesSent.Add(uint64(packetBytes))
					}
				}
			} else if s.transport != nil {
				if err := s.transport.Send(peer.Addr, pkt); err != nil && !errors.Is(err, net.ErrClosed) {
					log.Printf("streamer: send to %s: %v", peer.Addr, err)
				} else {
					peer.packetsSent.Add(1)
					peer.bytesSent.Add(uint64(len(pkt)))
				}
			}
		}
		s.peersMu.RUnlock()

		// Send RFC 5109 parity packet if group completed
		if s.fecEncoder != nil && !isSID {
			s.fecEncoderMu.Lock()
			parityPayload, ready := s.fecEncoder.Encode(seq, packetBuf[:packetBytes])
			if ready {
				s.fecParitySent.Add(1)
				paritySeq := s.sendSeq.Add(1) - 1
				parityHdr := transport.PacketHeader{
					Seq:        paritySeq,
					Timestamp:  uint64(time.Now().UnixNano()),
					SampleRate: uint32(s.config.SampleRate),
					Channels:   uint8(s.config.Channels),
					Flags:      transport.FlagFEC,
					StreamID:   sid,
				}
				parityPkt := transport.Marshal(parityHdr, parityPayload)
				s.peersMu.RLock()
				for _, peer := range s.peers {
					if !s.isLocalDest(peer.Addr) && s.transport != nil {
						if err := s.transport.Send(peer.Addr, parityPkt); err != nil && !errors.Is(err, net.ErrClosed) {
							log.Printf("streamer: send parity to %s: %v", peer.Addr, err)
						} else {
							peer.packetsSent.Add(1)
							peer.bytesSent.Add(uint64(len(parityPkt)))
						}
					}
				}
				s.peersMu.RUnlock()
			}
			s.fecEncoderMu.Unlock()
		}
	}
}

// recvLoop plays out audio from jitter buffers to the output stream.
func (s *Streamer) recvLoop(stopCh chan struct{}) {
	defer s.recvWG.Done()

	// Combine all jitter buffers - we play out at the rate of the first peer
	ticker := time.NewTicker(time.Duration(s.config.PacketTime) * time.Millisecond)
	defer ticker.Stop()
	cleanupEvery := s.config.PeerIdleTimeout / 2
	if cleanupEvery > time.Second {
		cleanupEvery = time.Second
	}
	if cleanupEvery < 10*time.Millisecond {
		cleanupEvery = 10 * time.Millisecond
	}
	cleanup := time.NewTicker(cleanupEvery)
	defer cleanup.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			s.playout()
		case <-cleanup.C:
			s.expireIdlePeers(time.Now())
		}
	}
}

// expireIdlePeers releases receive-only peers learned from the wire. Configured
// destinations have a different direction and must survive periods of silence.
func (s *Streamer) expireIdlePeers(now time.Time) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	s.expireIdlePeersLocked(now)
}

// Caller holds peersMu; also used before admitting a new sender so expired
// entries cannot keep the bounded peer table full until the next timer tick.
func (s *Streamer) expireIdlePeersLocked(now time.Time) {
	for addr, peer := range s.peers {
		if peer.Direction != DirRecvOnly || peer.lastPacket.IsZero() || now.Sub(peer.lastPacket) <= s.config.PeerIdleTimeout {
			continue
		}
		delete(s.peers, addr)
		if peer.JitterBuf != nil {
			_ = peer.JitterBuf.Close()
		}
	}
}

func (s *Streamer) playout() {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()

	var first []byte
	contributors := 0
	for _, peer := range s.peers {
		if peer.JitterBuf == nil || peer.Output == nil {
			continue
		}
		data, _, err := peer.JitterBuf.PopNonBlock()
		if err != nil {
			continue // underrun, silence
		}

		playData := data
		if peer.ASRC != nil {
			ratio := 1.0
			if jb, ok := peer.JitterBuf.(*JitterBuffer); ok && jb.driftCtrl != nil {
				ratio = jb.driftCtrl.Ratio()
			}
			if math.Abs(ratio-1.0) > 1.0e-6 {
				needed := len(data)*2 + 64
				if len(peer.asrcOutBuf) < needed {
					peer.asrcOutBuf = make([]byte, needed)
				}
				n := peer.ASRC.Process(data, peer.asrcOutBuf, ratio)
				if n > 0 {
					playData = peer.asrcOutBuf[:n]
				}
			}
		}
		if contributors == 0 {
			first = playData
			contributors = 1
			continue
		}
		// Accumulate in wider samples; clamp only after all contributors.
		if contributors == 1 {
			if len(s.mixAccum) < len(first)/2 {
				s.mixAccum = make([]int32, len(first)/2)
			}
			for i := 0; i < len(first)/2; i++ {
				s.mixAccum[i] = int32(int16(binary.LittleEndian.Uint16(first[2*i:])))
			}
		}
		// ASRC may produce slightly different lengths. Mix the common part.
		for i := 0; i < len(playData)/2 && i < len(first)/2; i++ {
			s.mixAccum[i] += int32(int16(binary.LittleEndian.Uint16(playData[2*i:])))
		}
		contributors++
	}
	if contributors == 0 || s.output == nil {
		return
	}
	if contributors > 1 {
		if len(s.mixPCM) < len(first) {
			s.mixPCM = make([]byte, len(first))
		}
		for i := 0; i < len(first)/2; i++ {
			v := s.mixAccum[i]
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(s.mixPCM[2*i:], uint16(int16(v)))
		}
		first = s.mixPCM[:len(first)]
	}
	if _, err := s.output.Write(first); err != nil {
		log.Printf("streamer: output write error: %v", err)
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
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	peer, ok := s.peers[peerAddr]

	// An address may carry several routes over a shared socket. A known peer
	// address never overrides the session ID check. The zero ID is retained as
	// an explicit accept-all mode for callers that intentionally request it.
	if sid != emptyID && hdr.StreamID != sid {
		return // not for us
	}
	// A sender can restart with sequence zero on the same UDP address. Start
	// with fresh jitter, FEC and sequence state after a genuine idle period.
	if ok && peer.Direction == DirRecvOnly && !peer.lastPacket.IsZero() && time.Since(peer.lastPacket) > s.config.PeerIdleTimeout {
		delete(s.peers, peerAddr)
		if peer.JitterBuf != nil {
			_ = peer.JitterBuf.Close()
		}
		ok = false
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
		jb.targetDelay = s.config.MinDelay
		tw := float64(s.config.MinDelay) / float64(jb.frameDuration())
		if tw < 1.0 {
			tw = 1.0
		}
		jb.driftCtrl = NewClockDriftController(DefaultClockDriftConfig(tw))

		fecGroupSize := s.config.FEC.GroupSize
		if fecGroupSize <= 0 {
			fecGroupSize = 4
		}
		fecDec, _ := transport.NewFECDecoder(fecGroupSize, packetBytes)
		peer = &RemotePeer{
			Addr:           peerAddr,
			StreamID:       hdr.StreamID,
			Direction:      DirRecvOnly,
			Output:         outStream,
			JitterBuf:      jb,
			IngressAdapter: NewIngressAdapter(s.config.SampleRate, s.config.Channels),
			FECDecoder:     fecDec,
			ASRC:           dsp.NewASRCResampler(s.config.Channels),
			asrcOutBuf:     make([]byte, packetBytes*2+64),
		}

		const maxDynamicPeers = 64
		if len(s.peers) >= maxDynamicPeers {
			s.expireIdlePeersLocked(time.Now())
			if len(s.peers) >= maxDynamicPeers {
				return
			}
		}
		s.peers[peerAddr] = peer
	}

	s.packetsRecv.Add(1)
	peer.packetsRecv.Add(1)
	peer.bytesRecv.Add(uint64(len(data)))

	// Sequence gap loss detection
	seq := hdr.Seq
	if peer.hasSeq.Load() {
		last := peer.lastSeq.Load()
		if seq > last+1 {
			gap := uint64(seq - last - 1)
			if gap < 100000 {
				peer.packetsLost.Add(gap)
			}
		}
	} else {
		peer.hasSeq.Store(true)
	}
	peer.lastSeq.Store(seq)

	// Handle RFC 5109 Parity Packet
	if (hdr.Flags & transport.FlagFEC) != 0 {
		s.fecParityRecv.Add(1)
		reconstructed, err := peer.FECDecoder.ProcessParity(payload)
		if err == nil && peer.Direction == DirRecvOnly {
			peer.lastPacket = time.Now()
		}
		if err == nil && len(reconstructed) > 0 {
			for _, rec := range reconstructed {
				s.fecRecovered.Add(1)
				peer.fecRecovered.Add(1)
				recData := rec.Data
				if peer.IngressAdapter != nil {
					if norm, err := peer.IngressAdapter.Process(recData, int(hdr.SampleRate), int(hdr.Channels)); err == nil {
						recData = norm
					}
				}
				if peer.JitterBuf != nil {
					peer.JitterBuf.PushWithFlags(rec.Seq, int64(hdr.Timestamp), recData, transport.FlagRedundant)
				}
			}
		}
		return
	}

	// For media packets, check if arriving packet solves a missing group
	if peer.FECDecoder != nil {
		reconstructed := peer.FECDecoder.ProcessMedia(hdr.Seq, payload)
		if len(reconstructed) > 0 {
			for _, rec := range reconstructed {
				s.fecRecovered.Add(1)
				peer.fecRecovered.Add(1)
				recData := rec.Data
				if peer.IngressAdapter != nil {
					if norm, err := peer.IngressAdapter.Process(recData, int(hdr.SampleRate), int(hdr.Channels)); err == nil {
						recData = norm
					}
				}
				if peer.JitterBuf != nil {
					peer.JitterBuf.PushWithFlags(rec.Seq, int64(hdr.Timestamp), recData, transport.FlagRedundant)
				}
			}
		}
	}

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
			if peer.Direction == DirRecvOnly {
				peer.lastPacket = time.Now()
			}
		} else if peer.Direction == DirRecvOnly {
			peer.lastPacket = time.Now()
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

// FECStats contains forward error correction metrics per ADR 0004.
type FECStats struct {
	GroupSizeK                 int    `json:"group_size_k"`
	ParityPacketsSentTotal     uint64 `json:"parity_packets_sent_total"`
	ParityPacketsReceivedTotal uint64 `json:"parity_packets_received_total"`
	PacketsRecoveredTotal      uint64 `json:"packets_recovered_total"`
	UnrecoverableGroupsTotal   uint64 `json:"unrecoverable_groups_total"`
}

// PLCStats contains packet loss concealment metrics per ADR 0004.
type PLCStats struct {
	ConcealedFramesTotal         uint64 `json:"concealed_frames_total"`
	PitchDetectedFramesTotal     uint64 `json:"pitch_detected_frames_total"`
	NoiseExtrapolatedFramesTotal uint64 `json:"noise_extrapolated_frames_total"`
	SilenceDecayEventsTotal      uint64 `json:"silence_decay_events_total"`
}

// ASRCStats contains asynchronous sample rate converter and drift metrics per ADR 0004.
type ASRCStats struct {
	NominalRateHz           int     `json:"nominal_rate_hz"`
	SmoothedWatermarkFrames float64 `json:"smoothed_watermark_frames"`
	TargetWatermarkFrames   float64 `json:"target_watermark_frames"`
	DriftPPM                float64 `json:"drift_ppm"`
	ResampleRatio           float64 `json:"resample_ratio"`
	SamplesAdjustedTotal    uint64  `json:"samples_adjusted_total"`
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
	FEC               FECStats                     `json:"fec"`
	PLC               PLCStats                     `json:"plc"`
	ASRC              ASRCStats                    `json:"asrc"`
}

// Stats returns statistics for this streamer and all peer connections.
func (s *Streamer) Stats() StreamerStats {
	s.mu.Lock()
	sid := hex.EncodeToString(s.streamID[:])
	s.mu.Unlock()

	s.peersMu.RLock()
	defer s.peersMu.RUnlock()

	fecK := s.config.FEC.GroupSize
	if fecK <= 0 {
		fecK = 4
	}

	stats := StreamerStats{
		StreamID:          sid,
		PacketsSent:       uint64(s.sendSeq.Load()),
		PacketsReceived:   s.packetsRecv.Load(),
		PacketsSuppressed: s.packetsSuppressed.Load(),
		Peers:             make(map[string]JitterBufferStats, len(s.peers)),
		FEC: FECStats{
			GroupSizeK:                 fecK,
			ParityPacketsSentTotal:     s.fecParitySent.Load(),
			ParityPacketsReceivedTotal: s.fecParityRecv.Load(),
			PacketsRecoveredTotal:      s.fecRecovered.Load(),
		},
		ASRC: ASRCStats{
			NominalRateHz: s.config.SampleRate,
			ResampleRatio: 1.0,
		},
	}

	if s.dtx != nil {
		metrics := s.dtx.LastMetrics()
		stats.RMSdBFS = metrics.RMSdBFS
		stats.PeakdBFS = metrics.PeakdBFS
		stats.DTXState = s.dtx.State().String()
	}

	for addr, peer := range s.peers {
		if peer.FECDecoder != nil {
			fDecStats := peer.FECDecoder.Stats()
			stats.FEC.UnrecoverableGroupsTotal += fDecStats.UnrecoverableGroupsTotal
		}
		if peer.ASRC != nil {
			stats.ASRC.SamplesAdjustedTotal += peer.ASRC.SamplesAdjusted()
		}
		if peer.JitterBuf != nil {
			if jb, ok := peer.JitterBuf.(*JitterBuffer); ok {
				jbStats := jb.Stats()
				stats.Peers[addr] = jbStats
				stats.Underruns += jbStats.Underruns
				stats.Overruns += jbStats.Overruns
				stats.LatePackets += jbStats.LatePackets
				stats.EarlyPackets += jbStats.EarlyPackets

				stats.PLC.ConcealedFramesTotal += jbStats.PLC.ConcealedFramesTotal
				stats.PLC.PitchDetectedFramesTotal += jbStats.PLC.PitchDetectedFramesTotal
				stats.PLC.NoiseExtrapolatedFramesTotal += jbStats.PLC.NoiseExtrapolatedFramesTotal
				stats.PLC.SilenceDecayEventsTotal += jbStats.PLC.SilenceDecayEventsTotal

				if jb.driftCtrl != nil {
					stats.ASRC.SmoothedWatermarkFrames = jb.driftCtrl.SmoothedWatermark()
					stats.ASRC.TargetWatermarkFrames = jb.driftCtrl.TargetWatermark()
					stats.ASRC.DriftPPM = jb.driftCtrl.DriftPPM()
					stats.ASRC.ResampleRatio = jb.driftCtrl.Ratio()
				}
			}
		}
	}
	return stats
}

// SetPeerID associates a peer address with an authoritative peer node ID.
func (s *Streamer) SetPeerID(addr string, peerID string) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	if p, ok := s.peers[addr]; ok {
		p.PeerID = peerID
	}
}

// RebindPeer changes the destination of a send-only UDP stream without
// restarting capture or resetting its sequence number. An empty address
// removes the destination while discovery considers the peer unavailable.
// The shared transport does not own a DTLS reconnect, so reject changes to
// its fixed connection rather than pretending packets will reach a new peer.
func (s *Streamer) RebindPeer(peerID, addr string) error {
	if peerID == "" {
		return errors.New("streamer: peer ID required for rebind")
	}
	if addr != "" {
		if _, err := net.ResolveUDPAddr("udp", addr); err != nil {
			return fmt.Errorf("streamer: invalid peer address %q: %w", addr, err)
		}
		if s.isLocalDest(addr) {
			return errors.New("streamer: cannot rebind remote peer to local bypass")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.output != nil {
		return errors.New("streamer: rebinding requires a started send-only stream")
	}
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	var previous *RemotePeer
	for oldAddr, peer := range s.peers {
		if peer.PeerID != peerID {
			continue
		}
		if oldAddr == addr {
			return nil
		}
		previous = peer
	}
	if previous != nil && s.transport != nil {
		if _, ok := s.transport.(*transport.DTLSTransport); ok {
			return errors.New("streamer: DTLS peer address is fixed; reconnect required")
		}
	}
	if addr != "" {
		if _, exists := s.peers[addr]; exists {
			return fmt.Errorf("streamer: destination %q already assigned", addr)
		}
		if len(s.peers) >= 64 && previous == nil {
			return errors.New("streamer: too many destinations")
		}
	}
	for oldAddr, peer := range s.peers {
		if peer.PeerID == peerID {
			delete(s.peers, oldAddr)
			if peer.JitterBuf != nil {
				_ = peer.JitterBuf.Close()
			}
		}
	}
	if addr != "" {
		p := &RemotePeer{Addr: addr, PeerID: peerID, Direction: DirSendOnly}
		if previous != nil {
			p.packetsSent.Store(previous.packetsSent.Load())
			p.bytesSent.Store(previous.bytesSent.Load())
		}
		s.peers[addr] = p
	}
	return nil
}

// PeerAddress reports the current destination for a bound peer (or empty
// while it is unavailable). It is safe to use during a live rebind.
func (s *Streamer) PeerAddress(peerID string) string {
	s.peersMu.RLock()
	defer s.peersMu.RUnlock()
	for addr, peer := range s.peers {
		if peer.PeerID == peerID {
			return addr
		}
	}
	return ""
}

// StreamLinkStats returns atomic link metrics per peer without stalling audio threads.
func (s *Streamer) StreamLinkStats() []telemetry.StreamLinkStats {
	s.mu.Lock()
	streamIDStr := hex.EncodeToString(s.streamID[:])
	s.mu.Unlock()

	s.peersMu.RLock()
	peers := make([]*RemotePeer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.peersMu.RUnlock()

	out := make([]telemetry.StreamLinkStats, 0, len(peers))
	now := time.Now().UnixNano()

	for _, peer := range peers {
		sent := peer.packetsSent.Load()
		recv := peer.packetsRecv.Load()
		lost := peer.packetsLost.Load()
		fecRec := peer.fecRecovered.Load()

		var lossRatio float64
		if (recv + lost) > 0 {
			lossRatio = float64(lost) / float64(recv+lost)
		}

		// Effective kbps calculation
		bytesTotal := peer.bytesRecv.Load() + peer.bytesSent.Load()
		lastTime := peer.lastRateCalc.Load()
		lastBytes := peer.lastRateBytes.Load()
		effectiveKbps := math.Float64frombits(peer.effectiveKbps.Load())

		if lastTime > 0 {
			dt := float64(now-lastTime) / 1e9
			if dt >= 0.2 && bytesTotal >= lastBytes {
				effectiveKbps = (float64(bytesTotal-lastBytes) * 8.0) / (dt * 1000.0)
				peer.effectiveKbps.Store(math.Float64bits(effectiveKbps))
				peer.lastRateBytes.Store(bytesTotal)
				peer.lastRateCalc.Store(now)
			}
		} else {
			peer.lastRateCalc.Store(now)
			peer.lastRateBytes.Store(bytesTotal)
		}

		var jbDepth int
		var targetDelayMS float64
		var underruns, overruns, plcConcealed uint64

		if peer.JitterBuf != nil {
			if jb, ok := peer.JitterBuf.(*JitterBuffer); ok {
				jbDepth, targetDelayMS, underruns, overruns, plcConcealed = jb.AtomicLinkStats()
			} else {
				st := peer.JitterBuf.Stats()
				jbDepth = st.BufferedFrames
				targetDelayMS = float64(st.TargetDelay.Milliseconds())
				underruns = st.Underruns
				overruns = st.Overruns
				plcConcealed = st.PLC.ConcealedFramesTotal
			}
		}

		peerID := peer.PeerID
		if peerID == "" {
			peerID = peer.Addr
		}

		out = append(out, telemetry.StreamLinkStats{
			StreamID:            streamIDStr,
			PeerID:              peerID,
			PacketsSent:         sent,
			PacketsReceived:     recv,
			PacketsLost:         lost,
			LossRatio:           lossRatio,
			EffectiveKbps:       effectiveKbps,
			JitterDepthFrames:   jbDepth,
			JitterTargetDelayMS: targetDelayMS,
			Underruns:           underruns,
			Overruns:            overruns,
			FECRecovered:        fecRec,
			PLCConcealed:        plcConcealed,
		})
	}

	return out
}

// isLocalDest reports whether hostport resolves to a local IP.
func (s *Streamer) isLocalDest(hostport string) bool {
	// A peer on the same host but a different UDP port is a distinct node,
	// not a local audio bypass. This matters for multi-daemon setups and
	// loopback integration tests.
	if s.transport != nil {
		_, port, err := net.SplitHostPort(hostport)
		if err != nil {
			return false
		}
		local := s.transport.LocalAddr()
		if local == nil || port != fmt.Sprint(local.Port) {
			return false
		}
	}
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
