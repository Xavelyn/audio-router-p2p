//go:build !noaudio

package audio

import (
	"regexp"
	"strconv"

	"github.com/audio-router-p2p/pkg/audio/discovery"
	"github.com/audio-router-p2p/pkg/audio/urn"
	"github.com/gordonklaus/portaudio"
)

var hwIndexRegex = regexp.MustCompile(`\(hw:(\d+)(?:,(\d+))?\)`)

// ListDevices returns all PortAudio devices with stable canonical URNs.
// If portaudio fails (e.g. no physical sound card / ALSA error), it returns
// fallback emulated devices so systems remain functional.
func ListDevices() ([]Device, error) {
	return withSuppressedStderr(func() ([]Device, error) {
		if err := initPortAudio(); err != nil {
			return emulatedDeviceList(), nil
		}
		defer termPortAudio()

		devs, err := portaudio.Devices()
		if err != nil || len(devs) == 0 {
			return emulatedDeviceList(), nil
		}

		hwDevs, _ := discovery.DiscoverDevices()
		hwMap := make(map[string]discovery.DiscoveredDevice)
		for _, hd := range hwDevs {
			key := strconv.Itoa(hd.CardIndex) + ":" + strconv.Itoa(hd.DeviceIndex)
			hwMap[key] = hd
		}

		out := make([]Device, 0, len(devs))
		for _, d := range devs {
			canonicalURN := ""
			if matches := hwIndexRegex.FindStringSubmatch(d.Name); len(matches) >= 2 {
				cIdx := matches[1]
				dIdx := "0"
				if len(matches) >= 3 && matches[2] != "" {
					dIdx = matches[2]
				}
				key := cIdx + ":" + dIdx
				if hd, ok := hwMap[key]; ok && hd.URN != "" {
					canonicalURN = hd.URN
				}
			}

			if canonicalURN == "" {
				canonicalURN = urn.NewName(d.Name)
			}

			legacyID := strconv.Itoa(d.Index)
			out = append(out, Device{
				ID:                canonicalURN,
				URN:               canonicalURN,
				LegacyID:          legacyID,
				Name:              d.Name,
				MaxInput:          d.MaxInputChannels,
				MaxOutput:         d.MaxOutputChannels,
				DefaultSampleRate: d.DefaultSampleRate,
				IsVirtual:         false,
				Status:            "active",
				RuntimeID:         d.Index,
			})
		}
		return out, nil
	})
}

func emulatedDeviceList() []Device {
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
	}
}
