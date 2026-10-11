//go:build !linux

package discovery

// DiscoverDevices returns empty results on non-Linux platforms (macOS CoreAudio, Windows WASAPI).
func DiscoverDevices() ([]DiscoveredDevice, error) {
	return nil, nil
}

// DetectActiveHardwareRate is a stub on non-Linux platforms.
func DetectActiveHardwareRate(card, device int, direction string) (int, bool) {
	return 0, false
}

// DetectActiveHardwareRateWithRoot is a stub on non-Linux platforms.
func DetectActiveHardwareRateWithRoot(procRoot string, card, device int, direction string) (int, bool) {
	return 0, false
}
