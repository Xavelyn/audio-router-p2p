package streamer

// SDP-style format negotiation: implemented and unit-tested, but NOT wired
// into the streaming path (finding 3.1 de-scope decision).
//
// Today every participant of a stream must be configured with the same
// format; there is no SDP message type in the wire protocol and
// Streamer.handlePacket drops anything that is not an audio packet for a
// known stream ID, so an offer/answer could not be exchanged today. The
// per-packet format is instead advertised in transport.PacketHeader
// (SampleRate, Channels), so receivers already learn the sender format.
//
// Wiring this later requires: (1) an SDP message type in the packet
// protocol, (2) negotiation before audio flows, and (3) assigning
// RemotePeer.Resampler when the remote rate differs from the local one.
// Until then the constructors below are used only by tests, which keep the
// handshake logic proven and ready.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MediaFormat describes the audio format for a stream.
type MediaFormat struct {
	SampleRate     int    `json:"sampleRate"`
	Channels       int    `json:"channels"`
	FrameSize      int    `json:"frameSize"`  // bytes per frame (channels * 2 for int16)
	Codec          string `json:"codec"`      // "pcm16" for now
	PacketTime     int    `json:"packetTime"` // ms per packet (e.g., 20ms)
	MaxPacketBytes int    `json:"maxPacketBytes,omitempty"`
}

// SessionDescription is the SDP-like offer/answer for stream setup.
type SessionDescription struct {
	Type      string        `json:"type"` // "offer" or "answer"
	StreamID  [16]byte      `json:"streamId"`
	Origin    string        `json:"origin"`    // node ID
	Formats   []MediaFormat `json:"formats"`   // supported formats (offer) / selected format (answer)
	Direction string        `json:"direction"` // "sendonly", "recvonly", "sendrecv"
	Timestamp int64         `json:"ts"`        // unix nano
	Nonce     uint64        `json:"nonce"`     // for correlation
}

func NewOffer(origin string, streamID [16]byte, formats []MediaFormat, direction string) *SessionDescription {
	var nonce uint64
	b := make([]byte, 8)
	rand.Read(b)
	nonce = uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	return &SessionDescription{
		Type:      "offer",
		StreamID:  streamID,
		Origin:    origin,
		Formats:   formats,
		Direction: direction,
		Timestamp: time.Now().UnixNano(),
		Nonce:     nonce,
	}
}

func (o *SessionDescription) Answer(selected MediaFormat, direction string) *SessionDescription {
	return &SessionDescription{
		Type:      "answer",
		StreamID:  o.StreamID,
		Origin:    o.Origin,
		Formats:   []MediaFormat{selected},
		Direction: direction,
		Timestamp: time.Now().UnixNano(),
		Nonce:     o.Nonce,
	}
}

func (s *SessionDescription) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

func UnmarshalSessionDescription(data []byte) (*SessionDescription, error) {
	var sd SessionDescription
	if err := json.Unmarshal(data, &sd); err != nil {
		return nil, err
	}
	if sd.Type != "offer" && sd.Type != "answer" {
		return nil, fmt.Errorf("invalid SDP type: %s", sd.Type)
	}
	return &sd, nil
}

// NegotiationState tracks the handshake state for a stream.
type NegotiationState int

const (
	NegStateIdle NegotiationState = iota
	NegStateOfferSent
	NegStateOfferReceived
	NegStateAnswerSent
	NegStateAnswerReceived
	NegStateEstablished
	NegStateFailed
)

type StreamNegotiator struct {
	streamID     [16]byte
	localNodeID  string
	remoteNodeID string
	state        NegotiationState
	offer        *SessionDescription
	answer       *SessionDescription
	selectedFmt  *MediaFormat
	remoteFmts   []MediaFormat
	mu           sync.Mutex
	doneCh       chan struct{}
	err          error
}

func NewStreamNegotiator(streamID [16]byte, localNodeID, remoteNodeID string) *StreamNegotiator {
	return &StreamNegotiator{
		streamID:     streamID,
		localNodeID:  localNodeID,
		remoteNodeID: remoteNodeID,
		state:        NegStateIdle,
		doneCh:       make(chan struct{}),
	}
}

func (n *StreamNegotiator) CreateOffer(formats []MediaFormat, direction string) (*SessionDescription, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != NegStateIdle {
		return nil, fmt.Errorf("negotiation not idle: %v", n.state)
	}
	offer := NewOffer(n.localNodeID, n.streamID, formats, direction)
	n.offer = offer
	n.state = NegStateOfferSent
	return offer, nil
}

func (n *StreamNegotiator) ReceiveOffer(offer *SessionDescription, localFormats []MediaFormat) (*SessionDescription, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != NegStateIdle && n.state != NegStateOfferSent {
		return nil, fmt.Errorf("invalid state for offer: %v", n.state)
	}
	// Select best matching format
	selected := matchFormat(offer.Formats, localFormats)
	if selected == nil {
		n.state = NegStateFailed
		return nil, fmt.Errorf("no compatible format")
	}
	n.offer = offer
	n.remoteFmts = offer.Formats
	n.selectedFmt = selected
	answer := offer.Answer(*selected, reverseDirection(offer.Direction))
	n.answer = answer
	n.state = NegStateEstablished
	close(n.doneCh)
	return answer, nil
}

func (n *StreamNegotiator) ReceiveAnswer(answer *SessionDescription) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != NegStateOfferSent {
		return fmt.Errorf("invalid state for answer: %v", n.state)
	}
	if answer.Type != "answer" || answer.Nonce != n.offer.Nonce {
		return fmt.Errorf("answer mismatch")
	}
	if len(answer.Formats) != 1 {
		return fmt.Errorf("answer must have exactly one format")
	}
	n.answer = answer
	n.selectedFmt = &answer.Formats[0]
	n.state = NegStateEstablished
	close(n.doneCh)
	return nil
}

func (n *StreamNegotiator) SelectedFormat() (*MediaFormat, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.state != NegStateEstablished {
		return nil, fmt.Errorf("not established: %v", n.state)
	}
	return n.selectedFmt, nil
}

func (n *StreamNegotiator) Wait() (*MediaFormat, error) {
	<-n.doneCh
	return n.SelectedFormat()
}

func (n *StreamNegotiator) State() NegotiationState {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state
}

func matchFormat(remote, local []MediaFormat) *MediaFormat {
	for _, r := range remote {
		for _, l := range local {
			if r.Codec == l.Codec && r.SampleRate == l.SampleRate && r.Channels == l.Channels {
				return &l
			}
		}
	}
	return nil
}

func reverseDirection(dir string) string {
	switch dir {
	case "sendonly":
		return "recvonly"
	case "recvonly":
		return "sendonly"
	case "sendrecv":
		return "sendrecv"
	}
	return "sendrecv"
}
