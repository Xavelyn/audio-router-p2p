//go:build !noaudio

package audio

import "github.com/gordonklaus/portaudio"

// ListDevices returns all PortAudio devices in a simplified struct.
func ListDevices() ([]Device, error) {
	if err := portaudio.Initialize(); err != nil {
		return nil, err
	}
	defer portaudio.Terminate()
	devs, err := portaudio.Devices()
	if err != nil {
		return nil, err
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
