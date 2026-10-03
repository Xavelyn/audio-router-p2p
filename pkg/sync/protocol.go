package sync

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Message wire types for NTP-style clock synchronization.
const (
	msgRequest  = "sync-request"
	msgResponse = "sync-response"
)

// message is the JSON wire format for clock-sync ping and pong packets (all timestamps in nanoseconds).
//
// Protocol Layout:
//
//	Client                         Server
//	  |                              |
//	  |--- sync-request (T1, Nonce)->| (Server records T2)
//	  |                              |
//	  |<-- sync-response (T1..T4)----| (Server records T3)
//	(Client records T4)
type message struct {
	Type  string `json:"type"`  // "sync-request" | "sync-response"
	ID    string `json:"id"`    // Sender node ID (diagnostic/advisory only; not trusted for accounting)
	Nonce uint64 `json:"nonce"` // Cryptographically random nonce correlating request and response
	T1    int64  `json:"t1"`    // Client transmit timestamp (ns)
	T2    int64  `json:"t2"`    // Server receive timestamp (ns)
	T3    int64  `json:"t3"`    // Server transmit timestamp (ns)
	T4    int64  `json:"t4"`    // Client receive timestamp (ns, populated in response)
}

// ErrMalformedMessage is returned when a sync datagram cannot be parsed or validated.
var ErrMalformedMessage = errors.New("malformed sync message")

// encodeMessage serializes a sync message into JSON bytes.
func encodeMessage(msg *message) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("cannot encode nil sync message")
	}
	return json.Marshal(msg)
}

// decodeMessage deserializes JSON bytes into a sync message and validates essential fields.
func decodeMessage(data []byte) (*message, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrMalformedMessage)
	}
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedMessage, err)
	}
	if msg.Type != msgRequest && msg.Type != msgResponse {
		return nil, fmt.Errorf("%w: unknown message type %q", ErrMalformedMessage, msg.Type)
	}
	if msg.Nonce == 0 {
		return nil, fmt.Errorf("%w: zero nonce", ErrMalformedMessage)
	}
	return &msg, nil
}

// randomNonce generates a cryptographically secure 64-bit non-zero random nonce.
func randomNonce() (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("sync: crypto random nonce failed: %w", err)
		}
		val := binary.LittleEndian.Uint64(b[:])
		if val != 0 {
			return val, nil
		}
	}
}
