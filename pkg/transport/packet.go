package transport

import (
	"encoding/binary"
	"fmt"
)

const HeaderSize = 33

// PacketHeader is the fixed-size wire header prepended to every audio packet.
type PacketHeader struct {
	Seq        uint32
	Timestamp  uint64
	StreamID   [16]byte // UUID
	SampleRate uint32   // Hz
	Channels   uint8    // number of channels
}

// Marshal converts header+payload into a byte slice.
// Layout (little endian): seq(4) timestamp(8) streamID(16) sampleRate(4) channels(1) payload...
func Marshal(header PacketHeader, payload []byte) []byte {
	buf := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], header.Seq)
	binary.LittleEndian.PutUint64(buf[4:12], header.Timestamp)
	copy(buf[12:28], header.StreamID[:])
	binary.LittleEndian.PutUint32(buf[28:32], header.SampleRate)
	buf[32] = header.Channels
	copy(buf[HeaderSize:], payload)
	return buf
}

// Unmarshal extracts the header and payload from a byte slice.
func Unmarshal(data []byte) (PacketHeader, []byte, error) {
	if len(data) < HeaderSize {
		return PacketHeader{}, nil, fmt.Errorf("packet too short: %d bytes (need at least %d)", len(data), HeaderSize)
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
	return hdr, data[HeaderSize:], nil
}
