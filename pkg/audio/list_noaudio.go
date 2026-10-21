//go:build noaudio

package audio

import (
	"github.com/audio-router-p2p/pkg/audio/urn"
)

// ListDevices returns a static list of fake audio devices with canonical URNs
// for CI / machines without audio hardware.
func ListDevices() ([]Device, error) {
	micURN := urn.NewName("Emulated Microphone")
	spkURN := urn.NewName("Emulated Speaker")
	return []Device{
		{
			ID:                micURN,
			URN:               micURN,
			LegacyID:          "0",
			Name:              "Emulated Microphone",
			MaxInput:          2,
			MaxOutput:         0,
			DefaultSampleRate: 44100,
			IsVirtual:         false,
			Status:            "active",
			RuntimeID:         0,
		},
		{
			ID:                spkURN,
			URN:               spkURN,
			LegacyID:          "1",
			Name:              "Emulated Speaker",
			MaxInput:          0,
			MaxOutput:         2,
			DefaultSampleRate: 44100,
			IsVirtual:         false,
			Status:            "active",
			RuntimeID:         1,
		},
	}, nil
}
