package transport

import (
	"encoding/binary"
	"fmt"
)

// Header size constants for wire protocol versions.
const (
	HeaderSizeV1 = 33
	HeaderSizeV2 = 34
	HeaderSize   = HeaderSizeV2 // 34-byte header in evolved protocol
)

// Packet control flags carried at byte 33.
const (
	FlagActive    uint8 = 1 << 0 // 0x01: Active unsuppressed audio frame
	FlagSID       uint8 = 1 << 1 // 0x02: Silence Insertion Descriptor keepalive frame
	FlagFEC       uint8 = 1 << 2 // 0x04: RFC 5109 XOR Parity packet
	FlagRedundant uint8 = 1 << 3 // 0x08: Reconstructed media packet generated via FEC recovery
)

// PacketHeader is the fixed-size wire header prepended to every audio packet.
type PacketHeader struct {
	Seq        uint32
	Timestamp  uint64
	StreamID   [16]byte // UUID
	SampleRate uint32   // Hz
	Channels   uint8    // number of channels
	Flags      uint8    // frame control flags (FlagActive, FlagSID)
}

// Marshal converts header+payload into a 34-byte header wire format byte slice.
// Layout (little endian):
// seq(4) timestamp(8) streamID(16) sampleRate(4) channels(1) flags(1) payload...
func Marshal(header PacketHeader, payload []byte) []byte {
	flags := header.Flags
	if flags == 0 {
		flags = FlagActive
	}
	buf := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], header.Seq)
	binary.LittleEndian.PutUint64(buf[4:12], header.Timestamp)
	copy(buf[12:28], header.StreamID[:])
	binary.LittleEndian.PutUint32(buf[28:32], header.SampleRate)
	buf[32] = header.Channels
	buf[33] = flags
	copy(buf[HeaderSize:], payload)
	return buf
}

// Unmarshal extracts the header and payload from a byte slice.
// Supports both V2 34-byte header packets and backward-compatible V1 33-byte packets.
// When decoding a legacy 33-byte packet, hdr.Flags defaults to FlagActive and payload starts at index 33.
func Unmarshal(data []byte) (PacketHeader, []byte, error) {
	if len(data) < HeaderSizeV1 {
		return PacketHeader{}, nil, fmt.Errorf("packet too short: %d bytes (need at least %d)", len(data), HeaderSizeV1)
	}

	var hdr PacketHeader
	hdr.Seq = binary.LittleEndian.Uint32(data[0:4])
	hdr.Timestamp = binary.LittleEndian.Uint64(data[4:12])
	copy(hdr.StreamID[:], data[12:28])
	hdr.SampleRate = binary.LittleEndian.Uint32(data[28:32])
	hdr.Channels = data[32]

	if hdr.Channels == 0 {
		return PacketHeader{}, nil, fmt.Errorf("packet has zero channels")
	}
	if hdr.SampleRate == 0 {
		return PacketHeader{}, nil, fmt.Errorf("packet has zero sample rate")
	}

	// Legacy 33-byte header with zero payload
	if len(data) == HeaderSizeV1 {
		hdr.Flags = FlagActive
		return hdr, data[HeaderSizeV1:], nil
	}

	frameBytes := int(hdr.Channels) * 2
	if frameBytes <= 0 {
		frameBytes = 2
	}

	// In 16-bit PCM, valid audio payloads are exact multiples of frameBytes (which is always even).
	// Because HeaderSizeV1(33) and HeaderSizeV2(34) have opposite parity, exactly one can align.
	if (len(data)-HeaderSizeV2)%frameBytes == 0 {
		flags := data[33]
		if flags == 0 {
			flags = FlagActive
		}
		hdr.Flags = flags
		return hdr, data[HeaderSizeV2:], nil
	}

	if (len(data)-HeaderSizeV1)%frameBytes == 0 {
		hdr.Flags = FlagActive
		return hdr, data[HeaderSizeV1:], nil
	}

	// Fallback for non-audio or arbitrary byte payloads
	if len(data) >= HeaderSizeV2 {
		flags := data[33]
		if flags&(FlagActive|FlagSID|FlagFEC|FlagRedundant) != 0 {
			hdr.Flags = flags
			return hdr, data[HeaderSizeV2:], nil
		}
	}

	hdr.Flags = FlagActive
	return hdr, data[HeaderSizeV1:], nil
}
