package transport

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestFECHeader_MarshalUnmarshal(t *testing.T) {
	hdr := FECHeader{
		BaseSeq:    42,
		Mask:       0x000F,
		PayloadLen: 960,
	}
	buf := make([]byte, FECHeaderSize)
	if err := MarshalFECHeader(hdr, buf); err != nil {
		t.Fatalf("MarshalFECHeader failed: %v", err)
	}

	decoded, err := UnmarshalFECHeader(buf)
	if err != nil {
		t.Fatalf("UnmarshalFECHeader failed: %v", err)
	}

	if decoded != hdr {
		t.Fatalf("decoded header %+v does not match expected %+v", decoded, hdr)
	}
}

func TestFEC_SinglePacketLossRecovery(t *testing.T) {
	encoder, err := NewFECEncoder(4, 960)
	if err != nil {
		t.Fatalf("NewFECEncoder failed: %v", err)
	}
	decoder, err := NewFECDecoder(4, 960)
	if err != nil {
		t.Fatalf("NewFECDecoder failed: %v", err)
	}

	// Generate 4 media packets
	packets := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		packets[i] = make([]byte, 960)
		for j := range packets[i] {
			packets[i][j] = byte(i*10 + (j % 256))
		}
	}

	var parityPkt []byte
	baseSeq := uint32(100)
	for i := 0; i < 4; i++ {
		p, ok := encoder.Encode(baseSeq+uint32(i), packets[i])
		if i == 3 {
			if !ok || p == nil {
				t.Fatalf("expected parity packet on 4th frame")
			}
			parityPkt = append([]byte(nil), p...)
		} else {
			if ok {
				t.Fatalf("unexpected parity packet on frame %d", i)
			}
		}
	}

	// Test losing packet 2 (seq 102)
	// Deliver 100, 101, 103, then parity packet
	decoder.ProcessMedia(100, packets[0])
	decoder.ProcessMedia(101, packets[1])
	// 102 is lost
	decoder.ProcessMedia(103, packets[3])

	recovered, err := decoder.ProcessParity(parityPkt)
	if err != nil {
		t.Fatalf("ProcessParity failed: %v", err)
	}

	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered packet, got %d", len(recovered))
	}

	rec := recovered[0]
	if rec.Seq != 102 {
		t.Fatalf("expected recovered seq 102, got %d", rec.Seq)
	}
	if rec.Flags != FlagRedundant {
		t.Fatalf("expected FlagRedundant, got %d", rec.Flags)
	}
	if !bytes.Equal(rec.Data, packets[2]) {
		t.Fatalf("reconstructed packet does not match original packet 2")
	}

	stats := decoder.Stats()
	if stats.PacketsRecoveredTotal != 1 {
		t.Fatalf("expected PacketsRecoveredTotal=1, got %d", stats.PacketsRecoveredTotal)
	}
	if stats.UnrecoverableGroupsTotal != 0 {
		t.Fatalf("expected UnrecoverableGroupsTotal=0, got %d", stats.UnrecoverableGroupsTotal)
	}
}

func TestFEC_TwoPacketLossUnrecoverable(t *testing.T) {
	encoder, _ := NewFECEncoder(4, 960)
	decoder, _ := NewFECDecoder(4, 960)

	packets := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		packets[i] = make([]byte, 960)
		rand.Read(packets[i])
	}

	baseSeq := uint32(200)
	var parityPkt []byte
	for i := 0; i < 4; i++ {
		p, ok := encoder.Encode(baseSeq+uint32(i), packets[i])
		if ok {
			parityPkt = append([]byte(nil), p...)
		}
	}

	// Lose both packet 1 (201) and packet 2 (202)
	decoder.ProcessMedia(200, packets[0])
	// 201 lost
	// 202 lost
	decoder.ProcessMedia(203, packets[3])

	recovered, err := decoder.ProcessParity(parityPkt)
	if err != nil {
		t.Fatalf("ProcessParity failed: %v", err)
	}

	if len(recovered) != 0 {
		t.Fatalf("expected 0 recovered packets for double loss, got %d", len(recovered))
	}

	// Before reaching the reorder horizon, group is not yet declared unrecoverable
	stats := decoder.Stats()
	if stats.PacketsRecoveredTotal != 0 {
		t.Fatalf("expected 0 recovered packets, got %d", stats.PacketsRecoveredTotal)
	}
	if stats.UnrecoverableGroupsTotal != 0 {
		t.Fatalf("expected 0 unrecoverable groups within reorder horizon, got %d", stats.UnrecoverableGroupsTotal)
	}

	// Media packet beyond reorder horizon (203 + groupSize=4 = 207) arrives
	decoder.ProcessMedia(207, packets[0])

	stats = decoder.Stats()
	if stats.UnrecoverableGroupsTotal != 1 {
		t.Fatalf("expected 1 unrecoverable group after passing reorder horizon, got %d", stats.UnrecoverableGroupsTotal)
	}
}

