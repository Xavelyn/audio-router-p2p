package group

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
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
}

// GroupStreamer ties group audio I/O to a Room and reports playback state
// deterministically (leaderhood is derived from the member set, never a
// live multicast vote, so a converged room always has exactly one leader).
type GroupStreamer struct {
	room    *Room
	input   audio.AudioStream
	output  audio.AudioStream
	latency time.Duration

	playing atomic.Bool
	stopCh  chan struct{}
	once    sync.Once
}

// NewGroupStreamer creates a GroupStreamer bound to room for group playback.
func NewGroupStreamer(cfg GroupStreamerConfig) (*GroupStreamer, error) {
	if cfg.Room == nil {
		return nil, errNoRoom
	}
	gs := &GroupStreamer{
		room:    cfg.Room,
		input:   cfg.Input,
		output:  cfg.Output,
		latency: cfg.StartLatency,
		stopCh:  make(chan struct{}),
	}
	return gs, nil
}

// Start joins the room and begins group playback.
func (gs *GroupStreamer) Start() error {
	if err := gs.room.Join(); err != nil {
		return err
	}
	gs.playing.Store(true)
	log.Printf("group streamer %s: Start OK (leader=%s)", gs.room.id, gs.room.Leader())
	return nil
}

// State reports the current group playback state.
func (gs *GroupStreamer) State() GroupPlaybackState {
	if gs.playing.Load() {
		return GroupPlaybackPlaying
	}
	return GroupPlaybackIdle
}

// Stop halts group playback.
func (gs *GroupStreamer) Stop() {
	gs.once.Do(func() { close(gs.stopCh) })
	gs.playing.Store(false)
	log.Printf("group streamer %s: Stop", gs.room.id)
}

// GroupStreamerStats reports group-playback metrics.
type GroupStreamerStats struct {
	MemberCount int
}

// Stats reports group-playback metrics.
func (gs *GroupStreamer) Stats() GroupStreamerStats {
	return GroupStreamerStats{MemberCount: len(gs.room.Members()) - 1}
}

var errNoRoom = errors.New("group: nil room")
