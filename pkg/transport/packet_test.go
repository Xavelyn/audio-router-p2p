package transport

import (
	"encoding/binary"
	"testing"
)

// marshalV1 encodes a legacy 33-byte packet header without the flags byte.
func marshalV1(hdr PacketHeader, payload []byte) []byte {
	buf := make([]byte, HeaderSizeV1+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], hdr.Seq)
	binary.LittleEndian.PutUint64(buf[4:12], hdr.Timestamp)
	copy(buf[12:28], hdr.StreamID[:])
	binary.LittleEndian.PutUint32(buf[28:32], hdr.SampleRate)
	buf[32] = hdr.Channels
	copy(buf[HeaderSizeV1:], payload)
	return buf
}

func TestPacketMarshalUnmarshal_V2(t *testing.T) {
	hdr := PacketHeader{
		Seq:        1234,
		Timestamp:  567890,
		SampleRate: 44100,
		Channels:   2,
		Flags:      FlagActive,
	}
	copy(hdr.StreamID[:], []byte("1234567890123456"))
	payload := []byte{0xAA, 0xBB, 0xCC, 0xDD} // 4 bytes (frame-aligned for stereo)
	data := Marshal(hdr, payload)

	if len(data) != HeaderSizeV2+len(payload) {
		t.Fatalf("expected length %d, got %d", HeaderSizeV2+len(payload), len(data))
	}

	decoded, pl, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Seq != hdr.Seq || decoded.Timestamp != hdr.Timestamp ||
		decoded.SampleRate != hdr.SampleRate || decoded.Channels != hdr.Channels ||
		decoded.Flags != hdr.Flags {
		t.Fatalf("header mismatch after unmarshal: got %+v, want %+v", decoded, hdr)
	}
	if string(decoded.StreamID[:]) != string(hdr.StreamID[:]) {
		t.Fatalf("streamID mismatch")
	}
	if string(pl) != string(payload) {
		t.Fatalf("payload mismatch: got %x want %x", pl, payload)
	}
}

func TestPacketMarshalUnmarshal_V2_SIDFlag(t *testing.T) {
	hdr := PacketHeader{
		Seq:        999,
		Timestamp:  123456789,
		SampleRate: 48000,
		Channels:   2,
		Flags:      FlagSID,
	}
	copy(hdr.StreamID[:], []byte("abcdefghijklmnop"))
	payload := []byte{0x00, 0x00, 0x00, 0x00}
	data := Marshal(hdr, payload)

	decoded, _, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if decoded.Flags != FlagSID {
		t.Fatalf("expected FlagSID, got %v", decoded.Flags)
	}
}

func TestPacketUnmarshal_LegacyV1(t *testing.T) {
	hdr := PacketHeader{
		Seq:        555,
		Timestamp:  888888,
		SampleRate: 48000,
		Channels:   2,
	}
	copy(hdr.StreamID[:], []byte("legacystreamid12"))
	payload := []byte{0x11, 0x22, 0x33, 0x44} // 4 bytes frame-aligned

	legacyData := marshalV1(hdr, payload)
	if len(legacyData) != HeaderSizeV1+len(payload) {
		t.Fatalf("expected legacy len %d, got %d", HeaderSizeV1+len(payload), len(legacyData))
	}

	decoded, pl, err := Unmarshal(legacyData)
	if err != nil {
		t.Fatalf("failed to unmarshal legacy V1 packet: %v", err)
	}

	if decoded.Seq != hdr.Seq || decoded.Timestamp != hdr.Timestamp ||
		decoded.SampleRate != hdr.SampleRate || decoded.Channels != hdr.Channels {
		t.Fatalf("legacy header field mismatch: %+v vs %+v", decoded, hdr)
	}
	// Legacy packet should default to FlagActive
	if decoded.Flags != FlagActive {
		t.Fatalf("expected legacy packet to have FlagActive, got %v", decoded.Flags)
	}
	if string(pl) != string(payload) {
		t.Fatalf("legacy payload mismatch: got %x, want %x", pl, payload)
	}
}

func TestPacketUnmarshal_LegacyV1_ZeroPayload(t *testing.T) {
	hdr := PacketHeader{
		Seq:        1,
		Timestamp:  100,
		SampleRate: 48000,
		Channels:   1,
	}
	copy(hdr.StreamID[:], []byte("emptyheaderid123"))

	legacyData := marshalV1(hdr, nil)
	if len(legacyData) != HeaderSizeV1 {
		t.Fatalf("expected %d bytes, got %d", HeaderSizeV1, len(legacyData))
	}

	decoded, pl, err := Unmarshal(legacyData)
	if err != nil {
		t.Fatalf("failed to unmarshal 33-byte packet: %v", err)
	}
	if decoded.Flags != FlagActive {
		t.Fatalf("expected FlagActive, got %v", decoded.Flags)
	}
	if len(pl) != 0 {
		t.Fatalf("expected 0 payload, got %d bytes", len(pl))
	}
}

func TestPacketUnmarshal_TooShort(t *testing.T) {
	shortData := make([]byte, 32)
	_, _, err := Unmarshal(shortData)
	if err == nil {
		t.Fatal("expected error on < 33 bytes, got nil")
	}
}
