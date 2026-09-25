package transport

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
)

// FECHeaderSize is the fixed 8-byte wire header prepended to parity payloads.
// Wire format (little endian):
// BaseSeq (4 bytes uint32) | Mask (2 bytes uint16) | PayloadLen (2 bytes uint16)
const FECHeaderSize = 8

// FECHeader represents the decoded RFC 5109 parity header.
type FECHeader struct {
	BaseSeq    uint32
	Mask       uint16
	PayloadLen uint16
}

// MarshalFECHeader encodes an FECHeader into an 8-byte buffer.
func MarshalFECHeader(hdr FECHeader, out []byte) error {
	if len(out) < FECHeaderSize {
		return errors.New("fec: buffer too small for FEC header")
	}
	binary.LittleEndian.PutUint32(out[0:4], hdr.BaseSeq)
	binary.LittleEndian.PutUint16(out[4:6], hdr.Mask)
	binary.LittleEndian.PutUint16(out[6:8], hdr.PayloadLen)
	return nil
}

// UnmarshalFECHeader decodes an FECHeader from an 8-byte buffer.
func UnmarshalFECHeader(data []byte) (FECHeader, error) {
	if len(data) < FECHeaderSize {
		return FECHeader{}, errors.New("fec: data too short for FEC header")
	}
	return FECHeader{
		BaseSeq:    binary.LittleEndian.Uint32(data[0:4]),
		Mask:       binary.LittleEndian.Uint16(data[4:6]),
		PayloadLen: binary.LittleEndian.Uint16(data[6:8]),
	}, nil
}

// FECEncoder aggregates media packets into protection groups and generates
// 1-D XOR parity packets complying with RFC 5109.
// Hot path execution (Encode) maintains zero dynamic allocations.
type FECEncoder struct {
	mu             sync.Mutex
	groupSize      int
	maxPayloadSize int
	count          int
	baseSeq        uint32
	mask           uint16
	maxLen         int
	scratch        []byte // holds FECHeaderSize + maxPayloadSize
	sentParity     atomic.Uint64
}

// NewFECEncoder creates a new FECEncoder.
// groupSize is the number of media packets per parity packet (1 to 16, default 4).
// maxPayloadSize is the maximum expected payload size in bytes (default 4096).
func NewFECEncoder(groupSize, maxPayloadSize int) (*FECEncoder, error) {
	if groupSize <= 0 {
		groupSize = 4
	}
	if groupSize > 16 {
		return nil, errors.New("fec: groupSize exceeds maximum mask width (16)")
	}
	if maxPayloadSize <= 0 {
		maxPayloadSize = 4096
	}

	return &FECEncoder{
		groupSize:      groupSize,
		maxPayloadSize: maxPayloadSize,
		scratch:        make([]byte, FECHeaderSize+maxPayloadSize),
	}, nil
}

// Encode feeds a media packet into the FEC encoder.
// If the group is complete, it returns the parity payload (including 8-byte FEC header)
// and true. The returned slice is backed by the encoder's internal scratch buffer
// and is valid until the next call to Encode.
// Returns nil, false while accumulating frames.
func (e *FECEncoder) Encode(seq uint32, payload []byte) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	payloadLen := len(payload)
	if payloadLen > e.maxPayloadSize {
		payloadLen = e.maxPayloadSize
	}

	if e.count == 0 {
		e.baseSeq = seq
		e.mask = 1 << 0
		e.maxLen = payloadLen
		parityArea := e.scratch[FECHeaderSize : FECHeaderSize+payloadLen]
		copy(parityArea, payload[:payloadLen])
		e.count = 1
	} else {
		offset := seq - e.baseSeq
		if offset < 16 {
			e.mask |= 1 << offset
		}

		if payloadLen > e.maxLen {
			// Zero out scratch extension before XOR
			for i := e.maxLen; i < payloadLen; i++ {
				e.scratch[FECHeaderSize+i] = 0
			}
			e.maxLen = payloadLen
		}

		for i := 0; i < payloadLen; i++ {
			e.scratch[FECHeaderSize+i] ^= payload[i]
		}
		e.count++
	}

	if e.count >= e.groupSize {
		binary.LittleEndian.PutUint32(e.scratch[0:4], e.baseSeq)
		binary.LittleEndian.PutUint16(e.scratch[4:6], e.mask)
		binary.LittleEndian.PutUint16(e.scratch[6:8], uint16(e.maxLen))

		totalLen := FECHeaderSize + e.maxLen
		e.count = 0
		e.sentParity.Add(1)
		return e.scratch[:totalLen], true
	}

	return nil, false
}

