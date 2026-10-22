package group

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
	"github.com/audio-router-p2p/pkg/streamer"
	"github.com/audio-router-p2p/pkg/transport"
)

// GroupPlaybackState is the playback state of a GroupStreamer.
type GroupPlaybackState int

const (
	GroupPlaybackIdle GroupPlaybackState = iota
	GroupPlaybackPlaying
	GroupPlaybackPaused
)

// GroupStreamerConfig configures a GroupStreamer.
type GroupStreamerConfig struct {
	Room         *Room
	Input        audio.AudioStream
	Output       audio.AudioStream
	StartLatency time.Duration
	SampleRate   int
	Channels     int
}

// GroupStreamer ties group audio I/O to a Room and reports playback state
// deterministically (leaderhood is derived from the member set, never a
// live multicast vote, so a converged room always has exactly one leader).
type GroupStreamer struct {
	room        *Room
	input       audio.AudioStream
	output      audio.AudioStream
	latency     time.Duration
	sampleRate  int
	channels    int
	packetBytes int
	streamID    [16]byte

	// Playout timing from the room's latency preset at join time.
	jitterFrames int
	minDelay     time.Duration
	maxDelay     time.Duration

	jitterBuf  *streamer.JitterBuffer
	bufMu      sync.Mutex
	adapter    *streamer.IngressAdapter
	unregister func()

	playing atomic.Bool
	stopCh  chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	sendSeq atomic.Uint32
}

// NewGroupStreamer creates a GroupStreamer bound to room for group playback.
func NewGroupStreamer(cfg GroupStreamerConfig) (*GroupStreamer, error) {
	if cfg.Room == nil {
		return nil, errNoRoom
	}
	sr := cfg.SampleRate
	if sr <= 0 {
		sr = 48000
	}
	ch := cfg.Channels
	if ch <= 0 {
		ch = 2
	}
	packetBytes := (sr * 20 / 1000) * ch * 2
	if packetBytes <= 0 {
		packetBytes = ch * 2 * 256
	}

	sid := streamer.DeriveStreamID("room", string(cfg.Room.ID()))

	// Playout timing follows the room's latency preset. All members of a
	// room should use the same preset so the group stream stays symmetric.
	timing := streamer.LatencyPreset(cfg.Room.LatencyPreset()).Timing()

	gs := &GroupStreamer{
		room:         cfg.Room,
		input:        cfg.Input,
		output:       cfg.Output,
		latency:      cfg.StartLatency,
		sampleRate:   sr,
		channels:     ch,
		packetBytes:  packetBytes,
		streamID:     sid,
		jitterFrames: timing.JitterFrames,
		minDelay:     timing.MinDelay,
		maxDelay:     timing.MaxDelay,
		adapter:      streamer.NewIngressAdapter(sr, ch),
		stopCh:       make(chan struct{}),
	}
	return gs, nil
}

// Start joins the room and begins group playback.
func (gs *GroupStreamer) Start() error {
	if err := gs.room.Join(); err != nil {
		return err
	}
	gs.playing.Store(true)

	// If output != nil and room has transport, listen for room audio packets
	if gs.output != nil && gs.room.transport != nil {
		jb := streamer.NewJitterBuffer(gs.packetBytes, gs.sampleRate, gs.channels, gs.jitterFrames)
		jb.SetDelayLimits(gs.minDelay, gs.maxDelay)
		gs.bufMu.Lock()
		gs.jitterBuf = jb
		gs.bufMu.Unlock()
		gs.unregister = gs.room.transport.Receive(gs.handlePacket)
		gs.wg.Add(1)
		go gs.playoutLoop()
	}

	// If input != nil, run send loop (only leader transmits)
	if gs.input != nil {
		gs.wg.Add(1)
		go gs.sendLoop()
	}

	log.Printf("group streamer %s: Start OK (leader=%s)", gs.room.id, gs.room.Leader())
	return nil
}

func (gs *GroupStreamer) handlePacket(data []byte, addr *net.UDPAddr) {
	if !gs.playing.Load() {
		return
	}
	hdr, payload, err := transport.Unmarshal(data)
	if err != nil {
		return
	}
	if hdr.StreamID != gs.streamID {
		return
	}
	if gs.adapter != nil {
		norm, err := gs.adapter.Process(payload, int(hdr.SampleRate), int(hdr.Channels))
		if err != nil {
			return
		}
		payload = norm
	}
	if gs.jitterBuf != nil {
		_, _ = gs.jitterBuf.Push(hdr.Seq, int64(hdr.Timestamp), payload)
	}
}

