//go:build noaudio

package audio

// ListDevices returns a static list of fake audio devices for CI / machines
// without audio hardware.
func ListDevices() ([]Device, error) {
	return []Device{
		{
			ID:                0,
			Name:              "Emulated Microphone",
			MaxInput:          2,
			MaxOutput:         0,
			DefaultSampleRate: 44100,
		},
		{
			ID:                1,
			Name:              "Emulated Speaker",
			MaxInput:          0,
			MaxOutput:         2,
			DefaultSampleRate: 44100,
		},
	}, nil
}
