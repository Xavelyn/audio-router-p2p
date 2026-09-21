//go:build noaudio

package audio

// OpenInput returns a synthetic sine-wave source. The real-device path is not
// available in the noaudio build.
func OpenInput(deviceID int, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	if channels < 1 {
		channels = 1
	}
	return newEmulatorInput(deviceID, sampleRate, channels), nil
}

// OpenOutput returns an in-memory sink in the noaudio build.
func OpenOutput(deviceID int, sampleRate, channels int, useEmulator bool) (AudioStream, error) {
	return newEmulatorOutput(deviceID), nil
}
