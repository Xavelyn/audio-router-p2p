package audio

// Device describes an audio input/output device.
type Device struct {
	ID                int
	Name              string
	MaxInput          int
	MaxOutput         int
	DefaultSampleRate float64
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
