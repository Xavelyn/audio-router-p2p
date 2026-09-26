//go:build !noaudio

package audio

import "github.com/gordonklaus/portaudio"

// ListDevices returns all PortAudio devices in a simplified struct.
// If portaudio fails (e.g. no physical sound card / ALSA error), it returns
// fallback emulated devices so systems remain functional.
func ListDevices() ([]Device, error) {
	if err := portaudio.Initialize(); err != nil {
		return emulatedDeviceList(), nil
	}
	defer portaudio.Terminate()
	devs, err := portaudio.Devices()
	if err != nil || len(devs) == 0 {
		return emulatedDeviceList(), nil
	}
	out := make([]Device, 0, len(devs))
	for _, d := range devs {
		out = append(out, Device{
			ID:                d.Index,
			Name:              d.Name,
			MaxInput:          d.MaxInputChannels,
			MaxOutput:         d.MaxOutputChannels,
			DefaultSampleRate: d.DefaultSampleRate,
		})
	}
	return out, nil
}

func emulatedDeviceList() []Device {
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
	}
}
