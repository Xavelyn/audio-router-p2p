package streamer

import (
	"testing"
)

func TestNegotiationOfferAnswer(t *testing.T) {
	localFormats := []MediaFormat{
		{SampleRate: 48000, Channels: 2, Codec: "pcm16", FrameSize: 4, PacketTime: 20},
		{SampleRate: 44100, Channels: 1, Codec: "pcm16", FrameSize: 2, PacketTime: 20},
		{SampleRate: 44100, Channels: 2, Codec: "pcm16", FrameSize: 4, PacketTime: 20},
	}

	remoteFormats := []MediaFormat{
		{SampleRate: 44100, Channels: 1, Codec: "pcm16", FrameSize: 2, PacketTime: 20},
		{SampleRate: 48000, Channels: 2, Codec: "opus", FrameSize: 4, PacketTime: 20},
	}

	// Create offer
	negA := NewStreamNegotiator([16]byte{1}, "node-A", "node-B")
	offer, err := negA.CreateOffer(localFormats, "sendrecv")
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	if offer.Type != "offer" {
		t.Fatalf("expected offer type, got %s", offer.Type)
	}

	// Receive offer and create answer
	negB := NewStreamNegotiator([16]byte{1}, "node-B", "node-A")
	answer, err := negB.ReceiveOffer(offer, remoteFormats)
	if err != nil {
		t.Fatalf("ReceiveOffer error: %v", err)
	}
	if answer.Type != "answer" {
		t.Fatalf("expected answer type, got %s", answer.Type)
	}
	if len(answer.Formats) != 1 {
		t.Fatalf("answer should have exactly one format, got %d", len(answer.Formats))
	}

	// Receive answer
	err = negA.ReceiveAnswer(answer)
	if err != nil {
		t.Fatalf("ReceiveAnswer error: %v", err)
	}

	// Check selected format
	fmtA, err := negA.SelectedFormat()
	if err != nil {
		t.Fatalf("SelectedFormat error: %v", err)
	}
	if fmtA.SampleRate != 44100 || fmtA.Channels != 1 || fmtA.Codec != "pcm16" {
		t.Fatalf("unexpected selected format: %+v", fmtA)
	}

	fmtB, err := negB.SelectedFormat()
	if err != nil {
		t.Fatalf("SelectedFormat error: %v", err)
	}
	if fmtB.SampleRate != 44100 || fmtB.Channels != 1 || fmtB.Codec != "pcm16" {
		t.Fatalf("unexpected selected format: %+v", fmtB)
	}
}

func TestNegotiationNoMatch(t *testing.T) {
	localFormats := []MediaFormat{
		{SampleRate: 48000, Channels: 2, Codec: "pcm16", FrameSize: 4, PacketTime: 20},
	}

	remoteFormats := []MediaFormat{
		{SampleRate: 44100, Channels: 1, Codec: "opus", FrameSize: 2, PacketTime: 20},
	}

	negA := NewStreamNegotiator([16]byte{1}, "node-A", "node-B")
	offer, _ := negA.CreateOffer(localFormats, "sendrecv")

	negB := NewStreamNegotiator([16]byte{1}, "node-B", "node-A")
	_, err := negB.ReceiveOffer(offer, remoteFormats)
	if err == nil {
		t.Fatal("expected error for no matching format")
	}
}

func TestNegotiationMarshalUnmarshal(t *testing.T) {
	formats := []MediaFormat{
		{SampleRate: 44100, Channels: 2, Codec: "pcm16", FrameSize: 4, PacketTime: 20},
	}

	neg := NewStreamNegotiator([16]byte{1, 2, 3, 4}, "node-A", "node-B")
	offer, _ := neg.CreateOffer(formats, "sendrecv")

	data, err := offer.Marshal()
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	decoded, err := UnmarshalSessionDescription(data)
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if decoded.Type != offer.Type {
		t.Fatalf("type mismatch")
	}
	if decoded.StreamID != offer.StreamID {
		t.Fatalf("streamID mismatch")
	}
	if decoded.Origin != offer.Origin {
		t.Fatalf("origin mismatch")
	}
	if decoded.Nonce != offer.Nonce {
		t.Fatalf("nonce mismatch")
	}
	if len(decoded.Formats) != len(offer.Formats) {
		t.Fatalf("formats length mismatch")
	}
}

func TestNegotiationWait(t *testing.T) {
	localFormats := []MediaFormat{
		{SampleRate: 44100, Channels: 1, Codec: "pcm16", FrameSize: 2, PacketTime: 20},
	}

	negA := NewStreamNegotiator([16]byte{1}, "node-A", "node-B")
	offer, _ := negA.CreateOffer(localFormats, "sendrecv")

	negB := NewStreamNegotiator([16]byte{1}, "node-B", "node-A")
	answer, _ := negB.ReceiveOffer(offer, localFormats)

	// Answer in a goroutine to test Wait
	go func() {
		_ = negA.ReceiveAnswer(answer)
	}()

	selected, err := negA.Wait()
	if err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	if selected.SampleRate != 44100 {
		t.Fatalf("wrong format after wait")
	}
}
