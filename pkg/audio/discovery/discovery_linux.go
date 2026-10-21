//go:build linux

package discovery

import (
	"os"
)

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
