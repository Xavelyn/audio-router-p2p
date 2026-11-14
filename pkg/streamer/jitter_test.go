package streamer

import (
	"testing"
	"time"

	"github.com/audio-router-p2p/pkg/transport"
)

// TestJitterBuffer_AtomicLatePackets verifies that the lock-free
// atomicLatePackets counter (ADR 0034) matches the locked latePackets counter
// exactly after late and out-of-order pushes.
func TestJitterBuffer_AtomicLatePackets(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)

	baseTime := time.Now().Add(-time.Second).UnixNano()

	// Anchor the playout timeline with seq 10.
	if _, err := jb.PushWithFlags(10, baseTime, []byte{1, 2, 3, 4}, transport.FlagActive); err != nil {
		t.Fatalf("Push(10): %v", err)
	}
	if _, _, _, _, _, late := jb.AtomicLinkStats(); late != 0 {
		t.Fatalf("late after anchor = %d; want 0", late)
	}

	// Out-of-order packet (seq < nextSeq): the distFromNext < 0 branch.
	if _, err := jb.PushWithFlags(9, baseTime, []byte{9, 9, 9, 9}, transport.FlagActive); err != nil {
		t.Fatalf("Push(9): %v", err)
	}
	if _, _, _, _, _, late := jb.AtomicLinkStats(); late != 1 {
		t.Fatalf("late after out-of-order = %d; want 1", late)
	}

	// Packet that arrives after its expected playout time (+ target delay):
	// the `late` branch in PushWithFlags.
	time.Sleep(80 * time.Millisecond)
	if _, err := jb.PushWithFlags(11, baseTime, []byte{5, 6, 7, 8}, transport.FlagActive); err != nil {
		t.Fatalf("Push(11): %v", err)
	}

	if _, _, _, _, _, late := jb.AtomicLinkStats(); late != 2 {
		t.Errorf("AtomicLinkStats latePackets = %d; want 2", late)
	}
	if got := jb.Stats().LatePackets; got != 2 {
		t.Errorf("Stats().LatePackets = %d; want 2 (atomic must mirror locked counter)", got)
	}
}

// TestJitterBuffer_AtomicLatePackets_NoLate verifies a clean in-order,
// on-time burst produces zero late packets in the atomic counter.
func TestJitterBuffer_AtomicLatePackets_NoLate(t *testing.T) {
	jb := NewJitterBuffer(4, 48000, 2, 50)

	baseTime := time.Now().Add(-time.Second).UnixNano()
	for seq := uint32(10); seq < 15; seq++ {
		if _, err := jb.PushWithFlags(seq, baseTime, []byte{1, 2, 3, 4}, transport.FlagActive); err != nil {
			t.Fatalf("Push(%d): %v", seq, err)
		}
	}

	if _, _, _, _, _, late := jb.AtomicLinkStats(); late != 0 {
		t.Errorf("late = %d; want 0 for in-order on-time burst", late)
	}
	if got := jb.Stats().LatePackets; got != 0 {
		t.Errorf("Stats().LatePackets = %d; want 0", got)
	}
}