// Reset clears the current accumulation group without resetting stats.
func (e *FECEncoder) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.count = 0
	e.baseSeq = 0
	e.mask = 0
	e.maxLen = 0
}

// ParityPacketsSentTotal returns the total number of parity packets emitted.
func (e *FECEncoder) ParityPacketsSentTotal() uint64 {
	return e.sentParity.Load()
}

// ReconstructedPacket represents a recovered media frame.
type ReconstructedPacket struct {
	Seq   uint32
	Flags uint8
	Data  []byte
}

type mediaSlot struct {
	seq    uint32
	length int
	valid  bool
	data   []byte
}

type paritySlot struct {
	valid         bool
	resolved      bool
	unrecoverable bool
	baseSeq       uint32
	mask          uint16
	payloadLen    uint16
	data          []byte
}

// FECDecoderStats contains diagnostic metrics for the FEC receiver.
type FECDecoderStats struct {
	ParityPacketsReceivedTotal uint64 `json:"parity_packets_received_total"`
	PacketsRecoveredTotal      uint64 `json:"packets_recovered_total"`
	UnrecoverableGroupsTotal   uint64 `json:"unrecoverable_groups_total"`
}

// FECDecoder reconstructs lost media packets from RFC 5109 parity packets.
// Zero heap allocations occur during steady-state media/parity processing.
type FECDecoder struct {
	mu             sync.Mutex
	groupSize      int
	maxPayloadSize int
	windowSize     int
	numParitySlots int

	mediaSlots    []mediaSlot
	paritySlots   []paritySlot
	scratch       []byte
	recovered     []ReconstructedPacket
	recoveredData [][]byte

	highestSeq uint32
	hasSeq     bool

	stats FECDecoderStats
}

// NewFECDecoder creates a new FECDecoder.
// groupSize is the protection group size $K$ (default 4).
// maxPayloadSize is the maximum expected frame size in bytes (default 4096).
func NewFECDecoder(groupSize, maxPayloadSize int) (*FECDecoder, error) {
	if groupSize <= 0 {
		groupSize = 4
	}
	if maxPayloadSize <= 0 {
		maxPayloadSize = 4096
	}

	// Sliding window covers at least 4 * groupSize packets (minimum 64)
	windowSize := groupSize * 8
	if windowSize < 64 {
		windowSize = 64
	}
	numParitySlots := 16

	mSlots := make([]mediaSlot, windowSize)
	for i := range mSlots {
		mSlots[i].data = make([]byte, maxPayloadSize)
	}

	pSlots := make([]paritySlot, numParitySlots)
	for i := range pSlots {
		pSlots[i].data = make([]byte, maxPayloadSize)
	}

	const maxRecoveredPerCall = 16
	recData := make([][]byte, maxRecoveredPerCall)
	for i := range recData {
		recData[i] = make([]byte, maxPayloadSize)
	}

	return &FECDecoder{
		groupSize:      groupSize,
		maxPayloadSize: maxPayloadSize,
		windowSize:     windowSize,
		numParitySlots: numParitySlots,
		mediaSlots:     mSlots,
		paritySlots:    pSlots,
		scratch:        make([]byte, maxPayloadSize),
		recovered:      make([]ReconstructedPacket, 0, maxRecoveredPerCall),
		recoveredData:  recData,
	}, nil
}

// ProcessMedia registers an incoming media packet into the sliding reconstruction window.
// If this packet allows resolving a previously missing packet in a known FEC group,
// any reconstructed packets are returned.
func (d *FECDecoder) ProcessMedia(seq uint32, payload []byte) []ReconstructedPacket {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.recovered = d.recovered[:0]

	if !d.hasSeq || int32(seq-d.highestSeq) > 0 {
		d.highestSeq = seq
		d.hasSeq = true
	}

	slotIdx := int(seq % uint32(d.windowSize))
	slot := &d.mediaSlots[slotIdx]
	slot.seq = seq
	n := copy(slot.data, payload)
	slot.length = n
	slot.valid = true

	d.tryRecover()
	return d.recovered
}

