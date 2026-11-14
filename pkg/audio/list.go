//go:build !noaudio

package audio

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/audio-router-p2p/pkg/audio/discovery"
	"github.com/audio-router-p2p/pkg/audio/urn"
	"github.com/gordonklaus/portaudio"
)

var (
	hwIndexRegex     = regexp.MustCompile(`\(?(?:hw|plughw):(\d+)(?:,(\d+))?\)?`)
	alsaCardDevRegex = regexp.MustCompile(`CARD=([a-zA-Z0-9_\-]+)(?:.*DEV=(\d+))?`)
)

func devicePriorityScore(name string, inCh, outCh int) int {
	nameLower := strings.ToLower(name)
	score := inCh + outCh
	if strings.Contains(nameLower, "(hw:") {
		score += 50
	} else if strings.Contains(nameLower, "hw:") {
		score += 40
	} else if strings.Contains(nameLower, "plughw:") {
		score += 30
	} else if strings.Contains(nameLower, "card=") {
		score += 20
	} else if strings.Contains(nameLower, "default") {
		score += 1
	}
	return score
}

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
		hwByURN := make(map[string]discovery.DiscoveredDevice)
		for _, hd := range hwDevs {
			hwMap[strconv.Itoa(hd.CardIndex)+":"+strconv.Itoa(hd.DeviceIndex)] = hd
			if hd.CardID != "" {
				hwMap[strings.ToLower(hd.CardID)+":"+strconv.Itoa(hd.DeviceIndex)] = hd
			}
			if hd.CardName != "" {
				hwMap[urn.Slugify(hd.CardName)+":"+strconv.Itoa(hd.DeviceIndex)] = hd
			}
			if hd.URN != "" {
				hwByURN[hd.URN] = hd
			}
		}

		type candidate struct {
			dev   Device
			score int
		}
		seen := make(map[string]candidate)

		for _, d := range devs {
			canonicalURN := ""
			dNameLower := strings.ToLower(d.Name)
			isInputOnly := d.MaxInputChannels > 0 && d.MaxOutputChannels == 0
			isOutputOnly := d.MaxOutputChannels > 0 && d.MaxInputChannels == 0

			// 1. Direct hardware index: hw:C,D or plughw:C,D
			if matches := hwIndexRegex.FindStringSubmatch(d.Name); len(matches) >= 2 {
				cIdx := matches[1]
				if len(matches) >= 3 && matches[2] != "" {
					dIdx := matches[2]
					if hd, ok := hwMap[cIdx+":"+dIdx]; ok && hd.URN != "" {
						canonicalURN = hd.URN
					}
				} else {
					cIdxInt, _ := strconv.Atoi(cIdx)
					for _, hd := range hwDevs {
						if hd.CardIndex == cIdxInt {
							if (isInputOnly && hd.CanCapture) || (isOutputOnly && hd.CanPlayback) {
								canonicalURN = hd.URN
								break
							}
						}
					}
					if canonicalURN == "" {
						if hd, ok := hwMap[cIdx+":0"]; ok && hd.URN != "" {
							canonicalURN = hd.URN
						}
					}
				}
			}

			// 2. ALSA card and optional dev parameter: CARD=...,DEV=...
			if canonicalURN == "" {
				if matches := alsaCardDevRegex.FindStringSubmatch(d.Name); len(matches) >= 2 && matches[1] != "" {
					cID := strings.ToLower(matches[1])
					if len(matches) >= 3 && matches[2] != "" {
						dIdx := matches[2]
						if hd, ok := hwMap[cID+":"+dIdx]; ok && hd.URN != "" {
							canonicalURN = hd.URN
						}
					} else {
						for _, hd := range hwDevs {
							if strings.ToLower(hd.CardID) == cID || urn.Slugify(hd.CardName) == cID {
								if (isInputOnly && hd.CanCapture) || (isOutputOnly && hd.CanPlayback) {
									canonicalURN = hd.URN
									break
								}
							}
						}
						if canonicalURN == "" {
							if hd, ok := hwMap[cID+":0"]; ok && hd.URN != "" {
								canonicalURN = hd.URN
							}
						}
					}
				}
			}

			// 3. Fallback: match by card name / card ID substring with capability check
			if canonicalURN == "" {
				for _, hd := range hwDevs {
					cardIdLower := strings.ToLower(hd.CardID)
					cardSlug := urn.Slugify(hd.CardName)
					if (cardIdLower != "" && strings.Contains(dNameLower, cardIdLower)) ||
						(cardSlug != "" && strings.Contains(dNameLower, cardSlug)) {
						if (isInputOnly && hd.CanCapture) || (isOutputOnly && hd.CanPlayback) {
							canonicalURN = hd.URN
							break
						}
					}
				}
				if canonicalURN == "" {
					for _, hd := range hwDevs {
						cardIdLower := strings.ToLower(hd.CardID)
						cardSlug := urn.Slugify(hd.CardName)
						if (cardIdLower != "" && strings.Contains(dNameLower, cardIdLower)) ||
							(cardSlug != "" && strings.Contains(dNameLower, cardSlug)) {
							if hd.DeviceIndex == 0 {
								canonicalURN = hd.URN
								break
							}
						}
					}
				}
			}

			if canonicalURN == "" {
				canonicalURN = urn.NewName(d.Name)
			}

			legacyID := strconv.Itoa(d.Index)
			maxIn := d.MaxInputChannels
			maxOut := d.MaxOutputChannels
			if hd, ok := hwByURN[canonicalURN]; ok {
				if !hd.CanCapture {
					maxIn = 0
				}
				if !hd.CanPlayback {
					maxOut = 0
				}
			}

			devItem := Device{
				ID:                canonicalURN,
				URN:               canonicalURN,
				LegacyID:          legacyID,
				Name:              d.Name,
				MaxInput:          maxIn,
				MaxOutput:         maxOut,
				DefaultSampleRate: d.DefaultSampleRate,
				IsVirtual:         false,
				Status:            "active",
				RuntimeID:         d.Index,
			}

			score := devicePriorityScore(d.Name, d.MaxInputChannels, d.MaxOutputChannels)
			if existing, ok := seen[canonicalURN]; !ok {
				seen[canonicalURN] = candidate{dev: devItem, score: score}
			} else {
				mergedDev := existing.dev
				if devItem.MaxInput > mergedDev.MaxInput {
					mergedDev.MaxInput = devItem.MaxInput
				}
				if devItem.MaxOutput > mergedDev.MaxOutput {
					mergedDev.MaxOutput = devItem.MaxOutput
				}
				if score > existing.score {
					mergedDev.RuntimeID = devItem.RuntimeID
					mergedDev.Name = devItem.Name
					seen[canonicalURN] = candidate{dev: mergedDev, score: score}
				} else {
					seen[canonicalURN] = candidate{dev: mergedDev, score: existing.score}
				}
			}
		}

		out := make([]Device, 0, len(seen))
		for _, c := range seen {
			out = append(out, c.dev)
		}

		sort.Slice(out, func(i, j int) bool {
			if out[i].RuntimeID != out[j].RuntimeID {
				return out[i].RuntimeID < out[j].RuntimeID
			}
			return out[i].URN < out[j].URN
		})

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
