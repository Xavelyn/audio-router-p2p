package transport

import (
	"encoding/binary"
	"testing"
)

func TestPacketMarshalUnmarshal(t *testing.T) {
	hdr := PacketHeader{Seq: 1234, Timestamp: 567890, SampleRate: 44100, Channels: 2}
	// Fill StreamID with deterministic data
	copy(hdr.StreamID[:], []byte("1234567890123456"))
	payload := []byte{0xAA, 0xBB, 0xCC}
	data := Marshal(hdr, payload)

	// Decode header fields back via Unmarshal
	decoded, pl, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if decoded.Seq != hdr.Seq || decoded.Timestamp != hdr.Timestamp || decoded.SampleRate != hdr.SampleRate || decoded.Channels != hdr.Channels {
		t.Fatalf("header mismatch after unmarshal")
	}
	if string(decoded.StreamID[:]) != string(hdr.StreamID[:]) {
		t.Fatalf("streamID mismatch after unmarshal")
	}
	if string(pl) != string(payload) {
		t.Fatalf("payload mismatch: got %x want %x", pl, payload)
	}
	// Verify total length = 33 header + payload
	expectedLen := 33 + len(payload)
	if len(data) != expectedLen {
		t.Fatalf("expected length %d got %d", expectedLen, len(data))
	}
	// Spot‑check binary encoding of sample rate
	sr := binary.LittleEndian.Uint32(data[28:32])
	if sr != hdr.SampleRate {
		t.Fatalf("sampleRate encoding mismatch: %d", sr)
	}
}
