package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/audio-router-p2p/pkg/audio"
)

// outputBus owns one physical output. Each route has one bounded pending block;
// a slow device cannot cause an unbounded queue of stale audio to accumulate.
type outputBus struct {
	mu     sync.Mutex
	device audio.AudioStream
	rate   int
	ch     int
	period time.Duration
	bytes  int
	slots  map[string]*busSlot
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

type busSlot struct {
	bus     *outputBus
	id      string
	pending []byte
	ready   bool
	closed  bool
}

func newOutputBus(device audio.AudioStream, rate, channels, packetTime int) *outputBus {
	b := &outputBus{
		device: device, rate: rate, ch: channels,
		period: time.Duration(packetTime) * time.Millisecond,
		bytes:  (rate * packetTime / 1000) * channels * 2,
		slots:  make(map[string]*busSlot), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *outputBus) add(id string) *busSlot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &busSlot{bus: b, id: id, pending: make([]byte, b.bytes)}
	b.slots[id] = s
	return s
}

func (b *outputBus) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.period)
	defer ticker.Stop()
	acc := make([]int32, b.bytes/2)
	pcm := make([]byte, b.bytes)
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
		}
		clear(acc)
		active := false
		b.mu.Lock()
		for _, slot := range b.slots {
			if !slot.ready {
				continue
			}
			active = true
			for i := range acc {
				acc[i] += int32(int16(binary.LittleEndian.Uint16(slot.pending[2*i:])))
			}
			slot.ready = false
		}
		b.mu.Unlock()
		if !active {
			continue
		}
		for i, v := range acc {
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(v)))
		}
		if _, err := b.device.Write(pcm); err != nil {
			// Device errors should not spin the loop; the ticker is the clock.
			log.Printf("output bus: device write: %v", err)
		}
	}
}

func (s *busSlot) Write(p []byte) (int, error) {
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.closed || b.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p)%(b.ch*2) != 0 {
		return 0, fmt.Errorf("output bus: unaligned PCM block of %d bytes", len(p))
	}
	clear(s.pending)
	copy(s.pending, p) // ASRC may differ by a frame; silence-pad or trim to the bus clock.
	s.ready = true     // replace the oldest pending frame if the output is behind
	return len(p), nil
}

func (s *busSlot) Read([]byte) (int, error) { return 0, nil }

// Counters reports the shared output device's hardware counters; every route
// on this bus drives the same physical device.
func (s *busSlot) Counters() audio.DeviceCounters { return audio.CountersOf(s.bus.device) }

func (s *busSlot) Close() error {
	b := s.bus
	b.mu.Lock()
	if s.closed {
		b.mu.Unlock()
		return nil
	}
	s.closed = true
	delete(b.slots, s.id)
	last := len(b.slots) == 0
	if last {
		b.closed = true
		close(b.stop)
	}
	b.mu.Unlock()
	if last {
		<-b.done
		return b.device.Close()
	}
	return nil
}

var _ audio.AudioStream = (*busSlot)(nil)
