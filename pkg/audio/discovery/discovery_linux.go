//go:build linux

package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var rateRegex = regexp.MustCompile(`(?m)^\s*rate:\s*(\d+)`)

// DetectActiveHardwareRate inspects /proc/asound/card<X>/pcm<Y><c|p>/sub0/hw_params.
// If an external daemon (like Raspotify or MPD) has locked the ALSA device, it returns
// the active sample rate and true. Otherwise it returns 0, false.
func DetectActiveHardwareRate(card, device int, direction string) (int, bool) {
	return DetectActiveHardwareRateWithRoot("/proc/asound", card, device, direction)
}

// DetectActiveHardwareRateWithRoot inspects procRoot/card<X>/pcm<Y><c|p>/sub0/hw_params for testing or host discovery.
func DetectActiveHardwareRateWithRoot(procRoot string, card, device int, direction string) (int, bool) {
	if procRoot == "" {
		procRoot = "/proc/asound"
	}

	dirSuffix := "c"
	dLower := strings.ToLower(strings.TrimSpace(direction))
	if strings.HasPrefix(dLower, "p") || strings.HasPrefix(dLower, "o") {
		dirSuffix = "p"
	}

	hwParamsPath := filepath.Join(procRoot, fmt.Sprintf("card%d", card), fmt.Sprintf("pcm%d%s", device, dirSuffix), "sub0", "hw_params")
	data, err := os.ReadFile(hwParamsPath)
	if err != nil {
		return 0, false
	}

	content := strings.TrimSpace(string(data))
	if content == "" || strings.Contains(content, "closed") {
		return 0, false
	}

	matches := rateRegex.FindStringSubmatch(content)
	if len(matches) >= 2 {
		rate, err := strconv.Atoi(matches[1])
		if err == nil && rate > 0 {
			return rate, true
		}
	}

	return 0, false
}

// DiscoverDevices enumerates host audio hardware on Linux via /sys/class/sound and /proc/asound.
// If running in a container or environment lacking audio sysfs/procfs, it returns an empty slice gracefully.
func DiscoverDevices() ([]DiscoveredDevice, error) {
	const (
		defaultSysRoot  = "/sys/class/sound"
		defaultProcRoot = "/proc/asound"
	)

	_, sysErr := os.Stat(defaultSysRoot)
	_, procErr := os.Stat(defaultProcRoot)

	// If neither sysfs nor procfs sound tree exists, fallback to empty slice
	if sysErr != nil && procErr != nil {
		return nil, nil
	}

	return DiscoverWithRoots(defaultSysRoot, defaultProcRoot)
}