func TestFEC_ReorderedPackets(t *testing.T) {
	encoder, err := NewFECEncoder(4, 960)
	if err != nil {
		t.Fatalf("NewFECEncoder failed: %v", err)
	}
	decoder, err := NewFECDecoder(4, 960)
	if err != nil {
		t.Fatalf("NewFECDecoder failed: %v", err)
	}

	packets := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		packets[i] = make([]byte, 960)
		for j := range packets[i] {
			packets[i][j] = byte(i*10 + (j % 256))
		}
	}

	baseSeq := uint32(500)
	var parityPkt []byte
	for i := 0; i < 4; i++ {
		p, ok := encoder.Encode(baseSeq+uint32(i), packets[i])
		if ok {
			parityPkt = append([]byte(nil), p...)
		}
	}

	// Group contains 500, 501, 502, 503.
	// Packet 501 is lost.
	// Packets arrive reordered:
	// 503 arrives first
	decoder.ProcessMedia(503, packets[3])

	// Parity packet arrives second.
	// At this point, missing packets are 500, 501, 502 (missingCount = 3).
	// Because highestSeq is 503 (within groupSize reorder horizon of highestGroupSeq=503),
	// the group must NOT be marked unrecoverable.
	recParity, err := decoder.ProcessParity(parityPkt)
	if err != nil {
		t.Fatalf("ProcessParity failed: %v", err)
	}
	if len(recParity) != 0 {
		t.Fatalf("expected 0 recovered packets at parity arrival, got %d", len(recParity))
	}
	if stats := decoder.Stats(); stats.UnrecoverableGroupsTotal != 0 {
		t.Fatalf("group prematurely declared unrecoverable: got %d", stats.UnrecoverableGroupsTotal)
	}

	// 500 arrives third (out of order)
	rec500 := decoder.ProcessMedia(500, packets[0])
	if len(rec500) != 0 {
		t.Fatalf("expected 0 recovered packets after 500, got %d", len(rec500))
	}

	// 502 arrives fourth (out of order)
	// Now only 501 is missing, so receiving 502 must trigger recovery of 501!
	rec502 := decoder.ProcessMedia(502, packets[2])
	if len(rec502) != 1 {
		t.Fatalf("expected 1 recovered packet after 502, got %d", len(rec502))
	}
	if rec502[0].Seq != 501 {
		t.Fatalf("expected recovered packet 501, got %d", rec502[0].Seq)
	}
	if !bytes.Equal(rec502[0].Data, packets[1]) {
		t.Fatalf("recovered packet 501 data mismatch")
	}

	stats := decoder.Stats()
	if stats.PacketsRecoveredTotal != 1 {
		t.Fatalf("expected PacketsRecoveredTotal=1, got %d", stats.PacketsRecoveredTotal)
	}
	if stats.UnrecoverableGroupsTotal != 0 {
		t.Fatalf("expected UnrecoverableGroupsTotal=0, got %d", stats.UnrecoverableGroupsTotal)
	}
}

func TestFEC_OutOfOrderPackets(t *testing.T) {
	encoder, _ := NewFECEncoder(4, 960)
	decoder, _ := NewFECDecoder(4, 960)

	packets := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		packets[i] = make([]byte, 960)
		rand.Read(packets[i])
	}

	baseSeq := uint32(300)
	var parityPkt []byte
	for i := 0; i < 4; i++ {
		p, ok := encoder.Encode(baseSeq+uint32(i), packets[i])
		if ok {
			parityPkt = append([]byte(nil), p...)
		}
	}

	// Case: Parity packet arrives FIRST, then packet 300, 302, 303.
	// Packet 301 is missing.
	recovered, err := decoder.ProcessParity(parityPkt)
	if err != nil {
		t.Fatalf("ProcessParity failed: %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("expected 0 recovered immediately after parity with all media missing, got %d", len(recovered))
	}

	// Deliver 300
	rec := decoder.ProcessMedia(300, packets[0])
	if len(rec) != 0 {
		t.Fatalf("expected 0 recovered, got %d", len(rec))
	}

	// Deliver 302
	rec = decoder.ProcessMedia(302, packets[2])
	if len(rec) != 0 {
		t.Fatalf("expected 0 recovered, got %d", len(rec))
	}

	// Deliver 303 -> now only 301 is missing, so it should be reconstructed!
	rec = decoder.ProcessMedia(303, packets[3])
	if len(rec) != 1 {
		t.Fatalf("expected 1 recovered packet upon receiving packet 303, got %d", len(rec))
	}

	if rec[0].Seq != 301 {
		t.Fatalf("expected recovered seq 301, got %d", rec[0].Seq)
	}
	if !bytes.Equal(rec[0].Data, packets[1]) {
		t.Fatalf("reconstructed packet 301 does not match original")
	}
}

func BenchmarkFECEncoder_Encode(b *testing.B) {
	enc, err := NewFECEncoder(4, 960)
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 960)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		enc.Encode(uint32(i), buf)
	}
}

func BenchmarkFECDecoder_ProcessMedia(b *testing.B) {
	dec, err := NewFECDecoder(4, 960)
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 960)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dec.ProcessMedia(uint32(i), buf)
	}
}

func BenchmarkFECDecoder_Recovery(b *testing.B) {
	enc, _ := NewFECEncoder(4, 960)
	dec, _ := NewFECDecoder(4, 960)
	p0 := make([]byte, 960)
	p1 := make([]byte, 960)
	p2 := make([]byte, 960)
	p3 := make([]byte, 960)

	enc.Encode(0, p0)
	enc.Encode(1, p1)
	enc.Encode(2, p2)
	parity, _ := enc.Encode(3, p3)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		seqBase := uint32(i * 16)
		dec.ProcessMedia(seqBase+0, p0)
		dec.ProcessMedia(seqBase+1, p1)
		// seqBase+2 lost
		dec.ProcessMedia(seqBase+3, p3)
		dec.ProcessParity(parity)
	}
}