func (gs *GroupStreamer) playoutLoop() {
	defer gs.wg.Done()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-gs.stopCh:
			return
		case <-ticker.C:
			if gs.jitterBuf == nil || gs.output == nil {
				continue
			}
			data, _, err := gs.jitterBuf.PopNonBlock()
			if err != nil {
				continue
			}
			if _, err := gs.output.Write(data); err != nil {
				log.Printf("group streamer %s: output write error: %v", gs.room.id, err)
			}
		}
	}
}

func (gs *GroupStreamer) sendLoop() {
	defer gs.wg.Done()
	packetBuf := make([]byte, gs.packetBytes)
	offset := 0

	for {
		select {
		case <-gs.stopCh:
			return
		default:
		}

		n, err := gs.input.Read(packetBuf[offset:])
		if err != nil {
			return
		}
		if n == 0 {
			select {
			case <-gs.stopCh:
				return
			case <-time.After(2 * time.Millisecond):
			}
			continue
		}
		offset += n
		if offset < gs.packetBytes {
			continue
		}

		payload := make([]byte, gs.packetBytes)
		copy(payload, packetBuf[:gs.packetBytes])
		offset = 0

		if !gs.room.IsLeader() {
			continue
		}

		seq := gs.sendSeq.Add(1) - 1
		pts := uint64(time.Now().Add(gs.latency).UnixNano())
		hdr := transport.PacketHeader{
			Seq:        seq,
			Timestamp:  pts,
			SampleRate: uint32(gs.sampleRate),
			Channels:   uint8(gs.channels),
			StreamID:   gs.streamID,
		}
		pkt := transport.Marshal(hdr, payload)

		if gs.room.transport != nil {
			members := gs.room.Members()
			for _, m := range members {
				if m.PeerID != gs.room.localID && m.StreamAddr != "" {
					_ = gs.room.transport.Send(m.StreamAddr, pkt)
				}
			}
		}

		if gs.jitterBuf != nil {
			_, _ = gs.jitterBuf.Push(hdr.Seq, int64(hdr.Timestamp), payload)
		}
	}
}

// State reports the current group playback state.
func (gs *GroupStreamer) State() GroupPlaybackState {
	if gs.playing.Load() {
		return GroupPlaybackPlaying
	}
	return GroupPlaybackIdle
}

// ApplyPreset retunes the running group playout's delay limits to the
// preset's timing. The room manager calls it when the room's latency preset
// changes while joined. It is a no-op before Start or after Stop. The buffer
// capacity is fixed at join time.
func (gs *GroupStreamer) ApplyPreset(p streamer.LatencyPreset) {
	t := p.Timing()
	gs.bufMu.Lock()
	jb := gs.jitterBuf
	gs.bufMu.Unlock()
	if jb != nil {
		jb.SetDelayLimits(t.MinDelay, t.MaxDelay)
	}
}

// Stop halts group playback and leaves the room.
func (gs *GroupStreamer) Stop() {
	gs.once.Do(func() {
		close(gs.stopCh)
		gs.playing.Store(false)
		if gs.unregister != nil {
			gs.unregister()
			gs.unregister = nil
		}
		if gs.jitterBuf != nil {
			_ = gs.jitterBuf.Close()
		}
		if closer, ok := gs.input.(io.Closer); ok {
			_ = closer.Close()
		}
		done := make(chan struct{})
		go func() {
			gs.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			log.Printf("group streamer %s: timed out waiting for goroutines to exit", gs.room.id)
		}
		_ = gs.room.Leave()
	})
	log.Printf("group streamer %s: Stop", gs.room.id)
}

// GroupStreamerStats reports group-playback metrics.
type GroupStreamerStats struct {
	MemberCount int
	Underruns   uint64
	Overruns    uint64
	PacketsSent uint32
}

// Stats reports group-playback metrics.
func (gs *GroupStreamer) Stats() GroupStreamerStats {
	stats := GroupStreamerStats{
		MemberCount: len(gs.room.Members()) - 1,
		PacketsSent: gs.sendSeq.Load(),
	}
	if gs.jitterBuf != nil {
		jbStats := gs.jitterBuf.Stats()
		stats.Underruns = jbStats.Underruns
		stats.Overruns = jbStats.Overruns
	}
	return stats
}

var errNoRoom = errors.New("group: nil room")