// ProcessParity decodes an RFC 5109 parity payload (including 8-byte FEC header)
// and attempts immediate reconstruction of any single missing packet.
func (d *FECDecoder) ProcessParity(parityPayload []byte) ([]ReconstructedPacket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.recovered = d.recovered[:0]
	if len(parityPayload) < FECHeaderSize {
		return nil, errors.New("fec: parity payload shorter than header size")
	}

	hdr, err := UnmarshalFECHeader(parityPayload)
	if err != nil {
		return nil, err
	}

	d.stats.ParityPacketsReceivedTotal++

	payloadLen := int(hdr.PayloadLen)
	if FECHeaderSize+payloadLen > len(parityPayload) {
		payloadLen = len(parityPayload) - FECHeaderSize
	}
	if payloadLen > d.maxPayloadSize {
		payloadLen = d.maxPayloadSize
	}

	// Find slot for this parity packet in the ring buffer
	slotIdx := int(hdr.BaseSeq % uint32(d.numParitySlots))
	p := &d.paritySlots[slotIdx]

	p.valid = true
	p.resolved = false
	p.unrecoverable = false
	p.baseSeq = hdr.BaseSeq
	p.mask = hdr.Mask
	p.payloadLen = uint16(payloadLen)
	copy(p.data[:payloadLen], parityPayload[FECHeaderSize:FECHeaderSize+payloadLen])

	d.tryRecover()
	return d.recovered, nil
}

// ProcessPacket inspects packet header flags and dispatches to ProcessParity or ProcessMedia.
func (d *FECDecoder) ProcessPacket(hdr PacketHeader, payload []byte) []ReconstructedPacket {
	if (hdr.Flags & FlagFEC) != 0 {
		reconstructed, err := d.ProcessParity(payload)
		if err != nil {
			return nil
		}
		return reconstructed
	}
	return d.ProcessMedia(hdr.Seq, payload)
}

func (d *FECDecoder) tryRecover() {
	for pIdx := 0; pIdx < d.numParitySlots; pIdx++ {
		p := &d.paritySlots[pIdx]
		if !p.valid || p.resolved || p.unrecoverable {
			continue
		}

		missingCount := 0
		var missingSeq uint32
		highestBit := 0

		for i := 0; i < 16; i++ {
			if (p.mask & (1 << i)) == 0 {
				continue
			}
			highestBit = i
			targetSeq := p.baseSeq + uint32(i)
			mSlot := &d.mediaSlots[int(targetSeq%uint32(d.windowSize))]
			if mSlot.valid && mSlot.seq == targetSeq {
				continue
			}
			missingCount++
			missingSeq = targetSeq
		}

		if missingCount == 0 {
			p.resolved = true
			continue
		}

		if missingCount == 1 {
			// Exactly one packet missing: perform XOR recovery
			payloadLen := int(p.payloadLen)
			copy(d.scratch[:payloadLen], p.data[:payloadLen])

			for i := 0; i < 16; i++ {
				if (p.mask & (1 << i)) == 0 {
					continue
				}
				targetSeq := p.baseSeq + uint32(i)
				if targetSeq == missingSeq {
					continue
				}
				mSlot := &d.mediaSlots[int(targetSeq%uint32(d.windowSize))]
				for b := 0; b < mSlot.length && b < payloadLen; b++ {
					d.scratch[b] ^= mSlot.data[b]
				}
			}

			// Store reconstructed packet into media window
			mSlot := &d.mediaSlots[int(missingSeq%uint32(d.windowSize))]
			mSlot.seq = missingSeq
			mSlot.length = payloadLen
			mSlot.valid = true
			copy(mSlot.data[:payloadLen], d.scratch[:payloadLen])

			// Append to recovered output slice
			recIdx := len(d.recovered)
			if recIdx < len(d.recoveredData) {
				copy(d.recoveredData[recIdx][:payloadLen], d.scratch[:payloadLen])
				d.recovered = append(d.recovered, ReconstructedPacket{
					Seq:   missingSeq,
					Flags: FlagRedundant,
					Data:  d.recoveredData[recIdx][:payloadLen],
				})
			}

			p.resolved = true
			d.stats.PacketsRecoveredTotal++
			continue
		}

		// missingCount >= 2
		// If we have received packets beyond the reordering horizon of this group,
		// the erasures are definitively unrecoverable.
		highestGroupSeq := p.baseSeq + uint32(highestBit)
		reorderHorizon := int32(d.groupSize)
		if d.hasSeq && int32(d.highestSeq-highestGroupSeq) >= reorderHorizon {
			p.unrecoverable = true
			d.stats.UnrecoverableGroupsTotal++
		}
	}
}

// Stats returns a snapshot of decoder diagnostics.
func (d *FECDecoder) Stats() FECDecoderStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}
