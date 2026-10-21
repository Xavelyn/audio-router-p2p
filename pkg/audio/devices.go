package audio

// Device describes an audio input/output device with persistent canonical identification.
type Device struct {
	ID                string // Canonical URN (Primary)
	URN               string // Explicit canonical URN
	LegacyID          string // Ephemeral PortAudio index or "0"
	Name              string
	MaxInput          int
	MaxOutput         int
	DefaultSampleRate float64
	IsVirtual         bool
	Status            string // "active", "quarantined", "disconnected"
	RuntimeID         int    // Internal PortAudio index
}

// BufferedStream is implemented by streams that expose the number of bytes
// currently buffered (used by the emulator and by monitoring code).
type BufferedStream interface {
	Buffered() int
}

// OutputBufferSize returns the number of bytes currently buffered in an output
// stream, or 0 if the stream does not expose buffering.
func OutputBufferSize(s AudioStream) int {
	if b, ok := s.(BufferedStream); ok {
		return b.Buffered()
	}
	return 0
}
